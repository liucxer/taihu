package taihuclient

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
)

// registry.go / picker.go / index.go / route_cache.go 的边界与降级路径。

func TestBuildSnapshotGroupsAndFiltersOffline(t *testing.T) {
	now := time.Now()
	all := []cluster.InstanceInfo{
		{Name: "local-ok", Hostname: "here", LastHeartbeat: now.Unix()},
		{Name: "remote-ok", Hostname: "there", LastHeartbeat: now.Unix()},
		{Name: "dead", Hostname: "here", LastHeartbeat: now.Add(-time.Hour).Unix()},
	}
	snap := buildSnapshot(all, "here", now, time.Minute)
	if len(snap.all) != 2 || len(snap.local) != 1 || len(snap.remote) != 1 {
		t.Fatalf("all=%d local=%d remote=%d", len(snap.all), len(snap.local), len(snap.remote))
	}
	if snap.local[0].Name != "local-ok" || snap.remote[0].Name != "remote-ok" {
		t.Fatalf("grouping wrong: local=%v remote=%v", snap.local, snap.remote)
	}
	if _, ok := snap.byName["dead"]; ok {
		t.Fatal("offline instance must be excluded from byName")
	}
}

func TestRegistryEmptySnapshotAndRefreshError(t *testing.T) {
	kv := cluster.NewMemoryKV()
	reg := newInstanceRegistry(kv, "node1", time.Hour, time.Hour)

	// 未 refresh：快照为空的默认值（byName 已初始化），lookup 不 panic。
	snap := reg.snapshot()
	if snap == nil || len(snap.all) != 0 || snap.byName == nil {
		t.Fatalf("empty snapshot: %+v", snap)
	}
	if _, ok := reg.lookup("nope"); ok {
		t.Fatal("lookup on empty snapshot must fail")
	}

	// 扫描失败：保留旧快照（TiKV 抖动降级，不 panic、不置空）。
	bad := newInstanceRegistry(&errKV{KV: kv, scanErr: errors.New("scan boom")}, "node1", time.Hour, time.Hour)
	bad.refresh()
	if bad.snapshot() == nil {
		t.Fatal("snapshot must not be nil after failed refresh")
	}
}

func TestRegistryStartStopIdempotentAndLoop(t *testing.T) {
	kv := cluster.NewMemoryKV()
	now := time.Now().Unix()
	if err := cluster.Register(context.Background(), kv, &cluster.InstanceInfo{
		Name: "a", Hostname: "here", Addr: "127.0.0.1:1", LastHeartbeat: now,
	}); err != nil {
		t.Fatal(err)
	}
	// interval 很小：覆盖 loop 的 ticker 分支。
	reg := newInstanceRegistry(kv, "here", 10*time.Millisecond, time.Hour)
	reg.start() // 立即 refresh + 启动 loop
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(reg.snapshot().all) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(reg.snapshot().all) != 1 {
		t.Fatalf("start should refresh snapshot: %d", len(reg.snapshot().all))
	}
	time.Sleep(30 * time.Millisecond) // 让 loop 至少走一轮 ticker
	reg.stop()
	reg.stop() // 幂等：第二次走 select 提前返回
	if _, ok := reg.lookup("a"); !ok {
		t.Fatal("lookup after start must see instance")
	}
}

func TestPickerNoLiveInstance(t *testing.T) {
	reg := newInstanceRegistry(cluster.NewMemoryKV(), "node1", time.Hour, time.Hour)
	for _, routing := range []string{RouteLocal, RouteRoundRobin} {
		p := newInstancePicker(reg, 80, routing)
		if _, ok := p.pick(); ok {
			t.Fatalf("%s: pick must fail with no online instance", routing)
		}
	}
}

