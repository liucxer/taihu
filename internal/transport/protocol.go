// Package transport 实现 taihu 的 netpoll 传输层（设计文档_v3 的远程访问层改造，
// 以 netpoll + LinkBuffer 取代 gRPC/HTTP-2）。协议为自定义帧流式多路复用：
//
//	帧格式: [4B len][4B streamID][1B op][payload...]
//	  len = 4 + 1 + len(payload)（len 字段之后的字节数），大端。
//	  streamID 用于连接内多路复用（每次 RPC 独占一个 stream）。
//	  payload 上限 = chunkSize(4MiB)，帧总长上限 = 9 + chunkSize。
//
// 零拷贝路径：
//   - 读：连接读循环 Peek 帧头、Slice 整帧（阻塞至就绪，Slice 生成零拷贝子 Reader），
//     按 streamID 分发给流处理器；流处理器用 Read 把负载直接拷入目标缓冲（一次拷贝）。
//   - 写：WriteBinary 对 >4K 负载零拷贝引用原缓冲，sendmsg(writev) 散射写出，
//     Flush 阻塞至输出缓冲排空（waitFlush），保证引用缓冲在返回后可安全复用。
package transport

import (
	"encoding/binary"
	"errors"

	"github.com/cloudwego/netpoll"

	"github.com/liucxer/taihu/pkg/taihu"
)

// chunkSize 单条数据帧负载上限（4MiB），与旧 gRPC 方案一致。
const chunkSize = 1 << 22 // 4MiB

// maxKeyLen 请求中 key 的最大长度，防止畸形长度字段放大内存。
const maxKeyLen = 1 << 16

// frameHeaderLen = streamID(4) + op(1)。
const frameHeaderLen = 5

// maxFrameTotal 帧负载上限 = frameHeaderLen + chunkSize。
const maxFrameTotal = frameHeaderLen + chunkSize

// inputNodeSize netpoll 收流节点容量上限：钳到单帧线上总长
// （4B 长度前缀 + frameHeaderLen + chunkSize）。节点容量==整帧线上大小，
// 读满一帧后 book 的剩余容量为 0，节点不再被复用（一帧一节点），
// 客户端 Get 可经 TakeTry 零拷贝移交该节点缓冲给调用方。
const inputNodeSize = 4 + maxFrameTotal

// OpCode 帧操作码。
type OpCode byte

const (
	opPutHeader OpCode = 0x01 // payload: keyLen(4) key size(8)
	opPutData   OpCode = 0x02 // payload: 原始数据
	opPutEnd    OpCode = 0x03 // payload: 无（客户端结束 Put 流，服务端回 opResp）
	opResp      OpCode = 0x04 // payload: code(4)（Put/Delete/Stat 错误响应）
	opGetReq    OpCode = 0x05 // payload: keyLen(4) key off(8) size(8)，size=-1 读至结尾
	opGetData   OpCode = 0x06 // payload: 原始数据
	opGetEnd    OpCode = 0x07 // payload: 无（旧版服务端 Get 正常结束帧，已弃用不再发送，保留常量兼容解析）
	opGetErr    OpCode = 0x08 // payload: code(4)（Get 错误，流结束）
	opDelReq    OpCode = 0x09 // payload: keyLen(4) key
	opStatReq   OpCode = 0x0A // payload: keyLen(4) key
	opStatResp  OpCode = 0x0B // payload: size(8)

	// opGetDataFinal 最后一个数据帧（opGetData|0x80）：服务端 Get 流以数据帧
	// 收尾而非 opGetEnd 空帧，客户端收齐 size 字节（或短读校验）后即结束，
	// 每请求省一个帧与一次写/读 syscall。帧头/负载格式与 opGetData 完全一致。
	opGetDataFinal OpCode = opGetData | 0x80
)

// errCode 错误码（wire 上 4 字节大端），与库错误一一映射。
type errCode uint32

const (
	codeOK              errCode = 0
	codeNotFound        errCode = 1
	codeInvalidRange    errCode = 2
	codeTooLarge        errCode = 3
	codeNoSpace         errCode = 4
	codeInternal        errCode = 5
	codeInvalidArgument errCode = 6
)

