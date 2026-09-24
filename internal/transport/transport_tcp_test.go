// TCP 数据面端到端测试（真实 storage.Storage + 真实 netpoll server + 127.0.0.1:0）。
// 覆盖 server.go / client.go / server_admin.go / client_admin.go 的正常与错误路径。
// 命名统一 tcp 前缀，避免与 shm 侧测试 helper 重名。
package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/internal/transport/protocol"
	"github.com/liucxer/taihu/pkg/ierr"
)

// tcpNewTestStorage 在 t.TempDir()（TMPDIR=/var/tmp，xfs，支持 O_DIRECT）下建真实 Storage。
func tcpNewTestStorage(t *testing.T) *storage.Storage {
	t.Helper()
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	if err := os.WriteFile(devPath, nil, 0o644); err != nil {
		t.Fatalf("create device: %v", err)
	}
	st, err := storage.NewStorage(context.Background(), filepath.Join(dir, "meta"), devPath,
		layout.Layout{SegmentSizeBytes: layout.DefaultSegmentSizeBytes, SegmentCount: 2048})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// tcpServe 在 127.0.0.1:0 上起 Server（cfg==nil 时用默认配置），返回监听地址。
func tcpServe(t *testing.T, st *storage.Storage, cfg *PipelineConfig) string {
	t.Helper()
	var srv *Server
	if cfg == nil {
		srv = NewServer(st)
	} else {
		srv = NewServerWithOptions(st, *cfg)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = ln.Close()
	})
	return ln.Addr().String()
}

func tcpServerAndStorage(t *testing.T) (string, *storage.Storage) {
	t.Helper()
	st := tcpNewTestStorage(t)
	return tcpServe(t, st, nil), st
}

