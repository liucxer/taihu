package metastore

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

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

// segState 返回段 id 的条目快照（深拷贝），调用方解锁后安全读取；
// 不直接返回内部指针，避免与后台 GC 的写竞争（-race）。
func segState(t *testing.T, s Store, id int64) *segEntry {
	t.Helper()
	ps := s.(*pebbleStore)
	ps.segs.mu.Lock()
	defer ps.segs.mu.Unlock()
	e := ps.segs.segs[id]
	if e == nil {
		return nil
	}
	cp := *e
	return &cp
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

// TestFreeSegmentReactivateOnPut：空闲池中的 Free 段被写入新对象 → 重新激活 Active 并移出空闲池。
func TestFreeSegmentReactivateOnPut(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()
	ps := s.(*pebbleStore)

	// 构造段 2 为 Free 并入池（模拟 GC 回收产物）。
	ps.segs.mu.Lock()
	ps.segs.segs[2] = &segEntry{meta: SegmentMeta{State: SegmentStateFree, ReclaimSeq: 1}}
	ps.segs.free = []int64{2}
	ps.segs.mu.Unlock()
	if err := ps.segs.persistLocked(ctx, 2, ps.segs.segs[2]); err != nil {
		t.Fatal(err)
	}

	// 新对象写入该段 → Active、AliveCount=1、移出空闲池。
	if err := s.PutMapping(ctx, "k", ObjectMeta{SegmentID: 2, Offset: 0, Size: 4}); err != nil {
		t.Fatal(err)
	}
	e := segState(t, s, 2)
	if e.meta.State != SegmentStateActive || e.meta.AliveCount != 1 {
		t.Fatalf("seg2 = %+v, want Active AliveCount=1", e.meta)
	}
	if len(ps.segs.free) != 0 {
		t.Fatalf("free = %v, want empty", ps.segs.free)
	}
}

// TestReclaimSeqIncrements：每轮「删光→回收」世代号递增。
func TestReclaimSeqIncrements(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()
	ps := s.(*pebbleStore)

	round := func(key string, seg int64, wantSeq int64) {
		t.Helper()
		if err := s.PutMapping(ctx, key, ObjectMeta{SegmentID: seg, Offset: 0, Size: 4}); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteMapping(ctx, key); err != nil {
			t.Fatal(err)
		}
		if n := ps.segs.reclaimOnce(ctx); n != 1 {
			t.Fatalf("reclaimOnce = %d, want 1", n)
		}
		e := segState(t, s, seg)
		if e.meta.State != SegmentStateFree || e.meta.ReclaimSeq != wantSeq {
			t.Fatalf("seg%d = %+v, want Free ReclaimSeq=%d", seg, e.meta, wantSeq)
		}
	}
	round("r0", 0, 1)
	round("r0b", 0, 2)
}

// TestBackgroundGCPeriodic：后台 GC goroutine 按周期自动回收 Reclaiming 段（Open 即启动）。
func TestBackgroundGCPeriodic(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()

	if err := s.PutMapping(ctx, "a", ObjectMeta{SegmentID: 0, Offset: 0, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMapping(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if e := segState(t, s, 0); e.meta.State != SegmentStateReclaiming {
		t.Fatalf("state = %v, want Reclaiming", e.meta.State)
	}
	// 等待后台 GC 一个周期内自动回收。
	deadline := time.Now().Add(3 * gcInterval)
	for time.Now().Before(deadline) {
		e := segState(t, s, 0)
		if e.meta.State == SegmentStateFree {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("segment not reclaimed by background GC within 3s")
}

// TestRefOnMissingSegment：对未知段 Ref/Unref 为空操作，不 panic、不产生段记录。
func TestRefOnMissingSegment(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	s.RefSegment(999)
	s.UnrefSegment(999)
	ps := s.(*pebbleStore)
	ps.segs.mu.Lock()
	_, exists := ps.segs.segs[999]
	ps.segs.mu.Unlock()
	if exists {
		t.Fatal("Ref on missing segment created an entry")
	}
}

// TestConcurrentPutDeleteGC：并发写/删/引用/回收下，内存计数与 mapping 一致（-race 下运行）。
func TestConcurrentPutDeleteGC(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()
	ps := s.(*pebbleStore)

	const segs, perSeg = 4, 8
	var keys []string
	for seg := 0; seg < segs; seg++ {
		for i := 0; i < perSeg; i++ {
			k := fmt.Sprintf("s%d-%d", seg, i)
			keys = append(keys, k)
			if err := s.PutMapping(ctx, k, ObjectMeta{SegmentID: int64(seg), Offset: int64(i) * 4096, Size: 4}); err != nil {
				t.Fatal(err)
			}
		}
	}

	var wg sync.WaitGroup
	// 删除者：4 路并发删光全部 key（互不重叠）。
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < len(keys); i += 4 {
				if err := s.DeleteMapping(ctx, keys[i]); err != nil {
					t.Errorf("DeleteMapping(%s): %v", keys[i], err)
				}
			}
		}(w)
	}
	// 引用者：随机 Ref/Unref 段。
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(42))
		for i := 0; i < 2000; i++ {
			seg := int64(rng.Intn(segs))
			if rng.Intn(2) == 0 {
				s.RefSegment(seg)
			} else {
				s.UnrefSegment(seg)
			}
		}
	}()
	// GC：持续回收。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			ps.segs.reclaimOnce(ctx)
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()

	// 对账：扫描 mapping，各段 AliveCount 应与其一致（全部删除 → 0）。
	alive := make(map[int64]int)
	if err := iterateMapping(ps.db, func(key string, m ObjectMeta) error {
		alive[m.SegmentID]++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ps.segs.mu.Lock()
	defer ps.segs.mu.Unlock()
	for seg := int64(0); seg < segs; seg++ {
		e := ps.segs.segs[seg]
		if e == nil {
			continue
		}
		if want := alive[seg]; e.meta.AliveCount != int64(want) {
			t.Fatalf("seg%d AliveCount = %d, mapping says %d", seg, e.meta.AliveCount, want)
		}
	}
}

// TestFullLifecycleReuse：完整生命周期 写→删→GC→复用：
// 对象删光的段回收入池，分配器游标到顶后复用该段并继续顺序写。
func TestFullLifecycleReuse(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	defer s.Close()
	ps := s.(*pebbleStore)

	// 1. 段 0 写 2 个对象。
	for i, k := range []string{"a", "b"} {
		if err := s.PutMapping(ctx, k, ObjectMeta{SegmentID: 0, Offset: int64(i) * 4096, Size: 4096}); err != nil {
			t.Fatal(err)
		}
	}
	// 2. 删光 → Reclaiming。
	for _, k := range []string{"a", "b"} {
		if err := s.DeleteMapping(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	if e := segState(t, s, 0); e.meta.State != SegmentStateReclaiming {
		t.Fatalf("state = %v, want Reclaiming", e.meta.State)
	}
	// 3. GC 回收 → Free 入池。
	if n := ps.segs.reclaimOnce(ctx); n != 1 {
		t.Fatalf("reclaimOnce = %d, want 1", n)
	}
	// 4. 分配器游标拨到段尾（模拟顺序写满滚动）。
	a := ps.alloc
	a.mu.Lock()
	a.db = ps.db
	a.curSeg = layout.SegmentCount - 1
	a.curOff = layout.SegmentSizeBytes
	a.cursorLoaded = true
	a.mu.Unlock()
	// 5. 复用段 0。
	seg, off, err := s.AllocateSegment(4096)
	if err != nil {
		t.Fatal(err)
	}
	if seg != 0 || off != 0 {
		t.Fatalf("alloc = (%d,%d), want (0,0)", seg, off)
	}
	if e := segState(t, s, 0); e.meta.State != SegmentStateActive {
		t.Fatalf("seg0 state = %v, want Active", e.meta.State)
	}
	// 6. 复用后继续顺序写同一段。
	if seg2, off2, err := s.AllocateSegment(4096); err != nil || seg2 != 0 || off2 != 4096 {
		t.Fatalf("alloc2 = (%d,%d,%v), want (0,4096,nil)", seg2, off2, err)
	}
}
