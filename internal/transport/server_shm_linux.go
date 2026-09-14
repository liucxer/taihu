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
// storage.Put/ReadAt/Delete/Stat；共享内存按帧 Reserve，Flush 后 peer 即可读。
package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/liucxer/taihu/third_party/shmipc-go"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/internal/transport/protocol"
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
		storage: storage,
		ln:      ln,
		conf:    conf,
		closed:  make(chan struct{}),
		batched: newShmBatchReader(storage, cfg.ReadBatch, cfg.ReadWorkers),
		writer:  newBatchWriter(storage, cfg.WriteWorkers, cfg.WriteBatch),
		deleter: newBatchDeleter(storage, cfg.DeleteWorkers, cfg.DeleteBatch),
	}
	go s.acceptLoop()
	return s, nil
}

// Close 关闭 unix listener 并等待全部连接/流处理 goroutine 退出。
func (s *shmServer) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	_ = s.ln.Close()
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

// serveConn 单连接服务循环：接受流并逐流起 goroutine 处理。
func (s *shmServer) serveConn(conn net.Conn) {
	defer s.wg.Done()
	session, err := shmipc.Server(conn, s.conf)
	if err != nil {
		return
	}
	defer session.Close()
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
func (s *shmServer) handleStream(st *shmipc.Stream) {
	r := st.BufferReader()
	var err error
	for {
		op, payload, rerr := shmReadFrame(r)
		if rerr != nil {
			err = rerr
			break
		}
		switch op {
		case protocol.OpPutHeader:
			err = s.handleShmPut(st, r, payload)
		case protocol.OpGetReq:
			err = s.handleShmGet(st, payload)
		case protocol.OpDelReq:
			err = s.handleShmDelete(st, payload)
		case protocol.OpStatReq:
			err = s.handleShmStat(st, payload)
		default:
			err = errShmStreamBroken
		}

		r.ReleasePreviousRead()
		if err != nil {
			break
		}
	}
	_ = st.Close()
}

// errShmStreamBroken 哨兵错误：流上残留未消费请求帧，无法继续复用，须关闭通知对端。
var errShmStreamBroken = errors.New("taihu: shm stream broken")

// shmRespErr 写错误响应并返回哨兵错误（handleStream 据此关闭流）。
// 与 TCP 不同（TCP 流用完即关、迟到帧被丢弃），shm 流被客户端 PutBack 复用，
// 请求未完整消费（超限/畸形）时残留帧会污染流，故错误响应后必须关闭。
func (s *shmServer) shmRespErr(st *shmipc.Stream, op protocol.OpCode, code protocol.ErrCode) error {
	_ = shmWriteFrame(st, op, protocol.EncCode(code))
	return errShmStreamBroken
}

// handleShmPut 处理 Put 请求流：PutHeader → PutData* → PutEnd，语义镜像 TCP handlePut。
//
// 写流水线路径（s.writer != nil）：PutBegin 在 PutHeader 后立即串行分配段内位置（游标连续），
// 各 PutData 帧的共享内存切片直引攒入 jobs（不逐帧放回共享内存），PutEnd 后把整对象投递到
// 写队列，由 worker 一次 AppendBatch 排空数据帧 + 一次 BatchPutCommit 批量建映射；
// submit 同步等磁盘写回，返回后 handleStream 统一 ReleasePreviousRead 归还全部 pin 切片。
// 旧路径（writer == nil）：逐帧 PutAppend 直写 + PutEnd 时 PutCommit（保持原行为）。
func (s *shmServer) handleShmPut(st *shmipc.Stream, r shmipc.BufferReader, payload []byte) error {
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
//   - 回退路径（off 非对齐）：Storage.ReadAt 读入 bufpool 对齐缓冲，shmWriteFrame 拷贝。
func (s *shmServer) handleShmGet(st *shmipc.Stream, payload []byte) error {
	key, off, size, err := protocol.ParseGetReq(protocol.NewSliceReader(payload))
	if err != nil {
		return shmWriteFrame(st, protocol.OpGetErr, protocol.EncCode(protocol.CodeInvalidArgument))
	}
	if size == -1 {
		total, err := s.storage.Stat(context.Background(), key)
		if err != nil {
			return shmWriteFrame(st, protocol.OpGetErr, protocol.EncCode(protocol.MapStorageErr(err)))
		}
		size = total - off
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
			done, red, served := shmWriteDataFramesChain(st, s.storage, key, slots, end)

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
			n, rerr := shmWriteDataFrameDirect(st, s.storage, key, pos, want, end)
			if rerr != nil {
				if rerr == io.EOF {
					return nil
				}
				return shmWriteFrame(st, protocol.OpGetErr, protocol.EncCode(protocol.MapStorageErr(rerr)))
			}
			pos += n
			continue
		}

		data, rerr := s.storage.ReadAt(context.Background(), key, pos, want)
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
// O_DIRECT 目标缓冲直接 DMA 进共享内存（storage.ReadAtInto 同步等待完成）；成功则
// 更新帧头为数据帧（len 按实际读入 n 更新，短读/EOF 时 n < want）。返回实际 payload
// 字节数 n。
//
// 空短读（n==0 && EOF）与直读失败（错误码帧已发）均返回 (0, io.EOF)：前者发空 final
// 帧（对端报 short read），后者已发 OpGetErr 错误帧；调用方按 EOF 收尾即可，不再追加
// 错误帧（避免未写帧头的直读切片污染流）。
func shmWriteDataFrameDirect(st *shmipc.Stream, storage *storage.Storage, key string, pos, want, end int64) (int64, error) {
	dlen := layout.Align4k(want)
	buf, err := st.BufferWriter().Reserve(protocol.ShmDataPad + int(dlen))
	if err != nil {
		return 0, err
	}

	binary.BigEndian.PutUint32(buf[:shmLenPrefixLen], uint32(shmOpLen+4))
	buf[shmLenPrefixLen] = byte(protocol.OpGetErr)
	copy(buf[shmLenPrefixLen+shmOpLen:], protocol.EncCode(protocol.CodeInternal))
	ctx := context.Background()
	n, rerr := storage.ReadAtInto(ctx, key, pos, want, buf[protocol.ShmDataPad:protocol.ShmDataPad+dlen])
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
//	Phase B（并行）:每块 goroutine 调 storage.ReadAtInto 直读各自切片数据区，随后按
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
func shmWriteDataFramesChain(st *shmipc.Stream, storage *storage.Storage, key string, slots []getSlot, end int64) (done bool, rerr error, served int) {

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
			n, rerr2 := storage.ReadAtInto(context.Background(), key, sl.pos, sl.want,
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
