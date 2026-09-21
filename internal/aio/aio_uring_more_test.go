//go:build linux

package aio

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestUringSetupRawInvalidEntries io_uring_setup 的参数被内核拒绝时返回 errno。
func TestUringSetupRawInvalidEntries(t *testing.T) {
	fd, _, err := uringSetupRaw(0, 0)
	if err == nil {
		_ = unix.Close(fd)
		t.Fatal("entries=0 应被内核拒绝")
	}
	if !errors.Is(err, unix.EINVAL) {
		t.Errorf("err=%v want EINVAL", err)
	}
}

// TestUringEntryGuards maxEvents 越界、以及内核回填 entries 不足（如被 IORING_SETUP_CLAMP
// 夹取后小于请求值）都必须报错，不得按错误深度建出 ring。
func TestUringEntryGuards(t *testing.T) {
	for _, n := range []int{0, -3, 1<<16 + 1} {
		r, err := newIOUringRing(n, false)
		if err == nil {
			_ = r.Close()
			t.Errorf("newIOUringRing(%d) 应报错", n)
		}
	}

	if fd, _, err := uringSetupRaw(16, 0); err != nil {
		t.Skipf("io_uring 不可用: %v", err)
	} else {
		_ = unix.Close(fd)
	}
	// 内核把 entries 夹到 IORING_MAX_ENTRIES(32768) < 请求值 → 参数校验必须拦住。
	r, err := newIOUringRing(1<<16, false)
	if err == nil {
		_ = r.Close()
		t.Skip("本机内核未夹取 entries，跳过回填参数不足的分支")
	}
	if !strings.Contains(err.Error(), "sq_entries") {
		t.Errorf("err=%v 应指明 sq_entries", err)
	}
}

// TestUringDepthLimit 在途上限复刻 libaio 队列深度语义：超出的部分被截断，
// 截断后序号不重叠，全部在途时返回 ErrFull，收割后可继续提交。
func TestUringDepthLimit(t *testing.T) {
	r, err := newIOUringRing(4, false)
	if err != nil {
		t.Skipf("io_uring 不可用: %v", err)
	}
	defer func() { _ = r.Close() }()

	f := newTestFile(t, 8*testChunk)
	fd := int(f.Fd())

	specs := make([]ReadSpec, 6)
	for i := range specs {
		specs[i] = ReadSpec{Buf: make([]byte, testChunk), Off: int64(i) * testChunk}
	}

	first, n, err := r.SubmitReadBatch(fd, specs[:3])
	if err != nil || n != 3 {
		t.Fatalf("首批提交 submitted=%d err=%v want 3, nil", n, err)
	}
	// depth=4 且已有 3 条在途 → 第二批只能排入 1 条。
	first2, n2, err := r.SubmitReadBatch(fd, specs[3:])
	if err != nil || n2 != 1 {
		t.Fatalf("截断提交 submitted=%d err=%v want 1, nil", n2, err)
	}
	if first2 != first+3 {
		t.Errorf("截断后首序号 %d want %d（序号不得重叠）", first2, first+3)
	}
	// 在途已达 depth → ErrFull（单条提交同样受限）
	if _, _, err := r.SubmitReadBatch(fd, specs[:1]); err != ErrFull {
		t.Errorf("SubmitReadBatch 在途满 err=%v want ErrFull", err)
	}
	if _, err := r.SubmitRead(fd, make([]byte, testChunk), 0); err != ErrFull {
		t.Errorf("SubmitRead 在途满 err=%v want ErrFull", err)
	}

	// 收割后深度重新可用。
	evs, err := r.Wait(4, 4, nil)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(evs) != 4 {
		t.Fatalf("got %d events want 4", len(evs))
	}
	for _, ev := range evs {
		if ev.Res != testChunk {
			t.Errorf("seq=%d res=%d want %d", ev.Data, ev.Res, testChunk)
		}
	}
	seq, err := r.SubmitRead(fd, make([]byte, testChunk), 0)
	if err != nil {
		t.Fatalf("收割后 SubmitRead: %v", err)
	}
	if evs, err := r.Wait(1, 1, nil); err != nil || len(evs) != 1 || evs[0].Data != seq {
		t.Fatalf("收割后 Wait: evs=%+v seq=%d err=%v", evs, seq, err)
	}
}

