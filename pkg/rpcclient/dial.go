// Package rpcclient 提供 taihu 远程访问层客户端（设计文档_v3 §5）。
// 与本地库 pkg/taihu 同签名，调用方可无感切换本地/远程存储。
package rpcclient

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/liucxer/taihu/rpc"
)

// chunkSize 与 server 端一致的数据块上限（读写均按此粒度传输）。
const chunkSize = 256 * 1024

// Dial 建立到 taihu-server 的连接。返回 *Storage，用完须 Close。
// 内网默认 insecure；断线重连由 gRPC 底层处理，多 goroutine 共享同一连接（HTTP/2 多路复用）。
func Dial(ctx context.Context, addr string) (*Storage, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(chunkSize*16),
			grpc.MaxCallSendMsgSize(chunkSize*16),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, err
	}
	return &Storage{conn: conn, c: rpc.NewObjectStoreClient(conn)}, nil
}