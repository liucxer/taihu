package aio

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// CheckIOPoll 校验目标块设备是否开启了队列级轮询 —— IOPOLL 的前置条件。
//
// 未开启时内核的 blk_poll 直接返回 0，io_uring 的轮询请求既不完成也不报错，
// 会永远停在 iopoll_list 上（表现为挂死），故这里提前硬失败并给出开启命令。
func CheckIOPoll(devPath string) error {
	sysPath := "/sys/class/block/" + filepath.Base(devPath) + "/queue/io_poll"
	b, err := os.ReadFile(sysPath)
	if err != nil {
		return fmt.Errorf("aio: IOPOLL 需要块设备开启队列轮询，但读取 %s 失败: %w", sysPath, err)
	}
	if v := strings.TrimSpace(string(b)); v != "1" {
		return fmt.Errorf("aio: IOPOLL 需要 %s 为 1（当前为 %q），请先执行: echo 1 > %s",
			sysPath, v, sysPath)
	}
	return nil
}

// io_uring 实现。结构与常量按 include/uapi/linux/io_uring.h（v5.10）手工声明，
// 与 aio_linux.go 手写 iocb 的做法一致：golang.org/x/sys 只提供三个系统调用号，
// 没有任何 IORING_* 常量或结构体。
//
// 与 libaio 的三处关键语义差异（决定了下面的实现形状）：
//
//  1. io_uring_enter 没有超时参数（带超时的 EXT_ARG 是 5.11+）。第 5/6 个参数是
//     信号掩码指针+长度。所以 Wait 的超时靠 poll(ring fd) 实现 —— ring fd 可 poll，
//     有 CQE 时返回 EPOLLIN。
//  2. enter 的返回值有歧义：同时传 to_submit>0 与 GETEVENTS 时返回的是提交条数，
//     且部分提交会直接跳过等待。故提交与等待拆成两次 enter，提交一律 flags=0。
//  3. SQ 空间在「内核取走 SQE」时释放（不是完成时），libaio 的队列深度语义消失。
//     故自行维护 inflight 计数复刻深度上限，并据此结构性排除 CQE 溢出。
const (
	ioringOpRead  uint8 = 22
	ioringOpWrite uint8 = 23

	ioringSetupIOPoll uint32 = 1 << 0
	ioringSetupClamp  uint32 = 1 << 4

	ioringFeatSingleMmap uint32 = 1 << 0

	// ioringSQCQOverflow 内核因 CQ 满而把完成事件挤到溢出链表的标志位。
	ioringSQCQOverflow uint32 = 1 << 1

	ioringEnterGetEvents uint32 = 1 << 0

	ioringOffSQRing uint64 = 0
	ioringOffCQRing uint64 = 0x8000000
	ioringOffSQEs   uint64 = 0x10000000

	ioringRegisterProbe uint32 = 8

	ioUringSQESize = 64
	ioUringCQESize = 16

	// 内核回填的各字段偏移都在 ring 首部（实测 <1KiB）。用这个宽松上界拦住
	// 「解析错位后的垃圾偏移」进入 mmap 长度计算；精确的段内边界在 mmap 后校验。
	ioUringMaxFieldOffset = 64 << 10
)

// ioUringSQE 提交队列项（struct io_uring_sqe，64 字节）。字段顺序不可调整。
type ioUringSQE struct {
	Opcode   uint8
	Flags    uint8
	IOPrio   uint16
	FD       int32
	Off      uint64 // 联合 off / addr2
	Addr     uint64 // 联合 addr / splice_off_in
	Len      uint32
	RWFlags  uint32 // 联合 rw_flags / fsync_flags / ...
	UserData uint64
	BufIndex uint16 // 联合 buf_index / buf_group（本实现用不到，保持 0）
	Person   uint16
	SpliceFD int32 // 联合 splice_fd_in / file_index
	Addr3    uint64
	Pad2     uint64
}

// ioUringCQE 完成队列项（struct io_uring_cqe，16 字节）。
type ioUringCQE struct {
	UserData uint64
	Res      int32 // >=0 字节数；<0 为 -errno
	Flags    uint32
}

// ioUringSQOffsets 提交环内各字段相对映射基址的字节偏移。
type ioUringSQOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Flags       uint32
	Dropped     uint32
	Array       uint32
	Resv1       uint32
	Resv2       uint64
}

