package transport

import (
	"context"
	"time"

	"github.com/liucxer/taihu/internal/transport/protocol"
)

// 管理类 RPC 处理器（taihu-cli 设计文档 §4）：Ping / Meta / Segments / ListKeys。
// 首版仅 TCP 路径（shmipc 路径不实现，见设计文档已知取舍）。

// handlePing 处理探活请求（一元）：回 OpPong{code, server_time_unix_nano}。
func (s *Server) handlePing(c *Conn, st *stream) {
	defer c.endStream(st)
	if _, err := c.await(context.Background(), st); err != nil {
		return
	}
	t, err := s.storage.Ping(context.Background())
	if err != nil {
		_ = c.writeFrame(st.id, protocol.OpPong, protocol.EncodePong(protocol.CodeInternal, time.Now().UnixNano()))
		return
	}
	_ = c.writeFrame(st.id, protocol.OpPong, protocol.EncodePong(protocol.CodeOK, t))
}

// handleMeta 处理对象元数据请求（一元）：成功回 OpMetaResp{segID, off, size}，
// 失败回 OpResp{code}（not found / invalid argument）。
func (s *Server) handleMeta(c *Conn, st *stream) {
	defer c.endStream(st)
	first, err := c.await(context.Background(), st)
	if err != nil {
		return
	}
	key, err := protocol.ParseKeyReq(first.r)
	first.r.Release()
	if err != nil {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
		return
	}
	m, err := s.storage.ObjectMeta(context.Background(), key)
	if err != nil {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
		return
	}
	_ = c.writeFrame(st.id, protocol.OpMetaResp, protocol.EncodeMetaResp(m.SegmentID, m.Offset, m.Size))
}

// handleSegments 处理段汇总+明细请求：流式下发 OpSegSum → OpSegData[*] → OpSegEnd。
// 出错直接 OpResp{code}（不再发后续帧）。
func (s *Server) handleSegments(c *Conn, st *stream) {
	defer c.endStream(st)
	if _, err := c.await(context.Background(), st); err != nil {
		return
	}
	sum, entries, err := s.storage.Segments(context.Background())
	if err != nil {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
		return
	}
	_ = c.writeFrame(st.id, protocol.OpSegSum, protocol.EncodeSegSum(protocol.CodeOK, protocol.SegmentSummary{
		Total:       sum.Total,
		Free:        sum.Free,
		Active:      sum.Active,
		Full:        sum.Full,
		Reclaiming:  sum.Reclaiming,
		CursorSeg:   sum.CursorSeg,
		CursorOff:   sum.CursorOff,
		SegSize:     sum.SegSize,
		ObjectCount: sum.ObjectCount,
	}))

	// 明细按 4MiB 帧上限分帧下发（2048 段 ≈ 51KB，正常单帧即可容纳）。
	const maxPerFrame = (protocol.ChunkSize - 4) / protocol.SegItemLen
	wire := make([]protocol.SegmentEntry, 0, len(entries))
	for _, e := range entries {
		wire = append(wire, protocol.SegmentEntry{
			SegmentID:  e.SegmentID,
			State:      uint8(e.State),
			AliveCount: e.AliveCount,
			ReclaimSeq: e.ReclaimSeq,
		})
	}
	for len(wire) > 0 {
		n := len(wire)
		if n > maxPerFrame {
			n = maxPerFrame
		}
		if wErr := c.writeFrame(st.id, protocol.OpSegData, protocol.EncodeSegData(wire[:n])); wErr != nil {
			return
		}
		wire = wire[n:]
	}
	_ = c.writeFrame(st.id, protocol.OpSegEnd, nil)
}

// handleListKeys 处理 key 枚举请求：OpKeysReq{prefix} → OpKeysData[*] → OpResp{code}。
// key 上限 MaxKeyLen(64KB) 远小于单帧负载，单 key 不拆帧（防御分支省略）。
func (s *Server) handleListKeys(c *Conn, st *stream) {
	defer c.endStream(st)
	first, err := c.await(context.Background(), st)
	if err != nil {
		return
	}
	prefix, err := protocol.ParseKeyReq(first.r)
	first.r.Release()
	if err != nil {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
		return
	}
	keys, err := s.storage.ListKeys(context.Background(), prefix)
	if err != nil {
		_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.MapStorageErr(err)))
		return
	}
	remain := keys
	for len(remain) > 0 {
		n, size := 0, 4
		for n < len(remain) && size+4+len(remain[n]) <= protocol.ChunkSize {
			size += 4 + len(remain[n])
			n++
		}
		if wErr := c.writeFrame(st.id, protocol.OpKeysData, protocol.EncodeKeysData(remain[:n])); wErr != nil {
			return
		}
		remain = remain[n:]
	}
	_ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeOK))
}