// tcpDial 建立一条客户端连接（默认配置 server 上），测试结束自动关闭。
func tcpDial(t *testing.T, addr string) *Conn {
	t.Helper()
	c, err := DialClient("tcp", addr)
	if err != nil {
		t.Fatalf("DialClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// tcpRawDial 建立裸 TCP 连接（用于手工构造帧，覆盖客户端不会产生的畸形请求）。
func tcpRawDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// tcpRawFrame 按 [4B len][4B sid][1B op][payload] 编码一帧。
func tcpRawFrame(sid uint32, op protocol.OpCode, payload []byte) []byte {
	b := make([]byte, 4+protocol.FrameHeaderLen+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(protocol.FrameHeaderLen+len(payload)))
	binary.BigEndian.PutUint32(b[4:8], sid)
	b[8] = byte(op)
	copy(b[9:], payload)
	return b
}

func tcpRawWrite(t *testing.T, c net.Conn, sid uint32, op protocol.OpCode, payload []byte) {
	t.Helper()
	if _, err := c.Write(tcpRawFrame(sid, op, payload)); err != nil {
		t.Fatalf("raw write sid=%d op=%d: %v", sid, op, err)
	}
}

// tcpReadFrameRaw 从裸连接读一帧（无 t.Fatal，供 goroutine 内使用）。
func tcpReadFrameRaw(r io.Reader) (uint32, protocol.OpCode, []byte, error) {
	hdr := make([]byte, 4+protocol.FrameHeaderLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[0:4])
	if int(n) < protocol.FrameHeaderLen {
		return 0, 0, nil, fmt.Errorf("bad frame len %d", n)
	}
	payload := make([]byte, int(n)-protocol.FrameHeaderLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, 0, nil, err
	}
	return binary.BigEndian.Uint32(hdr[4:8]), protocol.OpCode(hdr[8]), payload, nil
}

func tcpRawReadFrame(t *testing.T, c net.Conn, timeout time.Duration) (uint32, protocol.OpCode, []byte) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	sid, op, payload, err := tcpReadFrameRaw(c)
	if err != nil {
		t.Fatalf("raw read frame: %v", err)
	}
	return sid, op, payload
}

// tcpAssertConnClosed 断言连接被对端关闭（读到 EOF / RST，而非超时）。
func tcpAssertConnClosed(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	n, err := c.Read(buf)
	if err == nil {
		t.Fatalf("connection still open (read %d bytes)", n)
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("connection not closed (read timeout)")
	}
}

// tcpFakeServer 起一个裸 TCP 假服务端：每个连接交给 handler（handler 负责关闭连接）。
func tcpFakeServer(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handler(conn)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return ln.Addr().String()
}

// tcpScriptServer 起一个按脚本应答的假服务端：respond 返回待发帧（nil = 忽略该请求帧）。
func tcpScriptServer(t *testing.T, respond func(sid uint32, op protocol.OpCode, payload []byte) [][]byte) string {
	t.Helper()
	return tcpFakeServer(t, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		for {
			sid, op, payload, err := tcpReadFrameRaw(conn)
			if err != nil {
				return
			}
			for _, f := range respond(sid, op, payload) {
				if _, err := conn.Write(f); err != nil {
					return
				}
			}
		}
	})
}

// TestTCPRoundTripPutGetDeleteStat 覆盖四个数据面 RPC 的真往返、子区间读取、
// size=-1 读至结尾、越界错误码映射与删除后语义。
func TestTCPRoundTripPutGetDeleteStat(t *testing.T) {
	addr, _ := tcpServerAndStorage(t)
	c := tcpDial(t, addr)
	ctx := context.Background()

	for i, size := range []int{1, 5, 4096, 4096*3 + 7} {
		key := fmt.Sprintf("tcp/rt/%d", i)
		payload := make([]byte, size)
		for j := range payload {
			payload[j] = byte(j*7 + i)
		}
		if err := c.Put(ctx, key, int64(size), payload); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
		got, rel, err := c.Get(ctx, key, 0, int64(size))
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("Get %s: len=%d err=%v", key, len(got), err)
		}
		rel()

		if size >= 8 {
			off, n := int64(3), int64(size-5)
			part, relPart, err := c.Get(ctx, key, off, n)
			if err != nil || !bytes.Equal(part, payload[off:off+n]) {
				t.Fatalf("Get sub %s off=%d n=%d: len=%d err=%v", key, off, n, len(part), err)
			}
			relPart()
		}

		// size=-1：读至结尾（客户端先 Stat 拿总长）
		whole, relWhole, err := c.Get(ctx, key, 0, -1)
		if err != nil || !bytes.Equal(whole, payload) {
			t.Fatalf("Get(-1) %s: len=%d err=%v", key, len(whole), err)
		}
		relWhole()
		relWhole() // release 幂等（重复调用为空操作）

		sz, err := c.Stat(ctx, key)
		if err != nil || sz != int64(size) {
			t.Fatalf("Stat %s: sz=%d err=%v", key, sz, err)
		}
		seg, metaOff, metaSize, err := c.Meta(ctx, key)
		if err != nil || metaSize != int64(size) || metaOff%layout.BlockSize != 0 {
			t.Fatalf("Meta %s: seg=%d off=%d size=%d err=%v", key, seg, metaOff, metaSize, err)
		}

		// 零长读：不发请求即返回空
		if z, relZero, err := c.Get(ctx, key, 0, 0); err != nil || len(z) != 0 {
			t.Fatalf("Get(0) %s: len=%d err=%v", key, len(z), err)
		} else {
			relZero()
		}
		// off 越过对象末尾 → 服务端映射为 invalid range
		if _, _, err := c.Get(ctx, key, int64(size)+1, 1); !errors.Is(err, ierr.ErrInvalidRange) {
			t.Fatalf("Get(off>size) %s err=%v, want ErrInvalidRange", key, err)
		}
		// size=-1 且 off 越过对象末尾 → 客户端 Stat 后算出负长度 → invalid range
		if _, _, err := c.Get(ctx, key, int64(size)+1, -1); !errors.Is(err, ierr.ErrInvalidRange) {
			t.Fatalf("Get(off>size,size=-1) %s err=%v, want ErrInvalidRange", key, err)
		}
		// 请求越过对象末尾（对象内起步）→ 短读
		if size > 1 {
			if _, _, err := c.Get(ctx, key, int64(size-1), 4); err == nil || !strings.Contains(err.Error(), "short read") {
				t.Fatalf("Get(over-end) %s err=%v, want short read", key, err)
			}
		}

		if err := c.Delete(ctx, key); err != nil {
			t.Fatalf("Delete %s: %v", key, err)
		}
		if err := c.Delete(ctx, key); !errors.Is(err, ierr.ErrNotFound) {
			t.Fatalf("Delete(missing) %s err=%v", key, err)
		}
		if _, err := c.Stat(ctx, key); !errors.Is(err, ierr.ErrNotFound) {
			t.Fatalf("Stat(after delete) %s err=%v", key, err)
		}
		if _, _, err := c.Get(ctx, key, 0, 4); !errors.Is(err, ierr.ErrNotFound) {
			t.Fatalf("Get(after delete) %s err=%v", key, err)
		}
		// size=-1 时需先 Stat：key 不存在 → Stat 的错误直接上抛
		if _, _, err := c.Get(ctx, key, 0, -1); !errors.Is(err, ierr.ErrNotFound) {
			t.Fatalf("Get(size=-1, missing) %s err=%v, want ErrNotFound", key, err)
		}
		if _, _, _, err := c.Meta(ctx, key); !errors.Is(err, ierr.ErrNotFound) {
			t.Fatalf("Meta(after delete) %s err=%v", key, err)
		}
	}

	// off == size：服务端 ReadAt 返回 (nil, EOF) → 空 final 帧 → 客户端报短读（不挂死）
	key := "tcp/rt/short"
	if err := c.Put(ctx, key, 4, []byte("abcd")); err != nil {
		t.Fatalf("Put short: %v", err)
	}
	if _, _, err := c.Get(ctx, key, 4, 1); err == nil || !strings.Contains(err.Error(), "short read") {
		t.Fatalf("Get(off==size) err=%v, want short read", err)
	}
}

