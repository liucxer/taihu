// Package transport 实现 taihu 的 netpoll 传输层（设计文档_v3 的远程访问层改造，
// 以 netpoll + LinkBuffer 取代 gRPC/HTTP-2）。协议为自定义帧流式多路复用，
// 帧格式与编解码纯函数见 internal/transport/protocol 包。
//
// 帧格式: [4B len][4B streamID][1B op][payload...]
//
//	len = FrameHeaderLen + len(payload)（len 字段之后的字节数），大端。
//	streamID 用于连接内多路复用（每次 RPC 独占一个 stream）。
//
// 两条数据面共用同一套帧协议，仅承载方式不同：
//   - TCP：netpoll 连接 + LinkBuffer，多 stream 并发多路复用（conn.go / server.go）。
//   - shm：shmipc 共享内存，数据帧 4K 对齐供服务端 O_DIRECT 直读（shmipc_*.go）。
//
// 零拷贝路径：
//   - 读：连接读循环 Peek 帧头、Slice 整帧（阻塞至就绪，Slice 生成零拷贝子 Reader），
//     按 streamID 分发给流处理器；流处理器用 Read 把负载直接拷入目标缓冲（一次拷贝）。
//   - 写：WriteBinary 对 >4K 负载零拷贝引用原缓冲，sendmsg(writev) 散射写出，
//     Flush 阻塞至输出缓冲排空（waitFlush），保证引用缓冲在返回后可安全复用。
package transport

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"github.com/liucxer/taihu/third_party/netpoll"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/transport/protocol"
	"github.com/liucxer/taihu/pkg/taihu"
)

// Server 基于 netpoll EventLoop 的 taihu RPC 服务端（对应旧 gRPC rpcserver.New 的
// Serve/GracefulStop/Stop 生命周期）。每连接一个读循环，帧按 streamID 分发给
// 独立 goroutine 处理器，Put/Get/Delete/Stat 语义与旧 gRPC 服务端一致。
type Server struct {
	storage *taihu.Storage

	mu  sync.Mutex
	els []netpoll.EventLoop // 多 listener 场景：每 Serve 一个 EventLoop，停机时全部 Shutdown
}

// NewServer 构建服务端。
func NewServer(storage *taihu.Storage) *Server {
	return &Server{storage: storage}
}

// Serve 在 listener 上提供服务（阻塞直至 Shutdown/异常）。可对多个 listener 并发调用：
// 每个 listener 独立 EventLoop，GracefulStop 会一并停止。
func (s *Server) Serve(ln net.Listener) error {
	el, err := netpoll.NewEventLoop(func(ctx context.Context, c netpoll.Connection) error {
		return s.serveConn(ctx, c)
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.els = append(s.els, el)
	s.mu.Unlock()
	return el.Serve(ln)
}

// GracefulStop 优雅停机：停止全部 EventLoop，等待在途连接处理完毕。
func (s *Server) GracefulStop() {
	s.mu.Lock()
	els := append([]netpoll.EventLoop(nil), s.els...)
	s.mu.Unlock()
	if len(els) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, el := range els {
		_ = el.Shutdown(ctx)
	}
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
func (s *Server) dispatch(c *Conn, sid uint32, op protocol.OpCode, sub netpoll.Reader) deliverResult {
	var started bool
	c.mu.Lock()
	st := c.streams[sid]
	if st == nil {
		switch op {
		case protocol.OpPutHeader, protocol.OpGetReq, protocol.OpDelReq, protocol.OpStatReq, protocol.OpPing, protocol.OpMetaReq, protocol.OpSegReq, protocol.OpKeysReq:
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
		case protocol.OpPutHeader:
			go s.handlePut(c, st)
		case protocol.OpGetReq:
			go s.handleGet(c, st)
		case protocol.OpDelReq:
			go s.handleDelete(c, st)
		case protocol.OpStatReq:
			go s.handleStat(c, st)
		case protocol.OpPing:
			go s.handlePing(c, st)
		case protocol.OpMetaReq:
			go s.handleMeta(c, st)
		case protocol.OpSegReq:
			go s.handleSegments(c, st)
		case protocol.OpKeysReq:
			go s.handleListKeys(c, st)
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
	key, size, err := protocol.ParsePutHeader(first.r)
	first.r.Release()
	if err != nil {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
		return
	}
	if size < 0 {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
		return
	}
	if size > s.storage.MaxObjectSize() {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeTooLarge))
		return
	}
	if size == 0 {
		if err := s.storage.Put(context.Background(), key, 0, nil); err != nil {
			_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
			return
		}
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeOK))
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
		case protocol.OpPutData:
			rem := msg.r.Len()
			if pos+rem > int(size) {
				msg.r.Release()
				_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
				return
			}
			p, err := msg.r.Next(rem)
			if err != nil {
				msg.r.Release()
				_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeInternal))
				return
			}
			pos += copy(buf[pos:], p)
			msg.r.Release()
		case protocol.OpPutEnd:
			msg.r.Release()
			if pos != int(size) {
				_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
				return
			}
			if err := s.storage.Put(context.Background(), key, size, buf); err != nil {
				_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
				return
			}
			_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeOK))
			return
		default:
			msg.r.Release()
			_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
			return
		}
	}
}

