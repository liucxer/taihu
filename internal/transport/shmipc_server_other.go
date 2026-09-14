//go:build !linux

// 共享内存 IPC 服务端占位实现：shmipc-go 仅支持 Linux（官方约束），
// 非 Linux 平台返回"仅支持 linux"错误，保证 cmd 主文件无需 build tag。
package transport

import (
	"errors"
	"io"

	"github.com/liucxer/taihu/pkg/taihu"
)

// errShmUnsupported shmipc 仅支持 Linux。
var errShmUnsupported = errors.New("taihu: shmipc only supported on linux")

// ServeShm 非 Linux 平台的占位实现。
func ServeShm(storage *taihu.Storage, uds string) (io.Closer, error) {
	return nil, errShmUnsupported
}

// ServeShmWithBatch 非 Linux 平台的占位实现。
func ServeShmWithBatch(storage *taihu.Storage, uds string, batchTarget, batchWorkers int) (io.Closer, error) {
	return nil, errShmUnsupported
}