// ioUringCQOffsets 完成环内各字段相对映射基址的字节偏移。
type ioUringCQOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Overflow    uint32
	CQEs        uint32
	Flags       uint32
	Resv1       uint32
	Resv2       uint64
}

// ioUringParams 内核回填的 ring 参数（struct io_uring_params，120 字节）。
type ioUringParams struct {
	SQEntries    uint32
	CQEntries    uint32
	Flags        uint32
	SQThreadCPU  uint32
	SQThreadIdle uint32
	Features     uint32
	WQFD         uint32
	Resv         [3]uint32
	SQOff        ioUringSQOffsets
	CQOff        ioUringCQOffsets
}

// 编译期布局断言：与内核 UAPI 不一致时索引越界，直接编译失败（零运行时代价）。
var (
	_ = [1]byte{}[unsafe.Sizeof(ioUringParams{})-120]
	_ = [1]byte{}[unsafe.Sizeof(ioUringSQE{})-ioUringSQESize]
	_ = [1]byte{}[unsafe.Sizeof(ioUringCQE{})-ioUringCQESize]
	_ = [1]byte{}[unsafe.Offsetof(ioUringSQE{}.UserData)-32]
	_ = [1]byte{}[unsafe.Offsetof(ioUringSQE{}.BufIndex)-40]
	_ = [1]byte{}[unsafe.Offsetof(ioUringCQE{}.Res)-8]
)

// uringRing 基于 io_uring 的异步 IO 实现。
//
// 并发约定：Submit* 可由多个 goroutine 并发调用（由 mu 串行化，单生产者填 SQE）；
// Wait 由单一完成泵 goroutine 持有；两者通过原子读写的 SQ/CQ head/tail 交互。
type uringRing struct {
	fd       int
	pfd      unix.PollFd
	singleMM bool // SQ/CQ 是否共用同一块映射（IORING_FEAT_SINGLE_MMAP）

	mu       sync.Mutex // 保护 seq / sqeTail / inflight / closed
	seq      uint64
	sqeTail  uint32
	inflight uint32
	closed   bool

	depth     int    // 在途请求上限，复刻 libaio 的队列深度语义
	sqEntries uint32 // 内核回填的 SQ 深度（roundup_pow_of_two 后）
	sqMask    uint32
	cqMask    uint32
	cqeOff    uint32

	iopoll bool

	// mmap 原始句柄。Munmap 必须传回同一个 slice，故保留不重新切片为局部变量。
	sqRing []byte
	cqRing []byte
	sqes   []byte

	sqHead  *uint32
	sqTail  *uint32
	sqFlags *uint32
	cqHead  *uint32
	cqTail  *uint32
}

var uringOverflowOnce sync.Once

// uringSetupRaw 调 io_uring_setup 建 ring，返回 fd 与内核回填的参数。
// 不持有任何资源引用，调用方负责 Close(fd)。
func uringSetupRaw(entries uint32, flags uint32) (int, ioUringParams, error) {
	var p ioUringParams
	p.Flags = flags | ioringSetupClamp
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP,
		uintptr(entries), uintptr(unsafe.Pointer(&p)), 0)
	if errno != 0 {
		return -1, p, errno
	}
	return int(fd), p, nil
}

// newIOUringRing 建 io_uring ring：io_uring_setup + mmap（SINGLE_MMAP 下只映射一次）。
func newIOUringRing(maxEvents int, iopoll bool) (Ring, error) {
	if maxEvents <= 0 || maxEvents > 1<<16 {
		return nil, errInvalidMaxEvents
	}
	var flags uint32
	if iopoll {
		flags |= ioringSetupIOPoll
	}
	fd, p, err := uringSetupRaw(uint32(maxEvents), flags)
	if err != nil {
		return nil, err
	}

	r, err := mapUringRing(fd, p, maxEvents, iopoll)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return r, nil
}

