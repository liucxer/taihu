// 共享内存 IPC 客户端（shmipc-go）：数据面共享内存零拷贝，控制面 unix socket。
// 与 TCP 客户端（client.go）并存、接口同构（rpcclient.rpcConn），调用方可无感切换。
//
// 连接为一个 SessionManager（sessions 条会话：各自独立 unix socket + 共享内存），
// GetStream/PutBack 流复用。帧协议与服务端一致：[4B len][1B op][payload]。
// 共享内存由本端（客户端）创建并经 unix socket 传 memfd 给服务端映射，两端
// 零拷贝读写同一块内存；内存不足自动 fallback 到 unix socket 拷贝。
package transport

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/liucxer/taihu/third_party/shmipc-go"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/transport/protocol"
	"github.com/liucxer/taihu/pkg/ierr"
)

// 共享内存缓冲配置：小切片承载控制帧（请求/一元响应），大切片承载 4MiB 数据帧
// （单切片零拷贝读）。容量按会话在途帧数估算；memfd 稀疏分配，物理内存按写入页计。
//
// 大切片数量 = BufferCap × 大档百分比 / ShmSliceSize：读路径每个并发流在途 1 个
// 4MiB 响应切片，池数量必须 ≥ 期望并发读线程数，否则 Reserve 失败自动 fallback
// 到 unix socket 拷贝（带宽断崖 + 熔断）。故大切片档占比拉到 95%（控制帧仅占
// 5% 小巧致密），2GiB 池 ≈ 480 个 4MiB 切片，覆盖 128+ 并发读。
const (
	shmSmallSlice   = 16 * 1024        // 控制帧切片数据容量
	shmBufferCap    = 2 << 30          // 每会话共享内存容量 2GiB
	shmMaxStreamNum = 256              // 流池上限（超过 GetStream 阻塞等待 PutBack）
	shmInitTimeout  = 30 * time.Second // 会话初始化（建 memfd + 握手）超时
)

// ShmConn 一条 shmipc 共享内存客户端连接（rpcclient.rpcConn 语义，方法见下）。
// 内部为一个 SessionManager：并发 RPC 各自 GetStream，天然线程安全。
type ShmConn struct {
	sm *shmipc.SessionManager
}

// DialShm 建立 shmipc 共享内存连接池：uds 为服务端 unix socket 路径，sessions 为
// 会话数（≈连接数，对应 TCP DialPool 的 -conns，SessionManager 内部 round-robin）。
// 阻塞至全部会话完成握手；服务端未启动时在 InitializeTimeout 后报错。
func DialShm(uds string, sessions int) (*ShmConn, error) {
	if sessions < 1 {
		sessions = 1
	}
	conf := shmipc.DefaultSessionManagerConfig()
	conf.Network = "unix"
	conf.Address = uds
	conf.MemMapType = shmipc.MemMapTypeMemFd
	conf.SessionNum = sessions
	conf.MaxStreamNum = shmMaxStreamNum
	conf.StreamMaxIdleTime = 30 * time.Second
	conf.InitializeTimeout = shmInitTimeout
	conf.ShareMemoryBufferCap = shmBufferCap
	conf.BufferSliceSizes = []*shmipc.SizePercentPair{
		{Size: shmSmallSlice, Percent: 5},
		{Size: protocol.ShmSliceSize, Percent: 95},
	}
	sm, err := shmipc.NewSessionManager(conf)
	if err != nil {
		return nil, err
	}
	return &ShmConn{sm: sm}, nil
}

// Close 关闭连接池（全部会话与流，通知服务端）。
func (c *ShmConn) Close() error {
	return c.sm.Close()
}

