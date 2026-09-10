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

// Open 打开 pebble DB。目录不存在时自动创建。
// 写位置游标由内部的 allocator 在首次 AllocateSegment 时懒加载恢复，无需在启动时读取。
// segmentManager 在此重建段状态并启动后台 GC goroutine。
func Open(dir string) (Store, error) {
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		return nil, fmt.Errorf("taihu: open pebble %q: %w", dir, err)
	}
	s := &pebbleStore{db: db, alloc: &allocator{}, cache: &metaCache{}}
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

// AllocateSegment 原子申请 layout.Align4k(size) 的连续空间，返回 (segmentID, 段内 4K 对齐偏移)。
// 首次调用时懒加载持久化游标；段放不下则滚动到下一段并持久化新游标；分配后推进并持久化游标。
//
// 段滚动策略（保持磁盘顺序写）：
//   - 下一段未使用或处于 Free：顺序滚动（正常路径，行为与 v1 一致）；
//   - 下一段已被占用（Active/Full/Reclaiming）或游标到顶：从空闲池取 Free 段复用（游标回跳）；
//   - 空闲池为空：ErrNoSpace。
// 切换段时旧段标记 Full（不再写入），新段激活（Free→Active 或创建记录）。
func (s *pebbleStore) AllocateSegment(size int64) (int64, int64, error) {
	ctx := context.Background()
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
	if a.curOff+aligned > layout.SegmentSizeBytes {
		if a.curOff > 0 {
			if err := a.segs.markFull(ctx, a.curSeg); err != nil {
				return 0, 0, err
			}
		}
		if a.curSeg+1 < layout.SegmentCount && a.segs.canUse(a.curSeg+1) {
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