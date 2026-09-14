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
	"io"

	"github.com/cloudwego/netpoll"

	"github.com/liucxer/taihu/pkg/taihu"
)

// chunkSize 单条数据帧负载上限（4MiB），与旧 gRPC 方案一致。
const chunkSize = 1 << 22 // 4MiB

// shmDataPad 共享内存数据帧的 4K 对齐 pad 长度：数据帧布局
// [5B 帧头][4091B pad][4K 对齐数据区]，数据区起始 4K 对齐供服务端
// O_DIRECT 直读共享内存（免 bufpool→共享内存 memcpy）。客户端读数据帧时跳过 pad。
const shmDataPad = 4096

// shmSliceSize 共享内存（shmipc）单切片数据容量：须容纳最大直读数据帧
// [shmDataPad pad][5B 帧头][≤chunkSize 负载][≤4095B 对齐读余量]
// ≈ 4MiB+8KiB；取 4K 对齐值（同时满足 arm64 约束）。
const shmSliceSize = chunkSize + 8*1024

// shm 直读在途切片不再设批上限：整请求单链方案（shmWriteDataFramesChain）把请求段内
// 全部整 4MiB 块一次收进同一条共享内存链并发直读，单请求在途 = 请求整块数，由共享内存
// 池容量（2GiB @95% ≈ 480 个大切片）动态约束——池耗尽时 Reserve 逐块失败即截断，回退
// 路径兜底续读，无需静态常量控制。

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

	// 管理类 op（admin RPC，首版仅 TCP 路径；shmipc 路径不实现，见 taihu-cli 设计文档 §4）：
	opPing     OpCode = 0x0C // payload: 空；响应 opPong: code(4) server_time_unix_nano(8)
	opPong     OpCode = 0x0D
	opMetaReq  OpCode = 0x0E // payload: keyLen(4) key（编码同 opStatReq）；响应 opMetaResp/opResp
	opMetaResp OpCode = 0x0F // payload: segID(8) off(8) size(8)（code==0 时）
	opSegReq   OpCode = 0x10 // payload: 空；流式响应 opSegSum + opSegData* + opSegEnd
	opSegSum   OpCode = 0x11 // payload: code(4) total(8) free(8) active(8) full(8) reclaiming(8) cursorSeg(8) cursorOff(8) segSize(8) objectCount(8)
	opSegData  OpCode = 0x12 // payload: count(4) + count × [segID(8) state(1) alive(8) reclaimSeq(8)]
	opSegEnd   OpCode = 0x13 // payload: 空（正常结束）
	opKeysReq  OpCode = 0x14 // payload: keyLen(4) prefix（可为空）；响应 opKeysData* + opResp(code)
	opKeysData OpCode = 0x15 // payload: count(4) + count × [keyLen(4) key]
)

