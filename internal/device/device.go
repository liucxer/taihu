// Package device 提供底层存储设备访问：直接操作裸设备文件，按 segment 提供 append/read。
//
// 单个文件句柄，同时用于读写。Linux 上以 O_DIRECT 打开（绕过 page cache），
// 此时读写缓冲、文件偏移、每次 IO 长度都必须 layout.BlockSize(4K) 对齐：
//   - 读：ReadAt 读入 4K 对齐缓冲（上层 Storage.Get/ReadAt 负责对齐与拷贝）；
//   - 写：Append 将对象以整块 4K 对齐写出，末尾用 0 补齐到 4K。
//
// 磁盘 IO 经 internal/aio 异步提交（Linux: libaio；其他平台: goroutine 兜底），
// 由单一完成泵 goroutine 串行取回完成事件并分发到各提交方；对外 Append/ReadAt
// 仍保持同步语义（提交后阻塞至本请求完成）。不同偏移的并发读写安全。
package device

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/liucxer/taihu/internal/aio"
	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/pkg/ierr"
)

const (
	// aioDepth 内核 AIO 队列深度（io_setup maxEvents）。
	aioDepth = 256
	// pumpTimeout 完成泵的空闲轮询周期：无事件时泵每 200ms 醒来一次，用于 Close 时及时退出；
	// 有事件时 io_getevents 立即返回（min=1），不增加请求延迟。
	pumpTimeout = 200 * time.Millisecond
	// submitRetry 提交队列满（ErrFull）时的重试间隔。
	submitRetry = 100 * time.Microsecond
	// complRetryMax 完成侧瞬时错误（EAGAIN/EINTR）的最大重试次数。
	complRetryMax = 8
	// complRetryCap 完成侧重试退避上限：submitRetry 起逐次翻倍，不超过该值。
	complRetryCap = 2 * time.Millisecond
	// chunk4MiB 典型整块 IO 尺寸（与传输层 ChunkSize 一致），用于大小统计分档。
	chunk4MiB = 1 << 22
)

// ierr.ErrDeviceClosed 设备已关闭后仍尝试提交。

// Device 底层存储：直接操作裸设备文件。
type Device struct {
	f       *os.File
	path    string
	segSize int64 // 单段大小（由启动时布局注入，segmentBase/越界校验依赖）

	ring     aio.Ring
	mu       sync.Mutex // 保护 m/pending/inSubmit/closed
	m        map[uint64]chan aio.Event
	pending  map[uint64]aio.Event
	inSubmit int // 处于「已提交 io 但尚未注册/消费事件」的请求数
	closed   bool
	pumpDone chan struct{}

	// io4M/ioOther 磁盘 IO 尺寸统计（原子计数，供压测/验证单次 IO 是否整块 4MiB）。
	io4M       atomic.Int64 // 单次 IO == 4MiB 次数
	ioOther    atomic.Int64 // 单次 IO != 4MiB 次数
	bytes4M    atomic.Int64 // 4MiB IO 总字节
	bytesOther atomic.Int64 // 其他尺寸 IO 总字节
}

// NewDevice 打开裸设备文件并创建异步 IO 队列。segSize 为单段大小（layout.Layout.SegmentSizeBytes）。
// 平台差异（O_DIRECT / 普通打开）在 openDevice 中处理；异步 IO 后端由 opts 选择（默认 auto）。
// 返回 (*Device, error)，便于暴露打开失败。
func NewDevice(ctx context.Context, nvmePath string, segSize int64, opts ...Option) (*Device, error) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}

	f, err := openDevice(nvmePath)
	if err != nil {
		return nil, fmt.Errorf("taihu: open device %q: %w", nvmePath, err)
	}
	ring := o.ring // 仅测试注入；生产路径为 nil
	if ring == nil {
		// IOPOLL 的前置条件校验（设备队列轮询）由 aio 层在确实会建 io_uring 环时完成，
		// 故此处把设备路径一并传入 —— 「最终走哪个后端」的决策只在 aio 层知道。
		ring, err = aio.NewWithOptions(
			aio.Options{Mode: o.aioMode, MaxEvents: aioDepth, IOPoll: o.aioIOPoll, FD: int(f.Fd())}, nvmePath)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("taihu: create aio ring: %w", err)
		}
	}
	d := &Device{
		f:        f,
		path:     nvmePath,
		segSize:  segSize,
		ring:     ring,
		m:        make(map[uint64]chan aio.Event),
		pending:  make(map[uint64]aio.Event),
		pumpDone: make(chan struct{}),
	}
	go d.pump()
	return d, nil
}

