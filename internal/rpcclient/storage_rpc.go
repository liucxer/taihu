package rpcclient

import (
	"context"
	"sync"

	"github.com/liucxer/taihu/internal/transport"
)

// rpcConn 一条底层传输连接的统一接口：TCP（transport.Conn，netpoll 帧协议）或
// 共享内存（shmConn，shmipc 流帧协议）均可满足，Storage 按 round-robin 分发到
// 各连接，两方案对上层调用方透明。
type rpcConn interface {
	Put(ctx context.Context, key string, size int64, in []byte) error
	Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error)
	Delete(ctx context.Context, key string) error
	Stat(ctx context.Context, key string) (int64, error)
	Close() error
}

// Storage 远程对象存储实现（设计文档_v3 §5.1，整对象 []byte 语义）。
// 满足 ObjectStore（见 objectstore.go）；Get 返回 (data, release, err)——data 为整块
// 数据，调用方用毕调用 release()（幂等）归还（内部经 bufpool）。内部可持有 1..n 条
// 连接（DialPool：netpoll TCP；DialShmPool：shmipc 共享内存），RPC 按 round-robin
// 分发以提升单进程并发吞吐。
//
// 传输层为 internal/transport（netpoll + LinkBuffer 帧协议 / shmipc 共享内存）：
// Put 分块零拷贝发送，Get 单帧零拷贝移交接收缓冲、跨帧回退为一次汇入；Get 语义与
// 本地 ReadAt 一致（size=-1 读至结尾）。
type Storage struct {
	conns []rpcConn
	rr    uint64 // round-robin 分发计数器（原子）
}

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
// transport.Conn 内部按 ChunkSize(4MiB) 分帧零拷贝发送。
func (s *Storage) Put(ctx context.Context, key string, size int64, in []byte) error {
	return s.pick().Put(ctx, key, size, in)
}

// fdGetter 可选接口：底层连接支持共享内存 fd 交付时实现（仅 shm 连接；
// TCP 连接的接收缓冲不可 splice，不实现，回退 Get 拷贝路径）。
type fdGetter interface {
	GetFd(ctx context.Context, key string, off, size int64) (fd int, foff uint64, data []byte, release func(), err error)
}

// Get 读取对象内 [off, off+size) 子区间并返回整块数据；size=-1 读至对象结尾。
//
// 返回 (data, release, err)：data len==size 为本次调用私有缓冲，调用方用毕必须调用
// release()（幂等）归还（内部经 bufpool，与本地 ReadAt 的 bufpool.Put 语义一致）。
// 单帧零拷贝路径 data 直接引用接收缓冲；否则为一次对齐汇入拷贝，正确性等价。
func (s *Storage) Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error) {
	return s.pick().Get(ctx, key, off, size)
}

// GetFd 读取对象内 [off, off+size) 子区间并尽力交付共享内存 (fd, offset) 零拷贝源
// （供 FUSE 读路径 splice）；底层连接不支持（TCP/多帧）时回退 Get 拷贝路径。
// 返回值语义见 transport.ShmConn.GetFd：fd>0 时 splice(fd, foff, size) 后调用
// release()；fd==0 时 data 为池化拷贝缓冲，用毕 release()。
func (s *Storage) GetFd(ctx context.Context, key string, off, size int64) (int, uint64, []byte, func(), error) {
	c := s.pick()
	if g, ok := c.(fdGetter); ok {
		return g.GetFd(ctx, key, off, size)
	}
	data, rel, err := c.Get(ctx, key, off, size)
	return 0, 0, data, rel, err
}

// batchConn 可选接口：底层连接支持单流多请求批量读写时实现（仅 shm 连接；
// TCP 连接无对应批量帧协议，不实现，Storage 逐 key 回退）。
type batchConn interface {
	GetBatch(ctx context.Context, keys []string, off, size int64) ([][]byte, func(), error)
	GetFdBatch(ctx context.Context, keys []string, off, size int64) ([]*transport.FdBuf, func(), error)
	PutBatch(ctx context.Context, keys []string, size int64, in []byte) error
	PutBatchKeys(ctx context.Context, keys []string, size int64, datas [][]byte) error
}

// connGetFd 对单条底层连接取 fd（不实现 fdGetter 时回退 Get 拷贝路径，等价 Storage.GetFd）。
func connGetFd(c rpcConn, ctx context.Context, key string, off, size int64) (int, uint64, []byte, func(), error) {
	if g, ok := c.(fdGetter); ok {
		return g.GetFd(ctx, key, off, size)
	}
	data, rel, err := c.Get(ctx, key, off, size)
	return 0, 0, data, rel, err
}

