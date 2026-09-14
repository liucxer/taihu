//go:build !linux

package aio

import (
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// ring 非 Linux 平台（macOS 开发/自测）兜底实现：每个提交起一个 goroutine
// 执行同步 pread/pwrite，完成后唤醒 Wait。接口语义与 Linux 版一致。
type ring struct {
	mu       sync.Mutex
	seq      uint64
	inflight map[uint64]*op
	wake     chan struct{}
	closed   bool
}

// op 一个在途请求。
type op struct {
	ev   Event
	done chan struct{}
}

// newLibAIORing 构建兜底队列。非 Linux 平台没有 libaio，落到这里。
func newLibAIORing(maxEvents int) (Ring, error) {
	if maxEvents <= 0 {
		return nil, errInvalidMaxEvents
	}
	return &ring{
		inflight: make(map[uint64]*op),
		wake:     make(chan struct{}, 1),
	}, nil
}

// newIOUringRing 非 Linux 平台没有 io_uring。
func newIOUringRing(int, bool) (Ring, error) {
	return nil, errors.New("aio: io_uring 仅 Linux 支持")
}

// SubmitRead 实现 Ring.SubmitRead。
func (r *ring) SubmitRead(fd int, buf []byte, off int64) (uint64, error) {
	return r.submit(fd, buf, off, true)
}

// SubmitReadBatch 实现 Ring.SubmitReadBatch：非 Linux 兜底逐条 submit（编号连续）。
func (r *ring) SubmitReadBatch(fd int, specs []ReadSpec) (uint64, int, error) {
	var first uint64
	for i := range specs {
		seq, err := r.submit(fd, specs[i].Buf, specs[i].Off, true)
		if err != nil {
			return first, i, err
		}
		if i == 0 {
			first = seq
		}
	}
	return first, len(specs), nil
}

// SubmitWrite 实现 Ring.SubmitWrite。
func (r *ring) SubmitWrite(fd int, buf []byte, off int64) (uint64, error) {
	return r.submit(fd, buf, off, false)
}

func (r *ring) submit(fd int, buf []byte, off int64, read bool) (uint64, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, syscall.EBADF
	}
	r.seq++
	seq := r.seq
	o := &op{done: make(chan struct{})}
	r.inflight[seq] = o
	r.mu.Unlock()

	go func() {
		// 用 os.File 的带偏移读写替代 syscall.Pread/Pwrite（后者仅 Linux 存在）。
		// 注意：不能 Close 该包装句柄——它会关闭调用方持有的底层 fd/句柄，
		// 导致后续提交 EBADF。os.File 对象无 finalizer 关闭句柄，随 GC 释放即可。
		f := os.NewFile(uintptr(fd), "aio")
		var res int64
		if read {
			n, err := f.ReadAt(buf, off)
			res = result(n, err)
		} else {
			n, err := f.WriteAt(buf, off)
			res = result(n, err)
		}
		o.ev = Event{Data: seq, Res: res}
		close(o.done)
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}()
	return seq, nil
}

// result 归一化结果：>=0 字节数；<0 -errno。
func result(n int, err error) int64 {
	if err != nil {
		// 越界读到 EOF：与 Linux pread 语义一致，返回 0 字节（非错误）。
		if err == io.EOF {
			return 0
		}
		if pe, ok := err.(*os.PathError); ok {
			err = pe.Err
		}
		if errno, ok := err.(syscall.Errno); ok {
			return -int64(errno)
		}
		return -1
	}
	return int64(n)
}

// Wait 实现 Ring.Wait：收集已完成事件，不足 min 时等待唤醒或超时。
func (r *ring) Wait(min, max int, timeout *time.Duration) ([]Event, error) {
	if max <= 0 {
		return nil, nil
	}
	var deadline time.Time
	if timeout != nil {
		deadline = time.Now().Add(*timeout)
	}

	events := make([]Event, 0, max)
	collect := func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		for seq, o := range r.inflight {
			select {
			case <-o.done:
				events = append(events, o.ev)
				delete(r.inflight, seq)
				if len(events) >= max {
					return true
				}
			default:
			}
		}
		return len(events) >= min
	}

	for {
		if collect() {
			return events, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return events, ErrTimeout
		}
		var waitCh <-chan time.Time
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d <= 0 {
				return events, ErrTimeout
			}
			waitCh = time.After(d)
		}
		select {
		case <-r.wake:
		case <-waitCh:
		}
	}
}

// Close 实现 Ring.Close。
func (r *ring) Close() error {
	r.mu.Lock()
	r.closed = true
	r.inflight = nil
	r.mu.Unlock()
	return nil
}