// Close 关闭设备：置关闭标志 → 等完成泵排空在途事件 → 销毁 AIO 队列 → 关闭文件。
func (d *Device) Close() error {
	if d.f == nil {
		return nil
	}
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	<-d.pumpDone

	var err error
	if d.ring != nil {
		err = d.ring.Close()
	}
	if e := d.f.Close(); err == nil {
		err = e
	}
	d.f = nil
	return err
}

// segmentBase 返回段 i 的物理基址。
func (d *Device) segmentBase(segmentID int64) int64 {
	return segmentID * d.segSize
}

// bufAligned 报告 b 非空且首地址 4K 对齐（O_DIRECT 直写调用方缓冲的硬性前置）。
func bufAligned(b []byte) bool {
	return len(b) > 0 && uintptr(unsafe.Pointer(&b[0]))%uintptr(layout.BlockSize) == 0
}

// pump 完成泵：唯一调用 ring.Wait 的 goroutine，串行取回完成事件并按 seq 分发到提交方。
// 事件先到而提交方尚未注册通道时暂存 pending，由提交方注册时消费（不丢失、不重复）。
// 退出条件 closed 且无注册在途（m 空）且无未落定的提交（inSubmit==0），保证退出时
// 无任何 in-flight IO，ring/fd 可安全销毁。
func (d *Device) pump() {
	timeout := pumpTimeout
	for {
		evs, err := d.ring.Wait(1, 64, &timeout)
		if err != nil && err != ierr.ErrTimeout {
			// 硬错误：仅记录并继续轮询，避免 m 非空时提交方永久阻塞。
			fmt.Fprintf(os.Stderr, "taihu: aio pump wait: %v\n", err)
		}
		for _, ev := range evs {
			var ch chan aio.Event
			d.mu.Lock()
			if ch = d.m[ev.Data]; ch != nil {
				delete(d.m, ev.Data)
			} else {
				d.pending[ev.Data] = ev
			}
			d.mu.Unlock()
			if ch != nil {
				ch <- ev // cap=1，不阻塞
			}
		}
		d.mu.Lock()
		done := d.closed && len(d.m) == 0 && d.inSubmit == 0
		d.mu.Unlock()
		if done {
			close(d.pumpDone)
			return
		}
	}
}

// submitOp 提交一次异步 IO 并阻塞至其完成，返回完成事件。
// buf 必须存活到完成事件取回（O_DIRECT 下内核直读调用方缓冲）。
//
// 与泵的同步点：提交前先锁内检查 closed（未提交则快速失败）并自增 inSubmit，
// 使泵不会在「已 io_submit、事件尚未落定」的窗口内退出；随后锁内消费 pending
// 或注册 m，泵保证事件必然投递，提交方不会永久阻塞。
//
// 完成侧瞬时错误（EAGAIN/EINTR，见 retriableErrno）按原参数重提，最多 complRetryMax 次；
// 只有预算耗尽才把该 error 结果交回调用方 —— 与提交侧的 ErrFull 重试策略一致。
func (d *Device) submitOp(buf []byte, off int64, read bool) (aio.Event, error) {
	return d.submitOpN(buf, off, read, true)
}

