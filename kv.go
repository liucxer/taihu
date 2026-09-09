package taihu

import "context"

// store 抽象了元数据持久化（本实现基于 CockroachDB pebble，见 kv_pebble.go）。
// 两个逻辑命名空间：mapping（key → ObjectMeta）、state（cursor / seg/<id>）。
type store interface {
	GetMapping(ctx context.Context, key string) (ObjectMeta, error) // 不存在返回 ErrNotFound
	PutMapping(ctx context.Context, key string, m ObjectMeta) error
	DeleteMapping(ctx context.Context, key string) error
	IterMapping(ctx context.Context, fn func(key string, m ObjectMeta) error) error // 遍历全部分映射

	// LoadCache 预热加速缓存：全量扫描 mapping 命名空间载入内存缓存，消除后续 GetMapping 回查。
	LoadCache(ctx context.Context) error

	// AllocateSegment 原子申请一段连续空间，返回 (segmentID, 段内偏移)。内部管理写游标，
	// 段写满自动滚动，返回的偏移恒 4K 对齐、单调不重叠，可并发调用。
	AllocateSegment(size int64) (segmentID int64, offset int64, err error)

	GetSegment(ctx context.Context, segmentID int64) (SegmentMeta, bool, error)
	PutSegment(ctx context.Context, segmentID int64, m SegmentMeta) error

	Close() error
}