// Put 上传对象（与 TCP Conn.Put 同语义）：PutHeader → PutData* → PutEnd → OpResp。
// 数据帧一次 Reserve 直写共享内存（一次用户态拷贝，无内核参与），Flush 即对端可见。
func (c *ShmConn) Put(ctx context.Context, key string, size int64, in []byte) error {
	if int64(len(in)) < size {
		return ierr.ErrShortWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := c.sm.GetStream()
	if err != nil {
		return err
	}
	defer c.sm.PutBack(st)
	if d, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(d)
	}
	if err := shmWriteFrame(st, protocol.OpPutHeader, protocol.EncodePutHeader(key, size)); err != nil {
		return err
	}
	var off int64
	for off < size {
		end := off + protocol.ChunkSize
		if end > size {
			end = size
		}
		if err := shmWriteFrame(st, protocol.OpPutData, in[off:end]); err != nil {
			return err
		}
		off = end
	}
	if err := shmWriteFrame(st, protocol.OpPutEnd, nil); err != nil {
		return err
	}
	op, payload, err := shmReadFrame(st.BufferReader())
	if err != nil {
		return err
	}
	if op != protocol.OpResp {
		return fmt.Errorf("taihu: unexpected put response op %d", op)
	}
	code, err := protocol.ReadU32(protocol.NewSliceReader(payload))
	if err != nil {
		return err
	}
	return protocol.MapCode(protocol.ErrCode(code))
}

// getResult Get/GetFd 的统一交付结果：fd>0 时 data 为共享内存零拷贝引用
// （fd/foff 为 memfd splice 源）；fd==0 时 data 为池化拷贝缓冲。release 用毕必调（幂等）。
type getResult struct {
	fd      int
	foff    uint64
	data    []byte
	release func()
}

// fdBatchReleaser 批量零拷贝读的共享归还器（引用计数）：批内每个 FdBuf 各持一份
// 引用，全部归还后（或错误路径 force）一次性归还池化缓冲、释放帧 pin 并把流
// PutBack 复用。幂等（once 保护），与 GetBatch 的单 release 语义等价但支持 per-key 归还。
type fdBatchReleaser struct {
	n    int32
	once sync.Once
	bufs [][]byte
	r    shmipc.BufferReader
	st   *shmipc.Stream
	sm   *shmipc.SessionManager
}

func newFdBatchReleaser(refs int, r shmipc.BufferReader, st *shmipc.Stream, sm *shmipc.SessionManager) *fdBatchReleaser {
	return &fdBatchReleaser{n: int32(refs), r: r, st: st, sm: sm}
}

// releaseOne 归还一份引用；归零时执行整体清理。
func (br *fdBatchReleaser) releaseOne() {
	if atomic.AddInt32(&br.n, -1) == 0 {
		br.once.Do(br.cleanup)
	}
}

// force 错误路径兜底：不待引用归零直接清理（调用后不得再使用批内 FdBuf）。
func (br *fdBatchReleaser) force() {
	br.once.Do(br.cleanup)
}

func (br *fdBatchReleaser) cleanup() {
	for _, b := range br.bufs {
		if b != nil {
			bufpool.Put(b)
		}
	}
	br.r.ReleasePreviousRead()
	br.sm.PutBack(br.st)
}

// Get 读取对象内 [off, off+size) 子区间并返回整块数据；size=-1 读至对象结尾。
//
// 返回 (data, release, err)：data len==size 为本次调用私有缓冲，调用方用毕必须调用
// release()（幂等）归还（流随之 PutBack 复用）。
//
// 两条路径：
//   - 零拷贝移交（整响应恰一帧且落在单共享内存切片）：data 直接引用共享内存，
//     release 前保持有效（ReadBytes 返回的引用），全程零用户态拷贝。
//   - 对齐汇入（多帧响应/跨切片回退）：逐帧拷入 bufpool 对齐缓冲，恰一次用户态
//     拷贝，release 经 bufpool.Put 归还。正确性不依赖切片连续性。
func (c *ShmConn) Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error) {
	r, err := c.getEx(ctx, key, off, size)
	if err != nil {
		return nil, nil, err
	}
	return r.data, r.release, nil
}

// GetFd 读取对象内 [off, off+size) 子区间并交付为 memfd (fd, offset) 零拷贝源：
// 供 FUSE 读路径 splice（fuse.ReadResultFd），避免共享内存 payload 再拷入用户态缓冲。
//
// 返回 (fd, foff, data, release, err)：
//   - fd > 0：整响应恰一帧且落在单共享内存切片，data 零拷贝引用共享内存（此时
//     data 与 fd/foff 指向同一段内存）；调用方应 splice(fd, foff, size) 取数，
//     splice 完成（含 fallback Pread）后调用 release() 归还帧 pin 并 PutBack 流。
//   - fd == 0：整响应多帧/跨切片（或异常），data 为池化拷贝缓冲（恰一次汇入拷贝），
//     语义与 Get 一致，release() 归还。
//
// 两条路径用毕都必须调用 release()（幂等）。
func (c *ShmConn) GetFd(ctx context.Context, key string, off, size int64) (int, uint64, []byte, func(), error) {
	r, err := c.getEx(ctx, key, off, size)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	return r.fd, r.foff, r.data, r.release, nil
}