// submitOpN 是 submitOp 的实现；count 为 false 时不计入 IO 尺寸统计
// （批路径已按逻辑项统计过尺寸，重提不重复计数）。
func (d *Device) submitOpN(buf []byte, off int64, read bool, count bool) (aio.Event, error) {
	var retries int
	for {
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return aio.Event{}, ierr.ErrDeviceClosed
		}
		d.inSubmit++
		d.mu.Unlock()

		var seq uint64
		var err error
		if read {
			seq, err = d.ring.SubmitRead(buf, off)
		} else {
			seq, err = d.ring.SubmitWrite(buf, off)
		}

		ch := make(chan aio.Event, 1)
		d.mu.Lock()
		d.inSubmit--
		if err != nil {
			// 提交失败（含队列满）：无在途 IO，可直接返回；ErrFull 让出后重试。
			d.mu.Unlock()
			if err == ierr.ErrFull {
				time.Sleep(submitRetry)
				continue
			}
			return aio.Event{}, err
		}
		// 仅统计首次成功提交的 IO（ErrFull 与完成侧重试均不重复计数）。
		if count {
			d.recordIO(int64(len(buf)))
			count = false
		}
		var ev aio.Event
		if p, ok := d.pending[seq]; ok { // 泵已取回本事件，直接消费
			delete(d.pending, seq)
			d.mu.Unlock()
			ev = p
		} else {
			d.m[seq] = ch
			d.mu.Unlock()
			ev = <-ch
		}

		errno, retriable := retriableErrno(ev.Res)
		if !retriable {
			return ev, nil
		}
		if retries >= complRetryMax {
			d.logComplRetry(read, off, int64(len(buf)), errno, retries, true)
			return ev, nil
		}
		retries++
		if retries == 1 {
			d.logComplRetry(read, off, int64(len(buf)), errno, retries, false)
		}
		time.Sleep(complRetryBackoff(retries))
	}
}

// retriableErrno 判定完成结果 res 是否为可重试的瞬时错误。
// EAGAIN：O_DIRECT 直接 IO 暂时无法完成 —— 语义即「稍后重试」，失败时无字节落盘，
// 原样重发安全；EINTR：请求被信号打断，同样以原参数重发。
// 这类瞬时结果会出现在完成队列里（已实测：nvme 上 4MiB O_DIRECT 写经 io_uring 偶发
// 以 -EAGAIN 完成；同负载 libaio 不复现）。完成侧若不重试就会把它当永久错误上抛，
// 使一次本可成功的 Put/Read 无谓失败。
func retriableErrno(res int64) (syscall.Errno, bool) {
	if res >= 0 {
		return 0, false
	}
	e := syscall.Errno(-res)
	if e == syscall.EAGAIN || e == syscall.EINTR {
		return e, true
	}
	return 0, false
}

// complRetryBackoff 返回第 n 次（从 1 起）完成侧重试的退避时长。
func complRetryBackoff(n int) time.Duration {
	d := submitRetry
	for i := 1; i < n && d < complRetryCap; i++ {
		d *= 2
	}
	if d > complRetryCap {
		d = complRetryCap
	}
	return d
}

// logComplRetry 打印完成侧重试诊断：每条 IO 仅在首次重试与预算耗尽时各打一行，避免刷屏。
func (d *Device) logComplRetry(read bool, off, size int64, errno syscall.Errno, n int, giveUp bool) {
	op := "write"
	if read {
		op = "read"
	}
	if giveUp {
		fmt.Fprintf(os.Stderr, "taihu: device %s: %s off=%d size=%d errno=%v: 完成侧重试 %d 次仍失败，上抛错误\n",
			d.path, op, off, size, errno, n)
		return
	}
	fmt.Fprintf(os.Stderr, "taihu: device %s: %s off=%d size=%d errno=%v: 完成侧瞬时错误，按原参数重提（上限 %d 次）\n",
		d.path, op, off, size, errno, complRetryMax)
}

// recordIO 累计一次成功提交的磁盘 IO 尺寸（4MiB 整块 vs 其他）。
func (d *Device) recordIO(size int64) {
	if size == chunk4MiB {
		d.io4M.Add(1)
		d.bytes4M.Add(size)
		return
	}
	d.ioOther.Add(1)
	d.bytesOther.Add(size)
}

// Stats 返回磁盘 IO 尺寸统计：4MiB 整块次数/字节 与 其他尺寸次数/字节。
func (d *Device) Stats() (io4M, ioOther, bytes4M, bytesOther int64) {
	return d.io4M.Load(), d.ioOther.Load(), d.bytes4M.Load(), d.bytesOther.Load()
}

