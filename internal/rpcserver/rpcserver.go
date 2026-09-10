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

// chunkSize 单条数据帧大小（最大 4MiB，兼顾吞吐与流控），与 rpcclient 侧一致。
const chunkSize = 1 << 22 // 4MiB

// New 构建并注册 ObjectStore gRPC 服务，返回可 Serve 的 *grpc.Server。
// 流控与 codec 参数集中在此，保证服务端与集成测试配置一致。
func New(storage *taihu.Storage) *grpc.Server {
	gs := grpc.NewServer(
		// 协议参数：消息上限 = 单帧大小（帧最大 4MiB），双端须同步升级
		grpc.MaxRecvMsgSize(chunkSize*2),
		grpc.MaxSendMsgSize(chunkSize*2),
		grpc.InitialWindowSize(16<<20),      // 16MB 流窗口（≥ 4 帧 4MiB 在途/流，提升单流磁盘并发）
		grpc.InitialConnWindowSize(256<<20), // 256MB 连接窗口（多流共享）
		// 传输缓冲（实测热点）：syscall write 占 CPU 57%，默认 32KB 写缓冲 → 每次
		// syscall 仅搬 32KB；放大到帧大小后每帧一次 syscall，大幅削减系统调用次数。
		grpc.WriteBufferSize(1<<20),
		grpc.ReadBufferSize(1<<20),
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

// Put 流式上传：首帧 PutHeader{key,size} 后为数据块；数据帧逐块汇入 bufpool 缓冲，
// 收满声明 size 后整块交给 Storage.Put（内部再拷贝进 O_DIRECT 对齐缓冲完成落盘）。
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
	if h.Size == 0 {
		if err := s.storage.Put(stream.Context(), h.Key, 0, nil); err != nil {
			return mapStorageErr(err)
		}
		return stream.SendAndClose(&rpc.PutResp{})
	}

	// 整对象先汇入 bufpool 缓冲（数据帧解码缓冲不保证对齐，最终由 Storage.Put 拷贝进对齐缓冲
	// 落盘；bufpool 缓冲首地址 4K 对齐，size 为 4K 倍数时 device Append 走直写快路径）。
	// 收帧用 RawFrame 延迟物化：单缓冲帧零拷贝 Ref、多缓冲帧 bufpool 合并，
	// 相比 *[]byte 的 Materialize 消除每帧 1MiB 堆分配，wire→buf 仅一次对齐拷贝。
	buf := bufpool.Get(int(h.Size))
	defer bufpool.Put(buf)
	var pos int64
	for pos < h.Size {
		var f rpc.RawFrame
		if err := stream.RecvMsg(&f); err != nil {
			if err == io.EOF {
				return status.Error(codes.InvalidArgument, "taihu: put stream shorter than declared size")
			}
			return err
		}
		if f.Remaining() == 0 {
			f.Free()
			continue
		}
		if pos+int64(f.Remaining()) > h.Size {
			f.Free()
			return status.Error(codes.InvalidArgument, "taihu: put stream exceeds declared size")
		}
		pos += int64(f.CopyTo(buf[pos:]))
		f.Free()
	}
	if err := s.storage.Put(stream.Context(), h.Key, h.Size, buf[:h.Size]); err != nil {
		return mapStorageErr(err)
	}
	return stream.SendAndClose(&rpc.PutResp{})
}

// Get 流式下发 [off, off+size) 区间；size=-1 表示读到对象结尾。
// 零拷贝读路径：Storage.ReadAt 经 O_DIRECT 直读 bufpool 缓冲并返回该切片，
// SendMsg(RawData{Data:data, Orig:data}) 零拷贝透传（写完后 gRPC 归还 bufpool）。
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
		want := end - pos
		if want > chunkSize {
			want = chunkSize
		}
		data, rerr := s.storage.ReadAt(stream.Context(), req.Key, pos, want)
		if len(data) > 0 {
			if serr := stream.SendMsg(&rpc.RawData{Data: data, Orig: data}); serr != nil {
				return serr // 出错路径不归还：gRPC 可能在内部已 Free
			}
			pos += int64(len(data))
		}
		if rerr == io.EOF {
			if len(data) == 0 {
				bufpool.Put(data)
			}
			return nil
		}
		if rerr != nil {
			if len(data) == 0 {
				bufpool.Put(data)
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
