package rpccluster

import (
	"context"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
)

// indexFlushInterval 索引批量刷盘周期。
const indexFlushInterval = 100 * time.Millisecond

// indexBatchMax 单批最大条数。
const indexBatchMax = 128

// indexKey 返回 key→实例 索引的 KV key。
func indexKey(key string) string {
	return cluster.IndexKeyPrefix + key
}

type indexItem struct {
	key  string
	name string
}

// IndexManager key→实例名 索引，异步批量写 KV（尽力而为：失败丢弃，读 miss
// 由回源兜底；Delete 同步尽力删，保证删除语义）。
type IndexManager struct {
	kv   cluster.KV
	ch   chan indexItem
	stop chan struct{}
	done chan struct{}
}

// NewIndexManager 构造索引管理器。
func NewIndexManager(kv cluster.KV) *IndexManager {
	return &IndexManager{
		kv:   kv,
		ch:   make(chan indexItem, 4096),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Start 启动后台批量写循环。
func (m *IndexManager) Start() {
	go m.loop()
}

func (m *IndexManager) loop() {
	defer close(m.done)
	ticker := time.NewTicker(indexFlushInterval)
	defer ticker.Stop()
	batch := make(map[string][]byte)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		_ = m.kv.BatchPut(context.Background(), batch) // 尽力而为
		batch = make(map[string][]byte)
	}
	for {
		select {
		case <-m.stop:
			flush() // 退出前把剩余索引尽力落盘（不阻塞）
			return
		case item := <-m.ch:
			batch[indexKey(item.key)] = []byte(item.name)
			if len(batch) >= indexBatchMax {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Put 异步投递 key→实例 索引；队列满时丢弃（读 miss 回源兜底）。
func (m *IndexManager) Put(key, name string) {
	select {
	case m.ch <- indexItem{key: key, name: name}:
	default:
	}
}

// Get 同步查询索引；不存在返回 ("", nil)。
func (m *IndexManager) Get(ctx context.Context, key string) (string, error) {
	v, err := m.kv.Get(ctx, []byte(indexKey(key)))
	if err != nil || v == nil {
		return "", err
	}
	return string(v), nil
}

// Delete 同步删除索引（尽力）。
func (m *IndexManager) Delete(ctx context.Context, key string) error {
	return m.kv.Delete(ctx, []byte(indexKey(key)))
}

// Stop 停止后台批量循环（幂等）。
func (m *IndexManager) Stop() {
	select {
	case <-m.stop:
		return
	default:
		close(m.stop)
	}
	<-m.done
}
