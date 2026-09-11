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
	"github.com/liucxer/taihu/internal/rpcserver"
	"github.com/liucxer/taihu/pkg/taihu"
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

	storage, err := taihu.NewStorage(context.Background(), filepath.Join(dir, "meta"), devPath,
		layout.Layout{SegmentSizeBytes: layout.DefaultSegmentSizeBytes, SegmentCount: 2048})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	gs := rpcserver.New(storage)
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
// chunkSize=4MiB，对象取 4MiB+1 与 ~10MiB 两种尺寸，覆盖跨 2 帧及多帧场景，并做
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
