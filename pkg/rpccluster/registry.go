package rpccluster

import (
	"context"
	"sync"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
)

// instanceSnapshot 一次发现的实例快照（不可变，原子替换；并发读无锁）。
type instanceSnapshot struct {
	all    []cluster.InstanceInfo // 全部在线实例
	local  []cluster.InstanceInfo // 同 node 实例（本地优先）
	remote []cluster.InstanceInfo // 远端实例
	byName map[string]cluster.InstanceInfo
}

// buildSnapshot 按心跳超时过滤离线实例并分组。
func buildSnapshot(all []cluster.InstanceInfo, node string, now time.Time, timeout time.Duration) *instanceSnapshot {
	snap := &instanceSnapshot{byName: make(map[string]cluster.InstanceInfo, len(all))}
	for _, inst := range all {
		if !inst.Aliveness(now, timeout) {
			continue // 心跳超时：离线，排除
		}
		snap.all = append(snap.all, inst)
		snap.byName[inst.Name] = inst
		if inst.Node == node {
			snap.local = append(snap.local, inst)
		} else {
			snap.remote = append(snap.remote, inst)
		}
	}
	return snap
}

// InstanceRegistry 周期扫描 KV 注册区维护在线实例快照（修复 kvcache 的 start_time
// 复用问题：离线判定依据 LastHeartbeat 字段）。
type InstanceRegistry struct {
	kv       cluster.KV
	node     string
	interval time.Duration
	timeout  time.Duration

	mu   sync.RWMutex
	snap *instanceSnapshot

	stop chan struct{}
	done chan struct{}
}

// NewInstanceRegistry 构造实例发现器。
func NewInstanceRegistry(kv cluster.KV, node string, interval, timeout time.Duration) *InstanceRegistry {
	if interval <= 0 {
		interval = time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &InstanceRegistry{
		kv:       kv,
		node:     node,
		interval: interval,
		timeout:  timeout,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start 立即刷新一次并启动周期扫描。
func (r *InstanceRegistry) Start() {
	r.refresh()
	go r.loop()
}

func (r *InstanceRegistry) loop() {
	defer close(r.done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.refresh()
		}
	}
}

// refresh 扫描注册区并原子替换快照；扫描失败保留旧快照（TiKV 抖动降级）。
func (r *InstanceRegistry) refresh() {
	all, err := cluster.ListInstances(context.Background(), r.kv)
	if err != nil {
		return
	}
	snap := buildSnapshot(all, r.node, time.Now(), r.timeout)
	r.mu.Lock()
	r.snap = snap
	r.mu.Unlock()
}

// Snapshot 返回当前实例快照。
func (r *InstanceRegistry) Snapshot() *instanceSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snap == nil {
		return &instanceSnapshot{byName: make(map[string]cluster.InstanceInfo)}
	}
	return r.snap
}

// Lookup 按实例名查活跃实例。
func (r *InstanceRegistry) Lookup(name string) (cluster.InstanceInfo, bool) {
	inst, ok := r.Snapshot().byName[name]
	return inst, ok
}

// Stop 停止周期扫描。
func (r *InstanceRegistry) Stop() {
	select {
	case <-r.stop:
		return
	default:
		close(r.stop)
	}
	<-r.done
}
