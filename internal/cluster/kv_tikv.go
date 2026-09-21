package cluster

import (
	"context"
	"fmt"

	"github.com/tikv/client-go/v2/config"
	tikverr "github.com/tikv/client-go/v2/error"
	"github.com/tikv/client-go/v2/txnkv"
	"github.com/tikv/client-go/v2/txnkv/txnsnapshot"
)

// TLSConfig TiKV 集群 TLS 证书（PEM 文件路径）。三者齐全才启用 TLS；
// 全空表示明文连接（兼容非 TLS 测试集群）。
type TLSConfig struct {
	CA   string // CA 证书路径
	Cert string // 客户端证书路径
	Key  string // 客户端私钥路径
}

// enabled 三者齐全才视为启用 TLS。
func (t TLSConfig) enabled() bool { return t.CA != "" && t.Cert != "" && t.Key != "" }

// TiKVKV 基于 TiKV TxnKV 的 KV 实现：与 mgmt meta 等 TxnKV 客户端同编码共存
// （memcomparable），避免 rawkv 裸 key 混入导致的 region 边界解码问题。
type TiKVKV struct {
	c tikvClient
}

var _ KV = (*TiKVKV)(nil)

// 测试缝隙：TiKV 客户端/快照/事务/迭代器抽为最小接口（能力集与 client-go 具体类型
// 逐一对齐，无额外开销），生产实现为 txnkvClient/txnkvSnapshot，单元测试注入假实现
// 覆盖错误与边界路径——单测绝不连真实 TiKV/PD。迭代器接口之所以单独声明：
// client-go 的 unionstore.Iterator 位于其 internal 包，本包无法引用该类型。
type (
	// tikvIterator 快照迭代器能力集（对应 unionstore.Iterator）。
	tikvIterator interface {
		Valid() bool
		Key() []byte
		Value() []byte
		Next() error
		Close()
	}
	// tikvSnapshot 一致性快照能力集（对应 *txnsnapshot.KVSnapshot）。
	tikvSnapshot interface {
		Get(ctx context.Context, key []byte) ([]byte, error)
		BatchGet(ctx context.Context, keys [][]byte) (map[string][]byte, error)
		SetIsolationLevel(level txnkv.IsoLevel)
		Iter(start, end []byte) (tikvIterator, error)
	}
	// tikvTxn 事务能力集（对应 *txnkv.KVTxn）。
	tikvTxn interface {
		Set(key, value []byte) error
		Delete(key []byte) error
		Commit(ctx context.Context) error
	}
	// tikvClient 客户端能力集（对应 *txnkv.Client）。
	tikvClient interface {
		GetTimestamp(ctx context.Context) (uint64, error)
		GetSnapshot(ts uint64) tikvSnapshot
		Begin() (tikvTxn, error)
		Close() error
	}
)

// txnkvSnapshot 把真实 *txnsnapshot.KVSnapshot 适配为 tikvSnapshot
// （Iter 的 unionstore.Iterator 归一为 tikvIterator）。
type txnkvSnapshot struct{ s *txnsnapshot.KVSnapshot }

func (a txnkvSnapshot) Get(ctx context.Context, key []byte) ([]byte, error) {
	return a.s.Get(ctx, key)
}

func (a txnkvSnapshot) BatchGet(ctx context.Context, keys [][]byte) (map[string][]byte, error) {
	return a.s.BatchGet(ctx, keys)
}

func (a txnkvSnapshot) SetIsolationLevel(level txnkv.IsoLevel) { a.s.SetIsolationLevel(level) }

func (a txnkvSnapshot) Iter(start, end []byte) (tikvIterator, error) { return a.s.Iter(start, end) }

// txnkvClient 把真实 *txnkv.Client 适配为 tikvClient。
type txnkvClient struct{ c *txnkv.Client }

func (a txnkvClient) GetTimestamp(ctx context.Context) (uint64, error) { return a.c.GetTimestamp(ctx) }

func (a txnkvClient) GetSnapshot(ts uint64) tikvSnapshot {
	return txnkvSnapshot{s: a.c.GetSnapshot(ts)}
}

func (a txnkvClient) Begin() (tikvTxn, error) { return a.c.Begin() }