func (c *ShmConn) getEx(ctx context.Context, key string, off, size int64) (*getResult, error) {
	if size < 0 {
		total, err := c.Stat(ctx, key)
		if err != nil {
			return nil, err
		}
		size = total - off
	}
	if size < 0 {
		return nil, ierr.ErrInvalidRange
	}
	if size == 0 {
		return &getResult{data: nil, release: func() {}}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, err := c.sm.GetStream()
	if err != nil {
		return nil, err
	}
	if d, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(d)
	}
	if err := shmWriteFrame(st, protocol.OpGetReq, protocol.EncodeGetReq(key, off, size)); err != nil {
		_ = st.Close()
		return nil, err
	}

	r := st.BufferReader()
	var (
		pos  int64
		buf  []byte // 汇集缓冲（多帧路径），out = buf[:size]
		out  []byte // 返回缓冲
		fd   int
		foff uint64
		once sync.Once
	)
	// release 幂等归还：汇集路径归还 bufpool 缓冲；零拷贝路径保持 pin 直至归还；
	// 两者最终都释放帧 pin 并将流 PutBack 复用。
	release := func() {
		once.Do(func() {
			if buf != nil {
				bufpool.Put(buf)
			}
			r.ReleasePreviousRead()
			c.sm.PutBack(st)
		})
	}

	for {
		op, payload, err := shmReadFrame(r)
		if err != nil {
			release()
			return nil, err
		}
		switch op {
		case protocol.OpGetData, protocol.OpGetDataFinal:
			final := op == protocol.OpGetDataFinal
			rem := int64(len(payload))
			if rem > size-pos {
				release()
				return nil, fmt.Errorf("taihu: get stream exceeds requested size")
			}
			if buf == nil && out == nil {
				if rem == size {
					// 整响应恰一帧：零拷贝移交——payload 引用共享内存，release 前有效。
					recordRxDataFrame(int(rem), true)
					out = payload
					pos = size
					if final {
						// 解析 payload 的 memfd (fd, offset)：splice 零拷贝源。
						fd, foff, _ = shmipc.ResolveBufferRef(r, out)
						return &getResult{fd: fd, foff: foff, data: out, release: release}, nil
					}
					continue // 非 final（协议异常）：不释放，等下一帧触发超限报错
				}
				recordRxDataFrame(int(rem), false)
				buf = bufpool.Get(int(size))
				out = buf[:size]
			} else {
				recordRxDataFrame(int(rem), false)
			}
			// 汇入调用方缓冲：payload 拷出后立即释放该帧 pin（环形复用共享内存）。
			pos += int64(copy(out[pos:], payload))
			r.ReleasePreviousRead()
			if final {
				// final 帧：数据流收尾。缺帧（短读）在此报错，等价 TCP OpGetEnd 校验。
				if pos != size {
					release()
					return nil, fmt.Errorf("taihu: get short read: got %d want %d", pos, size)
				}
				return &getResult{fd: fd, foff: foff, data: out, release: release}, nil
			}
		case protocol.OpGetErr:
			code, err := protocol.ReadU32(protocol.NewSliceReader(payload))
			if err != nil {
				release()
				return nil, err
			}
			release()
			return nil, protocol.MapCode(protocol.ErrCode(code))
		default:
			release()
			return nil, fmt.Errorf("taihu: unexpected get frame op %d", op)
		}
	}
}

