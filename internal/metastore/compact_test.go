package metastore

import (
	"context"
	"testing"

	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/pkg/ierr"
)

// TestCompactingRebuildWithLiveObjects：搬移中断（Compacting 段仍有存活对象）重启后回退 Full，
// 下轮由搬移任务重新选中（幂等续搬）。
func TestCompactingRebuildWithLiveObjects(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir, testLayout)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutMapping(ctx, "a", ObjectMeta{SegmentID: 0, Offset: 0, Size: 4}); err != nil {
		t.Fatal(err)
	}
	// 模拟：段 0 已写满（Full）并被搬移任务标记为 Compacting，但对象尚未搬走。
	ps := s.(*pebbleStore)
	ps.segs.mu.Lock()
	e := ps.segs.segs[0]
	e.meta.State = SegmentStateFull
	ps.segs.mu.Unlock()
	if err := ps.segs.persistLocked(ctx, 0, e); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCompacting(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if e := segState(t, s, 0); e.meta.State != SegmentStateCompacting {
		t.Fatalf("state = %v, want Compacting", e.meta.State)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 重启：计数从 mapping 重建（1），Compacting 段应回退 Full。
	s2, err := Open(dir, testLayout)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if e := segState(t, s2, 0); e.meta.State != SegmentStateFull || e.meta.AliveCount != 1 {
		t.Fatalf("after reopen = %+v, want Full AliveCount=1", e.meta)
	}
}

// TestCompactingRebuildEmptyToReclaiming：搬完未收尾（Compacting 段已无存活对象）重启后直接转 Reclaiming，
// 由后台 GC 收尾入池。
func TestCompactingRebuildEmptyToReclaiming(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir, testLayout)
	if err != nil {
		t.Fatal(err)
	}
	ps := s.(*pebbleStore)
	ps.segs.mu.Lock()
	ps.segs.segs[1] = &segEntry{meta: SegmentMeta{State: SegmentStateCompacting}}
	ps.segs.mu.Unlock()
	if err := ps.segs.persistLocked(ctx, 1, ps.segs.segs[1]); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir, testLayout)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if e := segState(t, s2, 1); e.meta.State != SegmentStateReclaiming {
		t.Fatalf("after reopen = %+v, want Reclaiming", e.meta)
	}
}

// TestMoveMappingCAS：条件写搬移映射——成功（计数转移）、冲突（ErrConflict 不修改）、
// key 不存在（ErrConflict）、同段移动（计数不变）。
func TestMoveMappingCAS(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()

	old := ObjectMeta{SegmentID: 0, Offset: 0, Size: 4}
	if err := s.PutMapping(ctx, "k", old); err != nil {
		t.Fatal(err)
	}

	// 搬移到段 1：成功，映射更新；段 0 计数归零转 Reclaiming；段 1 计数 1。
	nw := ObjectMeta{SegmentID: 1, Offset: 0, Size: 4}
	if err := s.MoveMapping(ctx, "k", old, nw); err != nil {
		t.Fatalf("MoveMapping: %v", err)
	}
	if m, err := s.GetMapping(ctx, "k"); err != nil || m != nw {
		t.Fatalf("GetMapping = %+v, %v; want %+v", m, err, nw)
	}
	if e := segState(t, s, 0); e.meta.AliveCount != 0 || e.meta.State != SegmentStateReclaiming {
		t.Fatalf("seg0 = %+v, want AliveCount=0 Reclaiming", e.meta)
	}
	if e := segState(t, s, 1); e.meta.AliveCount != 1 {
		t.Fatalf("seg1 = %+v, want AliveCount=1", e.meta)
	}

	// 冲突：old 与当前映射不一致 → ErrConflict，数据不变。
	if err := s.MoveMapping(ctx, "k", old, ObjectMeta{SegmentID: 2, Offset: 0, Size: 4}); err != ierr.ErrConflict {
		t.Fatalf("stale MoveMapping err = %v, want ErrConflict", err)
	}
	if m, err := s.GetMapping(ctx, "k"); err != nil || m != nw {
		t.Fatalf("mapping changed on conflict: %+v, %v", m, err)
	}

	// key 不存在 → ErrConflict。
	if err := s.MoveMapping(ctx, "missing", nw, ObjectMeta{SegmentID: 2, Offset: 0, Size: 4}); err != ierr.ErrConflict {
		t.Fatalf("missing key MoveMapping err = %v, want ErrConflict", err)
	}

	// 同段移动（old.SegmentID == new.SegmentID）：段 1 计数不变、映射更新到新偏移。
	move2 := ObjectMeta{SegmentID: 1, Offset: 4096, Size: 4}
	if err := s.MoveMapping(ctx, "k", nw, move2); err != nil {
		t.Fatalf("same-seg MoveMapping: %v", err)
	}
	if e := segState(t, s, 1); e.meta.AliveCount != 1 {
		t.Fatalf("seg1 alive after same-seg move = %d, want 1", e.meta.AliveCount)
	}
	if m, err := s.GetMapping(ctx, "k"); err != nil || m != move2 {
		t.Fatalf("GetMapping = %+v, %v; want %+v", m, err, move2)
	}
}

// TestReserveSegmentsNotUsableByUser：预留缓冲段对用户写路径不可达（滚动上限排除），
// 而搬移专用分配 AllocateSegmentReserve 可滚动进入。
func TestReserveSegmentsNotUsableByUser(t *testing.T) {
	db := openRawDB(t, t.TempDir())
	defer db.Close()

	mkAlloc := func() *pebbleStore {
		m := newSegmentManager(db)
		a := &allocator{
			curSeg:       testLayout.SegmentCount - reserveSegs - 1, // 预留区前最后一段
			curOff:       testLayout.SegmentSizeBytes,               // 已写满
			segs:         m,
			segSize:      testLayout.SegmentSizeBytes,
			segCount:     testLayout.SegmentCount,
			cursorLoaded: true,
			db:           db,
		}
		return &pebbleStore{db: db, alloc: a, cache: &metaCache{}, segs: m}
	}

	// 用户路径：无法滚入预留区（下一段即预留区首段，被上限排除），空闲池空 → ErrNoSpace。
	if _, _, err := mkAlloc().AllocateSegment(layout.BlockSize); err != ierr.ErrNoSpace {
		t.Fatalf("user alloc err = %v, want ErrNoSpace", err)
	}

	// 搬移专用路径：可滚入预留区首段，偏移从 0 开始。
	seg, off, err := mkAlloc().AllocateSegmentReserve(layout.BlockSize)
	if err != nil {
		t.Fatalf("reserve alloc: %v", err)
	}
	if want := testLayout.SegmentCount - reserveSegs; seg != want || off != 0 {
		t.Fatalf("reserve alloc = (%d,%d), want (%d,0)", seg, off, want)
	}
}
