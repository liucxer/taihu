package metastore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/pkg/ierr"
)

// errInjected 注入的存储错误（用于错误分支断言）。
var errInjected = errors.New("injected store failure")

// TestIterMappingAndLoadCache：前缀遍历、回调错误传播与缓存预热。
func TestIterMappingAndLoadCache(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()
	ps := s.(*pebbleStore)

	for i, k := range []string{"a", "b", "c"} {
		if err := s.PutMapping(ctx, k, ObjectMeta{SegmentID: 0, Offset: int64(i) * 4096, Size: 4}); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	if err := s.IterMapping(ctx, func(key string, m ObjectMeta) error {
		got = append(got, key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("IterMapping keys = %q", got)
	}

	// 回调错误原样返回。
	if err := s.IterMapping(ctx, func(string, ObjectMeta) error { return errInjected }); err != errInjected {
		t.Fatalf("IterMapping fn error = %v", err)
	}

	// 预热缓存：清空后由 LoadCache 回填。
	ps.cache = &metaCache{}
	if err := s.LoadCache(ctx); err != nil {
		t.Fatal(err)
	}
	if m, ok := ps.cache.get("b"); !ok || m.Offset != 4096 {
		t.Fatalf("cache after LoadCache = (%+v,%v)", m, ok)
	}
}

// TestGetMappingErrors：key 不存在与值损坏分支。
func TestGetMappingErrors(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()
	ps := s.(*pebbleStore)

	if _, err := s.GetMapping(ctx, "nope"); err != ierr.ErrNotFound {
		t.Fatalf("GetMapping(missing) = %v, want ErrNotFound", err)
	}
	if err := ps.db.Set(keyMapping("bad"), []byte("xx"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMapping(ctx, "bad"); err == nil || err == ierr.ErrNotFound {
		t.Fatalf("GetMapping(corrupt) = %v, want decode error", err)
	}
}

// TestPutMappingOverCorruptOld：非缓存 key 的旧值回查命中损坏值 → 解码错误。
func TestPutMappingOverCorruptOld(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()
	ps := s.(*pebbleStore)

	// 缓存命中路径下的覆盖写：旧段计数 −1。
	if err := s.PutMapping(ctx, "k", ObjectMeta{SegmentID: 0, Offset: 0, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMapping(ctx, "k", ObjectMeta{SegmentID: 1, Offset: 0, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if m, err := s.GetMapping(ctx, "k"); err != nil || m.SegmentID != 1 {
		t.Fatalf("GetMapping(k) = %+v, %v", m, err)
	}

	// 缓存未命中 → 回查 pebble 得到损坏旧值 → 解码错误。
	if err := ps.db.Set(keyMapping("bad"), []byte("xx"), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMapping(ctx, "bad", ObjectMeta{SegmentID: 0, Size: 4}); err == nil {
		t.Fatal("PutMapping over corrupt mapping should fail")
	}
	// 缓存命中的 key 覆盖写仍成功（缓存为加速层，不再回查 pebble）。
	if err := s.PutMapping(ctx, "k", ObjectMeta{SegmentID: 2, Offset: 0, Size: 4}); err != nil {
		t.Fatalf("PutMapping(cached key) = %v", err)
	}
	if err := s.DeleteMapping(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMapping(ctx, "k"); err != ierr.ErrNotFound {
		t.Fatalf("GetMapping after delete = %v", err)
	}
}

// TestBatchGetMapping：缓存命中、去重排序、缺失与损坏值。
func TestBatchGetMapping(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()
	ps := s.(*pebbleStore)

	for i, k := range []string{"a", "b", "c"} {
		if err := s.PutMapping(ctx, k, ObjectMeta{SegmentID: 0, Offset: int64(i) * 4096, Size: 4}); err != nil {
			t.Fatal(err)
		}
	}
	ps.cache = &metaCache{}

	metas, err := s.BatchGetMapping(ctx, []string{"b", "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 || metas[0].Offset != 4096 || metas[1].Offset != 0 {
		t.Fatalf("BatchGetMapping = %+v", metas)
	}
	// 全部缓存命中（无 miss）：直接返回。
	if metas, err = s.BatchGetMapping(ctx, []string{"a", "b"}); err != nil || len(metas) != 2 {
		t.Fatalf("cached BatchGetMapping = %+v, %v", metas, err)
	}
	// 入参重复 key：去重后按位置回填。
	if metas, err = s.BatchGetMapping(ctx, []string{"a", "a", "c"}); err != nil {
		t.Fatal(err)
	}
	if metas[0] != metas[1] || metas[2].Offset != 8192 {
		t.Fatalf("dup BatchGetMapping = %+v", metas)
	}
	// 任一 key 缺失整体 ErrNotFound。
	if _, err = s.BatchGetMapping(ctx, []string{"a", "missing"}); err != ierr.ErrNotFound {
		t.Fatalf("BatchGetMapping missing = %v, want ErrNotFound", err)
	}
	// 损坏值 → 解码错误。
	if err := ps.db.Set(keyMapping("bad"), []byte("xx"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BatchGetMapping(ctx, []string{"bad"}); err == nil {
		t.Fatal("BatchGetMapping over corrupt value should fail")
	}
}

// TestBatchPutMapping：空批次、批量写、缓存命中覆盖写。
func TestBatchPutMapping(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()

	if err := s.BatchPutMapping(ctx, nil); err != nil {
		t.Fatalf("BatchPutMapping(empty) = %v", err)
	}
	items := []PutMappingItem{
		{Key: "k1", Meta: ObjectMeta{SegmentID: 0, Offset: 0, Size: 4}},
		{Key: "k2", Meta: ObjectMeta{SegmentID: 0, Offset: 4096, Size: 4}},
	}
	if err := s.BatchPutMapping(ctx, items); err != nil {
		t.Fatal(err)
	}
	if e := segState(t, s, 0); e.meta.AliveCount != 2 {
		t.Fatalf("seg0 alive = %d, want 2", e.meta.AliveCount)
	}
	// 缓存命中覆盖写：旧段计数 −1、新段 +1。
	if err := s.BatchPutMapping(ctx, []PutMappingItem{{Key: "k1", Meta: ObjectMeta{SegmentID: 1, Offset: 0, Size: 4}}}); err != nil {
		t.Fatal(err)
	}
	if e := segState(t, s, 0); e.meta.AliveCount != 1 {
		t.Fatalf("seg0 alive = %d, want 1", e.meta.AliveCount)
	}
	if e := segState(t, s, 1); e.meta.AliveCount != 1 {
		t.Fatalf("seg1 alive = %d, want 1", e.meta.AliveCount)
	}
	if m, err := s.GetMapping(ctx, "k1"); err != nil || m.SegmentID != 1 {
		t.Fatalf("GetMapping(k1) = %+v, %v", m, err)
	}
	// 批量写入的 mapping 可被全量遍历看到。
	var n int
	if err := s.IterMapping(ctx, func(string, ObjectMeta) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("IterMapping count = %d, want 2", n)
	}
}

// TestBatchDeleteMapping：per-key 缺失错误与批量删除。
func TestBatchDeleteMapping(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()

	for i, k := range []string{"a", "b", "c"} {
		if err := s.PutMapping(ctx, k, ObjectMeta{SegmentID: 0, Offset: int64(i) * 4096, Size: 4}); err != nil {
			t.Fatal(err)
		}
	}
	errs, err := s.BatchDeleteMapping(ctx, []string{"a", "missing", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 3 || errs[0] != nil || errs[1] != ierr.ErrNotFound || errs[2] != nil {
		t.Fatalf("per-key errs = %v", errs)
	}
	if _, err := s.GetMapping(ctx, "a"); err != ierr.ErrNotFound {
		t.Fatalf("GetMapping(a) after batch delete = %v", err)
	}
	if m, err := s.GetMapping(ctx, "c"); err != nil || m.Offset != 8192 {
		t.Fatalf("untouched key c = %+v, %v", m, err)
	}
	if e := segState(t, s, 0); e.meta.AliveCount != 1 {
		t.Fatalf("seg0 alive = %d, want 1", e.meta.AliveCount)
	}

	// 全部缺失：不产生删除批次。
	errs, err = s.BatchDeleteMapping(ctx, []string{"x", "y"})
	if err != nil || len(errs) != 2 || errs[0] != ierr.ErrNotFound || errs[1] != ierr.ErrNotFound {
		t.Fatalf("all-missing = (%v,%v)", errs, err)
	}
}

// TestSegmentCRUDAndList：段状态写读、解码失败、遍历与遍历错误。
func TestSegmentCRUDAndList(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()
	ps := s.(*pebbleStore)

	if _, ok, err := s.GetSegment(ctx, 3); err != nil || ok {
		t.Fatalf("GetSegment(missing) = ok=%v err=%v, want (false,nil)", ok, err)
	}
	want := SegmentMeta{State: SegmentStateFull, AliveCount: 2, ReclaimSeq: 1}
	if err := s.PutSegment(ctx, 3, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetSegment(ctx, 3)
	if err != nil || !ok || got != want {
		t.Fatalf("GetSegment = (%+v,%v,%v), want %+v", got, ok, err, want)
	}
	if err := ps.db.Set(keyState(segmentKey(4)), []byte("xx"), nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetSegment(ctx, 4); !ok || err == nil {
		t.Fatalf("GetSegment(corrupt) = ok=%v err=%v, want (true, err)", ok, err)
	}

	// ListSegments/SegmentStats 的数据源是内存引用表（PutSegment 只写库、不进内存表），
	// 故用 PutMapping 驱动出两个段条目。
	if err := s.PutMapping(ctx, "a", ObjectMeta{SegmentID: 0, Offset: 0, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMapping(ctx, "b", ObjectMeta{SegmentID: 1, Offset: 0, Size: 4}); err != nil {
		t.Fatal(err)
	}
	// MarkCompacting：非 Full/不存在的段为空操作。
	if err := s.MarkCompacting(ctx, 999); err != nil {
		t.Fatalf("MarkCompacting(missing) = %v", err)
	}
	var listed []int64
	if err := s.ListSegments(ctx, func(id int64, m SegmentMeta) error {
		listed = append(listed, id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("ListSegments ids = %v, want 2 段", listed)
	}
	if err := s.ListSegments(ctx, func(int64, SegmentMeta) error { return errInjected }); err != errInjected {
		t.Fatalf("ListSegments fn error = %v", err)
	}
	// 段状态汇总。
	stats := s.SegmentStats()
	if stats[SegmentStateActive] != 2 {
		t.Fatalf("SegmentStats = %v, want 2 Active", stats)
	}
}

// TestCursorAndAllocateSegmentBatch：游标初值、批量分配等价性与空间不足路径。
func TestCursorAndAllocateSegmentBatch(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	if seg, off := s.Cursor(); seg != 0 || off != 0 {
		t.Fatalf("initial Cursor = (%d,%d), want (0,0)", seg, off)
	}

	if res, err := s.AllocateSegmentBatch(nil); err != nil || len(res) != 0 {
		t.Fatalf("AllocateSegmentBatch(nil) = (%v,%v)", res, err)
	}
	res, err := s.AllocateSegmentBatch([]int64{layout.BlockSize, 2 * layout.BlockSize})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].SegmentID != 0 || res[0].Offset != 0 || res[1].Offset != layout.BlockSize {
		t.Fatalf("AllocateSegmentBatch = %+v", res)
	}
	if seg, off := s.Cursor(); seg != 0 || off != 3*layout.BlockSize {
		t.Fatalf("Cursor after batch = (%d,%d)", seg, off)
	}

	// 空间不足：段数=1、申请两段 → 第二次分配 ErrNoSpace。
	db := openRawDB(t, t.TempDir())
	defer db.Close()
	m := newSegmentManager(db)
	ps := &pebbleStore{db: db, cache: &metaCache{}, segs: m, alloc: &allocator{
		db: db, segs: m, segSize: layout.BlockSize, segCount: 1, cursorLoaded: true,
	}}
	if _, err := ps.AllocateSegmentBatch([]int64{layout.BlockSize, layout.BlockSize}); err != ierr.ErrNoSpace {
		t.Fatalf("AllocateSegmentBatch full = %v, want ErrNoSpace", err)
	}
	if _, _, err := ps.allocate(context.Background(), layout.BlockSize, true); err != ierr.ErrNoSpace {
		t.Fatalf("allocate with reserve = %v, want ErrNoSpace", err)
	}
}

// TestCursorPersistedAndRestored：游标持久化，重启后懒加载恢复写位置。
func TestCursorPersistedAndRestored(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, testLayout)
	if err != nil {
		t.Fatal(err)
	}
	// 首次分配即恢复游标（无持久化游标 → 从 0 开始）。
	if seg, off, err := s.AllocateSegment(layout.BlockSize); err != nil || seg != 0 || off != 0 {
		t.Fatalf("first alloc = (%d,%d,%v), want (0,0,nil)", seg, off, err)
	}
	if seg, off := s.Cursor(); seg != 0 || off != layout.BlockSize {
		t.Fatalf("Cursor = (%d,%d)", seg, off)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir, testLayout)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	// 重启后首分配从持久化游标续写（懒加载）。
	seg, off, err := s2.AllocateSegment(layout.BlockSize)
	if err != nil {
		t.Fatal(err)
	}
	if seg != 0 || off != layout.BlockSize {
		t.Fatalf("resumed alloc = (%d,%d), want (0,%d)", seg, off, layout.BlockSize)
	}
}

// TestUsedBytesActiveNonCursorSegment：非游标段的 Active 段按整段计入已用。
func TestUsedBytesActiveNonCursorSegment(t *testing.T) {
	db := openRawDB(t, t.TempDir())
	defer db.Close()
	m := newSegmentManager(db)
	m.mu.Lock()
	m.segs[0] = &segEntry{meta: SegmentMeta{State: SegmentStateActive}}
	m.segs[1] = &segEntry{meta: SegmentMeta{State: SegmentStateActive}}
	m.mu.Unlock()
	ps := &pebbleStore{db: db, cache: &metaCache{}, segs: m, alloc: &allocator{
		db: db, segs: m, segSize: testLayout.SegmentSizeBytes, segCount: testLayout.SegmentCount,
		curSeg: 1, curOff: 4096, cursorLoaded: true,
	}}
	want := testLayout.SegmentSizeBytes + 4096
	if got := ps.UsedBytes(); got != want {
		t.Fatalf("UsedBytes = %d, want %d", got, want)
	}
	if st := ps.SegmentStats(); st[SegmentStateActive] != 2 {
		t.Fatalf("SegmentStats = %v, want 2 Active", st)
	}
}

// TestAllocatorCursorDecodeError：持久化游标损坏时分配报错（懒加载路径）。
func TestAllocatorCursorDecodeError(t *testing.T) {
	dir := t.TempDir()
	db := openRawDB(t, dir)
	if err := db.Set(keyState([]byte(kvCursorKey)), []byte("bad"), syncWO); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, testLayout)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.AllocateSegment(layout.BlockSize); err == nil {
		t.Fatal("AllocateSegment with corrupt cursor should fail")
	}
}

// TestCloseBranches：退化 store（无 db / 无 segmentManager）的关闭语义。
func TestCloseBranches(t *testing.T) {
	if err := (&pebbleStore{}).Close(); err != nil {
		t.Fatalf("Close(empty) = %v", err)
	}
	db := openRawDB(t, t.TempDir())
	if err := (&pebbleStore{db: db}).Close(); err != nil {
		t.Fatalf("Close(no segs) = %v", err)
	}
}

// TestOpenErrors：pebble 打开失败、段键/段值/映射损坏时 Open 报错。
func TestOpenErrors(t *testing.T) {
	// 目录位置是普通文件 → pebble 打开失败。
	file := filepath.Join(t.TempDir(), "plain-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file, testLayout); err == nil {
		t.Fatal("Open on regular file should fail")
	}

	// 段键长度非法。
	dir1 := t.TempDir()
	db1 := openRawDB(t, dir1)
	badKey := append(keyState([]byte(kvSegmentPrefix)), 0x01)
	if err := db1.Set(badKey, []byte{}, syncWO); err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir1, testLayout); err == nil {
		t.Fatal("Open with bad segment key should fail")
	}

	// 段值损坏。
	dir2 := t.TempDir()
	db2 := openRawDB(t, dir2)
	if err := db2.Set(keyState(segmentKey(1)), []byte("xx"), syncWO); err != nil {
		t.Fatal(err)
	}
	if err := db2.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir2, testLayout); err == nil {
		t.Fatal("Open with corrupt segment value should fail")
	}

	// mapping 损坏（重建存活计数失败）。
	dir3 := t.TempDir()
	db3 := openRawDB(t, dir3)
	if err := db3.Set(keyMapping("bad"), []byte("xx"), syncWO); err != nil {
		t.Fatal(err)
	}
	if err := db3.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir3, testLayout); err == nil {
		t.Fatal("Open with corrupt mapping should fail")
	}
}

// TestRebuildRestoresFreeAndMissingSegments：重启重建段状态、Free 段回退与空闲池清理。
func TestRebuildRestoresFreeAndMissingSegments(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openRawDB(t, dir)
	if err := db.Set(keyState(segmentKey(7)), SegmentMeta{State: SegmentStateFree, ReclaimSeq: 3}.encode(), syncWO); err != nil {
		t.Fatal(err)
	}
	if err := db.Set(keyMapping("k"), ObjectMeta{SegmentID: 7, Offset: 0, Size: 4}.encode(), syncWO); err != nil {
		t.Fatal(err)
	}
	// 段 9 无段记录：由 mapping 重建。
	if err := db.Set(keyMapping("k2"), ObjectMeta{SegmentID: 9, Offset: 0, Size: 4}.encode(), syncWO); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir, testLayout)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ps := s.(*pebbleStore)

	if e := segState(t, s, 7); e == nil || e.meta.AliveCount != 1 || e.meta.State != SegmentStateActive {
		t.Fatalf("seg7 = %+v, want Active AliveCount=1", e)
	}
	if e := segState(t, s, 9); e == nil || e.meta.AliveCount != 1 || e.meta.State != SegmentStateActive {
		t.Fatalf("seg9 = %+v, want Active AliveCount=1", e)
	}
	ps.segs.mu.Lock()
	free := append([]int64(nil), ps.segs.free...)
	nseg := len(ps.segs.segs)
	ps.segs.mu.Unlock()
	if len(free) != 0 {
		t.Fatalf("free pool = %v, want empty (段 7 有存活对象)", free)
	}
	if nseg != 2 {
		t.Fatalf("segments = %d, want 2", nseg)
	}
	// 重建后 mapping 读取正常。
	if m, err := s.GetMapping(ctx, "k"); err != nil || m.SegmentID != 7 {
		t.Fatalf("GetMapping(k) = %+v, %v", m, err)
	}
}

// TestSegmentManagerCanUse：无记录段可用、Active 段不可用、Free 段可用。
func TestSegmentManagerCanUse(t *testing.T) {
	db := openRawDB(t, t.TempDir())
	defer db.Close()
	m := newSegmentManager(db)
	if !m.canUse(42) {
		t.Fatal("unknown segment should be usable")
	}
	m.mu.Lock()
	m.segs[42] = &segEntry{meta: SegmentMeta{State: SegmentStateActive}}
	m.mu.Unlock()
	if m.canUse(42) {
		t.Fatal("active segment should not be usable")
	}
	m.mu.Lock()
	m.segs[42].meta.State = SegmentStateFree
	m.mu.Unlock()
	if !m.canUse(42) {
		t.Fatal("free segment should be usable")
	}
}
