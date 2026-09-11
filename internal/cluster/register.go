package cluster

import (
	"context"
	"encoding/json"
	"time"
)

// Register 注册实例：写入 {InstanceKeyPrefix}{name} -> json(InstanceInfo)。
// 幂等：重复调用覆盖旧记录（重启场景）。
func Register(ctx context.Context, kv KV, info *InstanceInfo) error {
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return kv.Put(ctx, InstanceKey(info.Name), b)
}

// Unregister 注销实例：删除注册 key（优雅停机时调用，避免残留"僵尸"实例）。
func Unregister(ctx context.Context, kv KV, name string) error {
	return kv.Delete(ctx, InstanceKey(name))
}

// RunHeartbeat 周期心跳：每 interval 调用 build 重建实例信息（容量等动态字段
// 随 build 刷新），刷新 LastHeartbeat 后写回注册 key。ctx 取消即退出。
// build 每次应返回"全新"实例信息（内容可复用同一字段值，但须为新对象）。
func RunHeartbeat(ctx context.Context, kv KV, build func() *InstanceInfo, interval time.Duration) {
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
		// 心跳失败不致命：下一周期重试（离线判定由客户端按超时兜底）。
		_ = Register(context.Background(), kv, &info)
	}
}

// ListInstances 扫描注册区全部实例（含已过期未注销的，离线判定由调用方完成）。
func ListInstances(ctx context.Context, kv KV) ([]InstanceInfo, error) {
	start, end := InstanceScanRange()
	_, values, err := kv.Scan(ctx, start, end, 0)
	if err != nil {
		return nil, err
	}
	out := make([]InstanceInfo, 0, len(values))
	for _, v := range values {
		var info InstanceInfo
		if err := json.Unmarshal(v, &info); err != nil {
			continue // 脏数据跳过
		}
		out = append(out, info)
	}
	return out, nil
}

// GetInstance 读取单个实例注册信息；key 不存在返回 (nil, nil)。
func GetInstance(ctx context.Context, kv KV, name string) (*InstanceInfo, error) {
	v, err := kv.Get(ctx, InstanceKey(name))
	if err != nil || v == nil {
		return nil, err
	}
	var info InstanceInfo
	if err := json.Unmarshal(v, &info); err != nil {
		return nil, err
	}
	return &info, nil
}