func (d *Device) submitWrite(buf []byte, off int64) (aio.Event, error) {
	return d.submitOp(buf, off, false)
}

func (d *Device) submitRead(buf []byte, off int64) (aio.Event, error) {
	return d.submitOp(buf, off, true)
}

// checkWrite 校验写完成事件：res<0 为 -errno；res 必须等于 want（整块写出）。
func checkWrite(ev aio.Event, want int64) error {
	if ev.Res < 0 {
		return syscall.Errno(-ev.Res)
	}
	if ev.Res != want {
		return fmt.Errorf("taihu: short write: got %d want %d", ev.Res, want)
	}
	return nil
}

// Append 将 data 的 size 字节写入 segmentID 段、段内 offset 处，末尾用 0 补齐到 4K。
//
// offset 必须 4K 对齐。按 data 首地址是否 4K 对齐分两条路径：
//
//  1. 首地址 4K 对齐（server 侧 bufpool 汇集的整对象即满足）：主体（4K 倍数、地址/偏移
//     均对齐）直接异步直写调用方缓冲，零拷贝；仅不足 4K 的尾块分配一块 4K 临时缓冲补零后写出。
//     整对象大块不再暂存，最多 2 次异步写。
//  2. 首地址不对齐兜底：地址错位量在 4K 整数倍偏移下不变，任何子块都无法直写，
//     只能整体拷入对齐缓冲后按「对齐主体 + 尾部补齐」≤2 次异步写写出。
//
// data 不足 size 字节时返回错误。size 为 0 时无写 IO。
func (d *Device) Append(ctx context.Context, segmentID, off, size int64, data []byte) error {
	if off < 0 || off%layout.BlockSize != 0 {
		return fmt.Errorf("taihu: append offset %d not 4K aligned", off)
	}
	aligned := layout.Align4k(size)
	if off+aligned > d.segSize {
		return ierr.ErrTooLarge
	}
	if int64(len(data)) < size {
		return fmt.Errorf("taihu: append short data: size=%d have=%d", size, len(data))
	}
	if aligned == 0 {
		return nil // size == 0：无数据可写
	}

	pos := d.segmentBase(segmentID) + off
	bulkEnd := size &^ (layout.BlockSize - 1) // 4K 倍数的主体逻辑长度
	tailLen := size - bulkEnd                 // 尾部不足一格的字节数 [0, 4096)

	// 路径 1：首地址 4K 对齐 → 主体直写，只分配 4K 临时缓冲处理尾块。
	if bufAligned(data) {
		if bulkEnd > 0 {
			ev, err := d.submitWrite(data[:bulkEnd], pos)
			if err != nil {
				return err
			}
			if err := checkWrite(ev, bulkEnd); err != nil {
				return err
			}
		}
		if tailLen > 0 {
			tmp := bufpool.Get(int(layout.BlockSize))
			n := copy(tmp, data[bulkEnd:size])
			clear(tmp[n:]) // 补齐到整块 4K（tmp 长度恰为 BlockSize）
			ev, err := d.submitWrite(tmp, pos+bulkEnd)
			bufpool.Put(tmp)
			if err != nil {
				return err
			}
			if err := checkWrite(ev, layout.BlockSize); err != nil {
				return err
			}
		}
		return nil
	}

	// 路径 2：首地址不对齐兜底，整体拷贝进对齐缓冲后两次写出。
	buf := bufpool.Get(int(aligned))
	defer bufpool.Put(buf) // submit 均阻塞至完成，返回后缓冲即可复用
	copy(buf, data[:size])
	clear(buf[size:aligned]) // 末尾 0 填充到 aligned（buf 容量可能大于 aligned）

	if bulkEnd > 0 {
		ev, err := d.submitWrite(buf[:bulkEnd], pos)
		if err != nil {
			return err
		}
		if err := checkWrite(ev, bulkEnd); err != nil {
			return err
		}
	}
	if tailLen > 0 {
		ev, err := d.submitWrite(buf[bulkEnd:aligned], pos+bulkEnd)
		if err != nil {
			return err
		}
		if err := checkWrite(ev, aligned-bulkEnd); err != nil {
			return err
		}
	}
	return nil
}

