// 帧层：netpoll 连接上 [4B len][4B streamID][1B op][payload] 帧的收发与流多路复用。
// 客户端（client.go）与服务端（server.go）共用本文件 —— 双方各自注入一个 dispatch，
// 读循环只负责收帧、按 streamID 投递，不理解任何 RPC 语义。
package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/liucxer/taihu/third_party/netpoll"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/transport/protocol"
)

// init 使 netpoll 接收缓冲改用 bufpool 对齐分配：收流帧载荷落在单个对齐节点内时，
// 客户端 Get 可直接移交该缓冲给调用方（零拷贝），并经由 bufpool.Put 安全归还。
//
// 收流节点进一步改为精确尺寸对齐分配（容量=单帧线上总长 InputNodeSize）：
// 每帧独占一个节点，读满一帧后 book 剩余容量为 0、节点不再被复用——这是
// TakeTry 零拷贝移交（一帧一缓冲、移交后不被 netpoll 再写入）的前置条件。
func init() {
	netpoll.SetAlignedAllocator(bufpool.Get, bufpool.Put)
	netpoll.SetInputAlignedAllocator(bufpool.GetExact, bufpool.PutExact)
	netpoll.SetInputNodeSize(protocol.InputNodeSize)
}

// streamInCap 每流投递缓冲上限：读循环背压到流处理器消费速度。
const streamInCap = 8

var errConnClosed = errors.New("taihu: connection closed")

// frameMsg 读循环投递给流处理器的一帧：op 已解析，r 为零拷贝子 Reader（定位在
// payload 起点，Len() 即负载长度；无负载帧 Len()==0）。用毕必须 r.Release()。
type frameMsg struct {
	op protocol.OpCode
	r  netpoll.Reader
}

// stream 一个 RPC 流的接收队列与生命周期信号。
// 消费方（RPC 处理器/调用方）从 in 取帧；连接关闭或 RPC 结束时由 finish 关闭 done，
// 使读循环对被阻塞的投递解除（select done 分支）——in 通道永不被 close，避免
// 发送方与关闭方竞争。
type stream struct {
	id   uint32
	in   chan frameMsg
	done chan struct{}
	once sync.Once
}

func newStream(id uint32) *stream {
	return &stream{id: id, in: make(chan frameMsg, streamInCap), done: make(chan struct{})}
}

// finish 关闭 done（幂等）。消费方结束或连接关闭时调用。
func (st *stream) finish() { st.once.Do(func() { close(st.done) }) }

// deliverResult 读循环投递结果。
type deliverResult int

const (
	deliverOK    deliverResult = iota // 投递成功，继续
	deliverFatal                      // 连接级协议错误：退出读循环并关闭连接
)

// dispatch 由 Conn 创建方注入：处理一帧（含新建流与启动处理器）。返回 deliverFatal
// 时读循环退出；sub 的所有权随调用转移（函数内负责 Release）。
type dispatch func(c *Conn, sid uint32, op protocol.OpCode, sub netpoll.Reader) deliverResult

// Conn 一个 netpoll 连接的传输封装（客户端/服务端共用）。
// 读循环是连接 Reader 的唯一消费者：Peek 帧头、Slice 整帧零拷贝子 Reader、按 streamID 分发。
// 写由 wmu 串行化（netpoll Flush 并发调用会返回 ErrConcurrentAccess），
// WriteBinary 引用调用方缓冲直到 Flush 排空，故 wmu 必须在 Flush 返回后才释放。
type Conn struct {
	c netpoll.Connection

	wmu sync.Mutex // 串行化连接写（WriteBinary+Malloc+Flush 原子段）

	mu      sync.Mutex
	streams map[uint32]*stream
	sid     atomic.Uint32 // 客户端流 ID 分配器

	closed    chan struct{}
	closeOnce sync.Once

	dispatch dispatch
}

// readLoop 连接读循环：帧协议 [len(4)][sid(4)][op(1)][payload]。
// Peek(4) 阻塞至长度字段就绪；Slice(4+len) 阻塞至整帧就绪并生成零拷贝子 Reader
// （Slice 同时推进并 Release 主 Reader）。退出时关闭所有流并关闭连接。
func (c *Conn) readLoop() {
	defer func() {
		c.closeAll()
		_ = c.c.Close()
	}()
	for {
		p4, err := c.c.Reader().Peek(4)
		if err != nil {
			return
		}
		lenField := binary.BigEndian.Uint32(p4)
		if lenField < protocol.FrameHeaderLen || lenField > protocol.MaxFrameTotal {
			return
		}
		sub, err := c.c.Reader().Slice(4 + int(lenField))
		if err != nil {
			return
		}

		if err := c.c.Reader().Release(); err != nil {
			_ = sub.Release()
			return
		}

		if err := sub.Skip(4); err != nil {
			_ = sub.Release()
			return
		}
		sidB, err := sub.Next(4)
		if err != nil {
			_ = sub.Release()
			return
		}
		op, err := sub.ReadByte()
		if err != nil {
			_ = sub.Release()
			return
		}
		if c.dispatch(c, binary.BigEndian.Uint32(sidB), protocol.OpCode(op), sub) == deliverFatal {
			return
		}
	}
}

// closeAll 关闭连接信号并解除所有流阻塞（幂等）。
func (c *Conn) closeAll() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.mu.Lock()
		for _, st := range c.streams {
			st.finish()
		}
		c.mu.Unlock()
	})
}

// newStream 创建并注册一个客户端流。
func (c *Conn) newStream() *stream {
	st := newStream(c.sid.Add(1))
	c.mu.Lock()
	c.streams[st.id] = st
	c.mu.Unlock()
	return st
}

// removeStream 注销流并解除读循环可能存在的阻塞投递。
func (c *Conn) removeStream(st *stream) {
	c.mu.Lock()
	delete(c.streams, st.id)
	c.mu.Unlock()
	st.finish()
}

// await 等待本流的下一帧，或连接/上下文终止。
func (c *Conn) await(ctx context.Context, st *stream) (frameMsg, error) {
	select {
	case msg := <-st.in:
		return msg, nil
	case <-st.done:
		return frameMsg{}, errConnClosed
	case <-c.closed:
		return frameMsg{}, errConnClosed
	case <-ctx.Done():
		return frameMsg{}, ctx.Err()
	}
}

// writeFrame 串行写一帧。payload > 4K 时 WriteBinary 零拷贝引用原缓冲，
// Flush 阻塞至输出排空（waitFlush），返回后调用方即可安全复用/归还 payload。
func (c *Conn) writeFrame(sid uint32, op protocol.OpCode, payload []byte) error {
	statTxFrames.Add(1)
	statTxBytes.Add(int64(len(payload)))
	if op == protocol.OpGetData || op == protocol.OpGetDataFinal {
		statTxDataFrames.Add(1)
		statTxDataBytes.Add(int64(len(payload)))
		if len(payload) == protocol.ChunkSize {
			statTxData4M.Add(1)
		}
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var hdr [4 + protocol.FrameHeaderLen]byte // len(4)+sid(4)+op(1)
	binary.BigEndian.PutUint32(hdr[0:4], uint32(protocol.FrameHeaderLen+len(payload)))
	binary.BigEndian.PutUint32(hdr[4:8], sid)
	hdr[8] = byte(op)
	if _, err := c.c.Writer().WriteBinary(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.c.Writer().WriteBinary(payload); err != nil {
			return err
		}
	}
	return c.c.Writer().Flush()
}