// TestUringSubmitEmptyBatchAndAfterClose 空批次不占序号；Close 后所有提交入口返回 EBADF
// （必须先拦在入口，避免访问已解除映射的内存）。
func TestUringSubmitEmptyBatchAndAfterClose(t *testing.T) {
	r, err := newIOUringRing(4, false)
	if err != nil {
		t.Skipf("io_uring 不可用: %v", err)
	}
	if fs, n, err := r.SubmitReadBatch(0, nil); err != nil || n != 0 || fs != 0 {
		t.Errorf("空读批次 = (%d, %d, %v), want (0, 0, nil)", fs, n, err)
	}
	if fs, n, err := r.SubmitWriteBatch(0, nil); err != nil || n != 0 || fs != 0 {
		t.Errorf("空写批次 = (%d, %d, %v), want (0, 0, nil)", fs, n, err)
	}

	f := newTestFile(t, testChunk)
	fd := int(f.Fd())
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("二次 Close err=%v want nil", err)
	}

	buf := make([]byte, testChunk)
	if _, err := r.SubmitRead(fd, buf, 0); !errors.Is(err, unix.EBADF) {
		t.Errorf("Close 后 SubmitRead err=%v want EBADF", err)
	}
	if _, err := r.SubmitWrite(fd, buf, 0); !errors.Is(err, unix.EBADF) {
		t.Errorf("Close 后 SubmitWrite err=%v want EBADF", err)
	}
	if _, _, err := r.SubmitReadBatch(fd, []ReadSpec{{Buf: buf}}); !errors.Is(err, unix.EBADF) {
		t.Errorf("Close 后 SubmitReadBatch err=%v want EBADF", err)
	}
	if _, _, err := r.SubmitWriteBatch(fd, []WriteSpec{{Buf: buf}}); !errors.Is(err, unix.EBADF) {
		t.Errorf("Close 后 SubmitWriteBatch err=%v want EBADF", err)
	}
}

// TestUringEnterFailureKeepsRingUsable io_uring_enter 失败（未消费任何 SQE）时必须撤销
// 发布，ring 仍可继续提交，且 SQE 槽位/序号可复用。
func TestUringEnterFailureKeepsRingUsable(t *testing.T) {
	r, err := newIOUringRing(4, false)
	if err != nil {
		t.Skipf("io_uring 不可用: %v", err)
	}
	u := r.(*uringRing)
	defer func() { _ = r.Close() }()

	f := newTestFile(t, testChunk)
	fd := int(f.Fd())

	// 把 ring fd 换成非法值，制造 enter 失败（模拟 EINTR/EBADF 类瞬时错误）。
	saved := u.fd
	u.fd = -1
	_, n, err := u.submit(fd, []ReadSpec{{Buf: make([]byte, testChunk)}}, ioringOpRead)
	u.fd = saved
	if err == nil {
		t.Fatalf("enter 失败时应返回 errno（n=%d）", n)
	}
	if n != 0 {
		t.Errorf("未消费任何 SQE 时 submitted=%d want 0", n)
	}

	// 撤销发布后必须能重新提交且序号从 1 开始（失败的那条不占序号）。
	seq, err := r.SubmitRead(fd, make([]byte, testChunk), 0)
	if err != nil {
		t.Fatalf("失败后重新提交: %v", err)
	}
	if seq != 1 {
		t.Errorf("失败请求不应占用序号：seq=%d want 1", seq)
	}
	evs, err := r.Wait(1, 1, nil)
	if err != nil || len(evs) != 1 || evs[0].Data != seq || evs[0].Res != testChunk {
		t.Fatalf("evs=%+v seq=%d err=%v", evs, seq, err)
	}
}

// TestUringReapFakes reap 的空映射与 CQ 溢出标志路径（不依赖真实内核状态）。
func TestUringReapFakes(t *testing.T) {
	// cqRing 为 nil（Close 之后）：直接返回，不解引用。
	if got := (&uringRing{}).reap(make([]Event, 0, 4), 4); len(got) != 0 {
		t.Errorf("cqRing=nil 时 reap 返回 %v want 空", got)
	}

	// CQ 溢出标志置位：记录一次告警，head==tail 时不读任何 CQE。
	seg := make([]byte, 4096)
	head, tail, flags := new(uint32), new(uint32), new(uint32)
	*flags = ioringSQCQOverflow
	u := &uringRing{cqRing: seg, cqHead: head, cqTail: tail, sqFlags: flags}
	if got := u.reap(make([]Event, 0, 4), 4); len(got) != 0 {
		t.Errorf("溢出且无 CQE 时 reap 返回 %v want 空", got)
	}
}

