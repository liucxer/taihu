package device

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/aio"
	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/layout"
)

// fakeRing 是 aio.Ring 的测试替身：按提交顺序把「完成结果」写成事件交给完成泵，
// 用于构造真实设备难以稳定复现的瞬时完成结果（如 -EAGAIN）。
//
// overrides 以提交序号（0 起，批内每个 spec 各占一个序号）为键覆盖该次提交的完成结果；
// 未覆盖的提交按自然成功上报（写/读的长度即 buf 长度）。
type fakeRing struct {
	mu        sync.Mutex
	seq       uint64
	nSub      int
	overrides map[int]int64
	comps     []aio.Event
	closed    bool
}

func newFakeRing(overrides map[int]int64) *fakeRing {
	return &fakeRing{overrides: overrides}
}

// overall 返回累计提交次数（含批内各项与重试提交）。
func (f *fakeRing) overall() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nSub
}

// enqueue 追加一条完成事件（调用方须持有 f.mu）。
func (f *fakeRing) enqueue(buf []byte) {
	f.seq++
	res := int64(len(buf))
	if v, ok := f.overrides[f.nSub]; ok {
		res = v
	}
	f.nSub++
	f.comps = append(f.comps, aio.Event{Data: f.seq, Res: res})
}

func (f *fakeRing) SubmitRead(fd int, buf []byte, off int64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueue(buf)
	return f.seq, nil
}

func (f *fakeRing) SubmitWrite(fd int, buf []byte, off int64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueue(buf)
	return f.seq, nil
}

func (f *fakeRing) SubmitReadBatch(fd int, specs []aio.ReadSpec) (uint64, int, error) {
	return f.batch(len(specs), func(i int) []byte { return specs[i].Buf })
}

func (f *fakeRing) SubmitWriteBatch(fd int, specs []aio.WriteSpec) (uint64, int, error) {
	return f.batch(len(specs), func(i int) []byte { return specs[i].Buf })
}

func (f *fakeRing) batch(n int, bufAt func(int) []byte) (uint64, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var first uint64
	for i := 0; i < n; i++ {
		if i == 0 {
			first = f.seq + 1
		}
		f.enqueue(bufAt(i))
	}
	return first, n, nil
}

// Wait 立即取回已就绪事件；无事件时短暂让出后再返回 ErrTimeout，避免完成泵空转烧 CPU。
func (f *fakeRing) Wait(min, max int, timeout *time.Duration) ([]aio.Event, error) {
	f.mu.Lock()
	if len(f.comps) == 0 {
		f.mu.Unlock()
		time.Sleep(time.Millisecond)
		return nil, aio.ErrTimeout
	}
	n := len(f.comps)
	if n > max {
		n = max
	}
	out := f.comps[:n]
	f.comps = f.comps[n:]
	f.mu.Unlock()
	return out, nil
}

func (f *fakeRing) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

// withRing 注入测试替身 ring（仅测试使用；生产路径恒为真实 aio ring）。
func withRing(r aio.Ring) Option {
	return func(o *options) { o.ring = r }
}

// newFakeDevice 建一个挂 fakeRing 的 Device（文件真实存在即可，IO 由替身接管）。
func newFakeDevice(t *testing.T, fr *fakeRing) *Device {
	t.Helper()
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	fd, err := os.Create(devPath)
	if err != nil {
		t.Fatalf("create device file: %v", err)
	}
	_ = fd.Close()
	dev, err := NewDevice(context.Background(), devPath, layout.DefaultSegmentSizeBytes, withRing(fr))
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}
	t.Cleanup(func() { _ = dev.Close() })
	return dev
}

// TestRetriableErrno 瞬时错误判定：EAGAIN/EINTR 可重试，其余（含成功）不可。
func TestRetriableErrno(t *testing.T) {
	cases := []struct {
		res  int64
		want syscall.Errno
		ok   bool
	}{
		{0, 0, false},
		{4096, 0, false},
		{-int64(syscall.EAGAIN), syscall.EAGAIN, true},
		{-int64(syscall.EINTR), syscall.EINTR, true},
		{-int64(syscall.EIO), 0, false},
		{-int64(syscall.ENOSPC), 0, false},
	}
	for _, c := range cases {
		got, ok := retriableErrno(c.res)
		if ok != c.ok || got != c.want {
			t.Errorf("retriableErrno(%d) = (%v,%v), want (%v,%v)", c.res, got, ok, c.want, c.ok)
		}
	}
	// 退避从 submitRetry 起逐次翻倍且不超上限。
	if d := complRetryBackoff(1); d != submitRetry {
		t.Errorf("complRetryBackoff(1)=%v want %v", d, submitRetry)
	}
	if d := complRetryBackoff(2); d != 2*submitRetry {
		t.Errorf("complRetryBackoff(2)=%v want %v", d, 2*submitRetry)
	}
	if d := complRetryBackoff(complRetryMax); d > complRetryCap {
		t.Errorf("complRetryBackoff(%d)=%v 超过上限 %v", complRetryMax, d, complRetryCap)
	}
}

