package rpcclient

import (
	"bytes"
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

// Storage 远程对象存储实现，方法签名与 taihu.Storage 完全一致（设计文档_v3 §5.1）。
type Storage struct {
	conn *grpc.ClientConn
	c    rpc.ObjectStoreClient
}

// Close 关闭底层连接。
func (s *Storage) Close() error {
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}

// Put 流式上传对象。首帧发送 key+size，后续从 in 读取数据块发送。
func (s *Storage) Put(ctx context.Context, key string, size int64, in io.Reader) error {
	stream, err := s.c.Put(ctx)
	if err != nil {
		return rpcToErr(err)
	}
	if err := stream.Send(&rpc.PutChunk{Header: &rpc.PutHeader{Key: key, Size: size}}); err != nil {
		return rpcToErr(err)
	}
	// 发送缓冲从池取，结束后归还，避免每次 Put 分配 256KB。
	buf := bufpool.Get(chunkSize)
	defer bufpool.Put(buf)
	for {
		n, rerr := in.Read(buf[:chunkSize])
		if n > 0 {
			if serr := stream.Send(&rpc.PutChunk{Data: buf[:n]}); serr != nil {
				return rpcToErr(serr)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	_, err = stream.CloseAndRecv()
	return rpcToErr(err)
}

// Get 读取对象内 [off, off+size) 子区间，返回只读流（设计文档_v3 §5.2）。
// 返回流必须 Close：内部取消 ctx、终止 Recv goroutine 释放连接资源。
// 同步读取首帧以尽早暴露服务端错误（NotFound / OutOfRange）。
func (s *Storage) Get(ctx context.Context, key string, off, size int64) (io.ReadCloser, error) {
	cctx, cancel := context.WithCancel(ctx)
	stream, err := s.c.Get(cctx, &rpc.GetReq{Key: key, Off: off, Size: size})
	if err != nil {
		cancel()
		return nil, rpcToErr(err)
	}
	first, err := stream.Recv()
	if err != nil {
		cancel()
		if err == io.EOF {
			return io.NopCloser(bytes.NewReader(nil)), nil // 空对象
		}
		return nil, rpcToErr(err)
	}

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		defer cancel()
		if _, werr := pw.Write(first.GetData()); werr != nil {
			return
		}
		for {
			chunk, rerr := stream.Recv()
			if rerr == io.EOF {
				return
			}
			if rerr != nil {
				pw.CloseWithError(rpcToErr(rerr))
				return
			}
			if _, werr := pw.Write(chunk.GetData()); werr != nil {
				return
			}
		}
	}()
	return &rcCloser{Reader: pr, cancel: cancel}, nil
}

// rcCloser 关闭时取消底层流 ctx，终止 Recv goroutine。
type rcCloser struct {
	io.Reader
	cancel context.CancelFunc
}

func (r *rcCloser) Close() error {
	r.cancel()
	return nil
}

// Delete 删除对象映射；key 不存在时返回 ErrNotFound。
func (s *Storage) Delete(ctx context.Context, key string) error {
	_, err := s.c.Delete(ctx, &rpc.DeleteReq{Key: key})
	return rpcToErr(err)
}

// Stat 返回对象逻辑大小；key 不存在时返回 ErrNotFound。
func (s *Storage) Stat(ctx context.Context, key string) (int64, error) {
	resp, err := s.c.Stat(ctx, &rpc.StatReq{Key: key})
	if err != nil {
		return 0, rpcToErr(err)
	}
	return resp.Size, nil
}

// rpcToErr 将 gRPC status 还原为库错误（设计文档_v3 §6）。
func rpcToErr(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	switch st.Code() {
	case codes.NotFound:
		return taihu.ErrNotFound
	case codes.OutOfRange:
		return taihu.ErrInvalidRange
	case codes.ResourceExhausted:
		return errors.New("taihu: resource exhausted")
	default:
		return err
	}
}