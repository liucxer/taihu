//go:build !linux

package aio

import (
	"syscall"
	"sync"
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

// newRing 构建兜底队列。
func newRing(maxEvents int) (Ring, error) {
	if maxEvents <= 0 {
		return nil, errInvalidMaxEvents
	}
	return &ring{
		inflight: make(map[uint64]*op),
		wake:     make(chan struct{}, 1),
	}, nil
}

// SubmitRead 实现 Ring.SubmitRead。
func (r *ring) SubmitRead(fd int, buf []byte, off int64) (uint64, error) {
	return r.submit(fd, buf, off, true)
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
		var res int64
		if read {
			n, err := syscall.Pread(fd, buf, off)
			res = result(n, err)
		} else {
			n, err := syscall.Pwrite(fd, buf, off)
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
