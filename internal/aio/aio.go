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
package aio

import (
	"errors"
	"time"
)

// ErrFull 表示提交队列已满（io_submit 返回 EAGAIN），应先 Wait 取回完成事件后重试。
var ErrFull = errors.New("aio: submission queue full")

// errInvalidMaxEvents 表示 New 的 maxEvents 超出内核允许范围。
var errInvalidMaxEvents = errors.New("aio: maxEvents must be in [1, 65536]")

// ReadSpec 批读的一个提交项：读 off 处 len(Buf) 字节到 Buf。同一批共享同一 fd。
type ReadSpec struct {
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

	// Wait 取回完成事件：阻塞至至少 min 个事件完成或 timeout 到期（timeout 为 nil 表示无限等待）。
	// 返回最多 max 个事件；超时时返回已取回的部分事件（可能少于 min，error 为 ErrTimeout）。
	// 事件按完成顺序返回；Linux 实现顺序不保证与提交顺序一致，靠 Event.Data 关联。
	Wait(min, max int, timeout *time.Duration) ([]Event, error)

	// Close 销毁完成队列，释放内核/后台资源。不应再 Submit。
	Close() error
}

// ErrTimeout 表示 Wait 在超时时间内未取够 min 个事件。
var ErrTimeout = errors.New("aio: wait timeout")

// New 创建容量为 maxEvents 的异步 IO 队列，后端固定为 libaio（保持既有行为）。
//
// 需要 io_uring 请用 NewWithMode / NewWithOptions —— 本函数暂不改为 auto 探测，
// 以免既有的直接调用方（测试等）在支持 io_uring 的机器上静默切换后端。
// 生产路径（device 层）默认走 ModeAuto，并在启动日志里标明实际生效的后端。
//
// Linux 上 maxEvents 为队列深度上限（1..65536）；非 Linux 平台忽略上限语义。
func New(maxEvents int) (Ring, error) {
	return NewWithOptions(maxEvents, Options{Mode: ModeLibAIO})
}
