// Package rpccluster 提供 taihu 集群客户端（缓存场景：首写本地 + 索引锚定 + 回源兜底）。
//
// 定位层（Registry/Picker/Index/RouteCache）不进入数据面热路径：数据面复用
// pkg/rpcclient（netpoll 零拷贝），集群层只负责"key 写到哪个实例、从哪个实例读"。
// 注册/索引后端经 internal/cluster.KV 注入（内存 KV / TiKV rawkv），可降级：
// TiKV 不可用时系统退化为"本地实例 + 回源"，功能不中断。
package rpccluster

import (
	"context"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
)

// SourceGetter 回源接口：集群内全部 miss 时，从远端源拉取整对象数据。
// 返回 data 为整对象；实现方负责源侧错误语义。
type SourceGetter func(ctx context.Context, key string) ([]byte, error)

// ClusterConfig 集群客户端配置。
type ClusterConfig struct {
	// KV 注册/索引后端（必填；TiKV rawkv 或内存）。
	KV cluster.KV
	// ClientName 客户端标识（如 taihu-rpc-bench 的 -client-name）：仅作标注/客户端注册用，
	// 不参与路由。同机判定（同机走 shm、否则 TCP）由 SDK 比较本机 hostname 与
	// 服务端注册的 Hostname（os.Hostname）自动完成。
	ClientName string
	// RefreshInterval 实例发现刷新周期（<=0 默认 1s）。
	RefreshInterval time.Duration
	// HeartbeatTimeout 实例离线判定超时（<=0 默认 5s）。
	HeartbeatTimeout time.Duration
	// UsageThreshold 选实例的水位阈值百分比（<=0 或 >100 默认 80）：used/capacity
	// 超过则跳过该实例（写路径避免打满盘）。
	UsageThreshold float64
	// Conns 每实例数据面连接数：本地实例 shm 会话数（shmipc SessionNum）、跨节点 TCP
	// 连接数（DialPool n）。<=0 默认 1。多会话可支撑更高并发（48+ 线程不触发
	// shmipc 单会话过载）。
	Conns int
	// Source 回源回调（可选）：集群全 miss 时拉远端源并回写缓存。
	Source SourceGetter

	// ClientID 可选：SDK 客户端注册 ID。非空时该 SDK 自动向 KV 注册 + 周期心跳
	// 续约（taihu-cli 设计文档 §5），Close 时注销。需配合 KV 已配置。
	ClientID string
	// ClientAddr 可选：SDK 数据面地址（随心跳上报）。
	ClientAddr string
	// ClientLabels 可选：调用方自定义标签（随心跳上报）。
	ClientLabels map[string]string
}
