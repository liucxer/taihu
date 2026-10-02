//go:build !linux

// bench single 的 shm 数据面拨号占位：shmipc-go 仅支持 Linux，
// 非 Linux 平台返回 ErrShmUnsupported，保证 bench_single.go 主文件无需 build tag。
package cmd

import (
	"github.com/liucxer/taihu/internal/benchkit"
	"github.com/liucxer/taihu/pkg/ierr"
)

// dialShmBenchStore 非 Linux 平台的占位实现。
func dialShmBenchStore(uds string, sessions int) (benchkit.Store, error) {
	return nil, ierr.ErrShmUnsupported
}