// TestUringWaitNoIO 无在途请求时的 Wait 出口：max<=0 直接返回；timeout=nil 且 enter
// 失败时立刻返回 errno（不得挂死）；有超时时返回 ErrTimeout。
func TestUringWaitNoIO(t *testing.T) {
	fake := &uringRing{fd: -1}
	if evs, err := fake.Wait(1, 0, nil); err != nil || len(evs) != 0 {
		t.Errorf("Wait(max=0) = (%v, %v) want (nil, nil)", evs, err)
	}

	// 非法 fd：io_uring_enter 立即以 errno 返回，这里必须把 errno 透出去（不得挂死）。
	if evs, err := fake.Wait(1, 4, nil); err == nil {
		t.Errorf("enter 失败时 Wait 应返回 errno，evs=%v", evs)
	}

	// IOPOLL 且确有在途请求时先 enter 驱动内核；enter 失败同样要透出 errno。
	pollFake := &uringRing{fd: -1, iopoll: true, inflight: 1}
	if evs, err := pollFake.Wait(1, 4, nil); err == nil {
		t.Errorf("IOPOLL 路径 enter 失败应返回 errno，evs=%v", evs)
	}

	d := 20 * time.Millisecond
	start := time.Now()
	evs, err := fake.Wait(1, 4, &d)
	if err != ErrTimeout {
		t.Fatalf("Wait 超时 err=%v want ErrTimeout", err)
	}
	if len(evs) != 0 {
		t.Errorf("无事件时不该有返回事件: %v", evs)
	}
	if elapsed := time.Since(start); elapsed < d/2 {
		t.Errorf("返回过早: %v < %v", elapsed, d/2)
	}
}

// TestUringWaitTimedSuccess 定时 Wait 在事件到达时从非超时出口返回（与 libaio 语义对齐）。
func TestUringWaitTimedSuccess(t *testing.T) {
	r, err := newIOUringRing(4, false)
	if err != nil {
		t.Skipf("io_uring 不可用: %v", err)
	}
	defer func() { _ = r.Close() }()

	f := newTestFile(t, testChunk)
	fd := int(f.Fd())
	buf := make([]byte, testChunk)
	seq, err := r.SubmitRead(fd, buf, 0)
	if err != nil {
		t.Fatalf("SubmitRead: %v", err)
	}
	d := 5 * time.Second
	evs, err := r.Wait(1, 4, &d)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(evs) != 1 || evs[0].Data != seq || evs[0].Res != testChunk {
		t.Fatalf("evs=%+v seq=%d want res=%d", evs, seq, testChunk)
	}
}

// TestUringMapReadOnlyFd 内核给的映射 fd 不可写时，带 PROT_WRITE 的 mmap 必然失败；
// 单块映射（SINGLE_MMAP）与三段独立映射的首段都要把 errno 透出，不得留下半成品 ring。
func TestUringMapReadOnlyFd(t *testing.T) {
	_, p, err := uringSetupRaw(8, 0)
	if err != nil {
		t.Skipf("io_uring 不可用: %v", err)
	}

	path := filepath.Join(t.TempDir(), "ro")
	if werr := os.WriteFile(path, []byte("x"), 0o600); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	rof, err := os.Open(path) // 只读：PROT_WRITE 映射会被内核以 EACCES 拒绝
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rof.Close() }()

	if r, merr := mapUringRing(int(rof.Fd()), p, 8, false); merr == nil {
		_ = r.Close()
		t.Error("只读 fd 上单块映射应报错")
	}
	q := p
	q.Features &^= ioringFeatSingleMmap
	if r, merr := mapUringRing(int(rof.Fd()), q, 8, false); merr == nil {
		_ = r.Close()
		t.Error("只读 fd 上独立 SQ 段映射应报错")
	}
}