// TestTCPMultiFrameRoundTrip 覆盖跨帧 Put/Get（含恰一帧 4MiB 的零拷贝候选路径）。
func TestTCPMultiFrameRoundTrip(t *testing.T) {
	addr, _ := tcpServerAndStorage(t)
	c := tcpDial(t, addr)
	ctx := context.Background()

	for _, size := range []int64{protocol.ChunkSize, protocol.ChunkSize + 1, 3*protocol.ChunkSize + 999} {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i * 31)
		}
		key := fmt.Sprintf("tcp/multi/%d", size)
		if err := c.Put(ctx, key, size, payload); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
		got, rel, err := c.Get(ctx, key, 0, size)
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		if int64(len(got)) != size {
			t.Fatalf("Get %s: len=%d want %d", key, len(got), size)
		}
		if sha256.Sum256(got) != sha256.Sum256(payload) {
			t.Fatalf("Get %s: sha256 mismatch", key)
		}
		rel()

		if sz, err := c.Stat(ctx, key); err != nil || sz != size {
			t.Fatalf("Stat %s: sz=%d err=%v", key, sz, err)
		}
		// 跨帧子区间（起点落在首帧尾部、跨越帧边界）
		if size >= 2*protocol.ChunkSize {
			off, n := int64(protocol.ChunkSize-3), int64(protocol.ChunkSize+11)
			part, relPart, err := c.Get(ctx, key, off, n)
			if err != nil || !bytes.Equal(part, payload[off:off+n]) {
				t.Fatalf("Get sub %s: len=%d err=%v", key, len(part), err)
			}
			relPart()
		}
	}
}

// TestTCPPutValidation 覆盖 Put 的长度校验（客户端短路 + 服务端错误码）。
func TestTCPPutValidation(t *testing.T) {
	addr, st := tcpServerAndStorage(t)
	c := tcpDial(t, addr)
	ctx := context.Background()

	// 客户端侧：in 短于 size，不发请求
	if err := c.Put(ctx, "tcp/short-in", 8, []byte("1234")); !errors.Is(err, ierr.ErrShortWrite) {
		t.Fatalf("Put(short in) err=%v, want ErrShortWrite", err)
	}

	// size == 0 合法
	if err := c.Put(ctx, "tcp/zero", 0, nil); err != nil {
		t.Fatalf("Put(0): %v", err)
	}
	if sz, err := st.Stat(ctx, "tcp/zero"); err != nil || sz != 0 {
		t.Fatalf("Stat(tcp/zero) = %d, %v", sz, err)
	}

	cases := []struct {
		name    string
		payload []byte
		want    protocol.ErrCode
	}{
		{
			name:    "size 为负",
			payload: protocol.EncodePutHeader("k", -1),
			want:    protocol.CodeInvalidArgument,
		},
		{
			name:    "size 超过单段上限",
			payload: protocol.EncodePutHeader("k", st.MaxObjectSize()+1),
			want:    protocol.CodeTooLarge,
		},
		{
			name:    "头畸形",
			payload: []byte{0, 0},
			want:    protocol.CodeInvalidArgument,
		},
		{
			name:    "key 过长",
			payload: protocol.EncodePutHeader(strings.Repeat("k", protocol.MaxKeyLen+1), 4),
			want:    protocol.CodeInvalidArgument,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tcpRawDial(t, addr)
			tcpRawWrite(t, raw, 1, protocol.OpPutHeader, tc.payload)
			sid, op, p := tcpRawReadFrame(t, raw, 3*time.Second)
			if sid != 1 || op != protocol.OpResp || len(p) != 4 {
				t.Fatalf("resp sid=%d op=%d len=%d", sid, op, len(p))
			}
			if got := protocol.ErrCode(binary.BigEndian.Uint32(p)); got != tc.want {
				t.Fatalf("code=%d want %d", got, tc.want)
			}
		})
	}

	// 数据帧超出声明 size
	t.Run("数据超出声明长度", func(t *testing.T) {
		raw := tcpRawDial(t, addr)
		tcpRawWrite(t, raw, 2, protocol.OpPutHeader, protocol.EncodePutHeader("tcp/over", 4))
		tcpRawWrite(t, raw, 2, protocol.OpPutData, []byte("01234567"))
		_, op, p := tcpRawReadFrame(t, raw, 3*time.Second)
		if op != protocol.OpResp || protocol.ErrCode(binary.BigEndian.Uint32(p)) != protocol.CodeInvalidArgument {
			t.Fatalf("over-data resp op=%d p=%v", op, p)
		}
	})

	// PutEnd 时累计字节数不足
	t.Run("数据不足声明长度", func(t *testing.T) {
		raw := tcpRawDial(t, addr)
		tcpRawWrite(t, raw, 3, protocol.OpPutHeader, protocol.EncodePutHeader("tcp/under", 8))
		tcpRawWrite(t, raw, 3, protocol.OpPutData, []byte("0123"))
		tcpRawWrite(t, raw, 3, protocol.OpPutEnd, nil)
		_, op, p := tcpRawReadFrame(t, raw, 3*time.Second)
		if op != protocol.OpResp || protocol.ErrCode(binary.BigEndian.Uint32(p)) != protocol.CodeInvalidArgument {
			t.Fatalf("under-data resp op=%d p=%v", op, p)
		}
	})

	// 数据阶段收到非数据帧
	t.Run("数据阶段op非法", func(t *testing.T) {
		raw := tcpRawDial(t, addr)
		tcpRawWrite(t, raw, 4, protocol.OpPutHeader, protocol.EncodePutHeader("tcp/badop", 8))
		tcpRawWrite(t, raw, 4, protocol.OpDelReq, protocol.EncodeKeyReq("tcp/badop"))
		_, op, p := tcpRawReadFrame(t, raw, 3*time.Second)
		if op != protocol.OpResp || protocol.ErrCode(binary.BigEndian.Uint32(p)) != protocol.CodeInvalidArgument {
			t.Fatalf("bad-op resp op=%d p=%v", op, p)
		}
	})

	// 中途断开（handler 在第二个 await 上被 closeAll 唤醒）
	t.Run("数据阶段断开", func(t *testing.T) {
		raw := tcpRawDial(t, addr)
		tcpRawWrite(t, raw, 5, protocol.OpPutHeader, protocol.EncodePutHeader("tcp/abort", 4096))
		_ = raw.Close()
		if _, err := c.Ping(context.Background()); err != nil {
			t.Fatalf("server unusable after peer abort: %v", err)
		}
	})
}