// GetBatch 在单条流上连发 len(keys) 个 GetReq（每请求 off/size 相同）并按序读回
// len(keys) 个响应 —— 单流多请求 pipeline：相比逐 key Get（每请求 GetStream/
// PutBack + 一写一读一个往返），把流级往返固定开销摊薄到 P 个请求上，配合服务端
// per-stream 异步读流水线把单流在途从 1 提升到 P（benchkit.Batcher，压测用）。
//
// 返回 out[i] 对应 keys[i]：单帧响应（整块 4MiB 零拷贝）直接引用共享内存，多帧
// 响应逐响应汇入 bufpool 缓冲；用毕必须调用返回的 release()（幂等）一次性归还
// 全部帧 pin 与池缓冲，并把流 PutBack 复用。任一响应出错（服务端错误/短读/畸形帧）
// 整个调用失败并关闭流（残留帧不污染可复用流）。off/size 须显式给定（size<0 不支持）。
func (c *ShmConn) GetBatch(ctx context.Context, keys []string, off, size int64) ([][]byte, func(), error) {
	if len(keys) == 0 || size == 0 {
		return nil, func() {}, nil
	}
	if off < 0 || size < 0 {
		return nil, nil, ierr.ErrInvalidRange
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	st, err := c.sm.GetStream()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(d)
	}
	// 单流连发全部 GetReq（整批一次 Flush，帧按序入环）。
	for i, key := range keys {
		shmDbg("client batch send k=%d key=%s off=%d size=%d", i, key, off, size)
		if err := shmWriteFrame(st, protocol.OpGetReq, protocol.EncodeGetReq(key, off, size)); err != nil {
			_ = st.Close()
			return nil, nil, err
		}
	}

	r := st.BufferReader()
	out := make([][]byte, len(keys))
	bufs := make([][]byte, len(keys)) // 多帧响应汇入的 bufpool 缓冲（单帧路径 nil）
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, b := range bufs {
				if b != nil {
					bufpool.Put(b)
				}
			}
			r.ReleasePreviousRead()
			c.sm.PutBack(st)
		})
	}

	for k := range keys {
		// 读第 k 个响应：逐帧直到 final（帧序即响应边界；语义镜像 getEx 单响应）。
		var resp []byte // 本次响应返回缓冲（零拷贝引用或汇入缓冲）
		var buf []byte  // 本次响应汇入缓冲
		var got int
	readResp: // Go gotcha：switch case 内的无标签 break 只跳出 switch；final 后必须跳出
		// 本响应帧循环（否则会越界读到下一响应的帧，重则 exceeds、轻则挂死）。
		for {
			op, payload, ferr := shmReadFrame(r)
			shmDbg("client batch recv k=%d op=%d rem=%d got=%d size=%d", k, op, len(payload), got, size)
			if ferr != nil {
				release()
				return nil, nil, ferr
			}
			switch op {
			case protocol.OpGetData, protocol.OpGetDataFinal:
				final := op == protocol.OpGetDataFinal
				rem := int64(len(payload))
				if rem > size-int64(got) {
					shmDbg("client batch k=%d ERR exceeds: rem=%d got=%d size=%d", k, rem, got, size)
					release()
					return nil, nil, fmt.Errorf("taihu: get batch exceeds requested size")
				}
				if buf == nil && resp == nil {
					if rem == size {
						// 整响应恰一帧：零拷贝引用共享内存，release 前保持有效。
						recordRxDataFrame(int(rem), true)
						resp = payload
						got = int(size)
						if final {
							out[k] = resp
							break readResp
						}
						continue // 非 final（协议异常）：等下一帧触发超限报错
					}
					recordRxDataFrame(int(rem), false)
					buf = bufpool.Get(int(size))
					resp = buf[:size]
				} else {
					recordRxDataFrame(int(rem), false)
				}
				got += int(copy(resp[got:], payload))
				r.ReleasePreviousRead()
				if final {
					if got != int(size) {
						release()
						return nil, nil, fmt.Errorf("taihu: get batch short read: got %d want %d", got, size)
					}
					out[k] = resp
					bufs[k] = buf // 记录汇入缓冲，release 时归还
					break readResp
				}
			case protocol.OpGetErr:
				code, rerr := protocol.ReadU32(protocol.NewSliceReader(payload))
				if rerr != nil {
					release()
					return nil, nil, rerr
				}
				release()
				return nil, nil, protocol.MapCode(protocol.ErrCode(code))
			default:
				release()
				return nil, nil, fmt.Errorf("taihu: unexpected get batch frame op %d", op)
			}
		}
	}
	return out, release, nil
}

