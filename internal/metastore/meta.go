package metastore

import (
	"encoding/binary"
	"fmt"
)

// 磁盘值编码：定义 mapping / segment / cursor 三类记录的 value 布局与编解码，
// 是本层「磁盘格式」的唯一事实源（键编码见 model.go）。任何格式演进都从这里开始。
// 类型声明集中在 metastore.go，此处只保留私有编解码实现。
//
// 编码规则：固定字段 binary little-endian（int64 占 8 字节），
// 每个 value 头部预留 1 字节 version（当前为 0），便于版本演进。

const (
	metaVersion    = byte(0)
	objectMetaLen  = 1 + 8*3     // version + SegmentID + Offset + Size
	writeCursorLen = 1 + 8*2     // version + SegmentID + Offset
	segmentMetaLen = 1 + 1 + 8*2 // version + State + AliveCount + ReclaimSeq
)

// ObjectMeta 的 value 编码（SegmentID 所在 segment；Offset 段内起始偏移恒 4K 对齐；
// Size 逻辑大小不含 4K 填充）。
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

// writeCursor 顺序写游标（state 命名空间，key = "cursor"）：全局唯一，记录下一个
// object 的落点（Offset 4K 对齐）。类型私有：仅 allocator 内部使用，不参与对外导出面。
type writeCursor struct {
	SegmentID int64
	Offset    int64
}

func (c writeCursor) encode() []byte {
	b := make([]byte, writeCursorLen)
	b[0] = metaVersion
	binary.LittleEndian.PutUint64(b[1:], uint64(c.SegmentID))
	binary.LittleEndian.PutUint64(b[9:], uint64(c.Offset))
	return b
}

func decodeWriteCursor(b []byte) (writeCursor, error) {
	if len(b) != writeCursorLen {
		return writeCursor{}, fmt.Errorf("taihu: bad WriteCursor length %d", len(b))
	}
	return writeCursor{
		SegmentID: int64(binary.LittleEndian.Uint64(b[1:9])),
		Offset:    int64(binary.LittleEndian.Uint64(b[9:17])),
	}, nil
}

// SegmentMeta 的 value 编码（用于 segment 生命周期管理 / GC）。
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
