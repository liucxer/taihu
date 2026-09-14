package rpccluster

import (
	"context"
	"fmt"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
)

// TiKVOptions 基于 TiKV TxnKV 后端的集群客户端选项（外接 SDK / EFS_nefs 用）。
// 内部构造 cluster.NewTiKVKV（client-go v2.0.7 TxnKV，memcomparable 编码），
// 与 mgmt meta 等 TxnKV 客户端可共存同一集群。
type TiKVOptions struct {
	PDAddrs []string // TiKV PD 地址（必填，如 ["host:2379"]）
	CA      string   // TiKV TLS CA 证书路径（三件套齐全才启用；全空=明文）
	Cert    string   // TiKV TLS 客户端证书路径
	Key     string   // TiKV TLS 客户端私钥路径

	RefreshInterval  time.Duration // 实例发现刷新周期（<=0 默认 1s）
	HeartbeatTimeout time.Duration // 实例离线判定超时（<=0 默认 5s）
	UsageThreshold   float64       // 选实例水位阈值百分比 0-100（<=0 或 >100 默认 80）
	WriteRouting     string        // 写路由算法：RouteLocal（默认）或 RouteRoundRobin
	Conns            int           // 每地址数据面连接数（<=0 默认 1）

	Source       SourceGetter      // 回源回调（可选）
	ClientName   string            // 客户端标识（仅标注/客户端注册用）
	ClientID     string            // SDK 客户端注册 ID（可选，自动心跳续约）
	ClientAddr   string            // SDK 数据面地址（随心跳上报）
	ClientLabels map[string]string // 客户端自定义标签
}

// NewFromTiKV 连接 TiKV TxnKV 并构建集群客户端（Storage）。
// 返回的 Storage 已启动实例发现/索引后台任务，用毕须 Close()。
func NewFromTiKV(ctx context.Context, opts TiKVOptions) (*Storage, error) {
	if len(opts.PDAddrs) == 0 {
		return nil, fmt.Errorf("rpccluster: TiKVOptions.PDAddrs is required")
	}
	kv, err := cluster.NewTiKVKV(ctx, opts.PDAddrs, cluster.TLSConfig{
		CA: opts.CA, Cert: opts.Cert, Key: opts.Key,
	})
	if err != nil {
		return nil, err
	}
	st, err := NewCluster(ClusterConfig{
		KV:               kv,
		ClientName:       opts.ClientName,
		RefreshInterval:  opts.RefreshInterval,
		HeartbeatTimeout: opts.HeartbeatTimeout,
		UsageThreshold:   opts.UsageThreshold,
		WriteRouting:     opts.WriteRouting,
		Conns:            opts.Conns,
		Source:           opts.Source,
		ClientID:         opts.ClientID,
		ClientAddr:       opts.ClientAddr,
		ClientLabels:     opts.ClientLabels,
	})
	if err != nil {
		_ = kv.Close()
		return nil, err
	}
	return st, nil
}
