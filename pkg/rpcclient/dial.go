// Package rpcclient 提供 taihu 远程访问层客户端（设计文档_v3 §5，netpoll 传输层改造）。
// 与本地库 pkg/taihu 同签名，调用方可无感切换本地/远程存储。
package rpcclient

import (
	"context"
	"sync/atomic"

	"github.com/liucxer/taihu/internal/transport"
)

// Dial 建立到 taihu-server 的单连接客户端。返回 *Storage，用完须 Close。
func Dial(ctx context.Context, addr string) (*Storage, error) {
	return DialPool(ctx, addr, 1)
}

// DialPool 建立 n 条到 taihu-server 的 netpoll 连接，RPC 按 round-robin 分发到各连接。
// 每连接一条 TCP 链路 + 独立读循环，连接内以 streamID 多路复用多个并发 RPC；
// 多连接可让单客户端并发度突破单连接限制，达到多客户端聚合带宽。n < 1 视为 1。
func DialPool(ctx context.Context, addr string, n int) (*Storage, error) {
	if n < 1 {
		n = 1
	}
	s := &Storage{conns: make([]rpcConn, 0, n)}
	for i := 0; i < n; i++ {
		conn, err := transport.DialClient("tcp", addr)
		if err != nil {
			_ = s.Close()
			return nil, err
		}
		s.conns = append(s.conns, conn)
	}
	return s, nil
}

// pick 按 round-robin 返回一条连接。
func (s *Storage) pick() rpcConn {
	if len(s.conns) == 1 {
		return s.conns[0]
	}
	return s.conns[atomic.AddUint64(&s.rr, 1)%uint64(len(s.conns))]
}
