// Package protocol 实现 taihu 传输层的线路协议编解码（纯函数、无 I/O、无全局状态）。
//
// 帧格式: [4B len][4B streamID][1B op][payload...]
//
//	len = 4 + 1 + len(payload)（len 字段之后的字节数），大端。
//	streamID 用于连接内多路复用（每次 RPC 独占一个 stream）。
//	payload 上限 = ChunkSize(4MiB)，帧总长上限 = 9 + ChunkSize。
//
// 本包被两条数据面共用：netpoll TCP 路径（*netpoll.Reader）与 shmipc 共享内存路径
// （*SliceReader）。两条路径的读侧抽象为 ByteReader，故 Parse* 系列可在两者间复用。
//
// 编码约定（wire 上均为大端）：
//
//	错误码  4B（ErrCode）
//	keyLen  4B，key 3 段定长字段 off/size 等均为 8B
//	段明细  [segID(8) state(1) alive(8) reclaimSeq(8)]，见 SegItemLen
package protocol

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/liucxer/taihu/third_party/netpoll"

	"github.com/liucxer/taihu/internal/storage"
)

// ChunkSize 单条数据帧负载上限（4MiB），与旧 gRPC 方案一致。
const ChunkSize = 1 << 22 // 4MiB

// ShmDataPad 共享内存数据帧的 4K 对齐 pad 长度：数据帧布局
// [5B 帧头][4091B pad][4K 对齐数据区]，数据区起始 4K 对齐供服务端
// O_DIRECT 直读共享内存（免 bufpool→共享内存 memcpy）。客户端读数据帧时跳过 pad。
const ShmDataPad = 4096

// ShmSliceSize 共享内存（shmipc）单切片数据容量：须容纳最大直读数据帧
// [ShmDataPad pad][5B 帧头][≤ChunkSize 负载][≤4095B 对齐读余量]
// ≈ 4MiB+8KiB；取 4K 对齐值（同时满足 arm64 约束）。
const ShmSliceSize = ChunkSize + 8*1024

// shm 直读在途切片不再设批上限：整请求单链方案（shmWriteDataFramesChain）把请求段内
// 全部整 4MiB 块一次收进同一条共享内存链并发直读，单请求在途 = 请求整块数，由共享内存
// 池容量（2GiB @95% ≈ 480 个大切片）动态约束——池耗尽时 Reserve 逐块失败即截断，回退
// 路径兜底续读，无需静态常量控制。

// MaxKeyLen 请求中 key 的最大长度，防止畸形长度字段放大内存。
const MaxKeyLen = 1 << 16

// FrameHeaderLen = streamID(4) + op(1)。
const FrameHeaderLen = 5

// MaxFrameTotal 帧负载上限 = FrameHeaderLen + ChunkSize。
const MaxFrameTotal = FrameHeaderLen + ChunkSize

// InputNodeSize netpoll 收流节点容量上限：钳到单帧线上总长
// （4B 长度前缀 + FrameHeaderLen + ChunkSize）。节点容量==整帧线上大小，
// 读满一帧后 book 的剩余容量为 0，节点不再被复用（一帧一节点），
// 客户端 Get 可经 TakeTry 零拷贝移交该节点缓冲给调用方。
const InputNodeSize = 4 + MaxFrameTotal

// SegItemLen 单条段明细在线字节数。
const SegItemLen = 8 + 1 + 8 + 8

// OpCode 帧操作码。
type OpCode byte