// GetBatch 批量读取：在单条流上连发 len(keys) 个 Get 请求并读回全部响应（语义见
// transport.ShmConn.GetBatch）。底层连接不支持批量（TCP）时在同连接上逐 key Get 回退。
// 返回 out[i] 对应 keys[i]；用毕必须调用返回的 release()（幂等）归还全部缓冲。
func (s *Storage) GetBatch(ctx context.Context, keys []string, off, size int64) ([][]byte, func(), error) {
	c := s.pick()
	if bc, ok := c.(batchConn); ok {
		return bc.GetBatch(ctx, keys, off, size)
	}
	out := make([][]byte, len(keys))
	rels := make([]func(), len(keys))
	var once sync.Once
	for i, key := range keys {
		data, rel, err := c.Get(ctx, key, off, size)
		if err != nil {
			once.Do(func() {
				for j := 0; j < i; j++ {
					if rels[j] != nil {
						rels[j]()
					}
				}
			})
			return nil, nil, err
		}
		out[i], rels[i] = data, rel
	}
	return out, func() {
		once.Do(func() {
			for _, rel := range rels {
				if rel != nil {
					rel()
				}
			}
		})
	}, nil
}

// GetFdBatch 批量读取并逐个交付共享内存 fd（splice 零拷贝源），语义见
// transport.ShmConn.GetFdBatch。底层连接不支持（TCP）时并发逐 key GetFd 回退
// （wmu 串行化帧写、streamID 多路复用，同连接并发安全；串行回退会把整批拖成
// len(keys)×RTT，跨机 TCP 下 16 key×25ms≈400ms，并发后 RTT 重叠）。
// 批内各 FdBuf 独立 Release()（推荐）；返回的 release 为整批兜底（调用后不得
// 再使用/释放批内 FdBuf）。
func (s *Storage) GetFdBatch(ctx context.Context, keys []string, off, size int64) ([]*transport.FdBuf, func(), error) {
	c := s.pick()
	if bc, ok := c.(batchConn); ok {
		return bc.GetFdBatch(ctx, keys, off, size)
	}
	out := make([]*transport.FdBuf, len(keys))
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	for i, key := range keys {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			fd, foff, data, rel, err := connGetFd(c, ctx, key, off, size)
			if err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
				return
			}
			out[i] = transport.NewFdBuf(fd, foff, data, rel)
		}(i, key)
	}
	wg.Wait()
	if first != nil {
		for _, b := range out {
			if b != nil {
				b.Release()
			}
		}
		return nil, nil, first
	}
	var once sync.Once
	return out, func() {
		once.Do(func() {
			for _, b := range out {
				if b != nil {
					b.Release()
				}
			}
		})
	}, nil
}

// PutBatch 批量写：单条流连发 len(keys) 个完整 Put（各 key 均写 in 前 size 字节，
// 同内容批量写，语义见 transport.ShmConn.PutBatch）。底层连接不支持（TCP）时
// 在同连接上逐 key Put 回退。
func (s *Storage) PutBatch(ctx context.Context, keys []string, size int64, in []byte) error {
	c := s.pick()
	if bc, ok := c.(batchConn); ok {
		return bc.PutBatch(ctx, keys, size, in)
	}
	for _, key := range keys {
		if err := c.Put(ctx, key, size, in); err != nil {
			return err
		}
	}
	return nil
}

// PutBatchKeys 批量写不同内容的多个对象（每 key 写 datas[i] 前 size 字节，语义见
// transport.ShmConn.PutBatchKeys）。底层连接不支持（TCP）时并发逐 key Put 回退
// （wmu 串行化帧写、streamID 多路复用，同连接并发安全；串行回退 16 key×RTT≈400ms）。
func (s *Storage) PutBatchKeys(ctx context.Context, keys []string, size int64, datas [][]byte) error {
	c := s.pick()
	if bc, ok := c.(batchConn); ok {
		return bc.PutBatchKeys(ctx, keys, size, datas)
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	for i, key := range keys {
		wg.Add(1)
		go func(key string, data []byte) {
			defer wg.Done()
			if err := c.Put(ctx, key, size, data); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(key, datas[i])
	}
	wg.Wait()
	return first
}

// Delete 删除对象映射；key 不存在时返回 ErrNotFound。
func (s *Storage) Delete(ctx context.Context, key string) error {
	return s.pick().Delete(ctx, key)
}

// Stat 返回对象逻辑大小；key 不存在时返回 ErrNotFound。
func (s *Storage) Stat(ctx context.Context, key string) (int64, error) {
	return s.pick().Stat(ctx, key)
}
