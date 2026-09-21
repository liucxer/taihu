package metastore

import (
	"strconv"
	"strings"
	"testing"
)

func TestCacheEvictionBudget(t *testing.T) {
	c := &metaCache{}
	// 塞入足够条目触发逐出，验证各分片 usedBytes 不超 perShardLimit
	for i := 0; i < 100000; i++ {
		k := "key-" + strconv.Itoa(i)
		c.put(k, ObjectMeta{SegmentID: int64(i)})
	}
	for i := 0; i < shardCount; i++ {
		if c.shards[i].usedBytes > perShardLimit {
			t.Fatalf("shard %d over budget: %d > %d", i, c.shards[i].usedBytes, perShardLimit)
		}
	}
}

// TestCacheEvictsOversizedEntry：单条即超分片预算时被逐出（LRU 回收），
// 且不影响后续小条目写入。
func TestCacheEvictsOversizedEntry(t *testing.T) {
	c := &metaCache{}
	big := strings.Repeat("k", int(perShardLimit)+100)
	c.put(big, ObjectMeta{SegmentID: 1})
	if _, ok := c.get(big); ok {
		t.Fatal("oversized entry should be evicted")
	}
	s := c.shardFor(big)
	s.mu.Lock()
	used, n := s.usedBytes, len(s.entries)
	s.mu.Unlock()
	if used != 0 || n != 0 {
		t.Fatalf("shard after eviction = (used=%d, entries=%d), want (0,0)", used, n)
	}

	// 小条目正常写入并可命中。
	c.put("small", ObjectMeta{SegmentID: 2, Offset: 4096})
	if m, ok := c.get("small"); !ok || m.SegmentID != 2 || m.Offset != 4096 {
		t.Fatalf("small entry = (%+v,%v)", m, ok)
	}
}

// TestCacheUpsertAndDelete：同 key 覆盖写为 MRU、删除归还预算、删除不存在 key 为空操作。
func TestCacheUpsertAndDelete(t *testing.T) {
	c := &metaCache{}
	c.put("a", ObjectMeta{SegmentID: 1})
	c.put("a", ObjectMeta{SegmentID: 2})
	if m, ok := c.get("a"); !ok || m.SegmentID != 2 {
		t.Fatalf("upsert = (%+v,%v)", m, ok)
	}
	s := c.shardFor("a")
	s.mu.Lock()
	usedAfterPut := s.usedBytes
	s.mu.Unlock()
	if usedAfterPut != int64(len("a"))+perEntryOverhead {
		t.Fatalf("usedBytes after upsert = %d", usedAfterPut)
	}
	c.del("a")
	if _, ok := c.get("a"); ok {
		t.Fatal("entry not deleted")
	}
	s.mu.Lock()
	usedAfterDel := s.usedBytes
	s.mu.Unlock()
	if usedAfterDel != 0 {
		t.Fatalf("usedBytes after delete = %d, want 0", usedAfterDel)
	}
	c.del("a") // 不存在：空操作
}
