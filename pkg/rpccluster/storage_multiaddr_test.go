package rpccluster

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/internal/transport"
)

// multiAddrTestServerPair 起一个跑在本机 TCP 上的 taihu-server（同一 storage），通过
// 2 个独立 listener（不同端口）模拟"多 IP 监听"——两个地址连的是同一实例、数据一致。
// 返回 ()[addrA, addrB]，供 clientFor 多地址均分验证。
func multiAddrTestServerPair(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	if err := os.WriteFile(devPath, nil, 0o644); err != nil {
		t.Fatalf("create device: %v", err)
	}
	storage, err := storage.NewStorage(context.Background(), filepath.Join(dir, "meta"), devPath,
		layout.Layout{SegmentSizeBytes: layout.DefaultSegmentSizeBytes, SegmentCount: 2048})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	gs := transport.NewServer(storage)
	lnA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen A: %v", err)
	}
	lnB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen B: %v", err)
	}
	go func() { _ = gs.Serve(lnA) }()
	go func() { _ = gs.Serve(lnB) }()
	t.Cleanup(func() {
		gs.Stop()
		_ = storage.Close()
		_ = lnA.Close()
		_ = lnB.Close()
	})
	return lnA.Addr().String(), lnB.Addr().String()
}

// multiAddrTestServer 起一个跑在本机 TCP 上的 taihu-server（单 listener，127.0.0.1:0），
// 返回监听地址；用于单地址（回退/缓存）路径验证。
func multiAddrTestServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	if err := os.WriteFile(devPath, nil, 0o644); err != nil {
		t.Fatalf("create device: %v", err)
	}
	storage, err := storage.NewStorage(context.Background(), filepath.Join(dir, "meta"), devPath,
		layout.Layout{SegmentSizeBytes: layout.DefaultSegmentSizeBytes, SegmentCount: 2048})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	gs := transport.NewServer(storage)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(func() {
		gs.Stop()
		_ = storage.Close()
		_ = ln.Close()
	})
	return ln.Addr().String()
}

// newClientForStorage 构造集群客户端并注册远程实例（hostname 与本机不同 → 客户端走 TCP）。
// 需要真实 server 地址（起 server 后传地址）。
func newClientForStorage(t *testing.T, inst *cluster.InstanceInfo) *Storage {
	t.Helper()
	kv := cluster.NewMemoryKV()
	inst.StartTime = time.Now().Unix()
	inst.LastHeartbeat = time.Now().Unix()
	if err := cluster.Register(context.Background(), kv, inst); err != nil {
		t.Fatalf("register: %v", err)
	}
	s, err := NewCluster(ClusterConfig{
		KV:         kv,
		ClientName: "test-client",
		Conns:      1,
	})
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// 确保发现已刷新到实例
	s.registry.refresh()
	return s
}

func TestClientForMultiAddrConns(t *testing.T) {
	addrA, addrB := multiAddrTestServerPair(t)

	// hostname 用 "remote" 与本机不同 → 客户端 clientFor 走跨节点 TCP 分支。
	inst := &cluster.InstanceInfo{
		Name: "multi", Node: "remote", Hostname: "remote",
		Addr:  addrA,
		Addrs: []string{addrA, addrB},
	}
	s := newClientForStorage(t, inst)

	sc, err := s.clientFor(*inst)
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	conns := reflect.ValueOf(sc).Elem().FieldByName("conns")
	if n := conns.Len(); n != 2 {
		t.Fatalf("conns=%d want 2 (2 addrs x Conns=1)", n)
	}
	// round-trip：经多地址连接池写读一致。
	payload := []byte("multiaddr-clientfor-smoke")
	if err := sc.Put(context.Background(), "k1", int64(len(payload)), payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, rel, err := sc.Get(context.Background(), "k1", 0, int64(len(payload)))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("Get len=%d want %d", len(got), len(payload))
	}
	rel()
}

func TestClientForFallbackSingleAddr(t *testing.T) {
	addr := multiAddrTestServer(t)

	inst := &cluster.InstanceInfo{
		Name: "single", Node: "remote", Hostname: "remote",
		Addr: addr, // Addrs nil → 回退单地址
	}
	s := newClientForStorage(t, inst)

	sc, err := s.clientFor(*inst)
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	conns := reflect.ValueOf(sc).Elem().FieldByName("conns")
	if n := conns.Len(); n != 1 {
		t.Fatalf("conns=%d want 1 (single addr x Conns=1)", n)
	}
}

func TestClientForConnectionCaching(t *testing.T) {
	addr := multiAddrTestServer(t)

	inst := &cluster.InstanceInfo{
		Name: "cache", Node: "remote", Hostname: "remote",
		Addr: addr, Addrs: []string{addr},
	}
	s := newClientForStorage(t, inst)

	c1, err := s.clientFor(*inst)
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	c2, err := s.clientFor(*inst)
	if err != nil {
		t.Fatalf("clientFor 2nd: %v", err)
	}
	if c1 != c2 {
		t.Fatal("clientFor should return cached *Storage")
	}
}
