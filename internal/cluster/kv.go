// Package cluster 提供 taihu 集群支持的基础组件：注册中心 KV 接口、
// 实例注册/心跳/注销、实例信息模型。注册/索引后端可替换（内存 / TiKV TxnKV），
// 调用方通过 KV 接口注入，数据面（internal/transport、internal/storage）不感知集群细节。
package cluster

import "context"

// KV 是集群注册与索引所需的最小存储接口。
// 约定：Get 返回 nil 表示 key 不存在；Scan 返回 [start, end) 字典序区间
// 内的 keys/values（limit<=0 表示不限制）。
type KV interface {
	Put(ctx context.Context, key, value []byte) error
	Get(ctx context.Context, key []byte) ([]byte, error)
	Delete(ctx context.Context, key []byte) error
	// DeleteRange 删除 [start, end) 字典序区间的全部 key。
	DeleteRange(ctx context.Context, start, end []byte) error
	Scan(ctx context.Context, start, end []byte, limit int) ([][]byte, [][]byte, error)
	BatchPut(ctx context.Context, kvs map[string][]byte) error
	BatchGet(ctx context.Context, keys [][]byte) ([][]byte, error)
	Close() error
}

// key 前缀约定（与 kvcache 同风格，独立命名空间）。
const (
	// InstanceKeyPrefix 实例注册区：/taihu/instances/{name}
	InstanceKeyPrefix = "/taihu/instances/"
	// IndexKeyPrefix key→实例索引区：/taihu/index/{key}
	IndexKeyPrefix = "/taihu/index/"
	// ClientKeyPrefix SDK 客户端注册区：/taihu/clients/{id}
	ClientKeyPrefix = "/taihu/clients/"
	// UserDataPrefix taihu 全部写入 TiKV 的共享命名空间（实例/索引/客户端注册均位于其下）。
	UserDataPrefix = "/taihu/"
)

// TaihuDataRange 返回清空 taihu 全量元数据的 [start, end) 范围（含
// 实例注册/索引/SDK 客户端注册），供 DeleteRange 一次性清理。
func TaihuDataRange() (start, end []byte) {
	return []byte(UserDataPrefix), []byte(UserDataPrefix + "\xff")
}
