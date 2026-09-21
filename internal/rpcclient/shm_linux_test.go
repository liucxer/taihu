//go:build linux

package rpcclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/internal/transport"
)

// shm 路径（DialShm / NewPut 零拷贝写）需要 per-session 共享内存 + memfd，
// 只有 linux 可用；非 linux 由 dial_shm_other.go / putwriter_other.go 的占位实现覆盖。
// 本文件在进程内起真实 shmipc 服务端（transport.ServeShm），不依赖外部实例。

// newTestShmServer 起一个本机 shmipc 服务端（unix socket + memfd 共享内存），
// 返回 uds 与关闭函数。复用 pool_test.go 的 storage 构造方式。
func newTestShmServer(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	f, err := os.Create(devPath)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	_ = f.Close()

	st, err := storage.NewStorage(context.Background(), filepath.Join(dir, "meta"), devPath,
		layout.Layout{SegmentSizeBytes: layout.DefaultSegmentSizeBytes, SegmentCount: 2048})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	uds := filepath.Join(dir, "taihu.sock")
	srv, err := transport.ServeShm(st, uds)
	if err != nil {
		_ = st.Close()
		t.Fatalf("ServeShm: %v", err)
	}
	return uds, func() {
		_ = srv.Close()
		_ = st.Close()
	}
}

// TestDialShmRoundTrip 覆盖 DialShm/DialShmPool 与 shm 上的数据面往返。
func TestDialShmRoundTrip(t *testing.T) {
	uds, cleanup := newTestShmServer(t)
	defer cleanup()

	s, err := DialShm(context.Background(), uds)
	if err != nil {
		t.Fatalf("DialShm: %v", err)
	}
	defer s.Close()
	if len(s.conns) != 1 {
		t.Fatalf("conns = %d, want 1", len(s.conns))
	}

	ctx := context.Background()
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := s.Put(ctx, "shm/rt", int64(len(payload)), payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, rel, err := s.Get(ctx, "shm/rt", 0, int64(len(payload)))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rel()
	if len(got) != len(payload) {
		t.Fatalf("Get len=%d want %d", len(got), len(payload))
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("Get 数据不一致 @%d", i)
		}
	}
	sz, err := s.Stat(ctx, "shm/rt")
	if err != nil || sz != int64(len(payload)) {
		t.Fatalf("Stat sz=%d err=%v", sz, err)
	}
	if err := s.Delete(ctx, "shm/rt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat(ctx, "shm/rt"); err != ErrNotFound {
		t.Fatalf("Stat 已删 key = %v, want ErrNotFound", err)
	}
}

// TestDialShmPoolSessionsFloor 覆盖 DialShmPool 的 sessions < 1 下限（抬升为 1）。
func TestDialShmPoolSessionsFloor(t *testing.T) {
	uds, cleanup := newTestShmServer(t)
	defer cleanup()

	s, err := DialShmPool(context.Background(), uds, 0)
	if err != nil {
		t.Fatalf("DialShmPool(sessions=0): %v", err)
	}
	defer s.Close()
	if len(s.conns) != 1 {
		t.Fatalf("conns = %d, want 1", len(s.conns))
	}
}

// TestNewPutShmZeroCopy 覆盖 NewPut → Reserve 直写共享内存 → Commit 的完整零拷贝写路径。
func TestNewPutShmZeroCopy(t *testing.T) {
	uds, cleanup := newTestShmServer(t)
	defer cleanup()

	s, err := DialShm(context.Background(), uds)
	if err != nil {
		t.Fatalf("DialShm: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	const size = 4096
	w, err := s.NewPut(ctx, "shm/zc", size)
	if err != nil {
		t.Fatalf("NewPut: %v", err)
	}
	buf, err := w.Reserve(size)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if len(buf) != size {
		t.Fatalf("Reserve len=%d want %d", len(buf), size)
	}
	for i := range buf {
		buf[i] = byte(i * 7)
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	sz, err := s.Stat(ctx, "shm/zc")
	if err != nil || sz != size {
		t.Fatalf("Stat sz=%d err=%v", sz, err)
	}
}

// TestNewPutShmWriterEdges 覆盖零拷贝写的非法调用与边界：
//   - NewPut 的 size 为负 → PutBegin 立即拒绝（不落盘）；
//   - Reserve 后少写 → Commit 报 ErrShortWrite。
func TestNewPutShmWriterEdges(t *testing.T) {
	uds, cleanup := newTestShmServer(t)
	defer cleanup()

	s, err := DialShm(context.Background(), uds)
	if err != nil {
		t.Fatalf("DialShm: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if _, err := s.NewPut(ctx, "shm/bad", -1); err == nil {
		t.Fatal("NewPut(size=-1) 应报错")
	}

	w, err := s.NewPut(ctx, "shm/short", 8192)
	if err != nil {
		t.Fatalf("NewPut: %v", err)
	}
	if _, err := w.Reserve(4096); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := w.Commit(); err != ErrShortWrite {
		t.Fatalf("Commit 少写 = %v, want ErrShortWrite", err)
	}
}
