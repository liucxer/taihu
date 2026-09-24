package storage

// 本文件收纳 storage 层的全部测试（原 storage_test.go / storage_extra_test.go /
// compact_test.go / compact_extra_test.go / admin_extra_test.go 合并而来——本层测试统一单文件，
// 同 metastore 先例）。公共辅助（testLayout / newTestStorage / newTestStorageLayout 等）见下方对应定义。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/aio"
	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/pkg/bufpool"
	"github.com/liucxer/taihu/pkg/ierr"
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
	if err := s.Put(context.Background(), "short", 4096, make([]byte, 100)); err != ierr.ErrShortWrite {
		t.Fatalf("Put short: err=%v want ierr.ErrShortWrite", err)
	}
	if _, err := s.ReadAt(context.Background(), "short", 0, 4096); err != ierr.ErrNotFound {
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
	if _, err := s.ReadAt(context.Background(), "obj", int64(len(payload)+1), 1); err != ierr.ErrInvalidRange {
		t.Fatalf("off>Size err=%v want ierr.ErrInvalidRange", err)
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
	if _, err := s.ReadAt(context.Background(), "d", 0, int64(layout.BlockSize)); err != ierr.ErrNotFound {
		t.Fatalf("after delete ReadAt err=%v want ierr.ErrNotFound", err)
	}
	if err := s.Delete(context.Background(), "d"); err != ierr.ErrNotFound {
		t.Fatalf("double delete err=%v want ierr.ErrNotFound", err)
	}
}

func TestStorageNotFound(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	if _, err := s.ReadAt(context.Background(), "nope", 0, int64(layout.BlockSize)); err != ierr.ErrNotFound {
		t.Fatalf("ReadAt missing err=%v want ierr.ErrNotFound", err)
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

// TestBatchReadRefLeakOnPartialValidation：BatchRead 某块校验失败早退时，先前已 Ref
// 的段必须被整体释放（防空间泄漏）。回归点：Phase 1 早退路径曾遗留 Ref——段 AliveCount
// 归零转 Reclaiming 后，后台 GC 因泄漏的读引用永远无法回收为 Free。
func TestBatchReadRefLeakOnPartialValidation(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStorage(t)
	defer s.Close()

	// 对象落在段 0（首个分配段）。
	if err := s.Put(ctx, "leak/0", layout.BlockSize, alignedPayload(int(layout.BlockSize))); err != nil {
		t.Fatal(err)
	}

	blocks := []BatchReadBlock{
		{Key: "leak/0", Off: 0, Size: layout.BlockSize, Dst: alignedPayload(int(layout.BlockSize))},
		// 第 2 块 Dst 容量不足 → 物理 dlen 4096 > 8，Phase 1 校验失败早退。
		{Key: "leak/0", Off: 0, Size: layout.BlockSize, Dst: make([]byte, 8)},
	}
	if _, err := s.BatchRead(ctx, blocks); err == nil {
		t.Fatal("BatchRead with short dst should fail")
	}

	// 删除对象：段 0 存活计数归零转 Reclaiming；若 Phase 1 遗留 Ref，GC 将无法回收。
	if err := s.Delete(ctx, "leak/0"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.SegmentStats()[metastore.SegmentStateFree] >= 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("segment not reclaimed after BatchRead early return: leaked Ref")
}

// TestOptionsApply 覆盖 options.go 的 Option 构造与应用（含非法 AIO 模式透传）。
func TestOptionsApply(t *testing.T) {
	o := defaultOptions()
	if o.aioMode != aio.ModeAuto || o.aioIOPoll {
		t.Fatalf("defaults = %+v", o)
	}
	WithAIOMode(aio.ModeIOUring)(&o)
	WithAIOIOPoll(true)(&o)
	if o.aioMode != aio.ModeIOUring || !o.aioIOPoll {
		t.Fatalf("after opts = %+v", o)
	}
	bad := defaultOptions()
	WithAIOMode(aio.Mode(99))(&bad)
	WithAIOIOPoll(false)(&bad)
	if bad.aioMode != aio.Mode(99) || bad.aioIOPoll {
		t.Fatalf("bad mode not stored: %+v", bad)
	}
}

// createDev 在 dir 下建一个空文件当"盘"（xfs 上普通文件即可满足 O_DIRECT）。
func createDev(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "nvme.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	_ = f.Close()
	return p
}

// TestNewStorageOptionCombos 覆盖 NewStorage 的变参选项应用与两条初始化错误路径。
func TestNewStorageOptionCombos(t *testing.T) {
	dir := t.TempDir()
	devPath := createDev(t, dir)
	s, err := NewStorage(context.Background(), filepath.Join(dir, "meta"), devPath, testLayout,
		WithAIOMode(aio.ModeAuto), WithAIOIOPoll(false))
	if err != nil {
		t.Fatalf("NewStorage with opts: %v", err)
	}
	defer s.Close()
	if got := s.MaxObjectSize(); got != testLayout.SegmentSizeBytes {
		t.Fatalf("MaxObjectSize=%d want %d", got, testLayout.SegmentSizeBytes)
	}

	// 非法 AIO 模式：aio 层按 auto 兜底，构建不应失败。
	dir2 := t.TempDir()
	s2, err := NewStorage(context.Background(), filepath.Join(dir2, "meta"), createDev(t, dir2),
		testLayout, WithAIOMode(aio.Mode(99)))
	if err != nil {
		t.Fatalf("invalid aio mode should fall back: %v", err)
	}
	defer s2.Close()

	// pebble 打开失败：目标路径已是普通文件。
	badDir := filepath.Join(t.TempDir(), "meta")
	if err := os.WriteFile(badDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStorage(context.Background(), badDir, devPath, testLayout); err == nil {
		t.Fatal("NewStorage with file as pebble dir: want error")
	}
	// 设备打开失败：裸设备路径不存在。
	if _, err := NewStorage(context.Background(), filepath.Join(t.TempDir(), "meta"),
		filepath.Join(t.TempDir(), "missing.img"), testLayout); err == nil {
		t.Fatal("NewStorage with missing device: want error")
	}
}

// TestPutBoundaries 覆盖 Put/PutBegin 的非法 size 校验与零长对象路径。
func TestPutBoundaries(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	ctx := context.Background()

	if _, _, err := s.PutBegin(ctx, "neg", -1); err != ierr.ErrInvalidRange {
		t.Fatalf("PutBegin(-1) err=%v want ErrInvalidRange", err)
	}
	if _, _, err := s.PutBegin(ctx, "big", testLayout.SegmentSizeBytes+1); err != ierr.ErrTooLarge {
		t.Fatalf("PutBegin(too large) err=%v want ErrTooLarge", err)
	}
	if err := s.Put(ctx, "big", testLayout.SegmentSizeBytes+1, nil); err != ierr.ErrTooLarge {
		t.Fatalf("Put(too large) err=%v want ErrTooLarge", err)
	}

	// 零长对象：无设备写（Put 的 size>0 分支走 else），仅建立 size=0 映射。
	if err := s.Put(ctx, "empty", 0, nil); err != nil {
		t.Fatalf("Put empty: %v", err)
	}
	if n, err := s.Stat(ctx, "empty"); err != nil || n != 0 {
		t.Fatalf("Stat empty = %d,%v want 0,nil", n, err)
	}
	if _, err := s.ReadAt(ctx, "empty", 0, int64(layout.BlockSize)); err != io.EOF {
		t.Fatalf("ReadAt empty err=%v want io.EOF", err)
	}
}

// TestCapacityAndIOStats 覆盖 Stat / IOStats / GetDiskCapacity / Ping。
func TestCapacityAndIOStats(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	ctx := context.Background()

	if _, err := s.Stat(ctx, "missing"); err != ierr.ErrNotFound {
		t.Fatalf("Stat missing err=%v want ErrNotFound", err)
	}
	payload := alignedPayload(int(layout.BlockSize))
	if err := s.Put(ctx, "cap", int64(len(payload)), payload); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Stat(ctx, "cap"); err != nil || n != int64(len(payload)) {
		t.Fatalf("Stat = %d,%v want %d,nil", n, err, len(payload))
	}

	// 4096B 单次写不属于 4MiB 整块，应计入 other 分档。
	io4M, ioOther, bytes4M, bytesOther := s.IOStats()
	if io4M != 0 || bytes4M != 0 || ioOther <= 0 || bytesOther <= 0 {
		t.Fatalf("IOStats = %d,%d,%d,%d", io4M, ioOther, bytes4M, bytesOther)
	}

	capacity, available, used, err := s.GetDiskCapacity()
	if err != nil {
		t.Fatalf("GetDiskCapacity: %v", err)
	}
	wantCap := testLayout.SegmentSizeBytes * testLayout.SegmentCount
	if capacity != wantCap {
		t.Fatalf("capacity=%d want %d", capacity, wantCap)
	}
	if used <= 0 {
		t.Fatalf("used=%d want >0", used)
	}
	if available != capacity-used {
		t.Fatalf("available=%d want %d", available, capacity-used)
	}

	ts, err := s.Ping(ctx)
	if err != nil || ts <= 0 {
		t.Fatalf("Ping = %d,%v want >0,nil", ts, err)
	}
}

// TestBatchPut 覆盖 batchPut 的正常批量写（含 size==0 项）与各校验失败路径。
func TestBatchPut(t *testing.T) {
	l := layout.Layout{SegmentSizeBytes: 64 * 1024, SegmentCount: 64}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	if err := s.batchPut(ctx, nil); err != nil {
		t.Fatalf("batchPut empty: %v", err)
	}

	a := alignedPayload(int(layout.BlockSize))
	b := alignedPayload(int(layout.BlockSize))
	copy(b, "second batch object")
	items := []putItem{
		{Key: "bp/a", Size: int64(len(a)), Data: a},
		{Key: "bp/zero", Size: 0, Data: nil}, // size==0：跳过设备写，仅写映射
		{Key: "bp/b", Size: int64(len(b)), Data: b},
	}
	if err := s.batchPut(ctx, items); err != nil {
		t.Fatalf("batchPut: %v", err)
	}
	for i, it := range []struct {
		key string
		val []byte
	}{{"bp/a", a}, {"bp/b", b}} {
		got, err := s.ReadAt(ctx, it.key, 0, int64(len(it.val)))
		if err != nil {
			t.Fatalf("BatchPut read %s: %v", it.key, err)
		}
		if !bytes.Equal(got, it.val) {
			t.Fatalf("BatchPut read %s mismatch (i=%d)", it.key, i)
		}
		bufpool.Put(got)
	}
	if n, err := s.Stat(ctx, "bp/zero"); err != nil || n != 0 {
		t.Fatalf("Stat bp/zero = %d,%v", n, err)
	}

	if err := s.batchPut(ctx, []putItem{{Key: "x", Size: -1}}); err != ierr.ErrInvalidRange {
		t.Fatalf("batchPut size<0 err=%v", err)
	}
	if err := s.batchPut(ctx, []putItem{{Key: "x", Size: l.SegmentSizeBytes + 1}}); err != ierr.ErrTooLarge {
		t.Fatalf("batchPut size>seg err=%v", err)
	}
	if err := s.batchPut(ctx, []putItem{{Key: "x", Size: int64(layout.BlockSize), Data: make([]byte, 10)}}); err != ierr.ErrShortWrite {
		t.Fatalf("batchPut short data err=%v", err)
	}
}

// TestBatchAppendCommitDelete 覆盖 BatchAppend / BatchPutCommit / BatchDelete。
func TestBatchAppendCommitDelete(t *testing.T) {
	l := layout.Layout{SegmentSizeBytes: 64 * 1024, SegmentCount: 64}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	payload := alignedPayload(2 * int(layout.BlockSize))
	for i := range payload {
		payload[i] = byte(i)
	}
	// 不经分配/元数据，直接把两段数据写进段 0（第三项 size==0：无写 IO）。
	jobs := []device.WriteJob{
		{SegmentID: 0, Off: 0, Data: payload[:layout.BlockSize], Size: layout.BlockSize},
		{SegmentID: 0, Off: layout.BlockSize, Data: payload[layout.BlockSize:], Size: layout.BlockSize},
		{SegmentID: 1, Off: 0, Data: nil, Size: 0},
	}
	if err := s.BatchAppend(ctx, jobs); err != nil {
		t.Fatalf("BatchAppend: %v", err)
	}

	// 批量提交映射后经对象 API 读回，数据须与直写一致。
	comm := []metastore.PutMappingItem{
		{Key: "ba/0", Meta: metastore.ObjectMeta{SegmentID: 0, Offset: 0, Size: layout.BlockSize}},
		{Key: "ba/1", Meta: metastore.ObjectMeta{SegmentID: 0, Offset: layout.BlockSize, Size: layout.BlockSize}},
	}
	if err := s.BatchPutCommit(ctx, comm); err != nil {
		t.Fatalf("BatchPutCommit: %v", err)
	}
	got, err := s.ReadAt(ctx, "ba/1", 0, layout.BlockSize)
	if err != nil {
		t.Fatalf("read ba/1: %v", err)
	}
	if !bytes.Equal(got, payload[layout.BlockSize:]) {
		t.Fatal("ba/1 data mismatch")
	}
	bufpool.Put(got)

	// BatchDelete：存在→nil，缺失→ErrNotFound（per-key 错误并对齐入参）。
	errs, err := s.BatchDelete(ctx, []string{"ba/0", "nope"})
	if err != nil {
		t.Fatalf("BatchDelete: %v", err)
	}
	if len(errs) != 2 || errs[0] != nil || errs[1] != ierr.ErrNotFound {
		t.Fatalf("BatchDelete errs=%v", errs)
	}
	if _, err := s.Stat(ctx, "ba/0"); err != ierr.ErrNotFound {
		t.Fatalf("ba/0 after batch delete err=%v", err)
	}
	if _, err := s.Stat(ctx, "ba/1"); err != nil {
		t.Fatalf("ba/1 should survive: %v", err)
	}
}

// TestReadAtInto 覆盖 readAtInto 直读快路径的边界与错误分支。
func TestReadAtInto(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	ctx := context.Background()

	blk := int64(layout.BlockSize)
	payload := alignedPayload(2 * int(layout.BlockSize))
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := s.Put(ctx, "rio", int64(len(payload)), payload); err != nil {
		t.Fatal(err)
	}

	dst := bufpool.Get(int(blk))
	defer bufpool.Put(dst)

	n, err := s.readAtInto(ctx, "rio", 0, blk, dst)
	if err != nil || n != blk {
		t.Fatalf("readAtInto head = %d,%v want %d,nil", n, err, blk)
	}
	if !bytes.Equal(dst[:n], payload[:blk]) {
		t.Fatal("readAtInto head data mismatch")
	}

	n, err = s.readAtInto(ctx, "rio", blk, blk, dst)
	if err != nil || n != blk {
		t.Fatalf("readAtInto tail = %d,%v want %d,nil", n, err, blk)
	}
	if !bytes.Equal(dst[:n], payload[blk:]) {
		t.Fatal("readAtInto tail data mismatch")
	}

	// 越过对象结尾：截断到剩余字节 + io.EOF。
	if n, err := s.readAtInto(ctx, "rio", blk, 4*blk, dst); n != blk || err != io.EOF {
		t.Fatalf("readAtInto clamp = %d,%v want %d,io.EOF", n, err, blk)
	}
	// off == Size：剩余 0。
	if n, err := s.readAtInto(ctx, "rio", int64(len(payload)), blk, dst); n != 0 || err != io.EOF {
		t.Fatalf("readAtInto off==Size = %d,%v want 0,io.EOF", n, err)
	}
	// 非 4K 对齐 off。
	if _, err := s.readAtInto(ctx, "rio", 1, 1, dst); err != ierr.ErrInvalidRange {
		t.Fatalf("readAtInto unaligned off err=%v", err)
	}
	// off > Size。
	if _, err := s.readAtInto(ctx, "rio", int64(len(payload))+1, 1, dst); err != ierr.ErrInvalidRange {
		t.Fatalf("readAtInto off>Size err=%v", err)
	}
	// dst 容量不足物理对齐区间（dlen > len(dst)）。
	if _, err := s.readAtInto(ctx, "rio", 0, int64(len(payload)), make([]byte, blk)); err == nil {
		t.Fatal("readAtInto small dst: want error")
	}
	// key 不存在。
	if _, err := s.readAtInto(ctx, "missing", 0, blk, dst); err != ierr.ErrNotFound {
		t.Fatalf("readAtInto missing err=%v", err)
	}
}

// TestBatchRead 覆盖 BatchRead 的批量直读、EOF 截断与错误分支。
func TestBatchRead(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	ctx := context.Background()

	blk := int64(layout.BlockSize)
	pa := alignedPayload(2 * int(layout.BlockSize))
	for i := range pa {
		pa[i] = byte(i)
	}
	pb := alignedPayload(2 * int(layout.BlockSize))
	for i := range pb {
		pb[i] = byte(255 - i%251)
	}
	if err := s.Put(ctx, "br/a", int64(len(pa)), pa); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "br/b", int64(len(pb)), pb); err != nil {
		t.Fatal(err)
	}

	if res, err := s.BatchRead(ctx, nil); err != nil || len(res) != 0 {
		t.Fatalf("BatchRead empty = %v,%v", res, err)
	}

	d0 := bufpool.Get(2 * int(blk))
	d1 := bufpool.Get(int(blk))
	d2 := bufpool.Get(int(blk))
	res, err := s.BatchRead(ctx, []BatchReadBlock{
		{Key: "br/a", Off: 0, Size: 2 * blk, Dst: d0},   // 整对象满读
		{Key: "br/b", Off: blk, Size: blk, Dst: d1},     // 尾块精确读
		{Key: "br/a", Off: blk, Size: 2 * blk, Dst: d2}, // 越过结尾 → 截断 + EOF
	})
	if err != nil {
		t.Fatalf("BatchRead: %v", err)
	}
	if res[0].N != 2*blk || res[0].Err != nil {
		t.Fatalf("block0 = %+v", res[0])
	}
	if !bytes.Equal(d0[:res[0].N], pa) {
		t.Fatal("block0 data mismatch")
	}
	if res[1].N != blk || res[1].Err != nil {
		t.Fatalf("block1 = %+v", res[1])
	}
	if !bytes.Equal(d1[:res[1].N], pb[blk:]) {
		t.Fatal("block1 data mismatch")
	}
	if res[2].N != blk || res[2].Err != io.EOF {
		t.Fatalf("block2 = %+v want %d,io.EOF", res[2], blk)
	}
	bufpool.Put(d0)
	bufpool.Put(d1)
	bufpool.Put(d2)

	// 整批块均 off==Size：want==0，无任何设备 job。
	de := bufpool.Get(int(blk))
	res, err = s.BatchRead(ctx, []BatchReadBlock{{Key: "br/a", Off: 2 * blk, Size: blk, Dst: de}})
	if err != nil || res[0].N != 0 || res[0].Err != io.EOF {
		t.Fatalf("BatchRead all-eof = %v,%v", res, err)
	}
	bufpool.Put(de)

	// 错误分支：非对齐 off / off>Size / dst 过小 / 映射缺失。
	for _, tc := range []struct {
		name  string
		block BatchReadBlock
		want  error
	}{
		{"unaligned", BatchReadBlock{Key: "br/a", Off: 1, Size: blk}, ierr.ErrInvalidRange},
		{"past-end", BatchReadBlock{Key: "br/a", Off: 2*blk + 1, Size: blk}, ierr.ErrInvalidRange},
		{"missing", BatchReadBlock{Key: "nope", Off: 0, Size: blk}, ierr.ErrNotFound},
	} {
		b := tc.block
		b.Dst = bufpool.Get(int(blk))
		_, err := s.BatchRead(ctx, []BatchReadBlock{b})
		bufpool.Put(b.Dst)
		if err != tc.want {
			t.Fatalf("BatchRead %s err=%v want %v", tc.name, err, tc.want)
		}
	}
	if _, err := s.BatchRead(ctx, []BatchReadBlock{{Key: "br/a", Off: 0, Size: 2 * blk, Dst: make([]byte, blk)}}); err == nil {
		t.Fatal("BatchRead small dst: want error")
	}
}

// TestDeleteConcurrentIdempotent：同一 key 并发删除不返回意外错误，最终映射消失（删除幂等）。
func TestDeleteConcurrentIdempotent(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	ctx := context.Background()

	payload := alignedPayload(int(layout.BlockSize))
	if err := s.Put(ctx, "cd", int64(len(payload)), payload); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var ok, notFound atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch err := s.Delete(ctx, "cd"); err {
			case nil:
				ok.Add(1)
			case ierr.ErrNotFound:
				notFound.Add(1)
			default:
				t.Errorf("concurrent delete err=%v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() < 1 || ok.Load()+notFound.Load() != n {
		t.Fatalf("delete ok=%d notFound=%d (n=%d)", ok.Load(), notFound.Load(), n)
	}
	if _, err := s.Stat(ctx, "cd"); err != ierr.ErrNotFound {
		t.Fatalf("key still present after concurrent delete: %v", err)
	}
}

// TestReadAtMetaSnapshot 覆盖 Meta/ReadAtMeta/ReadAtIntoMeta 的映射快照契约：快照取得
// 后再覆盖写，按快照读仍返回旧版本（单次 GET 跨多个 chunk 不混版本的前提），按 key 读
// 返回新版本。快照段引用（RefSegment/UnrefSegment）在读取期间持有。
func TestReadAtMetaSnapshot(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	ctx := context.Background()

	blk := int64(layout.BlockSize)
	old := alignedPayload(2 * int(blk)) // 旧版本：2 块
	for i := range old {
		old[i] = byte(i)
	}
	if err := s.Put(ctx, "snap", int64(len(old)), old); err != nil {
		t.Fatal(err)
	}

	meta, err := s.Meta(ctx, "snap")
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.Size != int64(len(old)) {
		t.Fatalf("Meta size=%d want %d", meta.Size, len(old))
	}
	s.RefSegment(meta.SegmentID)
	defer s.UnrefSegment(meta.SegmentID)

	// 覆盖写：新版本更短，映射立刻指向新段（旧段计数归零，GC 仅在引用归零时可回收）。
	cur := alignedPayload(int(blk)) // 新版本：1 块
	for i := range cur {
		cur[i] = 0xFF
	}
	if err := s.Put(ctx, "snap", int64(len(cur)), cur); err != nil {
		t.Fatal(err)
	}

	// 快照读：仍是旧版本（含第二块）。
	got, err := s.ReadAtMeta(ctx, meta, 0, meta.Size)
	if err != nil {
		t.Fatalf("ReadAtMeta(snapshot): %v", err)
	}
	defer bufpool.Put(got)
	if !bytes.Equal(got, old) {
		t.Fatal("ReadAtMeta(snapshot) 未按快照返回旧版本")
	}

	// 按 key 读：新版本（证明上面的差异确由快照产生，而非读路径失效）。
	keyed, err := s.ReadAt(ctx, "snap", 0, int64(len(cur)))
	if err != nil {
		t.Fatalf("ReadAt(key): %v", err)
	}
	defer bufpool.Put(keyed)
	if !bytes.Equal(keyed, cur) {
		t.Fatal("ReadAt(key) 未返回最新版本")
	}

	// ReadAtIntoMeta 同快照语义。
	dst := bufpool.Get(int(blk))
	defer bufpool.Put(dst)
	n, err := s.ReadAtIntoMeta(ctx, meta, 0, blk, dst)
	if err != nil || n != blk || !bytes.Equal(dst[:n], old[:blk]) {
		t.Fatalf("ReadAtIntoMeta = %d,%v want %d,nil（应读快照版本）", n, err, blk)
	}

	if _, err := s.Meta(ctx, "missing"); err != ierr.ErrNotFound {
		t.Fatalf("Meta(missing) err=%v", err)
	}
}

// newTestStorageLayout 构造带指定布局的文件设备 Storage（testLayout 之外的小段布局便于触发搬移）。
func newTestStorageLayout(t *testing.T, l layout.Layout) *Storage {
	t.Helper()
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	f, err := os.Create(devPath)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	_ = f.Close()
	s, err := NewStorage(context.Background(), filepath.Join(dir, "meta"), devPath, l)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	return s
}

// TestCompactFreesSparseSegment：删除 80% 的高空洞段被 compaction 搬移存活对象（可动用预留缓冲段），
// 搬空后旧段走现有后台 GC 回收为 Free 复用；存活对象数据往返正确。
func TestCompactFreesSparseSegment(t *testing.T) {
	const segSize = 64 * 1024 // 每段 16 个 4KB 对象
	l := layout.Layout{SegmentSizeBytes: segSize, SegmentCount: 64}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	payload := alignedPayload(int(layout.BlockSize))
	const perSeg = 16
	for i := 0; i < 2*perSeg; i++ {
		if err := s.Put(ctx, fmt.Sprintf("k%02d", i), int64(len(payload)), payload); err != nil {
			t.Fatal(err)
		}
	}
	// 段 0 写满 16 个（Full），段 1 Active 写 4 个。删段 0 的 13 个 → 剩余 3 个，空洞率 ≈ 81%。
	for i := 0; i < 13; i++ {
		if err := s.Delete(ctx, fmt.Sprintf("k%02d", i)); err != nil {
			t.Fatal(err)
		}
	}

	c := NewCompactor(s, CompactorConfig{Interval: time.Hour, HoleThreshold: 0.8, ForceWatermark: 1.0, MaxMovePerRound: 100})
	moved, err := c.compactOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if moved == 0 {
		t.Fatal("compaction moved 0 objects")
	}

	// 搬空后段 0 → Reclaiming，后台 GC（1s 周期）回收为 Free。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if st := s.SegmentStats(); st[metastore.SegmentStateFree] >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sparse segment not reclaimed after compaction: %v", s.SegmentStats())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 存活对象（k13..k31）数据往返正确（读走新映射）。
	for i := 13; i < 2*perSeg; i++ {
		key := fmt.Sprintf("k%02d", i)
		got, err := s.ReadAt(ctx, key, 0, int64(len(payload)))
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("%s data mismatch", key)
		}
		bufpool.Put(got)
	}
}

// TestCompactConcurrentDelete：搬移期间并发删除候选对象（-race）。
// 数据一致性由 MoveMapping 的 CAS 保证：对象已删 → 跳过；读后删除 → 冲突跳过，不丢数据、不复活映射。
func TestCompactConcurrentDelete(t *testing.T) {
	const segSize = 64 * 1024
	l := layout.Layout{SegmentSizeBytes: segSize, SegmentCount: 64}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	payload := alignedPayload(int(layout.BlockSize))
	const perSeg = 16
	for i := 0; i < 2*perSeg; i++ {
		if err := s.Put(ctx, fmt.Sprintf("k%02d", i), int64(len(payload)), payload); err != nil {
			t.Fatal(err)
		}
	}
	// 段 0 删 13 个 → 剩 k13..k15 三个（空洞率 ≈ 81%），是搬移候选。
	for i := 0; i < 13; i++ {
		if err := s.Delete(ctx, fmt.Sprintf("k%02d", i)); err != nil {
			t.Fatal(err)
		}
	}

	c := NewCompactor(s, CompactorConfig{Interval: time.Hour, HoleThreshold: 0.8, ForceWatermark: 1.0, MaxMovePerRound: 100})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond) // 让搬移先读到候选对象
		_ = s.Delete(ctx, "k13")          // 候选段（0）内存活对象之一被并发删除
	}()
	moved, err := c.compactOnce(ctx)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if moved == 0 {
		t.Fatal("compaction moved 0 objects")
	}

	// k13 应已删除（不复活）；其余存活对象可读。
	if _, err := s.ObjectMeta(ctx, "k13"); err != ierr.ErrNotFound {
		t.Fatalf("k13 mapping = %v, want ierr.ErrNotFound", err)
	}
	for i := 14; i < 2*perSeg; i++ {
		key := fmt.Sprintf("k%02d", i)
		got, err := s.ReadAt(ctx, key, 0, int64(len(payload)))
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		bufpool.Put(got)
	}
}

// putAligned 写入 n 个 4KB 对象（key 形如 k%02d），返回负载。
func putAligned(t *testing.T, s *Storage, ctx context.Context, n int) []byte {
	t.Helper()
	payload := alignedPayload(int(layout.BlockSize))
	for i := 0; i < n; i++ {
		if err := s.Put(ctx, fmt.Sprintf("k%02d", i), int64(len(payload)), payload); err != nil {
			t.Fatalf("put k%02d: %v", i, err)
		}
	}
	return payload
}

// TestCompactorConfigAndLifecycle 覆盖 DefaultCompactorConfig 与 Start/run/Stop 后台循环。
func TestCompactorConfigAndLifecycle(t *testing.T) {
	cfg := DefaultCompactorConfig()
	if cfg.Interval != time.Minute || cfg.HoleThreshold != 0.8 || cfg.ForceWatermark != 0.8 || cfg.MaxMovePerRound != 512 {
		t.Fatalf("DefaultCompactorConfig = %+v", cfg)
	}

	const segSize = 64 * 1024
	l := layout.Layout{SegmentSizeBytes: segSize, SegmentCount: 16}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	putAligned(t, s, ctx, 17) // 写满段 0（第 17 个触发滚动 → 段 0 转 Full），段 1 Active
	for i := 0; i < 13; i++ {
		if err := s.Delete(ctx, fmt.Sprintf("k%02d", i)); err != nil {
			t.Fatal(err)
		}
	}

	c := NewCompactor(s, CompactorConfig{Interval: 5 * time.Millisecond, HoleThreshold: 0.8, ForceWatermark: 1.0, MaxMovePerRound: 100})
	c.Start()
	// 后台循环每 5ms 扫一轮：候选段存活对象被搬走后旧段经后台 GC 回收为 Free。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if st := s.SegmentStats(); st[metastore.SegmentStateFree] >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background compaction did not free sparse segment: %v", s.SegmentStats())
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.Stop()

	// 存活对象（k13..k15）搬移后数据往返正确。
	for i := 13; i < 16; i++ {
		key := fmt.Sprintf("k%02d", i)
		got, err := s.ReadAt(ctx, key, 0, int64(layout.BlockSize))
		if err != nil {
			t.Fatalf("read %s after background compaction: %v", key, err)
		}
		bufpool.Put(got)
	}
}

// TestCompactForceWatermark 覆盖全局水位强制压缩（空洞率低于单段阈值仍搬移）。
func TestCompactForceWatermark(t *testing.T) {
	const segSize = 64 * 1024
	l := layout.Layout{SegmentSizeBytes: segSize, SegmentCount: 16}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	putAligned(t, s, ctx, 17) // 段 0 写满转 Full，段 1 Active
	// 只删 1 个 → 空洞率 1/16 ≈ 6.3%，远低于阈值 99%；靠全局水位（2/16 ≥ 5%）强制入选。
	if err := s.Delete(ctx, "k00"); err != nil {
		t.Fatal(err)
	}

	c := NewCompactor(s, CompactorConfig{Interval: time.Hour, HoleThreshold: 0.99, ForceWatermark: 0.05, MaxMovePerRound: 100})
	moved, err := c.compactOnce(ctx)
	if err != nil {
		t.Fatalf("compactOnce: %v", err)
	}
	if moved != 15 {
		t.Fatalf("force watermark moved=%d want 15", moved)
	}
	// 搬移后存活对象读原数据。
	payload := alignedPayload(int(layout.BlockSize))
	got, err := s.ReadAt(ctx, "k01", 0, int64(len(payload)))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("read k01 after force compaction: err=%v", err)
	}
	bufpool.Put(got)
}

// TestCompactMoveLimit 覆盖每轮搬移对象数上限（跨候选段合并记账）。
func TestCompactMoveLimit(t *testing.T) {
	const segSize = 64 * 1024
	l := layout.Layout{SegmentSizeBytes: segSize, SegmentCount: 16}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	// 33 个对象：段 0 满、段 1 满（第 33 个触发滚动时标记 Full）、段 2 Active。
	putAligned(t, s, ctx, 33)
	// 两个 Full 段各删 13 个 → 各剩 3 个，空洞率均 ≈81%，都是候选（按段号升序）。
	for i := 0; i < 13; i++ {
		if err := s.Delete(ctx, fmt.Sprintf("k%02d", i)); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, fmt.Sprintf("k%02d", i+16)); err != nil {
			t.Fatal(err)
		}
	}

	c := NewCompactor(s, CompactorConfig{Interval: time.Hour, HoleThreshold: 0.8, ForceWatermark: 1.0, MaxMovePerRound: 1})
	moved, err := c.compactOnce(ctx)
	if err != nil {
		t.Fatalf("compactOnce: %v", err)
	}
	if moved != 1 {
		t.Fatalf("MaxMovePerRound=1 moved=%d want 1", moved)
	}
	// 被搬移的首个对象（段 0 的 k13）数据正确。
	payload := alignedPayload(int(layout.BlockSize))
	got, err := s.ReadAt(ctx, "k13", 0, int64(len(payload)))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("read k13 after limited compaction: err=%v", err)
	}
	bufpool.Put(got)
}

