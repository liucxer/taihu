//go:build !linux

package aio

import "errors"

// Info 描述 io_uring 可用性探测结果。
type Info struct {
	Supported     bool   // 当前内核是否可用 io_uring
	Reason        string // 人类可读原因（供日志/错误信息）
	KernelRelease string // 内核版本字符串，仅供日志
	SQEntries     uint32 // 内核回填的提交队列深度
	CQEntries     uint32 // 内核回填的完成队列深度
	Features      uint32 // 内核能力位（IORING_FEAT_*）
}

// Probe 探测 io_uring 可用性。非 Linux 平台恒不支持。
func Probe() Info {
	return Info{Reason: "io_uring 仅 Linux 支持"}
}

// kernelRelease 非 Linux 平台无内核版本可报。
func kernelRelease() string { return "n/a" }

// CheckIOPoll 非 Linux 平台没有 io_uring，也就没有 IOPOLL。
func CheckIOPoll(string) error {
	return errors.New("aio: IOPOLL 仅 Linux 支持")
}

// backendName 返回队列实际生效的后端名（仅用于日志）。
func backendName(Ring) string { return "fallback" }

// ringQueueDepth 非 Linux 后端没有 io_uring 队列，无深度可报。
func ringQueueDepth(Ring) (uint32, uint32, bool) { return 0, 0, false }
