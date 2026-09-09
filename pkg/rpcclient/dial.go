// Package rpcclient 提供 taihu 远程访问层客户端（设计文档_v3 §5）。
// 与本地库 pkg/taihu 同签名，调用方可无感切换本地/远程存储。
package rpcclient

import (
	"context"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/liucxer/taihu/rpc"
)

// chunkSize 与 server 端一致的数据块上限（读写均按此粒度传输）。
const chunkSize = 1 << 20 // 1MiB

// Dial 建立到 taihu-server 的单连接客户端。返回 *Storage，用完须 Close。
// 内网默认 insecure；断线重连由 gRPC 底层处理，多 goroutine 共享同一连接（HTTP/2 多路复用）。
// 使用 RawCodec：数据帧（[]byte/RawData）零拷贝透传，与 server 须同步升级。
func Dial(ctx context.Context, addr string) (*Storage, error) {
	return DialPool(ctx, addr, 1)
}

// DialPool 建立 n 条到 taihu-server 的连接，流式 RPC 按 round-robin 分发到各连接。
// 单进程持有多个 HTTP/2 连接（每连接独立的流控窗口、loopyWriter 写循环与 socket），
// 可让单客户端并发度突破单连接限制，达到多客户端聚合带宽。n < 1 视为 1。
func DialPool(ctx context.Context, addr string, n int) (*Storage, error) {
	return DialPoolWithOptions(ctx, addr, n)
}

// DialPoolWithOptions 在 DialPool 的标准选项基础上追加额外 DialOption（如测试注入 bufconn dialer）。
func DialPoolWithOptions(ctx context.Context, addr string, n int, extra ...grpc.DialOption) (*Storage, error) {
	if n < 1 {
		n = 1
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(chunkSize*16),
			grpc.MaxCallSendMsgSize(chunkSize*16),
			grpc.ForceCodecV2(rpc.RawCodec{}),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	}
	opts = append(opts, extra...)
	s := &Storage{
		conns: make([]*grpc.ClientConn, 0, n),
		cs:    make([]rpc.ObjectStoreClient, 0, n),
	}
	for i := 0; i < n; i++ {
		conn, err := grpc.NewClient(addr, opts...)
		if err != nil {
			_ = s.Close()
			return nil, err
		}
		s.conns = append(s.conns, conn)
		s.cs = append(s.cs, rpc.NewObjectStoreClient(conn))
	}
	return s, nil
}

// pick 按 round-robin 返回一个连接对应的 client。
func (s *Storage) pick() rpc.ObjectStoreClient {
	if len(s.cs) == 1 {
		return s.cs[0]
	}
	return s.cs[atomic.AddUint64(&s.rr, 1)%uint64(len(s.cs))]
}
