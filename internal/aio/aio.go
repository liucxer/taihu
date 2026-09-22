// Package aio 提供异步磁盘 IO 的纯 Go 实现，不依赖任何外部库。
//
// Linux 上有两个后端，都只封装系统调用、结构体按 UAPI 手工声明（无需 cgo）：
//
//   - libaio：io_setup / io_submit / io_getevents / io_destroy，
//     结构体见 linux/aio_abi.h。自 Linux 2.6 起可用，是兼容性兜底。
//   - io_uring：io_uring_setup / io_uring_enter / io_uring_register，
//     结构体见 linux/io_uring.h。自 5.1 起可用，完成事件零系统调用收割。
//
// 选哪个由 Mode 决定（auto 时按 io_uring_setup 的 errno 运行期探测，不比较版本号），
// 见 NewWithOptions / Probe。两个后端实现同一个 Ring 接口，调用方无感。
//
// 平台策略：
//   - Linux：真异步（libaio 或 io_uring），缓冲必须由调用方持有到 Wait 返回
//     （对应 O_DIRECT 对齐池）。
//   - 其余平台（macOS 开发/自测）：goroutine + 同步 pread/pwrite 兜底，
//     接口语义一致（Submit 立即返回序号，Wait 阻塞收集完成），便于本地开发测试。
//
// 使用约束（调用方）：
//   - buf 在 Submit 后、对应完成事件被 Wait 取回前必须保持存活且不被改写；
//   - Linux + O_DIRECT 时 buf 首地址、偏移、长度需 4K 对齐（由 bufpool/device 层保证）。
//
// 本文件集中该包的**全部对外 API**（类型、常量、错误与入口函数），且**只含导出名** ——
// 未导出的常量、变量与辅助函数在 aio_internal.go，探测结论缓存在 probe_cache.go。
// 后端实现按平台分文件：aio_linux.go（libaio）、aio_uring_linux.go（io_uring）、
// aio_other.go（非 Linux 兜底）；探测的实现细节在 probe_linux.go / probe_other.go。
package aio

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ErrFull 表示提交队列已满（io_submit 返回 EAGAIN），应先 Wait 取回完成事件后重试。
var ErrFull = errors.New("aio: submission queue full")

// ErrTimeout 表示 Wait 在超时时间内未取够 min 个事件。
var ErrTimeout = errors.New("aio: wait timeout")

// ── 提交与完成的数据形状 ──────────────────────────────────────────────

// ReadSpec 批读的一个提交项：读 off 处 len(Buf) 字节到 Buf。同一批共享同一 fd。
type ReadSpec struct {
	Buf []byte
	Off int64
}

// WriteSpec 批写的一个提交项：把 Buf 的前 len(Buf) 字节写到 off 处。同一批共享同一 fd。
// buf 在 Submit 后、对应完成事件被 Wait 取回前必须保持存活且不被改写（与 ReadSpec 同约束）。
type WriteSpec struct {
	Buf []byte
	Off int64
}

// Event 一次已完成的异步 IO 结果。
type Event struct {
	// Data 提交时写入的用户数据（本封装固定为自增序号 seq，用于关联请求）。
	Data uint64
	// Res 结果：>=0 为读/写字节数；<0 为 -errno。
	Res int64
}

// Ring 异步 IO 完成队列。同一 Ring 可被多个 goroutine 并发 Submit，
// Wait 应串行调用（或由单一完成泵 goroutine 持有）。
type Ring interface {
	// SubmitRead 异步读 fd 上 off 处 len(buf) 字节到 buf，返回关联序号。
	// 队列满时返回 ErrFull（Wait 回收后重试）。
	SubmitRead(fd int, buf []byte, off int64) (uint64, error)

	// SubmitReadBatch 一次 io_submit 批量提交多条异步读（同一 fd，摊薄 syscall）。
	// 返回首个关联序号 firstSeq（第 i 项序号 = firstSeq+i，i∈[0,submitted)）与成功排队
	// 条数 submitted。submitted 可能 < len(specs)（内核提交队列截断），调用方须把未排队
	// 部分追加提交；队列满且一条未排入时返回 ErrFull。各 buf 须存活到完成事件被取回。
	SubmitReadBatch(fd int, specs []ReadSpec) (firstSeq uint64, submitted int, err error)

	// SubmitWrite 异步写 buf 到 fd 上 off 处，返回关联序号。语义同 SubmitRead。
	SubmitWrite(fd int, buf []byte, off int64) (uint64, error)

	// SubmitWriteBatch 一次 io_submit 批量提交多条异步写（同一 fd，摊薄 syscall）。
	// 返回首个关联序号 firstSeq（第 i 项序号 = firstSeq+i，i∈[0,submitted)）与成功排队
	// 条数 submitted。submitted 可能 < len(specs)（内核提交队列截断），调用方须把未排队
	// 部分追加提交；队列满且一条未排入时返回 ErrFull。各 buf 须存活到完成事件被取回。
	SubmitWriteBatch(fd int, specs []WriteSpec) (firstSeq uint64, submitted int, err error)

	// Wait 取回完成事件：阻塞至至少 min 个事件完成或 timeout 到期（timeout 为 nil 表示无限等待）。
	// 返回最多 max 个事件；超时时返回已取回的部分事件（可能少于 min，error 为 ErrTimeout）。
	// 事件按完成顺序返回；Linux 实现顺序不保证与提交顺序一致，靠 Event.Data 关联。
	Wait(min, max int, timeout *time.Duration) ([]Event, error)

	// Close 销毁完成队列，释放内核/后台资源。不应再 Submit。
	Close() error
}

