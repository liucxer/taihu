// 共享内存 IPC 服务端（shmipc-go）：数据面共享内存零拷贝，控制面 unix socket。
// 与 TCP 服务端（server.go）并存，TCP 路径零改动，本文件为纯增量。
//
// shmipc 流天然按请求隔离（双向流，客户端 GetStream/PutBack 复用），无 streamID，
// 帧格式退化为 [4B len][1B op][payload]：len = 1 + len(payload)（大端）；
// 帧的读/写/提交原语在 shm_frame_linux.go。本文件负责接受连接、按流分发，以及把
// storage 的读结果按帧直写进共享内存（O_DIRECT 直写 → 客户端 O_DIRECT 直读）。
//
// 请求处理逻辑镜像 TCP 路径 handlePut/handleGet/handleDelete/handleStat，
// 复用 Parse* 纯函数（ByteReader 解耦后 SliceReader 适配共享内存切片）与
// storage.Put/ReadAtMeta/ReadAtIntoMeta/Delete/Stat；共享内存按帧 Reserve，Flush 后 peer 即可读。
package transport

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/liucxer/taihu/pkg/ierr"
	"github.com/liucxer/taihu/third_party/shmipc-go"

	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/internal/transport/protocol"
	"github.com/liucxer/taihu/pkg/bufpool"
)

// ShmSupported 报告本平台是否支持 shmipc 共享内存 IPC。调用方据此决定是跳过
// 还是调用 ServeShmWithBatch —— 非 Linux 上 shm 数据面不可用，但 TCP 数据面
// 照常，故应降级而非启动失败。
func ShmSupported() bool { return true }

// shmServer 共享内存 IPC 服务端。
type shmServer struct {
	storage *storage.Storage
	ln      *net.UnixListener
	conf    *shmipc.Config

	wg        sync.WaitGroup
	closed    chan struct{}
	closeOnce sync.Once
	batched   *shmBatchReader // 非 nil 时启用"多 stream 一 worker"批读（4MiB 整块聚合 io_submit）
	writer    *batchWriter    // 非 nil 时启用整对象攒批写（一次 AppendBatch + BatchPutCommit）
	deleter   *batchDeleter   // 非 nil 时启用批量删（一次 BatchDelete）
	inflight  int             // 每流在途异步读上限（--shm-inflight；0 = 关闭 per-stream 异步流水线）

	// sessions 已 accept 的 shmipc Session 集合（serveConn 增删，Close 逐个关闭）。
	// 不能只关 unix listener：shmipc.Server 内部 dup fd 后已关闭传入的 net.Conn，
	// 服务端的 ServeConn/AcceptStream/读帧实际阻塞在 session 自己的 shutdownCh/
	// closeNotifyCh 上，只有 session.Close() 会关闭这两条 channel 唤醒它们。
	sessionsMu sync.Mutex
	sessions   map[*shmipc.Session]struct{}
}

// ServeShm 在 unix socket 路径 uds 上提供 shmipc 服务，返回 io.Closer 关闭服务。
// 与 TCP 监听（Server.Serve）互不干扰，可同时启用。共享内存由客户端创建并传入
// （MemFd），服务端仅映射，因此本处配置只须通过 shmipc.VerifyConfig。
func ServeShm(storage *storage.Storage, uds string) (io.Closer, error) {
	return ServeShmWithBatch(storage, uds, 0, 0)
}

// shmBatchTask 服务端"多 stream 一 worker"批读的一个待调任务：把 key 上 pos 处 want 字节
// 直读进共享内存数据区 buf。done 由提交方（对应 stream goroutine）阻塞等待批读结果。
type shmBatchTask struct {
	key  string
	pos  int64
	want int64
	buf  []byte // 4K 对齐共享内存数据区（cap ≥ align4K(want)），O_DIRECT DMA 目标
	done chan shmBatchResult
}

// shmBatchResult 单任务批读结果。
type shmBatchResult struct {
	n     int64 // 请求窗口读入字节数
	final bool  // 该块是本次 Get 末帧（读满请求窗口或读至对象末尾）
	rerr  error // 非 nil 非 io.EOF 为设备/映射错误；io.EOF 表示短读收尾
}

// shmBatchReader 批读协调器：收集多个 stream 的对齐整块 4MiB 直读任务，攒满 target 或
// 超时后交给 Storage.BatchRead 一次 io_submit 批量提交（摊薄系统调用），再按任务路由
// 结果。worker 池（K 个）各跑一个 run goroutine，submission 按原子轮转分发——避免单 worker
// 串行化整机并发（单 worker 在途≈target，会把磁盘队列深度压到 target 而引发带宽回退）。K×target
// 即整机在途批读数；任务完成路由通过每任务 done 通道。
//
// 池约束：对端 stream goroutine 在 Reserve 共享内存切片后才 submit 本任务，故批协调器
// 在途持有的切片数与并发在途 Get 相同（不额外占用共享内存池）；只在攒批/批提交窗口
// 内短暂延后 DMA。批内任一返回错误仍按任务独立回写，不污染后续请求。
type shmBatchReader struct {
	storage *storage.Storage
	target  int           // 攒满即提交的批量
	timeout time.Duration // 未攒满时的最大攒批等待
	workers []chan *shmBatchTask
	rr      atomic.Uint64
}

// newShmBatchReader 构建批读协调器并启动 worker 池。target<=0 表示不启用。K 为 worker 数
// （整机在途 ≈ K×target；K*target 过大逼近共享内存池上限时并发流会储备失败）。
func newShmBatchReader(st *storage.Storage, target, workers int) *shmBatchReader {
	if target <= 0 {
		return nil
	}
	if target > 256 {
		target = 256
	}
	if workers < 1 {
		workers = 1
	}
	b := &shmBatchReader{
		storage: st,
		target:  target,
		timeout: 5 * time.Microsecond,
		workers: make([]chan *shmBatchTask, workers),
	}
	for i := range b.workers {
		b.workers[i] = make(chan *shmBatchTask, target*2)
		go b.run(b.workers[i])
	}
	return b
}

