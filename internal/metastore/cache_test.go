package metastore

import (
	"strconv"
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