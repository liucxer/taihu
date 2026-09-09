package taihu

import "context"

// store 抽象了元数据持久化（本实现基于 CockroachDB pebble，见 kv_pebble.go）。
// 两个逻辑命名空间：mapping（key → ObjectMeta）、state（cursor / seg/<id>）。
type store interface {
	GetMapping(ctx context.Context, key string) (ObjectMeta, error) // 不存在返回 ErrNotFound
	PutMapping(ctx context.Context, key string, m ObjectMeta) error
	DeleteMapping(ctx context.Context, key string) error
	IterMapping(ctx context.Context, fn func(key string, m ObjectMeta) error) error // 遍历全部分映射

	GetCursor(ctx context.Context) (WriteCursor, bool, error)
	PutCursor(ctx context.Context, c WriteCursor) error

	GetSegment(ctx context.Context, segmentID int64) (SegmentMeta, bool, error)
	PutSegment(ctx context.Context, segmentID int64, m SegmentMeta) error

	Close() error
}