// TestTCPPutNoSpace 覆盖服务端 PutBegin 失败时的 ErrNoSpace 错误码映射
// （小布局：单段 4KiB、3 段，第二段起不参与用户写路径的顺序滚动且无空闲段）。
func TestTCPPutNoSpace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	if err := os.WriteFile(devPath, nil, 0o644); err != nil {
		t.Fatalf("create device: %v", err)
	}
	st, err := storage.NewStorage(ctx, filepath.Join(dir, "meta"), devPath,
		layout.Layout{SegmentSizeBytes: 2 * layout.BlockSize, SegmentCount: 3})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	addr := tcpServe(t, st, nil)
	c := tcpDial(t, addr)

	// 小布局：写入若干 4KiB 对象直至段耗尽（游标到顶且无空闲段）→ 服务端 PutBegin 失败
	var putErr error
	written := 0
	for i := 0; i < 4 && putErr == nil; i++ {
		putErr = c.Put(ctx, fmt.Sprintf("tcp/nospace/%d", i), layout.BlockSize, make([]byte, layout.BlockSize))
		if putErr == nil {
			written++
		}
	}
	if !errors.Is(putErr, ierr.ErrNoSpace) {
		t.Fatalf("Put(段耗尽) err=%v, want ErrNoSpace", putErr)
	}
	if _, err := c.Stat(ctx, fmt.Sprintf("tcp/nospace/%d", written)); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("Stat(未写入) err=%v, want ErrNotFound", err)
	}
}