// run 单 worker 批协调主循环：攒批 → Storage.BatchRead → 逐任务路由。
func (b *shmBatchReader) run(in chan *shmBatchTask) {
	var drain []*shmBatchTask
	var timer *time.Timer
	var timerCh <-chan time.Time
	flush := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
			timerCh = nil
		}
		if len(drain) == 0 {
			return
		}
		batch := drain
		drain = nil
		blocks := make([]storage.BatchReadBlock, len(batch))
		for i, tk := range batch {
			blocks[i] = storage.BatchReadBlock{Key: tk.key, Off: tk.pos, Size: tk.want, Dst: tk.buf}
		}
		res, berr := b.storage.BatchRead(context.Background(), blocks)
		for i, tk := range batch {
			var r shmBatchResult
			if berr != nil {
				r = shmBatchResult{rerr: berr}
			} else {
				rr := res[i]
				r.n = rr.N
				r.final = rr.N >= tk.want || rr.Err == io.EOF
				if rr.Err != nil && rr.Err != io.EOF {
					r.rerr = rr.Err
				}
			}
			tk.done <- r
		}
	}
	for {
		select {
		case tk := <-in:
			drain = append(drain, tk)
			if len(drain) >= b.target {
				flush()
			} else if timer == nil {
				timer = time.NewTimer(b.timeout)
				timerCh = timer.C
			}
		case <-timerCh:
			flush()
		}
	}
}

// submit 提交单块直读任务并阻塞至完成，返回读入字节数、是否末帧与错误。
func (b *shmBatchReader) submit(key string, pos, want int64, buf []byte) (int64, bool, error) {
	w := (b.rr.Add(1) - 1) % uint64(len(b.workers))
	tk := &shmBatchTask{key: key, pos: pos, want: want, buf: buf, done: make(chan shmBatchResult, 1)}
	b.workers[w] <- tk
	r := <-tk.done
	return r.n, r.final, r.rerr
}

// ServeShmWithBatch 在 unix socket 路径 uds 上提供 shmipc 服务，并启用"多 stream 多 worker"
// 的批读（batchTarget>0）：对齐整块 4MiB 直读聚合进 storage.BatchRead 一次 io_submit
// 批量提交。batchWorkers 为 worker 池大小（>1 并行批提交，避免单 worker 串行化整机并发）。
// batchTarget<=0 时退化为常规 ServeShm（每块独立直读）。
// 返回 io.Closer 关闭服务。共享内存由客户端创建传入（MemFd），服务端仅映射。
func ServeShmWithBatch(storage *storage.Storage, uds string, batchTarget, batchWorkers int) (io.Closer, error) {
	return ServeShmWithConfig(storage, uds, PipelineConfig{
		ReadBatch: batchTarget, ReadWorkers: batchWorkers,
	})
}

// ServeShmWithConfig 在 unix socket 路径 uds 上提供 shmipc 服务，并按 cfg 启用批处理流水线：
//   - 读（ReadBatch>0）：多 stream 多 worker 聚合批读（Storage.BatchRead 一次 io_submit）；
//   - 写（WriteBatch>0）：整对象攒批写（流 goroutine PutBegin 串行分配 → worker 一次
//     AppendBatch + BatchPutCommit，数据帧直引共享内存零拷贝，submit 完成后统一归还）；
//   - 删（DeleteBatch>0）：批量删（一次 BatchDelete，per-key 结果独立）。
//
// 各批 ≤0 时对应流水线关闭，退化为逐请求串行处理（保持旧行为）。返回 io.Closer 关闭服务；
// 共享内存由客户端创建传入（MemFd），服务端仅映射。
func ServeShmWithConfig(storage *storage.Storage, uds string, cfg PipelineConfig) (io.Closer, error) {
	conf := shmipc.DefaultConfig()
	_ = os.Remove(uds)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: uds, Net: "unix"})
	if err != nil {
		return nil, err
	}
	s := &shmServer{
		storage:  storage,
		ln:       ln,
		conf:     conf,
		closed:   make(chan struct{}),
		sessions: make(map[*shmipc.Session]struct{}),
		batched:  newShmBatchReader(storage, cfg.ReadBatch, cfg.ReadWorkers),
		writer:   newBatchWriter(storage, cfg.WriteWorkers, cfg.WriteBatch),
		deleter:  newBatchDeleter(storage, cfg.DeleteWorkers, cfg.DeleteBatch),
		inflight: cfg.Inflight,
	}
	go s.acceptLoop()
	return s, nil
}

// Close 关闭 unix listener，并逐个关闭已 accept 的 shmipc Session，然后等待全部
// 连接/流处理 goroutine 退出。只关 listener 不够：AcceptStream 阻塞在 session 的
// shutdownCh、读帧阻塞在 stream 的 closeNotifyCh，session.Close() 会同步关闭这两条
// channel 使 serveConn/handleStream 出错返回，wg 因此有界（优雅停机不卡死）。
func (s *shmServer) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	_ = s.ln.Close()
	s.sessionsMu.Lock()
	sessions := make([]*shmipc.Session, 0, len(s.sessions))
	for sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessionsMu.Unlock()
	for _, sess := range sessions {
		_ = sess.Close() // 关闭已唤醒；serveConn 自己也会兜底关闭漏网会话
	}
	s.wg.Wait()
	return nil
}

// acceptLoop 接受 unix socket 连接，每连接一个 shmipc Session。
func (s *shmServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		select {
		case <-s.closed:
			_ = conn.Close()
			return
		default:
		}
		s.wg.Add(1)
		go s.serveConn(conn)
	}
}

// serveConn 单连接服务循环：接受流并逐流起 goroutine 处理。会话在建立后登记进
// s.sessions（Close 据此逐个关闭唤醒阻塞点），退出时注销并关闭。
func (s *shmServer) serveConn(conn net.Conn) {
	defer s.wg.Done()
	session, err := shmipc.Server(conn, s.conf)
	if err != nil {
		return
	}
	s.sessionsMu.Lock()
	s.sessions[session] = struct{}{}
	s.sessionsMu.Unlock()
	defer func() {
		s.sessionsMu.Lock()
		delete(s.sessions, session)
		s.sessionsMu.Unlock()
		_ = session.Close()
	}()
	// 落在 Close 快照之后的会话由这里兜底关闭（Close 的 wg.Wait 只等登记过的
	// serveConn/handleStream，会话必须自己收尾，否则无人关闭会阻塞退出）。
	select {
	case <-s.closed:
		return
	default:
	}
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func(st *shmipc.Stream) {
			defer s.wg.Done()
			s.handleStream(st)
		}(stream)
	}
}

