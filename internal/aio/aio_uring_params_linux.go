package aio

import (
	"strconv"
	"unsafe"
)

// 本文件负责 io_uring 的「内核回填参数与 mmap 结果的逐字段校验」：内核在 io_uring_setup
// 里回填结构体布局与队列深度，若与 aio_uring_uapi_linux.go 声明的 UAPI 不符，这些值会是
// 垃圾 —— 在此尽早拦住，而不是等到踩坏内存。运行时（建环、提交、收割）在 aio_uring_linux.go，
// UAPI 声明在 aio_uring_uapi_linux.go。

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
		{name: "sq_off.head", sq: true, off: p.SQOff.Head, width: 4},
		{name: "sq_off.tail", sq: true, off: p.SQOff.Tail, width: 4},
		{name: "sq_off.ring_mask", sq: true, off: p.SQOff.RingMask, width: 4},
		{name: "sq_off.ring_entries", sq: true, off: p.SQOff.RingEntries, width: 4},
		{name: "sq_off.flags", sq: true, off: p.SQOff.Flags, width: 4},
		{name: "sq_off.dropped", sq: true, off: p.SQOff.Dropped, width: 4},
		{name: "sq_off.array", sq: true, off: p.SQOff.Array, width: 4},
		{name: "cq_off.head", sq: false, off: p.CQOff.Head, width: 4},
		{name: "cq_off.tail", sq: false, off: p.CQOff.Tail, width: 4},
		{name: "cq_off.ring_mask", sq: false, off: p.CQOff.RingMask, width: 4},
		{name: "cq_off.ring_entries", sq: false, off: p.CQOff.RingEntries, width: 4},
		{name: "cq_off.overflow", sq: false, off: p.CQOff.Overflow, width: 4},
		{name: "cq_off.cqes", sq: false, off: p.CQOff.CQEs, width: 4},
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
		{name: "sq_off.ring_mask", got: u32At(r.sqRing, p.SQOff.RingMask), want: p.SQEntries - 1},
		{name: "sq_off.ring_entries", got: u32At(r.sqRing, p.SQOff.RingEntries), want: p.SQEntries},
		{name: "cq_off.ring_mask", got: u32At(r.cqRing, p.CQOff.RingMask), want: p.CQEntries - 1},
		{name: "cq_off.ring_entries", got: u32At(r.cqRing, p.CQOff.RingEntries), want: p.CQEntries},
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
