package rpcclient

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// 本文件补齐 pool_test.go / admin_test.go 未覆盖的分支：admin 在不支持 admin 的
// 传输上的错误返回、对端已断连接时的 RPC 错误、DialPoolMulti 的拨号失败与 perAddr
// 下限、Storage.Close 的错误聚合，以及 PutWriter（Reserve/Commit 透传）与 NewPut 的
// 前置校验。全部使用本机 TCP 或进程内测试替身，无外网、无真实 shm/TiKV。

// stubConn 可编程 rpcConn 替身。它**不是** *transport.Conn，因此可用来触发
// adminConn 的「当前传输不支持 admin」分支；closeErr 用于触发 Close 的错误聚合。
type stubConn struct {
	closeErr error
	closed   bool
}

func (c *stubConn) Put(context.Context, string, int64, []byte) error { return nil }

func (c *stubConn) Get(context.Context, string, int64, int64) ([]byte, func(), error) {
	return nil, func() {}, nil
}

func (c *stubConn) Delete(context.Context, string) error { return nil }

func (c *stubConn) Stat(context.Context, string) (int64, error) { return 0, nil }

func (c *stubConn) Close() error { c.closed = true; return c.closeErr }

// TestAdminUnsupportedTransport 验证连接池里没有 *transport.Conn 时，四个 admin
// 接口都返回错误而不是 panic（adminConn 的兜底分支）。
func TestAdminUnsupportedTransport(t *testing.T) {
	s := &Storage{conns: []rpcConn{&stubConn{}}}
	ctx := context.Background()

	if _, _, err := s.Ping(ctx); err == nil {
		t.Fatal("Ping on non-TCP transport should error")
	}
	if _, err := s.Meta(ctx, "k"); err == nil {
		t.Fatal("Meta on non-TCP transport should error")
	}
	if _, _, err := s.Segments(ctx); err == nil {
		t.Fatal("Segments on non-TCP transport should error")
	}
	if _, err := s.ListKeys(ctx, ""); err == nil {
		t.Fatal("ListKeys on non-TCP transport should error")
	}
}

// newClosedPeer 起一个「接受即关闭」的 TCP 监听，返回其地址：Dial 能成功（内核
// 完成三次握手），但随后对端 FIN 会让连接进入已关闭状态，用于触发 RPC 错误分支。
func newClosedPeer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

// TestAdminBrokenConnErrors 验证对端断连后 Ping/Segments 返回错误（而不是静默成功
// 或挂死）。用有界 context 兜底，避免异常情况下长时间阻塞。
func TestAdminBrokenConnErrors(t *testing.T) {
	s, err := Dial(context.Background(), newClosedPeer(t))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, _, err := s.Ping(ctx); err == nil {
		t.Fatal("Ping on closed peer should error")
	}
	if _, _, err := s.Segments(ctx); err == nil {
		t.Fatal("Segments on closed peer should error")
	}
}

// TestStorageCloseAggregatesErrors 验证 Close 跳过 nil 连接、返回**首个**错误、
// 清空 conns，且二次调用幂等。
func TestStorageCloseAggregatesErrors(t *testing.T) {
	first := errors.New("close-1")
	second := errors.New("close-2")
	c1 := &stubConn{closeErr: first}
	c2 := &stubConn{closeErr: second}

	s := &Storage{conns: []rpcConn{c1, nil, c2}}
	if err := s.Close(); err != first {
		t.Fatalf("Close = %v, want 首个错误 %v", err, first)
	}
	if !c1.closed || !c2.closed {
		t.Fatalf("Close 未关到全部连接: c1=%v c2=%v", c1.closed, c2.closed)
	}
	if s.conns != nil {
		t.Fatalf("Close 后 conns 应为 nil，实际 %d 条", len(s.conns))
	}
	// 已清空 → 二次 Close 无错。
	if err := s.Close(); err != nil {
		t.Fatalf("二次 Close = %v, want nil", err)
	}
}

