// Package rpcserver 实现 taihu 远程访问层的 gRPC 服务端（设计文档_v3 §4）。
//
// 单 Storage 实例，对外暴露 4 个 RPC：Put（client 流式上传）、Get（server 流式下发，
// 支持 off/size 区间）、Delete、Stat。4K 对齐等存储细节由底层库内部吸收，
// 服务端只做流与错误映射。
package rpcserver

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/pkg/taihu"
	"github.com/liucxer/taihu/rpc"
)

// chunkSize 单条数据帧大小（1MiB，兼顾吞吐与流控），与 rpcclient 侧一致。
const chunkSize = 1 << 20

// New 构建并注册 ObjectStore gRPC 服务，返回可 Serve 的 *grpc.Server。
// 流控与 codec 参数集中在此，保证服务端与集成测试配置一致。
func New(storage *taihu.Storage) *grpc.Server {
	gs := grpc.NewServer(
		// 流控参数（设计文档_v3 §4.3）：消息上限 ≥ chunk，窗口放大提升并发流吞吐
		grpc.MaxRecvMsgSize(chunkSize*16),
		grpc.MaxSendMsgSize(chunkSize*16),
		grpc.InitialWindowSize(4<<20),      // 4MB 流窗口（≥ 4 帧 1MiB 在途）
		grpc.InitialConnWindowSize(64<<20), // 64MB 连接窗口（多流共享）
		grpc.ForceServerCodecV2(rpc.RawCodec{}), // 数据帧裸字节透传（与 client 同步升级）
	)
	rpc.RegisterObjectStoreServer(gs, &server{storage: storage})
	return gs
}

// server 实现 rpc.ObjectStoreServer，持有单个 Storage。
type server struct {
	rpc.UnimplementedObjectStoreServer
	storage *taihu.Storage
}

// Put 流式上传：首帧 PutHeader{key,size} 后为数据块；数据经 streamReader 喂给 Storage.Put。
func (s *server) Put(stream rpc.ObjectStore_PutServer) error {
	first, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return status.Error(codes.InvalidArgument, "taihu: empty put stream, header expected")
		}
		return err
	}
	h := first.GetHeader()
	if h == nil {
		return status.Error(codes.InvalidArgument, "taihu: first frame must carry header")
	}
	if h.Size < 0 {
		return status.Error(codes.InvalidArgument, "taihu: negative size")
	}
	if h.Size > taihu.SegmentSizeBytes {
		return status.Error(codes.ResourceExhausted, "taihu: object too large, exceeds segment size")
	}
	if err := s.storage.Put(stream.Context(), h.Key, h.Size, &streamReader{stream: stream}); err != nil {
		return mapStorageErr(err)
	}
	return stream.SendAndClose(&rpc.PutResp{})
}

// streamReader 把 gRPC 入流包装为 io.Reader：Read 阻塞在下一个 RecvMsg（数据帧为裸字节），
// 免去 io.Pipe/额外 goroutine，天然流式且无泄漏。
type streamReader struct {
	stream  rpc.ObjectStore_PutServer
	pending []byte
}

func (r *streamReader) Read(p []byte) (int, error) {
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	for {
		var b []byte
		if err := r.stream.RecvMsg(&b); err != nil {
			return 0, err // io.EOF → Storage.Put 读满 size 即收尾
		}
		if len(b) == 0 {
			continue
		}
		n := copy(p, b)
		if n < len(b) {
			r.pending = b[n:]
		}
		return n, nil
	}
}

// Get 流式下发 [off, off+size) 区间；size=-1 表示读到对象结尾。
// 零拷贝读路径：Storage.ReadAt 把数据 O_DIRECT 直读入 bufpool 缓冲，SendMsg([]byte)
// 经 RawCodec 零拷贝透传（写完后缓冲由 gRPC 归还 bufpool），每块取新缓冲、不发复用。
func (s *server) Get(req *rpc.GetReq, stream rpc.ObjectStore_GetServer) error {
	size := req.Size
	if size == -1 {
		total, err := s.storage.Stat(stream.Context(), req.Key)
		if err != nil {
			return mapStorageErr(err)
		}
		size = total - req.Off
	}
	if size < 0 {
		return mapStorageErr(taihu.ErrInvalidRange)
	}
	pos, end := req.Off, req.Off+size
	for pos < end {
		buf := bufpool.Get(chunkSize)
		want := end - pos
		if want > chunkSize {
			want = chunkSize
		}
		n, rerr := s.storage.ReadAt(stream.Context(), req.Key, pos, buf[:want])
		handed := false
		if n > 0 {
			if serr := stream.SendMsg(&rpc.RawData{Data: buf[:n], Orig: buf}); serr != nil {
				return serr // 出错路径不归还：gRPC 可能在内部已 Free
			}
			handed = true
			pos += int64(n)
		}
		if rerr == io.EOF {
			if !handed {
				bufpool.Put(buf)
			}
			return nil
		}
		if rerr != nil {
			if !handed {
				bufpool.Put(buf)
			}
			return rerr
		}
	}
	return nil
}

func (s *server) Delete(ctx context.Context, req *rpc.DeleteReq) (*rpc.DeleteResp, error) {
	if err := s.storage.Delete(ctx, req.Key); err != nil {
		return nil, mapStorageErr(err)
	}
	return &rpc.DeleteResp{}, nil
}

func (s *server) Stat(ctx context.Context, req *rpc.StatReq) (*rpc.StatResp, error) {
	size, err := s.storage.Stat(ctx, req.Key)
	if err != nil {
		return nil, mapStorageErr(err)
	}
	return &rpc.StatResp{Size: size}, nil
}

// mapStorageErr 将库错误映射为 gRPC status（设计文档_v3 §6）。
func mapStorageErr(err error) error {
	switch {
	case errors.Is(err, taihu.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, taihu.ErrInvalidRange):
		return status.Error(codes.OutOfRange, err.Error())
	case errors.Is(err, taihu.ErrTooLarge), errors.Is(err, taihu.ErrNoSpace):
		return status.Error(codes.ResourceExhausted, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