// TestSegmentsSummary 覆盖 admin Segments 的段明细/汇总/游标与四种段状态分支。
func TestSegmentsSummary(t *testing.T) {
	const segSize = 64 * 1024
	l := layout.Layout{SegmentSizeBytes: segSize, SegmentCount: 16}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	payload := alignedPayload(int(layout.BlockSize))
	// 每段容纳 16 个 4KB 对象。
	const perSeg = 16
	for i := 0; i < perSeg+1; i++ { // 写满段 0（Full），第 17 个落在 Active 的段 1
		if err := s.Put(ctx, fmt.Sprintf("k%02d", i), int64(len(payload)), payload); err != nil {
			t.Fatal(err)
		}
	}

	sum, entries, err := s.Segments(ctx)
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}
	if sum.SegSize != segSize {
		t.Fatalf("SegSize=%d want %d", sum.SegSize, segSize)
	}
	if sum.ObjectCount != perSeg+1 {
		t.Fatalf("ObjectCount=%d want %d", sum.ObjectCount, perSeg+1)
	}
	if sum.Total != 2 || sum.Full != 1 || sum.Active != 1 || sum.Free != 0 || sum.Reclaiming != 0 {
		t.Fatalf("summary=%+v want Total=2 Full=1 Active=1", sum)
	}
	if sum.CursorSeg != 1 || sum.CursorOff != int64(len(payload)) {
		t.Fatalf("Cursor=(%d,%d) want (1,%d)", sum.CursorSeg, sum.CursorOff, len(payload))
	}
	if len(entries) != 2 || entries[0].SegmentID != 0 || entries[1].SegmentID != 1 {
		t.Fatalf("entries=%+v want ascending [0 1]", entries)
	}
	if entries[0].State != metastore.SegmentStateFull || entries[0].AliveCount != perSeg {
		t.Fatalf("entry0=%+v want Full/%d", entries[0], perSeg)
	}

	// 持段 0 读引用 → 删光后稳定停留在 Reclaiming（后台 GC 不会回收有引用的段）。
	s.db.RefSegment(0)
	for i := 0; i < perSeg; i++ {
		if err := s.Delete(ctx, fmt.Sprintf("k%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	sum, _, err = s.Segments(ctx)
	if err != nil {
		t.Fatalf("Segments after delete: %v", err)
	}
	if sum.Reclaiming != 1 || sum.Active != 1 || sum.Free != 0 || sum.Full != 0 {
		t.Fatalf("after delete summary=%+v want Reclaiming=1 Active=1", sum)
	}
	if sum.ObjectCount != 1 {
		t.Fatalf("ObjectCount after delete=%d want 1", sum.ObjectCount)
	}
	s.db.UnrefSegment(0)

	// 释放引用后后台 GC（1s 周期）把 Reclaiming 段回收为 Free。
	deadline := time.Now().Add(3 * time.Second)
	for {
		sum, _, err = s.Segments(ctx)
		if err != nil {
			t.Fatalf("Segments while waiting GC: %v", err)
		}
		if sum.Free == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("segment not freed after 3s: %+v", sum)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sum.Reclaiming != 0 || sum.Full != 0 {
		t.Fatalf("final summary=%+v want Reclaiming=0 Full=0", sum)
	}
}

// TestAdminListKeysObjectMetaAndPing 覆盖 ListKeys 前缀过滤、ObjectMeta 与 Ping。
func TestAdminListKeysObjectMetaAndPing(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	ctx := context.Background()

	payload := alignedPayload(int(layout.BlockSize))
	for _, k := range []string{"a/1", "a/2", "b/1"} {
		if err := s.Put(ctx, k, int64(len(payload)), payload); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.ListKeys(ctx, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("ListKeys all = %v,%v want 3 keys", all, err)
	}
	prefixed, err := s.ListKeys(ctx, "a/")
	if err != nil {
		t.Fatalf("ListKeys a/: %v", err)
	}
	if len(prefixed) != 2 || prefixed[0] != "a/1" || prefixed[1] != "a/2" {
		t.Fatalf("ListKeys a/ = %v want [a/1 a/2]", prefixed)
	}
	if none, err := s.ListKeys(ctx, "a/1/x"); err != nil || len(none) != 0 {
		t.Fatalf("ListKeys longer-than-key prefix = %v,%v want empty", none, err)
	}

	m, err := s.ObjectMeta(ctx, "a/1")
	if err != nil {
		t.Fatalf("ObjectMeta: %v", err)
	}
	if m.Size != int64(len(payload)) {
		t.Fatalf("ObjectMeta size=%d want %d", m.Size, len(payload))
	}
	if _, err := s.ObjectMeta(ctx, "nope"); err != ierr.ErrNotFound {
		t.Fatalf("ObjectMeta missing err=%v want ErrNotFound", err)
	}

	ts, err := s.Ping(ctx)
	if err != nil || ts <= 0 {
		t.Fatalf("Ping = %d,%v want >0,nil", ts, err)
	}
}
