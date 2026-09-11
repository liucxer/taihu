package metastore

import (
	"context"
	"fmt"
	"sync"

	"github.com/cockroachdb/pebble"

	"github.com/liucxer/taihu/internal/ierr"
	"github.com/liucxer/taihu/internal/layout"
)

// pebbleStore 基于 CockroachDB pebble（纯 Go LSM）实现 Store。
//
// pebble 为单 keyspace、无列族；此处用 key 前缀隔离两个逻辑命名空间：
//   - kvPrefixMapping：mapping（用户 key → ObjectMeta）
//   - kvPrefixState：state（cursor / seg/<id>）
//
// 写入默认 Sync，保证「先写设备数据 → 写 mapping → 更新 cursor」的持久化顺序，崩溃后游标不回退。

// 两个逻辑命名空间的前缀。用户 key 会被整体映射到 mapping 前缀之下，
// 因此即便用户 key 恰好以 state 前缀开头也不会与内部键冲突。
const (
	kvPrefixMapping = "m\x00"
	kvPrefixState   = "s\x00"
)

var _ Store = (*pebbleStore)(nil)

type pebbleStore struct {
	db    *pebble.DB
	alloc *allocator
	cache *metaCache // 加速层：mapping 的读缓存（read-through），pebble 仍为真实源
	segs  *segmentManager
}

// Open 打开 pebble DB。目录不存在时自动创建。l 为设备物理布局（段大小/段数），
// 由启动时读取的真实设备容量计算并注入 allocator（游标滚动/段尾判断依赖）。
// 写位置游标由内部的 allocator 在首次 AllocateSegment 时懒加载恢复，无需在启动时读取。
// segmentManager 在此重建段状态并启动后台 GC goroutine。
func Open(dir string, l layout.Layout) (Store, error) {
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		return nil, fmt.Errorf("taihu: open pebble %q: %w", dir, err)
	}
	s := &pebbleStore{
		db:    db,
		alloc: &allocator{segSize: l.SegmentSizeBytes, segCount: l.SegmentCount},
		cache: &metaCache{},
	}
	s.segs = newSegmentManager(db)
	s.alloc.segs = s.segs
	if err := s.segs.rebuild(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("taihu: rebuild segments: %w", err)
	}
	s.segs.run()
	return s, nil
}

// syncWO 用于需要落盘持久化的写入（mapping / cursor）。
var syncWO = &pebble.WriteOptions{Sync: true}

// reserveSegs 预留搬移缓冲段数：最后 reserveSegs 个段不参与用户写路径的顺序滚动，
// 仅 compaction 搬移（AllocateSegmentReserve）可动用。保证用户写满时搬移仍有落点，避免死锁。
// 占 2048 中 2 段 ≈ 0.1% 容量（设计文档 §4.5 over-provisioning）。
const reserveSegs = 2

func keyMapping(key string) []byte {
	b := make([]byte, 0, len(kvPrefixMapping)+len(key))
	b = append(b, kvPrefixMapping...)
	b = append(b, key...)
	return b
}

func keyState(inner []byte) []byte {
	b := make([]byte, 0, len(kvPrefixState)+len(inner))
	b = append(b, kvPrefixState...)
	b = append(b, inner...)
	return b
}

