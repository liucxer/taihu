// 帧层（frame.go / stats.go / client.go 分发与错误分支）白盒测试。
// 需要构造客户端不会发出的帧序列时，用裸 TCP 假服务端按脚本应答。
package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liucxer/taihu/third_party/netpoll"

	"github.com/liucxer/taihu/internal/ierr"
	"github.com/liucxer/taihu/internal/transport/protocol"
)

// TestTCPStreamLifecycleAndIDs 覆盖 stream/finish 幂等与 Conn 的流注册/注销。
func TestTCPStreamLifecycleAndIDs(t *testing.T) {
	addr, _ := tcpServerAndStorage(t)
	c := tcpDial(t, addr)

	s1 := c.newStream()
	s2 := c.newStream()
	if s2.id != s1.id+1 {
		t.Fatalf("stream ids = %d, %d; want 差 1", s1.id, s2.id)
	}
	if len(c.streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(c.streams))
	}

	// finish 幂等：重复调用不 panic
	s1.finish()
	s1.finish()
	select {
	case <-s1.done:
	default:
		t.Fatal("finish 未关闭 done")
	}

	c.removeStream(s1)
	if _, ok := c.streams[s1.id]; ok {
		t.Fatalf("removeStream 未注销流 %d", s1.id)
	}
	c.removeStream(s2)
	if len(c.streams) != 0 {
		t.Fatalf("streams = %d, want 0", len(c.streams))
	}
}

// TestTCPConnCloseUnblocksAwait 覆盖 closeAll 幂等与 await 的 conn/stream 终止分支。
func TestTCPConnCloseUnblocksAwait(t *testing.T) {
	addr, _ := tcpServerAndStorage(t)
	c, err := DialClient("tcp", addr)
	if err != nil {
		t.Fatalf("DialClient: %v", err)
	}
	st := c.newStream()
	st2 := c.newStream()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.await(context.Background(), st); !errors.Is(err, errConnClosed) {
		t.Fatalf("await after close err=%v, want errConnClosed", err)
	}
	// 幂等
	c.closeAll()
	c.closeAll()

	// Close/closeAll 会 finish 全部已注册流
	for _, s := range []*stream{st, st2} {
		select {
		case <-s.done:
		default:
			t.Fatalf("closeAll 未 finish 流 %d", s.id)
		}
	}
	select {
	case <-c.closed:
	default:
		t.Fatal("closeAll 未关闭 closed")
	}
}

// TestTCPWriteFrameOnClosedConn 覆盖 writeFrame 在已关闭连接上的错误返回。
func TestTCPWriteFrameOnClosedConn(t *testing.T) {
	addr, _ := tcpServerAndStorage(t)
	c, err := DialClient("tcp", addr)
	if err != nil {
		t.Fatalf("DialClient: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.writeFrame(1, protocol.OpPing, nil); err == nil {
		t.Fatal("writeFrame on closed conn should error")
	}
	// 大负载（>4K，走零拷贝引用分支）同样返回错误
	if err := c.writeFrame(2, protocol.OpPutData, make([]byte, 8192)); err == nil {
		t.Fatal("writeFrame(large) on closed conn should error")
	}
}

// TestTCPReadLoopExitOnPeerClose 覆盖读循环 Peek 失败退出并关闭连接。
func TestTCPReadLoopExitOnPeerClose(t *testing.T) {
	addr := tcpFakeServer(t, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(io.Discard, conn)
	})
	nc, err := netpoll.DialConnection("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("DialConnection: %v", err)
	}
	c := &Conn{c: nc, streams: make(map[uint32]*stream), closed: make(chan struct{}), dispatch: clientDispatch}
	done := make(chan struct{})
	go func() {
		c.readLoop()
		close(done)
	}()
	_ = nc.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("readLoop did not exit after peer close")
	}
	select {
	case <-c.closed:
	default:
		t.Fatal("readLoop exit 未关闭 closed")
	}
}

