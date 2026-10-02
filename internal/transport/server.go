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
//   - TCP：netpoll 连接 + LinkBuffer，多 stream 并发多路复用（frame.go / client.go / server.go）。
//   - shm：shmipc 共享内存，数据帧 4K 对齐供服务端 O_DIRECT 直读（shm_frame_linux.go / client_shm_linux.go / server_shm_linux.go）。
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

	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/internal/transport/protocol"
	"github.com/liucxer/taihu/pkg/bufpool"
)

// Server 基于 netpoll EventLoop 的 taihu RPC 服务端（对应旧 gRPC rpcserver.New 的
// Serve/GracefulStop/Stop 生命周期）。每连接一个读循环，帧按 streamID 分发给
// 独立 goroutine 处理器，Put/Get/Delete/Stat 语义与旧 gRPC 服务端一致。
type Server struct {
	storage *storage.Storage

	pipeline *pipeline // 写/删批处理流水线（nil 时逐请求串行，保持旧行为）

	// inflight TCP 每流在途异步读上限（-tcp-inflight）：>0 时 Get 流走 per-stream
	// 异步流水线（对齐整块 4MiB 读提交不等待、按序排空写帧），0 退化为逐请求同步。
	inflight int

	mu  sync.Mutex
	els []netpoll.EventLoop // 多 listener 场景：每 Serve 一个 EventLoop，停机时全部 Shutdown
}

// NewServer 构建服务端（默认不启用写/删流水线）。
func NewServer(storage *storage.Storage) *Server {
	return NewServerWithOptions(storage, PipelineConfig{})
}

