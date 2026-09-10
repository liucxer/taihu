// Package aio 提供异步磁盘 IO 的纯 Go 实现，不依赖任何外部库。
//
// 设计背景：部署节点内核过旧不支持 io_uring，故采用 Linux 原生 AIO（libaio 内核接口，
// 自 Linux 2.6 起可用）。本包只封装四个系统调用：
//
//	io_setup / io_submit / io_getevents / io_destroy
//
// struct iocb / io_event 直接按 linux/aio_abi.h 的布局声明，无需 cgo。
//
// 平台策略：
//   - Linux：真异步（libaio），缓冲必须由调用方持有到 Wait 返回（对应 O_DIRECT 对齐池）。
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

// New 创建容量为 maxEvents 的异步 IO 队列。
// Linux 上 maxEvents 为 io_setup 的队列深度上限（1..65536）；非 Linux 平台忽略上限语义。
func New(maxEvents int) (Ring, error) {
	return newRing(maxEvents)
}