// ── 后端选择 ────────────────────────────────────────────────────────

// Mode 选择异步磁盘 IO 的后端实现。
type Mode int

const (
	// ModeAuto 自动探测：内核支持 io_uring 则用，否则回退 libaio 并记录原因。
	ModeAuto Mode = iota
	// ModeLibAIO 强制 Linux 原生 AIO（libaio 内核接口）。
	ModeLibAIO
	// ModeIOUring 强制 io_uring；内核不支持时返回错误（不静默降级）。
	ModeIOUring
)

// ParseMode 解析命令行取值：auto（默认）|on|off。on 等价 ModeIOUring，off 等价 ModeLibAIO。
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return ModeAuto, nil
	case "on":
		return ModeIOUring, nil
	case "off":
		return ModeLibAIO, nil
	}
	return ModeAuto, fmt.Errorf("aio: invalid io-uring mode %q (want auto|on|off)", s)
}

// Options 创建异步 IO 队列的参数。
type Options struct {
	// Mode 后端选择，零值为 ModeAuto。
	Mode Mode
	// IOPoll 启用 IORING_SETUP_IOPOLL，仅 io_uring 后端有效。
	// 前置条件：目标块设备队列须开启轮询（/sys/class/block/<dev>/queue/io_poll=1），
	// 否则请求会永远停在 iopoll_list 上不完成（blk_poll 直接返回 0）。
	IOPoll bool
}

// NewWithOptions 按指定参数创建异步 IO 队列。
// Linux 上 maxEvents 为队列深度上限（1..65536）；非 Linux 平台忽略上限语义。
func NewWithOptions(maxEvents int, o Options) (Ring, error) {
	if o.Mode == ModeAuto {
		if v := os.Getenv(envMode); v != "" {
			m, err := ParseMode(v)
			if err != nil {
				return nil, fmt.Errorf("%s=%q: %w", envMode, v, err)
			}
			o.Mode = m
		}
	}

	switch o.Mode {
	case ModeIOUring:
		r, err := newIOUringRing(maxEvents, o.IOPoll)
		if err != nil {
			return nil, err
		}
		logBackend(r, "forced on"+iopollSuffix(o.IOPoll))
		return r, nil
	case ModeLibAIO:
		r, err := newLibAIORing(maxEvents)
		if err != nil {
			return nil, err
		}
		logBackend(r, "forced off")
		return r, nil
	}

	// ModeAuto：运行期探测（不比较版本号，见 Probe）。
	info := Probe()
	if info.Supported {
		r, err := newIOUringRing(maxEvents, o.IOPoll)
		if err != nil {
			return nil, err
		}
		logBackend(r, fmt.Sprintf("auto: probe ok, features=0x%x%s",
			info.Features, iopollSuffix(o.IOPoll)))
		return r, nil
	}
	r, err := newLibAIORing(maxEvents)
	if err != nil {
		return nil, err
	}
	logBackend(r, "auto: io_uring 不可用 — "+info.Reason)
	return r, nil
}

// ── io_uring 可用性探测 ──────────────────────────────────────────────

// Info 描述 io_uring 可用性探测结果。
type Info struct {
	Supported     bool   // 当前内核是否可用 io_uring
	Reason        string // 人类可读原因（供日志与错误信息）
	KernelRelease string // 内核版本字符串，仅供日志
	SQEntries     uint32 // 内核回填的提交队列深度
	CQEntries     uint32 // 内核回填的完成队列深度
	Features      uint32 // 内核能力位（IORING_FEAT_*）
}

// Probe 探测当前内核是否可用 io_uring。
//
// 判定完全基于 io_uring_setup 的 errno，不比较内核版本号 —— 版本号反映不了三类
// 误判：RHEL 系的 io_uring_disabled sysctl、容器 seccomp 拦截、以及发行版把 io_uring
// 反向移植进老内核（例如 openEuler/BCLinux 4.19.90 就带完整 backport，能力集约等于 5.8）。
//
// 只缓存确定性结论：资源类瞬时错误（ENOMEM/EMFILE 等）下次调用会重新探测，
// 否则一次偶发失败会把进程永久钉死在 libaio 上。
//
// 平台差异收敛在 probe() 内：非 Linux 平台恒为不支持（见 probe_other.go）。
// 缓存与转发逻辑在 probe_cache.go —— 那部分平台无关，不该进平台文件：平台文件只应
// 放真正的平台差异，混入平台无关逻辑会让「两平台行为是否一致」无法靠 diff 判断。
func Probe() Info {
	return probeCached()
}

// CheckIOPoll 校验目标块设备是否开启了队列级轮询 —— IOPOLL 的前置条件。
//
// 未开启时内核的 blk_poll 直接返回 0，io_uring 的轮询请求既不完成也不报错，
// 会永远停在 iopoll_list 上（表现为挂死），故这里提前硬失败并给出开启命令。
//
// 实现按平台分文件：Linux 读 sysfs 校验，非 Linux 恒报不支持（见 probe_other.go）。
func CheckIOPoll(devPath string) error {
	return checkIOPoll(devPath)
}