// mapUringRing 校验内核回填参数并完成三段 mmap（SQ ring / CQ ring / SQE 数组）。
func mapUringRing(fd int, p ioUringParams, maxEvents int, iopoll bool) (*uringRing, error) {
	if err := validateUringParams(&p, maxEvents); err != nil {
		return nil, err
	}

	sqRingSz := uintptr(p.SQOff.Array) + uintptr(p.SQEntries)*4
	cqRingSz := uintptr(p.CQOff.CQEs) + uintptr(p.CQEntries)*ioUringCQESize
	sqesSz := uintptr(p.SQEntries) * ioUringSQESize

	const prot = unix.PROT_READ | unix.PROT_WRITE
	const mflags = unix.MAP_SHARED | unix.MAP_POPULATE

	r := &uringRing{
		fd:        fd,
		pfd:       unix.PollFd{Fd: int32(fd), Events: unix.EPOLLIN},
		depth:     maxEvents,
		sqEntries: p.SQEntries,
		sqMask:    p.SQEntries - 1,
		cqMask:    p.CQEntries - 1,
		cqeOff:    p.CQOff.CQEs,
		iopoll:    iopoll,
	}

	if p.Features&ioringFeatSingleMmap != 0 {
		// SQ 与 CQ 是同一块映射：只 mmap 一次，长度取两者较大值，CQ 基址即 SQ 基址。
		sz := sqRingSz
		if cqRingSz > sz {
			sz = cqRingSz
		}
		m, err := unix.Mmap(fd, int64(ioringOffSQRing), int(sz), prot, mflags)
		if err != nil {
			return nil, err
		}
		r.singleMM, r.sqRing, r.cqRing = true, m, m
	} else {
		sq, err := unix.Mmap(fd, int64(ioringOffSQRing), int(sqRingSz), prot, mflags)
		if err != nil {
			return nil, err
		}
		cq, err := unix.Mmap(fd, int64(ioringOffCQRing), int(cqRingSz), prot, mflags)
		if err != nil {
			_ = unix.Munmap(sq)
			return nil, err
		}
		r.sqRing, r.cqRing = sq, cq
	}

	sqes, err := unix.Mmap(fd, int64(ioringOffSQEs), int(sqesSz), prot, mflags)
	if err != nil {
		unmapUringRing(r)
		return nil, err
	}
	r.sqes = sqes

	// 段长已知，此时才能做「偏移 + 宽度落在段内」的精确校验与取值自洽检查。
	if err := verifyUringRing(r, &p); err != nil {
		unmapUringRing(r)
		return nil, err
	}

	// 对象地址由切片索引派生（不用 uintptr 加法）：偏移有界、对齐正确，
	// 且 -race 的 checkptr 不会误报 "pointer arithmetic result points to invalid allocation"。
	r.sqHead = (*uint32)(unsafe.Pointer(&r.sqRing[p.SQOff.Head]))
	r.sqTail = (*uint32)(unsafe.Pointer(&r.sqRing[p.SQOff.Tail]))
	r.sqFlags = (*uint32)(unsafe.Pointer(&r.sqRing[p.SQOff.Flags]))
	r.cqHead = (*uint32)(unsafe.Pointer(&r.cqRing[p.CQOff.Head]))
	r.cqTail = (*uint32)(unsafe.Pointer(&r.cqRing[p.CQOff.Tail]))

	// SQ array 是「槽位索引 → SQE 下标」的映射表。本实现槽位与 SQE 一一对应，
	// 故 setup 时一次性填 i=i 后不再改写。
	arr := r.sqRing[p.SQOff.Array:]
	for i := uint32(0); i < p.SQEntries; i++ {
		*(*uint32)(unsafe.Pointer(&arr[uintptr(i)*4])) = i
	}

	return r, nil
}

// unmapUringRing 解除映射（SINGLE_MMAP 下 SQ/CQ 是同一块，只能解一次）。
func unmapUringRing(r *uringRing) {
	if r.sqRing != nil {
		_ = unix.Munmap(r.sqRing)
	}
	if r.cqRing != nil && !r.singleMM {
		_ = unix.Munmap(r.cqRing)
	}
	if r.sqes != nil {
		_ = unix.Munmap(r.sqes)
	}
	r.sqRing, r.cqRing, r.sqes = nil, nil, nil
}

// uringRingField ring 映射内的一个字段：所在段（SQ/CQ）、相对该段基址的字节偏移、宽度。
type uringRingField struct {
	name  string
	sq    bool
	off   uint32
	width uintptr
}

