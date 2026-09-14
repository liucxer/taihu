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

// ShmSupported 报告本平台是否支持 shmipc 共享内存 IPC：非 Linux 恒为 false。
// 调用方（server 启动）据此跳过 shm 服务而非启动失败 —— 本平台 TCP 数据面
// 仍然完整可用，只是少了同机零拷贝那条路径。
func ShmSupported() bool { return false }

// ServeShm 非 Linux 平台的占位实现。
func ServeShm(storage *taihu.Storage, uds string) (io.Closer, error) {
	return nil, errShmUnsupported
}

// ServeShmWithBatch 非 Linux 平台的占位实现。
func ServeShmWithBatch(storage *taihu.Storage, uds string, batchTarget, batchWorkers int) (io.Closer, error) {
	return nil, errShmUnsupported
}