// GetFdBatch 在单条流上连发 len(keys) 个 GetReq（每请求 off/size 相同）并按序读回
// len(keys) 个响应，批内每个 key 尽力交付 memfd (fd, foff) 零拷贝 splice 源——
// 对称 GetBatch 的批量 pipeline，同时保留单 key GetFd 的零拷贝语义（批量读不汇入
// 拷贝，否则 splice 优势尽失）。
//
// 返回 out[i] 对应 keys[i]（*FdBuf，字段语义见上）：单帧整块响应直接引用共享内存
// 并解析出 (fd, foff)；多帧/跨切片响应回退为池化拷贝缓冲（fd==0）。批内每个 FdBuf
// 各持一份引用：用毕逐个 FdBuf.Release()（推荐，per-key 归还，全部归还后整体回收）
// 或一次性调用返回的 release 兜底（调用后不得再使用批内 FdBuf）。任一响应出错整个
// 调用失败并关闭流（残留帧不污染可复用流），不返回任何 FdBuf。
func (c *ShmConn) GetFdBatch(ctx context.Context, keys []string, off, size int64) ([]*FdBuf, func(), error) {
	if len(keys) == 0 || size == 0 {
		return nil, func() {}, nil
	}
	if off < 0 || size < 0 {
		return nil, nil, ierr.ErrInvalidRange
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	st, err := c.sm.GetStream()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(d)
	}
	// 单流连发全部 GetReq（整批一次 Flush，帧按序入环）。
	for i, key := range keys {
		shmDbg("client batch fd send k=%d key=%s off=%d size=%d", i, key, off, size)
		if err := shmWriteFrame(st, protocol.OpGetReq, protocol.EncodeGetReq(key, off, size)); err != nil {
			_ = st.Close()
			return nil, nil, err
		}
	}

	r := st.BufferReader()
	out := make([]*FdBuf, len(keys))
	bufs := make([][]byte, len(keys)) // 多帧响应汇入的池化缓冲（单帧路径 nil）
	br := newFdBatchReleaser(len(keys), r, st, c.sm)
	br.bufs = bufs // 清理时统一归还（错误路径也可见已汇入的缓冲）
	abort := br.force
	// 整批兜底：直接强制归还（幂等）。正常路径只逐个 FdBuf.Release()。
	batchRel := abort

	for k := range keys {
		// 读第 k 个响应：逐帧直到 final（帧序即响应边界；语义镜像 GetBatch 单响应）。
		var resp []byte // 本次响应返回缓冲（零拷贝引用或汇入缓冲）
		var buf []byte  // 本次响应汇入缓冲
		var got int
	readResp: // Go gotcha：switch case 内的无标签 break 只跳出 switch；final 后必须跳出
		// 本响应帧循环（否则会越界读到下一响应的帧，重则 exceeds、轻则挂死）。
		for {
			op, payload, ferr := shmReadFrame(r)
			shmDbg("client batch fd recv k=%d op=%d rem=%d got=%d size=%d", k, op, len(payload), got, size)
			if ferr != nil {
				abort()
				return nil, nil, ferr
			}
			switch op {
			case protocol.OpGetData, protocol.OpGetDataFinal:
				final := op == protocol.OpGetDataFinal
				rem := int64(len(payload))
				if rem > size-int64(got) {
					shmDbg("client batch fd k=%d ERR exceeds: rem=%d got=%d size=%d", k, rem, got, size)
					abort()
					return nil, nil, fmt.Errorf("taihu: get fd batch exceeds requested size")
				}
				if buf == nil && resp == nil {
					if rem == size {
						// 整响应恰一帧：零拷贝引用共享内存，并解析 memfd (fd, foff)。
						recordRxDataFrame(int(rem), true)
						resp = payload
						got = int(size)
						if final {
							fd, foff, _ := shmipc.ResolveBufferRef(r, resp)
							out[k] = &FdBuf{Fd: fd, Foff: foff, Data: resp, release: br.releaseOne}
							break readResp
						}
						continue // 非 final（协议异常）：不释放，等下一帧触发超限报错
					}
					recordRxDataFrame(int(rem), false)
					buf = bufpool.Get(int(size))
					resp = buf[:size]
				} else {
					recordRxDataFrame(int(rem), false)
				}
				got += int(copy(resp[got:], payload))
				r.ReleasePreviousRead()
				if final {
					if got != int(size) {
						abort()
						return nil, nil, fmt.Errorf("taihu: get fd batch short read: got %d want %d", got, size)
					}
					out[k] = &FdBuf{Fd: 0, Foff: 0, Data: resp, release: br.releaseOne}
					bufs[k] = buf // 记录汇入缓冲，release 时归还
					break readResp
				}
			case protocol.OpGetErr:
				code, rerr := protocol.ReadU32(protocol.NewSliceReader(payload))
				if rerr != nil {
					abort()
					return nil, nil, rerr
				}
				abort()
				return nil, nil, protocol.MapCode(protocol.ErrCode(code))
			default:
				abort()
				return nil, nil, fmt.Errorf("taihu: unexpected get fd batch frame op %d", op)
			}
		}
	}
	return out, batchRel, nil
}