// uringRingFields 列出 sq_off / cq_off 中需要校验的字段。
//
// 关键语义：这两个结构体里的每个成员都是「相对本段 ring 映射基址的**字节偏移**」，
// 而不是字段的取值。例如 sq_off.ring_mask == 256 表示「掩码那个 u32 在偏移 256 处」，
// 掩码本身为 7 需要用该偏移读出来（见 verifyUringRing）。把偏移当值来比是错的。
func uringRingFields(p *ioUringParams) []uringRingField {
	return []uringRingField{
		{"sq_off.head", true, p.SQOff.Head, 4},
		{"sq_off.tail", true, p.SQOff.Tail, 4},
		{"sq_off.ring_mask", true, p.SQOff.RingMask, 4},
		{"sq_off.ring_entries", true, p.SQOff.RingEntries, 4},
		{"sq_off.flags", true, p.SQOff.Flags, 4},
		{"sq_off.dropped", true, p.SQOff.Dropped, 4},
		{"sq_off.array", true, p.SQOff.Array, 4},
		{"cq_off.head", false, p.CQOff.Head, 4},
		{"cq_off.tail", false, p.CQOff.Tail, 4},
		{"cq_off.ring_mask", false, p.CQOff.RingMask, 4},
		{"cq_off.ring_entries", false, p.CQOff.RingEntries, 4},
		{"cq_off.overflow", false, p.CQOff.Overflow, 4},
		{"cq_off.cqes", false, p.CQOff.CQEs, 4},
	}
}

// validateUringParams 校验内核回填的参数，使调用方能安全地据此计算 mmap 长度。
// 布局若写错，这些值会是垃圾值 —— 在此尽早拦住，而不是等到踩坏内存。
func validateUringParams(p *ioUringParams, want int) error {
	if p.SQEntries == 0 || p.SQEntries&(p.SQEntries-1) != 0 || p.SQEntries < uint32(want) {
		return &uringParamError{field: "sq_entries", got: p.SQEntries}
	}
	if p.CQEntries == 0 || p.CQEntries&(p.CQEntries-1) != 0 || p.CQEntries < p.SQEntries {
		return &uringParamError{field: "cq_entries", got: p.CQEntries}
	}
	for _, f := range uringRingFields(p) {
		if f.off%4 != 0 || f.off >= ioUringMaxFieldOffset {
			return &uringParamError{field: f.name, got: f.off}
		}
	}
	return nil
}

// verifyUringRing 在 mmap 之后核对布局：每个字段的「偏移 + 宽度」必须落在所在段内，
// 且 ring_mask / ring_entries 的**取值**要与内核回填的 entries 自洽。
//
// 这是真正能发现「结构体布局与内核 UAPI 不符」的检查 —— 布局错位时读到的掩码或
// 条目数会对不上。段长要到 mmap 之后才知道，所以放在这里而不是参数校验里。
func verifyUringRing(r *uringRing, p *ioUringParams) error {
	for _, f := range uringRingFields(p) {
		seg := r.cqRing
		if f.sq {
			seg = r.sqRing
		}
		if uintptr(f.off)+f.width > uintptr(len(seg)) {
			return &uringParamError{field: f.name, got: f.off}
		}
	}
	u32At := func(seg []byte, off uint32) uint32 {
		return *(*uint32)(unsafe.Pointer(&seg[off]))
	}
	checks := []struct {
		name string
		got  uint32
		want uint32
	}{
		{"sq_off.ring_mask", u32At(r.sqRing, p.SQOff.RingMask), p.SQEntries - 1},
		{"sq_off.ring_entries", u32At(r.sqRing, p.SQOff.RingEntries), p.SQEntries},
		{"cq_off.ring_mask", u32At(r.cqRing, p.CQOff.RingMask), p.CQEntries - 1},
		{"cq_off.ring_entries", u32At(r.cqRing, p.CQOff.RingEntries), p.CQEntries},
	}
	for _, c := range checks {
		if c.got != c.want {
			return &uringParamError{field: c.name, got: c.got}
		}
	}
	return nil
}

// uringParamError 内核回填的 ring 参数不合理（通常意味着结构体布局与 UAPI 不符）。
type uringParamError struct {
	field string
	got   uint32
}

func (e *uringParamError) Error() string {
	return "aio: io_uring 参数校验失败: " + e.field + "=" +
		strconv.FormatUint(uint64(e.got), 10) + " 非法（布局与内核 UAPI 不一致？）"
}

// ringQueueDepth 返回 ring 的实际队列深度（供启动日志标注真正建出的队列规模）。
// 非 io_uring 后端返回 ok=false。
func ringQueueDepth(r Ring) (sq, cq uint32, ok bool) {
	if u, isUring := r.(*uringRing); isUring {
		return u.sqEntries, u.cqMask + 1, true
	}
	return 0, 0, false
}

// sqeAt 取第 idx 个 SQE 槽位。
func (r *uringRing) sqeAt(idx uint32) *ioUringSQE {
	return (*ioUringSQE)(unsafe.Pointer(&r.sqes[uintptr(idx)*ioUringSQESize]))
}

