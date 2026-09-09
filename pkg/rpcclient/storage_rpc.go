package rpcclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/pkg/taihu"
	"github.com/liucxer/taihu/rpc"
)

// Storage 远程对象存储实现，方法签名与 taihu.Storage 完全一致（设计文档_v3 §5.1）。
// 内部可持有 1..n 条 gRPC 连接（DialPool），流式 RPC 按 round-robin 分发以提升单进程并发吞吐。
type Storage struct {
	conns []*grpc.ClientConn
	cs    []rpc.ObjectStoreClient
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
	s.cs = nil
	return first
}

// Put 流式上传对象。首帧发送 key+size，后续从 in 读取数据块发送。
func (s *Storage) Put(ctx context.Context, key string, size int64, in io.Reader) error {
	stream, err := s.pick().Put(ctx)
	if err != nil {
		return rpcToErr(err)
	}
	if err := stream.Send(&rpc.PutChunk{Header: &rpc.PutHeader{Key: key, Size: size}}); err != nil {
		return rpcToErr(err)
	}
	// 数据帧零拷贝透传：每块取新池缓冲并交予 gRPC（异步写完后经 reclaimBuf 归还 bufpool），
	// 发送返回后不得复用该缓冲。短读（n < chunkSize 且 err == nil）也安全：缓冲已交给 gRPC。
	for {
		buf := bufpool.Get(chunkSize)
		handed := false
		n, rerr := in.Read(buf)
		if n > 0 {
			if serr := stream.SendMsg(&rpc.RawData{Data: buf[:n], Orig: buf}); serr != nil {
				return rpcToErr(serr) // 出错路径不归还：gRPC 可能在内部已 Free
			}
			handed = true
		}
		if rerr == io.EOF {
			if !handed {
				bufpool.Put(buf)
			}
			break
		}
		if rerr != nil {
			if !handed {
				bufpool.Put(buf)
			}
			return rerr
		}
	}
	_, err = stream.CloseAndRecv()
	return rpcToErr(err)
}

// Get 读取对象内 [off, off+size) 子区间，返回只读流（设计文档_v3 §5.2）。
// 返回流必须 Close：内部取消 ctx、终止 Recv goroutine 释放连接资源。
// 同步读取首帧以尽早暴露服务端错误（NotFound / OutOfRange）。
func (s *Storage) Get(ctx context.Context, key string, off, size int64) (io.ReadCloser, error) {
	cctx, cancel := context.WithCancel(ctx)
	stream, err := s.pick().Get(cctx, &rpc.GetReq{Key: key, Off: off, Size: size})
	if err != nil {
		cancel()
		return nil, rpcToErr(err)
	}
	g := &getStream{stream: stream, cancel: cancel}
	g.cond = sync.NewCond(&g.mu)
	// 同步读首帧，尽早暴露服务端错误。
	var first rpc.RawFrame
	if err := stream.RecvMsg(&first); err != nil {
		cancel()
		if err == io.EOF {
			return io.NopCloser(bytes.NewReader(nil)), nil // 空对象
		}
		return nil, rpcToErr(err)
	}
	if first.Remaining() > 0 {
		g.chunk = &first
	}
	go g.pump()
	return g, nil
}

// GetRaw 与 Get 语义一致，但以帧方式返回原始数据，避免 Read 的中间拷贝
// （帧数据单缓冲时零拷贝引用 wire 缓冲）。
//
// 用法：循环调用 Next() 获取下一帧 []byte，返回 io.EOF 结束；上一帧数据在
// 下一次 Next() 后失效（底层缓冲复用），不得长期持有；用毕必须 Close。
func (s *Storage) GetRaw(ctx context.Context, key string, off, size int64) (*RawStream, error) {
	cctx, cancel := context.WithCancel(ctx)
	stream, err := s.pick().Get(cctx, &rpc.GetReq{Key: key, Off: off, Size: size})
	if err != nil {
		cancel()
		return nil, rpcToErr(err)
	}
	g := &getStream{stream: stream, cancel: cancel}
	g.cond = sync.NewCond(&g.mu)
	var first rpc.RawFrame
	if err := stream.RecvMsg(&first); err != nil {
		cancel()
		if err == io.EOF {
			g.done = true // 空对象：Next 直接 EOF
			return &RawStream{g: g}, nil
		}
		return nil, rpcToErr(err)
	}
	if first.Remaining() > 0 {
		g.chunk = &first
	}
	go g.pump()
	return &RawStream{g: g}, nil
}

// RawStream 是 GetRaw 的帧式读取流。
type RawStream struct {
	g    *getStream
	held *rpc.RawFrame // 已交给调用方的帧引用，下次 Next/Close 时 Free
}

