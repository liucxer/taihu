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
// 见 NewWithOptions。两个后端实现同一个 Ring 接口，调用方无感。
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
// 本文件集中该包的**全部对外 API**（类型、常量与入口函数），且**只含导出名** ——
// 未导出的常量、变量、类型与辅助函数在 aio_internal.go，探测结论缓存在 probe_cache.go。
// 后端实现按平台分文件：aio_linux.go（libaio）、aio_uring_linux.go（io_uring）、
// aio_other.go（非 Linux 兜底）；探测的实现细节在 probe_linux.go / probe_other.go。
package aio

import (
	"fmt"
	"os"
	"strings"
	"time"
)

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

// Options 创建异步 IO 队列的参数，只描述队列自身。
// 设备上下文不在此处 —— 「目标块设备路径」仅 IOPoll 前置校验需要，由 NewWithOptions 的
// 独立参数提供（校验前它对该队列没有任何意义）。
type Options struct {
	// Mode 后端选择，零值为 ModeAuto。
	Mode Mode
	// MaxEvents 队列深度上限：libaio 为 io_setup 的 maxEvents，
	// io_uring 为 SQ entries（内核回填的 CQ 为其 2 倍）；非 Linux 兜底实现忽略该值。
	// 合法区间 [1, 65536]，越界由各后端构造返回 errInvalidMaxEvents。
	MaxEvents int
	// IOPoll 启用 IORING_SETUP_IOPOLL，仅 io_uring 后端有效。
	// 前置条件：目标块设备队列须开启轮询（/sys/class/block/<dev>/queue/io_poll=1），
	// 否则请求会永远停在 iopoll_list 上不完成（blk_poll 直接返回 0）。
	// 该前置条件由 NewWithOptions 在确实会建 io_uring 环时用 devPath 自动校验（见 checkIOPoll）。
	IOPoll bool
}

// NewWithOptions 按指定参数创建异步 IO 队列。
// devPath 为目标块设备路径，仅供 IOPoll 的前置条件校验定位 sysfs；为空则跳过校验。
// Linux 上 o.MaxEvents 为队列深度上限（1..65536）；非 Linux 平台忽略上限语义。
func NewWithOptions(o Options, devPath string) (Ring, error) {
	if o.Mode == ModeAuto {
		if v := os.Getenv(envMode); v != "" {
			m, err := ParseMode(v)
			if err != nil {
				return nil, fmt.Errorf("%s=%q: %w", envMode, v, err)
			}
			o.Mode = m
		}
	}

	// IOPOLL 前置校验：只在确定会建 io_uring 环（强制 on，或 auto 且探测支持）时才校验，
	// 否则 auto 回退 libaio 的场景会被无谓拦下 —— 那时 IOPoll 本就不生效。
	// 位置必须在本函数内、env 覆盖之后：TAIHU_AIO_URING=off 把 auto 拨到 libaio 时不该校验，
	// 而「最终走哪个后端」的决策只有这里知道，故校验归此处而非调用方。
	// devPath 为空则跳过 —— 纯队列用途的调用方（包内测试、非 IOPOLL 场景）没有设备上下文。
	if o.IOPoll && devPath != "" &&
		(o.Mode == ModeIOUring || (o.Mode == ModeAuto && probeCached().Supported)) {
		if err := checkIOPoll(devPath); err != nil {
			return nil, err
		}
	}

	switch o.Mode {
	case ModeIOUring:
		r, err := newIOUringRing(o.MaxEvents, o.IOPoll)
		if err != nil {
			return nil, err
		}
		logBackend(r, "forced on"+iopollSuffix(o.IOPoll))
		return r, nil
	case ModeLibAIO:
		r, err := newLibAIORing(o.MaxEvents)
		if err != nil {
			return nil, err
		}
		logBackend(r, "forced off")
		return r, nil
	}

	// ModeAuto：运行期探测（不比较版本号，见 probeCached）。
	pi := probeCached()
	if pi.Supported {
		r, err := newIOUringRing(o.MaxEvents, o.IOPoll)
		if err != nil {
			return nil, err
		}
		logBackend(r, fmt.Sprintf("auto: probe ok, features=0x%x%s",
			pi.Features, iopollSuffix(o.IOPoll)))
		return r, nil
	}
	r, err := newLibAIORing(o.MaxEvents)
	if err != nil {
		return nil, err
	}
	logBackend(r, "auto: io_uring 不可用 — "+pi.Reason)
	return r, nil
}