// WriteJob 设备级批写项：把 Data 的前 Size 字节写到 segmentID 段内 off 处
// （末尾不足 4K 补零，语义与 Append 逐项一致）。批内各项独立、可并发提交。
type WriteJob struct {
	SegmentID int64
	Off       int64
	Data      []byte
	Size      int64
}

// AppendBatch 一次 io_submit 批量提交多条段内写（同一 ring、共用设备 fd），
// 全部完成后返回。逐项语义与 Append 完全等价：
//   - off 须 4K 对齐，off+Align4k(Size) ≤ 段大小；
//   - 首地址 4K 对齐的项主体直写调用方缓冲，仅尾块分配 4K 临时缓冲补零（零拷贝主体）；
//   - 首地址不对齐的项整体拷入对齐缓冲后写出；
//   - 各项 size==0 时无写 IO。
//
// 批内任意项的缓冲必须由调用方持有到本函数返回（submit 同步等待全部完成）。
func (d *Device) AppendBatch(ctx context.Context, jobs []WriteJob) error {
	type pspec struct {
		buf []byte
		off int64
	}
	var specs []pspec
	var tmps [][]byte // 补零/对齐临时缓冲，须存活到全部事件取回
	defer func() {
		for _, t := range tmps {
			bufpool.Put(t)
		}
	}()

	for i := range jobs {
		j := &jobs[i]
		if j.Off < 0 || j.Off%layout.BlockSize != 0 {
			return fmt.Errorf("taihu: append offset %d not 4K aligned", j.Off)
		}
		size := j.Size
		if int64(len(j.Data)) < size {
			return fmt.Errorf("taihu: append short data: size=%d have=%d", size, len(j.Data))
		}
		aligned := layout.Align4k(size)
		if j.Off+aligned > d.segSize {
			return ierr.ErrTooLarge
		}
		if aligned == 0 {
			continue // size == 0：无数据可写
		}

		pos := d.segmentBase(j.SegmentID) + j.Off
		bulkEnd := size &^ (layout.BlockSize - 1) // 4K 倍数的主体逻辑长度
		tailLen := size - bulkEnd                 // 尾部不足一格的字节数 [0, 4096)

		if bufAligned(j.Data) {
			if bulkEnd > 0 {
				specs = append(specs, pspec{buf: j.Data[:bulkEnd], off: pos})
			}
			if tailLen > 0 {
				tmp := bufpool.Get(int(layout.BlockSize))
				n := copy(tmp, j.Data[bulkEnd:size])
				clear(tmp[n:])
				tmps = append(tmps, tmp)
				specs = append(specs, pspec{buf: tmp, off: pos + bulkEnd})
			}
			continue
		}

		// 首地址不对齐兜底：整体拷入对齐缓冲后写出。
		buf := bufpool.Get(int(aligned))
		copy(buf, j.Data[:size])
		clear(buf[size:aligned])
		tmps = append(tmps, buf)
		if bulkEnd > 0 {
			specs = append(specs, pspec{buf: buf[:bulkEnd], off: pos})
		}
		if tailLen > 0 {
			specs = append(specs, pspec{buf: buf[bulkEnd:aligned], off: pos + bulkEnd})
		}
	}
	if len(specs) == 0 {
		return nil
	}

	var firstErr error
	remaining := specs
	for len(remaining) > 0 {
		chunk := remaining
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return ierr.ErrDeviceClosed
		}
		d.inSubmit += len(chunk)
		d.mu.Unlock()

		rs := make([]aio.WriteSpec, len(chunk))
		for i := range chunk {
			rs[i] = aio.WriteSpec{Buf: chunk[i].buf, Off: chunk[i].off}
		}
		first, n, err := d.ring.SubmitWriteBatch(rs)
		if err == ierr.ErrFull || n == 0 {
			// 队列满且一条未排入：让出后重试整块（未推进 seq，不丢 IO）。
			d.mu.Lock()
			d.inSubmit -= len(chunk)
			d.mu.Unlock()
			time.Sleep(submitRetry)
			continue
		}
		if err != nil {
			d.mu.Lock()
			d.inSubmit -= len(chunk)
			d.mu.Unlock()
			return err
		}

		evs, werr := d.batchWait(first, n)
		d.mu.Lock()
		d.inSubmit -= len(chunk)
		d.mu.Unlock()
		if werr != nil {
			return werr
		}
		for i := 0; i < n; i++ {
			d.recordIO(int64(len(chunk[i].buf)))
			if firstErr != nil {
				continue
			}
			ev := evs[i]
			if _, retriable := retriableErrno(ev.Res); retriable {
				// 批内瞬时错误：该条单独重提（完成侧重试在 submitOpN 内；尺寸已计过，不重复统计）。
				ev, werr = d.submitOpN(chunk[i].buf, chunk[i].off, false, false)
				if werr != nil {
					firstErr = werr
					continue
				}
			}
			if e := checkWrite(ev, int64(len(chunk[i].buf))); e != nil {
				firstErr = e
			}
		}
		remaining = chunk[n:]
	}
	return firstErr
}