// Next 返回下一帧数据；io.EOF 表示流结束。返回的切片在下一次 Next 或 Close 后失效。
func (rs *RawStream) Next() ([]byte, error) {
	rs.g.mu.Lock()
	defer rs.g.mu.Unlock()
	if rs.held != nil {
		rs.held.Free()
		rs.held = nil
	}
	for {
		if rs.g.chunk != nil {
			f := rs.g.chunk
			rs.g.chunk = nil
			rs.g.cond.Broadcast() // 放行 pump 继续 Recv
			rs.held = f
			return f.Data(), nil
		}
		if rs.g.done {
			if rs.g.err != nil {
				return nil, rs.g.err
			}
			return nil, io.EOF
		}
		rs.g.cond.Wait()
	}
}

// Close 释放全部帧引用并终止底层流。
func (rs *RawStream) Close() error {
	rs.g.mu.Lock()
	if rs.held != nil {
		rs.held.Free()
		rs.held = nil
	}
	if rs.g.chunk != nil {
		rs.g.chunk.Free()
		rs.g.chunk = nil
	}
	rs.g.cancel()
	rs.g.cond.Broadcast()
	rs.g.mu.Unlock()
	return nil
}

// getStream 实现 Get 的 io.ReadCloser：pump goroutine 持续 Recv（RawFrame 延迟物化），
// Read 把待消费帧单次拷贝到调用方缓冲（替代 io.Pipe 的双拷贝）；背压由单槽 pending
// 保证（pump 只有在 chunk 被消费后才继续 Recv）。
type getStream struct {
	stream rpc.ObjectStore_GetClient
	cancel context.CancelFunc
	mu     sync.Mutex
	cond   *sync.Cond
	chunk  *rpc.RawFrame // 待消费帧；nil 表示等待更多数据
	err    error         // 终态错误（nil 表示正常 EOF）
	done   bool          // pump 已结束（流结束或出错）
}

// pump 持续 Recv 数据块，直到流结束或出错。
func (g *getStream) pump() {
	defer g.cancel()
	for {
		var f rpc.RawFrame
		if err := g.stream.RecvMsg(&f); err != nil {
			g.mu.Lock()
			if err == io.EOF {
				err = nil
			} else {
				err = rpcToErr(err)
			}
			g.err = err
			g.done = true
			g.cond.Broadcast()
			g.mu.Unlock()
			return
		}
		if f.Len() == 0 {
			continue
		}
		g.mu.Lock()
		for g.chunk != nil {
			g.cond.Wait() // 等消费者取走上一帧（背压）
		}
		g.chunk = &f
		g.cond.Broadcast()
		g.mu.Unlock()
	}
}

func (g *getStream) Read(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for {
		if g.chunk != nil {
			if g.chunk.Remaining() == 0 {
				g.chunk.Free()
				g.chunk = nil
				g.cond.Broadcast()
				continue
			}
			n := g.chunk.CopyTo(p)
			if g.chunk.Remaining() == 0 {
				g.chunk.Free()
				g.chunk = nil
				g.cond.Broadcast() // 放行 pump 继续 Recv
			}
			return n, nil
		}
		if g.done {
			if g.err != nil {
				return 0, g.err
			}
			return 0, io.EOF
		}
		g.cond.Wait()
	}
}

// Close 取消底层流 ctx，终止 pump goroutine。
func (g *getStream) Close() error {
	g.mu.Lock()
	if g.chunk != nil {
		g.chunk.Free()
		g.chunk = nil
	}
	g.cancel()
	g.cond.Broadcast()
	g.mu.Unlock()
	return nil
}

// Delete 删除对象映射；key 不存在时返回 ErrNotFound。
func (s *Storage) Delete(ctx context.Context, key string) error {
	_, err := s.pick().Delete(ctx, &rpc.DeleteReq{Key: key})
	return rpcToErr(err)
}

// Stat 返回对象逻辑大小；key 不存在时返回 ErrNotFound。
func (s *Storage) Stat(ctx context.Context, key string) (int64, error) {
	resp, err := s.pick().Stat(ctx, &rpc.StatReq{Key: key})
	if err != nil {
		return 0, rpcToErr(err)
	}
	return resp.Size, nil
}

// rpcToErr 将 gRPC status 还原为库错误（设计文档_v3 §6）。
func rpcToErr(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	switch st.Code() {
	case codes.NotFound:
		return taihu.ErrNotFound
	case codes.OutOfRange:
		return taihu.ErrInvalidRange
	case codes.ResourceExhausted:
		return errors.New("taihu: resource exhausted")
	default:
		return err
	}
}
