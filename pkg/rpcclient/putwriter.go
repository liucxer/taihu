// 共享内存零拷贝写包装（跨平台类型定义）：PutWriter 包装传输层写流接口，
// NewPut 仅在 linux（shmipc 支持）实现，非 linux 返回 ErrShmOnly。
package rpcclient

import (
	"context"
	"errors"
)

// ErrShmOnly 零拷贝写（NewPut）仅共享内存连接支持。
var ErrShmOnly = errors.New("taihu: zero-copy write requires shm connection")

// putStream 传输层零拷贝写流接口（linux 由 transport.ShmPutWriter 实现）。
type putStream interface {
	Reserve(n int) ([]byte, error)
	Commit() error
}

// PutWriter 共享内存零拷贝写（rpcclient 层封装）。
// NewPut 后 Reserve 拿共享内存可写区直写（免 memcpy），Commit 收尾。
type PutWriter struct {
	w putStream
}

// Reserve 返回 n 字节共享内存可写区（零拷贝直写，4K 对齐）。
func (w *PutWriter) Reserve(n int) ([]byte, error) { return w.w.Reserve(n) }

// Commit 结束写入（发 opPutEnd 收响应）。
func (w *PutWriter) Commit() error { return w.w.Commit() }

// NewPut 开始零拷贝写对象（仅 shm 连接支持；TCP/非 linux 返回 ErrShmOnly）。
// 单次 Reserve 不超过 4MiB，更大对象分块多次 Reserve。
func (s *Storage) NewPut(ctx context.Context, key string, size int64) (*PutWriter, error) {
	if len(s.conns) == 0 {
		return nil, errors.New("taihu: storage closed")
	}
	return newShmPut(ctx, s, key, size)
}