// handleStream 单流请求循环：流被客户端 PutBack 复用（不关闭），服务端持续读下一
// 请求。每请求帧处理镜像 TCP 路径对应 handler；响应写毕后释放本请求帧 pin 的
// 共享内存（读缓冲与写缓冲相互独立，无冲突）。任何错误（协议畸形/写失败）都
// st.Close() 通知对端流关闭，客户端池将丢弃该流（下次 GetStream 自动重开），
// 避免残留未消费帧污染可复用流导致对端阻塞。
//
// per-stream 异步流水线（s.inflight > 0 且未启用批读时）：对齐整块 4MiB Get 提交
// 后不等待完成（pend 在途），由循环按到达顺序排空写帧 —— 配合客户端单流多请求
// pipeline 把单流在途从 1 提升到 P，摊薄每请求固定开销（ring 往返 + meta + flush）。
// 保序地基：响应帧只能按请求到达顺序写出，乱序完成只允许在队列内（HOL stall，
// 顺序读近似有序，代价可接受）。三条防死锁规则：
//  1. 填掉已完成的在途响应帧头，全部完成时整链一次 Flush（响应只能整批可见，
//     原因见 drainCompleted 注释）；
//  2. 在途达上限时等全部在途完成、整链一次 Flush 腾出额度（backpressure）；
//  3. 无新请求可读（r.Len()==0）且仍有在途时，先排空再阻塞读下一帧 —— 否则响应
//     滞留环上、双方互相等待（老客户端单在途：发一请求即停手等响应，必中此坑）。
func (s *shmServer) handleStream(st *shmipc.Stream) {
	r := st.BufferReader()
	var err error
	var pend []*pendingOp // 在途异步项（get：直读；put：batchWriter 异步写；队列顺序==请求到达顺序==响应写出顺序）
	for {
		// 停机分支：s.closed 后尽快收尾（读帧阻塞由 Close 关 session 唤醒，此处
		// 兜住两帧之间未阻塞的空窗，避免多读一轮请求）。
		select {
		case <-s.closed:
			return
		default:
		}
		// 规则 1：填掉已完成的在途响应帧头；全部完成时整链一次 Flush。
		if err = s.drainCompleted(st, r, &pend); err != nil {
			break
		}
		// 规则 2：在途达上限，等全部在途完成、整链一次 Flush 腾出额度。
		if s.inflight > 0 && len(pend) >= s.inflight {
			if err = s.drainAll(st, r, &pend); err != nil {
				break
			}
		}
		// 规则 3：无新请求可读且仍有在途 → 先排空再阻塞读下一帧。
		if len(pend) > 0 && r.Len() == 0 {
			if err = s.drainAll(st, r, &pend); err != nil {
				break
			}
		}
		op, payload, rerr := shmReadFrame(r)
		shmDbg("server stream recv op=%d payloadLen=%d", op, len(payload))
		if rerr != nil {
			err = rerr
			break
		}
		switch op {
		case protocol.OpPutHeader:
			// 写异步流水线（writer != nil 且启用 inflight 异步写）：PutEnd 后不写响应、
			// 任务投 batchWriter 异步排空（pend 保序，响应由 drain 写出）；否则先排空
			// 全部在途（保序）再同步处理（写响应 + 立即回读）。
			if s.inflight > 0 && s.writer != nil {
				err = s.handleShmPut(st, r, payload, &pend)
			} else {
				err = s.drainAll(st, r, &pend)
				if err == nil {
					err = s.handleShmPut(st, r, payload, &pend)
				}
			}
		case protocol.OpGetReq:
			if s.inflight > 0 && s.batched == nil {
				if h, ok := s.trySubmitAsyncGet(st, payload); ok {
					pend = append(pend, h)
				} else {
					// 非异步场景（非单块/非对齐/储备失败等）回退：排空在途保序后
					// 走原同步路径（错误语义与帧型由原路径保证）。
					err = s.drainAll(st, r, &pend)
					if err == nil {
						err = s.handleShmGet(st, payload)
					}
				}
			} else {
				err = s.handleShmGet(st, payload)
			}
		case protocol.OpDelReq:
			err = s.drainAll(st, r, &pend)
			if err == nil {
				err = s.handleShmDelete(st, payload)
			}
		case protocol.OpStatReq:
			err = s.drainAll(st, r, &pend)
			if err == nil {
				err = s.handleShmStat(st, payload)
			}
		default:
			err = ierr.ErrShmStreamBroken
		}

		// 延迟批量释放：数据帧切片须存活到对应异步写完成，仅当无在途项时才可释放本批
		// 已消费的读缓冲（drain 排空时统一 ReleasePreviousRead）。同步路径下 len(pend)==0
		// 恒成立，行为与旧版逐请求释放一致。
		if len(pend) == 0 {
			r.ReleasePreviousRead()
		}
		if err != nil {
			break
		}
	}
	// 退出前等完剩余在途（归还段读引用 / 取回写结果；帧不写 —— 流即将关闭，对端按流关闭重开）。
	for _, h := range pend {
		if !h.done {
			s.awaitPending(h)
		}
	}
	_ = st.Close()
}

// pendingKind 在途异步项类型：get（直读，Reserve 响应切片）与 put（直写，batchWriter 异步排空）。
type pendingKind uint8

const (
	pendingGet pendingKind = iota + 1
	pendingPut
)

// pendingOp 一条在途异步 Get/Put（per-stream 流水线）：已 Reserve 的响应切片（get，含占位
// 帧头）与已提交的异步读句柄 / 已投递 batchWriter 的写任务（put）。完成结果在 drain 取回后
// 写入 done/n/rerr（get）/ code（put）；响应帧由 handleStream 按请求到达顺序写出
// （保序地基：乱序完成只允许在队列内，不能越序写帧）。
type pendingOp struct {
	kind pendingKind
	done bool // get：Await 已取回；put：wt.done 已回投（或立即完成项）
	// get 侧字段（kind==pendingGet）
	buf  []byte // 共享内存切片整区 [pad][帧头][数据区]（O_DIRECT DMA 目标）
	pos  int64  // 请求窗口起点（对象内）
	end  int64  // 请求窗口终点（对象内），final 判定
	ar   *storage.AsyncReadAt
	n    int64
	rerr error
	// put 侧字段（kind==pendingPut）
	wt   *writeTask
	code protocol.ErrCode // 立即完成项（size==0）预置；wt 项由 drain 从 wt.done 取
}