const (
	OpPutHeader OpCode = 0x01 // payload: keyLen(4) key size(8)
	OpPutData   OpCode = 0x02 // payload: 原始数据
	OpPutEnd    OpCode = 0x03 // payload: 无（客户端结束 Put 流，服务端回 opResp）
	OpResp      OpCode = 0x04 // payload: code(4)（Put/Delete/Stat 错误响应）
	OpGetReq    OpCode = 0x05 // payload: keyLen(4) key off(8) size(8)，size=-1 读至结尾
	OpGetData   OpCode = 0x06 // payload: 原始数据
	OpGetEnd    OpCode = 0x07 // payload: 无（旧版服务端 Get 正常结束帧，已弃用不再发送，保留常量兼容解析）
	OpGetErr    OpCode = 0x08 // payload: code(4)（Get 错误，流结束）
	OpDelReq    OpCode = 0x09 // payload: keyLen(4) key
	OpStatReq   OpCode = 0x0A // payload: keyLen(4) key
	OpStatResp  OpCode = 0x0B // payload: size(8)

	// OpGetDataFinal 最后一个数据帧（OpGetData|0x80）：服务端 Get 流以数据帧
	// 收尾而非 OpGetEnd 空帧，客户端收齐 size 字节（或短读校验）后即结束，
	// 每请求省一个帧与一次写/读 syscall。帧头/负载格式与 OpGetData 完全一致。
	OpGetDataFinal OpCode = OpGetData | 0x80

	// 管理类 op（admin RPC，首版仅 TCP 路径；shmipc 路径不实现，见 taihu-cli 设计文档 §4）：
	OpPing     OpCode = 0x0C // payload: 空；响应 OpPong: code(4) server_time_unix_nano(8)
	OpPong     OpCode = 0x0D
	OpMetaReq  OpCode = 0x0E // payload: keyLen(4) key（编码同 OpStatReq）；响应 OpMetaResp/OpResp
	OpMetaResp OpCode = 0x0F // payload: segID(8) off(8) size(8)（code==0 时）
	OpSegReq   OpCode = 0x10 // payload: 空；流式响应 OpSegSum + OpSegData* + OpSegEnd
	OpSegSum   OpCode = 0x11 // payload: code(4) total(8) free(8) active(8) full(8) reclaiming(8) cursorSeg(8) cursorOff(8) segSize(8) objectCount(8)
	OpSegData  OpCode = 0x12 // payload: count(4) + count × [segID(8) state(1) alive(8) reclaimSeq(8)]
	OpSegEnd   OpCode = 0x13 // payload: 空（正常结束）
	OpKeysReq  OpCode = 0x14 // payload: keyLen(4) prefix（可为空）；响应 OpKeysData* + OpResp(code)
	OpKeysData OpCode = 0x15 // payload: count(4) + count × [keyLen(4) key]
)

// ErrCode 错误码（wire 上 4 字节大端），与库错误一一映射。
type ErrCode uint32

const (
	CodeOK              ErrCode = 0
	CodeNotFound        ErrCode = 1
	CodeInvalidRange    ErrCode = 2
	CodeTooLarge        ErrCode = 3
	CodeNoSpace         ErrCode = 4
	CodeInternal        ErrCode = 5
	CodeInvalidArgument ErrCode = 6
)

// MapStorageErr 将库错误映射为错误码（对应旧 gRPC status 映射）。
func MapStorageErr(err error) ErrCode {
	switch {
	case errors.Is(err, taihu.ErrNotFound):
		return CodeNotFound
	case errors.Is(err, taihu.ErrInvalidRange):
		return CodeInvalidRange
	case errors.Is(err, taihu.ErrTooLarge):
		return CodeTooLarge
	case errors.Is(err, taihu.ErrNoSpace):
		return CodeNoSpace
	default:
		return CodeInternal
	}
}

// MapCode 将错误码还原为库错误（对应旧 gRPC status 还原）。
func MapCode(c ErrCode) error {
	switch c {
	case CodeOK:
		return nil
	case CodeNotFound:
		return taihu.ErrNotFound
	case CodeInvalidRange:
		return taihu.ErrInvalidRange
	case CodeTooLarge:
		return taihu.ErrTooLarge
	case CodeNoSpace:
		return taihu.ErrNoSpace
	case CodeInvalidArgument:
		return errors.New("taihu: invalid argument")
	default:
		return errors.New("taihu: rpc error")
	}
}

// EncCode 编码错误码为 4 字节大端 payload。
func EncCode(c ErrCode) []byte {
	p := make([]byte, 4)
	binary.BigEndian.PutUint32(p, uint32(c))
	return p
}

// EncodePutHeader 编码 PutHeader payload: keyLen(4) key size(8)。
func EncodePutHeader(key string, size int64) []byte {
	p := make([]byte, 4+len(key)+8)
	binary.BigEndian.PutUint32(p[:4], uint32(len(key)))
	copy(p[4:4+len(key)], key)
	binary.BigEndian.PutUint64(p[4+len(key):], uint64(size))
	return p
}

// EncodeGetReq 编码 GetReq payload: keyLen(4) key off(8) size(8)。
func EncodeGetReq(key string, off, size int64) []byte {
	p := make([]byte, 4+len(key)+16)
	binary.BigEndian.PutUint32(p[:4], uint32(len(key)))
	copy(p[4:4+len(key)], key)
	binary.BigEndian.PutUint64(p[4+len(key):], uint64(off))
	binary.BigEndian.PutUint64(p[4+len(key)+8:], uint64(size))
	return p
}

