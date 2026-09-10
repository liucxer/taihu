// Package metastore 抽象元数据持久化（本实现基于 CockroachDB pebble，见 kv_pebble.go）。
// 两个逻辑命名空间：mapping（key → ObjectMeta）、state（cursor / seg/<id>）。
package metastore

import "context"

// Store 是元数据存储接口，供 Storage 层调用。
type Store interface {
	GetMapping(ctx context.Context, key string) (ObjectMeta, error) // 不存在返回 ErrNotFound
	PutMapping(ctx context.Context, key string, m ObjectMeta) error
	DeleteMapping(ctx context.Context, key string) error
	IterMapping(ctx context.Context, fn func(key string, m ObjectMeta) error) error // 遍历全部分映射

	// LoadCache 预热加速缓存：全量扫描 mapping 命名空间载入内存缓存，消除后续 GetMapping 回查。
	LoadCache(ctx context.Context) error

	// AllocateSegment 原子申请一段连续空间，返回 (segmentID, 段内偏移)。内部管理写游标，
	// 段写满自动滚动，返回的偏移恒 4K 对齐、单调不重叠，可并发调用。
	AllocateSegment(size int64) (segmentID int64, offset int64, err error)

	// GetSegment 读取 segment 状态，gc 使用。
	GetSegment(ctx context.Context, segmentID int64) (SegmentMeta, bool, error)
	// PutSegment 写 segment 状态，gc 使用。
	PutSegment(ctx context.Context, segmentID int64, m SegmentMeta) error

	// RefSegment 记录一次段内读引用（读开始前调用）；UnrefSegment 读结束后调用。
	// 配合 GC：仅在引用归零时允许 Reclaiming 段回收复用，防止迟到读读错数据。
	RefSegment(segmentID int64)
	UnrefSegment(segmentID int64)

	// SegmentStats 返回各状态段数量（管理/验证用）。
	SegmentStats() map[SegmentState]int

	Close() error
}