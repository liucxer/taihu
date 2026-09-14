package aio

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// uringProbeHeaderLen 是 struct io_uring_probe 的头部长度：
//
//	struct io_uring_probe { __u8 last_op; __u8 ops_len; __u16 resv; __u32 resv2[3]; };
//
// 其后紧跟 ops_len 个 struct io_uring_probe_op（各 8 字节）。
const uringProbeHeaderLen = 16

// Info 描述 io_uring 可用性探测结果。
type Info struct {
	Supported     bool   // 当前内核是否可用 io_uring
	Reason        string // 人类可读原因（供日志与错误信息）
	KernelRelease string // 内核版本字符串，仅供日志
	SQEntries     uint32 // 内核回填的提交队列深度
	CQEntries     uint32 // 内核回填的完成队列深度
	Features      uint32 // 内核能力位（IORING_FEAT_*）
}

var (
	probeMu   sync.Mutex
	probeInfo Info
	probeDone bool
)

// Probe 探测当前内核是否可用 io_uring。
//
// 判定完全基于 io_uring_setup 的 errno，不比较内核版本号 —— 版本号反映不了三类
// 误判：RHEL 系的 io_uring_disabled sysctl、容器 seccomp 拦截、以及发行版把 io_uring
// 反向移植进老内核（例如 openEuler/BCLinux 4.19.90 就带完整 backport，能力集约等于 5.8）。
//
// 只缓存确定性结论：资源类瞬时错误（ENOMEM/EMFILE 等）下次调用会重新探测，
// 否则一次偶发失败会把进程永久钉死在 libaio 上。
func Probe() Info {
	probeMu.Lock()
	defer probeMu.Unlock()
	if probeDone {
		return probeInfo
	}
	info, deterministic := probe()
	if info.Supported || deterministic {
		probeInfo, probeDone = info, true
	}
	return info
}

// probe 实际执行一次探测，第二个返回值表示结论是否确定性（可否缓存）。
func probe() (Info, bool) {
	info := Info{KernelRelease: kernelRelease()}

	// 建一个最小 ring 试跑：成功后立刻关闭，不长期占用 fd 与 memlock 记账
	// （io_uring_setup 会按 RLIMIT_MEMLOCK 记账）。
	fd, params, err := uringSetupRaw(64, 0)
	if err != nil {
		info.Reason = describeUringErr(err)
		return info, !transientErrno(err)
	}
	defer func() { _ = unix.Close(fd) }()

	if ok, why := uringSupportsRW(fd); !ok {
		info.Reason = why
		return info, true
	}

	info.Supported = true
	info.Reason = "ok"
	info.SQEntries = params.SQEntries
	info.CQEntries = params.CQEntries
	info.Features = params.Features
	return info, true
}

// uringSupportsRW 用 IORING_REGISTER_PROBE 确认内核确实实现了 IORING_OP_READ/WRITE。
// 5.1~5.5 只有 READV/WRITEV，老内核的 backport 也可能裁剪。探测本身不可用时
// （老内核没有 REGISTER_PROBE）不作判断，以免把可用内核误判为不支持。
func uringSupportsRW(fd int) (bool, string) {
	const maxOps = 256
	buf := make([]byte, uringProbeHeaderLen+maxOps*8)
	_, _, errno := unix.Syscall6(unix.SYS_IO_URING_REGISTER, uintptr(fd),
		uintptr(ioringRegisterProbe), uintptr(unsafe.Pointer(&buf[0])), maxOps, 0, 0)
	if errno != 0 {
		return true, ""
	}

	opsLen := int(buf[1])
	if opsLen > maxOps {
		opsLen = maxOps
	}
	var haveRead, haveWrite bool
	for i := 0; i < opsLen; i++ {
		switch buf[uringProbeHeaderLen+i*8] {
		case ioringOpRead:
			haveRead = true
		case ioringOpWrite:
			haveWrite = true
		}
	}
	if !haveRead || !haveWrite {
		return false, "内核未实现 IORING_OP_READ/WRITE（仅 READV/WRITEV 的旧版本）"
	}
	return true, ""
}

// transientErrno 判断 errno 是否为瞬时失败：这类失败不应被缓存成「不支持」。
func transientErrno(err error) bool {
	switch err {
	case unix.ENOMEM, unix.EMFILE, unix.ENFILE, unix.EINTR, unix.EBUSY, unix.EAGAIN:
		return true
	}
	return false
}

// describeUringErr 把 io_uring_setup 的 errno 翻译成可读原因。
func describeUringErr(err error) string {
	switch err {
	case unix.ENOSYS:
		return "内核未提供 io_uring 系统调用（需 5.1+ 且 CONFIG_IO_URING=y）"
	case unix.EPERM, unix.EACCES:
		return "io_uring 被策略禁用（seccomp、io_uring_disabled sysctl 或容器限制），errno=" + err.Error()
	case unix.EINVAL:
		return "io_uring_setup 参数被拒绝（内核过旧或 backport 能力不足），errno=EINVAL"
	case unix.EMFILE, unix.ENFILE:
		return "io_uring_setup 失败：文件描述符耗尽"
	case unix.ENOMEM:
		return "io_uring_setup 失败：内存或 RLIMIT_MEMLOCK 不足"
	case unix.EBUSY, unix.EAGAIN:
		return "io_uring_setup 暂时失败（资源繁忙，稍后重试）"
	}
	return fmt.Sprintf("io_uring_setup 失败: %v", err)
}

// kernelRelease 取内核版本字符串，仅用于日志。
func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "unknown"
	}
	return unix.ByteSliceToString(u.Release[:])
}

// backendName 返回队列实际生效的后端名（仅用于日志）。
func backendName(r Ring) string {
	if _, ok := r.(*uringRing); ok {
		return "io_uring"
	}
	return "libaio"
}
