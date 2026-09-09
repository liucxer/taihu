//go:build linux

package taihu

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
)

// openDevice 打开设备：O_RDWR|O_DIRECT。读写均绕过 page cache 做直接 IO。
// 不设 O_SYNC：让 WriteAt 走后端异步批量落盘，避免每次写同步等待确认拖低带宽；
// 需要强制落盘时由存储层显式调用 d.f.Sync()。
func openDevice(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|syscall.O_DIRECT, 0)
}

// read 读取 segmentID 段、段内 4K 对齐起点 off 起 size（4K 对齐）字节。
// 经 O_DIRECT 读入池化对齐缓冲后返回只读流；返回流的 Close 将缓冲归还池。
// 调用方（Storage.Get）已保证 off、size 均为 BlockSize 对齐，且必须 Close。
func (d *Device) read(ctx context.Context, segmentID, off, size int64) (io.ReadCloser, error) {
	pos := d.segmentBase(segmentID) + off
	if pos%BlockSize != 0 || size%BlockSize != 0 {
		return nil, fmt.Errorf("taihu: O_DIRECT read requires %d-aligned pos/size", BlockSize)
	}
	if size == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	buf := bufPool.get(int(size))
	n, err := d.f.ReadAt(buf[:size], pos)
	if err != nil && !errorsIsEOF(err) {
		bufPool.put(buf)
		return nil, err
	}
	return &pooledBufReader{pool: bufPool, buf: buf, r: bytes.NewReader(buf[:n])}, nil
}

// pooledBufReader 持有从池取出的对齐缓冲，Read 透传 bytes.Reader，Close 时归还缓冲。
type pooledBufReader struct {
	pool *alignedBufPool
	buf  []byte
	r    *bytes.Reader
	once sync.Once
}

func (r *pooledBufReader) Read(p []byte) (int, error) { return r.r.Read(p) }

func (r *pooledBufReader) Close() error {
	r.once.Do(func() {
		if r.buf != nil {
			r.pool.put(r.buf)
			r.buf = nil
		}
	})
	return nil
}

func errorsIsEOF(err error) bool {
	return err == io.EOF
}