// ReadAt 读取段内 off 起 size 字节，经异步 IO 直接读入池化对齐缓冲并返回数据切片。
//
// 要求 off 与 size 均为 4K 对齐（O_DIRECT 约束，由上层 Storage.ReadAt 负责对齐）。
// 返回切片持有一块 bufpool 缓冲：正常情况下 len == size；读到设备/对象末尾不足时
// 返回已读前缀（不附错误，长度即已读字节数）。调用方不再使用后必须将返回值原样交还
// bufpool.Put（归还池）。size == 0 返回 (nil, nil)。
func (d *Device) ReadAt(ctx context.Context, segmentID, off, size int64) ([]byte, error) {
	if off < 0 || off%layout.BlockSize != 0 {
		return nil, fmt.Errorf("taihu: read offset %d not 4K aligned", off)
	}
	if size < 0 || size%layout.BlockSize != 0 {
		return nil, fmt.Errorf("taihu: read size %d not 4K aligned", size)
	}
	if size == 0 {
		return nil, nil
	}

	buf := bufpool.Get(int(size))
	ev, err := d.submitRead(buf[:size], d.segmentBase(segmentID)+off)
	if err != nil {
		bufpool.Put(buf)
		return nil, err
	}
	if ev.Res < 0 {
		bufpool.Put(buf)
		return nil, syscall.Errno(-ev.Res)
	}
	n := ev.Res
	if n == 0 {
		bufpool.Put(buf)
		return nil, io.EOF
	}
	return buf[:n], nil
}