// TestTCPClientDispatchRouting 覆盖客户端分发三条路径：未知流丢弃、正常投递、
// 流已结束（done）丢弃、连接已关闭（fatal）。手工驱动读循环，避免竞态。
func TestTCPClientDispatchRouting(t *testing.T) {
	frames := make(chan []byte, 32)
	peerClosed := make(chan struct{})
	var once sync.Once
	addr := tcpFakeServer(t, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		go func() {
			_, _ = io.Copy(io.Discard, conn)
			once.Do(func() { close(peerClosed) })
		}()
		for b := range frames {
			if _, err := conn.Write(b); err != nil {
				return
			}
		}
	})
	defer close(frames)

	nc, err := netpoll.DialConnection("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("DialConnection: %v", err)
	}
	c := &Conn{c: nc, streams: make(map[uint32]*stream), closed: make(chan struct{}), dispatch: clientDispatch}
	go c.readLoop()

	// 1) 未知流帧：丢弃，连接保持可用
	frames <- tcpRawFrame(999, protocol.OpResp, protocol.EncCode(protocol.CodeOK))

	// 2) 正常投递
	st := c.newStream()
	frames <- tcpRawFrame(st.id, protocol.OpPong, protocol.EncodePong(protocol.CodeOK, 1234))
	msg, err := c.await(context.Background(), st)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if msg.op != protocol.OpPong {
		t.Fatalf("op = %d, want OpPong（未知流帧不应中断连接）", msg.op)
	}
	if tv, err := protocol.ParsePong(msg.r); err != nil || tv != 1234 {
		t.Fatalf("ParsePong = %d, %v", tv, err)
	}
	msg.r.Release()
	c.removeStream(st)

	// 3) 流已结束（in 满 + done 关闭）：丢弃，连接保持可用
	st2 := c.newStream()
	for i := 0; i < streamInCap; i++ {
		st2.in <- frameMsg{}
	}
	st2.finish()
	frames <- tcpRawFrame(st2.id, protocol.OpResp, protocol.EncCode(protocol.CodeOK))

	st3 := c.newStream()
	frames <- tcpRawFrame(st3.id, protocol.OpPong, protocol.EncodePong(protocol.CodeOK, 7))
	msg3, err := c.await(context.Background(), st3)
	if err != nil {
		t.Fatalf("await after done-drop: %v", err)
	}
	msg3.r.Release()
	c.removeStream(st3)

	// 4) 连接已关闭（closed 关闭）：fatal → 读循环退出、连接关闭
	st4 := c.newStream()
	for i := 0; i < streamInCap; i++ {
		st4.in <- frameMsg{}
	}
	c.mu.Lock()
	delete(c.streams, st4.id) // 先把流摘出 map，使 closeAll 不关闭其 done
	c.mu.Unlock()
	c.closeAll()
	c.mu.Lock()
	c.streams[st4.id] = st4
	c.mu.Unlock()
	frames <- tcpRawFrame(st4.id, protocol.OpResp, protocol.EncCode(protocol.CodeOK))
	select {
	case <-peerClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("readLoop did not treat frame after conn close as fatal")
	}
}

