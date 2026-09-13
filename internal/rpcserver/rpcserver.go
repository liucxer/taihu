// Package rpcserver 提供 taihu 远程访问层服务端入口（netpoll 传输层，替代 gRPC）。
//
// 单 Storage 实例，对外暴露 4 个 RPC：Put（client 流式上传）、Get（server 流式下发，
// 支持 off/size 区间）、Delete、Stat。传输层实现在 internal/transport（netpoll +
// LinkBuffer 零拷贝），本包仅做薄封装以保持 taihu server 子命令生命周期接口不变
// （Serve/GracefulStop/Stop）。
package rpcserver

import (
	"github.com/liucxer/taihu/internal/transport"
	"github.com/liucxer/taihu/pkg/taihu"
)

// New 构建 ObjectStore 服务端（netpoll EventLoop）。
func New(storage *taihu.Storage) *transport.Server {
	return transport.NewServer(storage)
}
