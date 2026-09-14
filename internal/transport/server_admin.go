package transport

import (
	"context"
	"time"
)

// 管理类 RPC 处理器（taihu-cli 设计文档 §4）：Ping / Meta / Segments / ListKeys。
// 首版仅 TCP 路径（shmipc 路径不实现，见设计文档已知取舍）。

// handlePing 处理探活请求（一元）：回 opPong{code, server_time_unix_nano}。
func (s *Server) handlePing(c *Conn, st *stream) {
	defer c.endStream(st)
	if _, err := c.await(context.Background(), st); err != nil {
		return
	}
	t, err := s.storage.Ping(context.Background())
	if err != nil {
		_ = c.writeFrame(st.id, opPong, encodePong(codeInternal, time.Now().UnixNano()))
		return
	}
	_ = c.writeFrame(st.id, opPong, encodePong(codeOK, t))
}

// handleMeta 处理对象元数据请求（一元）：成功回 opMetaResp{segID, off, size}，
// 失败回 opResp{code}（not found / invalid argument）。
func (s *Server) handleMeta(c *Conn, st *stream) {
	defer c.endStream(st)
	first, err := c.await(context.Background(), st)
	if err != nil {
		return
	}
	key, err := parseKeyReq(first.r)
	first.r.Release()
	if err != nil {
		_ = c.writeFrame(st.id, opResp, encCode(codeInvalidArgument))
		return
	}
	m, err := s.storage.ObjectMeta(context.Background(), key)
	if err != nil {
		_ = c.writeFrame(st.id, opResp, encCode(mapStorageErr(err)))
		return
	}
	_ = c.writeFrame(st.id, opMetaResp, encodeMetaResp(m.SegmentID, m.Offset, m.Size))
}

// handleSegments 处理段汇总+明细请求：流式下发 opSegSum → opSegData[*] → opSegEnd。
// 出错直接 opResp{code}（不再发后续帧）。
func (s *Server) handleSegments(c *Conn, st *stream) {
	defer c.endStream(st)
	if _, err := c.await(context.Background(), st); err != nil {
		return
	}
	sum, entries, err := s.storage.Segments(context.Background())
	if err != nil {
		_ = c.writeFrame(st.id, opResp, encCode(mapStorageErr(err)))
		return
	}
	_ = c.writeFrame(st.id, opSegSum, encodeSegSum(codeOK, SegmentSummary{
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
	const maxPerFrame = (chunkSize - 4) / segItemLen
	wire := make([]SegmentEntry, 0, len(entries))
	for _, e := range entries {
		wire = append(wire, SegmentEntry{
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
		if wErr := c.writeFrame(st.id, opSegData, encodeSegData(wire[:n])); wErr != nil {
			return
		}
		wire = wire[n:]
	}
	_ = c.writeFrame(st.id, opSegEnd, nil)
}

// handleListKeys 处理 key 枚举请求：opKeysReq{prefix} → opKeysData[*] → opResp{code}。
// key 上限 maxKeyLen(64KB) 远小于单帧负载，单 key 不拆帧（防御分支省略）。
func (s *Server) handleListKeys(c *Conn, st *stream) {
	defer c.endStream(st)
	first, err := c.await(context.Background(), st)
	if err != nil {
		return
	}
	prefix, err := parseKeyReq(first.r)
	first.r.Release()
	if err != nil {
		_ = c.writeFrame(st.id, opResp, encCode(codeInvalidArgument))
		return
	}
	keys, err := s.storage.ListKeys(context.Background(), prefix)
	if err != nil {
		_ = c.writeFrame(st.id, opResp, encCode(mapStorageErr(err)))
		return
	}
	remain := keys
	for len(remain) > 0 {
		n, size := 0, 4
		for n < len(remain) && size+4+len(remain[n]) <= chunkSize {
			size += 4 + len(remain[n])
			n++
		}
		if wErr := c.writeFrame(st.id, opKeysData, encodeKeysData(remain[:n])); wErr != nil {
			return
		}
		remain = remain[n:]
	}
	_ = c.writeFrame(st.id, opResp, encCode(codeOK))
}
