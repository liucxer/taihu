package rpcclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/internal/transport"
)

// newTestServer 起一个跑在本机 TCP 上的 taihu-server（netpoll EventLoop），
// 返回监听地址和关闭函数。
func newTestServer(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	f, err := os.Create(devPath)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	_ = f.Close()

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
	go func() {
		_ = gs.Serve(ln)
	}()
	return ln.Addr().String(), func() {
		gs.Stop()
		_ = storage.Close()
		_ = ln.Close()
	}
}

func TestDialPoolRoundTrip(t *testing.T) {
	addr, cleanup := newTestServer(t)
	defer cleanup()

	s, err := DialPool(context.Background(), addr, 4)
	if err != nil {
		t.Fatalf("DialPool: %v", err)
	}
	defer s.Close()

	// 4 连接 × 多对象交错读写，覆盖 round-robin 分发到不同连接。
	payload := bytes.Repeat([]byte("taihu-pool-smoke"), 4096) // ~64KiB
	if os.Getenv("BIG4M") != "" {
		payload = make([]byte, 4194304)
		for i := range payload {
			payload[i] = byte(i)
		}
	}
	for i := 0; i < 32; i++ {
		key := "pool/" + string(rune('a'+i%26)) + string(rune('0'+i%10))
		if err := s.Put(context.Background(), key, int64(len(payload)), payload); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
		// Get 读回，验证 round-trip 数据一致。
		got, rel, err := s.Get(context.Background(), key, 0, int64(len(payload)))
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("Get mismatch key=%s got=%dB want=%dB", key, len(got), len(payload))
		}
		if got != nil {
			rel() // Get 返回私有缓冲，用毕 release() 归还
		}
		sz, err := s.Stat(context.Background(), key)
		if err != nil || sz != int64(len(payload)) {
			t.Fatalf("Stat %s: sz=%d err=%v", key, sz, err)
		}
		if err := s.Delete(context.Background(), key); err != nil {
			t.Fatalf("Delete %s: %v", key, err)
		}
	}
}

// TestDialPoolMultiFrame 验证 >4MiB（多帧）对象的 Get 拷贝路径能正确跨帧汇并。
// ChunkSize=4MiB，对象取 4MiB+1 与 ~10MiB 两种尺寸，覆盖跨 2 帧及多帧场景，并做
// SHA256 校验确保帧间拼接无遗漏/错位。
func TestDialPoolMultiFrame(t *testing.T) {
	addr, cleanup := newTestServer(t)
	defer cleanup()

	s, err := DialPool(context.Background(), addr, 2)
	if err != nil {
		t.Fatalf("DialPool: %v", err)
	}
	defer s.Close()

	sizes := []int64{1<<22 + 1, (1 << 23) + (1 << 22) + 999} // 4MiB+1, ~10MiB+999
	for _, size := range sizes {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i * 31)
		}
		key := fmt.Sprintf("multi/%d", size)
		if err := s.Put(context.Background(), key, size, payload); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
		got, rel, err := s.Get(context.Background(), key, 0, size)
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		gotHash := sha256.Sum256(got)
		expHash := sha256.Sum256(payload)
		if gotHash != expHash {
			t.Fatalf("Get mismatch key=%s size=%d got sha256=%x want=%x",
				key, len(got), gotHash, expHash)
		}
		if int64(len(got)) != size {
			t.Fatalf("Get short key=%s got=%dB want=%dB", key, len(got), size)
		}
		rel()
		sz, err := s.Stat(context.Background(), key)
		if err != nil || sz != size {
			t.Fatalf("Stat %s: sz=%d err=%v", key, sz, err)
		}
	}
}

// newTestServerMultiAddr 起一个跑在本机 TCP 上的 taihu-server（同一 storage），
// 通过 2 个独立 listener（不同端口）模拟"多 IP 监听"——两个地址连的是同一实例，
// 数据一致，round-robin 到任一地址读写均命中。返回两个监听地址。
func newTestServerMultiAddr(t *testing.T) (string, string, func()) {
	t.Helper()
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	f, err := os.Create(devPath)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	_ = f.Close()

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
	// 同一 Server 多 listener 并发（对应 server 多 IP 监听语义，见 internal/transport.Serve）。
	go func() { _ = gs.Serve(lnA) }()
	go func() { _ = gs.Serve(lnB) }()
	return lnA.Addr().String(), lnB.Addr().String(), func() {
		gs.Stop()
		_ = storage.Close()
		_ = lnA.Close()
		_ = lnB.Close()
	}
}

// TestDialPoolMultiRoundTrip 验证多地址连接池：对同一 server 的 2 个监听地址各建 3 条连接
// （共 6 条），round-trip 读写覆盖全部连接（数据一致 → 均分到任一地址都能命中）。
func TestDialPoolMultiRoundTrip(t *testing.T) {
	addrA, addrB, cleanup := newTestServerMultiAddr(t)
	defer cleanup()

	s, err := DialPoolMulti(context.Background(), []string{addrA, addrB}, 3)
	if err != nil {
		t.Fatalf("DialPoolMulti: %v", err)
	}
	defer s.Close()
	if len(s.conns) != 6 {
		t.Fatalf("conns = %d, want 6 (2 addrs x 3 perAddr)", len(s.conns))
	}

	payload := bytes.Repeat([]byte("taihu-multiaddr"), 512)
	for i := 0; i < 48; i++ {
		key := fmt.Sprintf("multiaddr/%d", i)
		if err := s.Put(context.Background(), key, int64(len(payload)), payload); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
		got, rel, err := s.Get(context.Background(), key, 0, int64(len(payload)))
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("Get mismatch key=%s", key)
		}
		rel()
	}
}

// TestDialPoolMultiEmptyAddrs 验证空地址列表直接报错（不构造空连接池）。
func TestDialPoolMultiEmptyAddrs(t *testing.T) {
	if _, err := DialPoolMulti(context.Background(), nil, 2); err == nil {
		t.Fatal("DialPoolMulti(nil) should error")
	}
}
