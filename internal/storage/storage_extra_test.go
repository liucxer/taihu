package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/liucxer/taihu/internal/aio"
	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/pkg/ierr"
)

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

// TestBatchPut 覆盖 BatchPut 的正常批量写（含 size==0 项）与各校验失败路径。
func TestBatchPut(t *testing.T) {
	l := layout.Layout{SegmentSizeBytes: 64 * 1024, SegmentCount: 64}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	if err := s.BatchPut(ctx, nil); err != nil {
		t.Fatalf("BatchPut empty: %v", err)
	}

	a := alignedPayload(int(layout.BlockSize))
	b := alignedPayload(int(layout.BlockSize))
	copy(b, "second batch object")
	items := []PutItem{
		{Key: "bp/a", Size: int64(len(a)), Data: a},
		{Key: "bp/zero", Size: 0, Data: nil}, // size==0：跳过设备写，仅写映射
		{Key: "bp/b", Size: int64(len(b)), Data: b},
	}
	if err := s.BatchPut(ctx, items); err != nil {
		t.Fatalf("BatchPut: %v", err)
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

	if err := s.BatchPut(ctx, []PutItem{{Key: "x", Size: -1}}); err != ierr.ErrInvalidRange {
		t.Fatalf("BatchPut size<0 err=%v", err)
	}
	if err := s.BatchPut(ctx, []PutItem{{Key: "x", Size: l.SegmentSizeBytes + 1}}); err != ierr.ErrTooLarge {
		t.Fatalf("BatchPut size>seg err=%v", err)
	}
	if err := s.BatchPut(ctx, []PutItem{{Key: "x", Size: int64(layout.BlockSize), Data: make([]byte, 10)}}); err != ierr.ErrShortWrite {
		t.Fatalf("BatchPut short data err=%v", err)
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

// TestReadAtInto 覆盖 ReadAtInto 直读快路径的边界与错误分支。
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

	n, err := s.ReadAtInto(ctx, "rio", 0, blk, dst)
	if err != nil || n != blk {
		t.Fatalf("ReadAtInto head = %d,%v want %d,nil", n, err, blk)
	}
	if !bytes.Equal(dst[:n], payload[:blk]) {
		t.Fatal("ReadAtInto head data mismatch")
	}

	n, err = s.ReadAtInto(ctx, "rio", blk, blk, dst)
	if err != nil || n != blk {
		t.Fatalf("ReadAtInto tail = %d,%v want %d,nil", n, err, blk)
	}
	if !bytes.Equal(dst[:n], payload[blk:]) {
		t.Fatal("ReadAtInto tail data mismatch")
	}

	// 越过对象结尾：截断到剩余字节 + io.EOF。
	if n, err := s.ReadAtInto(ctx, "rio", blk, 4*blk, dst); n != blk || err != io.EOF {
		t.Fatalf("ReadAtInto clamp = %d,%v want %d,io.EOF", n, err, blk)
	}
	// off == Size：剩余 0。
	if n, err := s.ReadAtInto(ctx, "rio", int64(len(payload)), blk, dst); n != 0 || err != io.EOF {
		t.Fatalf("ReadAtInto off==Size = %d,%v want 0,io.EOF", n, err)
	}
	// 非 4K 对齐 off。
	if _, err := s.ReadAtInto(ctx, "rio", 1, 1, dst); err != ierr.ErrInvalidRange {
		t.Fatalf("ReadAtInto unaligned off err=%v", err)
	}
	// off > Size。
	if _, err := s.ReadAtInto(ctx, "rio", int64(len(payload))+1, 1, dst); err != ierr.ErrInvalidRange {
		t.Fatalf("ReadAtInto off>Size err=%v", err)
	}
	// dst 容量不足物理对齐区间（dlen > len(dst)）。
	if _, err := s.ReadAtInto(ctx, "rio", 0, int64(len(payload)), make([]byte, blk)); err == nil {
		t.Fatal("ReadAtInto small dst: want error")
	}
	// key 不存在。
	if _, err := s.ReadAtInto(ctx, "missing", 0, blk, dst); err != ierr.ErrNotFound {
		t.Fatalf("ReadAtInto missing err=%v", err)
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