// PutBatch 在单条流上连发 len(keys) 个完整 Put（每 key 一个 OpPutHeader + OpPutData* +
// OpPutEnd）并按序读回 len(keys) 个 OpResp —— 单流多请求 pipeline（对称 GetBatch）：
// 相比逐 key Put（每请求 GetStream/PutBack + 一写一读一个往返），把流级往返固定开销
// 摊薄到 P 个请求上，配合服务端 per-stream 异步写流水线（inflight>0 + batchWriter）把
// 单流在途写从 1 提升到 P。各 key 数据均取 in 的前 size 字节（与逐 key Put 同 payload 语义）。
//
// 任一响应为业务错误（写失败）或帧畸形时整个调用失败并关闭流：残留的后续响应帧会
// 污染可复用流，不能 PutBack（下次 GetStream 会读到脏帧）。成功时 PutBack 复用。
func (c *ShmConn) PutBatch(ctx context.Context, keys []string, size int64, in []byte) error {
	if len(keys) == 0 || size == 0 {
		return nil
	}
	if int64(len(in)) < size {
		return ierr.ErrShortWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := c.sm.GetStream()
	if err != nil {
		return err
	}
	if d, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(d)
	}
	// 单流连发全部 Put（整批一次 Flush，帧按序入环）。
	for _, key := range keys {
		if err := shmWriteFrame(st, protocol.OpPutHeader, protocol.EncodePutHeader(key, size)); err != nil {
			_ = st.Close()
			return err
		}
		var off int64
		for off < size {
			end := off + protocol.ChunkSize
			if end > size {
				end = size
			}
			if err := shmWriteFrame(st, protocol.OpPutData, in[off:end]); err != nil {
				_ = st.Close()
				return err
			}
			off = end
		}
		if err := shmWriteFrame(st, protocol.OpPutEnd, nil); err != nil {
			_ = st.Close()
			return err
		}
	}
	// 按序读回 P 个 OpResp；每帧读完即释放 pin（控制帧，payload 解析后不再引用）。
	r := st.BufferReader()
	for range keys {
		op, payload, err := shmReadFrame(r)
		if err != nil {
			_ = st.Close()
			return err
		}
		if op != protocol.OpResp {
			_ = st.Close()
			return fmt.Errorf("taihu: unexpected put batch response op %d", op)
		}
		code, err := protocol.ReadU32(protocol.NewSliceReader(payload))
		if err != nil {
			_ = st.Close()
			return err
		}
		if e := protocol.MapCode(protocol.ErrCode(code)); e != nil {
			// 业务错误：流上仍残留未读响应帧，关闭丢弃（不能 PutBack 复用）。
			_ = st.Close()
			return e
		}
		r.ReleasePreviousRead()
	}
	c.sm.PutBack(st)
	return nil
}

