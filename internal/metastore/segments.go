package metastore

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
)

// segmentManager 管理 segment 生命周期（对象删除驱动的 GC 与复用）。
//
// 核心机制：
//   - 存活计数 AliveCount：每段现存对象数。内存维护（mapping 为真实源），
//     每次 Put/Delete 与 mapping 写入同一 pebble WriteBatch 原子持久化；
//     启动时全量扫描 mapping 重建（权威），兼顾历史数据一致性。
//   - 状态机：Free → Active（分配写入）→ Full（游标写出段尾）→ Reclaiming（对象删光）
//     → Free（GC 确认无在途读者后回收入池）。
//   - 读引用计数 refCount：防「回收后立即复用 → 迟到读读错数据」竞态；
//     仅在 refCount==0 时允许 Reclaiming→Free。
//   - 空闲池 free：FIFO，供 allocator 在游标到顶时取段复用（游标回跳，段内仍顺序写）。
//   - 后台 GC goroutine：周期扫描，把「无引用」的 Reclaiming 段转 Free 入池。
//
// 锁序：allocator.mu → segmentManager.mu（GC / PutMapping 路径只持 segmentManager.mu，无反向）。
type segmentManager struct {
	mu   sync.Mutex
	db   *pebble.DB
	segs map[int64]*segEntry
	free []int64 // 空闲段池（FIFO）

	stop chan struct{}
	wg   sync.WaitGroup
}

type segEntry struct {
	meta     SegmentMeta // State / AliveCount / ReclaimSeq（AliveCount 以内存为准）
	refCount int64       // 在途读引用计数
}

// gcInterval 后台 GC 扫描周期。
const gcInterval = time.Second

// newSegmentManager 构造 manager（不启动 GC goroutine）。
func newSegmentManager(db *pebble.DB) *segmentManager {
	return &segmentManager{db: db, segs: make(map[int64]*segEntry), stop: make(chan struct{})}
}

// rebuild 启动恢复：从 seg/ 前缀恢复段状态与空闲池，从 mapping 全量重建存活计数。
// 全量扫描成本为一次性（与 LoadCache 同量级），换来自洽的计数基准。
func (m *segmentManager) rebuild(ctx context.Context) error {
	// 1. 段状态：扫描 s\x00seg/ 前缀。
	prefix := keyState([]byte(kvSegmentPrefix))
	upper := append([]byte{}, prefix...)
	upper[len(upper)-1]++
	it, err := m.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upper})
	if err != nil {
		return err
	}
	for it.First(); it.Valid(); it.Next() {
		if len(it.Key()) != len(prefix)+8 {
			return fmt.Errorf("taihu: bad segment key %q", it.Key())
		}
		id := int64(binary.LittleEndian.Uint64(it.Key()[len(prefix):]))
		meta, err := decodeSegmentMeta(it.Value())
		if err != nil {
			it.Close()
			return err
		}
		// AliveCount 以 mapping 全量扫描为准（步骤 2），此处丢弃持久化计数避免双重累加。
		meta.AliveCount = 0
		e := &segEntry{meta: meta}
		if meta.State == SegmentStateFree {
			m.free = append(m.free, id)
		}
		m.segs[id] = e
	}
	it.Close()
	if err := it.Error(); err != nil {
		return err
	}

	// 2. 存活计数：全量扫描 mapping 重建（权威）。
	return m.rebuildMapping(ctx)
}

// rebuildMapping 遍历 mapping 重建各段存活计数。
func (m *segmentManager) rebuildMapping(ctx context.Context) error {
	return iterateMapping(m.db, func(key string, meta ObjectMeta) error {
		id := meta.SegmentID
		e := m.segs[id]
		if e == nil {
			e = &segEntry{meta: SegmentMeta{State: SegmentStateActive}}
			m.segs[id] = e
		}
		e.meta.AliveCount++
		if e.meta.State == SegmentStateFree {
			// 不一致：段标记 Free 但仍有对象引用 → 以 mapping 为准回退 Active。
			e.meta.State = SegmentStateActive
			m.removeFree(id)
		}
		return nil
	})
}

