package rpcclient

import (
	"context"

	"github.com/liucxer/taihu/internal/transport"
	"github.com/liucxer/taihu/pkg/taihu"
)

// Storage 远程对象存储实现（设计文档_v3 §5.1，整对象 []byte 语义）。
// Put/Get/Delete/Stat 与本地 taihu.Storage 同签名（读路径本地为 ReadAt，返回池化缓冲；
// 远端 Get 返回 bufpool 池化缓冲，同样须交还）。内部可持有 1..n 条 netpoll 连接（DialPool），
// RPC 按 round-robin 分发以提升单进程并发吞吐。
//
// 传输层为 internal/transport（netpoll + LinkBuffer 帧协议）：Put 分块零拷贝发送，
// Get 收流单次拷贝进返回缓冲；Get 语义与本地 ReadAt 一致（size=-1 读至结尾）。
type Storage struct {
	conns []*transport.Conn
	rr    uint64 // round-robin 分发计数器（原子）
}

var _ taihu.ObjectStore = (*Storage)(nil)

// Close 关闭全部底层连接。
func (s *Storage) Close() error {
	var first error
	for i := range s.conns {
		if s.conns[i] != nil {
			if err := s.conns[i].Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	s.conns = nil
	return first
}

// Put 上传对象。与本地库同语义：in 不足 size 字节时报 ErrShortWrite。
// transport.Conn 内部按 chunkSize(4MiB) 分帧零拷贝发送。
func (s *Storage) Put(ctx context.Context, key string, size int64, in []byte) error {
	return s.pick().Put(ctx, key, size, in)
}

// Get 读取对象内 [off, off+size) 子区间并返回整块数据；size=-1 读至对象结尾。
//
// 返回 bufpool 池化缓冲（len==size），调用方用毕必须交还 bufpool.Put(返回值)，
// 与本地 Storage.ReadAt 语义一致。收流逐帧单次拷贝进返回缓冲。
func (s *Storage) Get(ctx context.Context, key string, off, size int64) ([]byte, error) {
	return s.pick().Get(ctx, key, off, size)
}

// Delete 删除对象映射；key 不存在时返回 ErrNotFound。
func (s *Storage) Delete(ctx context.Context, key string) error {
	return s.pick().Delete(ctx, key)
}

// Stat 返回对象逻辑大小；key 不存在时返回 ErrNotFound。
func (s *Storage) Stat(ctx context.Context, key string) (int64, error) {
	return s.pick().Stat(ctx, key)
}