// trySubmitAsyncGet 尝试把 Get 投入 per-stream 异步流水线（快路径专用）：
//   - 请求须为单个对齐整 4MiB 块（off 4K 对齐、size == ChunkSize、对象剩余 ≥ ChunkSize）；
//   - 同步解析映射快照（pebble Get）→ 持段读引用（AsyncReadAt 内部）→ Reserve 响应
//     切片 + 写 OpGetErr 占位帧头 → storage.SubmitReadAtIntoMeta 异步提交，不等待完成。
// 成功返回 (h, true)，响应帧由 handleStream 按序排空写出。参数/映射/储备/提交失败：
// Meta 与提交失败以「已完成错误项」入队（复用已 Reserve 切片，错误帧按序写出）；
// 仅 Reserve 失败（共享内存池不足）返回 (nil, false)，调用方排空在途后走原同步路径。
func (s *shmServer) trySubmitAsyncGet(st *shmipc.Stream, payload []byte) (*pendingOp, bool) {
	key, off, size, err := protocol.ParseGetReq(protocol.NewSliceReader(payload))
	if err != nil || size != protocol.ChunkSize || off%layout.BlockSize != 0 {
		shmDbg("server async get key=%s err=%v size=%d off=%d -> fallback", key, err, size, off)
		return nil, false
	}
	meta, err := s.storage.Meta(context.Background(), key)
	if err != nil {
		shmDbg("server async get key=%s meta err=%v -> fallback", key, err)
		return nil, false
	}
	if meta.Size-off < protocol.ChunkSize {
		shmDbg("server async get key=%s meta.size=%d off=%d < chunk -> fallback", key, meta.Size, off)
		return nil, false
	}
	dlen := layout.Align4k(protocol.ChunkSize)
	buf, err := st.BufferWriter().Reserve(protocol.ShmDataPad + int(dlen))
	if err != nil {
		shmDbg("server async get key=%s reserve err=%v -> fallback", key, err)
		return nil, false
	}
	// 占位帧头（与 shmWriteDataFrameDirect 一致）：DMA 直写期间对端不可读。
	binary.BigEndian.PutUint32(buf[:shmLenPrefixLen], uint32(shmOpLen+4))
	buf[shmLenPrefixLen] = byte(protocol.OpGetErr)
	copy(buf[shmLenPrefixLen+shmOpLen:], protocol.EncCode(protocol.CodeInternal))

	h := &pendingOp{kind: pendingGet, buf: buf, pos: off, end: off + protocol.ChunkSize}
	ar, err := s.storage.SubmitReadAtIntoMeta(meta, off, protocol.ChunkSize,
		buf[protocol.ShmDataPad:protocol.ShmDataPad+dlen])
	if err != nil {
		shmDbg("server async get key=%s submit err=%v -> done-err", key, err)
		h.done, h.rerr = true, err
		return h, true
	}
	h.ar = ar
	shmDbg("server async get key=%s submitted seg=%d", key, meta.SegmentID)
	return h, true
}

// drainCompleted 填掉全部已完成的在途响应（get 填帧头 / put 写 OpResp 帧，纯共享内存写，
// 未 Flush 前对端不可见），且仅当全部在途都完成时才 Flush 一次（整条响应链一次性暴露），
// 随后释放本批已消费的读缓冲切片（数据切片须存活到对应异步写完成，队列排空时方可释放）。
// 返回写错误。
//
// 不能逐帧 Flush：Stream.Flush → done() 会把 front→writeSlice 整条链（含全部已 Reserve
// 但帧头尚未填的在途切片）的切片头更新并链接，对端沿 next 指针整链可见。异步流水线中
// 后到的在途切片帧头仍为 trySubmitAsyncGet 写的占位 OpGetErr（[5][8][CodeInternal]），
// 若在其完成前 Flush，客户端会读到错误帧（实测 GetBatch k=1 报 taihu: rpc error）。
// 故响应只能整批（全部在途完成、帧头全部填好）一起可见；DMA 提交仍随请求到达即时
// 异步下发，磁盘队列深度不受影响。
func (s *shmServer) drainCompleted(st *shmipc.Stream, r shmipc.BufferReader, pend *[]*pendingOp) error {
	allDone := true
	for i := range *pend {
		h := (*pend)[i]
		if !h.done {
			allDone = false
			continue
		}
		if err := s.fillPendingResp(st, h); err != nil {
			return err
		}
	}
	if !allDone {
		return nil
	}
	if err := st.Flush(false); err != nil {
		return err
	}
	r.ReleasePreviousRead()
	*pend = nil
	return nil
}

// drainAll 等全部在途完成、填响应并整链一次 Flush（排空）。backpressure（在途达上限）
// 与"无新请求可读仍有余帧"共用：必须整批排空后才能继续 —— 逐帧写会把未完成切片的
// 占位帧头暴露给对端（见 drainCompleted 注释）。排空后统一释放已消费读缓冲切片。
// 返回写错误。
func (s *shmServer) drainAll(st *shmipc.Stream, r shmipc.BufferReader, pend *[]*pendingOp) error {
	for i := range *pend {
		h := (*pend)[i]
		if !h.done {
			if err := s.awaitPending(h); err != nil {
				return err
			}
		}
		if err := s.fillPendingResp(st, h); err != nil {
			return err
		}
	}
	if err := st.Flush(false); err != nil {
		return err
	}
	r.ReleasePreviousRead()
	*pend = nil
	return nil
}

// awaitPending 等一个在途项完成：get 取回异步读结果（n/rerr），put 取回 batchWriter
// 回投的写错误（映射为响应码）。await 返回错误仅表示底层异常（流须关闭）。
func (s *shmServer) awaitPending(h *pendingOp) error {
	switch h.kind {
	case pendingGet:
		n, rerr := h.ar.Await()
		h.n, h.rerr, h.done = n, rerr, true
		shmDbg("server drainAll Await n=%d rerr=%v", n, rerr)
	case pendingPut:
		werr := <-h.wt.done
		if werr != nil {
			h.code = protocol.MapStorageErr(werr)
		} else {
			h.code = protocol.CodeOK
		}
		h.done = true
		shmDbg("server drainAll put done code=%d err=%v", h.code, werr)
	}
	return nil
}

