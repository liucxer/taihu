//go:build linux

package aio

import (
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// 事件与 iocb 布局必须与 linux/aio_abi.h 一致（64 位平台）。
//
//	struct io_event { __u64 data; __u64 obj; __s64 res; __s64 res2; }; // 32 字节
//	struct iocb { ... };                                             // 64 字节

// ioEvent 内核完成事件。
type ioEvent struct {
	Data uint64
	Obj  uint64
	Res  int64
	Res2 int64
}

// iocb 内核提交块（64 字节，字段顺序不可调）。
type iocb struct {
	Data     uint64
	Key      uint32 // 与 RwFlags 共用 4 字节（内核 PADDED(aio_key, aio_rw_flags)）
	RwFlags  uint32
	LioOp    uint16 // IOCB_CMD_PREAD / IOCB_CMD_PWRITE
	ReqPrio  int16
	Fildes   uint32
	Buf      uint64
	Nbytes   uint64
	Offset   int64
	Reserved uint64
	Flags    uint32
	Resfd    uint32
}

const (
	opcodePread  uint16 = 0
	opcodePwrite uint16 = 1
)

// ring 基于 Linux 原生 AIO 的真异步实现。
type ring struct {
	ctx uint64 // aio_context_t

	mu  sync.Mutex // 保护 seq 与 iocb 复用
	seq uint64

	events []ioEvent // Wait 复用缓冲
}

// newRing 创建内核 AIO 上下文（io_setup）。
func newRing(maxEvents int) (Ring, error) {
	if maxEvents <= 0 || maxEvents > 1<<16 {
		return nil, errInvalidMaxEvents
	}
	var ctx uint64
	_, _, errno := unix.Syscall(unix.SYS_IO_SETUP, uintptr(maxEvents), uintptr(unsafe.Pointer(&ctx)), 0)
	if errno != 0 {
		return nil, errno
	}
	return &ring{ctx: ctx, events: make([]ioEvent, 0, 64)}, nil
}

// SubmitRead 实现 Ring.SubmitRead。
func (r *ring) SubmitRead(fd int, buf []byte, off int64) (uint64, error) {
	return r.submit(fd, buf, off, opcodePread)
}

// SubmitWrite 实现 Ring.SubmitWrite。
func (r *ring) SubmitWrite(fd int, buf []byte, off int64) (uint64, error) {
	return r.submit(fd, buf, off, opcodePwrite)
}

// submit 填充 iocb 并 io_submit。内核在 io_submit 内深拷贝 iocb，返回后 iocb 可复用。
func (r *ring) submit(fd int, buf []byte, off int64, op uint16) (uint64, error) {
	r.mu.Lock()
	r.seq++
	seq := r.seq
	cb := &iocb{
		Data:   seq,
		LioOp:  op,
		Fildes: uint32(fd),
		Nbytes: uint64(len(buf)),
		Offset: off,
	}
	if len(buf) > 0 {
		cb.Buf = uint64(uintptr(unsafe.Pointer(&buf[0])))
	}
	cbpp := [1]*iocb{cb}
	_, _, errno := unix.Syscall(unix.SYS_IO_SUBMIT, uintptr(r.ctx), 1, uintptr(unsafe.Pointer(&cbpp[0])))
	r.mu.Unlock()
	if errno != 0 {
		if errno == unix.EAGAIN {
			return 0, ErrFull
		}
		return 0, errno
	}
	// 缓冲交由调用方持有到 Wait 取回事件（O_DIRECT 下内核直读 buf）。
	return seq, nil
}

// Wait 实现 Ring.Wait：io_getevents 阻塞取回 [min, max] 个完成事件。
func (r *ring) Wait(min, max int, timeout *time.Duration) ([]Event, error) {
	if max <= 0 {
		return nil, nil
	}
	if cap(r.events) < max {
		r.events = make([]ioEvent, 0, max)
	}
	evs := r.events[:max]

	var tsp *unix.Timespec
	var tspv unix.Timespec
	if timeout != nil {
		tspv = unix.NsecToTimespec(timeout.Nanoseconds())
		tsp = &tspv
	}
	var tspPtr uintptr
	if tsp != nil {
		tspPtr = uintptr(unsafe.Pointer(tsp))
	}

	// io_getevents 在信号打断时返回 EINTR，重试直到取够或超时。
	var got int64
	for {
		n, _, errno := unix.Syscall6(unix.SYS_IO_GETEVENTS, uintptr(r.ctx),
			uintptr(min), uintptr(max), uintptr(unsafe.Pointer(&evs[0])), tspPtr, 0)
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			return nil, errno
		}
		got = int64(n)
		break
	}

	out := make([]Event, 0, got)
	for i := int64(0); i < got; i++ {
		out = append(out, Event{Data: evs[i].Data, Res: evs[i].Res})
	}
	if int(got) < min && timeout != nil {
		return out, ErrTimeout
	}
	return out, nil
}

// Close 实现 Ring.Close：io_destroy 销毁内核上下文。
func (r *ring) Close() error {
	if r.ctx == 0 {
		return nil
	}
	ctx := r.ctx
	r.ctx = 0
	_, _, errno := unix.Syscall(unix.SYS_IO_DESTROY, uintptr(ctx), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
