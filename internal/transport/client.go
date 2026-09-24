// 客户端：拨号、连接生命周期，以及 Put/Get/Delete/Stat 四个 RPC 的调用侧。
// 帧层（frame.go）负责收发与多路复用，本文件只负责把一次 RPC 表达成若干帧。
package transport

import (
	"context"
	"fmt"
	"time"

	"github.com/liucxer/taihu/third_party/netpoll"

	"github.com/liucxer/taihu/internal/transport/protocol"
	"github.com/liucxer/taihu/pkg/bufpool"
	"github.com/liucxer/taihu/pkg/ierr"
)

// dialTimeout 客户端拨号超时。
const dialTimeout = 10 * time.Second

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
