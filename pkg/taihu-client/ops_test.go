package taihuclient

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
)

// ops.go 覆盖：在线判定 / 用量上报 / 索引区枚举（含 KV 错误透出）。

func TestOpsHasLiveAndUsage(t *testing.T) {
	kv := cluster.NewMemoryKV()
	s := newClusterWithKV(t, kv, func(c *ClusterConfig) { c.HeartbeatTimeout = time.Hour })

	// 空集群：无在线实例。
	if s.HasLive() {
		t.Fatal("empty cluster must have no live instance")
	}
	if err := s.CheckPoolIsValid(); !errors.Is(err, ErrNoInstances) {
		t.Fatalf("CheckPoolIsValid empty: %v", err)
	}
	if _, err := s.UsageGet(); !errors.Is(err, ErrNoInstances) {
		t.Fatalf("UsageGet empty: %v", err)
	}

	// 未上报容量（Capacity<=0）不产生用量，但实例仍在线。
	registerInstances(t, s, &cluster.InstanceInfo{
		Name: "no-cap", Node: "remote", Hostname: "remote-node", Addr: "127.0.0.1:1",
	})
	if !s.HasLive() {
		t.Fatal("expected live instance")
	}
	if err := s.CheckPoolIsValid(); err != nil {
		t.Fatalf("CheckPoolIsValid: %v", err)
	}
	if _, err := s.UsageGet(); !errors.Is(err, ErrNoInstances) {
		t.Fatalf("UsageGet with capacity<=0: %v", err)
	}

	// 本地 20% / 远端 60%：取最低水位。
	registerInstances(t, s,
		&cluster.InstanceInfo{Name: "near", Node: "remote", Hostname: "remote-node", Addr: "127.0.0.1:2", Used: 20, Capacity: 100},
		&cluster.InstanceInfo{Name: "far", Node: "other", Hostname: "other-node", Addr: "127.0.0.1:3", Used: 60, Capacity: 100},
	)
	ratio, err := s.UsageGet()
	if err != nil {
		t.Fatalf("UsageGet: %v", err)
	}
	if ratio != 0.2 {
		t.Fatalf("UsageGet=%v want 0.2", ratio)
	}
}

func TestOpsListIndexKeys(t *testing.T) {
	ctx := context.Background()
	kv := cluster.NewMemoryKV()
	s := newClusterWithKV(t, kv, nil)

	for _, k := range []string{"a", "b", "other/x"} {
		if err := kv.Put(ctx, []byte(cluster.IndexKeyPrefix+k), []byte("inst")); err != nil {
			t.Fatalf("seed index %s: %v", k, err)
		}
	}
	// 非索引区（实例注册）不应被枚举出来。
	if err := kv.Put(ctx, []byte(cluster.InstanceKeyPrefix+"i"), []byte("{}")); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListIndexKeys(ctx, "")
	if err != nil {
		t.Fatalf("ListIndexKeys: %v", err)
	}
	if want := []string{"a", "b", "other/x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListIndexKeys=%v want %v", got, want)
	}

	got, err = s.ListIndexKeys(ctx, "other/")
	if err != nil {
		t.Fatalf("ListIndexKeys prefix: %v", err)
	}
	if want := []string{"other/x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListIndexKeys(prefix)=%v want %v", got, want)
	}

	// 无匹配前缀：返回空切片而非报错。
	if got, err = s.ListIndexKeys(ctx, "nothing/"); err != nil || len(got) != 0 {
		t.Fatalf("ListIndexKeys(no match)=%v %v", got, err)
	}

	// Scan 失败透出底层错误。
	se := newClusterWithKV(t, &errKV{KV: kv, scanErr: errors.New("scan boom")}, nil)
	if _, err := se.ListIndexKeys(ctx, ""); err == nil {
		t.Fatal("want scan error")
	}
}
