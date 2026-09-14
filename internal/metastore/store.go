// Package metastore 抽象元数据持久化（本实现基于 CockroachDB pebble，见 kv_pebble.go）。
// 两个逻辑命名空间：mapping（key → ObjectMeta）、state（cursor / seg/<id>）。
package metastore

import "context"

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