// get 通用读：返回值字节、是否存在。
func (s *pebbleStore) get(key []byte) ([]byte, bool, error) {
	v, closer, err := s.db.Get(key)
	if err != nil {
		if err == pebble.ErrNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	data := make([]byte, len(v))
	copy(data, v)
	_ = closer.Close()
	return data, true, nil
}

// GetMapping 读 mapping，缓存命中直接返回；未命中回查 pebble 并回填缓存。
func (s *pebbleStore) GetMapping(ctx context.Context, key string) (ObjectMeta, error) {
	if m, ok := s.cache.get(key); ok {
		return m, nil
	}
	v, found, err := s.get(keyMapping(key))
	if err != nil {
		return ObjectMeta{}, err
	}
	if !found {
		return ObjectMeta{}, ierr.ErrNotFound
	}
	m, err := decodeObjectMeta(v)
	if err != nil {
		return ObjectMeta{}, err
	}
	s.cache.put(key, m)
	return m, nil
}

// PutMapping 写 mapping（WriteBatch 原子含段存活计数更新），写穿：pebble 成功后回填缓存。
// 同 key 已有对象时先减旧段计数（覆盖写不泄漏旧段空间计数）。
func (s *pebbleStore) PutMapping(ctx context.Context, key string, m ObjectMeta) error {
	var old *ObjectMeta
	if om, ok := s.cache.get(key); ok {
		old = &om
	} else if v, found, err := s.get(keyMapping(key)); err != nil {
		return err
	} else if found {
		om, err := decodeObjectMeta(v)
		if err != nil {
			return err
		}
		old = &om
	}
	if err := s.segs.putObject(ctx, key, m, old); err != nil {
		return err
	}
	s.cache.put(key, m)
	return nil
}

// DeleteMapping 删除 mapping（WriteBatch 原子含段存活计数更新），并失效缓存条目。
// 计数归零的段转为 Reclaiming，由后台 GC 回收。
func (s *pebbleStore) DeleteMapping(ctx context.Context, key string) error {
	old, err := s.GetMapping(ctx, key)
	if err != nil {
		return err
	}
	if err := s.segs.delObject(ctx, key, old); err != nil {
		return err
	}
	s.cache.del(key)
	return nil
}

// RefSegment 记录一次段内读引用（读开始前调用）。
func (s *pebbleStore) RefSegment(segmentID int64) {
	s.segs.Ref(segmentID)
}

// UnrefSegment 释放一次段内读引用（读结束后调用）。
func (s *pebbleStore) UnrefSegment(segmentID int64) {
	s.segs.Unref(segmentID)
}

// SegmentStats 返回各状态段数量（管理/验证用）。
func (s *pebbleStore) SegmentStats() map[SegmentState]int {
	return s.segs.stats()
}

// LoadCache 预热加速缓存：全量扫描 mapping 命名空间回填，受同一 LRU 预算约束。
func (s *pebbleStore) LoadCache(ctx context.Context) error {
	return s.IterMapping(ctx, func(key string, m ObjectMeta) error {
		s.cache.put(key, m)
		return nil
	})
}

// IterMapping 以 mapping 前缀区间顺序遍历全部 key → ObjectMeta。
func (s *pebbleStore) IterMapping(ctx context.Context, fn func(key string, m ObjectMeta) error) error {
	prefix := []byte(kvPrefixMapping) // "m\x00"
	upper := append([]byte{}, prefix...)
	upper[len(upper)-1]++ // 前缀末字节 +1 → 独占上界，覆盖所有以 prefix 开头的键

	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upper})
	if err != nil {
		return err
	}
	defer it.Close()

	for it.First(); it.Valid(); it.Next() {
		m, err := decodeObjectMeta(it.Value())
		if err != nil {
			return err
		}
		if err := fn(string(it.Key()[len(prefix):]), m); err != nil {
			return err
		}
	}
	return it.Error()
}

// allocator 原子管理顺序写游标（curSeg/curOff）与游标持久化。持有独立锁，
// 使「申请偏移」成为廉价原子操作，真正的设备写由调用方在锁外执行，从而支持并发写不同偏移。
type allocator struct {
	mu     sync.Mutex
	db     *pebble.DB // 首次分配时从 pebbleStore 注入，persist 使用
	segs   *segmentManager
	curSeg int64
	curOff int64

	// 布局（启动时由设备容量计算注入）：segSize 单段大小、segCount 整盘段数。
	segSize  int64
	segCount int64

	cursorLoaded bool // 是否已从持久化游标恢复
}

func (a *allocator) persist(ctx context.Context, seg, off int64) error {
	return a.db.Set(keyState([]byte(kvCursorKey)), WriteCursor{SegmentID: seg, Offset: off}.encode(), syncWO)
}

// loadCursor 从持久化游标恢复写位置。游标不存在(首次)时保持 seg=0, off=0。
func (a *allocator) loadCursor(s *pebbleStore) error {
	v, found, err := s.get(keyState([]byte(kvCursorKey)))
	if err != nil || !found {
		return err
	}
	c, err := decodeWriteCursor(v)
	if err != nil {
		return err
	}
	a.curSeg, a.curOff = c.SegmentID, c.Offset
	return nil
}

// allocate 原子申请 layout.Align4k(size) 的连续空间，返回 (segmentID, 段内 4K 对齐偏移)；
// useReserve=true 时可滚动进入预留缓冲段（compaction 搬移专用）。
// 首次调用时懒加载持久化游标；段放不下则滚动到下一段并持久化新游标；分配后推进并持久化游标。
//
// 段滚动策略（保持磁盘顺序写）：
//   - 下一段未使用或处于 Free：顺序滚动（正常路径，行为与 v1 一致）；
//     用户路径滚动上限为 SegmentCount−reserveSegs（预留缓冲段不参与）；
//   - 下一段已被占用（Active/Full/Reclaiming）或游标到顶：从空闲池取 Free 段复用（游标回跳）；
//   - 空闲池为空：ErrNoSpace。
//
// 切换段时旧段标记 Full（不再写入），新段激活（Free→Active 或创建记录）。
func (s *pebbleStore) allocate(ctx context.Context, size int64, useReserve bool) (int64, int64, error) {
	a := s.alloc
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.db == nil {
		a.db = s.db
	}
	if !a.cursorLoaded {
		if err := a.loadCursor(s); err != nil {
			return 0, 0, err
		}
		a.cursorLoaded = true
	}

	aligned := layout.Align4k(size)
	if a.curOff+aligned > a.segSize {
		if a.curOff > 0 {
			if err := a.segs.markFull(ctx, a.curSeg); err != nil {
				return 0, 0, err
			}
		}
		upper := a.segCount
		if !useReserve {
			upper = a.segCount - reserveSegs
		}
		if a.curSeg+1 < upper && a.segs.canUse(a.curSeg+1) {
			a.curSeg++
			a.curOff = 0
			if err := a.segs.activate(ctx, a.curSeg); err != nil {
				return 0, 0, err
			}
			if err := a.persist(ctx, a.curSeg, a.curOff); err != nil {
				return 0, 0, err
			}
		} else {
			seg, ok := a.segs.popFree(ctx)
			if !ok {
				return 0, 0, ierr.ErrNoSpace
			}
			a.curSeg, a.curOff = seg, 0
			if err := a.persist(ctx, a.curSeg, a.curOff); err != nil {
				return 0, 0, err
			}
		}
	}

	seg, off := a.curSeg, a.curOff
	a.curOff += aligned
	if err := a.persist(ctx, seg, a.curOff); err != nil {
		return 0, 0, err
	}
	return seg, off, nil
}