// TestUringWaitTimedBlockingRead 大块 O_DIRECT 读在首次零系统调用收割时通常尚未完成，
// Wait 必须走 ppoll 等待，事件到达后从「非超时」出口返回，且读回数据要与写入一致。
func TestUringWaitTimedBlockingRead(t *testing.T) {
	r, err := newIOUringRing(4, false)
	if err != nil {
		t.Skipf("io_uring 不可用: %v", err)
	}
	defer func() { _ = r.Close() }()

	f, err := os.OpenFile(filepath.Join(t.TempDir(), "aio-uring-blocking"),
		os.O_RDWR|os.O_CREATE|syscall.O_DIRECT, 0o600)
	if err != nil {
		t.Skipf("O_DIRECT unsupported: %v", err)
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd())

	const n = 4 << 20
	wbuf := alignedBuf(n)
	copy(wbuf, pattern(0x5A, n))
	if _, err := r.SubmitWrite(fd, wbuf, 0); err != nil {
		t.Fatalf("SubmitWrite: %v", err)
	}
	if evs, err := r.Wait(1, 1, nil); err != nil || len(evs) != 1 || evs[0].Res != n {
		t.Fatalf("Wait write: evs=%v err=%v want res=%d", evs, err, n)
	}

	rbuf := alignedBuf(n)
	seq, err := r.SubmitRead(fd, rbuf, 0)
	if err != nil {
		t.Fatalf("SubmitRead: %v", err)
	}
	d := 10 * time.Second
	evs, err := r.Wait(1, 4, &d)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(evs) != 1 || evs[0].Data != seq || evs[0].Res != n {
		t.Fatalf("evs=%+v seq=%d want res=%d", evs, seq, n)
	}
	if !bytes.Equal(rbuf, wbuf) {
		t.Fatal("大块 O_DIRECT 读回数据与写入不一致")
	}
}

// TestUringParamErrorFormat 参数校验错误的文案必须点出字段名与实际取值。
func TestUringParamErrorFormat(t *testing.T) {
	msg := (&uringParamError{field: "sq_entries", got: 3}).Error()
	if !strings.Contains(msg, "sq_entries") || !strings.Contains(msg, "3") {
		t.Errorf("Error() = %q，应包含字段名与取值", msg)
	}
}