// TestTCPPutNoSpaceDrainsTail 覆盖「服务端在 PutHeader 之后提前失败时必须排空客户端
// 已在途的尾部帧」，否则残留 PutData/OpPutEnd 会被 dispatch 判为未知流非首帧
// （deliverFatal）而关闭整条连接——连接一死，其上后续所有 RPC 全部报
// "connection has been closed when write binary"。
//
// 触发与否取决于「尾帧到达 vs 处理器结束」的竞态，而该竞态由尾帧长度决定：小对象
// （如 TestTCPPutNoSpace 的 4KiB）三帧能一次性从收流缓冲切出（纯用户态、不让出 P），
// 在处理器 goroutine 被调度前就进了流缓冲，故不触发（假阴性）；大对象读循环必须阻塞
// 等整帧到齐、让出 P，处理器先跑完并注销流，尾帧才必然落到 fatal 分支。因此本用例
// 必须覆盖大对象。
func TestTCPPutNoSpaceDrainsTail(t *testing.T) {
	ctx := context.Background()
	for _, objSize := range []int{int(layout.BlockSize), 1 << 20, 4 << 20} {
		objSize := objSize
		t.Run(fmt.Sprintf("obj-%d", objSize), func(t *testing.T) {
			dir := t.TempDir()
			devPath := filepath.Join(dir, "nvme.img")
			if err := os.WriteFile(devPath, nil, 0o644); err != nil {
				t.Fatalf("create device: %v", err)
			}
			// 8MiB×3 段：reserveSegs=2 ⇒ 用户顺序滚动只覆盖 0 号段（写满即 ErrNoSpace），
			// 且段大小 > 4MiB，使大对象能通过 MaxObjectSize 检查、真正走到 PutBegin。
			st, err := storage.NewStorage(ctx, filepath.Join(dir, "meta"), devPath,
				layout.Layout{SegmentSizeBytes: 8 << 20, SegmentCount: 3})
			if err != nil {
				t.Fatalf("NewStorage: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			c := tcpDial(t, tcpServe(t, st, nil))

			// 用 4KiB 对象写满盘
			var putErr error
			for i := 0; i < 3000 && putErr == nil; i++ {
				putErr = c.Put(ctx, fmt.Sprintf("tcp/nospace/fill/%d", i), layout.BlockSize, make([]byte, layout.BlockSize))
			}
			if !errors.Is(putErr, ierr.ErrNoSpace) {
				t.Fatalf("写满失败: err=%v, want ErrNoSpace", putErr)
			}

			// 满盘后再发一个对象：期望 ErrNoSpace，且连接必须保持可用
			if err := c.Put(ctx, "tcp/nospace/tail", int64(objSize), make([]byte, objSize)); !errors.Is(err, ierr.ErrNoSpace) {
				t.Fatalf("Put(满盘,%d字节) err=%v, want ErrNoSpace", objSize, err)
			}
			if _, err := c.Stat(ctx, "tcp/nospace/absent"); !errors.Is(err, ierr.ErrNotFound) {
				t.Fatalf("ErrNoSpace 后连接不可用: Stat err=%v, want ErrNotFound", err)
			}
		})
	}
}

// TestTCPPutZeroSizeKeepsConn 覆盖零长 Put：客户端仍会发 OpPutEnd 收尾帧，
// 服务端在 size==0 分支提前返回时若不消费它，该帧在流注销后到达即被判协议错误而关连接。
// 用裸连接手工控制时序（读到响应后再等一拍才发 OpPutEnd），使收尾帧必然晚于处理器结束，
// 避免小帧「一次性进缓冲」的假阴性。
func TestTCPPutZeroSizeKeepsConn(t *testing.T) {
	addr, _ := tcpServerAndStorage(t)
	raw := tcpRawDial(t, addr)
	tcpRawWrite(t, raw, 1, protocol.OpPutHeader, protocol.EncodePutHeader("tcp/zero/raw", 0))
	_, op, p := tcpRawReadFrame(t, raw, 3*time.Second)
	if op != protocol.OpResp || protocol.ErrCode(binary.BigEndian.Uint32(p)) != protocol.CodeOK {
		t.Fatalf("零长 Put 响应 op=%d p=%v", op, p)
	}
	time.Sleep(200 * time.Millisecond) // 等处理器结束并注销流
	tcpRawWrite(t, raw, 1, protocol.OpPutEnd, nil)
	// 连接必须仍可用：Ping 应得到 OpPong。注意换一个 streamID——复用刚注销的 sid 会与
	// 流注销竞态（真实客户端 sid 单调递增、从不复用）。
	tcpRawWrite(t, raw, 2, protocol.OpPing, nil)
	if _, op, p = tcpRawReadFrame(t, raw, 3*time.Second); op != protocol.OpPong {
		t.Fatalf("OpPutEnd 后连接已不可用: op=%d p=%v", op, p)
	}
}

// TestTCPRawProtocolFatal 覆盖读循环/分发层的连接级协议错误（deliverFatal）。
func TestTCPRawProtocolFatal(t *testing.T) {
	addr, _ := tcpServerAndStorage(t)

	t.Run("长度字段过小", func(t *testing.T) {
		c := tcpRawDial(t, addr)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], protocol.FrameHeaderLen-2)
		if _, err := c.Write(b[:]); err != nil {
			t.Fatalf("write: %v", err)
		}
		tcpAssertConnClosed(t, c)
	})

	t.Run("长度字段超上限", func(t *testing.T) {
		c := tcpRawDial(t, addr)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], protocol.MaxFrameTotal+1)
		if _, err := c.Write(b[:]); err != nil {
			t.Fatalf("write: %v", err)
		}
		tcpAssertConnClosed(t, c)
	})

	t.Run("未知流首帧非起始op", func(t *testing.T) {
		c := tcpRawDial(t, addr)
		tcpRawWrite(t, c, 7, protocol.OpResp, protocol.EncCode(protocol.CodeOK))
		tcpAssertConnClosed(t, c)
	})
}

