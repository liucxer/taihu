package rpccluster

import (
	"container/list"
	"sync"
)

// routeShards RouteCache 分片数（FNV-32 哈希取模）。
const routeShards = 16

// RouteCache key→实例名 分片 LRU（读定位加速，避免每读打索引 KV）。
type RouteCache struct {
	shards [routeShards]*routeShard
}

type routeShard struct {
	mu  sync.Mutex
	m   map[string]*list.Element
	l   *list.List // front=最近使用
	cap int
}

type routeEntry struct {
	key  string
	name string
}

// NewRouteCache 构造路由缓存，cap 为每分片容量（<=0 默认 4096）。
func NewRouteCache(cap int) *RouteCache {
	if cap <= 0 {
		cap = 4096
	}
	rc := &RouteCache{}
	for i := range rc.shards {
		rc.shards[i] = &routeShard{m: make(map[string]*list.Element), l: list.New(), cap: cap}
	}
	return rc
}

func (rc *RouteCache) shard(key string) *routeShard {
	return rc.shards[fnv32(key)%routeShards]
}

// Get 命中并刷新为最近使用；未命中返回 false。
func (rc *RouteCache) Get(key string) (string, bool) {
	sh := rc.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if e, ok := sh.m[key]; ok {
		sh.l.MoveToFront(e)
		return e.Value.(*routeEntry).name, true
	}
	return "", false
}

// Put 写入/刷新 key→实例名。
func (rc *RouteCache) Put(key, name string) {
	sh := rc.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if e, ok := sh.m[key]; ok {
		e.Value.(*routeEntry).name = name
		sh.l.MoveToFront(e)
		return
	}
	e := sh.l.PushFront(&routeEntry{key: key, name: name})
	sh.m[key] = e
	if sh.l.Len() > sh.cap {
		back := sh.l.Back()
		if back != nil {
			sh.l.Remove(back)
			delete(sh.m, back.Value.(*routeEntry).key)
		}
	}
}

// Delete 删除缓存项。
func (rc *RouteCache) Delete(key string) {
	sh := rc.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if e, ok := sh.m[key]; ok {
		sh.l.Remove(e)
		delete(sh.m, key)
	}
}

// fnv32 FNV-1a 32 位哈希（分片定位）。
func fnv32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
