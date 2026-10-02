// 客户端：拨号、连接生命周期，以及 Put/Get/Delete/Stat 四个 RPC 的调用侧。
// 帧层（frame.go）负责收发与多路复用，本文件只负责把一次 RPC 表达成若干帧。
package transport

import (
	"context"
	"fmt"
	"time"

	"github.com/liucxer/taihu/third_party/netpoll"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/transport/protocol"
	"github.com/liucxer/taihu/pkg/ierr"
)

// dialTimeout 客户端拨号超时。
const dialTimeout = 10 * time.Second

// FdBuf 批量读的单个 key 交付结果：Fd>0 时 Data 零拷贝引用共享内存
// （memfd(Fd,Foff) 为 splice 源，Data 与 fd/foff 指向同一段内存）；Fd==0 时 Data
// 为池化拷贝缓冲。两种情况用毕都必须 Release()（幂等）——批内各 FdBuf 独立引用
// 计数，全部归还后一次性释放帧 pin 并把流 PutBack 复用。
//
// 类型本身平台无关（仅字段 + 归还回调），定义在本文件而非 client_shm_linux.go：
// internal/rpcclient/storage_rpc.go 的 batchConn 接口与 TCP 回退路径在非 linux
// 平台也要能命名本类型；shm 专属的 GetFdBatch 实现仍留在 client_shm_linux.go。
type FdBuf struct {
	Fd      int
	Foff    uint64
	Data    []byte
	release func()
}

// NewFdBuf 构造带归还回调的 FdBuf（供 transport 包外构造：rpcclient 批量回退路径等，
// 逐 key 的 release 无法在包外直接赋值）。release 可为 nil（此时 Release() 为 no-op）。
func NewFdBuf(fd int, foff uint64, data []byte, release func()) *FdBuf {
	return &FdBuf{Fd: fd, Foff: foff, Data: data, release: release}
}

// Release 归还本 FdBuf 底层缓冲（幂等）。
func (b *FdBuf) Release() {
	if b != nil && b.release != nil {
		b.release()
		b.release = nil
	}
}

// NewClientConn 包装客户端连接并启动读循环。
func NewClientConn(c netpoll.Connection) *Conn {
	conn := &Conn{
		c:        c,
		streams:  make(map[uint32]*stream),
		closed:   make(chan struct{}),
		dispatch: clientDispatch,
	}
	go conn.readLoop()
	return conn
}

// DialClient 拨号并建立客户端连接。
func DialClient(network, addr string) (*Conn, error) {
	c, err := netpoll.DialConnection(network, addr, dialTimeout)
	if err != nil {
		return nil, err
	}
	return NewClientConn(c), nil
}

// Close 关闭连接并解除所有流的阻塞。
func (c *Conn) Close() error {
	c.closeAll()
	return c.c.Close()
}

// clientDispatch 客户端侧分发：按 streamID 路由到对应流；未知流/已结束流
// （RPC 完成或取消）直接丢弃该帧，不中断连接。
func clientDispatch(c *Conn, sid uint32, op protocol.OpCode, sub netpoll.Reader) deliverResult {
	c.mu.Lock()
	st := c.streams[sid]
	c.mu.Unlock()
	if st == nil {
		_ = sub.Release()
		return deliverOK
	}
	select {
	case st.in <- frameMsg{op, sub}:
		return deliverOK
	case <-st.done:
		_ = sub.Release()
		return deliverOK
	case <-c.closed:
		_ = sub.Release()
		return deliverFatal
	}
}

// Put 上传对象（与引擎侧 storage.Storage.Put 同语义）。首帧发 key+size，随后按
// ChunkSize 分帧零拷贝发送（in 在返回前不会被引用），OpPutEnd 后等待 OpResp。
func (c *Conn) Put(ctx context.Context, key string, size int64, in []byte) error {
	if int64(len(in)) < size {
		return ierr.ErrShortWrite
	}
	st := c.newStream()
	defer c.removeStream(st)
	if err := c.writeFrame(st.id, protocol.OpPutHeader, protocol.EncodePutHeader(key, size)); err != nil {
		return err
	}
	var off int64
	for off < size {
		end := off + protocol.ChunkSize
		if end > size {
			end = size
		}
		if err := c.writeFrame(st.id, protocol.OpPutData, in[off:end]); err != nil {
			return err
		}
		off = end
	}
	if err := c.writeFrame(st.id, protocol.OpPutEnd, nil); err != nil {
		return err
	}
	msg, err := c.await(ctx, st)
	if err != nil {
		return err
	}
	defer msg.r.Release()
	if msg.op != protocol.OpResp {
		return fmt.Errorf("taihu: unexpected put response op %d", msg.op)
	}
	code, err := protocol.ReadU32(msg.r)
	if err != nil {
		return err
	}
	return protocol.MapCode(protocol.ErrCode(code))
}

