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
	mem.Buffer // 嵌入以提供未导出的 split/read 方法
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
