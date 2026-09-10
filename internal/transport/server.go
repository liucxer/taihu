package transport

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"github.com/cloudwego/netpoll"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/pkg/taihu"
)

// Server 基于 netpoll EventLoop 的 taihu RPC 服务端（对应旧 gRPC rpcserver.New 的
// Serve/GracefulStop/Stop 生命周期）。每连接一个读循环，帧按 streamID 分发给
// 独立 goroutine 处理器，Put/Get/Delete/Stat 语义与旧 gRPC 服务端一致。
type Server struct {
	storage *taihu.Storage

	mu sync.Mutex
	el netpoll.EventLoop
}

// NewServer 构建服务端。
func NewServer(storage *taihu.Storage) *Server {
	return &Server{storage: storage}
}

// Serve 在 listener 上提供服务（阻塞直至 Shutdown/异常）。
func (s *Server) Serve(ln net.Listener) error {
	el, err := netpoll.NewEventLoop(func(ctx context.Context, c netpoll.Connection) error {
		return s.serveConn(ctx, c)
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.el = el
	s.mu.Unlock()
	return el.Serve(ln)
}

// GracefulStop 优雅停机：等待在途连接处理完毕。
func (s *Server) GracefulStop() {
	s.mu.Lock()
	el := s.el
	s.mu.Unlock()
	if el == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = el.Shutdown(ctx)
}

// Stop 强制停机。
func (s *Server) Stop() {
	s.GracefulStop()
}

// serveConn 单个连接的服务入口：启动帧读循环（阻塞直至连接关闭）。
func (s *Server) serveConn(_ context.Context, c netpoll.Connection) error {
	conn := &Conn{
		c:        c,
		streams:  make(map[uint32]*stream),
		closed:   make(chan struct{}),
		dispatch: s.dispatch,
	}
	conn.readLoop()
	return nil
}

// dispatch 服务端分发：请求首帧开流并启动处理器 goroutine；后续帧按 streamID
// 投递给对应流。未知流数据帧视为协议错误（关闭连接）。
func (s *Server) dispatch(c *Conn, sid uint32, op OpCode, sub netpoll.Reader) deliverResult {
	var started bool
	c.mu.Lock()
	st := c.streams[sid]
	if st == nil {
		switch op {
		case opPutHeader, opGetReq, opDelReq, opStatReq:
			st = newStream(sid)
			c.streams[sid] = st
			started = true
		default:
			c.mu.Unlock()
			_ = sub.Release()
			return deliverFatal // 未知流首帧：协议错误
		}
	}
	c.mu.Unlock()

	select {
	case st.in <- frameMsg{op, sub}:
	case <-st.done:
		_ = sub.Release()
		return deliverFatal // 处理器已结束仍收到该流帧：协议错误
	case <-c.closed:
		_ = sub.Release()
		return deliverFatal
	}

	if started {
		switch op {
		case opPutHeader:
			go s.handlePut(c, st)
		case opGetReq:
			go s.handleGet(c, st)
		case opDelReq:
			go s.handleDelete(c, st)
		case opStatReq:
			go s.handleStat(c, st)
		}
	}
	return deliverOK
}

// endStream 处理器收尾：注销流并解除读循环可能存在的阻塞投递。
func (c *Conn) endStream(st *stream) {
	c.removeStream(st)
}

// handlePut 处理 Put 流：首帧 PutHeader{key,size}，随后 PutData 帧汇入 bufpool 缓冲，
// PutEnd 后整块交给 Storage.Put（device 内部拷贝进 O_DIRECT 对齐缓冲落盘）。
func (s *Server) handlePut(c *Conn, st *stream) {
	defer c.endStream(st)

	first, err := c.await(context.Background(), st)
	if err != nil {
		return
	}
	key, size, err := parsePutHeader(first.r)
	first.r.Release()
	if err != nil {
		_ = c.writeFrame(st.id, opResp, encCode(codeInvalidArgument))
		return
	}
	if size < 0 {
		_ = c.writeFrame(st.id, opResp, encCode(codeInvalidArgument))
		return
	}
	if size > taihu.SegmentSizeBytes {
		_ = c.writeFrame(st.id, opResp, encCode(codeTooLarge))
		return
	}
	if size == 0 {
		if err := s.storage.Put(context.Background(), key, 0, nil); err != nil {
			_ = c.writeFrame(st.id, opResp, encCode(mapStorageErr(err)))
			return
		}
		_ = c.writeFrame(st.id, opResp, encCode(codeOK))
		return
	}

	buf := bufpool.Get(int(size))
	defer bufpool.Put(buf)
	var pos int
	for {
		msg, err := c.await(context.Background(), st)
		if err != nil {
			return
		}
		switch msg.op {
		case opPutData:
			rem := msg.r.Len()
			if pos+rem > int(size) {
				msg.r.Release()
				_ = c.writeFrame(st.id, opResp, encCode(codeInvalidArgument))
				return
			}
			p, err := msg.r.Next(rem)
			if err != nil {
				msg.r.Release()
				_ = c.writeFrame(st.id, opResp, encCode(codeInternal))
				return
			}
			pos += copy(buf[pos:], p)
			msg.r.Release()
		case opPutEnd:
			msg.r.Release()
			if pos != int(size) {
				_ = c.writeFrame(st.id, opResp, encCode(codeInvalidArgument))
				return
			}
			if err := s.storage.Put(context.Background(), key, size, buf); err != nil {
				_ = c.writeFrame(st.id, opResp, encCode(mapStorageErr(err)))
				return
			}
			_ = c.writeFrame(st.id, opResp, encCode(codeOK))
			return
		default:
			msg.r.Release()
			_ = c.writeFrame(st.id, opResp, encCode(codeInvalidArgument))
			return
		}
	}
}

// handleGet 处理 Get 请求：按 chunkSize 分块 ReadAt 下发 opGetData，收尾 opGetEnd；
// 出错发 opGetErr（带错误码）。数据帧零拷贝引用 ReadAt 返回的 bufpool 缓冲，
// writeFrame 返回（Flush 排空）后归还缓冲。
func (s *Server) handleGet(c *Conn, st *stream) {
	defer c.endStream(st)

	first, err := c.await(context.Background(), st)
	if err != nil {
		return
	}
	key, off, size, err := parseGetReq(first.r)
	first.r.Release()
	if err != nil {
		_ = c.writeFrame(st.id, opGetErr, encCode(codeInvalidArgument))
		return
	}
	if size == -1 {
		total, err := s.storage.Stat(context.Background(), key)
		if err != nil {
			_ = c.writeFrame(st.id, opGetErr, encCode(mapStorageErr(err)))
			return
		}
		size = total - off
	}
	if size < 0 {
		_ = c.writeFrame(st.id, opGetErr, encCode(codeInvalidRange))
		return
	}
	pos, end := off, off+size
	for pos < end {
		want := end - pos
		if want > chunkSize {
			want = chunkSize
		}
		data, rerr := s.storage.ReadAt(context.Background(), key, pos, want)
		if len(data) > 0 {
			if serr := c.writeFrame(st.id, opGetData, data); serr != nil {
				bufpool.Put(data)
				return
			}
			bufpool.Put(data)
			pos += int64(len(data))
		}
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			_ = c.writeFrame(st.id, opGetErr, encCode(mapStorageErr(rerr)))
			return
		}
	}
	_ = c.writeFrame(st.id, opGetEnd, nil)
}

// handleDelete 处理 Delete 请求（一元）。
func (s *Server) handleDelete(c *Conn, st *stream) {
	defer c.endStream(st)

	first, err := c.await(context.Background(), st)
	if err != nil {
		return
	}
	key, err := parseKeyReq(first.r)
	first.r.Release()
	if err != nil {
		_ = c.writeFrame(st.id, opResp, encCode(codeInvalidArgument))
		return
	}
	if err := s.storage.Delete(context.Background(), key); err != nil {
		_ = c.writeFrame(st.id, opResp, encCode(mapStorageErr(err)))
		return
	}
	_ = c.writeFrame(st.id, opResp, encCode(codeOK))
}

// handleStat 处理 Stat 请求（一元）：成功回 opStatResp{size}，失败回 opResp{code}。
func (s *Server) handleStat(c *Conn, st *stream) {
	defer c.endStream(st)

	first, err := c.await(context.Background(), st)
	if err != nil {
		return
	}
	key, err := parseKeyReq(first.r)
	first.r.Release()
	if err != nil {
		_ = c.writeFrame(st.id, opResp, encCode(codeInvalidArgument))
		return
	}
	size, err := s.storage.Stat(context.Background(), key)
	if err != nil {
		_ = c.writeFrame(st.id, opResp, encCode(mapStorageErr(err)))
		return
	}
	p := make([]byte, 8)
	binary.BigEndian.PutUint64(p, uint64(size))
	_ = c.writeFrame(st.id, opStatResp, p)
}