// PutBatchKeys 在单条流上连发 len(keys) 个完整 Put（每 key 数据取自 datas[i]，各 key
// 内容相互独立）并按序读回 len(keys) 个 OpResp —— 对称 GetFdBatch 的批量写 pipeline。
// 与 PutBatch 的差异仅在数据来源：PutBatch 各 key 均取 in 的前 size 字节（bench 用，
// 全 key 同内容）；PutBatchKeys 各 key 写 datas[i] 的前 size 字节（FUSE 数据面用，
// 每块内容不同，供攒批器 concat 后一次下发）。其余语义一致：任一响应为业务错误或
// 帧畸形时整个调用失败并关闭流（残留响应帧不能 PutBack 复用）。
func (c *ShmConn) PutBatchKeys(ctx context.Context, keys []string, size int64, datas [][]byte) error {
	if len(keys) == 0 || size == 0 {
		return nil
	}
	if len(datas) != len(keys) {
		return fmt.Errorf("taihu: put batch keys %d != datas %d", len(keys), len(datas))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := c.sm.GetStream()
	if err != nil {
		return err
	}
	if d, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(d)
	}
	// 单流连发全部 Put（整批一次 Flush，帧按序入环）。
	for i, key := range keys {
		in := datas[i]
		if int64(len(in)) < size {
			_ = st.Close()
			return ierr.ErrShortWrite
		}
		if err := shmWriteFrame(st, protocol.OpPutHeader, protocol.EncodePutHeader(key, size)); err != nil {
			_ = st.Close()
			return err
		}
		var off int64
		for off < size {
			end := off + protocol.ChunkSize
			if end > size {
				end = size
			}
			if err := shmWriteFrame(st, protocol.OpPutData, in[off:end]); err != nil {
				_ = st.Close()
				return err
			}
			off = end
		}
		if err := shmWriteFrame(st, protocol.OpPutEnd, nil); err != nil {
			_ = st.Close()
			return err
		}
	}
	// 按序读回 P 个 OpResp；每帧读完即释放 pin（控制帧，payload 解析后不再引用）。
	r := st.BufferReader()
	for range keys {
		op, payload, err := shmReadFrame(r)
		if err != nil {
			_ = st.Close()
			return err
		}
		if op != protocol.OpResp {
			_ = st.Close()
			return fmt.Errorf("taihu: unexpected put batch keys response op %d", op)
		}
		code, err := protocol.ReadU32(protocol.NewSliceReader(payload))
		if err != nil {
			_ = st.Close()
			return err
		}
		if e := protocol.MapCode(protocol.ErrCode(code)); e != nil {
			// 业务错误：流上仍残留未读响应帧，关闭丢弃（不能 PutBack 复用）。
			_ = st.Close()
			return e
		}
		r.ReleasePreviousRead()
	}
	c.sm.PutBack(st)
	return nil
}

// PutBegin 开始共享内存零拷贝写：GetStream + 发 OpPutHeader，返回 ShmPutWriter。
// 调用方随后 Reserve 拿共享内存可写区直写（免 memcpy），全部写毕后 Commit 收尾。
// 协议与服务端 handleShmPut 兼容（OpPutData 带 pad 数据帧，服务端逐帧直写设备）。
func (c *ShmConn) PutBegin(ctx context.Context, key string, size int64) (*ShmPutWriter, error) {
	if size < 0 {
		return nil, ierr.ErrInvalidRange
	}
	st, err := c.sm.GetStream()
	if err != nil {
		return nil, err
	}
	if d, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(d)
	}
	if err := shmWriteFrame(st, protocol.OpPutHeader, protocol.EncodePutHeader(key, size)); err != nil {
		c.sm.PutBack(st)
		return nil, err
	}
	return &ShmPutWriter{c: c, st: st, size: size}, nil
}

// ShmPutWriter 共享内存零拷贝写流（一个对象一个，持有流与逐帧状态）。
// Reserve 返回共享内存数据区直接引用（4K 对齐），调用方直接写入（零拷贝），
// 写满 ChunkSize 自动切帧；Commit 发 OpPutEnd 并收响应。
type ShmPutWriter struct {
	c         *ShmConn
	st        *shmipc.Stream
	size      int64
	written   int64
	curLen    int    // 当前帧已写 payload 字节
	frameHead []byte // 当前帧首区域 [0:5] 帧头引用（共享内存）
	err       error
}

