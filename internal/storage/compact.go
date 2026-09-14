package taihu

import (
	"context"
	"fmt"
	"io"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/ierr"
	"github.com/liucxer/taihu/internal/metastore"
)

// CompactorConfig 后台段压缩（compaction）配置。
// 对应设计文档《segment 级 Compaction（数据迁移）设计方案》。
type CompactorConfig struct {
	Interval        time.Duration // 扫描周期
	HoleThreshold   float64       // 单段空洞率阈值：≥ 该值的 Full 段入候选
	ForceWatermark  float64       // 全局水位：已用段占比 ≥ 该值强制压缩（不等单段阈值）
	MaxMovePerRound int           // 每轮搬移对象数上限（限速，防冲击稳态带宽）
}

// DefaultCompactorConfig 返回默认配置：60s 低频扫描、空洞 80% 或段水位 80% 触发、每轮 ≤512 对象。
func DefaultCompactorConfig() CompactorConfig {
	return CompactorConfig{
		Interval:        time.Minute,
		HoleThreshold:   0.8,
		ForceWatermark:  0.8,
		MaxMovePerRound: 512,
	}
}

// Compactor 后台段压缩器：把高空洞 Full 段（含中断未完成的 Compacting 段）的存活对象
// 搬移到新位置（可动用预留缓冲段），搬空后旧段计数归零自动转 Reclaiming，
// 由现有后台 GC 回收入池复用。与 GC 为两条独立惰性链，互不冲突。
type Compactor struct {
	st   *Storage
	cfg  CompactorConfig
	stop chan struct{}
	wg   sync.WaitGroup
}

// NewCompactor 构造压缩器（不启动；调用 Start/Stop 管理生命周期）。
func NewCompactor(st *Storage, cfg CompactorConfig) *Compactor {
	return &Compactor{st: st, cfg: cfg, stop: make(chan struct{})}
}

// Start 启动后台压缩循环。
func (c *Compactor) Start() {
	c.wg.Add(1)
	go c.run()
}

// Stop 停止后台压缩循环并等待退出。
func (c *Compactor) Stop() {
	close(c.stop)
	c.wg.Wait()
}

func (c *Compactor) run() {
	defer c.wg.Done()
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			moved, err := c.compactOnce(context.Background())
			if moved > 0 || err != nil {
				log.Printf("compaction: moved=%d err=%v", moved, err)
			}
		case <-c.stop:
			return
		}
	}
}

// compactOnce 执行一轮压缩：全扫选候选段（空洞率降序）→ 逐个对象搬移（限额内）。
// 返回本轮搬移对象数。可并发调用（同一 Go 进程内唯一定时器驱动）。
func (c *Compactor) compactOnce(ctx context.Context) (int, error) {
	// 1. 全扫 mapping 聚合按段存活对象（一趟同时得到存活字节与对象清单）。
	type segAgg struct {
		keys       []string
		aliveBytes int64
	}
	aggs := make(map[int64]*segAgg)
	if err := c.st.db.IterMapping(ctx, func(key string, m metastore.ObjectMeta) error {
		a := aggs[m.SegmentID]
		if a == nil {
			a = &segAgg{}
			aggs[m.SegmentID] = a
		}
		a.keys = append(a.keys, key)
		a.aliveBytes += m.Size
		return nil
	}); err != nil {
		return 0, err
	}

	force := float64(len(aggs))/float64(c.st.layout.SegmentCount) >= c.cfg.ForceWatermark

	// 2. 候选：Full（或上次中断的 Compacting）且空洞率达标（或强制压缩）。
	type cand struct {
		id        int64
		holeRatio float64
		keys      []string
	}
	var cands []cand
	for id, a := range aggs {
		sm, found, err := c.st.db.GetSegment(ctx, id)
		if err != nil {
			return 0, err
		}
		if !found || (sm.State != metastore.SegmentStateFull && sm.State != metastore.SegmentStateCompacting) {
			continue
		}
		hole := 1 - float64(a.aliveBytes)/float64(c.st.layout.SegmentSizeBytes)
		if hole >= c.cfg.HoleThreshold || force {
			cands = append(cands, cand{id: id, holeRatio: hole, keys: a.keys})
		}
	}
	// 空洞率降序（空洞率最大的段先搬，单位搬移释放空间最多），同率按段号升序（确定性）。
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].holeRatio != cands[j].holeRatio {
			return cands[i].holeRatio > cands[j].holeRatio
		}
		return cands[i].id < cands[j].id
	})

	// 3. 搬移（限额内，限额跨段合并记账）。
	moved := 0
	for _, cd := range cands {
		if moved >= c.cfg.MaxMovePerRound {
			break
		}
		sm, found, err := c.st.db.GetSegment(ctx, cd.id)
		if err != nil {
			return moved, err
		}
		if !found {
			continue
		}
		if sm.State == metastore.SegmentStateFull {
			if err := c.st.db.MarkCompacting(ctx, cd.id); err != nil {
				return moved, err
			}
		}
		for _, key := range cd.keys {
			n, err := c.moveObject(ctx, key)
			if err == ierr.ErrConflict {
				continue // 并发 Put/Delete 改动该 key，跳过（下轮重扫）
			}
			if err != nil {
				return moved, err
			}
			moved += n
			if moved >= c.cfg.MaxMovePerRound {
				break
			}
		}
	}
	if moved > 0 {
		log.Printf("compaction: round done, moved %d objects", moved)
	}
	return moved, nil
}

// moveObject 搬移单个对象：读旧位置（自带 Ref/Unref 防回收竞态）→ 分配新位置 →
// 写设备（先数据后元数据）→ CAS 切映射（MoveMapping，附带新段 +1/旧段 −1 原子计数）。
// 返回 1=搬移成功、0=跳过（对象已不存在）；ErrConflict=被并发改动。
func (c *Compactor) moveObject(ctx context.Context, key string) (int, error) {
	meta, err := c.st.db.GetMapping(ctx, key)
	if err != nil {
		if err == ierr.ErrNotFound {
			return 0, nil // 已被并发删除
		}
		return 0, err
	}

	buf, err := c.st.ReadAt(ctx, key, 0, meta.Size)
	if err != nil && err != io.EOF {
		return 0, err
	}
	if buf == nil {
		return 0, nil
	}
	defer bufpool.Put(buf)
	if int64(len(buf)) < meta.Size {
		return 0, fmt.Errorf("compact: short read %s: got %d, want %d", key, len(buf), meta.Size)
	}

	// 分配新落点（可动用预留缓冲段；源段为 Full/Compacting，分配器不会选中它）。
	seg, off, err := c.st.db.AllocateSegmentReserve(meta.Size)
	if err != nil {
		return 0, err
	}
	if err := c.st.dev.Append(ctx, seg, off, meta.Size, buf); err != nil {
		return 0, err
	}
	if err := c.st.db.MoveMapping(ctx, key, meta, metastore.ObjectMeta{SegmentID: seg, Offset: off, Size: meta.Size}); err != nil {
		return 0, err // ErrConflict → 跳过该对象（数据未丢，下轮重扫）
	}
	return 1, nil
}
