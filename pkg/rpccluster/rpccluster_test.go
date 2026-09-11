package rpccluster

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
)

func TestRouteCacheBasic(t *testing.T) {
	rc := NewRouteCache(2)
	// cap 为每分片容量：选 3 个映射到同一分片的 key 验证 LRU 淘汰。
	sh := rc.shard("seed")
	sameShard := func(prefix string) string {
		for i := 0; ; i++ {
			k := prefix + strconv.Itoa(i)
			if rc.shard(k) == sh {
				return k
			}
		}
	}
	k1, k2, k3 := sameShard("k1-"), sameShard("k2-"), sameShard("k3-")
	rc.Put(k1, "inst1")
	if name, ok := rc.Get(k1); !ok || name != "inst1" {
		t.Fatalf("get %s: %q %v", k1, name, ok)
	}
	rc.Put(k2, "inst2")
	rc.Put(k3, "inst3") // 超容量 → 淘汰最久未用 k1
	if _, ok := rc.Get(k1); ok {
		t.Fatalf("%s should be evicted", k1)
	}
	if name, ok := rc.Get(k2); !ok || name != "inst2" {
		t.Fatalf("get %s: %q %v", k2, name, ok)
	}
	rc.Delete(k2)
	if _, ok := rc.Get(k2); ok {
		t.Fatalf("%s should be deleted", k2)
	}
	// 更新已有项
	rc.Put(k3, "inst3-new")
	if name, _ := rc.Get(k3); name != "inst3-new" {
		t.Fatalf("k3 update: %q", name)
	}
}

func TestRegistryDiscovery(t *testing.T) {
	kv := cluster.NewMemoryKV()
	ctx := context.Background()
	now := time.Now()
	// 两个在线（node1/node2 各一）+ 一个心跳超时离线
	for _, inst := range []*cluster.InstanceInfo{
		{Name: "a", Node: "node1", Hostname: "node1", Addr: "127.0.0.1:1", LastHeartbeat: now.Unix()},
		{Name: "b", Node: "node2", Hostname: "node2", Addr: "127.0.0.1:2", LastHeartbeat: now.Unix()},
		{Name: "c", Node: "node1", Hostname: "node1", Addr: "127.0.0.1:3", LastHeartbeat: now.Add(-10 * time.Second).Unix()},
	} {
		if err := cluster.Register(ctx, kv, inst); err != nil {
			t.Fatal(err)
		}
	}
	reg := NewInstanceRegistry(kv, "node1", time.Hour, 5*time.Second)
	reg.refresh()
	snap := reg.Snapshot()
	if len(snap.all) != 2 {
		t.Fatalf("all=%d want 2", len(snap.all))
	}
	if len(snap.local) != 1 || snap.local[0].Name != "a" {
		t.Fatalf("local=%+v", snap.local)
	}
	if len(snap.remote) != 1 || snap.remote[0].Name != "b" {
		t.Fatalf("remote=%+v", snap.remote)
	}
	if _, ok := reg.Lookup("c"); ok {
		t.Fatal("offline instance must not be lookup-able")
	}
}

