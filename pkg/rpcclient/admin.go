package rpcclient

import (
	"context"
	"errors"
	"time"

	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/internal/transport"
)

// 管理类 RPC（taihu-cli 设计文档 §4）。首版仅 TCP 路径支持：Storage 内部连接中
// 取第一条 *transport.Conn（netpoll TCP），shmipc 连接不支持 admin 调用。
// 若上层需要强制走 TCP，请用 DialPool 构建 Storage。

// Ping 探活：测 RTT 并返回服务端时间（unix 纳秒）。
func (s *Storage) Ping(ctx context.Context) (rtt time.Duration, serverTime int64, err error) {
	conn, err := s.adminConn()
	if err != nil {
		return 0, 0, err
	}
	start := time.Now()
	t, err := conn.Ping(ctx)
	if err != nil {
		return 0, 0, err
	}
	return time.Since(start), t, nil
}

// Meta 返回对象落盘元数据（segment/offset/size）；key 不存在返回 ErrNotFound。
func (s *Storage) Meta(ctx context.Context, key string) (storage.ObjectMeta, error) {
	conn, err := s.adminConn()
	if err != nil {
		return storage.ObjectMeta{}, err
	}
	segID, off, size, err := conn.Meta(ctx, key)
	if err != nil {
		return storage.ObjectMeta{}, err
	}
	return storage.ObjectMeta{SegmentID: segID, Offset: off, Size: size}, nil
}

// Segments 返回段汇总与全部段明细。
func (s *Storage) Segments(ctx context.Context) (storage.SegmentSummary, []storage.SegmentEntry, error) {
	conn, err := s.adminConn()
	if err != nil {
		return storage.SegmentSummary{}, nil, err
	}
	sum, entries, err := conn.Segments(ctx)
	if err != nil {
		return storage.SegmentSummary{}, nil, err
	}
	tsum := storage.SegmentSummary{
		Total:       sum.Total,
		Free:        sum.Free,
		Active:      sum.Active,
		Full:        sum.Full,
		Reclaiming:  sum.Reclaiming,
		CursorSeg:   sum.CursorSeg,
		CursorOff:   sum.CursorOff,
		SegSize:     sum.SegSize,
		ObjectCount: sum.ObjectCount,
	}
	tentries := make([]storage.SegmentEntry, 0, len(entries))
	for _, e := range entries {
		tentries = append(tentries, storage.SegmentEntry{
			SegmentID:  e.SegmentID,
			State:      metastoreSegmentState(e.State),
			AliveCount: e.AliveCount,
			ReclaimSeq: e.ReclaimSeq,
		})
	}
	return tsum, tentries, nil
}

// ListKeys 按前缀枚举 key（前缀为空=全部）。
func (s *Storage) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	conn, err := s.adminConn()
	if err != nil {
		return nil, err
	}
	return conn.ListKeys(ctx, prefix)
}

// adminConn 取第一条支持 admin RPC 的 TCP 连接。
func (s *Storage) adminConn() (*transport.Conn, error) {
	for _, c := range s.conns {
		if t, ok := c.(*transport.Conn); ok {
			return t, nil
		}
	}
	return nil, errors.New("taihu: admin RPC not supported on this transport (shm only)")
}

// metastoreSegmentState 将 wire(uint8) 还原为 metastore.SegmentState。
func metastoreSegmentState(b uint8) metastore.SegmentState {
	return metastore.SegmentState(b)
}
