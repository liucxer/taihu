//go:build linux

// bench single 的 shm 数据面拨号（仅 Linux）：shmipc-go 只支持 Linux，
// 非 Linux 平台的同名函数见 bench_single_shm_other.go。
package cmd

import (
	"github.com/liucxer/taihu/internal/benchkit"
	"github.com/liucxer/taihu/internal/transport"
)

// dialShmBenchStore 以 ShmConn 直接作为 bench Store（绕过 rpcclient.Storage，
// 保留 benchkit.Batcher 批量读能力）。
func dialShmBenchStore(uds string, sessions int) (benchkit.Store, error) {
	return transport.DialShm(uds, sessions)
}
