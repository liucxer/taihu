// Package metastore 抽象元数据持久化（本实现基于 CockroachDB pebble，见 kv_pebble.go）。
// 两个逻辑命名空间：mapping（key → ObjectMeta）、state（cursor / seg/<id>）。
//
// 本文件是包内**唯一导出文件**：全部 public 顶层声明（Store 接口、值类型与状态常量、
// Open 等）集中于此；其余文件（model.go / kv_pebble.go / segments.go /
// cache.go）只保留私有实现与内部辅助。新增对外符号一律收敛到本文件，避免导出面散落。
package metastore

import (
	"context"
	"fmt"

	"github.com/cockroachdb/pebble"

	"github.com/liucxer/taihu/internal/layout"
)

// PutMappingItem 批量写映射的一个条目。
type PutMappingItem struct {
	Key  string
	Meta ObjectMeta
}

// AllocResult 批量分配的一个结果。
type AllocResult struct {
	SegmentID int64
	Offset    int64
}

// ObjectMeta 存放在 mapping 列族：key = 用户 key。
// SegmentID 所在 segment；Offset 段内起始偏移（恒 4K 对齐）；Size 逻辑大小（不含 4K 填充）。
// value 编码见 model.go（本层磁盘格式的唯一事实源，字段与 encode/decode 一一对应）。
type ObjectMeta struct {
	SegmentID int64
	Offset    int64
	Size      int64
}

// SegmentState 描述单个 segment 的生命周期状态。
type SegmentState uint8

const (
	SegmentStateFree       SegmentState = iota // 空闲，可分配
	SegmentStateActive                         // 正在顺序写入
	SegmentStateFull                           // 已写满，仅读
	SegmentStateReclaiming                     // 待回收（计数归零）
	SegmentStateCompacting                     // 搬移中（高空洞段存活对象搬迁，禁止分配/回收）
)

// SegmentMeta 存放在 state 列族，key = "seg/<segmentID>"。
// 用于 segment 生命周期管理 / GC（v1 预留字段，物理回收后续实现）。
// value 编码见 model.go。
type SegmentMeta struct {
	State      SegmentState
	AliveCount int64
	ReclaimSeq int64
}

// SegmentEntry 单个 segment 的状态明细（领域态，唯一一份定义）。
//
// 为什么在这里：本包已拥有 segment 的状态机（SegmentState）与 SegmentMeta，
// 段明细只是把 SegmentMeta 加上段号后摊平，归属本包最自然。
//
// 编解码另有一份 wire 结构：internal/transport/protocol.SegmentEntry，字段相同
// 但 State 是 uint8。**刻意不让 protocol import 本包** —— 本包依赖
// github.com/cockroachdb/pebble，而 protocol 是一个只依赖 encoding/binary 的
// 纯 codec（带表驱动单测），把持久化模型拖进编解码层不划算；领域态与 wire 态
// 本就是两个东西，转换发生在服务端边界（internal/transport/server_admin.go）
// 是合理的。两份结构的字段平齐性由 protocol_test.go 的 TestSegmentWireParity 守着。
type SegmentEntry struct {
	SegmentID  int64
	State      SegmentState
	AliveCount int64
	ReclaimSeq int64
}

// SegmentSummary 实例段汇总与写游标（领域态，唯一一份定义）。
type SegmentSummary struct {
	Total       int64
	Free        int64
	Active      int64
	Full        int64
	Reclaiming  int64
	CursorSeg   int64
	CursorOff   int64
	SegSize     int64
	ObjectCount int64
}

