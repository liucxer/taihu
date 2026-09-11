package cluster

import (
	"context"
	"sort"
	"sync"
)

// MemoryKV 内存 KV 实现：单机开发/测试、无 TiKV 环境的降级运行。
// 数据不持久化，进程退出即丢失（与缓存语义一致：可丢）。
type MemoryKV struct {
	mu sync.RWMutex
	m  map[string][]byte
}

var _ KV = (*MemoryKV)(nil)

// NewMemoryKV 构造空的内存 KV。
func NewMemoryKV() *MemoryKV {
	return &MemoryKV{m: make(map[string][]byte)}
}

func (k *MemoryKV) Put(_ context.Context, key, value []byte) error {
	k.mu.Lock()
	k.m[string(key)] = append([]byte(nil), value...)
	k.mu.Unlock()
	return nil
}

func (k *MemoryKV) Get(_ context.Context, key []byte) ([]byte, error) {
	k.mu.RLock()
	v, ok := k.m[string(key)]
	k.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	return append([]byte(nil), v...), nil
}

func (k *MemoryKV) Delete(_ context.Context, key []byte) error {
	k.mu.Lock()
	delete(k.m, string(key))
	k.mu.Unlock()
	return nil
}

func (k *MemoryKV) Scan(_ context.Context, start, end []byte, limit int) ([][]byte, [][]byte, error) {
	s, e := string(start), string(end)
	k.mu.RLock()
	defer k.mu.RUnlock()
	keys := make([]string, 0, len(k.m))
	for key := range k.m {
		if (s == "" || key >= s) && (e == "" || key < e) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	ks := make([][]byte, 0, len(keys))
	vs := make([][]byte, 0, len(keys))
	for _, key := range keys {
		ks = append(ks, []byte(key))
		vs = append(vs, append([]byte(nil), k.m[key]...))
	}
	return ks, vs, nil
}

func (k *MemoryKV) BatchPut(_ context.Context, kvs map[string][]byte) error {
	k.mu.Lock()
	for key, value := range kvs {
		k.m[key] = append([]byte(nil), value...)
	}
	k.mu.Unlock()
	return nil
}

func (k *MemoryKV) BatchGet(_ context.Context, keys [][]byte) ([][]byte, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([][]byte, len(keys))
	for i, key := range keys {
		if v, ok := k.m[string(key)]; ok {
			out[i] = append([]byte(nil), v...)
		}
	}
	return out, nil
}

func (k *MemoryKV) Close() error { return nil }
