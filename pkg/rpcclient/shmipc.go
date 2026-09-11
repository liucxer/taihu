//go:build linux

// 共享内存（shmipc）客户端连接池：与 TCP DialPool 同接口，sessions 映射 -conns。
// 仅同机部署可用（uds 为服务端 unix socket 路径）；跨节点请用 DialPool(TCP)。
package rpcclient

import (
	"context"

	"github.com/liucxer/taihu/internal/transport"
)

// DialShm 建立单会话 shmipc 共享内存连接（= DialShmPool(…,1)）。
func DialShm(ctx context.Context, uds string) (*Storage, error) {
	return DialShmPool(ctx, uds, 1)
}

// DialShmPool 建立 sessions 条 shmipc 会话的共享内存连接池。内部为一个
// transport.ShmConn（SessionManager），RPC 在会话间 round-robin；对上层与
// DialPool 返回的 *Storage 完全一致，TCP/shm 双方案透明切换。
func DialShmPool(ctx context.Context, uds string, sessions int) (*Storage, error) {
	if sessions < 1 {
		sessions = 1
	}
	conn, err := transport.DialShm(uds, sessions)
	if err != nil {
		return nil, err
	}
	return &Storage{conns: []rpcConn{conn}}, nil
}
