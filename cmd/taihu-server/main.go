// Command taihu-server 是 taihu 远程访问层的 gRPC 服务端（设计文档_v3 §4）。
//
// 单 Storage 实例（-db pebble 目录 + -dev 裸设备），对外暴露 4 个 RPC：
// Put（client 流式上传）、Get（server 流式下发，支持 off/size 区间）、Delete、Stat。
// 4K 对齐等存储细节由底层库内部吸收，服务端只做流与错误映射。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/pkg/taihu"
	"github.com/liucxer/taihu/rpc"
)

// chunkSize 单条 Get 响应数据块大小（1MiB，兼顾吞吐与流控）。
const chunkSize = 256 * 1024

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

// streamReader 把 gRPC 入流包装为 io.Reader：Read 阻塞在下一个 Recv，
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
		ch, err := r.stream.Recv()
		if err != nil {
			return 0, err // io.EOF → Storage.Put 读满 size 即收尾
		}
		d := ch.GetData()
		if len(d) == 0 {
			continue // 仅 header 帧
		}
		n := copy(p, d)
		if n < len(d) {
			r.pending = d[n:]
		}
		return n, nil
	}
}

// Get 流式下发 [off, off+size) 区间；size=-1 表示读到对象结尾。
func (s *server) Get(req *rpc.GetReq, stream rpc.ObjectStore_GetServer) error {
	size := req.Size
	if size == -1 {
		total, err := s.storage.Stat(stream.Context(), req.Key)
		if err != nil {
			return mapStorageErr(err)
		}
		size = total - req.Off
	}
	rc, err := s.storage.Get(stream.Context(), req.Key, req.Off, size)
	if err != nil {
		return mapStorageErr(err)
	}
	defer rc.Close()

	// 读缓冲从池取，结束后归还，避免每 RPC 分配 256KB。
	buf := bufpool.Get(chunkSize)
	defer bufpool.Put(buf)

	for {
		n, rerr := rc.Read(buf[:chunkSize])
		if n > 0 {
			if serr := stream.Send(&rpc.GetChunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
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

func main() {
	var (
		addr = flag.String("addr", ":50051", "listen address")
		db   = flag.String("db", "", "pebble metadata directory (required)")
		dev  = flag.String("dev", "", "raw device path (required)")
	)
	flag.Parse()
	if *db == "" || *dev == "" {
		fmt.Fprintln(os.Stderr, "taihu-server: -db and -dev are required")
		os.Exit(2)
	}

	storage, err := taihu.NewStorage(context.Background(), *db, *dev)
	if err != nil {
		log.Fatalf("NewStorage: %v", err)
	}
	defer storage.Close()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}

	gs := grpc.NewServer(
		// 流控参数（设计文档_v3 §4.3）：消息上限 ≥ chunk，窗口放大提升并发流吞吐
		grpc.MaxRecvMsgSize(chunkSize*16),
		grpc.MaxSendMsgSize(chunkSize*16),
		grpc.InitialWindowSize(1<<20), // 1MB 流窗口
		grpc.InitialConnWindowSize(4<<20), // 4MB 连接窗口
	)
	rpc.RegisterObjectStoreServer(gs, &server{storage: storage})

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutting down...")
		gs.GracefulStop()
	}()

	log.Printf("taihu-server listening on %s (db=%s dev=%s)", *addr, *db, *dev)
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}