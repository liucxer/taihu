package storage

import (
	"context"
	"sort"
	"time"

	"github.com/liucxer/taihu/internal/metastore"
)

// 管理/诊断能力（taihu-cli 设计文档 §4）：Segment 详情、key 枚举、Ping。
// 全部走服务端新增 admin RPC 透出；数据面热路径不调用。

// SegmentEntry 单段状态明细（wire 上的 state 经 transport 编码为 1 字节）。
type SegmentEntry struct {
	SegmentID  int64
	State      metastore.SegmentState
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

// Segments 枚举全部段状态并统计汇总（含写游标与对象数；对象数经 mapping 全量计数）。
// 返回明细按 SegmentID 升序，便于稳定展示。
func (s *Storage) Segments(ctx context.Context) (SegmentSummary, []SegmentEntry, error) {
	var (
		summary SegmentSummary
		entries []SegmentEntry
	)
	summary.SegSize = s.layout.SegmentSizeBytes
	summary.CursorSeg, summary.CursorOff = s.db.Cursor()

	err := s.db.ListSegments(ctx, func(id int64, m metastore.SegmentMeta) error {
		entries = append(entries, SegmentEntry{SegmentID: id, State: m.State, AliveCount: m.AliveCount, ReclaimSeq: m.ReclaimSeq})
		summary.Total++
		switch m.State {
		case metastore.SegmentStateFree:
			summary.Free++
		case metastore.SegmentStateActive:
			summary.Active++
		case metastore.SegmentStateFull:
			summary.Full++
		case metastore.SegmentStateReclaiming:
			summary.Reclaiming++
		}
		return nil
	})
	if err != nil {
		return summary, nil, err
	}
	_ = s.db.IterMapping(ctx, func(key string, m ObjectMeta) error { // 对象数（尽力而为）
		summary.ObjectCount++
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].SegmentID < entries[j].SegmentID })
	return summary, entries, nil
}

// ListKeys 枚举 mapping 中 key 前缀匹配的全部对象 key（字典序）。prefix 为空=全部。
// 全量扫描，仅 admin 使用。
func (s *Storage) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	err := s.db.IterMapping(ctx, func(key string, m ObjectMeta) error {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			keys = append(keys, key)
		}
		return nil
	})
	return keys, err
}

// ObjectMeta 返回对象落盘元数据（segment/offset/size）；key 不存在返回 ErrNotFound。
func (s *Storage) ObjectMeta(ctx context.Context, key string) (ObjectMeta, error) {
	return s.db.GetMapping(ctx, key)
}

// Ping 探活：返回服务端时间（unix 纳秒）。CLI 侧掐表测 RTT。
func (s *Storage) Ping(ctx context.Context) (int64, error) {
	return time.Now().UnixNano(), nil
}
