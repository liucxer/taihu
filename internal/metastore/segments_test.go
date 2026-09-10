package metastore

import (
	"context"
	"testing"

	"github.com/cockroachdb/pebble"

	"github.com/liucxer/taihu/internal/layout"
)

func openRawDB(t *testing.T, dir string) *pebble.DB {
	t.Helper()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	return db
}

func openTestStore(t *testing.T) Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func segState(t *testing.T, s Store, id int64) *segEntry {
	t.Helper()
	ps := s.(*pebbleStore)
	ps.segs.mu.Lock()
	defer ps.segs.mu.Unlock()
	return ps.segs.segs[id]
}

// TestAliveCountAndReclaim：Put/Delete 维护存活计数；删光转 Reclaiming；GC 回收为 Free 入池。
func TestAliveCountAndReclaim(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()

	// 段 0 写两个对象（段 ID 由调用方指定，映射写入驱动计数）。
	if err := s.PutMapping(ctx, "a", ObjectMeta{SegmentID: 0, Offset: 0, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMapping(ctx, "b", ObjectMeta{SegmentID: 0, Offset: 4096, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if e := segState(t, s, 0); e.meta.AliveCount != 2 {
		t.Fatalf("AliveCount = %d, want 2", e.meta.AliveCount)
	}

	// 删一个 → 1
	if err := s.DeleteMapping(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if e := segState(t, s, 0); e.meta.AliveCount != 1 {
		t.Fatalf("AliveCount = %d, want 1", e.meta.AliveCount)
	}

	// 删光 → 0 且 Reclaiming
	if err := s.DeleteMapping(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	e := segState(t, s, 0)
	if e.meta.AliveCount != 0 || e.meta.State != SegmentStateReclaiming {
		t.Fatalf("state = %+v, want AliveCount=0 Reclaiming", e.meta)
	}

	// GC 回收 → Free 入池
	ps := s.(*pebbleStore)
	if n := ps.segs.reclaimOnce(ctx); n != 1 {
		t.Fatalf("reclaimOnce = %d, want 1", n)
	}
	e = segState(t, s, 0)
	if e.meta.State != SegmentStateFree || e.meta.ReclaimSeq != 1 {
		t.Fatalf("after reclaim = %+v, want Free ReclaimSeq=1", e.meta)
	}
	if len(ps.segs.free) != 1 || ps.segs.free[0] != 0 {
		t.Fatalf("free pool = %v, want [0]", ps.segs.free)
	}
}

// TestRefBlocksReclaim：在途读引用阻止回收；释放后才回收。
func TestRefBlocksReclaim(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()

	ps := s.(*pebbleStore)
	if err := s.PutMapping(ctx, "a", ObjectMeta{SegmentID: 0, Offset: 0, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMapping(ctx, "a"); err != nil {
		t.Fatal(err)
	}

	s.RefSegment(0)
	if n := ps.segs.reclaimOnce(ctx); n != 0 {
		t.Fatalf("reclaim with ref = %d, want 0", n)
	}
	if e := segState(t, s, 0); e.meta.State != SegmentStateReclaiming {
		t.Fatalf("state = %v, want Reclaiming", e.meta.State)
	}

	s.UnrefSegment(0)
	if n := ps.segs.reclaimOnce(ctx); n != 1 {
		t.Fatalf("reclaim after unref = %d, want 1", n)
	}
	if e := segState(t, s, 0); e.meta.State != SegmentStateFree {
		t.Fatalf("state = %v, want Free", e.meta.State)
	}
}

// TestOverwriteSameKey：覆盖写扣减旧段计数（不泄漏），同段覆盖计数不变。
func TestOverwriteSameKey(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()

	if err := s.PutMapping(ctx, "k", ObjectMeta{SegmentID: 0, Offset: 0, Size: 4}); err != nil {
		t.Fatal(err)
	}
	// 同 key 覆盖到段 1：段 0 计数归零转 Reclaiming，段 1 计数 1。
	if err := s.PutMapping(ctx, "k", ObjectMeta{SegmentID: 1, Offset: 0, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if e := segState(t, s, 0); e.meta.AliveCount != 0 || e.meta.State != SegmentStateReclaiming {
		t.Fatalf("seg0 = %+v, want AliveCount=0 Reclaiming", e.meta)
	}
	if e := segState(t, s, 1); e.meta.AliveCount != 1 {
		t.Fatalf("seg1 = %+v, want AliveCount=1", e.meta)
	}

	// 同 key 覆盖回段 0：段 1 归零 Reclaiming；段 0 重新激活（AliveCount=1，旧对象被覆盖不新增）。
	if err := s.PutMapping(ctx, "k", ObjectMeta{SegmentID: 0, Offset: 4096, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if e := segState(t, s, 0); e.meta.AliveCount != 1 || e.meta.State != SegmentStateActive {
		t.Fatalf("seg0 = %+v, want AliveCount=1 Active", e.meta)
	}
	if e := segState(t, s, 1); e.meta.AliveCount != 0 || e.meta.State != SegmentStateReclaiming {
		t.Fatalf("seg1 = %+v, want AliveCount=0 Reclaiming", e.meta)
	}
}

// TestAllocatorReuseFreeSegment：游标到顶时从空闲池取段复用（游标回跳），随后继续顺序写。
func TestAllocatorReuseFreeSegment(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openRawDB(t, dir)
	defer db.Close()

	m := newSegmentManager(db)
	m.mu.Lock()
	m.segs[5] = &segEntry{meta: SegmentMeta{State: SegmentStateFree, ReclaimSeq: 1}}
	m.free = []int64{5}
	m.mu.Unlock()
	if err := m.persistLocked(ctx, 5, m.segs[5]); err != nil {
		t.Fatal(err)
	}

	a := &allocator{curSeg: layout.SegmentCount - 1, curOff: layout.SegmentSizeBytes, segs: m, cursorLoaded: true}
	s := &pebbleStore{db: db, alloc: a, cache: &metaCache{}, segs: m}

	// 游标已到顶且下一段不存在：取空闲段 5。
	seg, off, err := s.AllocateSegment(layout.BlockSize)
	if err != nil {
		t.Fatal(err)
	}
	if seg != 5 || off != 0 {
		t.Fatalf("alloc = (%d,%d), want (5,0)", seg, off)
	}
	if e := m.segs[5]; e.meta.State != SegmentStateActive {
		t.Fatalf("seg5 state = %v, want Active", e.meta.State)
	}
	if len(m.free) != 0 {
		t.Fatalf("free = %v, want empty", m.free)
	}

	// 复用后继续顺序写同一段。
	seg2, off2, err := s.AllocateSegment(layout.BlockSize)
	if err != nil {
		t.Fatal(err)
	}
	if seg2 != 5 || off2 != layout.BlockSize {
		t.Fatalf("alloc = (%d,%d), want (5,%d)", seg2, off2, layout.BlockSize)
	}
}

// TestRebuildFromMapping：重启后段计数/状态从 mapping 与 seg/ 记录重建。
func TestRebuildFromMapping(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range []struct {
		k string
		m ObjectMeta
	}{
		{"a", ObjectMeta{SegmentID: 0, Offset: 0, Size: 4}},
		{"b", ObjectMeta{SegmentID: 0, Offset: 4096, Size: 4}},
		{"c", ObjectMeta{SegmentID: 1, Offset: 0, Size: 4}},
	} {
		if err := s.PutMapping(ctx, kv.k, kv.m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if e := segState(t, s2, 0); e == nil || e.meta.AliveCount != 2 {
		t.Fatalf("seg0 = %+v, want AliveCount=2", e)
	}
	if e := segState(t, s2, 1); e == nil || e.meta.AliveCount != 1 {
		t.Fatalf("seg1 = %+v, want AliveCount=1", e)
	}
	// mapping 仍在：读路径不受影响。
	if m, err := s2.GetMapping(ctx, "a"); err != nil || m.SegmentID != 0 {
		t.Fatalf("GetMapping(a) = %+v, %v", m, err)
	}
}