// 导出的帧操作码（wire 协议常量，供 rpcclient/shmipc.go 等外部包复用；
// 内部 TCP 路径继续使用未导出短名，保持零改动）。
const (
	OpPutHeader    OpCode = opPutHeader
	OpPutData      OpCode = opPutData
	OpPutEnd       OpCode = opPutEnd
	OpResp         OpCode = opResp
	OpGetReq       OpCode = opGetReq
	OpGetData      OpCode = opGetData
	OpGetEnd       OpCode = opGetEnd
	OpGetErr       OpCode = opGetErr
	OpDelReq       OpCode = opDelReq
	OpStatReq      OpCode = opStatReq
	OpStatResp     OpCode = opStatResp
	OpGetDataFinal OpCode = opGetDataFinal

	OpPing    OpCode = opPing
	OpSegReq  OpCode = opSegReq
	OpMetaReq OpCode = opMetaReq
	OpKeysReq OpCode = opKeysReq
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

// byteReader 帧 payload 读取的最小接口。netpoll.Reader 天然满足（TCP 路径）；
// sliceReader 适配共享内存切片（shmipc 路径），使 parse* 纯函数在两传输下复用。
type byteReader interface {
	// Next 返回后续 size 字节（并消费），不足时返回错误。
	Next(size int) ([]byte, error)
	// ReadString 读取 size 字节并转为 string（并消费）。
	ReadString(size int) (string, error)
}

var _ byteReader = netpoll.Reader(nil)
var _ byteReader = (*sliceReader)(nil)

// sliceReader 基于 []byte 的 byteReader 适配，用于 shmipc BufferReader.ReadBytes 返回的共享内存切片。
// 语义与 netpoll.Reader.Next 一致：pos 前进、返回切片引用（零拷贝）。
type sliceReader struct {
	b   []byte
	pos int
}

// newSliceReader 构造切片读取器。
func newSliceReader(b []byte) *sliceReader {
	return &sliceReader{b: b}
}

// Len 返回未读字节数。
func (r *sliceReader) Len() int { return len(r.b) - r.pos }

// Next 返回后续 size 字节（零拷贝引用，不复制）。
func (r *sliceReader) Next(size int) ([]byte, error) {
	if r.pos+size > len(r.b) {
		return nil, io.ErrUnexpectedEOF
	}
	b := r.b[r.pos : r.pos+size]
	r.pos += size
	return b, nil
}

// ReadString 读取 size 字节并转为 string。
func (r *sliceReader) ReadString(size int) (string, error) {
	b, err := r.Next(size)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// readU32 从 byteReader 读 4 字节大端 uint32。
func readU32(r byteReader) (uint32, error) {
	b, err := r.Next(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

// readU64 从 byteReader 读 8 字节大端 uint64。
func readU64(r byteReader) (uint64, error) {
	b, err := r.Next(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

// parsePutHeader 解析 PutHeader payload。
func parsePutHeader(r byteReader) (key string, size int64, err error) {
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
func parseGetReq(r byteReader) (key string, off, size int64, err error) {
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
func parseKeyReq(r byteReader) (string, error) {
	kl, err := readU32(r)
	if err != nil {
		return "", err
	}
	if kl > maxKeyLen {
		return "", errors.New("taihu: key too long")
	}
	return r.ReadString(int(kl))
}

// --- admin RPC 编解码（taihu-cli 设计文档 §4）---

// SegmentEntry 单段状态明细（wire 上 state 占 1 字节）。
type SegmentEntry struct {
	SegmentID  int64
	State      uint8
	AliveCount int64
	ReclaimSeq int64
}

// SegmentSummary 实例段汇总与写游标。
type SegmentSummary struct {
	Total       int64
	Free        int64
	Active      int64
	Full        int64
	Reclaiming  int64
	CursorSeg   int64
	CursorOff   int64
	SegSize     int64
	ObjectCount int64
}

// encodePong 编码 Ping 响应 payload: code(4) server_time_unix_nano(8)。
func encodePong(code errCode, t int64) []byte {
	p := make([]byte, 12)
	binary.BigEndian.PutUint32(p[:4], uint32(code))
	binary.BigEndian.PutUint64(p[4:], uint64(t))
	return p
}

// parsePong 解析 Ping 响应 payload，返回 (server_time_unix_nano, error)。
func parsePong(r byteReader) (int64, error) {
	code, err := readU32(r)
	if err != nil {
		return 0, err
	}
	if errCode(code) != codeOK {
		return 0, mapCode(errCode(code))
	}
	t, err := readU64(r)
	if err != nil {
		return 0, err
	}
	return int64(t), nil
}

// encodeMetaResp 编码 Meta 响应 payload: segID(8) off(8) size(8)。
func encodeMetaResp(segID, off, size int64) []byte {
	p := make([]byte, 24)
	binary.BigEndian.PutUint64(p[0:], uint64(segID))
	binary.BigEndian.PutUint64(p[8:], uint64(off))
	binary.BigEndian.PutUint64(p[16:], uint64(size))
	return p
}

// parseMetaResp 解析 Meta 响应 payload。
func parseMetaResp(r byteReader) (segID, off, size int64, err error) {
	var n uint64
	if n, err = readU64(r); err != nil {
		return 0, 0, 0, err
	}
	segID = int64(n)
	if n, err = readU64(r); err != nil {
		return 0, 0, 0, err
	}
	off = int64(n)
	if n, err = readU64(r); err != nil {
		return 0, 0, 0, err
	}
	size = int64(n)
	return segID, off, size, nil
}

// encodeSegSum 编码段汇总帧 payload。
func encodeSegSum(code errCode, s SegmentSummary) []byte {
	p := make([]byte, 4+8*9)
	binary.BigEndian.PutUint32(p[:4], uint32(code))
	putI64 := func(b int, v int64) {
		binary.BigEndian.PutUint64(p[b:], uint64(v))
	}
	putI64(4, s.Total)
	putI64(12, s.Free)
	putI64(20, s.Active)
	putI64(28, s.Full)
	putI64(36, s.Reclaiming)
	putI64(44, s.CursorSeg)
	putI64(52, s.CursorOff)
	putI64(60, s.SegSize)
	putI64(68, s.ObjectCount)
	return p
}

// parseSegSum 解析段汇总帧 payload。
func parseSegSum(r byteReader) (SegmentSummary, error) {
	code, err := readU32(r)
	if err != nil {
		return SegmentSummary{}, err
	}
	if errCode(code) != codeOK {
		return SegmentSummary{}, mapCode(errCode(code))
	}
	var s SegmentSummary
	for _, v := range []*int64{&s.Total, &s.Free, &s.Active, &s.Full, &s.Reclaiming, &s.CursorSeg, &s.CursorOff, &s.SegSize, &s.ObjectCount} {
		n, err := readU64(r)
		if err != nil {
			return SegmentSummary{}, err
		}
		*v = int64(n)
	}
	return s, nil
}

// segItemLen 单条段明细在线字节数。
const segItemLen = 8 + 1 + 8 + 8

// encodeSegData 编码段明细帧 payload（单帧可容纳 ~16 万条，远大于 2048 段配额）。
func encodeSegData(entries []SegmentEntry) []byte {
	p := make([]byte, 4+len(entries)*segItemLen)
	binary.BigEndian.PutUint32(p[:4], uint32(len(entries)))
	for i, e := range entries {
		b := 4 + i*segItemLen
		binary.BigEndian.PutUint64(p[b:], uint64(e.SegmentID))
		p[b+8] = e.State
		binary.BigEndian.PutUint64(p[b+9:], uint64(e.AliveCount))
		binary.BigEndian.PutUint64(p[b+17:], uint64(e.ReclaimSeq))
	}
	return p
}

// parseSegData 解析段明细帧 payload，追加到 out。
func parseSegData(r byteReader, out []SegmentEntry) ([]SegmentEntry, error) {
	count, err := readU32(r)
	if err != nil {
		return out, err
	}
	for i := uint32(0); i < count; i++ {
		var (
			id, alive, seq int64
			n              uint64
			err            error
		)
		if n, err = readU64(r); err != nil {
			return out, err
		}
		id = int64(n)
		st, err := r.Next(1)
		if err != nil {
			return out, err
		}
		if n, err = readU64(r); err != nil {
			return out, err
		}
		alive = int64(n)
		if n, err = readU64(r); err != nil {
			return out, err
		}
		seq = int64(n)
		out = append(out, SegmentEntry{SegmentID: id, State: st[0], AliveCount: alive, ReclaimSeq: seq})
	}
	return out, nil
}

// encodeKeysData 编码 key 列表帧 payload: count(4) + count × [keyLen(4) key]。
func encodeKeysData(keys []string) []byte {
	size := 4
	for _, k := range keys {
		size += 4 + len(k)
	}
	p := make([]byte, size)
	binary.BigEndian.PutUint32(p[:4], uint32(len(keys)))
	pos := 4
	for _, k := range keys {
		binary.BigEndian.PutUint32(p[pos:], uint32(len(k)))
		pos += 4
		copy(p[pos:], k)
		pos += len(k)
	}
	return p
}

// parseKeysData 解析 key 列表帧 payload，追加到 out。
func parseKeysData(r byteReader, out []string) ([]string, error) {
	count, err := readU32(r)
	if err != nil {
		return out, err
	}
	for i := uint32(0); i < count; i++ {
		kl, err := readU32(r)
		if err != nil {
			return out, err
		}
		if kl > maxKeyLen {
			return out, errors.New("taihu: key too long")
		}
		k, err := r.ReadString(int(kl))
		if err != nil {
			return out, err
		}
		out = append(out, k)
	}
	return out, nil
}