// TestDeviceComplRetryTransientWrite 写完成首次返回 -EAGAIN 时必须按原参数重提并最终成功。
func TestDeviceComplRetryTransientWrite(t *testing.T) {
	fr := newFakeRing(map[int]int64{0: -int64(syscall.EAGAIN)})
	dev := newFakeDevice(t, fr)

	data := make([]byte, layout.BlockSize)
	if err := dev.Append(context.Background(), 0, 0, layout.BlockSize, data); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if n := fr.overall(); n != 2 {
		t.Fatalf("提交次数=%d, want 2（1 次 EAGAIN + 1 次重提）", n)
	}
}

// TestDeviceComplRetryExhausted 完成侧持续瞬时错误时，重试到上限后仍按原语义上抛 errno。
func TestDeviceComplRetryExhausted(t *testing.T) {
	over := make(map[int]int64, complRetryMax+1)
	for i := 0; i <= complRetryMax; i++ {
		over[i] = -int64(syscall.EAGAIN)
	}
	fr := newFakeRing(over)
	dev := newFakeDevice(t, fr)

	err := dev.Append(context.Background(), 0, 0, layout.BlockSize, make([]byte, layout.BlockSize))
	if !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("Append err=%v, want EAGAIN", err)
	}
	if n := fr.overall(); n != 1+complRetryMax {
		t.Fatalf("提交次数=%d, want %d（首次 + 上限次重试）", n, 1+complRetryMax)
	}
}

// TestDeviceComplRetryBatchWrite 批写中单条完成瞬时错误时，应只重提该条且不重复计入尺寸统计。
func TestDeviceComplRetryBatchWrite(t *testing.T) {
	fr := newFakeRing(map[int]int64{1: -int64(syscall.EAGAIN)}) // 批内第 2 条
	dev := newFakeDevice(t, fr)

	jobs := []WriteJob{
		{SegmentID: 0, Off: 0, Data: make([]byte, layout.BlockSize), Size: layout.BlockSize},
		{SegmentID: 0, Off: layout.BlockSize, Data: make([]byte, layout.BlockSize), Size: layout.BlockSize},
	}
	if err := dev.AppendBatch(context.Background(), jobs); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if n := fr.overall(); n != 3 {
		t.Fatalf("提交次数=%d, want 3（批内 2 条 + 重提 1 条）", n)
	}
	// 重提不重复统计：两次 4K 逻辑 IO 应记为 ioOther=2。
	_, ioOther, _, _ := dev.Stats()
	if ioOther != 2 {
		t.Fatalf("ioOther=%d, want 2（重提不得重复计入尺寸统计）", ioOther)
	}
}

// TestDeviceComplRetryTransientRead 读完成首次返回 -EAGAIN 时同样重提并成功返回。
func TestDeviceComplRetryTransientRead(t *testing.T) {
	fr := newFakeRing(map[int]int64{0: -int64(syscall.EAGAIN)})
	dev := newFakeDevice(t, fr)

	buf, err := dev.ReadAt(context.Background(), 0, 0, layout.BlockSize)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if int64(len(buf)) != layout.BlockSize {
		t.Fatalf("ReadAt 返回 %d 字节, want %d", len(buf), layout.BlockSize)
	}
	if n := fr.overall(); n != 2 {
		t.Fatalf("提交次数=%d, want 2（1 次 EAGAIN + 1 次重提）", n)
	}
}

// TestDeviceComplRetryBatchRead 批读中单条完成瞬时错误时，应重提该条并返回请求窗口。
func TestDeviceComplRetryBatchRead(t *testing.T) {
	fr := newFakeRing(map[int]int64{1: -int64(syscall.EAGAIN)}) // 批内第 2 条
	dev := newFakeDevice(t, fr)

	jobs := []ReadJob{
		{SegmentID: 0, Off: 0, Buf: bufpool.Get(int(layout.BlockSize)), Size: layout.BlockSize},
		{SegmentID: 0, Off: layout.BlockSize, Buf: bufpool.Get(int(layout.BlockSize)), Size: layout.BlockSize},
	}
	t.Cleanup(func() {
		for i := range jobs {
			bufpool.Put(jobs[i].Buf)
		}
	})
	ns, err := dev.ReadAtIntoBatch(context.Background(), jobs)
	if err != nil {
		t.Fatalf("ReadAtIntoBatch: %v", err)
	}
	for i, n := range ns {
		if n != layout.BlockSize {
			t.Fatalf("jobs[%d] 读入 %d 字节, want %d", i, n, layout.BlockSize)
		}
	}
	if n := fr.overall(); n != 3 {
		t.Fatalf("提交次数=%d, want 3（批内 2 条 + 重提 1 条）", n)
	}
}