// NewServerWithOptions 构建服务端并按 cfg 启用写/删批处理流水线
// （-write-batch / -del-batch >0 时生效，沿用 drain 取批 + 多 worker 并发提交）。
func NewServerWithOptions(storage *storage.Storage, cfg PipelineConfig) *Server {
	return &Server{storage: storage, pipeline: newPipeline(storage, cfg), inflight: cfg.Inflight}
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
			go s.handleGetStream(c, st)
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

// drainPutTail 丢弃 Put 流已无用的尾部帧。
//
// 客户端 Put 是「全量先行发送」：PutHeader 之后立刻把所有 PutData 与 OpPutEnd 发完，
// 最后才等响应。故服务端在 PutHeader 之后提前失败（对象超限 / 零长 / PutBegin 失败）时，
// 客户端仍有尾帧在途；这些帧必须由本流消费掉——否则它们到达时流已被 endStream 注销、
// op 又不是首帧类型，会被 dispatch 判为「未知流非首帧」协议错误 → deliverFatal → 关闭
// 整条连接（连接一死，其上后续所有 RPC 全部失败；客户端仅轮转选连接，无剔除重建，
// 于是死连接被永久复用，错误随时间线性累积）。
//
// budget 为待丢弃的数据字节数（即 PutHeader 的 size），消费满后再吃掉收尾的 OpPutEnd；
// 对端提前收尾或连接关闭时立即返回。等待语义与正常 Put 路径一致（同样是等对端发完），
// 不引入新的阻塞面。
func (c *Conn) drainPutTail(st *stream, budget int64) {
	var got int64
	for got < budget {
		msg, err := c.await(context.Background(), st)
		if err != nil {
			return
		}
		if msg.op != protocol.OpPutData {
			msg.r.Release() // 对端提前收尾（OpPutEnd）
			return
		}
		got += int64(msg.r.Len())
		msg.r.Release()
	}
	msg, err := c.await(context.Background(), st) // 收尾帧 OpPutEnd
	if err != nil {
		return
	}
	msg.r.Release()
}

// drainPutTailUnknown 在 PutHeader 解析失败（size 未知、无法按字节记账）时排空尾部：
// 持续丢弃数据帧，直到收尾帧（OpPutEnd）或对端收尾/连接关闭。
// 依据同一契约：客户端每个 Put 恒以收尾帧结束，且同流内各 Put 的帧不交错，
// 故「丢到第一个非数据帧」恰好排空本请求、且不会吃掉下一个 Put 的帧。
// 与 drainPutTail 同属 th-279：PutHeader 之后任何提前返回分支都必须先排空尾部，
// 否则残留帧会被 dispatch 判为「未知流非首帧」→ deliverFatal → 关闭整条连接。
func (c *Conn) drainPutTailUnknown(st *stream) {
	for {
		msg, err := c.await(context.Background(), st)
		if err != nil {
			return
		}
		op := msg.op
		msg.r.Release()
		if op != protocol.OpPutData {
			return
		}
	}
}

// handlePut 处理 Put 流：首帧 PutHeader{key,size}，PutHeader 后立即 PutBegin 串行分配
// 段内位置（游标连续，保序分配）；随后 PutData 帧汇入 bufpool 缓冲，PutEnd 后整对象
// 交给写流水线（AppendBatch 一次 io_submit 排空 + BatchPutCommit 批量建映射；流水线
// 关闭时退化为 Storage.Put 单条路径）。设备写位于分配锁之外，并发 Put 可写不同偏移。
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
		c.drainPutTailUnknown(st) // PutHeader 已收到，客户端数据帧/收尾帧仍在途，不排空会关连接
		return
	}
	if size < 0 {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
		c.drainPutTail(st, 0) // size 非法的 Put 不带数据帧，仅需吃掉收尾帧 OpPutEnd
		return
	}
	if size > s.storage.MaxObjectSize() {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeTooLarge))
		c.drainPutTail(st, size) // 客户端的数据帧已在途，不排空会关连接
		return
	}
	if size == 0 {
		if err := s.storage.Put(context.Background(), key, 0, nil); err != nil {
			_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
			c.drainPutTail(st, 0) // 零长 Put 也带 OpPutEnd 收尾帧
			return
		}
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeOK))
		c.drainPutTail(st, 0)
		return
	}

	seg, off, err := s.storage.PutBegin(context.Background(), key, size)
	if err != nil {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
		c.drainPutTail(st, size) // 典型：ErrNoSpace（盘满）时客户端整对象已在途
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
			if s.pipeline != nil && s.pipeline.w != nil {
				job := device.WriteJob{SegmentID: seg, Off: off, Data: buf[:size], Size: size}
				wt := &writeTask{key: key, seg: seg, off: off, size: size, jobs: []device.WriteJob{job}, done: make(chan error, 1)}
				if err := s.pipeline.w.submit(wt); err != nil {
					_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
					return
				}
			} else if err := s.storage.Put(context.Background(), key, size, buf); err != nil {
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

// tcpGetPending 一条 TCP 在途异步读项（per-stream 流水线）：已提交的异步读句柄与
// bufpool 对齐缓冲（O_DIRECT DMA 目标）。完成结果在 drain 取回后写入 done/n/rerr；
// 响应帧由 handleGetStream 按请求到达顺序写出（保序地基：乱序完成只允许在队列内，
// 不能越过未完成项写后面的帧）。buf 写出后置 nil 防重复归还。
type tcpGetPending struct {
	pos, end int64 // 请求窗口 [pos, end)（对象内）：pos 为块起点，end 恒为整个 GetReq
	// 窗口终点（off+size，跨块共享）——final 判定必须基于整个请求而非单块，
	// 否则多块请求的首块会被误打成 final、客户端提前收流报 short read
	buf      []byte
	ar       *storage.AsyncReadAt
	done     bool
	n        int64
	rerr     error
	// unrefSeg ≥0：写出/丢弃该项时释放该段读引用。一次 GetReq 的 GET 级段引用
	// （由入口 MetaRef 取得并复验）挂在该请求最后一个在途项上，写出（或退出兜底）
	// 时释放；-1 表示该项不持有引用。
	unrefSeg int64
}

// handleGetStream 单流 Get 请求循环（TCP 版 per-stream 异步流水线）：s.inflight>0 时
// 对齐整块 4MiB Get 提交后不等待完成（pend 在途），由循环按到达顺序排空写帧 ——
// 配合客户端单流多请求 pipeline（GetBatch）把单流在途从 1 提升到 P，摊薄每请求
// 固定开销（ring 往返 + meta + flush）。s.inflight==0 时退化为逐请求同步处理
// （getSync），语义与旧 handleGet 一致，且仍能正确消费批帧（GetBatch 连发的
// 多个 GetReq，旧单次 handler 遇批帧会误判协议错误关连接）。
//
// 保序地基与三条防死锁规则镜像 shm handleStream（见 server_shm_linux.go 注释）：
//  1. 填掉已完成的队首在途项（按序写帧，TCP 每帧独立 writeFrame、无 shm 占位帧头
//     问题，可随完成即写，无需整链合并 Flush）；
//  2. 在途达上限时等全部在途完成、写帧腾出额度（backpressure）；
//  3. 无新请求可读（tryAwait 空）且仍有在途时，先排空再阻塞读下一帧 —— 否则响应
//     滞留、客户端互等（老客户端单在途：发一请求即停手等响应，必中此坑）。
func (s *Server) handleGetStream(c *Conn, st *stream) {
	defer c.endStream(st)

	var pend []*tcpGetPending
	defer func() {
		// 退出兜底：未排空的在途项先 Await（归还段读引用；DMA 目标缓冲须等读完成
		// 才能归还池）再归还缓冲。
		for _, h := range pend {
			if !h.done && h.ar != nil {
				h.ar.Await()
			}
			if h.buf != nil {
				bufpool.Put(h.buf)
			}
			// 未写出项的 GET 级段引用同样要释放（否则段永不回收）。
			if h.unrefSeg >= 0 {
				s.storage.UnrefSegment(h.unrefSeg)
				h.unrefSeg = -1
			}
		}
	}()

	for {
		// 规则 1：填掉已完成的队首在途项（按序写帧 + 归还缓冲）。
		if err := s.drainTcpGetHead(c, st, &pend); err != nil {
			return
		}
		// 规则 2：在途达上限，等全部在途完成、写帧腾出额度。
		if s.inflight > 0 && len(pend) >= s.inflight {
			if err := s.drainAllTcpGet(c, st, &pend); err != nil {
				return
			}
		}
		// 规则 3：无新请求可读且仍有在途 → 先排空再阻塞读下一帧。
		msg, ok := c.tryAwait(st)
		if !ok {
			if len(pend) > 0 {
				if err := s.drainAllTcpGet(c, st, &pend); err != nil {
					return
				}
				continue
			}
			m, err := c.await(context.Background(), st)
			if err != nil {
				return
			}
			msg = m
		}
		if msg.op != protocol.OpGetReq {
			msg.r.Release()
			return
		}
		if err := s.submitTcpGetReq(c, st, msg, &pend); err != nil {
			return
		}
	}
}

// submitTcpGetReq 处理一个 GetReq 帧：解析 → 映射快照 → size 推导 → 校验。
// s.inflight>0 且请求为对齐整块（off 4K 对齐 + size 为 ChunkSize 整数倍）时逐块
// 投入异步流水线（提交不等待）；否则排空在途保序后走同步路径（getSync）。
// 参数/映射/校验失败写 OpGetErr 后继续循环（流级可复用，不关流）；错误帧必须
// 先排空在途再写 —— 响应帧只能按请求到达顺序写出，越序写帧会让客户端把错误帧
// 对到错误的请求上（批量连发场景尤其致命）。
func (s *Server) submitTcpGetReq(c *Conn, st *stream, msg frameMsg, pend *[]*tcpGetPending) error {
	key, off, size, err := protocol.ParseGetReq(msg.r)
	msg.r.Release()
	if err != nil {
		if derr := s.drainAllTcpGet(c, st, pend); derr != nil {
			return derr
		}
		return c.writeFrame(st.id, protocol.OpGetErr, protocol.EncCode(protocol.CodeInvalidArgument))
	}
	// GET 级映射快照 + 段读引用：入口解析一次 key→meta（MetaRef 内在 pebble 复验快照
	// 仍有效，闭合「取快照 → 取引用」之间的回收复用窗口）并全程复用，使整段响应严格
	// 来自同一版本（逐 chunk 重解析会在并发覆盖写中途切换版本，产生 chunk 级混合）；
	// size==-1 的 size 也由该快照推导（与数据同版本）。
	// 引用归属：同步路径由本函数 defer 释放；异步路径转交该请求的最后一个在途项。
	meta, err := s.storage.MetaRef(context.Background(), key)
	if err != nil {
		if derr := s.drainAllTcpGet(c, st, pend); derr != nil {
			return derr
		}
		return c.writeFrame(st.id, protocol.OpGetErr, protocol.EncCode(protocol.MapStorageErr(err)))
	}
	refPending := true
	defer func() {
		if refPending {
			s.storage.UnrefSegment(meta.SegmentID)
		}
	}()
	if size == -1 {
		size = meta.Size - off
	}
	if size < 0 {
		if derr := s.drainAllTcpGet(c, st, pend); derr != nil {
			return derr
		}
		return c.writeFrame(st.id, protocol.OpGetErr, protocol.EncCode(protocol.CodeInvalidRange))
	}

	// 异步快路径：对齐整块逐块提交（每块 4MiB 独立异步读，O_DIRECT 直读 bufpool 对齐缓冲）。
	if s.inflight > 0 && off%layout.BlockSize == 0 && size > 0 && size%protocol.ChunkSize == 0 {
		for pos := off; pos < off+size; pos += protocol.ChunkSize {
			buf := bufpool.Get(int(protocol.ChunkSize))
			h := &tcpGetPending{pos: pos, end: off + size, buf: buf, unrefSeg: -1}
			ar, serr := s.storage.SubmitReadAtIntoMeta(meta, pos, protocol.ChunkSize, buf)
			if serr != nil {
				// 提交失败（含空窗口 io.EOF）：缓冲未投入使用，立即归还；以「已完成
				// 错误项」入队保序（错误帧按序写出，避免越序写帧破坏响应边界）。
				bufpool.Put(buf)
				h.buf = nil
				h.done, h.rerr = true, serr
			} else {
				h.ar = ar
			}
			*pend = append(*pend, h)
		}
		// 入口段引用转交本请求最后一个在途项：写出（或退出兜底）时释放。
		(*pend)[len(*pend)-1].unrefSeg = meta.SegmentID
		refPending = false
		return nil
	}

	// 同步路径：排空在途保序后走 getSync（错误语义与帧型与原 handleGet 一致）。
	if err := s.drainAllTcpGet(c, st, pend); err != nil {
		return err
	}
	return s.getSync(c, st, meta, off, size)
}

// getSync 同步读一个 Get 请求（镜像原 handleGet）：快照段读引用由调用方
// （submitTcpGetReq 经 MetaRef 取得）在整段读取期间持有，本函数按 ChunkSize 分块
// ReadAtMeta 下发 OpGetData，末帧 final 收尾；出错发 OpGetErr。数据帧零拷贝引用
// ReadAt 返回的 bufpool 缓冲，writeFrame 返回（Flush 排空）后归还。
func (s *Server) getSync(c *Conn, st *stream, meta metastore.ObjectMeta, off, size int64) error {
	pos, end := off, off+size
	for pos < end {
		want := end - pos
		if want > protocol.ChunkSize {
			want = protocol.ChunkSize
		}
		data, rerr := s.storage.ReadAtMeta(context.Background(), meta, pos, want)
		if len(data) > 0 {
			op := protocol.OpCode(protocol.OpGetData)
			if pos+int64(len(data)) >= end || rerr == io.EOF {
				// 最后一个数据帧带 final 位收尾；EOF 短读同样置 final，
				// 客户端 final 校验 pos!=size 报 short read（而非挂死等待）。
				op = protocol.OpGetDataFinal
			}
			if serr := c.writeFrame(st.id, op, data); serr != nil {
				bufpool.Put(data)
				return serr
			}
			bufpool.Put(data)
			pos += int64(len(data))
		}
		if rerr == io.EOF {
			if len(data) == 0 {
				// 空短读兜底：发空 final 帧让客户端报 short read，避免客户端挂死。
				_ = c.writeFrame(st.id, protocol.OpGetDataFinal, nil)
			}
			return nil
		}
		if rerr != nil {
			return c.writeFrame(st.id, protocol.OpGetErr, protocol.EncCode(protocol.MapStorageErr(rerr)))
		}
	}
	return nil
}

// drainTcpGetHead 填掉队首已完成的在途项（按序写帧 + 归还缓冲）；遇未完成项即停
// （保序：不能越过未完成项写后面的帧）。TCP 每帧独立 writeFrame（无 shm 占位帧头
// 问题），可随完成即写，无需整链合并一次 Flush。
func (s *Server) drainTcpGetHead(c *Conn, st *stream, pend *[]*tcpGetPending) error {
	for len(*pend) > 0 {
		h := (*pend)[0]
		if !h.done {
			return nil
		}
		if err := s.writeTcpGetFrame(c, st, h); err != nil {
			return err
		}
		*pend = (*pend)[1:]
	}
	return nil
}

// drainAllTcpGet 等全部在途完成并按序写帧（排空队列）。backpressure（在途达上限）
// 与「无新请求可读仍有余帧」共用；返回写错误。
func (s *Server) drainAllTcpGet(c *Conn, st *stream, pend *[]*tcpGetPending) error {
	for len(*pend) > 0 {
		h := (*pend)[0]
		if !h.done {
			if h.ar != nil {
				h.n, h.rerr = h.ar.Await()
			}
			h.done = true
		}
		if err := s.writeTcpGetFrame(c, st, h); err != nil {
			return err
		}
		*pend = (*pend)[1:]
	}
	return nil
}

// writeTcpGetFrame 把一条已完成的在途项写成一帧：读错误（非 EOF）写 OpGetErr；
// 成功写 OpGetData/OpGetDataFinal（末帧判定 pos+n>=end 或 EOF 短读）；n==0 的
// 空短读发空 final 帧兜底（客户端 short read 报错，避免挂死）。写出后归还缓冲
// 并置 nil（防退出兜底重复归还）。
func (s *Server) writeTcpGetFrame(c *Conn, st *stream, h *tcpGetPending) error {
	defer func() {
		if h.buf != nil {
			bufpool.Put(h.buf)
			h.buf = nil
		}
		if h.unrefSeg >= 0 {
			s.storage.UnrefSegment(h.unrefSeg)
			h.unrefSeg = -1
		}
	}()
	n, rerr := h.n, h.rerr
	if rerr != nil && rerr != io.EOF {
		return c.writeFrame(st.id, protocol.OpGetErr, protocol.EncCode(protocol.MapStorageErr(rerr)))
	}
	if n == 0 && rerr == io.EOF {
		// 空短读兜底：发空 final 帧让客户端报 short read，避免客户端挂死。
		return c.writeFrame(st.id, protocol.OpGetDataFinal, nil)
	}
	op := protocol.OpGetData
	if h.pos+n >= h.end || rerr == io.EOF {
		op = protocol.OpGetDataFinal
	}
	return c.writeFrame(st.id, op, h.buf[:n])
}

// handleDelete 处理 Delete 请求（一元）。启用删流水线时整 key 投批删除，否则逐条 Delete。
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
	var delErr error
	if s.pipeline != nil && s.pipeline.d != nil {
		delErr = s.pipeline.d.submit(key)
	} else {
		delErr = s.storage.Delete(context.Background(), key)
	}
	if delErr != nil {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(delErr)))
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
