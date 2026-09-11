package cluster

import (
	"context"
	"testing"
	"time"
)

func TestMemoryKVBasic(t *testing.T) {
	kv := NewMemoryKV()
	ctx := context.Background()

	if err := kv.Put(ctx, []byte("a"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	v, err := kv.Get(ctx, []byte("a"))
	if err != nil || string(v) != "1" {
		t.Fatalf("Get a: %v %q", err, v)
	}
	if v, err := kv.Get(ctx, []byte("missing")); err != nil || v != nil {
		t.Fatalf("missing should be (nil, nil), got %q %v", v, err)
	}
	if err := kv.Delete(ctx, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if v, _ := kv.Get(ctx, []byte("a")); v != nil {
		t.Fatal("deleted key still present")
	}
}

func TestMemoryKVScanBatch(t *testing.T) {
	kv := NewMemoryKV()
	ctx := context.Background()
	kvs := map[string][]byte{
		"/p/x1": []byte("v1"),
		"/p/x2": []byte("v2"),
		"/q/x3": []byte("v3"),
	}
	if err := kv.BatchPut(ctx, kvs); err != nil {
		t.Fatal(err)
	}
	start, end := []byte("/p"), []byte("/p\xff")
	keys, values, err := kv.Scan(ctx, start, end, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || string(keys[0]) != "/p/x1" || string(values[0]) != "v1" {
		t.Fatalf("scan: keys=%q values=%q", keys, values)
	}
	// limit 生效
	keys, _, _ = kv.Scan(ctx, start, end, 1)
	if len(keys) != 1 {
		t.Fatalf("scan limit=1 got %d", len(keys))
	}
	// BatchGet
	vals, err := kv.BatchGet(ctx, [][]byte{[]byte("/p/x1"), []byte("/nope")})
	if err != nil || len(vals) != 2 || string(vals[0]) != "v1" || vals[1] != nil {
		t.Fatalf("batchget: %v %v", vals, err)
	}
}

func TestRegisterListUnregister(t *testing.T) {
	kv := NewMemoryKV()
	ctx := context.Background()
	info := &InstanceInfo{
		Name: "n1", Node: "node1", Addr: "127.0.0.1:50051",
		Capacity: 100, Available: 60, Used: 40,
		StartTime: time.Now().Unix(), LastHeartbeat: time.Now().Unix(),
	}
	if err := Register(ctx, kv, info); err != nil {
		t.Fatal(err)
	}
	all, err := ListInstances(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Name != "n1" || all[0].Used != 40 {
		t.Fatalf("list: %+v", all)
	}
	got, err := GetInstance(ctx, kv, "n1")
	if err != nil || got == nil || got.Node != "node1" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if err := Unregister(ctx, kv, "n1"); err != nil {
		t.Fatal(err)
	}
	got, err = GetInstance(ctx, kv, "n1")
	if err != nil || got != nil {
		t.Fatalf("after unregister: %v %+v", err, got)
	}
}

func TestAlivenessByLastHeartbeat(t *testing.T) {
	now := time.Now()
	alive := &InstanceInfo{LastHeartbeat: now.Unix()}
	if !alive.Aliveness(now, 5*time.Second) {
		t.Fatal("fresh heartbeat should be alive")
	}
	stale := &InstanceInfo{LastHeartbeat: now.Add(-10 * time.Second).Unix()}
	if stale.Aliveness(now, 5*time.Second) {
		t.Fatal("stale heartbeat should be offline")
	}
}
