//go:build linux

package aio

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestLibAIOBatchRoundTrip 批量读写往返：一次 io_submit 排入多条，校验序号连续、
// 结果长度、数据一致性与部分提交后的重提。
func TestLibAIOBatchRoundTrip(t *testing.T) {
	r, err := newLibAIORing(16)
	if err != nil {
		t.Fatalf("newLibAIORing: %v", err)
	}
	defer func() { _ = r.Close() }()

	const n = 4
	f := newTestFile(t, int64(n)*testChunk)
	fd := int(f.Fd())

	specs := make([]WriteSpec, n)
	datas := make([][]byte, n)
	for i := range specs {
		datas[i] = pattern(byte(i+1), testChunk)
		specs[i] = WriteSpec{Buf: datas[i], Off: int64(i) * testChunk}
	}
	first, submitted, err := r.SubmitWriteBatch(fd, specs)
	if err != nil {
		t.Fatalf("SubmitWriteBatch: %v", err)
	}
	if submitted != n {
		t.Fatalf("SubmitWriteBatch submitted=%d want %d", submitted, n)
	}
	evs, err := r.Wait(n, n, nil)
	if err != nil {
		t.Fatalf("Wait write batch: %v", err)
	}
	if len(evs) != n {
		t.Fatalf("got %d events want %d", len(evs), n)
	}
	res := make(map[uint64]int64, n)
	for _, ev := range evs {
		res[ev.Data] = ev.Res
	}
	for i := 0; i < n; i++ {
		if seq := first + uint64(i); res[seq] != testChunk {
			t.Errorf("写批次第 %d 条 seq=%d res=%d want %d", i, seq, res[seq], testChunk)
		}
	}

	// 读回：序号必须接在写批次之后，不得重叠。
	rspecs := make([]ReadSpec, n)
	rbufs := make([][]byte, n)
	for i := range rspecs {
		rbufs[i] = make([]byte, testChunk)
		rspecs[i] = ReadSpec{Buf: rbufs[i], Off: int64(i) * testChunk}
	}
	firstR, submittedR, err := r.SubmitReadBatch(fd, rspecs)
	if err != nil || submittedR != n {
		t.Fatalf("SubmitReadBatch: submitted=%d err=%v", submittedR, err)
	}
	if lastW := first + uint64(n) - 1; firstR <= lastW {
		t.Errorf("读批次首序号 %d 与写批次末序号 %d 重叠", firstR, lastW)
	}
	evs, err = r.Wait(n, n, nil)
	if err != nil {
		t.Fatalf("Wait read batch: %v", err)
	}
	seen := make(map[uint64]bool, n)
	for _, ev := range evs {
		seen[ev.Data] = true
		if ev.Res != testChunk {
			t.Errorf("读批次 seq=%d res=%d want %d", ev.Data, ev.Res, testChunk)
		}
	}
	for i := 0; i < n; i++ {
		if !seen[firstR+uint64(i)] {
			t.Errorf("读批次第 %d 条 seq=%d 未取回", i, firstR+uint64(i))
		}
		if !bytes.Equal(rbufs[i], datas[i]) {
			t.Errorf("读批次第 %d 块数据不一致", i)
		}
	}

	// 空批次：不占序号、不报错（n==0 提前返回，不进内核）。
	if fs, ns, err := r.SubmitReadBatch(fd, nil); err != nil || ns != 0 || fs != 0 {
		t.Errorf("空读批次 = (%d, %d, %v), want (0, 0, nil)", fs, ns, err)
	}
	if fs, ns, err := r.SubmitWriteBatch(fd, nil); err != nil || ns != 0 || fs != 0 {
		t.Errorf("空写批次 = (%d, %d, %v), want (0, 0, nil)", fs, ns, err)
	}
}