// TestTCPRawGetAndKeyReqErrors 覆盖 Get/Delete/Stat/Meta/ListKeys 请求解析与边界错误码。
func TestTCPRawGetAndKeyReqErrors(t *testing.T) {
	addr, _ := tcpServerAndStorage(t)
	c := tcpDial(t, addr)
	ctx := context.Background()
	if err := c.Put(ctx, "tcp/raw/key", 8, []byte("01234567")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	t.Run("Get size=-1 且 key 不存在", func(t *testing.T) {
		raw := tcpRawDial(t, addr)
		tcpRawWrite(t, raw, 1, protocol.OpGetReq, protocol.EncodeGetReq("tcp/raw/missing", 0, -1))
		_, op, p := tcpRawReadFrame(t, raw, 3*time.Second)
		if op != protocol.OpGetErr || protocol.ErrCode(binary.BigEndian.Uint32(p)) != protocol.CodeNotFound {
			t.Fatalf("resp op=%d p=%v", op, p)
		}
	})

	t.Run("Get size=-1 合法", func(t *testing.T) {
		raw := tcpRawDial(t, addr)
		tcpRawWrite(t, raw, 1, protocol.OpGetReq, protocol.EncodeGetReq("tcp/raw/key", 0, -1))
		var (
			got    []byte
			frames int
		)
		for {
			_, op, p := tcpRawReadFrame(t, raw, 3*time.Second)
			frames++
			got = append(got, p...)
			if op == protocol.OpGetDataFinal || op == protocol.OpGetErr {
				break
			}
			if frames > 8 {
				t.Fatalf("too many frames")
			}
		}
		if !bytes.Equal(got, []byte("01234567")) {
			t.Fatalf("data=%q want 01234567", got)
		}
	})

	t.Run("Get size 为其他负值", func(t *testing.T) {
		raw := tcpRawDial(t, addr)
		tcpRawWrite(t, raw, 1, protocol.OpGetReq, protocol.EncodeGetReq("tcp/raw/key", 0, -5))
		_, op, p := tcpRawReadFrame(t, raw, 3*time.Second)
		if op != protocol.OpGetErr || protocol.ErrCode(binary.BigEndian.Uint32(p)) != protocol.CodeInvalidRange {
			t.Fatalf("resp op=%d p=%v", op, p)
		}
	})

	t.Run("Get 请求头畸形", func(t *testing.T) {
		raw := tcpRawDial(t, addr)
		tcpRawWrite(t, raw, 1, protocol.OpGetReq, []byte{0, 0, 0})
		_, op, p := tcpRawReadFrame(t, raw, 3*time.Second)
		if op != protocol.OpGetErr || protocol.ErrCode(binary.BigEndian.Uint32(p)) != protocol.CodeInvalidArgument {
			t.Fatalf("resp op=%d p=%v", op, p)
		}
	})

	// Delete/Stat/Meta/ListKeys 的 key 解析错误
	for _, tc := range []struct {
		name string
		op   protocol.OpCode
		want protocol.OpCode
	}{
		{"Delete key 过长", protocol.OpDelReq, protocol.OpResp},
		{"Stat key 过长", protocol.OpStatReq, protocol.OpResp},
		{"Meta key 过长", protocol.OpMetaReq, protocol.OpResp},
		{"ListKeys 前缀过长", protocol.OpKeysReq, protocol.OpResp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tcpRawDial(t, addr)
			tcpRawWrite(t, raw, 1, tc.op, protocol.EncodeKeyReq(strings.Repeat("k", protocol.MaxKeyLen+1)))
			_, op, p := tcpRawReadFrame(t, raw, 3*time.Second)
			if op != tc.want || protocol.ErrCode(binary.BigEndian.Uint32(p)) != protocol.CodeInvalidArgument {
				t.Fatalf("resp op=%d p=%v", op, p)
			}
		})
	}

	t.Run("Stat 与 Meta 成功响应", func(t *testing.T) {
		raw := tcpRawDial(t, addr)
		tcpRawWrite(t, raw, 1, protocol.OpStatReq, protocol.EncodeKeyReq("tcp/raw/key"))
		_, op, p := tcpRawReadFrame(t, raw, 3*time.Second)
		if op != protocol.OpStatResp || binary.BigEndian.Uint64(p) != 8 {
			t.Fatalf("stat resp op=%d p=%v", op, p)
		}
		raw2 := tcpRawDial(t, addr)
		tcpRawWrite(t, raw2, 1, protocol.OpMetaReq, protocol.EncodeKeyReq("tcp/raw/key"))
		_, op2, p2 := tcpRawReadFrame(t, raw2, 3*time.Second)
		if op2 != protocol.OpMetaResp || len(p2) != 24 || binary.BigEndian.Uint64(p2[16:]) != 8 {
			t.Fatalf("meta resp op=%d p=%v", op2, p2)
		}
	})

	// 各一元处理器：请求发出后立即断开 → await 被 closeAll 唤醒后返回
	for _, op := range []protocol.OpCode{
		protocol.OpGetReq, protocol.OpDelReq, protocol.OpStatReq,
		protocol.OpPing, protocol.OpMetaReq, protocol.OpSegReq, protocol.OpKeysReq,
	} {
		raw := tcpRawDial(t, addr)
		var payload []byte
		switch op {
		case protocol.OpGetReq:
			payload = protocol.EncodeGetReq("tcp/raw/key", 0, 4)
		case protocol.OpDelReq, protocol.OpStatReq, protocol.OpMetaReq:
			payload = protocol.EncodeKeyReq("tcp/raw/key")
		}
		tcpRawWrite(t, raw, 1, op, payload)
		_ = raw.Close()
	}
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("server unusable after aborted unary requests: %v", err)
	}
}