// Get 读取对象内 [off, off+size) 子区间并返回整块数据（size=-1 读至结尾）。
//
// 返回 (data, release, err)：data len==size 为本次调用私有缓冲；调用方用毕必须调用
// release()（幂等）归还。
//
// 实现为对齐汇入：逐帧 ReadCopy/Next 汇入 bufpool 对齐缓冲，恰一次用户态拷贝，
// release 经 bufpool.Put 归还。正确性不依赖 netpoll 节点复用。
//
// 曾用过 netpoll 的零拷贝移交 TakeTry（整响应恰一帧时直接移交收流节点缓冲，零拷贝）。
// 实测该路径存在静默数据错配（移交后整块缓冲被归还池并复用，内容被后续收流覆盖），
// 仅收紧「帧独占整块」（base==0）仍会复现，故 TCP 路径已停用，netpoll 侧保留
// base==0 护栏与单测；shm 数据面的单帧移交不受影响（recordRxDataFrame 的 zeroCopy）。
func (c *Conn) Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error) {
	if size < 0 {
		total, err := c.Stat(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		size = total - off
	}
	if size < 0 {
		return nil, nil, ierr.ErrInvalidRange
	}
	if size == 0 {
		return nil, func() {}, nil
	}
	st := c.newStream()
	defer c.removeStream(st)
	if err := c.writeFrame(st.id, protocol.OpGetReq, protocol.EncodeGetReq(key, off, size)); err != nil {
		return nil, nil, err
	}

	var (
		pos      int64
		buf      []byte // 汇集缓冲（对齐池，out = buf[:size]）
		out      []byte // 返回缓冲
		disposed bool
	)

	dispose := func() {
		if disposed {
			return
		}
		disposed = true
		if buf != nil {
			bufpool.Put(buf)
		}
	}

	for {
		msg, err := c.await(ctx, st)
		if err != nil {
			dispose()
			return nil, nil, err
		}
		switch msg.op {
		case protocol.OpGetData, protocol.OpGetDataFinal:
			final := msg.op == protocol.OpGetDataFinal
			rem := int64(msg.r.Len())
			statRxFrames.Add(1)
			statRxBytes.Add(rem)
			if rem == protocol.ChunkSize {
				statRxData4M.Add(1)
			}
			if rem > size-pos {
				msg.r.Release()
				dispose()
				return nil, nil, fmt.Errorf("taihu: get stream exceeds requested size")
			}
			if buf == nil {
				buf = bufpool.Get(int(size))
				out = buf[:size]
			}

			if rc, ok := msg.r.(interface{ ReadCopy([]byte) (int, error) }); ok {
				statRxCopy.Add(1)
				n, err := rc.ReadCopy(out[pos : pos+int64(rem)])
				if err != nil {
					msg.r.Release()
					dispose()
					return nil, nil, err
				}
				pos += int64(n)
			} else {
				statRxCopy.Add(1)
				p, err := msg.r.Next(int(rem))
				if err != nil {
					msg.r.Release()
					dispose()
					return nil, nil, err
				}
				pos += int64(copy(out[pos:], p))
			}
			msg.r.Release()
			if final {
				if pos != size {
					dispose()
					return nil, nil, fmt.Errorf("taihu: get short read: got %d want %d", pos, size)
				}
				return out, dispose, nil
			}
		case protocol.OpGetEnd:
			msg.r.Release()
			if pos != size {
				dispose()
				return nil, nil, fmt.Errorf("taihu: get short read: got %d want %d", pos, size)
			}
			return out, dispose, nil
		case protocol.OpGetErr:
			code, err := protocol.ReadU32(msg.r)
			msg.r.Release()
			if err != nil {
				dispose()
				return nil, nil, err
			}
			dispose()
			return nil, nil, protocol.MapCode(protocol.ErrCode(code))
		default:
			msg.r.Release()
			dispose()
			return nil, nil, fmt.Errorf("taihu: unexpected get frame op %d", msg.op)
		}
	}
}

// GetBatch 在单条流上连发 len(keys) 个 GetReq（每请求 off/size 相同）并按序读回
// len(keys) 个响应 —— 单流多请求 pipeline：相比逐 key Get（每请求开流/关流 +
// 一写一读一个往返），把流级往返固定开销摊薄到 P 个请求上，配合服务端 per-stream
// 异步读流水线（-tcp-inflight）把单流在途从 1 提升到 P（benchkit.Batcher，压测用）。
//
// 返回 out[i] 对应 keys[i]（对齐汇入 bufpool 缓冲，语义与 Get 一致）；用毕必须调用
// 返回的 release()（幂等）一次性归还全部缓冲并关流。任一响应出错（服务端错误/短读/
// 畸形帧）整个调用失败并释放已分配缓冲。off/size 须显式给定（size<0 不支持，与
// 单 Get 的 size==-1 推导不同 —— 批量各 key 尺寸未知，须调用方显式限定）。
func (c *Conn) GetBatch(ctx context.Context, keys []string, off, size int64) ([][]byte, func(), error) {
	if len(keys) == 0 || size == 0 {
		return nil, func() {}, nil
	}
	if off < 0 || size < 0 {
		return nil, nil, ierr.ErrInvalidRange
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	st := c.newStream()

	var bufs [][]byte // 各 key 汇入缓冲（未分配为 nil），release 时统一归还
	disposed := false
	release := func() {
		if disposed {
			return
		}
		disposed = true
		for _, b := range bufs {
			if b != nil {
				bufpool.Put(b)
			}
		}
		c.removeStream(st)
	}
	fail := func(err error) ([][]byte, func(), error) {
		release()
		return nil, nil, err
	}

	// 单流连发全部 GetReq：持 wmu 连发（writeFrameLocked 不做 Flush）后整批一次
	// Flush，摊薄 Flush/waitFlush 固定开销。EncodeGetReq 每次分配新负载，WriteBinary
	// 零拷贝引用安全（Flush 排空后即可复用）。
	c.wmu.Lock()
	for _, key := range keys {
		if err := c.writeFrameLocked(st.id, protocol.OpGetReq, protocol.EncodeGetReq(key, off, size)); err != nil {
			c.wmu.Unlock()
			return fail(err)
		}
	}
	err := c.c.Writer().Flush()
	c.wmu.Unlock()
	if err != nil {
		return fail(err)
	}

	out := make([][]byte, len(keys))
	bufs = make([][]byte, len(keys))
	for k := range keys {
		// 读第 k 个响应：逐帧直到 final（帧序即响应边界；语义镜像 Get 单响应）。
		var resp []byte // 本次响应返回缓冲（汇入缓冲）
		var buf []byte  // 本次响应汇入缓冲
		var got int64
	readResp: // Go gotcha：switch case 内的无标签 break 只跳出 switch；final 后必须跳出
		// 本响应帧循环（否则会越界读到下一响应的帧，重则 exceeds、轻则挂死）。
		for {
			msg, err := c.await(ctx, st)
			if err != nil {
				return fail(err)
			}
			switch msg.op {
			case protocol.OpGetData, protocol.OpGetDataFinal:
				final := msg.op == protocol.OpGetDataFinal
				rem := int64(msg.r.Len())
				statRxFrames.Add(1)
				statRxBytes.Add(rem)
				if rem == protocol.ChunkSize {
					statRxData4M.Add(1)
				}
				if rem > size-got {
					msg.r.Release()
					return fail(fmt.Errorf("taihu: get batch exceeds requested size"))
				}
				if buf == nil {
					buf = bufpool.Get(int(size))
					resp = buf[:size]
				}

				if rc, ok := msg.r.(interface{ ReadCopy([]byte) (int, error) }); ok {
					statRxCopy.Add(1)
					n, err := rc.ReadCopy(resp[got : got+rem])
					if err != nil {
						msg.r.Release()
						return fail(err)
					}
					got += int64(n)
				} else {
					statRxCopy.Add(1)
					p, err := msg.r.Next(int(rem))
					if err != nil {
						msg.r.Release()
						return fail(err)
					}
					got += int64(copy(resp[got:], p))
				}
				msg.r.Release()
				if final {
					if got != size {
						return fail(fmt.Errorf("taihu: get batch short read: got %d want %d", got, size))
					}
					out[k] = resp
					bufs[k] = buf
					break readResp
				}
			case protocol.OpGetErr:
				code, rerr := protocol.ReadU32(msg.r)
				msg.r.Release()
				if rerr != nil {
					return fail(rerr)
				}
				return fail(protocol.MapCode(protocol.ErrCode(code)))
			default:
				msg.r.Release()
				return fail(fmt.Errorf("taihu: unexpected get batch frame op %d", msg.op))
			}
		}
	}
	return out, release, nil
}

// Delete 删除对象映射（key 不存在返回 ErrNotFound）。
func (c *Conn) Delete(ctx context.Context, key string) error {
	st := c.newStream()
	defer c.removeStream(st)
	if err := c.writeFrame(st.id, protocol.OpDelReq, protocol.EncodeKeyReq(key)); err != nil {
		return err
	}
	msg, err := c.await(ctx, st)
	if err != nil {
		return err
	}
	defer msg.r.Release()
	if msg.op != protocol.OpResp {
		return fmt.Errorf("taihu: unexpected delete response op %d", msg.op)
	}
	code, err := protocol.ReadU32(msg.r)
	if err != nil {
		return err
	}
	return protocol.MapCode(protocol.ErrCode(code))
}

// Stat 返回对象逻辑大小（key 不存在返回 ErrNotFound）。
func (c *Conn) Stat(ctx context.Context, key string) (int64, error) {
	st := c.newStream()
	defer c.removeStream(st)
	if err := c.writeFrame(st.id, protocol.OpStatReq, protocol.EncodeKeyReq(key)); err != nil {
		return 0, err
	}
	msg, err := c.await(ctx, st)
	if err != nil {
		return 0, err
	}
	defer msg.r.Release()
	switch msg.op {
	case protocol.OpStatResp:
		sz, err := protocol.ReadU64(msg.r)
		if err != nil {
			return 0, err
		}
		return int64(sz), nil
	case protocol.OpResp:
		code, err := protocol.ReadU32(msg.r)
		if err != nil {
			return 0, err
		}
		return 0, protocol.MapCode(protocol.ErrCode(code))
	default:
		return 0, fmt.Errorf("taihu: unexpected stat response op %d", msg.op)
	}
}
