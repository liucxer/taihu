package device

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/liucxer/taihu/internal/aio"
	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/ierr"
)

// covSegSize 本文件各测试用的段大小（64MiB）：既容得下 4MiB 整块 IO 与多段偏移，
// 又让临时目录下的稀疏测试文件保持小体积。
const covSegSize int64 = 64 << 20

// newCovDevice 在 t.TempDir() 上建一个空文件作为段容器并创建 Device。
// 临时目录由 TMPDIR 决定（测试须指向支持 O_DIRECT 的 xfs，而非 tmpfs），
// 测试结束自动 Close。
func newCovDevice(t *testing.T, opts ...Option) *Device {
	t.Helper()
	devPath := filepath.Join(t.TempDir(), "nvme.img")
	f, err := os.Create(devPath)
	if err != nil {
		t.Fatalf("create backing file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close backing file: %v", err)
	}
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
	devPath := filepath.Join(t.TempDir(), "nvme.img")
	f, err := os.Create(devPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

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

// TestDeviceClosedOps 覆盖各提交入口在设备已关闭时的 errDeviceClosed 快路径。
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

	if err := dev.Append(ctx, 0, 0, 4096, blk); !errors.Is(err, errDeviceClosed) {
		t.Fatalf("关闭后 Append: %v, want errDeviceClosed", err)
	}
	if _, err := dev.ReadAt(ctx, 0, 0, 4096); !errors.Is(err, errDeviceClosed) {
		t.Fatalf("关闭后 ReadAt: %v, want errDeviceClosed", err)
	}
	if _, err := dev.ReadAtInto(ctx, 0, 0, 4096, dst); !errors.Is(err, errDeviceClosed) {
		t.Fatalf("关闭后 ReadAtInto: %v, want errDeviceClosed", err)
	}
	if err := dev.AppendBatch(ctx, []WriteJob{{SegmentID: 0, Off: 0, Data: blk, Size: 4096}}); !errors.Is(err, errDeviceClosed) {
		t.Fatalf("关闭后 AppendBatch: %v, want errDeviceClosed", err)
	}
	if _, err := dev.ReadAtIntoBatch(ctx, []ReadJob{{SegmentID: 0, Off: 0, Buf: dst, Size: 4096}}); !errors.Is(err, errDeviceClosed) {
		t.Fatalf("关闭后 ReadAtIntoBatch: %v, want errDeviceClosed", err)
	}
	// 完成泵已退出：batchWait 须在 pumpDone 上立即返回，不永久阻塞。
	if _, err := dev.batchWait(1, 1); !errors.Is(err, errDeviceClosed) {
		t.Fatalf("泵退出后 batchWait: %v, want errDeviceClosed", err)
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

	// append-only：Delete 为占位，须返回 nil。
	if err := dev.Delete(ctx, 0); err != nil {
		t.Fatalf("Delete: %v", err)
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
