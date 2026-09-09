package taihu

import (
	"container/list"
	"hash/fnv"
	"sync"
)

// 有界 LRU 元数据缓存，无锁（分片锁）设计。
// 缓存 key → ObjectMeta；RocksDB 为真实源，本缓存为 read-through 加速层，
// 任何时刻都可由 RocksDB 重建，淘汰/崩溃不丢失持久性。

const (
	// shardCount 分片数（2 的幂，用于取模）。
	shardCount = 256
	// maxCacheBytes 全局预算 1GB。
	maxCacheBytes = 1 << 30
	// perShardLimit 每个分片硬上限。
	perShardLimit = maxCacheBytes / shardCount
	// perEntryOverhead 每条目固定开销（key 头 16 + value 24 + map/节点 ~52），估算值。
	perEntryOverhead = 92
)

// cacheEntry 单个缓存条目。key 复用 map 的字符串底层数组，不额外拷贝字节。
type cacheEntry struct {
	key  string
	meta ObjectMeta
}

type shard struct {
	mu        sync.Mutex
	entries   map[string]*list.Element // O(1) 命中
	ll        *list.List               // Front=MRU, Back=LRU
	usedBytes int64
}

type metaCache struct {
	shards [shardCount]shard
}

// shardFor 哈希寻片（无锁阶段，此后仅需锁命中 shard）。
func (c *metaCache) shardFor(key string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &c.shards[h.Sum32()%shardCount]
}

// get 命中时移到 MRU 端并返回 meta。
func (c *metaCache) get(key string) (ObjectMeta, bool) {
	s := c.shardFor(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok {
		s.ll.MoveToFront(e)
		return e.Value.(*cacheEntry).meta, true
	}
	return ObjectMeta{}, false
}

// put 写入（upsert）为 MRU；若超分片预算则逐出 LRU。
func (c *metaCache) put(key string, meta ObjectMeta) {
	s := c.shardFor(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok {
		e.Value.(*cacheEntry).meta = meta
		s.ll.MoveToFront(e)
		return
	}
	if s.entries == nil {
		s.entries = make(map[string]*list.Element)
		s.ll = list.New()
	}
	cost := int64(len(key)) + perEntryOverhead
	s.entries[key] = s.ll.PushFront(&cacheEntry{key: key, meta: meta})
	s.usedBytes += cost
	s.evictLocked()
}

// del 删除并归还预算。
func (c *metaCache) del(key string) {
	s := c.shardFor(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok {
		s.ll.Remove(e)
		delete(s.entries, key)
		s.usedBytes -= int64(len(key)) + perEntryOverhead
	}
}

// evictLocked 分片内逐出，直到该分片重新 ≤ perShardLimit。
func (s *shard) evictLocked() {
	for s.usedBytes > perShardLimit && s.ll.Len() > 0 {
		back := s.ll.Back()
		entry := back.Value.(*cacheEntry)
		s.ll.Remove(back)
		delete(s.entries, entry.key)
		s.usedBytes -= int64(len(entry.key)) + perEntryOverhead
	}
}