// removeFree 从空闲池移除指定段（free 池较小，顺序扫描可接受）。
func (m *segmentManager) removeFree(id int64) {
	for i, f := range m.free {
		if f == id {
			m.free = append(m.free[:i], m.free[i+1:]...)
			return
		}
	}
}

// run 启动后台 GC goroutine：周期把「无引用」的 Reclaiming 段回收为 Free。
func (m *segmentManager) run() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t := time.NewTicker(gcInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				m.reclaimOnce(context.Background())
			case <-m.stop:
				return
			}
		}
	}()
}

// stop 停止 GC goroutine。
func (m *segmentManager) stopGC() {
	close(m.stop)
	m.wg.Wait()
}

// ensureLocked 取段条目，不存在则创建（默认 Active）。调用方须持有 m.mu。
func (m *segmentManager) ensureLocked(id int64) *segEntry {
	e := m.segs[id]
	if e == nil {
		e = &segEntry{meta: SegmentMeta{State: SegmentStateActive}}
		m.segs[id] = e
	}
	return e
}

// persistLocked 持久化段状态。调用方须持有 m.mu。
func (m *segmentManager) persistLocked(ctx context.Context, id int64, e *segEntry) error {
	return m.db.Set(keyState(segmentKey(id)), e.meta.encode(), syncWO)
}

// putObject 写对象映射并原子更新段存活计数。
// old 为被覆盖的旧映射（同 key 已有对象时非 nil）：旧对象所在段计数 -1。
// mapping 写与两段 seg meta 写同 batch 原子提交。
func (m *segmentManager) putObject(ctx context.Context, key string, meta ObjectMeta, old *ObjectMeta) error {
	b := m.db.NewBatch()
	defer b.Close()
	b.Set(keyMapping(key), meta.encode(), nil)

	m.mu.Lock()
	e := m.ensureLocked(meta.SegmentID)
	oldSame := old != nil && old.SegmentID == meta.SegmentID
	if !oldSame {
		e.meta.AliveCount++
	}
	// Free/Reclaiming 段被写入新对象 → 重新激活（Reclaiming 从 GC 候选退出）。
	if e.meta.State == SegmentStateFree || e.meta.State == SegmentStateReclaiming {
		if e.meta.State == SegmentStateFree {
			m.removeFree(meta.SegmentID)
		}
		e.meta.State = SegmentStateActive
	}
	b.Set(keyState(segmentKey(meta.SegmentID)), e.meta.encode(), nil)
	if old != nil && !oldSame {
		if oe := m.segs[old.SegmentID]; oe != nil {
			oe.meta.AliveCount--
			if oe.meta.AliveCount <= 0 {
				oe.meta.AliveCount = 0
				oe.meta.State = SegmentStateReclaiming
			}
			b.Set(keyState(segmentKey(old.SegmentID)), oe.meta.encode(), nil)
		}
	}
	m.mu.Unlock()

	return m.db.Apply(b, syncWO)
}

// delObject 删除对象映射并原子更新段存活计数。
// 计数归零的段立即转 Reclaiming，等待 GC 确认无在途读者后回收。
func (m *segmentManager) delObject(ctx context.Context, key string, meta ObjectMeta) error {
	b := m.db.NewBatch()
	defer b.Close()
	b.Delete(keyMapping(key), nil)

	m.mu.Lock()
	if e := m.segs[meta.SegmentID]; e != nil {
		e.meta.AliveCount--
		if e.meta.AliveCount <= 0 {
			e.meta.AliveCount = 0
			e.meta.State = SegmentStateReclaiming
		}
		b.Set(keyState(segmentKey(meta.SegmentID)), e.meta.encode(), nil)
	}
	m.mu.Unlock()

	return m.db.Apply(b, syncWO)
}

// Ref 记录一次段内读引用（读开始前调用）。
func (m *segmentManager) Ref(segmentID int64) {
	m.mu.Lock()
	if e := m.segs[segmentID]; e != nil {
		e.refCount++
	}
	m.mu.Unlock()
}

