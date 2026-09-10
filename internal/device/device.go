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
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/liucxer/taihu/internal/aio"
	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/ierr"
	"github.com/liucxer/taihu/internal/layout"
)

const (
	// aioDepth 内核 AIO 队列深度（io_setup maxEvents）。
	aioDepth = 256
	// pumpTimeout 完成泵的空闲轮询周期：无事件时泵每 200ms 醒来一次，用于 Close 时及时退出；
	// 有事件时 io_getevents 立即返回（min=1），不增加请求延迟。
	pumpTimeout = 200 * time.Millisecond
	// submitRetry 提交队列满（ErrFull）时的重试间隔。
	submitRetry = 100 * time.Microsecond
)

// errDeviceClosed 设备已关闭后仍尝试提交。
var errDeviceClosed = errors.New("taihu: device closed")

// Device 底层存储：直接操作裸设备文件。
type Device struct {
	f    *os.File
	path string

	ring   aio.Ring
	mu     sync.Mutex // 保护 m/pending/inSubmit/closed
	m      map[uint64]chan aio.Event
	pending map[uint64]aio.Event
	inSubmit int        // 处于「已提交 io 但尚未注册/消费事件」的请求数
	closed   bool
	pumpDone chan struct{}
}

// NewDevice 打开裸设备文件并创建异步 IO 队列。平台差异（O_DIRECT / 普通打开）在 openDevice 中处理。
// 返回 (*Device, error)，便于暴露打开失败。
func NewDevice(ctx context.Context, nvmePath string) (*Device, error) {
	f, err := openDevice(nvmePath)
	if err != nil {
		return nil, fmt.Errorf("taihu: open device %q: %w", nvmePath, err)
	}
	ring, err := aio.New(aioDepth)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("taihu: create aio ring: %w", err)
	}
	d := &Device{
		f:        f,
		path:     nvmePath,
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
	return segmentID * layout.SegmentSizeBytes
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
		if err != nil && err != aio.ErrTimeout {
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
func (d *Device) submitOp(buf []byte, off int64, read bool) (aio.Event, error) {
	fd := int(d.f.Fd())
	for {
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return aio.Event{}, errDeviceClosed
		}
		d.inSubmit++
		d.mu.Unlock()

		var seq uint64
		var err error
		if read {
			seq, err = d.ring.SubmitRead(fd, buf, off)
		} else {
			seq, err = d.ring.SubmitWrite(fd, buf, off)
		}

		ch := make(chan aio.Event, 1)
		d.mu.Lock()
		d.inSubmit--
		if err != nil {
			// 提交失败（含队列满）：无在途 IO，可直接返回；ErrFull 让出后重试。
			d.mu.Unlock()
			if err == aio.ErrFull {
				time.Sleep(submitRetry)
				continue
			}
			return aio.Event{}, err
		}
		if ev, ok := d.pending[seq]; ok { // 泵已取回本事件，直接消费
			delete(d.pending, seq)
			d.mu.Unlock()
			return ev, nil
		}
		d.m[seq] = ch
		d.mu.Unlock()
		return <-ch, nil
	}
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
	if off+aligned > layout.SegmentSizeBytes {
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

// Delete 将整段标记可回收。当前采用 append-only，物理擦除/重写延迟到 segment 级 GC 实现，
// 此处仅作占位，返回 nil。
func (d *Device) Delete(ctx context.Context, segmentID int64) error {
	_ = segmentID
	return nil
}