// ReadAtInto 读取段内 off 起 size 字节，经异步 IO 直接 DMA 进调用方 dst。
//
// 要求 off 与 size 均为 4K 对齐（O_DIRECT 约束），dst 首地址 4K 对齐（bufAligned）
// 且 cap ≥ size；dst 由调用方持有至本函数返回（submit 同步等待完成）。
// 返回实际读入字节数（读到设备/对象末尾不足时短读，不附错误）；size == 0 返回 (0, nil)。
// 单条：同一 ring 异步 submit + pump 完成泵；返回实际读入字节数（请求窗口），短读为 io.EOF。
func (d *Device) ReadAtInto(ctx context.Context, segmentID, off, size int64, dst []byte) (int64, error) {
	if off < 0 || off%layout.BlockSize != 0 {
		return 0, fmt.Errorf("taihu: read offset %d not 4K aligned", off)
	}
	if size < 0 || size%layout.BlockSize != 0 {
		return 0, fmt.Errorf("taihu: read size %d not 4K aligned", size)
	}
	if size == 0 {
		return 0, nil
	}
	if int64(len(dst)) < size {
		return 0, fmt.Errorf("taihu: read dst %d < size %d", len(dst), size)
	}
	if !bufAligned(dst) {
		return 0, fmt.Errorf("taihu: read dst not 4K aligned")
	}

	ev, err := d.submitRead(dst[:size], d.segmentBase(segmentID)+off)
	if err != nil {
		return 0, err
	}
	if ev.Res < 0 {
		return 0, syscall.Errno(-ev.Res)
	}
	n := ev.Res
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// ReadJob 设备级批读项：读 segmentID 段内 off 处 size 字节到 buf。
// 仅供 O_DIRECT 直读快路径使用：off、size 均须 4K 对齐，buf 首地址 4K 对齐且 cap ≥ size。
type ReadJob struct {
	SegmentID int64
	Off       int64
	Buf       []byte
	Size      int64
}

// ReadAtIntoBatch 一次 io_submit 批量提交多条段内直读（同一 ring、共用设备 fd），
// 全部完成后按 jobs 顺序返回各项读入字节数。批内任意项不满足直读快路径（非 4K 对齐
// 等）时，该项回退为单条 ReadAtInto；整批 submit 失败（队列满/截断）时全量回退单条。
// 语义与逐条 ReadAtInto 完全等价，仅合并 io_submit 摊薄系统调用开销。
func (d *Device) ReadAtIntoBatch(ctx context.Context, jobs []ReadJob) ([]int64, error) {
	ns := make([]int64, len(jobs))
	if len(jobs) == 0 {
		return ns, nil
	}
	// 收集批直读项（4K 对齐 + buf 对齐），其余标特殊标记回退单条。
	specs0 := make([]aio.ReadSpec, 0, len(jobs))
	idx0 := make([]int, 0, len(jobs)) // spec→jobs 位置
	req0 := make([]int64, 0, len(jobs))
	fallback := make(map[int]struct{})
	for i := range jobs {
		j := &jobs[i]
		if j.Off < 0 || j.Off%layout.BlockSize != 0 || j.Size <= 0 ||
			j.Size%layout.BlockSize != 0 || int64(len(j.Buf)) < j.Size ||
			!bufAligned(j.Buf[:1]) {
			fallback[i] = struct{}{}
			continue
		}
		specs0 = append(specs0, aio.ReadSpec{Buf: j.Buf[:j.Size], Off: d.segmentBase(j.SegmentID) + j.Off})
		idx0 = append(idx0, i)
		req0 = append(req0, j.Size)
	}
	if len(specs0) == 0 {
		return d.readJobsIndividually(ctx, jobs)
	}

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, ierr.ErrDeviceClosed
	}
	d.inSubmit += len(specs0)
	d.mu.Unlock()

	firstSeq, submitted, err := d.ring.SubmitReadBatch(specs0)
	if err != nil || submitted == 0 {
		d.mu.Lock()
		d.inSubmit -= len(specs0)
		d.mu.Unlock()
		return d.readJobsIndividually(ctx, jobs)
	}

	evs, err := d.batchWait(firstSeq, submitted)
	d.mu.Lock()
	d.inSubmit -= len(specs0)
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for i := 0; i < submitted; i++ {
		j := idx0[i]
		ev := evs[i]
		d.recordIO(req0[i])
		switch {
		case ev.Res < 0:
			if _, retriable := retriableErrno(ev.Res); retriable {
				// 批内瞬时错误：该条退化为单条重提（完成侧重试在 submitOpN 内；尺寸已计过）。
				ev2, werr2 := d.submitOpN(jobs[j].Buf[:req0[i]],
					d.segmentBase(jobs[j].SegmentID)+jobs[j].Off, true, false)
				if werr2 != nil {
					ns[j] = 0
					return ns, werr2
				}
				if ev2.Res < 0 {
					ns[j] = 0
					return ns, syscall.Errno(-ev2.Res)
				}
				n := ev2.Res
				if n > 0 && n < req0[i] {
					n = req0[i] // 与批路径口径一致：短读按请求窗口上报
				}
				ns[j] = n
				continue
			}
			ns[j] = 0
			return ns, syscall.Errno(-ev.Res)
		case ev.Res == 0:
			ns[j] = 0
			// 对象/设备末尾：短读。交给剩余 jobs 逐条填，return 由本次短读语义交给调用方。
		default:
			n := ev.Res
			if n < req0[i] {
				n = req0[i]
			}
			ns[j] = n
		}
	}
	// 回退未批排队的项（submitted 截断后的尾部或非批项）。
	for j := range fallback {
		if ns[j] == 0 && jobs[j].Size > 0 {
			n, e := d.ReadAtInto(ctx, jobs[j].SegmentID, jobs[j].Off, jobs[j].Size, jobs[j].Buf)
			ns[j] = n
			if e != nil && e != io.EOF {
				return ns, e
			}
		}
	}
	// submitted 可能 < len(specs0)：specs0[submitted:] 对应的 jobs 尚未读，补单条。
	for k := submitted; k < len(specs0); k++ {
		j := idx0[k]
		n, e := d.ReadAtInto(ctx, jobs[j].SegmentID, jobs[j].Off, jobs[j].Size, jobs[j].Buf)
		ns[j] = n
		if e != nil && e != io.EOF {
			return ns, e
		}
	}
	return ns, nil
}

