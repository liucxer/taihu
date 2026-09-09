//go:build !linux

package device

import (
	"context"
	"io"
	"os"
)

// openDevice 打开设备：O_RDWR|O_SYNC。非 Linux 平台无 O_DIRECT，退回普通直接打开。
func openDevice(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_SYNC, 0)
}

// Read 读取 segmentID 段、段内 offset 起 size 字节的只读流。
// 非 Linux 平台为 byte 级随机读，无需对齐，也不池化缓冲。
func (d *Device) Read(ctx context.Context, segmentID, off, size int64) (io.ReadCloser, error) {
	pos := d.segmentBase(segmentID) + off
	return io.NopCloser(io.NewSectionReader(d.f, pos, size)), nil
}