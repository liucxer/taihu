package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/netpoll"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/pkg/taihu"
)

// dialTimeout 客户端拨号超时。
const dialTimeout = 10 * time.Second

// init 使 netpoll 接收缓冲改用 bufpool 对齐分配：收流帧载荷落在单个对齐节点内时，
// 客户端 Get 可直接移交该缓冲给调用方（零拷贝），并经由 bufpool.Put 安全归还。
func init() {
	netpoll.SetAlignedAllocator(bufpool.Get, bufpool.Put)
}

// streamInCap 每流投递缓冲上限：读循环背压到流处理器消费速度。
const streamInCap = 8

var errConnClosed = errors.New("taihu: connection closed")

// frameMsg 读循环投递给流处理器的一帧：op 已解析，r 为零拷贝子 Reader（定位在
// payload 起点，Len() 即负载长度；无负载帧 Len()==0）。用毕必须 r.Release()。
type frameMsg struct {
	op OpCode
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
type dispatch func(c *Conn, sid uint32, op OpCode, sub netpoll.Reader) deliverResult

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
		if lenField < frameHeaderLen || lenField > maxFrameTotal {
			return // 协议错误：帧长越界
		}
		sub, err := c.c.Reader().Slice(4 + int(lenField))
		if err != nil {
			return
		}
		// Slice 覆盖整帧（含 4 字节长度前缀），先跳过长度字段，子 Reader 定位到 sid。
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
		if c.dispatch(c, binary.BigEndian.Uint32(sidB), OpCode(op), sub) == deliverFatal {
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
func (c *Conn) writeFrame(sid uint32, op OpCode, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var hdr [4 + frameHeaderLen]byte // len(4)+sid(4)+op(1)
	binary.BigEndian.PutUint32(hdr[0:4], uint32(frameHeaderLen+len(payload)))
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

// clientDispatch 客户端侧分发：按 streamID 路由到对应流；未知流/已结束流
// （RPC 完成或取消）直接丢弃该帧，不中断连接。
func clientDispatch(c *Conn, sid uint32, op OpCode, sub netpoll.Reader) deliverResult {
	c.mu.Lock()
	st := c.streams[sid]
	c.mu.Unlock()
	if st == nil {
		_ = sub.Release()
		return deliverOK // 迟到帧：丢弃
	}
	select {
	case st.in <- frameMsg{op, sub}:
		return deliverOK
	case <-st.done:
		_ = sub.Release()
		return deliverOK // RPC 已结束：丢弃
	case <-c.closed:
		_ = sub.Release()
		return deliverFatal
	}
}

// Put 上传对象（与本地 taihu.Storage.Put 同语义）。首帧发 key+size，随后按
// chunkSize 分帧零拷贝发送（in 在返回前不会被引用），opPutEnd 后等待 opResp。
func (c *Conn) Put(ctx context.Context, key string, size int64, in []byte) error {
	if int64(len(in)) < size {
		return taihu.ErrShortWrite
	}
	st := c.newStream()
	defer c.removeStream(st)
	if err := c.writeFrame(st.id, opPutHeader, encodePutHeader(key, size)); err != nil {
		return err
	}
	var off int64
	for off < size {
		end := off + chunkSize
		if end > size {
			end = size
		}
		if err := c.writeFrame(st.id, opPutData, in[off:end]); err != nil {
			return err
		}
		off = end
	}
	if err := c.writeFrame(st.id, opPutEnd, nil); err != nil {
		return err
	}
	msg, err := c.await(ctx, st)
	if err != nil {
		return err
	}
	defer msg.r.Release()
	if msg.op != opResp {
		return fmt.Errorf("taihu: unexpected put response op %d", msg.op)
	}
	code, err := readU32(msg.r)
	if err != nil {
		return err
	}
	return mapCode(errCode(code))
}

// Get 读取对象内 [off, off+size) 子区间并返回整块数据（size=-1 读至结尾）。
//
// 返回 (data, release, err)：data len==size 为本次调用私有缓冲（4K 对齐 bufpool
// 缓冲）；调用方用毕必须调用 release()（幂等）归还。收流帧逐帧汇入对齐缓冲，全程
// 恰一次用户态拷贝（header 消耗 + 帧数据逐段 copy），正确性不依赖 netpoll 节点复用。
//
// 说明：曾尝试“单帧零拷贝移交 netpoll 收节点”给调用方，但客户端诊断 Reader 会把
// 该节点当作活跃写节点复用（bookAck 对同一 backing array 追加后续帧），Take 归还到
// bufpool 后会被再次写入，构成内存生命周期错乱（-race 实证）。按计划风险回退条款，
// 此路径收敛为 1 次对齐汇入拷贝，与本地 ReadAt 语义一致。
func (c *Conn) Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error) {
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
	st := c.newStream()
	defer c.removeStream(st)
	if err := c.writeFrame(st.id, opGetReq, encodeGetReq(key, off, size)); err != nil {
		return nil, nil, err
	}

	var (
		pos      int64
		buf      []byte // 汇集缓冲（对齐池，out = buf[:size]）
		out      []byte // 返回缓冲
		disposed bool
	)
	// dispose 幂等归还最终占用的缓冲。
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
		case opGetData:
			rem := int64(msg.r.Len())
			if rem > size-pos {
				msg.r.Release()
				dispose()
				return nil, nil, fmt.Errorf("taihu: get stream exceeds requested size")
			}
			if buf == nil {
				buf = bufpool.Get(int(size))
				out = buf[:size]
			}
			// 直写调用方缓冲：多节点帧由 ReadCopy 一次拷入，消除 Next 的中间搬移；
			// 断言失败（如 race 变体未实现）回退现有 Next+copy 路径。
			if rc, ok := msg.r.(interface{ ReadCopy([]byte) (int, error) }); ok {
				n, err := rc.ReadCopy(out[pos : pos+int64(rem)])
				if err != nil {
					msg.r.Release()
					dispose()
					return nil, nil, err
				}
				pos += int64(n)
			} else {
				p, err := msg.r.Next(int(rem))
				if err != nil {
					msg.r.Release()
					dispose()
					return nil, nil, err
				}
				pos += int64(copy(out[pos:], p))
			}
			msg.r.Release()
		case opGetEnd:
			msg.r.Release()
			if pos != size {
				dispose()
				return nil, nil, fmt.Errorf("taihu: get short read: got %d want %d", pos, size)
			}
			return out, dispose, nil
		case opGetErr:
			code, err := readU32(msg.r)
			msg.r.Release()
			if err != nil {
				dispose()
				return nil, nil, err
			}
			dispose()
			return nil, nil, mapCode(errCode(code))
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
	if err := c.writeFrame(st.id, opDelReq, encodeKeyReq(key)); err != nil {
		return err
	}
	msg, err := c.await(ctx, st)
	if err != nil {
		return err
	}
	defer msg.r.Release()
	if msg.op != opResp {
		return fmt.Errorf("taihu: unexpected delete response op %d", msg.op)
	}
	code, err := readU32(msg.r)
	if err != nil {
		return err
	}
	return mapCode(errCode(code))
}

// Stat 返回对象逻辑大小（key 不存在返回 ErrNotFound）。
func (c *Conn) Stat(ctx context.Context, key string) (int64, error) {
	st := c.newStream()
	defer c.removeStream(st)
	if err := c.writeFrame(st.id, opStatReq, encodeKeyReq(key)); err != nil {
		return 0, err
	}
	msg, err := c.await(ctx, st)
	if err != nil {
		return 0, err
	}
	defer msg.r.Release()
	switch msg.op {
	case opStatResp:
		sz, err := readU64(msg.r)
		if err != nil {
			return 0, err
		}
		return int64(sz), nil
	case opResp:
		code, err := readU32(msg.r)
		if err != nil {
			return 0, err
		}
		return 0, mapCode(errCode(code))
	default:
		return 0, fmt.Errorf("taihu: unexpected stat response op %d", msg.op)
	}
}
