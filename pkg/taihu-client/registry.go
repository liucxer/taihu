package taihuclient

import (
	"context"
	"sync"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
)

// instanceSnapshot 一次发现的实例快照（不可变，原子替换；并发读无锁）。
type instanceSnapshot struct {
	all    []cluster.InstanceInfo // 全部在线实例
	local  []cluster.InstanceInfo // 同机（Hostname 一致）实例（本地优先）
	remote []cluster.InstanceInfo // 远端实例
	byName map[string]cluster.InstanceInfo
}

// buildSnapshot 按心跳超时过滤离线实例并分组：本地 = Hostname 与本机一致。
func buildSnapshot(all []cluster.InstanceInfo, localHostname string, now time.Time, timeout time.Duration) *instanceSnapshot {
	snap := &instanceSnapshot{byName: make(map[string]cluster.InstanceInfo, len(all))}
	for _, inst := range all {
		if !inst.Aliveness(now, timeout) {
			continue // 心跳超时：离线，排除
		}
		snap.all = append(snap.all, inst)
		snap.byName[inst.Name] = inst
		if inst.Hostname == localHostname {
			snap.local = append(snap.local, inst)
		} else {
			snap.remote = append(snap.remote, inst)
		}
	}
	return snap
}

// instanceRegistry 周期扫描 KV 注册区维护在线实例快照（修复 kvcache 的 start_time
// 复用问题：离线判定依据 LastHeartbeat 字段）。
type instanceRegistry struct {
	kv            cluster.KV
	localHostname string
	interval      time.Duration
	timeout       time.Duration

	mu   sync.RWMutex
	snap *instanceSnapshot

	stopCh chan struct{}
	done   chan struct{}
}

// newInstanceRegistry 构造实例发现器。localHostname 为本机 hostname（os.Hostname），
// 用于本地优先分组（与实例注册的 Hostname 比较）。
func newInstanceRegistry(kv cluster.KV, localHostname string, interval, timeout time.Duration) *instanceRegistry {
	if interval <= 0 {
		interval = time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &instanceRegistry{
		kv:            kv,
		localHostname: localHostname,
		interval:      interval,
		timeout:       timeout,
		stopCh:        make(chan struct{}),
		done:          make(chan struct{}),
	}
}

// Start 立即刷新一次并启动周期扫描。
func (r *instanceRegistry) start() {
	r.refresh()
	go r.loop()
}

func (r *instanceRegistry) loop() {
	defer close(r.done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.refresh()
		}
	}
}

// refresh 扫描注册区并原子替换快照；扫描失败保留旧快照（TiKV 抖动降级）。
func (r *instanceRegistry) refresh() {
	all, err := cluster.ListInstances(context.Background(), r.kv)
	if err != nil {
		return
	}
	snap := buildSnapshot(all, r.localHostname, time.Now(), r.timeout)
	r.mu.Lock()
	r.snap = snap
	r.mu.Unlock()
}

// Snapshot 返回当前实例快照。
func (r *instanceRegistry) snapshot() *instanceSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snap == nil {
		return &instanceSnapshot{byName: make(map[string]cluster.InstanceInfo)}
	}
	return r.snap
}

// Lookup 按实例名查活跃实例。
func (r *instanceRegistry) lookup(name string) (cluster.InstanceInfo, bool) {
	inst, ok := r.snapshot().byName[name]
	return inst, ok
}

// Stop 停止周期扫描。
func (r *instanceRegistry) stop() {
	select {
	case <-r.stopCh:
		return
	default:
		close(r.stopCh)
	}
	<-r.done
}