func TestPickerLocalFirstAndHealthy(t *testing.T) {
	kv := cluster.NewMemoryKV()
	ctx := context.Background()
	now := time.Now()
	reg := NewInstanceRegistry(kv, "node1", time.Hour, 5*time.Second)
	regs := func(name, node string, used, cap int64) {
		if err := cluster.Register(ctx, kv, &cluster.InstanceInfo{
			Name: name, Node: node, Hostname: node, Addr: "127.0.0.1:9",
			Used: used, Capacity: cap, LastHeartbeat: now.Unix(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	regs("a-full", "node1", 90, 100) // 本地但超水位
	regs("b-ok", "node1", 10, 100)   // 本地健康
	regs("c-ok", "node2", 10, 100)   // 远端健康
	reg.refresh()
	p := NewInstancePicker(reg, 80, RouteLocal)
	for i := 0; i < 50; i++ {
		inst, ok := p.Pick()
		if !ok {
			t.Fatal("pick failed")
		}
		if inst.Name == "a-full" {
			t.Fatal("over-threshold instance picked")
		}
		if inst.Name != "b-ok" {
			t.Fatalf("expected local healthy b-ok, got %s", inst.Name)
		}
	}
}

func TestPickerRoundRobin(t *testing.T) {
	kv := cluster.NewMemoryKV()
	ctx := context.Background()
	now := time.Now()
	reg := NewInstanceRegistry(kv, "node1", time.Hour, 5*time.Second)
	for _, inst := range []*cluster.InstanceInfo{
		{Name: "a", Node: "node1", Hostname: "node1", Addr: "127.0.0.1:9", Used: 10, Capacity: 100, LastHeartbeat: now.Unix()},
		{Name: "b", Node: "node2", Hostname: "node2", Addr: "127.0.0.1:8", Used: 10, Capacity: 100, LastHeartbeat: now.Unix()},
		{Name: "c", Node: "node3", Hostname: "node3", Addr: "127.0.0.1:7", Used: 10, Capacity: 100, LastHeartbeat: now.Unix()},
	} {
		if err := cluster.Register(ctx, kv, inst); err != nil {
			t.Fatal(err)
		}
	}
	reg.refresh()
	// 3 个健康实例：轮询 12 次应均分，每实例恰 4 次。
	p := NewInstancePicker(reg, 80, RouteRoundRobin)
	counts := map[string]int{}
	for i := 0; i < 12; i++ {
		inst, ok := p.Pick()
		if !ok {
			t.Fatal("pick failed")
		}
		counts[inst.Name]++
	}
	for name, want := range map[string]int{"a": 4, "b": 4, "c": 4} {
		if counts[name] != want {
			t.Fatalf("round-robin %s: got %d want %d (counts=%v)", name, counts[name], want, counts)
		}
	}
}

func TestPickerRoundRobinSkipsFull(t *testing.T) {
	kv := cluster.NewMemoryKV()
	ctx := context.Background()
	now := time.Now()
	reg := NewInstanceRegistry(kv, "node1", time.Hour, 5*time.Second)
	for _, inst := range []*cluster.InstanceInfo{
		{Name: "full", Node: "node1", Hostname: "node1", Addr: "127.0.0.1:9", Used: 95, Capacity: 100, LastHeartbeat: now.Unix()},
		{Name: "ok", Node: "node2", Hostname: "node2", Addr: "127.0.0.1:8", Used: 10, Capacity: 100, LastHeartbeat: now.Unix()},
	} {
		if err := cluster.Register(ctx, kv, inst); err != nil {
			t.Fatal(err)
		}
	}
	reg.refresh()
	p := NewInstancePicker(reg, 80, RouteRoundRobin)
	for i := 0; i < 20; i++ {
		inst, ok := p.Pick()
		if !ok || inst.Name != "ok" {
			t.Fatalf("round-robin should skip full instance, got %+v ok=%v", inst, ok)
		}
	}
}

func TestPickerFullLocalFallbackRemote(t *testing.T) {
	kv := cluster.NewMemoryKV()
	ctx := context.Background()
	now := time.Now()
	reg := NewInstanceRegistry(kv, "node1", time.Hour, 5*time.Second)
	for _, inst := range []*cluster.InstanceInfo{
		{Name: "a-full", Node: "node1", Hostname: "node1", Addr: "127.0.0.1:9", Used: 95, Capacity: 100, LastHeartbeat: now.Unix()},
		{Name: "c-ok", Node: "node2", Hostname: "node2", Addr: "127.0.0.1:8", Used: 10, Capacity: 100, LastHeartbeat: now.Unix()},
	} {
		if err := cluster.Register(ctx, kv, inst); err != nil {
			t.Fatal(err)
		}
	}
	reg.refresh()
	p := NewInstancePicker(reg, 80, RouteLocal)
	for i := 0; i < 20; i++ {
		inst, ok := p.Pick()
		if !ok || inst.Name != "c-ok" {
			t.Fatalf("expected remote c-ok, got %+v ok=%v", inst, ok)
		}
	}
}

func TestIndexManager(t *testing.T) {
	kv := cluster.NewMemoryKV()
	m := NewIndexManager(kv)
	m.Start()
	defer m.Stop()

	for i := 0; i < 300; i++ { // 超过 indexBatchMax，触发多批 flush
		m.Put("key"+strconv.Itoa(i), "inst"+strconv.Itoa(i))
	}
	m.Put("last", "inst-last")
	time.Sleep(300 * time.Millisecond)

	ctx := context.Background()
	for i := 0; i < 300; i++ {
		name, err := m.Get(ctx, "key"+strconv.Itoa(i))
		if err != nil || name != "inst"+strconv.Itoa(i) {
			t.Fatalf("index %d: %q %v", i, name, err)
		}
	}
	if name, err := m.Get(ctx, "last"); err != nil || name != "inst-last" {
		t.Fatalf("index last: %q %v", name, err)
	}
	if err := m.Delete(ctx, "key0"); err != nil {
		t.Fatal(err)
	}
	if name, _ := m.Get(ctx, "key0"); name != "" {
		t.Fatalf("deleted index still present: %q", name)
	}
}