// fillPendingResp 把一条已完成的在途项填成响应：
//   - get：写响应帧头（数据区已由 DMA 直写），成功 OpGetData/OpGetDataFinal（len 按实际
//     读入 n 更新），读错误 OpGetErr；
//   - put：Reserve 控制帧写 OpResp + 4B 错误码（成功 CodeOK）。
// 不做 Flush —— 连续完成的帧由 drain 合并一次 Flush。
func (s *shmServer) fillPendingResp(st *shmipc.Stream, h *pendingOp) error {
	if h.kind == pendingPut {
		return s.fillPutResp(st, h)
	}
	return s.fillPendingHeader(h)
}

// fillPutResp 把一个已完成的异步写结果填成 OpResp 控制帧（[4B len][1B op=OpResp][4B code]，
// 环上共 4+5=9 字节，len=5；与 shmWriteFrame 控制帧布局一致），并入 sendBuf 响应链（不
// Flush）。成功 code=CodeOK，写失败 code=MapStorageErr(err)。
// 注意 Reserve 必须为完整帧长（shmLenPrefixLen+shmOpLen+4）：只 Reserve 5 字节时
// buf[5:9] 是空切片，copy 会静默丢弃 code 字节，对端把下一帧的 len 前缀读成错误码
// （单响应时直接双方互等死锁）。
func (s *shmServer) fillPutResp(st *shmipc.Stream, h *pendingOp) error {
	buf, err := st.BufferWriter().Reserve(shmLenPrefixLen + shmOpLen + 4)
	if err != nil {
		return err
	}
	binary.BigEndian.PutUint32(buf[:shmLenPrefixLen], uint32(shmOpLen+4))
	buf[shmLenPrefixLen] = byte(protocol.OpResp)
	copy(buf[shmLenPrefixLen+shmOpLen:], protocol.EncCode(h.code))
	statTxFrames.Add(1)
	statTxBytes.Add(4)
	return nil
}

// fillPendingHeader 把一个已完成的在途读结果填成响应帧头（数据区已由 DMA 直写）：
// 成功写 OpGetData/OpGetDataFinal（len 按实际读入 n 更新），读错误写 OpGetErr。
// 不做 Flush —— 连续完成的帧由 drainCompleted 合并一次 Flush。
func (s *shmServer) fillPendingHeader(h *pendingOp) error {
	n, rerr := h.n, h.rerr
	if rerr != nil && rerr != io.EOF {
		shmDbg("server fillPendingHeader ERR n=%d rerr=%v", n, rerr)
		copy(h.buf[shmLenPrefixLen+shmOpLen:], protocol.EncCode(protocol.MapStorageErr(rerr)))
		statTxFrames.Add(1)
		statTxBytes.Add(4)
		return nil
	}
	op := byte(protocol.OpGetData)
	if h.pos+n >= h.end || rerr == io.EOF {
		op = byte(protocol.OpGetDataFinal)
	}
	binary.BigEndian.PutUint32(h.buf[:shmLenPrefixLen], uint32(shmOpLen+n))
	h.buf[shmLenPrefixLen] = op
	statTxFrames.Add(1)
	statTxBytes.Add(n)
	statTxDataFrames.Add(1)
	statTxDataBytes.Add(n)
	if n == protocol.ChunkSize {
		statTxData4M.Add(1)
	}
	return nil
}

// ierr.ErrShmStreamBroken 哨兵错误：流上残留未消费请求帧，无法继续复用，须关闭通知对端。

// shmRespErr 写错误响应并返回哨兵错误（handleStream 据此关闭流）。
// 与 TCP 不同（TCP 流用完即关、迟到帧被丢弃），shm 流被客户端 PutBack 复用，
// 请求未完整消费（超限/畸形）时残留帧会污染流，故错误响应后必须关闭。
func (s *shmServer) shmRespErr(st *shmipc.Stream, op protocol.OpCode, code protocol.ErrCode) error {
	_ = shmWriteFrame(st, op, protocol.EncCode(code))
	return ierr.ErrShmStreamBroken
}

