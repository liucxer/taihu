package cluster

import (
	"context"
	"encoding/json"
)

// 容量记录区：/taihu/capacity/{name}。启动时读取 nvme 真实容量后按实例唯一名记录；
// 后续启动再次读取并与记录比较，不一致则拒绝启动（防止容量变化导致布局错乱）。
const CapacityKeyPrefix = "/taihu/capacity/"

// CapacityRecord 实例容量记录（JSON 序列化后存于 KV）。
type CapacityRecord struct {
	CapacityBytes    int64  `json:"capacity_bytes"`    // 启动时读取的 nvme 容量（字节）
	SegmentSizeBytes int64  `json:"segment_size_bytes"` // 布局段大小
	SegmentCount     int64  `json:"segment_count"`     // 布局段数 = capacity/segmentSize
	ListenAddr       string `json:"listen_addr"`       // 实例监听地址（审计/查询用）
	UpdateTime       int64  `json:"update_time"`       // 记录写入时间（unix 秒）
}

// CapacityKey 返回实例容量记录 key。
func CapacityKey(name string) []byte {
	return []byte(CapacityKeyPrefix + name)
}

// GetCapacity 读取实例容量记录；不存在返回 ok=false。
func GetCapacity(ctx context.Context, kv KV, name string) (CapacityRecord, bool, error) {
	v, err := kv.Get(ctx, CapacityKey(name))
	if err != nil || v == nil {
		return CapacityRecord{}, false, err
	}
	var c CapacityRecord
	if err := json.Unmarshal(v, &c); err != nil {
		return CapacityRecord{}, false, err
	}
	return c, true, nil
}

// PutCapacity 写入/覆盖实例容量记录。
func PutCapacity(ctx context.Context, kv KV, name string, c CapacityRecord) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return kv.Put(ctx, CapacityKey(name), b)
}