// handleGet 处理 Get 请求：按 ChunkSize 分块 ReadAt 下发 OpGetData，
// 最后一个数据帧置 final 位（OpGetDataFinal）收尾（不再发 OpGetEnd 空帧）；
// 出错发 OpGetErr（带错误码）。数据帧零拷贝引用 ReadAt 返回的 bufpool 缓冲，
// writeFrame 返回（Flush 排空）后归还缓冲。
func (s *Server) handleGet(c *Conn, st *stream) {
	defer c.endStream(st)

	first, err := c.await(context.Background(), st)
	if err != nil {
		return
	}
	key, off, size, err := protocol.ParseGetReq(first.r)
	first.r.Release()
	if err != nil {
		_ = c.writeFrame(st.id, protocol.OpGetErr, protocol.EncCode(protocol.CodeInvalidArgument))
		return
	}
	if size == -1 {
		total, err := s.storage.Stat(context.Background(), key)
		if err != nil {
			_ = c.writeFrame(st.id, protocol.OpGetErr, protocol.EncCode(protocol.MapStorageErr(err)))
			return
		}
		size = total - off
	}
	if size < 0 {
		_ = c.writeFrame(st.id, protocol.OpGetErr, protocol.EncCode(protocol.CodeInvalidRange))
		return
	}
	pos, end := off, off+size
	for pos < end {
		want := end - pos
		if want > protocol.ChunkSize {
			want = protocol.ChunkSize
		}
		data, rerr := s.storage.ReadAt(context.Background(), key, pos, want)
		if len(data) > 0 {
			op := protocol.OpCode(protocol.OpGetData)
			if pos+int64(len(data)) >= end || rerr == io.EOF {
				// 最后一个数据帧带 final 位收尾；EOF 短读同样置 final，
				// 客户端 final 校验 pos!=size 报 short read（而非挂死等待）。
				op = protocol.OpGetDataFinal
			}
			if serr := c.writeFrame(st.id, op, data); serr != nil {
				bufpool.Put(data)
				return
			}
			bufpool.Put(data)
			pos += int64(len(data))
		}
		if rerr == io.EOF {
			if len(data) == 0 {
				// 空短读兜底：发空 final 帧让客户端报 short read，避免客户端挂死。
				_ = c.writeFrame(st.id, protocol.OpGetDataFinal, nil)
			}
			return
		}
		if rerr != nil {
			_ = c.writeFrame(st.id, protocol.OpGetErr, protocol.EncCode(protocol.MapStorageErr(rerr)))
			return
		}
	}
}

// handleDelete 处理 Delete 请求（一元）。
func (s *Server) handleDelete(c *Conn, st *stream) {
	defer c.endStream(st)

	first, err := c.await(context.Background(), st)
	if err != nil {
		return
	}
	key, err := protocol.ParseKeyReq(first.r)
	first.r.Release()
	if err != nil {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
		return
	}
	if err := s.storage.Delete(context.Background(), key); err != nil {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
		return
	}
	_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeOK))
}

// handleStat 处理 Stat 请求（一元）：成功回 OpStatResp{size}，失败回 OpResp{code}。
func (s *Server) handleStat(c *Conn, st *stream) {
	defer c.endStream(st)

	first, err := c.await(context.Background(), st)
	if err != nil {
		return
	}
	key, err := protocol.ParseKeyReq(first.r)
	first.r.Release()
	if err != nil {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
		return
	}
	size, err := s.storage.Stat(context.Background(), key)
	if err != nil {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
		return
	}
	p := make([]byte, 8)
	binary.BigEndian.PutUint64(p, uint64(size))
	_ = c.writeFrame(st.id, protocol.OpStatResp, p)
}
