package cluster

import "time"

// InstanceInfo 实例注册信息，JSON 序列化后存于 KV 的 InstanceKeyPrefix/{name}。
// StartTime 注册时刻固定不变；LastHeartbeat 每次心跳刷新，是客户端离线判定的依据
// （修复 kvcache 复用 start_time 当心跳时间戳的语义错位）。
type InstanceInfo struct {
	Name          string   `json:"name"`
	Node          string   `json:"node"`
	Hostname      string   `json:"hostname"`  // 主机名：SDK 据此判断与客户端是否同机（同机走 shm，否则 TCP）
	Addr          string   `json:"addr"`      // TCP/网络地址（首个 -listen IP:port），跨节点访问用；兼容旧客户端
	Addrs         []string `json:"addrs"`     // 完整 TCP 地址列表（全部 -listen IP:port 同端口）；旧 server 无此字段时为空
	ShmAddr       string   `json:"shm_addr"`  // 同机 unix socket 路径，共享内存访问用（空=未开放 shm）
	Capacity      int64    `json:"capacity"`  // 字节
	Available     int64    `json:"available"` // 字节
	Used          int64    `json:"used"`      // 字节
	StartTime     int64    `json:"start_time"`
	LastHeartbeat int64    `json:"last_heartbeat"` // unix 秒
}

// InstanceKey 返回实例注册 key。
func InstanceKey(name string) []byte {
	return []byte(InstanceKeyPrefix + name)
}

// InstanceScanRange 返回实例注册区前缀扫描的 [start, end)。
// end = prefix+"\xff"：所有 /taihu/instances/xxx 均 < end（\xff 为最大字节）。
func InstanceScanRange() (start, end []byte) {
	return []byte(InstanceKeyPrefix), []byte(InstanceKeyPrefix + "\xff")
}

// IndexKey 返回 key→实例 索引 key。
func IndexKey(key string) []byte {
	return []byte(IndexKeyPrefix + key)
}

// Aliveness 按 last_heartbeat 判定是否存活（now 为判定时刻，timeout 秒内心跳即存活）。
func (i *InstanceInfo) Aliveness(now time.Time, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return now.Sub(time.Unix(i.LastHeartbeat, 0)) <= timeout
}
