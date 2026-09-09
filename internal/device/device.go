// Package device 提供底层存储设备访问：直接操作裸设备文件，按 segment 提供 append/read。
//
// 单个文件句柄，同时用于读写。Linux 上以 O_DIRECT 打开（绕过 page cache），
// 此时读写缓冲、文件偏移、每次 IO 长度都必须 layout.BlockSize(4K) 对齐：
//   - 读：由 Storage.Get 保证 off/size 对齐，Read 读入对齐缓冲返回；
//   - 写：Append 将对象以整块 4K 对齐写出，末尾用 0 补齐到 4K。
//
// WriteAt / ReadAt 语义保证不同偏移的并发读写安全。
package device

import (
	"context"
	"fmt"
	"io"
	"os"

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

// Append 将 data 的 size 字节写入 segmentID 段、段内 offset 处，末尾用 0 补齐到 4K。
//
// offset 必须 4K 对齐。为了把写 IO 次数压到 ≤2，先将整个对象读入一块对齐内存，
// 再发出 O_DIRECT 写；长度 4K 对齐时仅 1 次 WriteAt，不对齐时 2 次（对齐主体 + 尾部补齐块）：
//   - 主体：size 向下 4K 对齐的一段（长度 4K 倍数、偏移对齐）→ 1 次；
//   - 尾部：剩余 <4K 的字节补零成 1 个 4K 块 → 1 次（size 已对齐时为空）。
//
// 代价：整对象需要暂存于内存（对象上限为单 segment 8GB），换取确定性的 ≤2 次写 IO。
// data 被读取不足 size 字节时返回 ErrShortWrite。
func (d *Device) Append(ctx context.Context, segmentID, off, size int64, data io.Reader) error {
	if off < 0 || off%layout.BlockSize != 0 {
		return fmt.Errorf("taihu: append offset %d not 4K aligned", off)
	}
	aligned := layout.Align4k(size)
	if off+aligned > layout.SegmentSizeBytes {
		return ierr.ErrTooLarge
	}

	pos := d.segmentBase(segmentID) + off
	buf := bufpool.Get(int(aligned))
	defer bufpool.Put(buf) // WriteAt 是同步落盘（O_DIRECT），返回后缓冲即可复用
	if _, err := io.ReadFull(data, buf[:size]); err != nil {
		return fmt.Errorf("taihu: append short read: %w", err)
	}
	clear(buf[size:aligned]) // 末尾 0 填充到 aligned（buf 容量可能大于 aligned）

	// 写 1：向下对齐的主体段（长度为 4K 倍数、偏移对齐，满足 O_DIRECT）。
	if bulkEnd := size &^ (layout.BlockSize - 1); bulkEnd > 0 {
		if _, err := d.f.WriteAt(buf[:bulkEnd], pos); err != nil {
			return err
		}
	}
	// 写 2：尾部不足一格的 4K 补齐块（size 已 4K 对齐时为空）。
	if tailStart := size &^ (layout.BlockSize - 1); tailStart < aligned {
		if _, err := d.f.WriteAt(buf[tailStart:aligned], pos+tailStart); err != nil {
			return err
		}
	}
	return nil
}

// ReadAt 将段内 off 处至多 len(buf) 字节读入 buf，返回实际读入字节数（单次 O_DIRECT ReadAt）。
//
// 要求 off 与 len(buf) 均为 4K 对齐；buf 起始地址须 4K 对齐（O_DIRECT 约束，
// bufpool.Get 保证）。用于 Storage.ReadAt 快路径：O_DIRECT 直接读入发送缓冲。
func (d *Device) ReadAt(ctx context.Context, segmentID, off int64, buf []byte) (int, error) {
	if off < 0 || off%layout.BlockSize != 0 {
		return 0, fmt.Errorf("taihu: read offset %d not 4K aligned", off)
	}
	if int64(len(buf))%layout.BlockSize != 0 {
		return 0, fmt.Errorf("taihu: read length %d not 4K aligned", len(buf))
	}
	return d.f.ReadAt(buf, d.segmentBase(segmentID)+off)
}

// Delete 将整段标记可回收。当前采用 append-only，物理擦除/重写延迟到 segment 级 GC 实现，
// 此处仅作占位，返回 nil。
func (d *Device) Delete(ctx context.Context, segmentID int64) error {
	_ = segmentID
	return nil
}