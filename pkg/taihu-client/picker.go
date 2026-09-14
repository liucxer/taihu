package taihuclient

import (
	"math/rand"
	"sync/atomic"

	"github.com/liucxer/taihu/internal/cluster"
)

// instancePicker 实例选择器：写路由算法决定选路方式。
//   - RouteLocal（默认）：本地优先（同机 Hostname 一致）→ 远端兜底；每档内只挑
//     水位未超阈值的实例，全超时整档随机兜底（缓存场景：宁可写满盘也不丢写入）。
//   - RouteRoundRobin：写请求在所有在线实例间轮询（含跨节点 TCP），单实例超水位跳过该次。
type instancePicker struct {
	registry  *instanceRegistry
	threshold float64       // 0-100
	routing   string        // RouteLocal / RouteRoundRobin
	rr        atomic.Uint64 // round-robin 游标
}

// newInstancePicker 构造选择器；threshold <=0 或 >100 时取默认 80，routing 非
// RouteRoundRobin 时按 RouteLocal 处理。
func newInstancePicker(registry *instanceRegistry, threshold float64, routing string) *instancePicker {
	if threshold <= 0 || threshold > 100 {
		threshold = 80
	}
	if routing != RouteRoundRobin {
		routing = RouteLocal
	}
	return &instancePicker{registry: registry, threshold: threshold, routing: routing}
}

// Pick 选择写路径锚定实例。无在线实例返回 false。
func (p *instancePicker) pick() (cluster.InstanceInfo, bool) {
	snap := p.registry.snapshot()
	if len(snap.all) == 0 {
		return cluster.InstanceInfo{}, false
	}
	switch p.routing {
	case RouteRoundRobin:
		return p.pickRoundRobin(snap.all)
	default:
		return p.pickLocalFirst(snap)
	}
}

// pickLocalFirst 本地优先：同机健康 → 远端健康 → 本地随机 → 全部随机。
func (p *instancePicker) pickLocalFirst(snap *instanceSnapshot) (cluster.InstanceInfo, bool) {
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

// pickRoundRobin 在所有在线实例间轮询：从游标处向前找第一个水位未超阈值的实例；
// 全部超水位时兜底返回游标处实例（不丢写入）。
func (p *instancePicker) pickRoundRobin(all []cluster.InstanceInfo) (cluster.InstanceInfo, bool) {
	n := len(all)
	start := int(p.rr.Add(1)-1) % n
	for i := 0; i < n; i++ {
		if inst := all[(start+i)%n]; usagePercent(inst) < p.threshold {
			return inst, true
		}
	}
	return all[start], true
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