// cqeAt 取第 idx 个 CQE 槽位。
func (r *uringRing) cqeAt(idx uint32) *ioUringCQE {
	return (*ioUringCQE)(unsafe.Pointer(&r.cqRing[uintptr(r.cqeOff)+uintptr(idx)*ioUringCQESize]))
}

// sqFree 当前可用 SQ 槽位数（须持 mu）。sq_head 由内核推进，必须原子读；
// tail-head 在 uint32 上回绕，差值恒 ≤ sq_entries，故无需取模。
func (r *uringRing) sqFree() int {
	head := atomic.LoadUint32(r.sqHead)
	return int(r.sqEntries - (r.sqeTail - head))
}

// rewind 回退 SQ tail，撤销未被内核消费的 SQE 的发布。
func (r *uringRing) rewind(to uint32) {
	r.sqeTail = to
	atomic.StoreUint32(r.sqTail, to)
}

// SubmitRead 实现 Ring.SubmitRead。
func (r *uringRing) SubmitRead(fd int, buf []byte, off int64) (uint64, error) {
	seq, n, err := r.submit(fd, []ReadSpec{{Buf: buf, Off: off}}, ioringOpRead)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrFull
	}
	return seq, nil
}

// SubmitReadBatch 实现 Ring.SubmitReadBatch：一次 enter 批量提交，允许部分提交。
func (r *uringRing) SubmitReadBatch(fd int, specs []ReadSpec) (uint64, int, error) {
	return r.submit(fd, specs, ioringOpRead)
}

// SubmitWrite 实现 Ring.SubmitWrite。
func (r *uringRing) SubmitWrite(fd int, buf []byte, off int64) (uint64, error) {
	seq, n, err := r.submit(fd, []ReadSpec{{Buf: buf, Off: off}}, ioringOpWrite)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrFull
	}
	return seq, nil
}

// submit 填 SQE 并一次 io_uring_enter 提交（flags=0，只提交不等待）。
// 返回首个关联序号与成功排队条数；未排队部分的 SQE 已从 ring 撤销发布，
// 调用方追加提交时会拿到同一批序号，序号不重叠。
func (r *uringRing) submit(fd int, specs []ReadSpec, op uint8) (uint64, int, error) {
	n := len(specs)
	if n == 0 {
		return 0, 0, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, 0, unix.EBADF
	}

	// 同时受 SQ 空闲空间与在途上限约束。后者复刻 libaio 的 depth 语义，并让
	// 「在途 ≤ depth ≤ sq_entries < cq_entries」成立，从而结构性排除 CQ 溢出。
	free := r.sqFree()
	if lim := r.depth - int(atomic.LoadUint32(&r.inflight)); lim < free {
		free = lim
	}
	if free <= 0 {
		return 0, 0, ErrFull
	}
	if n > free {
		n = free
	}

	base, tail0 := r.seq, r.sqeTail
	for i := 0; i < n; i++ {
		sp := specs[i]
		var addr uint64
		if len(sp.Buf) > 0 {
			addr = uint64(uintptr(unsafe.Pointer(&sp.Buf[0])))
		}
		// 整块覆盖写，避免 flags/ioprio/buf_index 残留（误留 IOSQE_IO_LINK 会改变语义）。
		*r.sqeAt((tail0 + uint32(i)) & r.sqMask) = ioUringSQE{
			Opcode:   op,
			FD:       int32(fd),
			Off:      uint64(sp.Off),
			Addr:     addr,
			Len:      uint32(len(sp.Buf)),
			UserData: base + uint64(i) + 1,
		}
	}
	atomic.StoreUint32(r.sqTail, tail0+uint32(n)) // release：SQE 字节先于 tail 对内核可见
	r.sqeTail = tail0 + uint32(n)

	ret, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER,
		uintptr(r.fd), uintptr(n), 0, 0, 0, 0)

	var consumed int
	switch {
	case errno == 0:
		consumed = int(ret)
	case errno == unix.EAGAIN || errno == unix.EBUSY || errno == unix.ENOMEM:
		// 一条都未被消费（EBUSY = CQ 溢出链表未能 flush；EAGAIN = 请求槽位不足）。
		r.rewind(tail0)
		return 0, 0, ErrFull
	default:
		// EINTR 等：SQE 可能已被内核取走，绝不能重试；以内核推进的 sq_head 为准。
		consumed = r.consumedSince(tail0)
		if consumed == 0 {
			r.rewind(tail0)
			return 0, 0, errno
		}
	}
	if consumed > n {
		consumed = n
	}
	if consumed < 0 {
		consumed = 0
	}

	r.rewind(tail0 + uint32(consumed)) // ★ 未消费的 SQE 必须撤销发布，否则会被重复提交
	atomic.AddUint32(&r.inflight, uint32(consumed))
	r.seq = base + uint64(consumed)

	return base + 1, consumed, nil
}

