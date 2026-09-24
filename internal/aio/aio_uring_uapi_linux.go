package aio

import "unsafe"

// 本文件是 io_uring 的 UAPI 声明：常量、结构体与编译期布局断言 —— 纯声明、零逻辑。
//
// 内容按 include/uapi/linux/io_uring.h（v5.10）手工拷贝：golang.org/x/sys 只提供
// io_uring 的三个系统调用号，没有任何 IORING_* 常量或结构体。
//
// 字段顺序不可调整：这些结构体是与内核共享的二进制布局，顺序或宽度一旦偏离 UAPI，
// 内核回填的数据就会错位。下面的编译期断言把「结构体大小 / 关键字段偏移」与 UAPI 钉死，
// 不一致时索引越界、直接编译失败（零运行时代价）。
//
// 分工：本文件只放声明；运行时（建环、提交、收割）在 aio_uring_linux.go，
// 内核回填参数与 mmap 结果的校验在 aio_uring_params_linux.go。

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
