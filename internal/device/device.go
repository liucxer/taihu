// Package device 提供底层存储设备访问：直接操作裸设备文件，按 segment 提供 append/read。
//
// 单个文件句柄，同时用于读写。Linux 上以 O_DIRECT 打开（绕过 page cache），
// 此时读写缓冲、文件偏移、每次 IO 长度都必须 layout.BlockSize(4K) 对齐：
//   - 读：ReadAt 读入 4K 对齐缓冲（上层 Storage.Get/ReadAt 负责对齐与拷贝）；
//   - 写：Append 将对象以整块 4K 对齐写出，末尾用 0 补齐到 4K。
//
// WriteAt / ReadAt 语义保证不同偏移的并发读写安全。
package device

import (
	"context"
	"fmt"
	"io"
	"os"
	"unsafe"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/ierr"
	"github.com/liucxer/taihu/internal/layout"
)

// Device 底层存储：直接操作裸设备文件。
type Device struct {
	f    *os.File
	path string
}

// NewDevice 打开裸设备文件。平台差异（O_DIRECT / 普通打开）在 openDevice 中处理。
// 返回 (*Device, error)，便于暴露打开失败。
func NewDevice(ctx context.Context, nvmePath string) (*Device, error) {
	f, err := openDevice(nvmePath)
	if err != nil {
		return nil, fmt.Errorf("taihu: open device %q: %w", nvmePath, err)
	}
	return &Device{f: f, path: nvmePath}, nil
}

// Close 关闭设备文件。
func (d *Device) Close() error {
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

// segmentBase 返回段 i 的物理基址。
func (d *Device) segmentBase(segmentID int64) int64 {
	return segmentID * layout.SegmentSizeBytes
}

// bufAligned 报告 b 非空且首地址 4K 对齐（O_DIRECT 直写调用方缓冲的硬性前置）。
func bufAligned(b []byte) bool {
	return len(b) > 0 && uintptr(unsafe.Pointer(&b[0]))%uintptr(layout.BlockSize) == 0
}

// Append 将 data 的 size 字节写入 segmentID 段、段内 offset 处，末尾用 0 补齐到 4K。
//
// offset 必须 4K 对齐。按 data 首地址是否 4K 对齐分两条路径：
//
//  1. 首地址 4K 对齐（server 侧 bufpool 汇集的整对象即满足）：主体（4K 倍数、地址/偏移
//     均对齐）直接 O_DIRECT 直写，零拷贝；仅不足 4K 的尾块分配一块 4K 临时缓冲补零后写出。
//     整对象大块不再暂存，最多 2 次 WriteAt。
//  2. 首地址不对齐兜底：地址错位量在 4K 整数倍偏移下不变，任何子块都无法直写，
//     只能整体拷入对齐缓冲后按「对齐主体 + 尾部补齐」≤2 次 WriteAt 写出。
//
// data 不足 size 字节时返回错误。size 为 0 时无写 IO。
func (d *Device) Append(ctx context.Context, segmentID, off, size int64, data []byte) error {
	if off < 0 || off%layout.BlockSize != 0 {
		return fmt.Errorf("taihu: append offset %d not 4K aligned", off)
	}
	aligned := layout.Align4k(size)
	if off+aligned > layout.SegmentSizeBytes {
		return ierr.ErrTooLarge
	}
	if int64(len(data)) < size {
		return fmt.Errorf("taihu: append short data: size=%d have=%d", size, len(data))
	}
	if aligned == 0 {
		return nil // size == 0：无数据可写
	}

	pos := d.segmentBase(segmentID) + off
	bulkEnd := size &^ (layout.BlockSize - 1) // 4K 倍数的主体逻辑长度
	tailLen := size - bulkEnd                 // 尾部不足一格的字节数 [0, 4096)

	// 路径 1：首地址 4K 对齐 → 主体直写，只分配 4K 临时缓冲处理尾块。
	if bufAligned(data) {
		if bulkEnd > 0 {
			if _, err := d.f.WriteAt(data[:bulkEnd], pos); err != nil {
				return err
			}
		}
		if tailLen > 0 {
			tmp := bufpool.Get(int(layout.BlockSize))
			defer bufpool.Put(tmp)
			n := copy(tmp, data[bulkEnd:size])
			clear(tmp[n:]) // 补齐到整块 4K（tmp 长度恰为 BlockSize）
			if _, err := d.f.WriteAt(tmp, pos+bulkEnd); err != nil {
				return err
			}
		}
		return nil
	}

	// 路径 2：首地址不对齐兜底，整体拷贝进对齐缓冲后两次写出。
	buf := bufpool.Get(int(aligned))
	defer bufpool.Put(buf) // WriteAt 是同步落盘（O_DIRECT），返回后缓冲即可复用
	copy(buf, data[:size])
	clear(buf[size:aligned]) // 末尾 0 填充到 aligned（buf 容量可能大于 aligned）

	// 写 1：向下对齐的主体段（长度为 4K 倍数、偏移对齐，满足 O_DIRECT）。
	if bulkEnd > 0 {
		if _, err := d.f.WriteAt(buf[:bulkEnd], pos); err != nil {
			return err
		}
	}
	// 写 2：尾部不足一格的 4K 补齐块（size 已 4K 对齐时为空）。
	if tailLen > 0 {
		if _, err := d.f.WriteAt(buf[bulkEnd:aligned], pos+bulkEnd); err != nil {
			return err
		}
	}
	return nil
}

// ReadAt 读取段内 off 起 size 字节，经 O_DIRECT 直接读入池化对齐缓冲并返回数据切片。
//
// 要求 off 与 size 均为 4K 对齐（O_DIRECT 约束，由上层 Storage.ReadAt 负责对齐）。
// 返回切片持有一块 bufpool 缓冲：正常情况下 len == size；读到设备/对象末尾不足时
// 返回已读前缀并附 io.EOF。调用方不再使用后必须将返回值原样交还 bufpool.Put（归还池）。
// size == 0 返回 (nil, nil)。
func (d *Device) ReadAt(ctx context.Context, segmentID, off, size int64) ([]byte, error) {
	if off < 0 || off%layout.BlockSize != 0 {
		return nil, fmt.Errorf("taihu: read offset %d not 4K aligned", off)
	}
	if size < 0 || size%layout.BlockSize != 0 {
		return nil, fmt.Errorf("taihu: read size %d not 4K aligned", size)
	}
	if size == 0 {
		return nil, nil
	}

	buf := bufpool.Get(int(size))
	n, err := d.f.ReadAt(buf[:size], d.segmentBase(segmentID)+off)
	if n == 0 {
		bufpool.Put(buf)
		if err == nil {
			err = io.EOF
		}
		return nil, err
	}
	if err != nil && err != io.EOF {
		bufpool.Put(buf)
		return nil, err
	}
	return buf[:n], nil
}

// Delete 将整段标记可回收。当前采用 append-only，物理擦除/重写延迟到 segment 级 GC 实现，
// 此处仅作占位，返回 nil。
func (d *Device) Delete(ctx context.Context, segmentID int64) error {
	_ = segmentID
	return nil
}
