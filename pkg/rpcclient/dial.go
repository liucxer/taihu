// Package rpcclient 提供 taihu 客户端（设计文档_v3 §5，netpoll 传输层改造）。
// 本包是直连客户端（连一个已知实例）；集群路由客户端见 pkg/rpccluster。
// 两者方法集一致、共同满足本包 ObjectStore，调用方可在直连/集群间无感切换。
package rpcclient

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/liucxer/taihu/internal/transport"
)

// Dial 建立到 taihu-server 的单连接客户端。返回 *Storage，用完须 Close。
func Dial(ctx context.Context, addr string) (*Storage, error) {
	return DialPool(ctx, addr, 1)
}

// DialPool 建立 n 条到 taihu-server（单地址）的 netpoll 连接，RPC 按 round-robin 分发到各连接。
// 每连接一条 TCP 链路 + 独立读循环，连接内以 streamID 多路复用多个并发 RPC；
// 多连接可让单客户端并发度突破单连接限制，达到多客户端聚合带宽。n < 1 视为 1。
func DialPool(ctx context.Context, addr string, n int) (*Storage, error) {
	return DialPoolMulti(ctx, []string{addr}, n)
}

// DialPoolMulti 建立到 taihu-server 的多地址连接池：对 addrs 中每个地址各建 perAddr 条
// netpoll TCP 连接，全部塞入同一 Storage.conns。Storage.pick() 的 round-robin 会自动在
// 「地址数 × perAddr」全部连接上均分——服务端多 IP 监听时，读写请求按 IP 均分到各地址链路。
// 任一地址拨号失败即整体返回错误（并关闭已建连接），与单地址 DialPool 语义一致。
// addrs 为空时返回明确错误（避免构造出空连接池导致 pick() 越界）。
func DialPoolMulti(ctx context.Context, addrs []string, perAddr int) (*Storage, error) {
	if len(addrs) == 0 {
		return nil, fmt.Errorf("DialPoolMulti: empty addrs")
	}
	if perAddr < 1 {
		perAddr = 1
	}
	s := &Storage{conns: make([]rpcConn, 0, len(addrs)*perAddr)}
	for _, addr := range addrs {
		for i := 0; i < perAddr; i++ {
			conn, err := transport.DialClient("tcp", addr)
			if err != nil {
				_ = s.Close()
				return nil, err
			}
			s.conns = append(s.conns, conn)
		}
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
