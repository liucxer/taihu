//go:build !linux

package aio

import "errors"

// probe 探测 io_uring 可用性。非 Linux 平台恒不支持，且结论是确定性的（可缓存）。
// 导出入口在 aio.go，结论缓存与转发在 probe_cache.go。
func probe() (Info, bool) {
	return Info{Reason: "io_uring 仅 Linux 支持", KernelRelease: kernelRelease()}, true
}

// kernelRelease 非 Linux 平台无内核版本可报。
func kernelRelease() string { return "n/a" }

// checkIOPoll 非 Linux 平台没有 io_uring，也就没有 IOPOLL。导出入口在 aio.go。
func checkIOPoll(string) error {
	return errors.New("aio: IOPOLL 仅 Linux 支持")
}

// backendName 返回队列实际生效的后端名（仅用于日志）。
func backendName(Ring) string { return "fallback" }

// ringQueueDepth 非 Linux 后端没有 io_uring 队列，无深度可报。
func ringQueueDepth(Ring) (uint32, uint32, bool) { return 0, 0, false }
