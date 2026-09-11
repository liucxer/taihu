package rpccluster

import (
	"math/rand"

	"github.com/liucxer/taihu/internal/cluster"
)

// InstancePicker 实例选择器：本地优先（同 node）→ 远端兜底；每档内只挑
// 水位未超阈值的实例，全超时整档随机兜底（缓存场景：宁可写满盘也不丢写入）。
type InstancePicker struct {
	registry  *InstanceRegistry
	threshold float64 // 0-100
}

// NewInstancePicker 构造选择器；threshold <=0 或 >100 时取默认 80。
func NewInstancePicker(registry *InstanceRegistry, threshold float64) *InstancePicker {
	if threshold <= 0 || threshold > 100 {
		threshold = 80
	}
	return &InstancePicker{registry: registry, threshold: threshold}
}

// Pick 选择写路径锚定实例。无在线实例返回 false。
func (p *InstancePicker) Pick() (cluster.InstanceInfo, bool) {
	snap := p.registry.Snapshot()
	if len(snap.all) == 0 {
		return cluster.InstanceInfo{}, false
	}
	if inst, ok := pickRandomHealthy(snap.local, p.threshold); ok {
		return inst, true
	}
	if inst, ok := pickRandomHealthy(snap.remote, p.threshold); ok {
		return inst, true
	}
	if len(snap.local) > 0 {
		return snap.local[rand.Intn(len(snap.local))], true
	}
	return snap.all[rand.Intn(len(snap.all))], true
}

// pickRandomHealthy 从实例列表中随机挑一个水位未超阈值的。
func pickRandomHealthy(insts []cluster.InstanceInfo, threshold float64) (cluster.InstanceInfo, bool) {
	var healthy []cluster.InstanceInfo
	for _, inst := range insts {
		if usagePercent(inst) < threshold {
			healthy = append(healthy, inst)
		}
	}
	if len(healthy) == 0 {
		return cluster.InstanceInfo{}, false
	}
	return healthy[rand.Intn(len(healthy))], true
}

// usagePercent 实例已用水位百分比（capacity<=0 视为 0，兼容未上报容量的注册记录）。
func usagePercent(inst cluster.InstanceInfo) float64 {
	if inst.Capacity <= 0 {
		return 0
	}
	return float64(inst.Used) * 100 / float64(inst.Capacity)
}