// AllocateSegment 用户写路径分配（预留缓冲段不可用）。
func (s *pebbleStore) AllocateSegment(size int64) (int64, int64, error) {
	return s.allocate(context.Background(), size, false)
}

// AllocateSegmentReserve 搬移专用分配：可动用预留缓冲段，保证 compaction 有落点。
func (s *pebbleStore) AllocateSegmentReserve(size int64) (int64, int64, error) {
	return s.allocate(context.Background(), size, true)
}

func (s *pebbleStore) GetSegment(ctx context.Context, segmentID int64) (SegmentMeta, bool, error) {
	v, found, err := s.get(keyState(segmentKey(segmentID)))
	if err != nil || !found {
		return SegmentMeta{}, found, err
	}
	m, err := decodeSegmentMeta(v)
	return m, true, err
}

func (s *pebbleStore) PutSegment(ctx context.Context, segmentID int64, m SegmentMeta) error {
	return s.db.Set(keyState(segmentKey(segmentID)), m.encode(), syncWO)
}

// MarkCompacting 将 Full 段标记为搬移中（提供 Store 接口透传）。
func (s *pebbleStore) MarkCompacting(ctx context.Context, segmentID int64) error {
	return s.segs.markCompacting(ctx, segmentID)
}

// MoveMapping 条件写（CAS）搬移对象映射：以 pebble 为权威校验当前 mapping == old，
// 一致则原子切换为 new 并转移段存活计数（new 段 +1、old 段 −1，同一 WriteBatch），成功后回填缓存。
// 不一致或 key 不存在返回 ierr.ErrConflict，不修改任何数据（并发 Put/Delete 冲突由调用方跳过重试）。
func (s *pebbleStore) MoveMapping(ctx context.Context, key string, old, new ObjectMeta) error {
	v, found, err := s.get(keyMapping(key))
	if err != nil {
		return err
	}
	if !found {
		return ierr.ErrConflict
	}
	cur, err := decodeObjectMeta(v)
	if err != nil {
		return err
	}
	if cur != old {
		return ierr.ErrConflict
	}

	b := s.db.NewBatch()
	defer b.Close()
	s.segs.mu.Lock()
	s.segs.putObjectLocked(b, key, new, &old)
	s.segs.mu.Unlock()
	if err := s.db.Apply(b, syncWO); err != nil {
		return err
	}
	s.cache.put(key, new)
	return nil
}

// ListSegments 枚举内存引用表中的全部段状态（权限同 SegmentStats，admin 透传出服务端 RPC）。
func (s *pebbleStore) ListSegments(_ context.Context, fn func(id int64, m SegmentMeta) error) error {
	s.segs.mu.Lock()
	defer s.segs.mu.Unlock()
	for id, e := range s.segs.segs {
		if err := fn(id, e.meta); err != nil {
			return err
		}
	}
	return nil
}

// Cursor 返回当前顺序写游标。游标懒加载（首次分配时恢复），尚无写入时返回 (0, 0)。
func (s *pebbleStore) Cursor() (segmentID, offset int64) {
	a := s.alloc
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.curSeg, a.curOff
}

// UsedBytes 统计已用物理字节：Full/Reclaiming 段计整段大小，Active 段计已写偏移（curOff）。
// 锁序 allocator.mu → segmentManager.mu；此处取快照后释放 a.mu 再取 m.mu，无嵌套持有。
// 段数 ≤2048，心跳 1s 一次，遍历开销可忽略。
func (s *pebbleStore) UsedBytes() int64 {
	a := s.alloc
	a.mu.Lock()
	segSize := a.segSize
	curSeg, curOff := a.curSeg, a.curOff
	a.mu.Unlock()

	m := s.segs
	m.mu.Lock()
	defer m.mu.Unlock()
	var used int64
	for id, e := range m.segs {
		switch e.meta.State {
		case SegmentStateFull, SegmentStateReclaiming:
			used += segSize
		case SegmentStateActive:
			if id == curSeg {
				used += curOff
			} else {
				used += segSize
			}
		}
	}
	return used
}

// Close 停止后台 GC 并关闭 pebble DB。
func (s *pebbleStore) Close() error {
	if s.db == nil {
		return nil
	}
	if s.segs != nil {
		s.segs.stopGC()
	}
	err := s.db.Close()
	s.db = nil
	return err
}