// Store 是元数据存储接口，供 Storage 层调用。
type Store interface {
	GetMapping(ctx context.Context, key string) (ObjectMeta, error) // 不存在返回 ErrNotFound
	PutMapping(ctx context.Context, key string, m ObjectMeta) error
	DeleteMapping(ctx context.Context, key string) error
	IterMapping(ctx context.Context, fn func(key string, m ObjectMeta) error) error // 遍历全部分映射

	// BatchGetMapping 一次读取多个 key 的对象映射（等价于多次 GetMapping，
	// 但未命中走单快照 + 单迭代有序 Seek，摊薄 N 次独立 pebble Get）。
	// 结果按入参 key 顺序返回；任一 key 缺失整体返回 ierr.ErrNotFound。
	BatchGetMapping(ctx context.Context, keys []string) ([]ObjectMeta, error)

	// BatchPutMapping 批量写对象映射：单个 Pebble Batch 原子提交（含段存活计数
	// 批量更新）。覆盖写只在缓存命中时减旧段计数（对象存储以写一次为主）。
	BatchPutMapping(ctx context.Context, items []PutMappingItem) error

	// BatchDeleteMapping 批量删对象映射：单个 Pebble Batch 原子提交（含段存活计数
	// 批量减量）。返回 per-key 错误（缺失 key 为 ierr.ErrNotFound，其余为 nil）；
	// 整体存储错误经第二个返回值暴露。
	BatchDeleteMapping(ctx context.Context, keys []string) ([]error, error)

	// LoadCache 预热加速缓存：全量扫描 mapping 命名空间载入内存缓存，消除后续 GetMapping 回查。
	LoadCache(ctx context.Context) error

	// AllocateSegment 原子申请一段连续空间，返回 (segmentID, 段内偏移)。内部管理写游标，
	// 段写满自动滚动，返回的偏移恒 4K 对齐、单调不重叠，可并发调用。
	// 用户写路径：滚动上限保留 ≥reserveSegs 个缓冲段（见 kv_pebble.go），不触碰预留区。
	AllocateSegment(size int64) (segmentID int64, offset int64, err error)
	// AllocateSegmentBatch 批量申请连续空间：与多次 AllocateSegment 语义等价，
	// 但游标持久化合并为一次（摊薄 N 次 sync 写），适合写流水线批量分配。
	AllocateSegmentBatch(sizes []int64) ([]AllocResult, error)
	// AllocateSegmentReserve 搬移专用分配：可动用预留缓冲段（用户路径不可用），
	// 保证 compaction 在用户写入占满时仍有落点，避免搬移死锁。
	AllocateSegmentReserve(size int64) (segmentID int64, offset int64, err error)

	// GetSegment 读取 segment 状态，gc 使用。
	GetSegment(ctx context.Context, segmentID int64) (SegmentMeta, bool, error)
	// PutSegment 写 segment 状态，gc 使用。
	PutSegment(ctx context.Context, segmentID int64, m SegmentMeta) error

	// MarkCompacting 将 Full 段标记为 Compacting（搬移中）并持久化；
	// 段不存在或非 Full 时为空操作（返回 nil）。
	MarkCompacting(ctx context.Context, segmentID int64) error

	// MoveMapping 条件写（CAS）搬移对象映射：仅当当前 mapping 与 old 完全一致时，
	// 原子地改为 new 并转移段存活计数（new 段 +1、old 段 −1，同一 WriteBatch）。
	// 不一致（key 不存在/已被并发 Put/Delete 改变）返回 ierr.ErrConflict，不修改任何数据。
	MoveMapping(ctx context.Context, key string, old, new ObjectMeta) error

	// ListSegments 枚举全部段状态（含内存引用表），admin/诊断用。
	ListSegments(ctx context.Context, fn func(id int64, m SegmentMeta) error) error
	// Cursor 返回当前顺序写游标（segmentID, 段内偏移）。尚无写入时返回 (0, 0)。
	Cursor() (segmentID, offset int64)

	// RefSegment 记录一次段内读引用（读开始前调用）；UnrefSegment 读结束后调用。
	// 配合 GC：仅在引用归零时允许 Reclaiming 段回收复用，防止迟到读读错数据。
	RefSegment(segmentID int64)
	UnrefSegment(segmentID int64)

	// SegmentStats 返回各状态段数量（管理/验证用）。
	SegmentStats() map[SegmentState]int

	// UsedBytes 统计已用物理字节（Full/Reclaiming 段计整段、Active 段计已写偏移），
	// 供容量上报 Available = Capacity - Used。
	UsedBytes() int64

	Close() error
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