// handleShmPut 处理 Put 请求流：PutHeader → PutData* → PutEnd，语义镜像 TCP handlePut。
//
// 写流水线路径（s.writer != nil）：PutBegin 在 PutHeader 后立即串行分配段内位置（游标连续），
// 各 PutData 帧的共享内存切片直引攒入 jobs（不逐帧放回共享内存），PutEnd 后把整对象投递到
// 写队列，由 worker 一次 AppendBatch 排空数据帧 + 一次 BatchPutCommit 批量建映射；
// 提交完成后 handleStream 统一 ReleasePreviousRead 归还全部 pin 切片。
//   - 同步（inflight==0）：submit 阻塞等磁盘写回，返回后立即写 OpResp 帧；
//   - 异步（inflight>0）：submitAsync 非阻塞投递，任务入 pend 保序队列，OpResp 帧由
//     drain 按请求到达顺序写出 —— 数据帧切片存活到异步写完成（延迟批量释放）。
// 旧路径（writer == nil）：逐帧 PutAppend 直写 + PutEnd 时 PutCommit（保持原行为）。
func (s *shmServer) handleShmPut(st *shmipc.Stream, r shmipc.BufferReader, payload []byte, pend *[]*pendingOp) error {
	key, size, err := protocol.ParsePutHeader(protocol.NewSliceReader(payload))
	if err != nil {
		return s.shmRespErr(st, protocol.OpResp, protocol.CodeInvalidArgument)
	}
	if size < 0 {
		return s.shmRespErr(st, protocol.OpResp, protocol.CodeInvalidArgument)
	}
	if size > s.storage.MaxObjectSize() {
		return s.shmRespErr(st, protocol.OpResp, protocol.CodeTooLarge)
	}

	seg, off, err := s.storage.PutBegin(context.Background(), key, size)
	if err != nil {
		return s.shmRespErr(st, protocol.OpResp, protocol.MapStorageErr(err))
	}
	if size == 0 {
		op, _, err := shmReadFrame(r)
		if err != nil {
			return err
		}
		if op != protocol.OpPutEnd {
			return s.shmRespErr(st, protocol.OpResp, protocol.CodeInvalidArgument)
		}
		if err := s.storage.PutCommit(context.Background(), key, seg, off, 0); err != nil {
			return s.shmRespErr(st, protocol.OpResp, protocol.MapStorageErr(err))
		}
		if s.inflight > 0 && s.writer != nil {
			// 异步保序：size==0 立即完成项入队，OpResp 由 drain 按序写出（不越序）。
			*pend = append(*pend, &pendingOp{kind: pendingPut, done: true, code: protocol.CodeOK})
			return nil
		}
		return shmWriteFrame(st, protocol.OpResp, protocol.EncCode(protocol.CodeOK))
	}

	// 攒整对象的数据帧：Data 直引共享内存（4K 对齐零拷贝），先不 release，
	// 待提交完成由 handleStream 统一 ReleasePreviousRead 归还共享内存。
	var jobs []device.WriteJob
	var pos int64
	for {
		op, p, err := shmReadFrame(r)
		if err != nil {
			return err
		}
		switch op {
		case protocol.OpPutData:
			if pos+int64(len(p)) > size {
				return s.shmRespErr(st, protocol.OpResp, protocol.CodeInvalidArgument)
			}
			jobs = append(jobs, device.WriteJob{SegmentID: seg, Off: off + pos, Data: p, Size: int64(len(p))})
			pos += int64(len(p))
		case protocol.OpPutEnd:
			if pos != size {
				return s.shmRespErr(st, protocol.OpResp, protocol.CodeInvalidArgument)
			}
			if s.writer != nil {
				wt := &writeTask{key: key, seg: seg, off: off, size: size, jobs: jobs, done: make(chan error, 1)}
				if s.inflight > 0 {
					// 异步：非阻塞投递，任务入 pend 保序队列，OpResp 由 drain 按序写出
					//（数据帧切片由延迟批量释放归还）。不在此写响应。
					s.writer.submitAsync(wt)
					*pend = append(*pend, &pendingOp{kind: pendingPut, wt: wt})
					return nil
				}
				if err := s.writer.submit(wt); err != nil {
					return shmWriteFrame(st, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
				}
			} else {
				for _, j := range jobs {
					if err := s.storage.PutAppend(context.Background(), j.SegmentID, j.Off, j.Size, j.Data); err != nil {
						return shmWriteFrame(st, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
					}
				}
				if err := s.storage.PutCommit(context.Background(), key, seg, off, size); err != nil {
					return shmWriteFrame(st, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
				}
			}
			return shmWriteFrame(st, protocol.OpResp, protocol.EncCode(protocol.CodeOK))
		default:
			return s.shmRespErr(st, protocol.OpResp, protocol.CodeInvalidArgument)
		}
	}
}

// handleShmGet 处理 Get 请求：按 ChunkSize 分块读下发 OpGetData，末帧置 final 位
// （OpGetDataFinal）收尾。语义镜像 TCP handleGet。
//
// 两条数据帧路径（布局一致，客户端 shmReadFrame 统一跳 pad）：
//   - 直读快路径（off 4K 对齐，skip==0）：O_DIRECT 直读共享内存切片数据区，
//     免 bufpool→共享内存 memcpy（读路径零拷贝）。请求段内全部整 4MiB 块收进
//     同一条共享内存链并发直读，整链一次 Flush——单请求全程只有一个等齐屏障与
//     一次 Flush，无逐批同步间隙。
//   - 回退路径（off 非对齐）：Storage.ReadAtMeta 读入 bufpool 对齐缓冲，shmWriteFrame 拷贝。
//
// 一次请求的全部帧共用入口解析的同一映射快照（meta），并全程持有该段引用。
func (s *shmServer) handleShmGet(st *shmipc.Stream, payload []byte) error {
	key, off, size, err := protocol.ParseGetReq(protocol.NewSliceReader(payload))
	shmDbg("server get key=%s off=%d size=%d", key, off, size)
	if err != nil {
		return shmWriteFrame(st, protocol.OpGetErr, protocol.EncCode(protocol.CodeInvalidArgument))
	}
	// GET 级映射快照（语义同 TCP handleGet）：一次请求内所有数据帧共用同一 meta，使整段
	// 响应严格来自单一版本；size==-1 的 size 亦由该快照推导。多帧路径全程持有快照段引用，
	// 防止该段被 GC 回收复用。单块请求（下方 batch 快路径）由 BatchRead 内部一次解析并
	// 持引用，不存在跨块混合，无需快照。
	meta, err := s.storage.Meta(context.Background(), key)
	if err != nil {
		return shmWriteFrame(st, protocol.OpGetErr, protocol.EncCode(protocol.MapStorageErr(err)))
	}
	if size == -1 {
		size = meta.Size - off
	}
	if size < 0 {
		return shmWriteFrame(st, protocol.OpGetErr, protocol.EncCode(protocol.CodeInvalidRange))
	}
	pos, end := off, off+size

	if s.batched != nil && pos%layout.BlockSize == 0 && end-pos == protocol.ChunkSize {
		_, rerr := s.shmWriteDataFrameBatch(st, s.batched, key, pos, end-pos, end)

		_ = rerr
		return nil
	}

	s.storage.RefSegment(meta.SegmentID)
	defer s.storage.UnrefSegment(meta.SegmentID)

	if off%layout.BlockSize == 0 {
		var slots []getSlot
		for q := off; q < end; {
			want := end - q
			if want > protocol.ChunkSize {
				want = protocol.ChunkSize
			}
			if want != protocol.ChunkSize || want%layout.BlockSize != 0 {
				break
			}
			slots = append(slots, getSlot{pos: q, want: want, dlen: layout.Align4k(want)})
			q += want
		}
		if len(slots) > 0 {
			done, red, served := shmWriteDataFramesChain(st, s.storage, meta, slots, end)

			pos = slots[0].pos + int64(served)*protocol.ChunkSize
			if red != nil {
				return nil
			}
			if done {
				return nil
			}
		}
	}

	for pos < end {
		want := end - pos
		if want > protocol.ChunkSize {
			want = protocol.ChunkSize
		}

		if pos%layout.BlockSize == 0 && want%layout.BlockSize == 0 {
			n, rerr := shmWriteDataFrameDirect(st, s.storage, meta, pos, want, end)
			if rerr != nil {
				if rerr == io.EOF {
					return nil
				}
				return shmWriteFrame(st, protocol.OpGetErr, protocol.EncCode(protocol.MapStorageErr(rerr)))
			}
			pos += n
			continue
		}

		data, rerr := s.storage.ReadAtMeta(context.Background(), meta, pos, want)
		if len(data) > 0 {
			op := protocol.OpCode(protocol.OpGetData)
			if pos+int64(len(data)) >= end || rerr == io.EOF {
				op = protocol.OpGetDataFinal
			}
			if serr := shmWriteFrame(st, op, data); serr != nil {
				bufpool.Put(data)
				return serr
			}
			bufpool.Put(data)
			pos += int64(len(data))
		}
		if rerr == io.EOF {
			if len(data) == 0 {

				_ = shmWriteFrame(st, protocol.OpGetDataFinal, nil)
			}
			return nil
		}
		if rerr != nil {
			return shmWriteFrame(st, protocol.OpGetErr, protocol.EncCode(protocol.MapStorageErr(rerr)))
		}
	}
	return nil
}

// shmWriteDataFrameBatch 批读协调路径的数据帧写：Reserve 对齐切片后先写 OpGetErr
// 占位帧头，数据区 [ShmDataPad:ShmDataPad+dlen] 交给批协调器（shmBatchReader）与其它
// stream 的读合并为一次 io_submit 批量提交；批完成回读 n 后更新帧头为数据帧
// （OpGetData/OpGetDataFinal）并 Flush。命中请求恰为单个对齐整块（handleShmGet 已过滤）。
//
// 成功返回实际 payload 字节数 n；读失败时已 Flush 错误帧，返回 rerr（调用方按读完毕
// 收尾，不追加错误帧）。
func (s *shmServer) shmWriteDataFrameBatch(st *shmipc.Stream, b *shmBatchReader, key string, pos, want, end int64) (int64, error) {
	dlen := layout.Align4k(want)
	buf, err := st.BufferWriter().Reserve(protocol.ShmDataPad + int(dlen))
	if err != nil {
		return 0, err
	}

	binary.BigEndian.PutUint32(buf[:shmLenPrefixLen], uint32(shmOpLen+4))
	buf[shmLenPrefixLen] = byte(protocol.OpGetErr)
	copy(buf[shmLenPrefixLen+shmOpLen:], protocol.EncCode(protocol.CodeInternal))

	n, final, rerr := b.submit(key, pos, want, buf[protocol.ShmDataPad:protocol.ShmDataPad+dlen])
	if rerr != nil {

		copy(buf[shmLenPrefixLen+shmOpLen:], protocol.EncCode(protocol.MapStorageErr(rerr)))
		statTxFrames.Add(1)
		statTxBytes.Add(4)
		if err := st.Flush(false); err != nil {
			return 0, err
		}
		return 0, rerr
	}

	op := byte(protocol.OpGetData)
	if final {
		op = byte(protocol.OpGetDataFinal)
	}
	binary.BigEndian.PutUint32(buf[:shmLenPrefixLen], uint32(shmOpLen+n))
	buf[shmLenPrefixLen] = op
	statTxFrames.Add(1)
	statTxBytes.Add(n)
	statTxDataFrames.Add(1)
	statTxDataBytes.Add(n)
	if n == protocol.ChunkSize {
		statTxData4M.Add(1)
	}
	if err := st.Flush(false); err != nil {
		return 0, err
	}
	return n, nil
}

// shmWriteDataFrameDirect O_DIRECT 直读共享内存的数据帧写（免 memcpy 快路径）：
// Reserve 对齐切片后先写 OpGetErr 占位帧头，数据区 [ShmDataPad:ShmDataPad+dlen] 作为
// O_DIRECT 目标缓冲直接 DMA 进共享内存（storage.ReadAtIntoMeta 同步等待完成，meta 为
// 本次 GET 的请求级映射快照）；成功则
// 更新帧头为数据帧（len 按实际读入 n 更新，短读/EOF 时 n < want）。返回实际 payload
// 字节数 n。
//
// 空短读（n==0 && EOF）与直读失败（错误码帧已发）均返回 (0, io.EOF)：前者发空 final
// 帧（对端报 short read），后者已发 OpGetErr 错误帧；调用方按 EOF 收尾即可，不再追加
// 错误帧（避免未写帧头的直读切片污染流）。
func shmWriteDataFrameDirect(st *shmipc.Stream, storage *storage.Storage, meta metastore.ObjectMeta, pos, want, end int64) (int64, error) {
	dlen := layout.Align4k(want)
	buf, err := st.BufferWriter().Reserve(protocol.ShmDataPad + int(dlen))
	if err != nil {
		return 0, err
	}

	binary.BigEndian.PutUint32(buf[:shmLenPrefixLen], uint32(shmOpLen+4))
	buf[shmLenPrefixLen] = byte(protocol.OpGetErr)
	copy(buf[shmLenPrefixLen+shmOpLen:], protocol.EncCode(protocol.CodeInternal))
	ctx := context.Background()
	n, rerr := storage.ReadAtIntoMeta(ctx, meta, pos, want, buf[protocol.ShmDataPad:protocol.ShmDataPad+dlen])
	if rerr != nil && rerr != io.EOF {

		copy(buf[shmLenPrefixLen+shmOpLen:], protocol.EncCode(protocol.MapStorageErr(rerr)))
		statTxFrames.Add(1)
		statTxBytes.Add(4)
		if err := st.Flush(false); err != nil {
			return 0, err
		}
		return 0, io.EOF
	}

	op := byte(protocol.OpGetData)
	if pos+n >= end || rerr == io.EOF {
		op = byte(protocol.OpGetDataFinal)
	}
	binary.BigEndian.PutUint32(buf[:shmLenPrefixLen], uint32(shmOpLen+n))
	buf[shmLenPrefixLen] = op

	statTxFrames.Add(1)
	statTxBytes.Add(n)
	statTxDataFrames.Add(1)
	statTxDataBytes.Add(n)
	if n == protocol.ChunkSize {
		statTxData4M.Add(1)
	}
	if err := st.Flush(false); err != nil {
		return 0, err
	}
	return n, rerr
}

// getSlot 记录从共享内存直读的一个 4K 对齐块：Reserve 返回的共享内存切片 buf 与
// DMA 读出的 n/错误。只允许整 4MiB（ChunkSize）块参与链式直读；切片各自独立，
// 并行 DMA 各写各切片无竞争。
type getSlot struct {
	pos, want, dlen int64
	buf             []byte
	n               int64
	rerr            error
}

// shmWriteDataFramesChain 把一次 Get 请求对齐段内的全部整 4MiB 块放进同一条共享内存链
// 并发直读（整请求单链）：相比旧的"逐批 Reserve→wg.Wait→整批 Flush"循环，批间同步
// 间隙（整批等齐→Flush→下一批 Reserve 期间磁盘空转）被完全消除——整请求只在最后出现
// 一次等齐屏障与一次 Flush，DMA 启动后滚动推进，磁盘队列在整个请求段内持续被喂满。
//
//	Phase A（主 goroutine，串行按序）:逐块 Reserve 共享内存切片 + 写占位 OpGetErr
//	  帧头；链序==块序（保序地基，严禁并发 Reserve）。某槽 Reserve 失败即截断，以已
//	  Reserve 前缀为链，返回 served = 已 Reserve 槽数；未 Reserve 部分交由调用方回退
//	  路径续读（不越界、不丢数据）。
//	Phase B（并行）:每块 goroutine 调 storage.ReadAtIntoMeta（meta 为本次 GET 的请求级
//	  映射快照，整链同版本）直读各自切片数据区，随后按
//	  结果写帧头（成功写 OpGetData/OpGetDataFinal，失败写 OpGetErr+MapStorageErr）。
//	Phase C（主 goroutine）:整链一次 Flush 送出；客户端 shmReadFrame 按 [4B len] 定界
//	  逐帧读，天然保序。
//
// 契约：无论成败，所有已 Reserve 槽都填成合法帧（data/final 或 OpGetErr）并整链 Flush
// 一次，保证链边界发送缓冲干净、流可复用、不污染下一请求。
//
// 池约束：链上在途切片在整链 Flush 前不回共享内存池，故单请求在途 = 请求整块数（而非
// 批上限）。≤8 块与旧批在途一致；更大对象在途随块数增长，逼近池上限（2GiB ≈ 480 个
// 大切片）时 Reserve 逐块失败→served 截断→调用方回退路径兜底，不会越界或崩溃。
//
// 返回 done（对象已读毕，末槽已发 final）、rerr（首个非 EOF 读错误，错误帧已整链发出）
// 与 served（实际 Reserve 的槽数，≤ len(slots)）。
func shmWriteDataFramesChain(st *shmipc.Stream, storage *storage.Storage, meta metastore.ObjectMeta, slots []getSlot, end int64) (done bool, rerr error, served int) {

	for i := range slots {
		buf, err := st.BufferWriter().Reserve(protocol.ShmDataPad + int(slots[i].dlen))
		if err != nil {
			break
		}
		slots[i].buf = buf
		binary.BigEndian.PutUint32(buf[:shmLenPrefixLen], uint32(shmOpLen+4))
		buf[shmLenPrefixLen] = byte(protocol.OpGetErr)
		copy(buf[shmLenPrefixLen+shmOpLen:], protocol.EncCode(protocol.CodeInternal))
		served++
	}
	if served == 0 {
		return false, nil, 0
	}
	slots = slots[:served]

	// Phase B：并行 DMA + 各自收尾帧头（全部并发启动，滚动推进，无批间空转）。
	var wg sync.WaitGroup
	for i := range slots {
		wg.Add(1)
		go func(sl *getSlot) {
			defer wg.Done()
			n, rerr2 := storage.ReadAtIntoMeta(context.Background(), meta, sl.pos, sl.want,
				sl.buf[protocol.ShmDataPad:protocol.ShmDataPad+sl.dlen])
			sl.n, sl.rerr = n, rerr2
			if rerr2 != nil && rerr2 != io.EOF {

				copy(sl.buf[shmLenPrefixLen+shmOpLen:], protocol.EncCode(protocol.MapStorageErr(rerr2)))
				statTxFrames.Add(1)
				statTxBytes.Add(4)
				return
			}
			op := byte(protocol.OpGetData)
			if sl.pos+n >= end || rerr2 == io.EOF {
				op = byte(protocol.OpGetDataFinal)
			}
			binary.BigEndian.PutUint32(sl.buf[:shmLenPrefixLen], uint32(shmOpLen+n))
			sl.buf[shmLenPrefixLen] = op
			statTxFrames.Add(1)
			statTxBytes.Add(n)
			statTxDataFrames.Add(1)
			statTxDataBytes.Add(n)
			if n == protocol.ChunkSize {
				statTxData4M.Add(1)
			}
		}(&slots[i])
	}
	wg.Wait()

	if err := st.Flush(false); err != nil {
		return false, err, served
	}
	last := &slots[len(slots)-1]
	if last.pos+last.n >= end || last.rerr == io.EOF {
		return true, nil, served
	}
	for _, sl := range slots {
		if sl.rerr != nil && sl.rerr != io.EOF {
			return false, sl.rerr, served
		}
	}
	return false, nil, served
}

// handleShmDelete 处理 Delete 请求（一元），语义镜像 TCP handleDelete。
// 启用删流水线时整 key 投批删除，否则逐条 Delete。
func (s *shmServer) handleShmDelete(st *shmipc.Stream, payload []byte) error {
	key, err := protocol.ParseKeyReq(protocol.NewSliceReader(payload))
	if err != nil {
		return shmWriteFrame(st, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
	}
	var delErr error
	if s.deleter != nil {
		delErr = s.deleter.submit(key)
	} else {
		delErr = s.storage.Delete(context.Background(), key)
	}
	if delErr != nil {
		return shmWriteFrame(st, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(delErr)))
	}
	return shmWriteFrame(st, protocol.OpResp, protocol.EncCode(protocol.CodeOK))
}

// handleShmStat 处理 Stat 请求（一元）：成功回 OpStatResp{size}，失败回 OpResp{code}。
func (s *shmServer) handleShmStat(st *shmipc.Stream, payload []byte) error {
	key, err := protocol.ParseKeyReq(protocol.NewSliceReader(payload))
	if err != nil {
		return shmWriteFrame(st, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
	}
	size, err := s.storage.Stat(context.Background(), key)
	if err != nil {
		return shmWriteFrame(st, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
	}
	p := make([]byte, 8)
	binary.BigEndian.PutUint64(p, uint64(size))
	return shmWriteFrame(st, protocol.OpStatResp, p)
}