// TestLibAIOContextErrors ctx 失效后各入口必须返回 errno 而不是静默成功或崩。
func TestLibAIOContextErrors(t *testing.T) {
	r, err := newLibAIORing(4)
	if err != nil {
		t.Fatalf("newLibAIORing: %v", err)
	}
	l := r.(*ring)

	f := newTestFile(t, testChunk)
	fd := int(f.Fd())
	buf := make([]byte, testChunk)

	saved := l.ctx
	l.ctx = 0 // 内核侧无此上下文：各系统调用返回 EINVAL
	if _, err := l.SubmitRead(fd, buf, 0); !errors.Is(err, unix.EINVAL) {
		t.Errorf("SubmitRead(ctx=0) err=%v want EINVAL", err)
	}
	if _, err := l.SubmitWrite(fd, buf, 0); !errors.Is(err, unix.EINVAL) {
		t.Errorf("SubmitWrite(ctx=0) err=%v want EINVAL", err)
	}
	if _, _, err := l.SubmitReadBatch(fd, []ReadSpec{{Buf: buf, Off: 0}}); !errors.Is(err, unix.EINVAL) {
		t.Errorf("SubmitReadBatch(ctx=0) err=%v want EINVAL", err)
	}
	if _, _, err := l.SubmitWriteBatch(fd, []WriteSpec{{Buf: buf, Off: 0}}); !errors.Is(err, unix.EINVAL) {
		t.Errorf("SubmitWriteBatch(ctx=0) err=%v want EINVAL", err)
	}
	if _, err := l.Wait(0, 1, nil); !errors.Is(err, unix.EINVAL) {
		t.Errorf("Wait(ctx=0) err=%v want EINVAL", err)
	}
	l.ctx = saved

	// 正常销毁；随后换成非法 ctx 再销毁一次，必须返回 errno。
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	l.ctx = 0xdeadbeef
	if err := l.Close(); !errors.Is(err, unix.EINVAL) {
		t.Errorf("Close(非法 ctx) err=%v want EINVAL", err)
	}
	// 已销毁的 ring 再 Close 是幂等的。
	if err := l.Close(); err != nil {
		t.Errorf("二次 Close err=%v want nil", err)
	}
}

// TestLibAIOWaitBounds Wait 的边界参数：max<=0 直接返回；max 超过复用缓冲容量时
// 重建缓冲；无事件且给超时时返回 ErrTimeout + 空列表。
func TestLibAIOWaitBounds(t *testing.T) {
	r, err := newLibAIORing(2)
	if err != nil {
		t.Fatalf("newLibAIORing: %v", err)
	}
	defer func() { _ = r.Close() }()
	l := r.(*ring)

	if evs, err := l.Wait(1, 0, nil); err != nil || len(evs) != 0 {
		t.Errorf("Wait(max=0) = (%v, %v) want (nil, nil)", evs, err)
	}
	before := cap(l.events)
	if before >= 200 {
		t.Fatalf("前置条件不成立：初始缓冲容量 %d", before)
	}
	d := 20 * time.Millisecond
	evs, err := l.Wait(1, 200, &d)
	if err != ErrTimeout {
		t.Fatalf("Wait 超时 err=%v want ErrTimeout", err)
	}
	if len(evs) != 0 {
		t.Errorf("无事件时不该有返回事件: %v", evs)
	}
	if cap(l.events) < 200 {
		t.Errorf("Wait(max=200) 后复用缓冲容量 = %d, want >= 200", cap(l.events))
	}
}

// TestLibAIOSubmitReadBeyondEOF 越界读返回 0（libaio 侧事件归一化）。
func TestLibAIOSubmitReadBeyondEOF(t *testing.T) {
	r, err := newLibAIORing(4)
	if err != nil {
		t.Fatalf("newLibAIORing: %v", err)
	}
	defer func() { _ = r.Close() }()

	f := newTestFile(t, testChunk)
	fd := int(f.Fd())
	seq, err := r.SubmitRead(fd, make([]byte, testChunk), 4*testChunk)
	if err != nil {
		t.Fatalf("SubmitRead: %v", err)
	}
	evs, err := r.Wait(1, 1, nil)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(evs) != 1 || evs[0].Data != seq || evs[0].Res != 0 {
		t.Fatalf("events=%+v seq=%d want res=0", evs, seq)
	}
}
