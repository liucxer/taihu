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
type Storage struct {
	conn *grpc.ClientConn
	c    rpc.ObjectStoreClient
}

// Close 关闭底层连接。
func (s *Storage) Close() error {
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}

// Put 流式上传对象。首帧发送 key+size，后续从 in 读取数据块发送。
func (s *Storage) Put(ctx context.Context, key string, size int64, in io.Reader) error {
	stream, err := s.c.Put(ctx)
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
	stream, err := s.c.Get(cctx, &rpc.GetReq{Key: key, Off: off, Size: size})
	if err != nil {
		cancel()
		return nil, rpcToErr(err)
	}
	g := &getStream{stream: stream, cancel: cancel}
	g.cond = sync.NewCond(&g.mu)
	// 同步读首帧，尽早暴露服务端错误。
	var first []byte
	if err := stream.RecvMsg(&first); err != nil {
		cancel()
		if err == io.EOF {
			return io.NopCloser(bytes.NewReader(nil)), nil // 空对象
		}
		return nil, rpcToErr(err)
	}
	g.chunk = first
	go g.pump()
	return g, nil
}

// getStream 实现 Get 的 io.ReadCloser：pump goroutine 持续 Recv，Read 把待消费
// chunk 单次拷贝到调用方缓冲（替代 io.Pipe 的双拷贝）；背压由单槽 pending 保证
// （pump 只有在 chunk 被消费后才继续 Recv）。
type getStream struct {
	stream rpc.ObjectStore_GetClient
	cancel context.CancelFunc
	mu     sync.Mutex
	cond   *sync.Cond
	chunk  []byte // 待消费的 chunk；nil 表示等待更多数据
	err    error  // 终态错误（nil 表示正常 EOF）
	done   bool   // pump 已结束（流结束或出错）
}

// pump 持续 Recv 数据块，直到流结束或出错。
func (g *getStream) pump() {
	defer g.cancel()
	for {
		var b []byte
		if err := g.stream.RecvMsg(&b); err != nil {
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
		if len(b) == 0 {
			continue
		}
		g.mu.Lock()
		for g.chunk != nil {
			g.cond.Wait() // 等消费者取走上一个 chunk（背压）
		}
		g.chunk = b
		g.cond.Broadcast()
		g.mu.Unlock()
	}
}

func (g *getStream) Read(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for {
		if g.chunk != nil {
			n := copy(p, g.chunk)
			if n == len(g.chunk) {
				g.chunk = nil
				g.cond.Broadcast() // 放行 pump 继续 Recv
			} else {
				g.chunk = g.chunk[n:]
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
	g.cancel()
	return nil
}

// Delete 删除对象映射；key 不存在时返回 ErrNotFound。
func (s *Storage) Delete(ctx context.Context, key string) error {
	_, err := s.c.Delete(ctx, &rpc.DeleteReq{Key: key})
	return rpcToErr(err)
}

// Stat 返回对象逻辑大小；key 不存在时返回 ErrNotFound。
func (s *Storage) Stat(ctx context.Context, key string) (int64, error) {
	resp, err := s.c.Stat(ctx, &rpc.StatReq{Key: key})
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