// TestTCPClientUnexpectedResponseOp 覆盖客户端各 RPC 收到非法响应 op 的分支。
func TestTCPClientUnexpectedResponseOp(t *testing.T) {
	addr := tcpScriptServer(t, func(sid uint32, op protocol.OpCode, _ []byte) [][]byte {
		switch op {
		case protocol.OpPutHeader, protocol.OpPutData:
			return nil // Put 流多帧，只应答结束帧
		case protocol.OpKeysReq:
			return [][]byte{tcpRawFrame(sid, protocol.OpSegSum, make([]byte, 4))}
		default:
			return [][]byte{tcpRawFrame(sid, protocol.OpKeysData, protocol.EncodeKeysData(nil))}
		}
	})
	c, err := DialClient("tcp", addr)
	if err != nil {
		t.Fatalf("DialClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	if err := c.Put(ctx, "k", 1, []byte("x")); err == nil {
		t.Fatal("Put with unexpected op should error")
	}
	if err := c.Delete(ctx, "k"); err == nil {
		t.Fatal("Delete with unexpected op should error")
	}
	if _, err := c.Stat(ctx, "k"); err == nil {
		t.Fatal("Stat with unexpected op should error")
	}
	if _, err := c.Ping(ctx); err == nil {
		t.Fatal("Ping with unexpected op should error")
	}
	if _, _, _, err := c.Meta(ctx, "k"); err == nil {
		t.Fatal("Meta with unexpected op should error")
	}
	if _, _, err := c.Segments(ctx); err == nil {
		t.Fatal("Segments with unexpected op should error")
	}
	if _, err := c.ListKeys(ctx, ""); err == nil {
		t.Fatal("ListKeys with unexpected op should error")
	}
	if _, _, err := c.Get(ctx, "k", 0, 1); err == nil {
		t.Fatal("Get with unexpected op should error")
	}
}

// TestTCPClientFrameSequenceErrors 用脚本服务端构造客户端解析/收流错误分支。
func TestTCPClientFrameSequenceErrors(t *testing.T) {
	addr := tcpScriptServer(t, func(sid uint32, op protocol.OpCode, payload []byte) [][]byte {
		switch op {
		case protocol.OpGetReq:
			key, _, size, err := protocol.ParseGetReq(protocol.NewSliceReader(payload))
			if err != nil {
				return nil
			}
			switch key {
			case "get/end": // 旧版收尾帧 OpGetEnd
				return [][]byte{
					tcpRawFrame(sid, protocol.OpGetData, bytes.Repeat([]byte{'z'}, int(size))),
					tcpRawFrame(sid, protocol.OpGetEnd, nil),
				}
			case "get/short-end":
				return [][]byte{
					tcpRawFrame(sid, protocol.OpGetData, []byte("ab")),
					tcpRawFrame(sid, protocol.OpGetEnd, nil),
				}
			case "get/exceeds":
				return [][]byte{tcpRawFrame(sid, protocol.OpGetData, bytes.Repeat([]byte{'z'}, int(size)+1))}
			case "get/err":
				return [][]byte{tcpRawFrame(sid, protocol.OpGetErr, protocol.EncCode(protocol.CodeInvalidRange))}
			}
			return nil
		case protocol.OpStatReq:
			key, err := protocol.ParseKeyReq(protocol.NewSliceReader(payload))
			if err == nil && key == "stat/short" {
				return [][]byte{tcpRawFrame(sid, protocol.OpStatResp, []byte{1, 2, 3, 4})}
			}
			return nil
		case protocol.OpPutEnd, protocol.OpDelReq:
			return [][]byte{tcpRawFrame(sid, protocol.OpResp, []byte{1, 2})}
		case protocol.OpPing:
			return [][]byte{tcpRawFrame(sid, protocol.OpPong, protocol.EncodePong(protocol.CodeInternal, 0))}
		case protocol.OpKeysReq:
			return [][]byte{tcpRawFrame(sid, protocol.OpResp, protocol.EncCode(protocol.CodeInternal))}
		case protocol.OpSegReq: // 缺 OpSegSum 就收尾
			return [][]byte{
				tcpRawFrame(sid, protocol.OpSegData, protocol.EncodeSegData(nil)),
				tcpRawFrame(sid, protocol.OpSegEnd, nil),
			}
		case protocol.OpMetaReq:
			return [][]byte{tcpRawFrame(sid, protocol.OpMetaResp, []byte{1, 2, 3})}
		}
		return nil
	})
	c, err := DialClient("tcp", addr)
	if err != nil {
		t.Fatalf("DialClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	// OpGetEnd 正常收尾
	got, rel, err := c.Get(ctx, "get/end", 0, 4)
	if err != nil || !bytes.Equal(got, []byte("zzzz")) {
		t.Fatalf("Get(get/end) = %q, %v", got, err)
	}
	rel()
	// 短读（OpGetEnd 前数据不足）
	if _, _, err := c.Get(ctx, "get/short-end", 0, 4); err == nil || !strings.Contains(err.Error(), "short read") {
		t.Fatalf("Get(short-end) err=%v", err)
	}
	// 响应超过请求长度
	if _, _, err := c.Get(ctx, "get/exceeds", 0, 4); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Get(exceeds) err=%v", err)
	}
	// OpGetErr 错误码映射
	if _, _, err := c.Get(ctx, "get/err", 0, 4); !errors.Is(err, ierr.ErrInvalidRange) {
		t.Fatalf("Get(err) err=%v", err)
	}
	// Stat 响应 payload 过短
	if _, err := c.Stat(ctx, "stat/short"); err == nil {
		t.Fatal("Stat(short payload) should error")
	}
	// Put/Delete 响应 payload 过短（读 code 失败）
	if err := c.Put(ctx, "k", 1, []byte("x")); err == nil {
		t.Fatal("Put(short code payload) should error")
	}
	if err := c.Delete(ctx, "k"); err == nil {
		t.Fatal("Delete(short code payload) should error")
	}
	// Ping 响应带错误码
	if _, err := c.Ping(ctx); err == nil {
		t.Fatal("Ping(error code) should error")
	}
	// ListKeys 收到错误码
	if _, err := c.ListKeys(ctx, ""); err == nil {
		t.Fatal("ListKeys(error code) should error")
	}
	// Segments 缺汇总帧
	if _, _, err := c.Segments(ctx); err == nil {
		t.Fatal("Segments(missing summary) should error")
	}
	// Meta 响应 payload 过短
	if _, _, _, err := c.Meta(ctx, "k"); err == nil {
		t.Fatal("Meta(short payload) should error")
	}
}

// tcpFailWriter 是 netpoll.Writer 桩：前 remaining 次 WriteBinary 成功，其后一律失败。
type tcpFailWriter struct {
	netpoll.Writer
	remaining int
}

func (w *tcpFailWriter) WriteBinary(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, errors.New("tcp: stub writer rejected write")
	}
	w.remaining--
	return len(p), nil
}

func (w *tcpFailWriter) Flush() error { return nil }

// tcpStubConn 是 netpoll.Connection 桩：只接管 Writer（本测试路径不触发其它方法）。
type tcpStubConn struct {
	netpoll.Connection
	w netpoll.Writer
}

func (c *tcpStubConn) Writer() netpoll.Writer { return c.w }

// tcpStubClientConn 构造写侧在第 remaining+1 次 WriteBinary 上失败的客户端连接。
func tcpStubClientConn(remaining int) *Conn {
	return &Conn{
		c:        &tcpStubConn{w: &tcpFailWriter{remaining: remaining}},
		streams:  make(map[uint32]*stream),
		closed:   make(chan struct{}),
		dispatch: clientDispatch,
	}
}

// TestTCPWriteFrameErrors 覆盖各 RPC 在写帧失败（首帧/数据帧/结束帧/负载帧）时的错误返回。
// Put 的帧构成：头帧 2 次 WriteBinary（帧头+负载）、数据帧 2 次、结束帧 1 次。
func TestTCPWriteFrameErrors(t *testing.T) {
	ctx := context.Background()
	payload := []byte("hello")

	cases := []struct {
		name      string
		remaining int
		call      func(c *Conn) error
	}{
		{"put-header-frame", 0, func(c *Conn) error { return c.Put(ctx, "k", int64(len(payload)), payload) }},
		{"put-data-frame", 2, func(c *Conn) error { return c.Put(ctx, "k", int64(len(payload)), payload) }},
		{"put-end-frame", 4, func(c *Conn) error { return c.Put(ctx, "k", int64(len(payload)), payload) }},
		{"get", 0, func(c *Conn) error { _, _, err := c.Get(ctx, "k", 0, 5); return err }},
		{"delete", 0, func(c *Conn) error { return c.Delete(ctx, "k") }},
		{"stat", 0, func(c *Conn) error { _, err := c.Stat(ctx, "k"); return err }},
		{"ping", 0, func(c *Conn) error { _, err := c.Ping(ctx); return err }},
		{"meta", 0, func(c *Conn) error { _, _, _, err := c.Meta(ctx, "k"); return err }},
		{"segments", 0, func(c *Conn) error { _, _, err := c.Segments(ctx); return err }},
		{"listkeys", 0, func(c *Conn) error { _, err := c.ListKeys(ctx, ""); return err }},
		{"writeframe-payload", 1, func(c *Conn) error { return c.writeFrame(1, protocol.OpPutData, make([]byte, 16)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(tcpStubClientConn(tc.remaining)); err == nil {
				t.Fatalf("%s: 写失败应返回错误", tc.name)
			}
		})
	}
}

// TestTCPClientResponsePayloadErrors 覆盖客户端各 RPC 收到畸形/过短响应 payload 的解析错误分支。
func TestTCPClientResponsePayloadErrors(t *testing.T) {
	ctx := context.Background()
	sum := protocol.EncodeSegSum(protocol.CodeOK, protocol.SegmentSummary{Total: 1})

	cases := []struct {
		name    string
		respond func(sid uint32) [][]byte
		call    func(c *Conn) error
		wantErr bool
	}{
		{
			name:    "get-err-short",
			respond: func(sid uint32) [][]byte { return [][]byte{tcpRawFrame(sid, protocol.OpGetErr, []byte{1, 2})} },
			call:    func(c *Conn) error { _, _, err := c.Get(ctx, "k", 0, 4); return err },
			wantErr: true,
		},
		{
			name:    "stat-resp-short",
			respond: func(sid uint32) [][]byte { return [][]byte{tcpRawFrame(sid, protocol.OpResp, []byte{1, 2})} },
			call:    func(c *Conn) error { _, err := c.Stat(ctx, "k"); return err },
			wantErr: true,
		},
		{
			name:    "meta-resp-short",
			respond: func(sid uint32) [][]byte { return [][]byte{tcpRawFrame(sid, protocol.OpResp, []byte{1, 2})} },
			call:    func(c *Conn) error { _, _, _, err := c.Meta(ctx, "k"); return err },
			wantErr: true,
		},
		{
			name:    "segments-sum-malformed",
			respond: func(sid uint32) [][]byte { return [][]byte{tcpRawFrame(sid, protocol.OpSegSum, []byte{1, 2, 3})} },
			call:    func(c *Conn) error { _, _, err := c.Segments(ctx); return err },
			wantErr: true,
		},
		{
			name: "segments-data-malformed",
			respond: func(sid uint32) [][]byte {
				return [][]byte{
					tcpRawFrame(sid, protocol.OpSegSum, sum),
					tcpRawFrame(sid, protocol.OpSegData, []byte{1, 2}),
				}
			},
			call:    func(c *Conn) error { _, _, err := c.Segments(ctx); return err },
			wantErr: true,
		},
		{
			name: "segments-resp-ok",
			respond: func(sid uint32) [][]byte {
				return [][]byte{tcpRawFrame(sid, protocol.OpResp, protocol.EncCode(protocol.CodeOK))}
			},
			call:    func(c *Conn) error { _, _, err := c.Segments(ctx); return err },
			wantErr: false,
		},
		{
			name:    "segments-resp-short",
			respond: func(sid uint32) [][]byte { return [][]byte{tcpRawFrame(sid, protocol.OpResp, []byte{1, 2})} },
			call:    func(c *Conn) error { _, _, err := c.Segments(ctx); return err },
			wantErr: true,
		},
		{
			name:    "keys-data-malformed",
			respond: func(sid uint32) [][]byte { return [][]byte{tcpRawFrame(sid, protocol.OpKeysData, []byte{1, 2})} },
			call:    func(c *Conn) error { _, err := c.ListKeys(ctx, ""); return err },
			wantErr: true,
		},
		{
			name:    "keys-resp-short",
			respond: func(sid uint32) [][]byte { return [][]byte{tcpRawFrame(sid, protocol.OpResp, []byte{1, 2})} },
			call:    func(c *Conn) error { _, err := c.ListKeys(ctx, ""); return err },
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := tcpScriptServer(t, func(sid uint32, _ protocol.OpCode, _ []byte) [][]byte {
				return tc.respond(sid)
			})
			err := tc.call(tcpDial(t, addr))
			if tc.wantErr && err == nil {
				t.Fatalf("%s: 期望错误，实际 nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%s: 期望成功，实际 %v", tc.name, err)
			}
		})
	}
}

// TestTCPStatsCounters 覆盖 stats.go 的计数分支与文本输出，并验证真往返计数增长。
func TestTCPStatsCounters(t *testing.T) {
	before := [5]int64{
		statRxFrames.Load(), statRxBytes.Load(), statRxData4M.Load(),
		statRxTake.Load(), statRxCopy.Load(),
	}
	recordRxDataFrame(protocol.ChunkSize, true)
	recordRxDataFrame(4, false)
	recordRxDataFrame(protocol.ChunkSize, false)
	after := [5]int64{
		statRxFrames.Load(), statRxBytes.Load(), statRxData4M.Load(),
		statRxTake.Load(), statRxCopy.Load(),
	}
	if got := after[0] - before[0]; got != 3 {
		t.Fatalf("statRxFrames delta = %d, want 3", got)
	}
	if got := after[1] - before[1]; got != 2*protocol.ChunkSize+4 {
		t.Fatalf("statRxBytes delta = %d, want %d", got, 2*protocol.ChunkSize+4)
	}
	if got := after[2] - before[2]; got != 2 {
		t.Fatalf("statRxData4M delta = %d, want 2", got)
	}
	if got := after[3] - before[3]; got != 1 {
		t.Fatalf("statRxTake delta = %d, want 1", got)
	}
	if got := after[4] - before[4]; got != 2 {
		t.Fatalf("statRxCopy delta = %d, want 2", got)
	}

	var b strings.Builder
	DumpStats(&b)
	out := b.String()
	for _, want := range []string{"transport-tx", "transport-rx", "transport-pipeline", "data4MiB="} {
		if !strings.Contains(out, want) {
			t.Fatalf("DumpStats 输出缺少 %q: %q", want, out)
		}
	}
	if s := StatsString(); !strings.Contains(s, "transport-rx") {
		t.Fatalf("StatsString = %q", s)
	}

	// 真往返：4MiB 整帧 → 服务端 tx 数据帧与客户端 rx 数据帧的 4MiB 计数均增长
	addr, _ := tcpServerAndStorage(t)
	c := tcpDial(t, addr)
	ctx := context.Background()
	payload := bytes.Repeat([]byte("x"), protocol.ChunkSize)
	txBefore, rxBefore := statTxData4M.Load(), statRxData4M.Load()
	txFramesBefore, rxFramesBefore := statTxDataFrames.Load(), statRxFrames.Load()
	if err := c.Put(ctx, "tcp/stats", int64(len(payload)), payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, rel, err := c.Get(ctx, "tcp/stats", 0, int64(len(payload)))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("Get: len=%d err=%v", len(got), err)
	}
	rel()
	if statTxData4M.Load() == txBefore {
		t.Fatal("statTxData4M 未增长（服务端未回 4MiB 整帧）")
	}
	if statRxData4M.Load() == rxBefore {
		t.Fatal("statRxData4M 未增长（客户端未收 4MiB 整帧）")
	}
	if statTxDataFrames.Load() == txFramesBefore || statRxFrames.Load() == rxFramesBefore {
		t.Fatal("数据帧计数未增长")
	}
}