// consumedSince 计算自 tail0 起被内核实际取走的 SQE 条数。
func (r *uringRing) consumedSince(tail0 uint32) int {
	return int(atomic.LoadUint32(r.sqHead) - tail0)
}

// reap 直接读 CQ ring 收割完成事件（0 系统调用 —— io_uring 相对 libaio 的主要红利）。
func (r *uringRing) reap(out []Event, max int) []Event {
	if r.cqRing == nil {
		return out
	}
	if atomic.LoadUint32(r.sqFlags)&ioringSQCQOverflow != 0 {
		uringOverflowOnce.Do(func() {
			log.Printf("taihu: aio io_uring CQ 溢出，完成事件被挤入溢出链表（在途上限失效？）")
		})
	}
	// cq.tail 由内核写：原子读即 acquire，保证随后读到的 CQE 内容已可见。
	head, tail := atomic.LoadUint32(r.cqHead), atomic.LoadUint32(r.cqTail)
	for head != tail && len(out) < max {
		cqe := *r.cqeAt(head & r.cqMask) // 值拷贝，避免字段被缓存在寄存器
		head++
		out = append(out, Event{Data: cqe.UserData, Res: int64(cqe.Res)})
		atomic.AddUint32(&r.inflight, ^uint32(0))
	}
	atomic.StoreUint32(r.cqHead, head) // release：把 CQ 空间还给内核
	return out
}

// Wait 实现 Ring.Wait：先零系统调用收割 CQ ring，不足时按超时 poll ring fd。
func (r *uringRing) Wait(min, max int, timeout *time.Duration) ([]Event, error) {
	if max <= 0 {
		return nil, nil
	}
	out := r.reap(make([]Event, 0, max), max)
	if len(out) >= min {
		return out, nil
	}

	// IOPOLL 下完成只在 io_uring_enter 内由内核忙等推进，poll 不会带来新 CQE；
	// 且无在途请求时 io_iopoll_check 会立即返回，故这里只在有在途时才 enter 驱动。
	if r.iopoll && atomic.LoadUint32(&r.inflight) > 0 {
		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(r.fd),
			0, uintptr(min-len(out)), uintptr(ioringEnterGetEvents), 0, 0)
		if errno != 0 && errno != unix.EINTR {
			return out, errno
		}
		out = r.reap(out, max)
		if len(out) >= min {
			return out, nil
		}
	}

	if timeout == nil {
		for len(out) < min && len(out) < max {
			// 只等不提交（to_submit=0）。绝不能在这里带 to_submit —— 那样返回值会变成提交条数。
			_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(r.fd),
				0, uintptr(min-len(out)), uintptr(ioringEnterGetEvents), 0, 0)
			if errno == unix.EINTR {
				continue
			}
			if errno != 0 {
				return out, errno
			}
			out = r.reap(out, max)
		}
		return out, nil
	}

	deadline := time.Now().Add(*timeout)
	for len(out) < min && len(out) < max {
		rem := time.Until(deadline)
		if rem <= 0 {
			break // timeout 为 0 或已到期：直接判定超时
		}
		ts := unix.NsecToTimespec(int64(rem))
		if _, err := unix.Ppoll([]unix.PollFd{r.pfd}, &ts, nil); err != nil && err != unix.EINTR {
			return out, err
		}
		out = r.reap(out, max)
	}
	if len(out) < min {
		return out, ErrTimeout // 与 libaio 一致：返回已取到的部分事件 + ErrTimeout
	}
	return out, nil
}

// Close 实现 Ring.Close：置 closed 后解除映射并关闭 ring fd。
// 必须先置 closed：libaio 在 io_destroy 后提交只是返回 EBADF，而本实现在解除映射后
// 提交会访问已失效地址（SIGSEGV），故入口处必须拦截。
func (r *uringRing) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	fd := r.fd
	r.fd = -1
	r.mu.Unlock()

	unmapUringRing(r)
	return unix.Close(fd)
}
