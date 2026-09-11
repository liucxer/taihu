package cluster

import (
	"context"
	"fmt"

	"github.com/tikv/client-go/v2/config"
	"github.com/tikv/client-go/v2/rawkv"
)

// TiKVKV TiKV rawkv 后端（真实集群模式）。依赖 github.com/tikv/client-go/v2
// （与 kvcache 同款），构造时指定 PD 地址列表。Get 返回 nil 表示 key 不存在
// （rawkv 语义），与 KV 接口约定一致。
type TiKVKV struct {
	c *rawkv.Client
}

var _ KV = (*TiKVKV)(nil)

// NewTiKVKV 连接 TiKV PD 集群。
func NewTiKVKV(ctx context.Context, pdAddrs []string) (*TiKVKV, error) {
	c, err := rawkv.NewClient(ctx, pdAddrs, config.Security{})
	if err != nil {
		return nil, fmt.Errorf("tikv rawkv connect: %w", err)
	}
	return &TiKVKV{c: c}, nil
}

func (k *TiKVKV) Put(ctx context.Context, key, value []byte) error {
	return k.c.Put(ctx, key, value)
}

func (k *TiKVKV) Get(ctx context.Context, key []byte) ([]byte, error) {
	return k.c.Get(ctx, key)
}

func (k *TiKVKV) Delete(ctx context.Context, key []byte) error {
	return k.c.Delete(ctx, key)
}

func (k *TiKVKV) Scan(ctx context.Context, start, end []byte, limit int) ([][]byte, [][]byte, error) {
	return k.c.Scan(ctx, start, end, limit)
}

func (k *TiKVKV) BatchPut(ctx context.Context, kvs map[string][]byte) error {
	keys := make([][]byte, 0, len(kvs))
	values := make([][]byte, 0, len(kvs))
	for key, value := range kvs {
		keys = append(keys, []byte(key))
		values = append(values, value)
	}
	return k.c.BatchPut(ctx, keys, values)
}

func (k *TiKVKV) BatchGet(ctx context.Context, keys [][]byte) ([][]byte, error) {
	return k.c.BatchGet(ctx, keys)
}

func (k *TiKVKV) Close() error {
	k.c.Close()
	return nil
}