// batchWait 等待 firstSeq 起 n 个已完成事件（镜像 submitOp 的 pending/m 消费语义：
// 事件先到 pump 则进 pending，由注册通道时消费）。泵退出的安全处理与 submitOp 一致。
func (d *Device) batchWait(firstSeq uint64, n int) ([]aio.Event, error) {
	evs := make([]aio.Event, n)
	for i := 0; i < n; i++ {
		seq := firstSeq + uint64(i)
		ch := make(chan aio.Event, 1)
		d.mu.Lock()
		if ev, ok := d.pending[seq]; ok {
			delete(d.pending, seq)
			d.mu.Unlock()
			evs[i] = ev
			continue
		}
		d.m[seq] = ch
		d.mu.Unlock()
		select {
		case ev := <-ch:
			evs[i] = ev
		case <-d.pumpDone:
			d.mu.Lock()
			delete(d.m, seq)
			d.mu.Unlock()
			return evs, ierr.ErrDeviceClosed
		}
	}
	return evs, nil
}

// readJobsIndividually 逐条 ReadAtInto 回退（非批项 / 整批提交失败时）。
func (d *Device) readJobsIndividually(ctx context.Context, jobs []ReadJob) ([]int64, error) {
	ns := make([]int64, len(jobs))
	for i := range jobs {
		j := &jobs[i]
		if j.Size <= 0 {
			ns[i] = 0
			continue
		}
		n, e := d.ReadAtInto(ctx, j.SegmentID, j.Off, j.Size, j.Buf)
		ns[i] = n
		if e != nil && e != io.EOF {
			return ns, e
		}
	}
	return ns, nil
}

// ── 构建选项 ────────────────────────────────────────────────────────

// Option 是 NewDevice 的可选参数（变参选项）。新增配置项时在此扩展，
// 既有调用点无需改动签名。
type Option func(*options)

// options NewDevice 的可选配置。
type options struct {
	aioMode   aio.Mode
	aioIOPoll bool
	// ring 仅测试注入：非 nil 时替代真实 aio ring（用于构造指定完成事件序列，
	// 如瞬时 EAGAIN）。生产路径恒为 nil。
	ring aio.Ring
}

// defaultOptions 默认走 auto：内核支持 io_uring 就用，否则回退 libaio 并记录原因。
// 实际生效的后端在 aio 层打启动日志（backend=... kernel=...）。
func defaultOptions() options {
	return options{aioMode: aio.ModeAuto}
}

// WithAIOMode 指定异步磁盘 IO 后端：aio.ModeAuto / ModeLibAIO / ModeIOUring。
// ModeIOUring 在内核不支持时返回错误（不静默降级）。
func WithAIOMode(m aio.Mode) Option {
	return func(o *options) { o.aioMode = m }
}

// WithAIOIOPoll 启用 io_uring 的 IORING_SETUP_IOPOLL（仅 io_uring 后端生效）。
// 前置条件：目标块设备队列须开启轮询（/sys/class/block/<dev>/queue/io_poll=1），
// 否则请求会永远停在 iopoll_list 上不完成。
func WithAIOIOPoll(on bool) Option {
	return func(o *options) { o.aioIOPoll = on }
}

func DeviceCapacity(path string) (int64, error) {
	return deviceCapacity(path)
}
