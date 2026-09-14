//go:build !linux

// 共享内存零拷贝写（非 linux 占位）：shmipc 仅 linux 支持，直接返回 ErrShmOnly。
package rpcclient

import "context"

func newShmPut(ctx context.Context, s *Storage, key string, size int64) (*PutWriter, error) {
	return nil, ErrShmOnly
}