// TestUringCraftedParams 用伪造的内核回填参数覆盖参数校验/映射/布局核对失败路径，
// 以及无 SINGLE_MMAP 时三段独立映射的建立与解除。
func TestUringCraftedParams(t *testing.T) {
	base := func() ioUringParams {
		t.Helper()
		fd, p, err := uringSetupRaw(8, 0)
		if err != nil {
			t.Skipf("io_uring 不可用: %v", err)
		}
		_ = unix.Close(fd)
		return p
	}
	p := base()

	// 每个失败用例独占一个 fd：mapUringRing 成功时会接管 fd，失败时由调用方关闭。
	expectFail := func(name string, mutate func(*ioUringParams)) {
		t.Helper()
		fd, _, err := uringSetupRaw(8, 0)
		if err != nil {
			t.Skipf("io_uring 不可用: %v", err)
		}
		bad := p
		mutate(&bad)
		r, err := mapUringRing(fd, bad, 8, false)
		if err == nil {
			_ = r.Close()
			t.Errorf("%s: 期望报错，却建出了 ring", name)
			return
		}
		_ = unix.Close(fd)
	}

	// sq_entries 非法：0 / 非 2 的幂 / 小于请求值
	expectFail("sq_entries=0", func(q *ioUringParams) { q.SQEntries = 0 })
	expectFail("sq_entries=3", func(q *ioUringParams) { q.SQEntries = 3 })
	expectFail("sq_entries=4<want8", func(q *ioUringParams) { q.SQEntries = 4 })
	// cq_entries 非法：0 / 非 2 的幂 / 小于 sq_entries
	expectFail("cq_entries=0", func(q *ioUringParams) { q.CQEntries = 0 })
	expectFail("cq_entries=24", func(q *ioUringParams) { q.CQEntries = 24 })
	expectFail("cq_entries<sq_entries", func(q *ioUringParams) {
		q.SQEntries, q.CQEntries = 16, 4
	})
	// 字段偏移非法：非 4 字节对齐 / 超出宽松上界
	expectFail("sq_off.head=2", func(q *ioUringParams) { q.SQOff.Head = 2 })
	expectFail("cq_off.tail=2", func(q *ioUringParams) { q.CQOff.Tail = 2 })
	expectFail("sq_off.array=上界", func(q *ioUringParams) { q.SQOff.Array = ioUringMaxFieldOffset })
	// mmap 之后：字段越出所在段长（cqes 段被人为缩小、overflow 偏移在段外）
	expectFail("verify 字段越界", func(q *ioUringParams) {
		q.CQOff.CQEs = 0
		q.CQOff.Overflow = 4000
	})
	// mmap 之后：ring_mask 取值与内核回填的 entries 不自洽
	expectFail("verify 掩码不自洽", func(q *ioUringParams) {
		q.SQEntries, q.CQEntries = 16, 32
	})
	// 无 SINGLE_MMAP：SQ/CQ 分两次 mmap，SQ 段被人为放大 → 内核拒绝超尺寸映射
	expectFail("独立 sq mmap 失败", func(q *ioUringParams) {
		q.Features &^= ioringFeatSingleMmap
		q.SQOff.Array = 60000
	})
	// 无 SINGLE_MMAP：SQ 段正常、CQ 段被人为放大 → CQ 映射失败要回滚已建的 SQ 映射
	expectFail("独立 cq mmap 失败", func(q *ioUringParams) {
		q.Features &^= ioringFeatSingleMmap
		q.CQOff.CQEs = 60000
	})
	// SQ/CQ 段长度都在可映射范围内，但 SQEs 段被人为放大 → 内核拒绝超尺寸映射，
	// 此时必须回滚已建的 SQ/CQ 映射再返回 errno。
	expectFail("sqes mmap 失败", func(q *ioUringParams) {
		q.SQEntries, q.CQEntries = 128, 128
	})

	// 单块映射：SQ 段被人为缩短后 CQ 段更长，映射长度必须取两者较大值。
	fd, _, err := uringSetupRaw(8, 0)
	if err != nil {
		t.Skipf("io_uring 不可用: %v", err)
	}
	shrunk := p
	shrunk.SQOff.Array = 24
	rs, err := mapUringRing(fd, shrunk, 8, false)
	if err != nil {
		_ = unix.Close(fd)
		t.Fatalf("mapUringRing(SQ 段缩短): %v", err)
	}
	wantCQ := int(p.CQOff.CQEs) + int(p.CQEntries)*ioUringCQESize
	if !rs.singleMM || len(rs.cqRing) != len(rs.sqRing) || len(rs.sqRing) < wantCQ {
		t.Errorf("单块映射下 SQ/CQ 应共用同一块（长度取较大值）：sq=%d cq=%d want>=%d singleMM=%v",
			len(rs.sqRing), len(rs.cqRing), wantCQ, rs.singleMM)
	}
	if err := rs.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	// 无 SINGLE_MMAP 且尺寸正常：三段独立映射建出可用 ring，解除映射时也要分别 munmap。
	fd, _, err = uringSetupRaw(8, 0)
	if err != nil {
		t.Skipf("io_uring 不可用: %v", err)
	}
	noMM := p
	noMM.Features &^= ioringFeatSingleMmap
	r, err := mapUringRing(fd, noMM, 8, false)
	if err != nil {
		_ = unix.Close(fd)
		t.Fatalf("mapUringRing(无 SINGLE_MMAP): %v", err)
	}
	if r.singleMM {
		t.Error("清掉 IORING_FEAT_SINGLE_MMAP 后 singleMM 应为 false")
	}
	if len(r.sqRing) == 0 || len(r.cqRing) == 0 || len(r.sqes) == 0 {
		t.Fatalf("三段映射不完整: %d/%d/%d", len(r.sqRing), len(r.cqRing), len(r.sqes))
	}
	if sq, cq, ok := ringQueueDepth(r); !ok || sq == 0 || cq == 0 {
		t.Errorf("ringQueueDepth = (%d, %d, %v)", sq, cq, ok)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestCheckIOPoll 覆盖 sysfs 读取失败与 io_poll 取值判定；成功分支只在真的有
// 开启队列轮询的块设备时才会命中。
func TestCheckIOPoll(t *testing.T) {
	err := CheckIOPoll("/dev/taihu-no-such-block-device")
	if err == nil {
		t.Fatal("不存在的块设备应报错")
	}
	if !strings.Contains(err.Error(), "/sys/class/block/") {
		t.Errorf("err=%v 应点出 sysfs 路径", err)
	}

	ents, derr := os.ReadDir("/sys/class/block")
	if derr != nil {
		t.Skipf("无 /sys/class/block: %v", derr)
	}
	checked := 0
	for _, e := range ents {
		b, rerr := os.ReadFile(filepath.Join("/sys/class/block", e.Name(), "queue", "io_poll"))
		if rerr != nil {
			continue // 分区没有 queue 目录
		}
		on := strings.TrimSpace(string(b)) == "1"
		got := CheckIOPoll("/dev/" + e.Name())
		switch {
		case on && got != nil:
			t.Errorf("%s io_poll=1，CheckIOPoll 却报错: %v", e.Name(), got)
		case !on && got == nil:
			t.Errorf("%s io_poll=%q，CheckIOPoll 却通过", e.Name(), strings.TrimSpace(string(b)))
		}
		checked++
	}
	if checked == 0 {
		t.Skip("没有可读 io_poll 的块设备")
	}
}