// EncodeKeyReq 编码 key 请求 payload: keyLen(4) key。
func EncodeKeyReq(key string) []byte {
	p := make([]byte, 4+len(key))
	binary.BigEndian.PutUint32(p[:4], uint32(len(key)))
	copy(p[4:], key)
	return p
}

// ByteReader 帧 payload 读取的最小接口。netpoll.Reader 天然满足（TCP 路径）；
// SliceReader 适配共享内存切片（shmipc 路径），使 Parse* 纯函数在两传输下复用。
type ByteReader interface {
	// Next 返回后续 size 字节（并消费），不足时返回错误。
	Next(size int) ([]byte, error)
	// ReadString 读取 size 字节并转为 string（并消费）。
	ReadString(size int) (string, error)
}

var _ ByteReader = netpoll.Reader(nil)
var _ ByteReader = (*SliceReader)(nil)

// SliceReader 基于 []byte 的 ByteReader 适配，用于 shmipc BufferReader.ReadBytes 返回的共享内存切片。
// 语义与 netpoll.Reader.Next 一致：pos 前进、返回切片引用（零拷贝）。
type SliceReader struct {
	b   []byte
	pos int
}

// NewSliceReader 构造切片读取器。
func NewSliceReader(b []byte) *SliceReader {
	return &SliceReader{b: b}
}

// Len 返回未读字节数。
func (r *SliceReader) Len() int { return len(r.b) - r.pos }

// Next 返回后续 size 字节（零拷贝引用，不复制）。
func (r *SliceReader) Next(size int) ([]byte, error) {
	if r.pos+size > len(r.b) {
		return nil, io.ErrUnexpectedEOF
	}
	b := r.b[r.pos : r.pos+size]
	r.pos += size
	return b, nil
}

