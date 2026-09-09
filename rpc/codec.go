package rpc

import (
	"fmt"
	"sync/atomic"

	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/proto"

	"github.com/liucxer/taihu/internal/bufpool"
)

// RawData 声明一段以原始字节发送的数据（协议优化：读写路径去拷贝）。
// Data 是发送视图；Orig 是 bufpool.Get 原样返回的切片，发送完成后由 gRPC 调用
// 底层 Buffer 的 Free 归还池；非池缓冲传 nil（由调用方自行管理生命周期）。
type RawData struct {
	Data []byte
	Orig []byte
}

// RawFrame 接收端延迟物化数据帧：持有单个 mem.Buffer（Unmarshal 时由 wire 缓冲
// 合并得到——单缓冲零拷贝 Ref、多缓冲 bufpool 复用合并 1 次拷贝无 GC），
// 消费时单次拷贝到目标缓冲（CopyTo）或零拷贝引用底层数据（Data），
// 用毕须 Free 归还引用。相比直接物化 []byte，省去每帧 1MiB 的堆分配 + 清零 +
// 整块拷贝（客户端读路径 CPU 大头）。
type RawFrame struct {
	buf mem.Buffer
	len int
	off int
}

// Len 返回帧总字节数。
func (f *RawFrame) Len() int { return f.len }

// Remaining 返回未消费字节数。
func (f *RawFrame) Remaining() int { return f.len - f.off }

// Data 返回剩余数据的连续视图（零拷贝引用底层缓冲）。
// 调用方须在下次 Free/Next 前使用。
func (f *RawFrame) Data() []byte {
	if f.buf == nil {
		return nil
	}
	return f.buf.ReadOnlyData()[f.off:]
}

// CopyTo 从当前偏移拷贝至多 len(dst) 字节到 dst，并推进偏移。
func (f *RawFrame) CopyTo(dst []byte) int {
	if f.buf == nil {
		return 0
	}
	n := copy(dst, f.buf.ReadOnlyData()[f.off:])
	f.off += n
	return n
}

// Free 释放引用（归还 gRPC wire 缓冲池 / bufpool）。调用后不得再使用。
func (f *RawFrame) Free() {
	if f.buf != nil {
		f.buf.Free()
		f.buf = nil
	}
	f.len, f.off = 0, 0
}

// bufpoolAdapter 把 taihu bufpool 适配为 mem.BufferPool（合并 wire 多缓冲用）。
type bufpoolAdapter struct{}

func (bufpoolAdapter) Get(length int) *[]byte {
	b := bufpool.Get(length)
	return &b
}

func (bufpoolAdapter) Put(b *[]byte) {
	if b != nil {
		bufpool.Put(*b)
	}
}

// materializeFrame 把 wire 缓冲切片合并为单缓冲：
// 单缓冲零拷贝 Ref（计数 +1，recv() 的 Free 释放原始引用后本帧仍持有）；
// 多缓冲（gRPC 按 16KB 帧粒度累积）用 bufpool 复用缓冲合并，1 次拷贝且无 GC 分配。
// 返回 nil + 0 表示空帧。
func materializeFrame(data mem.BufferSlice) (mem.Buffer, int) {
	l := data.Len()
	if l == 0 {
		return nil, 0
	}
	if len(data) == 1 {
		data[0].Ref()
		return data[0], l
	}
	buf := bufpool.Get(l)
	data.CopyTo(buf)
	buf = buf[:l] // reslice 到实际长度；bufpool 按向上取 2 幂分桶，归还回同桶
	return mem.NewBuffer(&buf, bufpoolAdapter{}), l
}

// RawCodecName 是 RawCodec 的注册名（gRPC content-subtype）。
const RawCodecName = "taihu-raw"

// RawCodec 数据帧零拷贝透传编解码器。
//
//   - []byte / RawData 消息：Marshal 直接引用原缓冲（0 拷贝），绕开 proto.Marshal 的
//     一次整块 memmove；Unmarshal 到 *[]byte 时拷贝（wire 缓冲由 gRPC 池管理，RecvMsg
//     返回后即归还，不能引用）。
//   - 其余 proto 消息（PutHeader/GetReq/PutResp 等）：委托标准 proto 编解码。
//
// wire 兼容性：Get/Put 数据帧为裸字节（非 GetChunk/PutChunk 的 message 编码），
// 客户端与服务端必须同步升级，与旧版本不兼容。
type RawCodec struct{}

func init() { encoding.RegisterCodecV2(RawCodec{}) }

func (RawCodec) Name() string { return RawCodecName }

func (RawCodec) Marshal(v any) (mem.BufferSlice, error) {
	switch m := v.(type) {
	case RawData:
		return mem.BufferSlice{newReclaimBuf(m.Data, m.Orig)}, nil
	case *RawData:
		if m == nil {
			return nil, fmt.Errorf("taihu-raw codec: nil *RawData")
		}
		return mem.BufferSlice{newReclaimBuf(m.Data, m.Orig)}, nil
	case []byte:
		// 调用方承诺发送期间不修改/不复用缓冲。
		return mem.BufferSlice{mem.SliceBuffer(m)}, nil
	}
	pm, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("taihu-raw codec: unsupported message type %T", v)
	}
	data, err := proto.Marshal(pm)
	if err != nil {
		return nil, err
	}
	return mem.BufferSlice{mem.SliceBuffer(data)}, nil
}

func (RawCodec) Unmarshal(data mem.BufferSlice, v any) error {
	switch t := v.(type) {
	case *[]byte:
		// 必须拷贝：wire 缓冲由 gRPC 池管理，RecvMsg 返回后即归还。
		*t = data.Materialize()
		return nil
	case *RawFrame:
		// 延迟物化到单缓冲（单缓冲 Ref 零拷贝 / 多缓冲 bufpool 复用合并），
		// 消费完成后由 RawFrame.Free 归还。
		buf, l := materializeFrame(data)
		t.buf = buf
		t.len = l
		t.off = 0
		return nil
	}
	pm, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("taihu-raw codec: unsupported message type %T", v)
	}
	if data.Len() == 0 {
		return nil
	}
	return proto.Unmarshal(data.Materialize(), pm)
}

// reclaimBuf 包装 bufpool 缓冲实现 mem.Buffer（零拷贝引用 + 写后归还）。
//
// gRPC 生命周期约定：Marshal 返回后 SendMsg 的 defer 会调用一次 data.Free()，
// 传输层 data.Reader() 会 Ref() 一次并在数据写完后逐块 Free()。因此起始引用计数为 1，
// 计数归零（发送方引用 + 传输层引用都释放）时才归还 bufpool —— 与 gRPC 内置
// buffer 的引用计数语义一致，确保缓冲在异步写出完成前不会被复用。
type reclaimBuf struct {
	mem.Buffer        // 嵌入以提供未导出的 split/read 方法
	orig       []byte // bufpool.Get 原样返回的切片；nil 表示非池缓冲
	refs       atomic.Int32
}

func newReclaimBuf(data, orig []byte) *reclaimBuf {
	b := &reclaimBuf{Buffer: mem.SliceBuffer(data), orig: orig}
	b.refs.Store(1)
	return b
}

func (b *reclaimBuf) Ref() { b.refs.Add(1) }

func (b *reclaimBuf) Free() {
	if b.refs.Add(-1) == 0 && b.orig != nil {
		bufpool.Put(b.orig)
	}
}
