package cluster

import (
	"context"
	"encoding/json"
	"time"
)

// SDK 客户端注册与心跳保活（taihu-cli 设计文档 §5）：所有 SDK 客户端定期向
// TiKV 续约保活（语义与服务端实例注册完全一致：StartTime 固定、LastHeartbeat 周期刷新、
// 离线判定照 LastHeartbeat 超时）。注册区独立前缀 /taihu/clients/，避免与实例区混淆。

// ClientInfo SDK 客户端注册信息，JSON 序列化后存于 ClientKeyPrefix/{ID}。
type ClientInfo struct {
	ID            string            `json:"id"`
	Node          string            `json:"node"`
	Addr          string            `json:"addr,omitempty"`  // 可选：SDK 数据面地址
	Host          string            `json:"host,omitempty"`  // 主机名
	Pid           int               `json:"pid"`             // 进程号
	SDKVersion    string            `json:"sdk_version"`     // 构建版本 commit_日期
	Extra         map[string]string `json:"extra,omitempty"` // 调用方自定义标签
	StartTime     int64             `json:"start_time"`      // unix 秒
	LastHeartbeat int64             `json:"last_heartbeat"`  // unix 秒
}

// ClientKey 返回客户端注册 key。
func ClientKey(id string) []byte {
	return []byte(ClientKeyPrefix + id)
}

// ClientScanRange 返回客户端注册区前缀扫描的 [start, end)。
func ClientScanRange() (start, end []byte) {
	return []byte(ClientKeyPrefix), []byte(ClientKeyPrefix + "\xff")
}

// Aliveness 按 last_heartbeat 判定是否存活（语义与 InstanceInfo.Aliveness 一致）。
func (c *ClientInfo) Aliveness(now time.Time, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return now.Sub(time.Unix(c.LastHeartbeat, 0)) <= timeout
}

// RegisterClient 注册客户端（幂等：重复调用覆盖旧记录，重启场景）。
func RegisterClient(ctx context.Context, kv KV, info *ClientInfo) error {
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return kv.Put(ctx, ClientKey(info.ID), b)
}

// UnregisterClient 注销客户端（优雅退出时调用）。
func UnregisterClient(ctx context.Context, kv KV, id string) error {
	return kv.Delete(ctx, ClientKey(id))
}

// RunClientHeartbeat 周期心跳：每 interval 调用 build 重建客户端信息（Addr/Extra 等
// 动态字段随 build 刷新），刷新 LastHeartbeat 后写回注册 key。ctx 取消即退出。
// 语义与 RunHeartbeat 一致：写失败不致命，下一周期重试。
func RunClientHeartbeat(ctx context.Context, kv KV, build func() *ClientInfo, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		info := *build()
		info.LastHeartbeat = time.Now().Unix()
		_ = RegisterClient(context.Background(), kv, &info)
	}
}

// ListClients 扫描客户端注册区全部记录（含已过期未注销的，离线判定由调用方完成）。
func ListClients(ctx context.Context, kv KV) ([]ClientInfo, error) {
	// 注意：TiKV rawkv Scan 受 MaxRawKVScanLimit(10240) 上限约束，
	// 与 register.go ListInstances 保持一致用固定小 limit（10000 < 10240）。
	start, end := ClientScanRange()
	_, values, err := kv.Scan(ctx, start, end, 10000)
	if err != nil {
		return nil, err
	}
	out := make([]ClientInfo, 0, len(values))
	for _, v := range values {
		var info ClientInfo
		if err := json.Unmarshal(v, &info); err != nil {
			continue // 脏数据跳过
		}
		out = append(out, info)
	}
	return out, nil
}

// GetClient 读取单个客户端注册信息；ID 不存在返回 (nil, nil)。
func GetClient(ctx context.Context, kv KV, id string) (*ClientInfo, error) {
	v, err := kv.Get(ctx, ClientKey(id))
	if err != nil || v == nil {
		return nil, err
	}
	var info ClientInfo
	if err := json.Unmarshal(v, &info); err != nil {
		return nil, err
	}
	return &info, nil
}