// Reserve 返回 n 字节共享内存可写区（零拷贝直写）。单次 n 不得超过 ChunkSize
// （4MiB）；更大对象请分块多次 Reserve。当前帧写满自动切帧（Flush 旧帧）。
func (w *ShmPutWriter) Reserve(n int) ([]byte, error) {
	if w.err != nil {
		return nil, w.err
	}
	if n <= 0 {
		return nil, nil
	}
	if n > protocol.ChunkSize {
		w.err = fmt.Errorf("taihu: reserve %d > ChunkSize %d", n, protocol.ChunkSize)
		return nil, w.err
	}
	// 切帧：当前帧 + n 超过单帧上限 → Flush 当前帧并开启新帧。
	if w.curLen > 0 && w.curLen+n > protocol.ChunkSize {
		if err := shmCommitFrame(w.st, w.frameHead, protocol.OpPutData, w.curLen); err != nil {
			w.err = err
			return nil, err
		}
		w.curLen, w.frameHead = 0, nil
	}
	full, err := w.st.BufferWriter().Reserve(protocol.ShmDataPad + n)
	if err != nil {
		w.err = err
		return nil, err
	}
	if w.frameHead == nil {
		// 新帧首区域：帧头 [4B len][1B op] 位于区域前 5B（pad 4096 内含帧头）。
		w.frameHead = full[:shmLenPrefixLen+shmOpLen]
	}
	w.curLen += n
	w.written += int64(n)
	return full[protocol.ShmDataPad : protocol.ShmDataPad+n], nil
}

// Write 拷贝写（通用语义：内部 Reserve + copy）。
func (w *ShmPutWriter) Write(p []byte) (int, error) {
	buf, err := w.Reserve(len(p))
	if err != nil {
		return 0, err
	}
	return copy(buf, p), nil
}

// Commit 结束写入：Flush 末帧 + 发 OpPutEnd + 读 OpResp + PutBack 流。
// 已写字节 != size 时返回 ErrShortWrite。
func (w *ShmPutWriter) Commit() error {
	defer w.c.sm.PutBack(w.st)
	if w.err != nil {
		return w.err
	}
	if w.written != w.size {
		w.err = ierr.ErrShortWrite
		return w.err
	}
	if w.curLen > 0 {
		if err := shmCommitFrame(w.st, w.frameHead, protocol.OpPutData, w.curLen); err != nil {
			return err
		}
		w.curLen, w.frameHead = 0, nil
	}
	if err := shmWriteFrame(w.st, protocol.OpPutEnd, nil); err != nil {
		return err
	}
	op, payload, err := shmReadFrame(w.st.BufferReader())
	if err != nil {
		return err
	}
	if op != protocol.OpResp {
		return fmt.Errorf("taihu: unexpected put response op %d", op)
	}
	code, err := protocol.ReadU32(protocol.NewSliceReader(payload))
	if err != nil {
		return err
	}
	return protocol.MapCode(protocol.ErrCode(code))
}

// Delete 删除对象映射（key 不存在返回 ErrNotFound）。
func (c *ShmConn) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := c.sm.GetStream()
	if err != nil {
		return err
	}
	defer c.sm.PutBack(st)
	if d, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(d)
	}
	if err := shmWriteFrame(st, protocol.OpDelReq, protocol.EncodeKeyReq(key)); err != nil {
		return err
	}
	op, payload, err := shmReadFrame(st.BufferReader())
	if err != nil {
		return err
	}
	if op != protocol.OpResp {
		return fmt.Errorf("taihu: unexpected delete response op %d", op)
	}
	code, err := protocol.ReadU32(protocol.NewSliceReader(payload))
	if err != nil {
		return err
	}
	return protocol.MapCode(protocol.ErrCode(code))
}

// Stat 返回对象逻辑大小（key 不存在返回 ErrNotFound）。
func (c *ShmConn) Stat(ctx context.Context, key string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	st, err := c.sm.GetStream()
	if err != nil {
		return 0, err
	}
	defer c.sm.PutBack(st)
	if d, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(d)
	}
	if err := shmWriteFrame(st, protocol.OpStatReq, protocol.EncodeKeyReq(key)); err != nil {
		return 0, err
	}
	op, payload, err := shmReadFrame(st.BufferReader())
	if err != nil {
		return 0, err
	}
	switch op {
	case protocol.OpStatResp:
		sz, err := protocol.ReadU64(protocol.NewSliceReader(payload))
		if err != nil {
			return 0, err
		}
		return int64(sz), nil
	case protocol.OpResp:
		code, err := protocol.ReadU32(protocol.NewSliceReader(payload))
		if err != nil {
			return 0, err
		}
		return 0, protocol.MapCode(protocol.ErrCode(code))
	default:
		return 0, fmt.Errorf("taihu: unexpected stat response op %d", op)
	}
}
