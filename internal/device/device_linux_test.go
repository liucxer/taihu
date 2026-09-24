//go:build linux

// 本文件是 internal/device 的**全部 Linux 测试**，按「一个平台一个文件」组织：
// 跨平台中立用例（原 device_test.go / device_more_test.go / device_compl_retry_test.go
// 合并）与 Linux 专属用例（O_DIRECT 方向错误注入、裸设备容量查询）都在此；
// 非 Linux 侧见 device_other_test.go。
package device

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/aio"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/pkg/bufpool"
	"github.com/liucxer/taihu/pkg/ierr"
)

func TestDeviceAppendAlignment(t *testing.T) {
	devPath := newDevBackingFile(t)

	dev, err := NewDevice(context.Background(), devPath, layout.DefaultSegmentSizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()

	if err := dev.Append(context.Background(), 0, 0, 3, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	// off 须推进到 4K 对齐
	if err := dev.Append(context.Background(), 0, 4096, 10, make([]byte, 10)); err != nil {
		t.Fatalf("aligned append: %v", err)
	}
	// 非 4K 对齐 offset 应报错
	if err := dev.Append(context.Background(), 0, 100, 10, make([]byte, 10)); err == nil {
		t.Fatalf("unaligned offset should error")
	}
	// 数据不足 size 应报错
	if err := dev.Append(context.Background(), 0, 8192, 10, make([]byte, 5)); err == nil {
		t.Fatalf("short data should error")
	}

	// 对齐整块读回：4096 字节里前 3 字节应为 "abc"，其余为补零（ReadAt 返回池化对齐缓冲）
	data, err := dev.ReadAt(context.Background(), 0, 0, 4096)
	if err != nil {
		t.Fatalf("device read: %v", err)
	}
	defer bufpool.Put(data)
	if len(data) != 4096 {
		t.Fatalf("device read got %dB want 4096", len(data))
	}
	if string(data[:3]) != "abc" {
		t.Fatalf("device read prefix got %q", data[:3])
	}
	for _, v := range data[3:] {
		if v != 0 {
			t.Fatalf("device read non-zero padding at byte 3: %d", v)
		}
	}
	// 读第二段（偏移 4096 处的 10 字节内容）
	data2, err := dev.ReadAt(context.Background(), 0, 4096, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer bufpool.Put(data2)
	for _, v := range data2[:10] {
		if v != 0 {
			t.Fatalf("second append misalign: %d", v)
		}
	}
}

func TestDeviceAppendAlignedFastPath(t *testing.T) {
	devPath := newDevBackingFile(t)

	dev, err := NewDevice(context.Background(), devPath, layout.DefaultSegmentSizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()

	// 4K 对齐地址 + 4K 倍数长度 → 命中直写快路径（bufpool.Get 保证首地址 4K 对齐）。
	data := bufpool.Get(int(4096))
	defer bufpool.Put(data)
	for i := range data[:4096] {
		data[i] = byte(i)
	}
	if err := dev.Append(context.Background(), 0, 0, 4096, data[:4096]); err != nil {
		t.Fatalf("aligned fast-path append: %v", err)
	}

	// 读回对比：快路径直写内容须与源一致（无补零、无错位）。
	got, err := dev.ReadAt(context.Background(), 0, 0, 4096)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer bufpool.Put(got)
	if !bytes.Equal(got[:4096], data[:4096]) {
		t.Fatal("aligned fast-path round trip mismatch")
	}

	// 非快路径（地址/长度不同时为 4K 对齐）仍走拷贝兜底，数据须一致。
	unaligned := make([]byte, 4096)
	for i := range unaligned {
		unaligned[i] = byte(0xff - i)
	}
	if err := dev.Append(context.Background(), 0, 8192, 4096, unaligned); err != nil {
		t.Fatalf("fallback append: %v", err)
	}
	got2, err := dev.ReadAt(context.Background(), 0, 8192, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer bufpool.Put(got2)
	if !bytes.Equal(got2[:4096], unaligned) {
		t.Fatal("fallback round trip mismatch")
	}

	// 对齐地址 + 非 4K 倍数长度（"4M+1k" 缩小版）：主体直写 + 仅 4K 尾块缓冲，数据与补零须正确。
	obj := bufpool.Get(16384)
	defer bufpool.Put(obj)
	payload := obj[:12288+100] // 12388 B：12288 对齐主体 + 100 字节尾块
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	if err := dev.Append(context.Background(), 0, 16384, int64(len(payload)), payload); err != nil {
		t.Fatalf("aligned bulk+tail append: %v", err)
	}
	got3, err := dev.ReadAt(context.Background(), 0, 16384, 16384)
	if err != nil {
		t.Fatal(err)
	}
	defer bufpool.Put(got3)
	if !bytes.Equal(got3[:len(payload)], payload) {
		t.Fatal("aligned bulk+tail payload mismatch")
	}
	for _, v := range got3[len(payload):16384] {
		if v != 0 {
			t.Fatal("aligned bulk+tail tail padding not zero")
		}
	}
}

// TestDeviceConcurrent 多 goroutine 异偏移并发 Append/ReadAt，校验完成泵分发正确性（-race）。
func TestDeviceConcurrent(t *testing.T) {
	devPath := newDevBackingFile(t)

	dev, err := NewDevice(context.Background(), devPath, layout.DefaultSegmentSizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()

	const n = 32
	const chunk = 4096
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := bufpool.Get(chunk)
			defer bufpool.Put(data)
			for j := range data {
				data[j] = byte(i)
			}
			if err := dev.Append(context.Background(), 0, int64(i)*chunk, chunk, data); err != nil {
				errs[i] = err
				return
			}
			got, err := dev.ReadAt(context.Background(), 0, int64(i)*chunk, chunk)
			if err != nil {
				errs[i] = err
				return
			}
			defer bufpool.Put(got)
			if !bytes.Equal(got, data) {
				errs[i] = fmt.Errorf("chunk %d mismatch", i)
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("worker %d: %v", i, e)
		}
	}
}

// TestDeviceCloseInflight 在途请求存在时 Close 须排空完成事件后返回，不挂起。
func TestDeviceCloseInflight(t *testing.T) {
	devPath := newDevBackingFile(t)

	dev, err := NewDevice(context.Background(), devPath, layout.DefaultSegmentSizeBytes)
	if err != nil {
		t.Fatal(err)
	}

	// 4MB 写：O_SYNC（非 Linux 打开方式）下耗时足够，可稳定被观测为在途。
	data := bufpool.Get(4 << 20)
	defer bufpool.Put(data)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = dev.Append(context.Background(), 0, 0, 4<<20, data)
	}()

	deadline := time.Now().Add(5 * time.Second)
poll:
	for {
		select {
		case <-done:
			break poll // 已完成也直接测 Close
		default:
		}
		dev.mu.Lock()
		inflight := len(dev.m) > 0
		dev.mu.Unlock()
		if inflight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("submit never registered")
		}
		time.Sleep(time.Millisecond)
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("close with inflight: %v", err)
	}
	<-done
}

// covSegSize 本文件各测试用的段大小（64MiB）：既容得下 4MiB 整块 IO 与多段偏移，
// 又让临时目录下的稀疏测试文件保持小体积。
const covSegSize int64 = 64 << 20

// newCovDevice 建一个空模拟设备文件（目录经 devBackingDir 选择，Linux 需支持
// O_DIRECT）并创建 Device，测试结束自动 Close。
func newCovDevice(t *testing.T, opts ...Option) *Device {
	t.Helper()
	devPath := newDevBackingFile(t)
	dev, err := NewDevice(context.Background(), devPath, covSegSize, opts...)
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}
	t.Cleanup(func() { _ = dev.Close() })
	return dev
}

// fillPattern 用确定性字节序列填充 b（比全 0 更能暴露错位/短写）。
func fillPattern(b []byte, seed int) {
	for i := range b {
		b[i] = byte(seed + i*7)
	}
}

// readBack 读回段内 off 起 size 字节（均须 4K 对齐），校验 want 前缀一致、
// 其余部分为补零。
func readBack(t *testing.T, dev *Device, segmentID, off, size int64, want []byte) {
	t.Helper()
	got, err := dev.ReadAt(context.Background(), segmentID, off, size)
	if err != nil {
		t.Fatalf("ReadAt(seg=%d off=%d size=%d): %v", segmentID, off, size, err)
	}
	defer bufpool.Put(got)
	if int64(len(got)) != size {
		t.Fatalf("ReadAt(seg=%d off=%d) 长度=%d want %d", segmentID, off, len(got), size)
	}
	if !bytes.Equal(got[:len(want)], want) {
		t.Fatalf("ReadAt(seg=%d off=%d) 数据不匹配", segmentID, off)
	}
	for i := len(want); i < len(got); i++ {
		if got[i] != 0 {
			t.Fatalf("ReadAt(seg=%d off=%d) 补零区第 %d 字节非零: %d", segmentID, off, i, got[i])
		}
	}
}

func TestNewDeviceOpenError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-device.img")
	if _, err := NewDevice(context.Background(), missing, covSegSize); err == nil {
		t.Fatal("打开不存在的设备应报错")
	}
}

func TestNewDeviceOptions(t *testing.T) {
	devPath := newDevBackingFile(t)

	// libaio 后端 + 打开 IOPOLL：IOPOLL 只对 io_uring 生效，libaio 下被忽略，应打开成功。
	dev, err := NewDevice(context.Background(), devPath, covSegSize,
		WithAIOMode(aio.ModeLibAIO), WithAIOIOPoll(true))
	if err != nil {
		t.Fatalf("libaio + iopoll 选项: %v", err)
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 强制 io_uring + IOPOLL：普通文件不在 /sys/class/block 下，前置校验必须拒绝。
	if _, err := NewDevice(context.Background(), devPath, covSegSize,
		WithAIOMode(aio.ModeIOUring), WithAIOIOPoll(true)); err == nil {
		t.Fatal("IOPOLL 前置校验对普通文件应失败")
	}
}

// TestCloseIdempotent 重复 Close 须返回 nil（第二次走已关闭快路径）。
func TestCloseIdempotent(t *testing.T) {
	dev := newCovDevice(t)
	if err := dev.Close(); err != nil {
		t.Fatalf("首次 Close: %v", err)
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
}

// TestDeviceClosedOps 覆盖各提交入口在设备已关闭时的 ierr.ErrDeviceClosed 快路径。
func TestDeviceClosedOps(t *testing.T) {
	dev := newCovDevice(t)
	// 只置关闭标志并等完成泵退出（不关 fd），使提交入口命中「已关闭」检查。
	dev.mu.Lock()
	dev.closed = true
	dev.mu.Unlock()
	<-dev.pumpDone

	ctx := context.Background()
	blk := bufpool.Get(4096)
	defer bufpool.Put(blk)
	dst := bufpool.Get(4096)
	defer bufpool.Put(dst)

	if err := dev.Append(ctx, 0, 0, 4096, blk); !errors.Is(err, ierr.ErrDeviceClosed) {
		t.Fatalf("关闭后 Append: %v, want ierr.ErrDeviceClosed", err)
	}
	if _, err := dev.ReadAt(ctx, 0, 0, 4096); !errors.Is(err, ierr.ErrDeviceClosed) {
		t.Fatalf("关闭后 ReadAt: %v, want ierr.ErrDeviceClosed", err)
	}
	if _, err := dev.ReadAtInto(ctx, 0, 0, 4096, dst); !errors.Is(err, ierr.ErrDeviceClosed) {
		t.Fatalf("关闭后 ReadAtInto: %v, want ierr.ErrDeviceClosed", err)
	}
	if err := dev.AppendBatch(ctx, []WriteJob{{SegmentID: 0, Off: 0, Data: blk, Size: 4096}}); !errors.Is(err, ierr.ErrDeviceClosed) {
		t.Fatalf("关闭后 AppendBatch: %v, want ierr.ErrDeviceClosed", err)
	}
	if _, err := dev.ReadAtIntoBatch(ctx, []ReadJob{{SegmentID: 0, Off: 0, Buf: dst, Size: 4096}}); !errors.Is(err, ierr.ErrDeviceClosed) {
		t.Fatalf("关闭后 ReadAtIntoBatch: %v, want ierr.ErrDeviceClosed", err)
	}
	// 完成泵已退出：batchWait 须在 pumpDone 上立即返回，不永久阻塞。
	if _, err := dev.batchWait(1, 1); !errors.Is(err, ierr.ErrDeviceClosed) {
		t.Fatalf("泵退出后 batchWait: %v, want ierr.ErrDeviceClosed", err)
	}
}

func TestAppendParamErrors(t *testing.T) {
	dev := newCovDevice(t)
	ctx := context.Background()
	blk := bufpool.Get(4096)
	defer bufpool.Put(blk)

	if err := dev.Append(ctx, 0, -4096, 4096, blk); err == nil {
		t.Fatal("负 offset 应报错")
	}
	if err := dev.Append(ctx, 0, 1, 4096, blk); err == nil {
		t.Fatal("非 4K 对齐 offset 应报错")
	}
	if err := dev.Append(ctx, 0, covSegSize, 4096, blk); !errors.Is(err, ierr.ErrTooLarge) {
		t.Fatalf("超出段大小: %v, want ierr.ErrTooLarge", err)
	}
	if err := dev.Append(ctx, 0, 0, 4096, blk[:10]); err == nil {
		t.Fatal("数据不足 size 应报错")
	}
	// size == 0：无写 IO，直接成功。
	if err := dev.Append(ctx, 0, 0, 0, nil); err != nil {
		t.Fatalf("size=0 Append: %v", err)
	}
}

func TestReadAtParamErrors(t *testing.T) {
	dev := newCovDevice(t)
	ctx := context.Background()

	if _, err := dev.ReadAt(ctx, 0, -4096, 4096); err == nil {
		t.Fatal("负 offset 应报错")
	}
	if _, err := dev.ReadAt(ctx, 0, 1, 4096); err == nil {
		t.Fatal("非 4K 对齐 offset 应报错")
	}
	if _, err := dev.ReadAt(ctx, 0, 0, -4096); err == nil {
		t.Fatal("负 size 应报错")
	}
	if _, err := dev.ReadAt(ctx, 0, 0, 1); err == nil {
		t.Fatal("非 4K 对齐 size 应报错")
	}
	if buf, err := dev.ReadAt(ctx, 0, 0, 0); err != nil || buf != nil {
		t.Fatalf("size=0 ReadAt: buf=%v err=%v, want nil/nil", buf, err)
	}
	// 未写过任何数据：段内偏移超过文件长度 → 短读 0 字节 → io.EOF。
	if _, err := dev.ReadAt(ctx, 3, 4096, 4096); !errors.Is(err, io.EOF) {
		t.Fatalf("越过文件末尾 ReadAt: %v, want io.EOF", err)
	}
}

func TestReadAtIntoParamErrors(t *testing.T) {
	dev := newCovDevice(t)
	ctx := context.Background()

	src := bufpool.Get(12288)
	defer bufpool.Put(src)
	fillPattern(src[:12288], 5)
	if err := dev.Append(ctx, 0, 0, 12288, src[:12288]); err != nil {
		t.Fatalf("Append: %v", err)
	}

	dst := bufpool.Get(8192)
	defer bufpool.Put(dst)

	n, err := dev.ReadAtInto(ctx, 0, 0, 4096, dst[:4096])
	if err != nil || n != 4096 {
		t.Fatalf("ReadAtInto: n=%d err=%v", n, err)
	}
	if !bytes.Equal(dst[:4096], src[:4096]) {
		t.Fatal("ReadAtInto 读到的内容不匹配")
	}

	if _, err := dev.ReadAtInto(ctx, 0, 1, 4096, dst); err == nil {
		t.Fatal("非 4K 对齐 offset 应报错")
	}
	if _, err := dev.ReadAtInto(ctx, 0, 0, 1, dst); err == nil {
		t.Fatal("非 4K 对齐 size 应报错")
	}
	if n, err := dev.ReadAtInto(ctx, 0, 0, 0, nil); err != nil || n != 0 {
		t.Fatalf("size=0 ReadAtInto: n=%d err=%v", n, err)
	}
	if _, err := dev.ReadAtInto(ctx, 0, 0, 8192, dst[:4096]); err == nil {
		t.Fatal("dst 小于 size 应报错")
	}
	if _, err := dev.ReadAtInto(ctx, 0, 0, 4096, dst[1:]); err == nil {
		t.Fatal("dst 首地址未 4K 对齐应报错")
	}
	// 未写过的段：读完为 0 字节 → io.EOF。
	if _, err := dev.ReadAtInto(ctx, 5, 0, 4096, dst[:4096]); !errors.Is(err, io.EOF) {
		t.Fatalf("越过文件末尾 ReadAtInto: %v, want io.EOF", err)
	}
}

// TestAppendBatchRoundTrip 一次批量提交多条段内写：对齐主体/对齐主体+尾块/
// 首地址不对齐兜底/异段/size=0 五类项混合，逐项读回校验内容与补零。
func TestAppendBatchRoundTrip(t *testing.T) {
	dev := newCovDevice(t)
	ctx := context.Background()

	blk := bufpool.Get(4096)
	defer bufpool.Put(blk)
	fillPattern(blk[:4096], 3)

	bulk := bufpool.Get(16384)
	defer bufpool.Put(bulk)
	payload := bulk[:12288+100] // 12288 对齐主体 + 100 字节尾块
	fillPattern(payload, 11)

	raw := bufpool.Get(8192)
	defer bufpool.Put(raw)
	tail := raw[1 : 1+4096+100] // 首地址不对齐 → 整体拷入对齐缓冲兜底
	fillPattern(tail, 29)

	jobs := []WriteJob{
		{SegmentID: 0, Off: 0, Data: blk[:4096], Size: 4096},
		{SegmentID: 0, Off: 8192, Data: payload, Size: int64(len(payload))},
		{SegmentID: 0, Off: 24576, Data: tail, Size: int64(len(tail))},
		{SegmentID: 1, Off: 0, Data: blk[:4096], Size: 4096},
		{SegmentID: 0, Off: 40960, Size: 0}, // size==0：无写 IO
	}
	if err := dev.AppendBatch(ctx, jobs); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	readBack(t, dev, 0, 0, 4096, blk[:4096])
	readBack(t, dev, 0, 8192, 16384, payload)
	readBack(t, dev, 0, 24576, 8192, tail)
	readBack(t, dev, 1, 0, 4096, blk[:4096])

	// 空批与全零长批都不产生写 IO，直接成功返回。
	if err := dev.AppendBatch(ctx, nil); err != nil {
		t.Fatalf("空批: %v", err)
	}
	if err := dev.AppendBatch(ctx, []WriteJob{{SegmentID: 0, Off: 0, Size: 0}}); err != nil {
		t.Fatalf("全零长批: %v", err)
	}
}

func TestAppendBatchErrors(t *testing.T) {
	dev := newCovDevice(t)
	ctx := context.Background()
	blk := bufpool.Get(4096)
	defer bufpool.Put(blk)

	cases := []struct {
		name string
		jobs []WriteJob
		want error
	}{
		{"off 非 4K 对齐",
			[]WriteJob{{SegmentID: 0, Off: 1, Data: blk[:4096], Size: 4096}}, nil},
		{"数据不足 size",
			[]WriteJob{{SegmentID: 0, Off: 0, Data: blk[:100], Size: 4096}}, nil},
		{"超出段大小",
			[]WriteJob{{SegmentID: 0, Off: covSegSize, Data: blk[:4096], Size: 4096}}, ierr.ErrTooLarge},
		{"批内第二项非法则整批失败",
			[]WriteJob{
				{SegmentID: 0, Off: 0, Data: blk[:4096], Size: 4096},
				{SegmentID: 0, Off: 8192, Data: blk[:10], Size: 4096},
			}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := dev.AppendBatch(ctx, c.jobs)
			if err == nil {
				t.Fatal("期望报错，实际为 nil")
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
	// 非法批失败后设备仍可用：正常批须成功。
	if err := dev.AppendBatch(ctx, []WriteJob{{SegmentID: 0, Off: 0, Data: blk[:4096], Size: 4096}}); err != nil {
		t.Fatalf("错误批之后正常批应成功: %v", err)
	}
}

// TestReadAtIntoBatch 覆盖批直读的三条路径：整批直读、混合批回退单条、
// 全非法批逐条回退，以及短读与空批。
func TestReadAtIntoBatch(t *testing.T) {
	dev := newCovDevice(t)
	ctx := context.Background()

	src := bufpool.Get(16384)
	defer bufpool.Put(src)
	fillPattern(src[:12288], 5)
	if err := dev.Append(ctx, 0, 0, 12288, src[:12288]); err != nil {
		t.Fatalf("Append: %v", err)
	}

	b1 := bufpool.Get(4096)
	defer bufpool.Put(b1)
	b2 := bufpool.Get(4096)
	defer bufpool.Put(b2)
	b3 := bufpool.Get(4096)
	defer bufpool.Put(b3)

	// 批路径：三项均满足直读快路径。
	ns, err := dev.ReadAtIntoBatch(ctx, []ReadJob{
		{SegmentID: 0, Off: 0, Buf: b1[:4096], Size: 4096},
		{SegmentID: 0, Off: 4096, Buf: b2[:4096], Size: 4096},
		{SegmentID: 0, Off: 8192, Buf: b3[:4096], Size: 4096},
	})
	if err != nil {
		t.Fatalf("整批直读: %v", err)
	}
	if len(ns) != 3 || ns[0] != 4096 || ns[1] != 4096 || ns[2] != 4096 {
		t.Fatalf("整批直读 ns=%v", ns)
	}
	if !bytes.Equal(b1[:4096], src[:4096]) || !bytes.Equal(b2[:4096], src[4096:8192]) {
		t.Fatal("整批直读内容不匹配")
	}
	if !bytes.Equal(b3[:4096], src[8192:12288]) {
		t.Fatal("整批直读第 3 项内容不匹配")
	}

	// 空批：返回空切片，不提交任何 IO。
	if ns, err := dev.ReadAtIntoBatch(ctx, nil); err != nil || len(ns) != 0 {
		t.Fatalf("空批: ns=%v err=%v", ns, err)
	}

	// 未写过的段：批读完成事件 Res==0 → 该项记 0 字节，不报错。
	if ns, err = dev.ReadAtIntoBatch(ctx, []ReadJob{{SegmentID: 6, Off: 0, Buf: b1[:4096], Size: 4096}}); err != nil {
		t.Fatalf("短读批: %v", err)
	} else if len(ns) != 1 || ns[0] != 0 {
		t.Fatalf("短读批 ns=%v, want [0]", ns)
	}

	// 混合批：非法项回退单条 ReadAtInto（此处 off 非 4K 对齐 → 报错返回）。
	ns, err = dev.ReadAtIntoBatch(ctx, []ReadJob{
		{SegmentID: 0, Off: 0, Buf: b1[:4096], Size: 4096},
		{SegmentID: 0, Off: 1, Buf: b2[:4096], Size: 4096},
	})
	if err == nil {
		t.Fatal("混合批中的非法项应报错")
	}
	if len(ns) != 2 {
		t.Fatalf("混合批返回项数=%d want 2", len(ns))
	}

	// 全非法批：走逐条回退（Size<=0 项记 0；off<0 项报错返回）。
	ns, err = dev.ReadAtIntoBatch(ctx, []ReadJob{
		{SegmentID: 0, Off: 0, Buf: nil, Size: 0},
		{SegmentID: 0, Off: -4096, Buf: b1[:4096], Size: 4096},
	})
	if err == nil {
		t.Fatal("off<0 项应报错")
	}
	if len(ns) != 2 || ns[0] != 0 {
		t.Fatalf("全非法批 ns=%v, want [0 0]", ns)
	}

	// 逐条回退全部为 size==0：都记 0 字节且不报错。
	if ns, err = dev.ReadAtIntoBatch(ctx, []ReadJob{{SegmentID: 0, Off: 0, Size: 0}}); err != nil {
		t.Fatalf("全零长批: %v", err)
	} else if len(ns) != 1 || ns[0] != 0 {
		t.Fatalf("全零长批 ns=%v, want [0]", ns)
	}
}

// TestDeviceStatsAndDelete 校验 4MiB 整块 IO 与 其他尺寸 IO 的尺寸分档统计。
func TestDeviceStatsAndDelete(t *testing.T) {
	dev := newCovDevice(t)
	ctx := context.Background()

	big := bufpool.Get(chunk4MiB)
	defer bufpool.Put(big)
	fillPattern(big[:chunk4MiB], 7)
	if err := dev.Append(ctx, 0, 0, int64(chunk4MiB), big[:chunk4MiB]); err != nil {
		t.Fatalf("4MiB Append: %v", err)
	}
	blk := bufpool.Get(4096)
	defer bufpool.Put(blk)
	if err := dev.Append(ctx, 0, int64(chunk4MiB), 4096, blk[:4096]); err != nil {
		t.Fatalf("4K Append: %v", err)
	}

	io4M, ioOther, bytes4M, bytesOther := dev.Stats()
	if io4M != 1 || bytes4M != chunk4MiB {
		t.Fatalf("4MiB 档统计 io4M=%d bytes4M=%d, want 1/%d", io4M, bytes4M, chunk4MiB)
	}
	if ioOther != 1 || bytesOther != 4096 {
		t.Fatalf("其他档统计 ioOther=%d bytesOther=%d, want 1/4096", ioOther, bytesOther)
	}
}

// TestSegmentBaseAndBufAligned 覆盖段基址换算与 4K 对齐判定（含空切片/错位切片）。
func TestSegmentBaseAndBufAligned(t *testing.T) {
	dev := newCovDevice(t)
	if got := dev.segmentBase(3); got != 3*covSegSize {
		t.Fatalf("segmentBase(3)=%d want %d", got, 3*covSegSize)
	}
	if bufAligned(nil) {
		t.Fatal("bufAligned(nil) 应为 false")
	}
	b := bufpool.Get(8192)
	defer bufpool.Put(b)
	if !bufAligned(b) {
		t.Fatal("bufpool 缓冲应 4K 对齐")
	}
	if bufAligned(b[1:]) {
		t.Fatal("错位 1 字节的切片不应判为对齐")
	}
}

// TestCheckWrite 覆盖写完成事件校验的三个分支：正常、-errno、短写。
func TestCheckWrite(t *testing.T) {
	if err := checkWrite(aio.Event{Res: 4096}, 4096); err != nil {
		t.Fatalf("正常事件: %v", err)
	}
	err := checkWrite(aio.Event{Res: -13}, 4096) // -EACCES
	if err == nil {
		t.Fatal("-errno 事件应返回错误")
	}
	if err.Error() != "permission denied" {
		t.Fatalf("-errno 事件错误信息=%q, want permission denied", err.Error())
	}
	if err := checkWrite(aio.Event{Res: 10}, 4096); err == nil {
		t.Fatal("短写应返回错误")
	}
}

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

func (f *fakeRing) SubmitRead(buf []byte, off int64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueue(buf)
	return f.seq, nil
}

func (f *fakeRing) SubmitWrite(buf []byte, off int64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueue(buf)
	return f.seq, nil
}

func (f *fakeRing) SubmitReadBatch(specs []aio.ReadSpec) (uint64, int, error) {
	return f.batch(len(specs), func(i int) []byte { return specs[i].Buf })
}

func (f *fakeRing) SubmitWriteBatch(specs []aio.WriteSpec) (uint64, int, error) {
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
		return nil, ierr.ErrTimeout
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
	devPath := newDevBackingFile(t)
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

// TestDeviceIOErrorPaths 把 ring 构造时绑定的 fd 换成「方向不符」的句柄（只写/只读）。
//
// fd 下沉之后（构造签名 newIOUringRing(devFD,…) / newLibAIORing(fd,…)），设备的所有
// IO 统一走**构造时绑定的 fd**，运行期改设备自己的句柄不再影响提交 —— 所以这里直接在
// aio 层用错误方向的 fd 建 ring，使方向不符的 IO 以完成事件 Res<0 失败，覆盖各 IO
// 入口的错误分支。
func TestDeviceIOErrorPaths(t *testing.T) {
	ctx := context.Background()
	blk := bufpool.Get(4096)
	defer bufpool.Put(blk)
	raw := bufpool.Get(8192)
	defer bufpool.Put(raw)
	dst := bufpool.Get(4096)
	defer bufpool.Put(dst)

	// 只写 fd 上读：ReadAt / ReadAtInto 必失败（读被只写句柄拒绝）。
	devPath := newDevBackingFile(t)
	wo, err := os.OpenFile(devPath, os.O_WRONLY|syscall.O_DIRECT, 0)
	if err != nil {
		t.Skipf("O_DIRECT 只写打开不可用: %v", err)
	}
	dev := newDeviceWithRing(t, devPath, newRingOnFD(t, wo.Fd(), "只写 fd"))
	if _, err := dev.ReadAt(ctx, 0, 0, 4096); err == nil {
		t.Fatal("只写 fd 上 ReadAt 应失败")
	}
	if _, err := dev.ReadAtInto(ctx, 0, 0, 4096, dst); err == nil {
		t.Fatal("只写 fd 上 ReadAtInto 应失败")
	}
	_ = wo.Close()

	// 只读 fd 上写：Append 两条路径（4K 对齐直写 / 首地址不对齐拷贝兜底）均失败。
	ro, err := os.OpenFile(devPath, os.O_RDONLY|syscall.O_DIRECT, 0)
	if err != nil {
		t.Skipf("O_DIRECT 只读打开不可用: %v", err)
	}
	dev = newDeviceWithRing(t, devPath, newRingOnFD(t, ro.Fd(), "只读 fd"))
	if err := dev.Append(ctx, 0, 0, 4096, blk[:4096]); err == nil {
		t.Fatal("只读 fd 上 Append（对齐主体直写）应失败")
	}
	if err := dev.Append(ctx, 0, 8192, 4096, raw[1:4097]); err == nil {
		t.Fatal("只读 fd 上 Append（首地址不对齐拷贝兜底）应失败")
	}
	_ = ro.Close()
}

// newRingOnFD 在原 fd 上建 aio ring（构造时绑定 fd；供方向不符的错误注入用）。
func newRingOnFD(t *testing.T, fd uintptr, what string) aio.Ring {
	t.Helper()
	r, err := aio.NewWithOptions(aio.Options{Mode: aio.ModeAuto, MaxEvents: 4, FD: int(fd)}, "")
	if err != nil {
		t.Fatalf("在 %s 上建 aio ring: %v", what, err)
	}
	return r
}

// newDeviceWithRing 用给定 ring 建 Device（文件真实存在即可，IO 由 ring 及其绑定的 fd 决定）。
func newDeviceWithRing(t *testing.T, devPath string, r aio.Ring) *Device {
	t.Helper()
	dev, err := NewDevice(context.Background(), devPath, covSegSize, withRing(r))
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}
	t.Cleanup(func() { _ = dev.Close() })
	return dev
}

// TestDeviceCapacity 覆盖裸设备容量查询的错误路径：路径不存在、以及普通文件
// 不具备 BLKGETSIZE64（内核返回 ENOTTY）。成功路径需要真实块设备，单测环境
// （xfs 上的普通文件）无法构造。
func TestDeviceCapacity(t *testing.T) {
	if _, err := DeviceCapacity(filepath.Join(t.TempDir(), "no-such.img")); err == nil {
		t.Fatal("不存在的路径应报错")
	}

	// 普通文件不是块设备：BLKGETSIZE64 ioctl 须失败（不得退化成文件大小）。
	plain := filepath.Join(t.TempDir(), "plain.img")
	if err := os.WriteFile(plain, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if sz, err := DeviceCapacity(plain); err == nil {
		t.Fatalf("普通文件上 BLKGETSIZE64 应报错，实际返回 %d", sz)
	}
}

// devBackingDir 返回测试设备文件所在目录：Linux 上必须支持 O_DIRECT（生产形态；tmpfs
// 的 /tmp 不支持，open 会直接 EINVAL）。依次试 TAIHU_DEVICE_TEST_DIR（可显式指定，
// 如 xfs 挂载点）、t.TempDir()、当前工作目录；全部不支持则整条测试 t.Skipf ——
// 不静默退化成普通 IO（那会让「O_DIRECT 路径跑过了」变成假象）。
func devBackingDir(t *testing.T) string {
	t.Helper()
	var dirs []string
	if d := os.Getenv("TAIHU_DEVICE_TEST_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	dirs = append(dirs, t.TempDir())
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	for _, d := range dirs {
		probe := filepath.Join(d, ".device-odirect-probe")
		f, err := os.OpenFile(probe, os.O_RDWR|os.O_CREATE|syscall.O_DIRECT, 0o600)
		if err != nil {
			continue
		}
		_ = f.Close()
		_ = os.Remove(probe)
		return d
	}
	t.Skipf("没有支持 O_DIRECT 的文件系统（试过 %v）；可设 TAIHU_DEVICE_TEST_DIR 指定目录", dirs)
	return ""
}

// newDevBackingFile 建一个模拟设备文件（空）并返回路径：文件落在 devBackingDir
// 选出的、支持 O_DIRECT 的目录，使 NewDevice 的 O_DIRECT 打开可用。
func newDevBackingFile(t *testing.T) string {
	t.Helper()
	devPath := filepath.Join(devBackingDir(t), "nvme.img")
	f, err := os.Create(devPath)
	if err != nil {
		t.Fatalf("create backing file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close backing file: %v", err)
	}
	return devPath
}
