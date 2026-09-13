//go:build linux

// 共享内存 IPC 客户端（shmipc-go）：数据面共享内存零拷贝，控制面 unix socket。
// 与 TCP 客户端（conn.go）并存、接口同构（rpcclient.rpcConn），调用方可无感切换。
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
	"time"

	"github.com/cloudwego/shmipc-go"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/pkg/taihu"
)

// 共享内存缓冲配置：小切片承载控制帧（请求/一元响应），大切片承载 4MiB 数据帧
// （单切片零拷贝读）。容量按会话在途帧数估算；memfd 稀疏分配，物理内存按写入页计。
//
// 大切片数量 = BufferCap × 大档百分比 / shmSliceSize：读路径每个并发流在途 1 个
// 4MiB 响应切片，池数量必须 ≥ 期望并发读线程数，否则 Reserve 失败自动 fallback
// 到 unix socket 拷贝（带宽断崖 + 熔断）。故大切片档占比拉到 95%（控制帧仅占
// 5% 小巧致密），1GiB 池 ≈ 240 个 4MiB 切片，覆盖 128+ 并发读。
const (
	shmSmallSlice   = 16 * 1024         // 控制帧切片数据容量
	shmBufferCap    = 1 << 30           // 每会话共享内存容量 1GiB
	shmMaxStreamNum = 256               // 流池上限（超过 GetStream 阻塞等待 PutBack）
	shmInitTimeout  = 30 * time.Second  // 会话初始化（建 memfd + 握手）超时
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
		{Size: shmSliceSize, Percent: 95},
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

// Put 上传对象（与 TCP Conn.Put 同语义）：PutHeader → PutData* → PutEnd → opResp。
// 数据帧一次 Reserve 直写共享内存（一次用户态拷贝，无内核参与），Flush 即对端可见。
func (c *ShmConn) Put(ctx context.Context, key string, size int64, in []byte) error {
	if int64(len(in)) < size {
		return taihu.ErrShortWrite
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
	if err := shmWriteFrame(st, opPutHeader, encodePutHeader(key, size)); err != nil {
		return err
	}
	var off int64
	for off < size {
		end := off + chunkSize
		if end > size {
			end = size
		}
		if err := shmWriteFrame(st, opPutData, in[off:end]); err != nil {
			return err
		}
		off = end
	}
	if err := shmWriteFrame(st, opPutEnd, nil); err != nil {
		return err
	}
	op, payload, err := shmReadFrame(st.BufferReader())
	if err != nil {
		return err
	}
	if op != opResp {
		return fmt.Errorf("taihu: unexpected put response op %d", op)
	}
	code, err := readU32(newSliceReader(payload))
	if err != nil {
		return err
	}
	return mapCode(errCode(code))
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
	if size < 0 {
		total, err := c.Stat(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		size = total - off
	}
	if size < 0 {
		return nil, nil, taihu.ErrInvalidRange
	}
	if size == 0 {
		return nil, func() {}, nil
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
	if err := shmWriteFrame(st, opGetReq, encodeGetReq(key, off, size)); err != nil {
		_ = st.Close()
		return nil, nil, err
	}

	r := st.BufferReader()
	var (
		pos int64
		buf []byte // 汇集缓冲（多帧路径），out = buf[:size]
		out []byte // 返回缓冲
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
			return nil, nil, err
		}
		switch op {
		case opGetData, opGetDataFinal:
			final := op == opGetDataFinal
			rem := int64(len(payload))
			if rem > size-pos {
				release()
				return nil, nil, fmt.Errorf("taihu: get stream exceeds requested size")
			}
			if buf == nil && out == nil {
				if rem == size {
					// 整响应恰一帧：零拷贝移交——payload 引用共享内存，release 前有效。
					recordRxDataFrame(int(rem), true)
					out = payload
					pos = size
					if final {
						return out, release, nil
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
				// final 帧：数据流收尾。缺帧（短读）在此报错，等价 TCP opGetEnd 校验。
				if pos != size {
					release()
					return nil, nil, fmt.Errorf("taihu: get short read: got %d want %d", pos, size)
				}
				return out, release, nil
			}
		case opGetErr:
			code, err := readU32(newSliceReader(payload))
			if err != nil {
				release()
				return nil, nil, err
			}
			release()
			return nil, nil, mapCode(errCode(code))
		default:
			release()
			return nil, nil, fmt.Errorf("taihu: unexpected get frame op %d", op)
		}
	}
}

// PutBegin 开始共享内存零拷贝写：GetStream + 发 opPutHeader，返回 ShmPutWriter。
// 调用方随后 Reserve 拿共享内存可写区直写（免 memcpy），全部写毕后 Commit 收尾。
// 协议与服务端 handleShmPut 兼容（opPutData 带 pad 数据帧，服务端逐帧直写设备）。
func (c *ShmConn) PutBegin(ctx context.Context, key string, size int64) (*ShmPutWriter, error) {
	if size < 0 {
		return nil, taihu.ErrInvalidRange
	}
	st, err := c.sm.GetStream()
	if err != nil {
		return nil, err
	}
	if d, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(d)
	}
	if err := shmWriteFrame(st, opPutHeader, encodePutHeader(key, size)); err != nil {
		c.sm.PutBack(st)
		return nil, err
	}
	return &ShmPutWriter{c: c, st: st, size: size}, nil
}

// ShmPutWriter 共享内存零拷贝写流（一个对象一个，持有流与逐帧状态）。
// Reserve 返回共享内存数据区直接引用（4K 对齐），调用方直接写入（零拷贝），
// 写满 chunkSize 自动切帧；Commit 发 opPutEnd 并收响应。
type ShmPutWriter struct {
	c        *ShmConn
	st       *shmipc.Stream
	size     int64
	written  int64
	curLen   int    // 当前帧已写 payload 字节
	frameHead []byte // 当前帧首区域 [0:5] 帧头引用（共享内存）
	err      error
}

// Reserve 返回 n 字节共享内存可写区（零拷贝直写）。单次 n 不得超过 chunkSize
// （4MiB）；更大对象请分块多次 Reserve。当前帧写满自动切帧（Flush 旧帧）。
func (w *ShmPutWriter) Reserve(n int) ([]byte, error) {
	if w.err != nil {
		return nil, w.err
	}
	if n <= 0 {
		return nil, nil
	}
	if n > chunkSize {
		w.err = fmt.Errorf("taihu: reserve %d > chunkSize %d", n, chunkSize)
		return nil, w.err
	}
	// 切帧：当前帧 + n 超过单帧上限 → Flush 当前帧并开启新帧。
	if w.curLen > 0 && w.curLen+n > chunkSize {
		if err := shmCommitFrame(w.st, w.frameHead, opPutData, w.curLen); err != nil {
			w.err = err
			return nil, err
		}
		w.curLen, w.frameHead = 0, nil
	}
	full, err := w.st.BufferWriter().Reserve(shmDataPad + n)
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
	return full[shmDataPad : shmDataPad+n], nil
}

// Write 拷贝写（通用语义：内部 Reserve + copy）。
func (w *ShmPutWriter) Write(p []byte) (int, error) {
	buf, err := w.Reserve(len(p))
	if err != nil {
		return 0, err
	}
	return copy(buf, p), nil
}

// Commit 结束写入：Flush 末帧 + 发 opPutEnd + 读 opResp + PutBack 流。
// 已写字节 != size 时返回 ErrShortWrite。
func (w *ShmPutWriter) Commit() error {
	defer w.c.sm.PutBack(w.st)
	if w.err != nil {
		return w.err
	}
	if w.written != w.size {
		w.err = taihu.ErrShortWrite
		return w.err
	}
	if w.curLen > 0 {
		if err := shmCommitFrame(w.st, w.frameHead, opPutData, w.curLen); err != nil {
			return err
		}
		w.curLen, w.frameHead = 0, nil
	}
	if err := shmWriteFrame(w.st, opPutEnd, nil); err != nil {
		return err
	}
	op, payload, err := shmReadFrame(w.st.BufferReader())
	if err != nil {
		return err
	}
	if op != opResp {
		return fmt.Errorf("taihu: unexpected put response op %d", op)
	}
	code, err := readU32(newSliceReader(payload))
	if err != nil {
		return err
	}
	return mapCode(errCode(code))
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
	if err := shmWriteFrame(st, opDelReq, encodeKeyReq(key)); err != nil {
		return err
	}
	op, payload, err := shmReadFrame(st.BufferReader())
	if err != nil {
		return err
	}
	if op != opResp {
		return fmt.Errorf("taihu: unexpected delete response op %d", op)
	}
	code, err := readU32(newSliceReader(payload))
	if err != nil {
		return err
	}
	return mapCode(errCode(code))
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
	if err := shmWriteFrame(st, opStatReq, encodeKeyReq(key)); err != nil {
		return 0, err
	}
	op, payload, err := shmReadFrame(st.BufferReader())
	if err != nil {
		return 0, err
	}
	switch op {
	case opStatResp:
		sz, err := readU64(newSliceReader(payload))
		if err != nil {
			return 0, err
		}
		return int64(sz), nil
	case opResp:
		code, err := readU32(newSliceReader(payload))
		if err != nil {
			return 0, err
		}
		return 0, mapCode(errCode(code))
	default:
		return 0, fmt.Errorf("taihu: unexpected stat response op %d", op)
	}
}
