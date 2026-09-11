// Package rpccluster 提供 taihu 集群客户端（缓存场景：首写本地 + 索引锚定 + 回源兜底）。
//
// 定位层（Registry/Picker/Index/RouteCache）不进入数据面热路径：数据面复用
// pkg/rpcclient（netpoll 零拷贝），集群层只负责"key 写到哪个实例、从哪个实例读"。
// 注册/索引后端经 internal/cluster.KV 注入（内存 KV / TiKV TxnKV），可降级：
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

// 写路由算法取值。
const (
	// RouteLocal 本地优先（默认）：写请求先锚定同机实例（Hostname 一致，走共享内存），
	// 本地无健康实例时 fallback 远端（水位数一致）。
	RouteLocal = "local"
	// RouteRoundRobin 轮询：写请求在所有在线实例间按 round-robin 均分（含跨节点 TCP），
	// 单实例超水位时跳过该次（全满则兜底）。
	RouteRoundRobin = "round-robin"
)

// ClusterConfig 集群客户端配置。
type ClusterConfig struct {
	// KV 注册/索引后端（必填；TiKV TxnKV 或内存）。
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
	// WriteRouting 写路由算法：RouteLocal（"local"，默认，优先本地实例）或
	// RouteRoundRobin（"round-robin"，所有在线实例轮询）。空字符串取默认 local。
	WriteRouting string
	// Conns 每地址数据面连接数：本地实例 shm 会话数（shmipc SessionNum）、跨节点每 TCP
	// 地址连接数（DialPoolMulti perAddr）。<=0 默认 1。服务端通告多地址（Addrs）时，跨节点
	// 总连接数 = 地址数 × Conns，读写请求 round-robin 均分到全部地址连接。
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
