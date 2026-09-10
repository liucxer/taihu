package rpcclient

import (
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/pkg/taihu"
	"github.com/liucxer/taihu/rpc"
)

// Storage 远程对象存储实现（设计文档_v3 §5.1，整对象 []byte 语义）。
// Put/Get/Delete/Stat 与本地 taihu.Storage 同签名（读路径本地为 ReadAt，返回池化缓冲；
// 远端 Get 返回独立 []byte 副本）。内部可持有 1..n 条 gRPC 连接（DialPool），
// 流式 RPC 按 round-robin 分发以提升单进程并发吞吐。
//
// 数据帧仍是 1MiB 分块流式传输（wire 层零拷贝透传），与本地库的整块内存语义等价。
type Storage struct {
	conns []*grpc.ClientConn
	cs    []rpc.ObjectStoreClient
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
	s.cs = nil
	return first
}

// Put 上传对象。首帧发送 key+size，随后将 in[:size] 按 1MiB 分帧发送。
// 每帧取新池缓冲并交予 gRPC（异步写完后经 reclaimBuf 归还 bufpool），
// 发送返回后不得复用该缓冲。in 不足 size 字节时报错（与本地库 ErrShortWrite 语义一致）。
func (s *Storage) Put(ctx context.Context, key string, size int64, in []byte) error {
	if int64(len(in)) < size {
		return taihu.ErrShortWrite
	}
	stream, err := s.pick().Put(ctx)
	if err != nil {
		return rpcToErr(err)
	}
	if err := stream.Send(&rpc.PutChunk{Header: &rpc.PutHeader{Key: key, Size: size}}); err != nil {
		return rpcToErr(err)
	}
	var off int64
	for off < size {
		buf := bufpool.Get(chunkSize)
		end := off + chunkSize
		if end > size {
			end = size
		}
		n := copy(buf, in[off:end])
		if serr := stream.SendMsg(&rpc.RawData{Data: buf[:n], Orig: buf}); serr != nil {
			return rpcToErr(serr) // 出错路径不归还：gRPC 可能在内部已 Free
		}
		off = end
	}
	_, err = stream.CloseAndRecv()
	return rpcToErr(err)
}

// Get 读取对象内 [off, off+size) 子区间并返回整块数据。
// size=-1 表示读到对象结尾（先经 Stat 取对象总长再按无头定长读取）。
//
// 返回值为 bufpool 池化缓冲（4K 对齐，len==size），调用方用毕必须交还
// bufpool.Put(返回值)，否则造成池泄漏——与本地 Storage.ReadAt 语义一致。
// 收流用 RawFrame 延迟物化，每帧单次拷贝进返回缓冲（避免逐帧物化的堆分配）。
func (s *Storage) Get(ctx context.Context, key string, off, size int64) ([]byte, error) {
	if size < 0 {
		// 未知长度：先取对象总长，与服务端 Get size=-1 的截断语义一致。
		total, err := s.Stat(ctx, key)
		if err != nil {
			return nil, err
		}
		size = total - off
	}
	if size < 0 {
		return nil, taihu.ErrInvalidRange
	}
	if size == 0 {
		return nil, nil
	}

	// 池化返回缓冲：复用 bufpool 对齐缓冲，替代每 op make 4M（清零+GC 压力）。
	buf := bufpool.Get(int(size))
	out := buf[:size]
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := s.pick().Get(cctx, &rpc.GetReq{Key: key, Off: off, Size: size})
	if err != nil {
		bufpool.Put(buf)
		return nil, rpcToErr(err)
	}
	var pos int64
	var fast []byte // 单帧零拷贝移交的缓冲（仍须消费流至 EOF 归还流控/收尾）
	for {
		var f rpc.RawFrame
		if err := stream.RecvMsg(&f); err != nil {
			if err == io.EOF {
				break
			}
			if fast != nil {
				bufpool.Put(fast)
			}
			bufpool.Put(buf)
			return nil, rpcToErr(err)
		}
		if f.Remaining() == 0 {
			f.Free()
			continue
		}
		// 单帧即完整对象（收帧缓冲为 tbpool FrameBuffer 时）：零拷贝移交，
		// 免收流聚合拷贝；随后仍消费至 EOF 完成流收尾。
		if pos == 0 && int64(f.Len()) >= size {
			if raw := f.Take(); raw != nil {
				fast = raw[:size]
				pos = size
				continue
			}
		}
		if pos >= size {
			f.Free()
			bufpool.Put(buf)
			return nil, fmt.Errorf("taihu: get stream exceeds requested size %d", size)
		}
		n := f.CopyTo(out[pos:])
		pos += int64(n)
		f.Free()
	}
	if fast != nil {
		bufpool.Put(buf) // 预取的聚合缓冲未用到，归还
		return fast, nil
	}
	if pos != size {
		bufpool.Put(buf)
		return nil, fmt.Errorf("taihu: get short read: got %d want %d", pos, size)
	}
	return out, nil
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