func TestPickerFallbackWhenAllOverThreshold(t *testing.T) {
	// 仅本地实例且全部超水位 → 本地随机兜底（缓存场景：宁可写满盘也不丢写入）。
	regLocal := registryWith(t, "node1", &cluster.InstanceInfo{
		Name: "l", Node: "node1", Hostname: "node1", Addr: "127.0.0.1:1", Used: 95, Capacity: 100,
	})
	p := newInstancePicker(regLocal, 80, RouteLocal)
	if inst, ok := p.pick(); !ok || inst.Name != "l" {
		t.Fatalf("local fallback: %+v ok=%v", inst, ok)
	}

	// 仅远端实例且全部超水位 → 全量随机兜底。
	regRemote := registryWith(t, "node1", &cluster.InstanceInfo{
		Name: "r", Node: "node2", Hostname: "node2", Addr: "127.0.0.1:2", Used: 95, Capacity: 100,
	})
	p2 := newInstancePicker(regRemote, 80, RouteLocal)
	if inst, ok := p2.pick(); !ok || inst.Name != "r" {
		t.Fatalf("all fallback: %+v ok=%v", inst, ok)
	}

	// round-robin 全超水位 → 兜底返回游标处实例。
	p3 := newInstancePicker(regRemote, 80, RouteRoundRobin)
	if inst, ok := p3.pick(); !ok || inst.Name != "r" {
		t.Fatalf("round-robin fallback: %+v ok=%v", inst, ok)
	}
}

func TestPickerDefaultsAndUsagePercent(t *testing.T) {
	if got := usagePercent(cluster.InstanceInfo{}); got != 0 {
		t.Fatalf("usagePercent(capacity=0)=%v want 0", got)
	}
	if got := usagePercent(cluster.InstanceInfo{Used: 50, Capacity: 200}); got != 25 {
		t.Fatalf("usagePercent=%v want 25", got)
	}
	// 阈值越界 → 默认 80；routing 非法 → local。
	p := newInstancePicker(nil, 0, "bogus")
	if p.threshold != 80 || p.routing != RouteLocal {
		t.Fatalf("defaults: %+v", p)
	}
	p = newInstancePicker(nil, 150, RouteRoundRobin)
	if p.threshold != 80 || p.routing != RouteRoundRobin {
		t.Fatalf("threshold clamp: %+v", p)
	}
}

func TestIndexManagerQueueFullAndStopIdempotent(t *testing.T) {
	m := newIndexManager(cluster.NewMemoryKV())
	// loop 未启动：队列（4096）写满后 put 走 default 丢弃，不阻塞。
	for i := 0; i < cap(m.ch)+16; i++ {
		m.put("k"+strconv.Itoa(i), "inst")
	}
	if len(m.ch) != cap(m.ch) {
		t.Fatalf("queue should be full: %d/%d", len(m.ch), cap(m.ch))
	}
	m.start()
	m.stop()
	m.stop() // 幂等：第二次直接返回
}

func TestIndexBatchPutFailureIsBestEffort(t *testing.T) {
	// BatchPut 失败：尽力而为（不 panic、不阻塞），索引缺失由回源兜底。
	kv := &errKV{KV: cluster.NewMemoryKV(), batchErr: errors.New("batch boom")}
	m := newIndexManager(kv)
	m.start()
	defer m.stop()
	m.put("k", "inst")
	time.Sleep(300 * time.Millisecond)
	name, err := m.get(context.Background(), "k")
	if err != nil || name != "" {
		t.Fatalf("index should stay empty when BatchPut fails: %q %v", name, err)
	}
}

func TestRouteCacheDefaultCapacityAndConcurrency(t *testing.T) {
	rc := newRouteCache(0)
	for i, sh := range rc.shards {
		if sh.cap != 4096 {
			t.Fatalf("shard %d cap=%d want 4096", i, sh.cap)
		}
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				k := "key-" + strconv.Itoa(i%64)
				rc.put(k, "inst")
				_, _ = rc.get(k)
				if i%7 == 0 {
					rc.delete(k)
				}
			}
		}()
	}
	wg.Wait()
	// 并发访问后仍可正常读写（分片锁保护下无数据竞争：go test -race 可复核）。
	rc.put("after", "inst-x")
	if name, ok := rc.get("after"); !ok || name != "inst-x" {
		t.Fatalf("get after concurrent access: %q ok=%v", name, ok)
	}
}