// Unref 释放一次段内读引用（读结束后调用）。
func (m *segmentManager) Unref(segmentID int64) {
	m.mu.Lock()
	if e := m.segs[segmentID]; e != nil && e.refCount > 0 {
		e.refCount--
	}
	m.mu.Unlock()
}

// canUse 判断段是否可被顺序分配器选中：无记录（从未使用）或处于 Free。
func (m *segmentManager) canUse(segmentID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.segs[segmentID]
	if e == nil {
		return true
	}
	return e.meta.State == SegmentStateFree
}

// activate 将段置为 Active 并持久化（顺序滚动选段时调用）。
func (m *segmentManager) activate(ctx context.Context, segmentID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.ensureLocked(segmentID)
	if e.meta.State == SegmentStateFree {
		m.removeFree(segmentID)
	}
	e.meta.State = SegmentStateActive
	return m.persistLocked(ctx, segmentID, e)
}

// markFull 将段置为 Full（游标写出段尾、不再写入）。
func (m *segmentManager) markFull(ctx context.Context, segmentID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.segs[segmentID]
	if e == nil {
		return nil // 段从未写入（无数据），无需标记
	}
	e.meta.State = SegmentStateFull
	return m.persistLocked(ctx, segmentID, e)
}

// popFree 从空闲池取一段供复用：Free → Active 并持久化。
// 分配器（游标到顶）调用；无空闲段返回 ok=false。
func (m *segmentManager) popFree(ctx context.Context) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.free) == 0 {
		return 0, false
	}
	id := m.free[0]
	m.free = m.free[1:]
	e := m.segs[id]
	e.meta.State = SegmentStateActive
	if err := m.persistLocked(ctx, id, e); err != nil {
		// 持久化失败：回滚（段仍保持可复用状态）。
		e.meta.State = SegmentStateFree
		m.free = append([]int64{id}, m.free...)
		return 0, false
	}
	return id, true
}

// reclaimOnce 执行一轮回收：所有 AliveCount==0 且无在途读者的 Reclaiming 段 → Free 入池。
// 返回本次回收段数。
func (m *segmentManager) reclaimOnce(ctx context.Context) int {
	m.mu.Lock()
	var cands []*segEntry
	var ids []int64
	for id, e := range m.segs {
		if e.meta.State == SegmentStateReclaiming && e.refCount == 0 {
			cands = append(cands, e)
			ids = append(ids, id)
		}
	}
	if len(cands) == 0 {
		m.mu.Unlock()
		return 0
	}
	var keys [][]byte
	for i, id := range ids {
		e := cands[i]
		e.meta.State = SegmentStateFree
		e.meta.AliveCount = 0
		e.meta.ReclaimSeq++ // 回收一代，世代号递增
		m.free = append(m.free, id)
		keys = append(keys, keyState(segmentKey(id)))
	}
	m.mu.Unlock()

	b := m.db.NewBatch()
	defer b.Close()
	for i, e := range cands {
		b.Set(keys[i], e.meta.encode(), nil)
	}
	if err := m.db.Apply(b, syncWO); err != nil {
		// 持久化失败：段留在 Free（内存态），下轮 GC 重试写入。
		return 0
	}
	return len(ids)
}

// stats 返回段状态汇总（管理/测试用）。
func (m *segmentManager) stats() map[SegmentState]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[SegmentState]int)
	for _, e := range m.segs {
		out[e.meta.State]++
	}
	return out
}

// iterateMapping 遍历 mapping 命名空间（供 rebuild 复用，避免与 Store 接口耦合）。
func iterateMapping(db *pebble.DB, fn func(key string, meta ObjectMeta) error) error {
	prefix := []byte(kvPrefixMapping)
	upper := append([]byte{}, prefix...)
	upper[len(upper)-1]++
	it, err := db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upper})
	if err != nil {
		return err
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		meta, err := decodeObjectMeta(it.Value())
		if err != nil {
			return err
		}
		if err := fn(string(it.Key()[len(prefix):]), meta); err != nil {
			return err
		}
	}
	return it.Error()
}