// TestTCPConcurrentStreams 覆盖同连接多流并发复用（流 ID 分配/回收、乱序响应）。
func TestTCPConcurrentStreams(t *testing.T) {
	addr, _ := tcpServerAndStorage(t)
	c := tcpDial(t, addr)

	const workers, per = 6, 8
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < per; i++ {
				key := fmt.Sprintf("tcp/conc/%d/%d", w, i)
				payload := bytes.Repeat([]byte{byte('a' + w)}, 700+i*13)
				if err := c.Put(ctx, key, int64(len(payload)), payload); err != nil {
					errCh <- fmt.Errorf("Put %s: %w", key, err)
					return
				}
				got, rel, err := c.Get(ctx, key, 0, int64(len(payload)))
				if err != nil {
					errCh <- fmt.Errorf("Get %s: %w", key, err)
					return
				}
				if !bytes.Equal(got, payload) {
					rel()
					errCh <- fmt.Errorf("Get %s: data mismatch", key)
					return
				}
				rel()
				if sz, err := c.Stat(ctx, key); err != nil || sz != int64(len(payload)) {
					errCh <- fmt.Errorf("Stat %s: %d, %w", key, sz, err)
					return
				}
				if err := c.Delete(ctx, key); err != nil {
					errCh <- fmt.Errorf("Delete %s: %w", key, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if n := len(c.streams); n != 0 {
		t.Fatalf("streams leaked: %d", n)
	}
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after concurrent load: %v", err)
	}
}

// TestTCPContextCancelAndTimeout 覆盖 await 的 ctx.Done 分支（主动取消与超时）。
func TestTCPContextCancelAndTimeout(t *testing.T) {
	addr, _ := tcpServerAndStorage(t)
	c := tcpDial(t, addr)

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Stat(cancelCtx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stat canceled err=%v", err)
	}
	if err := c.Delete(cancelCtx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete canceled err=%v", err)
	}
	if err := c.Put(cancelCtx, "k", 1, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put canceled err=%v", err)
	}
	if _, err := c.Ping(cancelCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ping canceled err=%v", err)
	}
	if _, _, _, err := c.Meta(cancelCtx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Meta canceled err=%v", err)
	}
	if _, _, err := c.Segments(cancelCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Segments canceled err=%v", err)
	}
	if _, err := c.ListKeys(cancelCtx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListKeys canceled err=%v", err)
	}
	if _, _, err := c.Get(cancelCtx, "k", 0, 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get canceled err=%v", err)
	}

	// 只读不答的假服务端 → await 超时
	addr2 := tcpFakeServer(t, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(io.Discard, conn)
	})
	c2, err := DialClient("tcp", addr2)
	if err != nil {
		t.Fatalf("DialClient(fake): %v", err)
	}
	defer func() { _ = c2.Close() }()
	ctx, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	start := time.Now()
	if _, err := c2.Ping(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ping timeout err=%v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %v", d)
	}
}

// TestTCPAdminRoundTrip 覆盖 Ping/Meta/Segments/ListKeys 四个管理接口。
func TestTCPAdminRoundTrip(t *testing.T) {
	addr, st := tcpServerAndStorage(t)
	c := tcpDial(t, addr)
	ctx := context.Background()

	lo := time.Now().Add(-time.Minute).UnixNano()
	srvTime, err := c.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if hi := time.Now().Add(time.Minute).UnixNano(); srvTime < lo || srvTime > hi {
		t.Fatalf("Ping time %d out of [%d,%d]", srvTime, lo, hi)
	}

	payload := []byte("admin-payload")
	if err := c.Put(ctx, "admin/k1", int64(len(payload)), payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Put(ctx, "admin/sub/a", 0, nil); err != nil {
		t.Fatalf("Put(zero): %v", err)
	}
	if err := c.Put(ctx, "other/k", 0, nil); err != nil {
		t.Fatalf("Put(other): %v", err)
	}

	seg, off, size, err := c.Meta(ctx, "admin/k1")
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	want, err := st.ObjectMeta(ctx, "admin/k1")
	if err != nil {
		t.Fatalf("ObjectMeta: %v", err)
	}
	if seg != want.SegmentID || off != want.Offset || size != want.Size || size != int64(len(payload)) {
		t.Fatalf("Meta = (%d,%d,%d), want (%d,%d,%d)", seg, off, size,
			want.SegmentID, want.Offset, want.Size)
	}
	if _, _, _, err := c.Meta(ctx, "admin/missing"); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("Meta(missing) err=%v", err)
	}

	sum, entries, err := c.Segments(ctx)
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}
	wantSum, wantEntries, err := st.Segments(ctx)
	if err != nil {
		t.Fatalf("storage.Segments: %v", err)
	}
	if sum.Total != wantSum.Total || sum.Free != wantSum.Free || sum.Active != wantSum.Active ||
		sum.SegSize != wantSum.SegSize || sum.CursorSeg != wantSum.CursorSeg ||
		sum.CursorOff != wantSum.CursorOff || sum.ObjectCount != wantSum.ObjectCount {
		t.Fatalf("Segments sum = %+v, want %+v", sum, wantSum)
	}
	if len(entries) != len(wantEntries) {
		t.Fatalf("Segments entries = %d, want %d", len(entries), len(wantEntries))
	}
	if len(entries) > 0 && entries[0] != (protocol.SegmentEntry{
		SegmentID:  wantEntries[0].SegmentID,
		State:      uint8(wantEntries[0].State),
		AliveCount: wantEntries[0].AliveCount,
		ReclaimSeq: wantEntries[0].ReclaimSeq,
	}) {
		t.Fatalf("Segments entry[0] = %+v", entries[0])
	}

	keys, err := c.ListKeys(ctx, "admin/")
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "admin/k1" || keys[1] != "admin/sub/a" {
		t.Fatalf("ListKeys(admin/) = %v", keys)
	}
	all, err := c.ListKeys(ctx, "")
	if err != nil {
		t.Fatalf("ListKeys(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListKeys(all) = %v", all)
	}
	none, err := c.ListKeys(ctx, "nomatch/")
	if err != nil || len(none) != 0 {
		t.Fatalf("ListKeys(nomatch) = %v, %v", none, err)
	}
}