// ReadString 读取 size 字节并转为 string。
func (r *SliceReader) ReadString(size int) (string, error) {
	b, err := r.Next(size)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadU32 从 ByteReader 读 4 字节大端 uint32。
func ReadU32(r ByteReader) (uint32, error) {
	b, err := r.Next(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

// ReadU64 从 ByteReader 读 8 字节大端 uint64。
func ReadU64(r ByteReader) (uint64, error) {
	b, err := r.Next(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

// ParsePutHeader 解析 PutHeader payload。
func ParsePutHeader(r ByteReader) (key string, size int64, err error) {
	kl, err := ReadU32(r)
	if err != nil {
		return "", 0, err
	}
	if kl > MaxKeyLen {
		return "", 0, errors.New("taihu: key too long")
	}
	key, err = r.ReadString(int(kl))
	if err != nil {
		return "", 0, err
	}
	sz, err := ReadU64(r)
	if err != nil {
		return "", 0, err
	}
	return key, int64(sz), nil
}

// ParseGetReq 解析 GetReq payload。
func ParseGetReq(r ByteReader) (key string, off, size int64, err error) {
	kl, err := ReadU32(r)
	if err != nil {
		return "", 0, 0, err
	}
	if kl > MaxKeyLen {
		return "", 0, 0, errors.New("taihu: key too long")
	}
	key, err = r.ReadString(int(kl))
	if err != nil {
		return "", 0, 0, err
	}
	o, err := ReadU64(r)
	if err != nil {
		return "", 0, 0, err
	}
	s, err := ReadU64(r)
	if err != nil {
		return "", 0, 0, err
	}
	return key, int64(o), int64(s), nil
}

// ParseKeyReq 解析 key 请求 payload（Delete/Stat）。
func ParseKeyReq(r ByteReader) (string, error) {
	kl, err := ReadU32(r)
	if err != nil {
		return "", err
	}
	if kl > MaxKeyLen {
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

// EncodePong 编码 Ping 响应 payload: code(4) server_time_unix_nano(8)。
func EncodePong(code ErrCode, t int64) []byte {
	p := make([]byte, 12)
	binary.BigEndian.PutUint32(p[:4], uint32(code))
	binary.BigEndian.PutUint64(p[4:], uint64(t))
	return p
}

// ParsePong 解析 Ping 响应 payload，返回 (server_time_unix_nano, error)。
func ParsePong(r ByteReader) (int64, error) {
	code, err := ReadU32(r)
	if err != nil {
		return 0, err
	}
	if ErrCode(code) != CodeOK {
		return 0, MapCode(ErrCode(code))
	}
	t, err := ReadU64(r)
	if err != nil {
		return 0, err
	}
	return int64(t), nil
}

// EncodeMetaResp 编码 Meta 响应 payload: segID(8) off(8) size(8)。
func EncodeMetaResp(segID, off, size int64) []byte {
	p := make([]byte, 24)
	binary.BigEndian.PutUint64(p[0:], uint64(segID))
	binary.BigEndian.PutUint64(p[8:], uint64(off))
	binary.BigEndian.PutUint64(p[16:], uint64(size))
	return p
}

// ParseMetaResp 解析 Meta 响应 payload。
func ParseMetaResp(r ByteReader) (segID, off, size int64, err error) {
	var n uint64
	if n, err = ReadU64(r); err != nil {
		return 0, 0, 0, err
	}
	segID = int64(n)
	if n, err = ReadU64(r); err != nil {
		return 0, 0, 0, err
	}
	off = int64(n)
	if n, err = ReadU64(r); err != nil {
		return 0, 0, 0, err
	}
	size = int64(n)
	return segID, off, size, nil
}

// EncodeSegSum 编码段汇总帧 payload。
func EncodeSegSum(code ErrCode, s SegmentSummary) []byte {
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

// ParseSegSum 解析段汇总帧 payload。
func ParseSegSum(r ByteReader) (SegmentSummary, error) {
	code, err := ReadU32(r)
	if err != nil {
		return SegmentSummary{}, err
	}
	if ErrCode(code) != CodeOK {
		return SegmentSummary{}, MapCode(ErrCode(code))
	}
	var s SegmentSummary
	for _, v := range []*int64{&s.Total, &s.Free, &s.Active, &s.Full, &s.Reclaiming, &s.CursorSeg, &s.CursorOff, &s.SegSize, &s.ObjectCount} {
		n, err := ReadU64(r)
		if err != nil {
			return SegmentSummary{}, err
		}
		*v = int64(n)
	}
	return s, nil
}

// EncodeSegData 编码段明细帧 payload（单帧可容纳 ~16 万条，远大于 2048 段配额）。
func EncodeSegData(entries []SegmentEntry) []byte {
	p := make([]byte, 4+len(entries)*SegItemLen)
	binary.BigEndian.PutUint32(p[:4], uint32(len(entries)))
	for i, e := range entries {
		b := 4 + i*SegItemLen
		binary.BigEndian.PutUint64(p[b:], uint64(e.SegmentID))
		p[b+8] = e.State
		binary.BigEndian.PutUint64(p[b+9:], uint64(e.AliveCount))
		binary.BigEndian.PutUint64(p[b+17:], uint64(e.ReclaimSeq))
	}
	return p
}

// ParseSegData 解析段明细帧 payload，追加到 out。
func ParseSegData(r ByteReader, out []SegmentEntry) ([]SegmentEntry, error) {
	count, err := ReadU32(r)
	if err != nil {
		return out, err
	}
	for i := uint32(0); i < count; i++ {
		var (
			id, alive, seq int64
			n              uint64
			err            error
		)
		if n, err = ReadU64(r); err != nil {
			return out, err
		}
		id = int64(n)
		st, err := r.Next(1)
		if err != nil {
			return out, err
		}
		if n, err = ReadU64(r); err != nil {
			return out, err
		}
		alive = int64(n)
		if n, err = ReadU64(r); err != nil {
			return out, err
		}
		seq = int64(n)
		out = append(out, SegmentEntry{SegmentID: id, State: st[0], AliveCount: alive, ReclaimSeq: seq})
	}
	return out, nil
}

// EncodeKeysData 编码 key 列表帧 payload: count(4) + count × [keyLen(4) key]。
func EncodeKeysData(keys []string) []byte {
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

// ParseKeysData 解析 key 列表帧 payload，追加到 out。
func ParseKeysData(r ByteReader, out []string) ([]string, error) {
	count, err := ReadU32(r)
	if err != nil {
		return out, err
	}
	for i := uint32(0); i < count; i++ {
		kl, err := ReadU32(r)
		if err != nil {
			return out, err
		}
		if kl > MaxKeyLen {
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
