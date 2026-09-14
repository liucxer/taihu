package taihu

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
)

// testLayout 测试用默认布局（段大小 8GB、段数 2048）。
var testLayout = layout.Layout{SegmentSizeBytes: layout.DefaultSegmentSizeBytes, SegmentCount: 2048}

// alignedPayload 返回 n 个字节的测试负载，n 须为 layout.BlockSize 整数倍。
func alignedPayload(n int) []byte {
	if n%int(layout.BlockSize) != 0 {
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

	s, err := NewStorage(context.Background(), rocksdbDir, devPath, testLayout)
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

	payload := alignedPayload(2 * int(layout.BlockSize)) // 8192 B
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := s.Put(context.Background(), "obj", int64(len(payload)), payload); err != nil {
		t.Fatal(err)
	}

	// 对齐子区间读
	got, err := s.ReadAt(context.Background(), "obj", 0, int64(layout.BlockSize))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload[:layout.BlockSize]) {
		t.Fatal("aligned sub-range mismatch")
	}
	bufpool.Put(got)

	// 越过对象结尾：截断到剩余字节并附 io.EOF
	got2, err := s.ReadAt(context.Background(), "obj", int64(layout.BlockSize), int64(4*layout.BlockSize))
	if !bytes.Equal(got2, payload[layout.BlockSize:]) {
		t.Fatalf("clamped tail mismatch: got %dB", len(got2))
	}
	if err != io.EOF {
		t.Fatalf("clamped tail err=%v want io.EOF", err)
	}
	bufpool.Put(got2)

	// 恰好的末尾子区间读
	got3, err := s.ReadAt(context.Background(), "obj", int64(layout.BlockSize), int64(layout.BlockSize))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got3, payload[layout.BlockSize:]) {
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

	payload := alignedPayload(int(layout.BlockSize))
	if err := s.Put(context.Background(), "d", int64(len(payload)), payload); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadAt(context.Background(), "d", 0, int64(layout.BlockSize)); err != ErrNotFound {
		t.Fatalf("after delete ReadAt err=%v want ErrNotFound", err)
	}
	if err := s.Delete(context.Background(), "d"); err != ErrNotFound {
		t.Fatalf("double delete err=%v want ErrNotFound", err)
	}
}

func TestStorageNotFound(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	if _, err := s.ReadAt(context.Background(), "nope", 0, int64(layout.BlockSize)); err != ErrNotFound {
		t.Fatalf("ReadAt missing err=%v want ErrNotFound", err)
	}
}

func TestStorageRestartPreservesCursor(t *testing.T) {
	dir := t.TempDir()
	rocksdbDir := filepath.Join(dir, "meta")
	devPath := filepath.Join(dir, "nvme.img")
	f, _ := os.Create(devPath)
	_ = f.Close()

	s1, err := NewStorage(context.Background(), rocksdbDir, devPath, testLayout)
	if err != nil {
		t.Fatal(err)
	}
	payload := alignedPayload(int(layout.BlockSize))
	if err := s1.Put(context.Background(), "k", int64(len(payload)), payload); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStorage(context.Background(), rocksdbDir, devPath, testLayout)
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
	payload2 := alignedPayload(int(layout.BlockSize))
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
	payload := alignedPayload(int(layout.BlockSize))
	keys := []string{"k1", "k2", "k3"}
	for _, k := range keys {
		if err := s.Put(context.Background(), k, int64(len(payload)), payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStorage(context.Background(), rocksdbDir, devPath, testLayout)
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

// TestStorageDeleteReuseLifecycle：Storage 层完整生命周期 写→删→GC→复用。
// 对象删光后段被后台 GC 回收为 Free，随后新对象复用该段，数据往返正确。
func TestStorageDeleteReuseLifecycle(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	ctx := context.Background()

	payload := alignedPayload(int(layout.BlockSize))
	if err := s.Put(ctx, "a", int64(len(payload)), payload); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "b", int64(len(payload)), payload); err != nil {
		t.Fatal(err)
	}
	if st := s.SegmentStats(); st[metastore.SegmentStateActive] != 1 {
		t.Fatalf("after write stats=%v, want Active=1", st)
	}

	// 删一个：段仍存活（计数 1），同段另一对象仍可读。
	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadAt(ctx, "b", 0, int64(len(payload))); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("read b after delete a: err=%v", err)
	} else {
		bufpool.Put(got)
	}

	// 删光 → Reclaiming，后台 GC（1s 周期）回收为 Free。
	if err := s.Delete(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := s.SegmentStats()
		if st[metastore.SegmentStateFree] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("segment not freed after 3s: stats=%v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 复用：写游标继续落该段，新对象数据往返正确。
	payload2 := alignedPayload(2 * int(layout.BlockSize))
	copy(payload2, "reused segment payload")
	if err := s.Put(ctx, "c", int64(len(payload2)), payload2); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadAt(ctx, "c", 0, int64(len(payload2))); err != nil || !bytes.Equal(got, payload2) {
		t.Fatalf("read c after reuse: err=%v", err)
	} else {
		bufpool.Put(got)
	}
}

// TestStorageConcurrentPutDeleteRead：并发 Put/Delete/ReadAt 无数据错乱、无残留映射
// （后台 GC 同时运行，验证 Ref/Unref 与回收的并发安全）。
func TestStorageConcurrentPutDeleteRead(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	ctx := context.Background()

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("obj/%d", i)
			payload := alignedPayload(int(layout.BlockSize) * (1 + i%4))
			if err := s.Put(ctx, key, int64(len(payload)), payload); err != nil {
				t.Errorf("put %s: %v", key, err)
				return
			}
			got, err := s.ReadAt(ctx, key, 0, int64(len(payload)))
			if err != nil {
				t.Errorf("read %s: %v", key, err)
				return
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("read %s data mismatch", key)
			}
			bufpool.Put(got)
			if err := s.Delete(ctx, key); err != nil {
				t.Errorf("delete %s: %v", key, err)
			}
		}(i)
	}
	wg.Wait()

	// 全部删光后，段不应卡在 Reclaiming（最终应被回收为 Free）。
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := s.SegmentStats()
		if st[metastore.SegmentStateReclaiming] == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("segments stuck in Reclaiming: stats=%v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
