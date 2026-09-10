package taihu

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/liucxer/taihu/internal/bufpool"
)

// alignedPayload 返回 n 个字节的测试负载，n 须为 BlockSize 整数倍。
func alignedPayload(n int) []byte {
	if n%int(BlockSize) != 0 {
		panic("alignedPayload requires 4K multiple")
	}
	b := make([]byte, n)
	copy(b, "taihu odirect object storage")
	return b
}

func newTestStorage(t *testing.T) (*Storage, string, string) {
	t.Helper()
	dir := t.TempDir()
	rocksdbDir := filepath.Join(dir, "meta")
	devPath := filepath.Join(dir, "nvme.img")
	f, err := os.Create(devPath)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	_ = f.Close()

	s, err := NewStorage(context.Background(), rocksdbDir, devPath)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	return s, rocksdbDir, devPath
}

func TestStoragePutGetRoundTrip(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()

	payload := alignedPayload(4096)
	if err := s.Put(context.Background(), "obj/1", int64(len(payload)), payload); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.ReadAt(context.Background(), "obj/1", 0, int64(len(payload)))
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	defer bufpool.Put(got)
	if !bytes.Equal(got, payload) {
		t.Fatalf("round trip mismatch: got %d bytes want %d", len(got), len(payload))
	}
}

func TestStoragePutShortData(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()

	// 数据不足声明 size 应报错，且不留下映射。
	if err := s.Put(context.Background(), "short", 4096, make([]byte, 100)); err != ErrShortWrite {
		t.Fatalf("Put short: err=%v want ErrShortWrite", err)
	}
	if _, err := s.ReadAt(context.Background(), "short", 0, 4096); err != ErrNotFound {
		t.Fatalf("short object should not exist, err=%v", err)
	}
}

func TestStorageReadRange(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()

	payload := alignedPayload(2 * int(BlockSize)) // 8192 B
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := s.Put(context.Background(), "obj", int64(len(payload)), payload); err != nil {
		t.Fatal(err)
	}

	// 对齐子区间读
	got, err := s.ReadAt(context.Background(), "obj", 0, int64(BlockSize))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload[:BlockSize]) {
		t.Fatal("aligned sub-range mismatch")
	}
	bufpool.Put(got)

	// 越过对象结尾：截断到剩余字节并附 io.EOF
	got2, err := s.ReadAt(context.Background(), "obj", int64(BlockSize), int64(4*BlockSize))
	if !bytes.Equal(got2, payload[BlockSize:]) {
		t.Fatalf("clamped tail mismatch: got %dB", len(got2))
	}
	if err != io.EOF {
		t.Fatalf("clamped tail err=%v want io.EOF", err)
	}
	bufpool.Put(got2)

	// 恰好的末尾子区间读
	got3, err := s.ReadAt(context.Background(), "obj", int64(BlockSize), int64(BlockSize))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got3, payload[BlockSize:]) {
		t.Fatal("tail sub-range mismatch")
	}
	bufpool.Put(got3)

	// 非对齐 off/size 也能读（Storage 层吸收对齐），数据须正确
	got4, err := s.ReadAt(context.Background(), "obj", 100, 2048)
	if err != nil {
		t.Fatalf("unaligned off/size ReadAt: %v", err)
	}
	if !bytes.Equal(got4, payload[100:100+2048]) {
		t.Fatal("unaligned sub-range mismatch")
	}
	bufpool.Put(got4)

	// off == Size：剩余 0 → io.EOF
	if _, err := s.ReadAt(context.Background(), "obj", int64(len(payload)), 1); err != io.EOF {
		t.Fatalf("off==Size err=%v want io.EOF", err)
	}
	// off > Size 越界
	if _, err := s.ReadAt(context.Background(), "obj", int64(len(payload)+1), 1); err != ErrInvalidRange {
		t.Fatalf("off>Size err=%v want ErrInvalidRange", err)
	}
}

func TestStorageDelete(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()

	payload := alignedPayload(int(BlockSize))
	if err := s.Put(context.Background(), "d", int64(len(payload)), payload); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadAt(context.Background(), "d", 0, int64(BlockSize)); err != ErrNotFound {
		t.Fatalf("after delete ReadAt err=%v want ErrNotFound", err)
	}
	if err := s.Delete(context.Background(), "d"); err != ErrNotFound {
		t.Fatalf("double delete err=%v want ErrNotFound", err)
	}
}

func TestStorageNotFound(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	if _, err := s.ReadAt(context.Background(), "nope", 0, int64(BlockSize)); err != ErrNotFound {
		t.Fatalf("ReadAt missing err=%v want ErrNotFound", err)
	}
}

func TestStorageRestartPreservesCursor(t *testing.T) {
	dir := t.TempDir()
	rocksdbDir := filepath.Join(dir, "meta")
	devPath := filepath.Join(dir, "nvme.img")
	f, _ := os.Create(devPath)
	_ = f.Close()

	s1, err := NewStorage(context.Background(), rocksdbDir, devPath)
	if err != nil {
		t.Fatal(err)
	}
	payload := alignedPayload(int(BlockSize))
	if err := s1.Put(context.Background(), "k", int64(len(payload)), payload); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStorage(context.Background(), rocksdbDir, devPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	// 重启后读旧对象
	b1, err := s2.ReadAt(context.Background(), "k", 0, int64(len(payload)))
	if err != nil {
		t.Fatalf("restart read old: %v", err)
	}
	if !bytes.Equal(b1, payload) {
		t.Fatal("restart old data mismatch")
	}
	bufpool.Put(b1)

	// 重启后续写（新对象起始于老对象 4K 对齐之后的下一位置）
	payload2 := alignedPayload(int(BlockSize))
	copy(payload2, "after restart")
	if err := s2.Put(context.Background(), "k2", int64(len(payload2)), payload2); err != nil {
		t.Fatalf("restart put: %v", err)
	}
	b2, err := s2.ReadAt(context.Background(), "k2", 0, int64(len(payload2)))
	if err != nil {
		t.Fatalf("restart get new: %v", err)
	}
	if !bytes.Equal(b2, payload2) {
		t.Fatal("restart new data mismatch")
	}
	bufpool.Put(b2)
}

func TestStorageLoadCache(t *testing.T) {
	s, rocksdbDir, devPath := newTestStorage(t)

	// 写入若干对象后关闭，用全新 Storage 验证 LoadCache 能从 pebble 重建缓存
	payload := alignedPayload(int(BlockSize))
	keys := []string{"k1", "k2", "k3"}
	for _, k := range keys {
		if err := s.Put(context.Background(), k, int64(len(payload)), payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStorage(context.Background(), rocksdbDir, devPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if err := s2.LoadCache(context.Background()); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}

	// 预热后读取全部能正确命中
	for _, k := range keys {
		got, err := s2.ReadAt(context.Background(), k, 0, int64(len(payload)))
		if err != nil {
			t.Fatalf("ReadAt %s after LoadCache: %v", k, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("ReadAt %s mismatch after LoadCache", k)
		}
		bufpool.Put(got)
	}
}
