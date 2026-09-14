package transport

import (
	"context"
	"fmt"
)

// 管理类 RPC 客户端（taihu-cli 设计文档 §4）。与数据面同连接、同流式帧协议。

// Ping 探活：回服务端时间（unix 纳秒）。RTT 由调用方掐表计算。
func (c *Conn) Ping(ctx context.Context) (int64, error) {
	st := c.newStream()
	defer c.removeStream(st)
	if werr := c.writeFrame(st.id, opPing, nil); werr != nil {
		return 0, werr
	}
	msg, err := c.await(ctx, st)
	if err != nil {
		return 0, err
	}
	defer msg.r.Release()
	if msg.op != opPong {
		return 0, fmt.Errorf("taihu: unexpected ping response op %d", msg.op)
	}
	return parsePong(msg.r)
}

// Meta 返回对象落盘元数据；key 不存在返回 ErrNotFound。
func (c *Conn) Meta(ctx context.Context, key string) (segID, off, size int64, err error) {
	st := c.newStream()
	defer c.removeStream(st)
	if werr := c.writeFrame(st.id, opMetaReq, encodeKeyReq(key)); werr != nil {
		return 0, 0, 0, werr
	}
	msg, err := c.await(ctx, st)
	if err != nil {
		return 0, 0, 0, err
	}
	defer msg.r.Release()
	switch msg.op {
	case opMetaResp:
		return parseMetaResp(msg.r)
	case opResp:
		code, err := readU32(msg.r)
		if err != nil {
			return 0, 0, 0, err
		}
		return 0, 0, 0, mapCode(errCode(code))
	default:
		return 0, 0, 0, fmt.Errorf("taihu: unexpected meta response op %d", msg.op)
	}
}

// Segments 拉取段汇总+全部明细（多帧聚合）。
func (c *Conn) Segments(ctx context.Context) (SegmentSummary, []SegmentEntry, error) {
	st := c.newStream()
	defer c.removeStream(st)
	if werr := c.writeFrame(st.id, opSegReq, nil); werr != nil {
		return SegmentSummary{}, nil, werr
	}

	var (
		sum     SegmentSummary
		entries []SegmentEntry
		gotSum  bool
	)
	for {
		msg, err := c.await(ctx, st)
		if err != nil {
			return SegmentSummary{}, nil, err
		}
		switch msg.op {
		case opSegSum:
			sum, err = parseSegSum(msg.r)
			msg.r.Release()
			if err != nil {
				return SegmentSummary{}, nil, err
			}
			gotSum = true
		case opSegData:
			entries, err = parseSegData(msg.r, entries)
			msg.r.Release()
			if err != nil {
				return SegmentSummary{}, nil, err
			}
		case opSegEnd:
			msg.r.Release()
			if !gotSum {
				return SegmentSummary{}, nil, fmt.Errorf("taihu: segments stream missing summary")
			}
			return sum, entries, nil
		case opResp:
			code, err := readU32(msg.r)
			msg.r.Release()
			if err != nil {
				return SegmentSummary{}, nil, err
			}
			return SegmentSummary{}, nil, mapCode(errCode(code))
		default:
			msg.r.Release()
			return SegmentSummary{}, nil, fmt.Errorf("taihu: unexpected segments frame op %d", msg.op)
		}
	}
}

// ListKeys 按前缀枚举 key（前缀为空=全部），流式聚合返回。
func (c *Conn) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	st := c.newStream()
	defer c.removeStream(st)
	if werr := c.writeFrame(st.id, opKeysReq, encodeKeyReq(prefix)); werr != nil {
		return nil, werr
	}

	var keys []string
	for {
		msg, err := c.await(ctx, st)
		if err != nil {
			return nil, err
		}
		switch msg.op {
		case opKeysData:
			keys, err = parseKeysData(msg.r, keys)
			msg.r.Release()
			if err != nil {
				return nil, err
			}
		case opResp:
			code, err := readU32(msg.r)
			msg.r.Release()
			if err != nil {
				return nil, err
			}
			if err := mapCode(errCode(code)); err != nil {
				return nil, err
			}
			return keys, nil
		default:
			msg.r.Release()
			return nil, fmt.Errorf("taihu: unexpected keys frame op %d", msg.op)
		}
	}
}
