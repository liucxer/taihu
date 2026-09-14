package metastore

import (
	"encoding/binary"
	"fmt"
)

// Model 层数据类型，明确定义 RocksDB 中 value 的结构体与二进制编码。
//
// 编码规则：固定字段 binary little-endian（int64 占 8 字节），
// 每个 value 头部预留 1 字节 version（当前为 0），便于版本演进。

const (
	metaVersion    = byte(0)
	objectMetaLen  = 1 + 8*3     // version + SegmentID + Offset + Size
	writeCursorLen = 1 + 8*2     // version + SegmentID + Offset
	segmentMetaLen = 1 + 1 + 8*2 // version + State + AliveCount + ReclaimSeq
)

// ObjectMeta 存放在 mapping 列族：key = 用户 key。
// SegmentID 所在 segment；Offset 段内起始偏移（恒 4K 对齐）；Size 逻辑大小（不含 4K 填充）。
type ObjectMeta struct {
	SegmentID int64
	Offset    int64
	Size      int64
}

func (m ObjectMeta) encode() []byte {
	b := make([]byte, objectMetaLen)
	b[0] = metaVersion
	binary.LittleEndian.PutUint64(b[1:], uint64(m.SegmentID))
	binary.LittleEndian.PutUint64(b[9:], uint64(m.Offset))
	binary.LittleEndian.PutUint64(b[17:], uint64(m.Size))
	return b
}

func decodeObjectMeta(b []byte) (ObjectMeta, error) {
	if len(b) != objectMetaLen {
		return ObjectMeta{}, fmt.Errorf("taihu: bad ObjectMeta length %d", len(b))
	}
	return ObjectMeta{
		SegmentID: int64(binary.LittleEndian.Uint64(b[1:9])),
		Offset:    int64(binary.LittleEndian.Uint64(b[9:17])),
		Size:      int64(binary.LittleEndian.Uint64(b[17:25])),
	}, nil
}

// WriteCursor 存放在 state 列族，key = "cursor"。
// 全局唯一顺序写游标，记录下一个 object 的落点（Offset 4K 对齐）。
type WriteCursor struct {
	SegmentID int64
	Offset    int64
}

func (c WriteCursor) encode() []byte {
	b := make([]byte, writeCursorLen)
	b[0] = metaVersion
	binary.LittleEndian.PutUint64(b[1:], uint64(c.SegmentID))
	binary.LittleEndian.PutUint64(b[9:], uint64(c.Offset))
	return b
}

func decodeWriteCursor(b []byte) (WriteCursor, error) {
	if len(b) != writeCursorLen {
		return WriteCursor{}, fmt.Errorf("taihu: bad WriteCursor length %d", len(b))
	}
	return WriteCursor{
		SegmentID: int64(binary.LittleEndian.Uint64(b[1:9])),
		Offset:    int64(binary.LittleEndian.Uint64(b[9:17])),
	}, nil
}

// SegmentState 描述单个 segment 的生命周期状态。
type SegmentState uint8

const (
	SegmentStateFree       SegmentState = iota // 空闲，可分配
	SegmentStateActive                         // 正在顺序写入
	SegmentStateFull                           // 已写满，仅读
	SegmentStateReclaiming                     // 待回收（计数归零）
	SegmentStateCompacting                     // 搬移中（高空洞段存活对象搬迁，禁止分配/回收）
)

// SegmentMeta 存放在 state 列族，key = "seg/<segmentID>"。
// 用于 segment 生命周期管理 / GC（v1 预留字段，物理回收后续实现）。
type SegmentMeta struct {
	State      SegmentState
	AliveCount int64
	ReclaimSeq int64
}

func (m SegmentMeta) encode() []byte {
	b := make([]byte, segmentMetaLen)
	b[0] = metaVersion
	b[1] = byte(m.State)
	binary.LittleEndian.PutUint64(b[2:], uint64(m.AliveCount))
	binary.LittleEndian.PutUint64(b[10:], uint64(m.ReclaimSeq))
	return b
}

func decodeSegmentMeta(b []byte) (SegmentMeta, error) {
	if len(b) != segmentMetaLen {
		return SegmentMeta{}, fmt.Errorf("taihu: bad SegmentMeta length %d", len(b))
	}
	return SegmentMeta{
		State:      SegmentState(b[1]),
		AliveCount: int64(binary.LittleEndian.Uint64(b[2:10])),
		ReclaimSeq: int64(binary.LittleEndian.Uint64(b[10:18])),
	}, nil
}

// SegmentEntry 单个 segment 的状态明细（领域态，唯一一份定义）。
//
// 为什么在这里：本包已拥有 segment 的状态机（SegmentState）与 SegmentMeta，
// 段明细只是把 SegmentMeta 加上段号后摊平，归属本包最自然。
//
// 编解码另有一份 wire 结构：internal/transport/protocol.SegmentEntry，字段相同
// 但 State 是 uint8。**刻意不让 protocol import 本包** —— 本包依赖
// github.com/cockroachdb/pebble，而 protocol 是一个只依赖 encoding/binary 的
// 纯 codec（带表驱动单测），把持久化模型拖进编解码层不划算；领域态与 wire 态
// 本就是两个东西，转换发生在服务端边界（internal/transport/server_admin.go）
// 是合理的。两份结构的字段平齐性由 protocol_test.go 的 TestSegmentWireParity 守着。
type SegmentEntry struct {
	SegmentID  int64
	State      SegmentState
	AliveCount int64
	ReclaimSeq int64
}

// SegmentSummary 实例段汇总与写游标（领域态，唯一一份定义）。
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

// state 列族中关键字。
const (
	kvCursorKey     = "cursor"
	kvSegmentPrefix = "seg/"
)

func segmentKey(segmentID int64) []byte {
	b := make([]byte, 0, len(kvSegmentPrefix)+8)
	b = append(b, kvSegmentPrefix...)
	b = binary.LittleEndian.AppendUint64(b, uint64(segmentID))
	return b
}
