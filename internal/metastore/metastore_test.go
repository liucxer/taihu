package metastore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/pkg/ierr"
)

// 本文件收纳 metastore 层的全部测试（原 store_test.go / compact_test.go /
// segments_test.go / meta_test.go / cache_test.go 合并而来——本层测试统一单文件，
// 与 aio / device 的平台拆分先例不同，此处是同层合并）。
// 公共辅助（openTestStore / segState 等）见文件底部。

// errInjected 注入的存储错误（用于错误分支断言）。
var errInjected = errors.New("injected store failure")

// testLayout 测试用默认布局：段大小 8GB、段数 2048（等价 v1 硬编码 16TB 布局）。
var testLayout = layout.Layout{SegmentSizeBytes: layout.DefaultSegmentSizeBytes, SegmentCount: 2048}

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
	s, err := Open(t.TempDir(), testLayout)
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

// ---- meta 层：编解码往返与键编码 ----

// TestCodecRoundTripAndDecodeErrors：编解码往返与长度非法时的错误分支。
func TestCodecRoundTripAndDecodeErrors(t *testing.T) {
	om := ObjectMeta{SegmentID: 3, Offset: 4096, Size: 100}
	got, err := decodeObjectMeta(om.encode())
	if err != nil || got != om {
		t.Fatalf("ObjectMeta roundtrip = (%+v,%v), want %+v", got, err, om)
	}
	if _, err := decodeObjectMeta([]byte{metaVersion, 1}); err == nil {
		t.Fatal("decodeObjectMeta with short value should fail")
	}

	wc := writeCursor{SegmentID: 2, Offset: 8192}
	gotC, err := decodeWriteCursor(wc.encode())
	if err != nil || gotC != wc {
		t.Fatalf("WriteCursor roundtrip = (%+v,%v), want %+v", gotC, err, wc)
	}
	if _, err := decodeWriteCursor(nil); err == nil {
		t.Fatal("decodeWriteCursor with empty value should fail")
	}

	sm := SegmentMeta{State: SegmentStateCompacting, AliveCount: 7, ReclaimSeq: 2}
	gotS, err := decodeSegmentMeta(sm.encode())
	if err != nil || gotS != sm {
		t.Fatalf("SegmentMeta roundtrip = (%+v,%v), want %+v", gotS, err, sm)
	}
	if _, err := decodeSegmentMeta([]byte{metaVersion}); err == nil {
		t.Fatal("decodeSegmentMeta with short value should fail")
	}

	// segmentKey：前缀 + 小端 8 字节段号。
	key := segmentKey(1)
	if len(key) != len(kvSegmentPrefix)+8 {
		t.Fatalf("segmentKey len = %d", len(key))
	}
	if string(key[:len(kvSegmentPrefix)]) != kvSegmentPrefix {
		t.Fatalf("segmentKey prefix = %q", key)
	}
}

// TestAppendUpper：返回前缀独占上界且不修改入参。
func TestAppendUpper(t *testing.T) {
	prefix := []byte("m\x00")
	upper := appendUpper(prefix)
	if string(upper) != "m\x01" {
		t.Fatalf("appendUpper = %q, want m\\x01", upper)
	}
	if string(prefix) != "m\x00" {
		t.Fatalf("appendUpper mutated input: %q", prefix)
	}
}

// ---- 缓存层：分片 LRU 预算 ----

func TestCacheEvictionBudget(t *testing.T) {
	c := &metaCache{}
	// 塞入足够条目触发逐出，验证各分片 usedBytes 不超 perShardLimit
	for i := 0; i < 100000; i++ {
		k := "key-" + strconv.Itoa(i)
		c.put(k, ObjectMeta{SegmentID: int64(i)})
	}
	for i := 0; i < shardCount; i++ {
		if c.shards[i].usedBytes > perShardLimit {
			t.Fatalf("shard %d over budget: %d > %d", i, c.shards[i].usedBytes, perShardLimit)
		}
	}
}