func (a txnkvClient) Close() error { return a.c.Close() }

// NewTiKVKV 连接 TiKV PD 集群（TxnKV）。tls 启用时写入 client-go 全局 Security
// 配置（txnkv.NewClient 只读全局配置）；ctx 仅为保持调用约定，暂不使用。
func NewTiKVKV(_ context.Context, pdAddrs []string, tls TLSConfig) (*TiKVKV, error) {
	if tls.enabled() {
		config.UpdateGlobal(func(conf *config.Config) {
			conf.Security = config.NewSecurity(tls.CA, tls.Cert, tls.Key, nil)
		})
	}
	c, err := txnkv.NewClient(pdAddrs)
	if err != nil {
		return nil, fmt.Errorf("tikv txnkv connect: %w", err)
	}
	return &TiKVKV{c: txnkvClient{c: c}}, nil
}

// snapshot 取最新时间戳的一致性快照；RC 隔离避免读被残留锁阻塞
// （注册/索引语义允许读到最新已提交值，不需要 SI 的锁检查）。
func (k *TiKVKV) snapshot(ctx context.Context) (tikvSnapshot, error) {
	ts, err := k.c.GetTimestamp(ctx)
	if err != nil {
		return nil, fmt.Errorf("tikv tso: %w", err)
	}
	snap := k.c.GetSnapshot(ts)
	snap.SetIsolationLevel(txnkv.RC)
	return snap, nil
}

func (k *TiKVKV) Put(ctx context.Context, key, value []byte) error {
	txn, err := k.c.Begin()
	if err != nil {
		return err
	}
	if err := txn.Set(key, value); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// Get 单 key 读取；key 不存在返回 (nil, nil)（对齐 KV 接口约定）。
func (k *TiKVKV) Get(ctx context.Context, key []byte) ([]byte, error) {
	snap, err := k.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	v, err := snap.Get(ctx, key)
	if tikverr.IsErrNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (k *TiKVKV) Delete(ctx context.Context, key []byte) error {
	txn, err := k.c.Begin()
	if err != nil {
		return err
	}
	if err := txn.Delete(key); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// DeleteRange 删除 [start, end) 区间全部 key。txnkv 无服务端范围删除，
// 客户端分批 scan+delete 循环（仅清理工具使用，非热路径）。
func (k *TiKVKV) DeleteRange(ctx context.Context, start, end []byte) error {
	for {
		keys, _, err := k.Scan(ctx, start, end, 1024)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
		txn, err := k.c.Begin()
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err := txn.Delete(key); err != nil {
				return err
			}
		}
		if err := txn.Commit(ctx); err != nil {
			return err
		}
	}
}

// Scan 返回 [start, end) 字典序区间的 keys/values（limit<=0 不限制）。
func (k *TiKVKV) Scan(ctx context.Context, start, end []byte, limit int) ([][]byte, [][]byte, error) {
	snap, err := k.snapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	it, err := snap.Iter(start, end)
	if err != nil {
		return nil, nil, err
	}
	defer it.Close()
	var ks, vs [][]byte
	for it.Valid() {
		if limit > 0 && len(ks) >= limit {
			break
		}
		ks = append(ks, append([]byte(nil), it.Key()...))
		vs = append(vs, append([]byte(nil), it.Value()...))
		if err := it.Next(); err != nil {
			return nil, nil, err
		}
	}
	return ks, vs, nil
}

func (k *TiKVKV) BatchPut(ctx context.Context, kvs map[string][]byte) error {
	txn, err := k.c.Begin()
	if err != nil {
		return err
	}
	for key, v := range kvs {
		if err := txn.Set([]byte(key), v); err != nil {
			return err
		}
	}
	return txn.Commit(ctx)
}

// BatchGet 批量读取；返回切片与 keys 等长对齐，缺失 key 对应位置为 nil。
func (k *TiKVKV) BatchGet(ctx context.Context, keys [][]byte) ([][]byte, error) {
	snap, err := k.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	m, err := snap.BatchGet(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(keys))
	for i, key := range keys {
		if v, ok := m[string(key)]; ok {
			out[i] = v
		}
	}
	return out, nil
}

func (k *TiKVKV) Close() error { return k.c.Close() }