// mapStorageErr 将库错误映射为错误码（对应旧 gRPC status 映射）。
func mapStorageErr(err error) errCode {
	switch {
	case errors.Is(err, taihu.ErrNotFound):
		return codeNotFound
	case errors.Is(err, taihu.ErrInvalidRange):
		return codeInvalidRange
	case errors.Is(err, taihu.ErrTooLarge):
		return codeTooLarge
	case errors.Is(err, taihu.ErrNoSpace):
		return codeNoSpace
	default:
		return codeInternal
	}
}

// mapCode 将错误码还原为库错误（对应旧 gRPC status 还原）。
func mapCode(c errCode) error {
	switch c {
	case codeOK:
		return nil
	case codeNotFound:
		return taihu.ErrNotFound
	case codeInvalidRange:
		return taihu.ErrInvalidRange
	case codeTooLarge:
		return taihu.ErrTooLarge
	case codeNoSpace:
		return taihu.ErrNoSpace
	case codeInvalidArgument:
		return errors.New("taihu: invalid argument")
	default:
		return errors.New("taihu: rpc error")
	}
}

// encCode 编码错误码为 4 字节大端 payload。
func encCode(c errCode) []byte {
	p := make([]byte, 4)
	binary.BigEndian.PutUint32(p, uint32(c))
	return p
}

// encodePutHeader 编码 PutHeader payload: keyLen(4) key size(8)。
func encodePutHeader(key string, size int64) []byte {
	p := make([]byte, 4+len(key)+8)
	binary.BigEndian.PutUint32(p[:4], uint32(len(key)))
	copy(p[4:4+len(key)], key)
	binary.BigEndian.PutUint64(p[4+len(key):], uint64(size))
	return p
}

// encodeGetReq 编码 GetReq payload: keyLen(4) key off(8) size(8)。
func encodeGetReq(key string, off, size int64) []byte {
	p := make([]byte, 4+len(key)+16)
	binary.BigEndian.PutUint32(p[:4], uint32(len(key)))
	copy(p[4:4+len(key)], key)
	binary.BigEndian.PutUint64(p[4+len(key):], uint64(off))
	binary.BigEndian.PutUint64(p[4+len(key)+8:], uint64(size))
	return p
}

// encodeKeyReq 编码 key 请求 payload: keyLen(4) key。
func encodeKeyReq(key string) []byte {
	p := make([]byte, 4+len(key))
	binary.BigEndian.PutUint32(p[:4], uint32(len(key)))
	copy(p[4:], key)
	return p
}

// readU32 从 Reader 读 4 字节大端 uint32。
func readU32(r netpoll.Reader) (uint32, error) {
	b, err := r.Next(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

// readU64 从 Reader 读 8 字节大端 uint64。
func readU64(r netpoll.Reader) (uint64, error) {
	b, err := r.Next(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

// parsePutHeader 解析 PutHeader payload。
func parsePutHeader(r netpoll.Reader) (key string, size int64, err error) {
	kl, err := readU32(r)
	if err != nil {
		return "", 0, err
	}
	if kl > maxKeyLen {
		return "", 0, errors.New("taihu: key too long")
	}
	key, err = r.ReadString(int(kl))
	if err != nil {
		return "", 0, err
	}
	sz, err := readU64(r)
	if err != nil {
		return "", 0, err
	}
	return key, int64(sz), nil
}

// parseGetReq 解析 GetReq payload。
func parseGetReq(r netpoll.Reader) (key string, off, size int64, err error) {
	kl, err := readU32(r)
	if err != nil {
		return "", 0, 0, err
	}
	if kl > maxKeyLen {
		return "", 0, 0, errors.New("taihu: key too long")
	}
	key, err = r.ReadString(int(kl))
	if err != nil {
		return "", 0, 0, err
	}
	o, err := readU64(r)
	if err != nil {
		return "", 0, 0, err
	}
	s, err := readU64(r)
	if err != nil {
		return "", 0, 0, err
	}
	return key, int64(o), int64(s), nil
}

// parseKeyReq 解析 key 请求 payload（Delete/Stat）。
func parseKeyReq(r netpoll.Reader) (string, error) {
	kl, err := readU32(r)
	if err != nil {
		return "", err
	}
	if kl > maxKeyLen {
		return "", errors.New("taihu: key too long")
	}
	return r.ReadString(int(kl))
}