// TestCacheEvictsOversizedEntry：单条即超分片预算时被逐出（LRU 回收），
// 且不影响后续小条目写入。
func TestCacheEvictsOversizedEntry(t *testing.T) {
	c := &metaCache{}
	big := strings.Repeat("k", int(perShardLimit)+100)
	c.put(big, ObjectMeta{SegmentID: 1})
	if _, ok := c.get(big); ok {
		t.Fatal("oversized entry should be evicted")
	}
	s := c.shardFor(big)
	s.mu.Lock()
	used, n := s.usedBytes, len(s.entries)
	s.mu.Unlock()
	if used != 0 || n != 0 {
		t.Fatalf("shard after eviction = (used=%d, entries=%d), want (0,0)", used, n)
	}

	// 小条目正常写入并可命中。
	c.put("small", ObjectMeta{SegmentID: 2, Offset: 4096})
	if m, ok := c.get("small"); !ok || m.SegmentID != 2 || m.Offset != 4096 {
		t.Fatalf("small entry = (%+v,%v)", m, ok)
	}
}

// TestCacheUpsertAndDelete：同 key 覆盖写为 MRU、删除归还预算、删除不存在 key 为空操作。
func TestCacheUpsertAndDelete(t *testing.T) {
	c := &metaCache{}
	c.put("a", ObjectMeta{SegmentID: 1})
	c.put("a", ObjectMeta{SegmentID: 2})
	if m, ok := c.get("a"); !ok || m.SegmentID != 2 {
		t.Fatalf("upsert = (%+v,%v)", m, ok)
	}
	s := c.shardFor("a")
	s.mu.Lock()
	usedAfterPut := s.usedBytes
	s.mu.Unlock()
	if usedAfterPut != int64(len("a"))+perEntryOverhead {
		t.Fatalf("usedBytes after upsert = %d", usedAfterPut)
	}
	c.del("a")
	if _, ok := c.get("a"); ok {
		t.Fatal("entry not deleted")
	}
	s.mu.Lock()
	usedAfterDel := s.usedBytes
	s.mu.Unlock()
	if usedAfterDel != 0 {
		t.Fatalf("usedBytes after delete = %d, want 0", usedAfterDel)
	}
	c.del("a") // 不存在：空操作
}

// ---- mapping / 分配 / 段 CRUD：Store 接口行为 ----

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

// ---- segments：存活计数 / 引用 / 分配复用 / 后台 GC ----

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

	a := &allocator{
		curSeg:       testLayout.SegmentCount - 1,
		curOff:       testLayout.SegmentSizeBytes,
		segs:         m,
		segSize:      testLayout.SegmentSizeBytes,
		segCount:     testLayout.SegmentCount,
		cursorLoaded: true,
	}
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
	s, err := Open(dir, testLayout)
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

	s2, err := Open(dir, testLayout)
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
	if err := ps.mapping().iter(func(key string, m ObjectMeta) error {
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
	a.curSeg = testLayout.SegmentCount - 1
	a.curOff = testLayout.SegmentSizeBytes
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

// TestUsedBytes：Full/Reclaiming 段计整段、Active 段计已写偏移。
func TestUsedBytes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openRawDB(t, dir)
	defer db.Close()

	m := newSegmentManager(db)
	m.mu.Lock()
	m.segs[0] = &segEntry{meta: SegmentMeta{State: SegmentStateFull}}
	m.segs[1] = &segEntry{meta: SegmentMeta{State: SegmentStateActive}}
	m.segs[2] = &segEntry{meta: SegmentMeta{State: SegmentStateReclaiming}}
	m.mu.Unlock()
	a := &allocator{segs: m, segSize: testLayout.SegmentSizeBytes, segCount: testLayout.SegmentCount, curSeg: 1, curOff: 4096}
	s := &pebbleStore{db: db, alloc: a, cache: &metaCache{}, segs: m}
	_ = ctx

	// Full(seg0) + Active(seg1, curOff 4096) + Reclaiming(seg2) = 2*segSize + 4096。
	want := 2*testLayout.SegmentSizeBytes + 4096
	if got := s.UsedBytes(); got != want {
		t.Fatalf("UsedBytes=%d want %d", got, want)
	}
}

// ---- compaction：CAS 搬移与预留缓冲段 ----

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