// closedAddr 返回一个「刚刚关闭、必被拒绝」的本机地址（连接拒绝即时返回，不超时）。
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestDialPoolMultiPerAddrFloor 验证 perAddr < 1 被抬升为 1，conns 数量符合预期。
func TestDialPoolMultiPerAddrFloor(t *testing.T) {
	addr, cleanup := newTestServer(t)
	defer cleanup()

	s, err := DialPoolMulti(context.Background(), []string{addr}, 0)
	if err != nil {
		t.Fatalf("DialPoolMulti(perAddr=0): %v", err)
	}
	defer s.Close()
	if len(s.conns) != 1 {
		t.Fatalf("conns = %d, want 1", len(s.conns))
	}
	if err := s.Put(context.Background(), "floor", 8, make([]byte, 8)); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// TestDialPoolMultiDialFailure 验证无可连地址时报错（不返回半成品连接池）。
func TestDialPoolMultiDialFailure(t *testing.T) {
	if _, err := DialPoolMulti(context.Background(), []string{closedAddr(t)}, 1); err == nil {
		t.Fatal("DialPoolMulti to closed listener should error")
	}
}

// TestDialPoolMultiPartialFailure 验证多地址中后一个失败时整体返回错误，且已建连接
// 被关闭回收（不泄漏）。
func TestDialPoolMultiPartialFailure(t *testing.T) {
	addr, cleanup := newTestServer(t)
	defer cleanup()

	if _, err := DialPoolMulti(context.Background(), []string{addr, closedAddr(t)}, 1); err == nil {
		t.Fatal("DialPoolMulti with one bad addr should error")
	}
}

// TestNewPutClosedStorage 验证连接池为空（已 Close）时 NewPut 立刻报错。
func TestNewPutClosedStorage(t *testing.T) {
	var s Storage
	if _, err := s.NewPut(context.Background(), "k", 1); err == nil {
		t.Fatal("NewPut on closed storage should error")
	}
}

// TestNewPutTCPIsShmOnly 验证 TCP 连接上零拷贝写不可用 → ErrShmOnly（shm 专属）。
func TestNewPutTCPIsShmOnly(t *testing.T) {
	addr, cleanup := newTestServer(t)
	defer cleanup()

	s, err := Dial(context.Background(), addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()

	if _, err := s.NewPut(context.Background(), "k", 4096); err != ErrShmOnly {
		t.Fatalf("NewPut on TCP = %v, want ErrShmOnly", err)
	}
}

// stubPutStream 可编程 putStream 替身：PutWriter 只是对底层写流的透传封装，
// 用替身即可覆盖 Reserve/Commit 的成功与错误透传，无需真实共享内存。
type stubPutStream struct {
	n          int
	reserveErr error
	commitErr  error
	committed  bool
}

func (s *stubPutStream) Reserve(n int) ([]byte, error) {
	s.n = n
	if s.reserveErr != nil {
		return nil, s.reserveErr
	}
	return make([]byte, n), nil
}

func (s *stubPutStream) Commit() error {
	s.committed = true
	return s.commitErr
}

// TestPutWriterDelegates 验证 PutWriter 的 Reserve/Commit 把参数与结果原样透传给
// 底层写流（成功与错误两条路径）。
func TestPutWriterDelegates(t *testing.T) {
	ok := &stubPutStream{}
	w := &PutWriter{w: ok}

	buf, err := w.Reserve(4096)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if len(buf) != 4096 || ok.n != 4096 {
		t.Fatalf("Reserve len=%d 透传 n=%d, want 4096", len(buf), ok.n)
	}
	// 零拷贝语义：返回的即底层可写区，可直接写入。
	copy(buf, "taihu")
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !ok.committed {
		t.Fatal("Commit 未透传到底层写流")
	}

	bad := &stubPutStream{reserveErr: errors.New("reserve-fail"), commitErr: errors.New("commit-fail")}
	w2 := &PutWriter{w: bad}
	if _, err := w2.Reserve(8); !errors.Is(err, bad.reserveErr) {
		t.Fatalf("Reserve err = %v, want %v", err, bad.reserveErr)
	}
	if err := w2.Commit(); !errors.Is(err, bad.commitErr) {
		t.Fatalf("Commit err = %v, want %v", err, bad.commitErr)
	}
}
