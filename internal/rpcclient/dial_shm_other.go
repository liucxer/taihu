//go:build !linux

// 共享内存（shmipc）客户端占位：shmipc-go 仅支持 Linux，非 Linux 平台报错。
// cmd 主文件无需 build tag，构建期自动选中本文件。
package rpcclient

import (
	"context"
	"errors"
)

var errShmUnsupported = errors.New("taihu: shmipc only supported on linux")

// DialShm 非 Linux 占位实现。
func DialShm(ctx context.Context, uds string) (*Storage, error) {
	return nil, errShmUnsupported
}

// DialShmPool 非 Linux 占位实现。
func DialShmPool(ctx context.Context, uds string, sessions int) (*Storage, error) {
	return nil, errShmUnsupported
}