// TestTCPListKeysMultiFrame 用长 key 撑爆单帧容量，覆盖 handleListKeys 的多帧下发与
// 客户端多帧聚合（ChunkSize=4MiB，key≈60KB → 需 2 帧）。
func TestTCPListKeysMultiFrame(t *testing.T) {
	addr, _ := tcpServerAndStorage(t)
	c := tcpDial(t, addr)
	ctx := context.Background()

	const (
		count  = 80
		keyLen = 60000
		prefix = "tcp/long/"
		suffix = "0123456789"
	)
	want := make([]string, 0, count)
	for i := 0; i < count; i++ {
		pad := strings.Repeat("p", keyLen-len(prefix)-2-len(suffix))
		key := fmt.Sprintf("%s%02d%s", prefix, i, pad+suffix)
		want = append(want, key)
		if err := c.Put(ctx, key, 0, nil); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	keys, err := c.ListKeys(ctx, prefix)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != count {
		t.Fatalf("ListKeys = %d keys, want %d", len(keys), count)
	}
	for _, k := range keys {
		if len(k) != keyLen {
			t.Fatalf("key len = %d, want %d", len(k), keyLen)
		}
	}
}

// TestTCPServerLifecycle 覆盖 Serve/GracefulStop/Stop 的幂等与停机语义。
func TestTCPServerLifecycle(t *testing.T) {
	st := tcpNewTestStorage(t)

	// 未 Serve 时停机是空操作
	NewServer(st).GracefulStop()
	NewServer(st).Stop()

	// 对已关闭 listener Serve → 返回错误
	t.Run("Serve 已关闭 listener", func(t *testing.T) {
		srv := NewServer(st)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		_ = ln.Close()
		done := make(chan error, 1)
		go func() { done <- srv.Serve(ln) }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("Serve on closed listener should return error")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Serve on closed listener did not return")
		}
		srv.Stop()
	})

	t.Run("停机幂等", func(t *testing.T) {
		st2 := tcpNewTestStorage(t)
		srv := NewServer(st2)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		go func() { _ = srv.Serve(ln) }()
		addr := ln.Addr().String()
		c, err := DialClient("tcp", addr)
		if err != nil {
			t.Fatalf("DialClient: %v", err)
		}
		if _, err := c.Ping(context.Background()); err != nil {
			t.Fatalf("Ping before stop: %v", err)
		}
		// 先关客户端连接，使 Shutdown 无需等待在途连接
		_ = c.Close()
		srv.GracefulStop()
		srv.Stop()
		srv.GracefulStop()
		// 停机后 listener 已关闭：新连接被拒绝
		if cc, err := DialClient("tcp", addr); err == nil {
			_ = cc.Close()
			t.Fatal("停机后不应再接受新连接")
		}
		_ = ln.Close()
	})
}

// TestTCPDialClientError 覆盖 DialClient 拨号失败分支。
func TestTCPDialClientError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if c, err := DialClient("tcp", addr); err == nil {
		_ = c.Close()
		t.Fatal("DialClient to closed port should error")
	}
}
