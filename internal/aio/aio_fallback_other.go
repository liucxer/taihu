//go:build !linux

package aio

import (
	"errors"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/liucxer/taihu/internal/ierr"
)

// ring 非 Linux 平台（macOS 开发/自测）兜底实现：每个提交起一个 goroutine
// 执行同步 pread/pwrite，完成后唤醒 Wait。接口语义与 Linux 版一致。
type ring struct {
	mu       sync.Mutex // 保护 seq / inflight / closed
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
	if maxEvents <= 0 || maxEvents > 1<<16 {
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

// SubmitWriteBatch 实现 Ring.SubmitWriteBatch：非 Linux 兜底逐条 submit（编号连续）。
func (r *ring) SubmitWriteBatch(fd int, specs []WriteSpec) (uint64, int, error) {
	var first uint64
	for i := range specs {
		seq, err := r.submit(fd, specs[i].Buf, specs[i].Off, false)
		if err != nil {
			return first, i, err
		}
		if i == 0 {
			first = seq
		}
	}
	return first, len(specs), nil
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
		// 用 x/sys/unix 的带偏移读写（darwin/linux 都有）。
		//
		// 绝不要用 os.NewFile(fd) 包装后再丢：os.NewFile 会给 *os.File 挂 finalizer
		// （os/file_unix.go: runtime.SetFinalizer(f.file, (*file).close)），包装对象一旦
		// 成为垃圾，GC 就会 close 掉**调用方持有的同一个 fd**——轻则后续 IO 随机 EBADF，
		// 重则 kevent 对该 fd 报 EBADF 触发 runtime fatal（netpoll failed）整进程退出。
		// 这是 macOS 侧测试随机「bad file descriptor」的根因。
		var res int64
		if read {
			n, err := unix.Pread(fd, buf, off)
			res = result(n, err)
		} else {
			n, err := unix.Pwrite(fd, buf, off)
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
// unix.Pread/Pwrite 读到文件末尾时返回 (0, nil)、部分读返回 (n, nil)，与 Linux
// pread(2) 语义一致，所以无需再特判 EOF。旧实现走 os.File.ReadAt，它在部分读时
// 返回 (n, io.EOF) 并被一律折算成 0，会丢掉已读到的字节 —— 换掉后顺带修正。
func result(n int, err error) int64 {
	if err == nil {
		return int64(n)
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return -int64(errno)
	}
	return -1
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
			return events, ierr.ErrTimeout
		}
		var waitCh <-chan time.Time
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d <= 0 {
				return events, ierr.ErrTimeout
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
