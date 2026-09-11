//go:build linux

// 共享内存零拷贝写（linux，shmipc 支持）：newShmPut 断言连接为 transport.ShmConn
// 并调用其 PutBegin（发 opPutHeader），返回包装后的 PutWriter。
package rpcclient

import (
	"context"

	"github.com/liucxer/taihu/internal/transport"
)

func newShmPut(ctx context.Context, s *Storage, key string, size int64) (*PutWriter, error) {
	conn, ok := s.conns[0].(*transport.ShmConn)
	if !ok {
		return nil, ErrShmOnly
	}
	w, err := conn.PutBegin(ctx, key, size)
	if err != nil {
		return nil, err
	}
	return &PutWriter{w: w}, nil
}
