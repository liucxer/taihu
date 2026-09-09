//go:build linux

package taihu

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
)

// openDevice 打开设备：O_RDWR|O_DIRECT。读写均绕过 page cache 做直接 IO。
// 不设 O_SYNC：让 WriteAt 走后端异步批量落盘，避免每次写同步等待确认拖低带宽；
// 需要强制落盘时由存储层显式调用 d.f.Sync()。
func openDevice(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|syscall.O_DIRECT, 0)
}

// read 读取 segmentID 段、段内 4K 对齐起点 off 起 size（4K 对齐）字节。
// 经 O_DIRECT 读入对齐缓冲后返回。调用方（Storage.Get）已保证 off、size 均为 BlockSize 对齐。
func (d *Device) read(ctx context.Context, segmentID, off, size int64) (io.Reader, error) {
	pos := d.segmentBase(segmentID) + off
	if pos%BlockSize != 0 || size%BlockSize != 0 {
		return nil, fmt.Errorf("taihu: O_DIRECT read requires %d-aligned pos/size", BlockSize)
	}
	if size == 0 {
		return bytes.NewReader(nil), nil
	}
	buf := alignedBuffer(int(size))
	n, err := d.f.ReadAt(buf, pos)
	if err != nil && !errorsIsEOF(err) {
		return nil, err
	}
	return bytes.NewReader(buf[:n]), nil
}

func errorsIsEOF(err error) bool {
	return err == io.EOF
}