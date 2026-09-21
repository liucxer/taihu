// Copyright 2022 CloudWeGo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !windows

package netpoll

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/liucxer/taihu/third_party/netpoll/internal/runner"
)

func TestExceptionExtra(t *testing.T) {
	ex := Exception(ErrReadTimeout, "suffix")
	exc, ok := ex.(*exception)
	MustTrue(t, ok)

	// Is: identity, errno equality and delegation to syscall.Errno.Is
	MustTrue(t, exc.Is(ex))
	MustTrue(t, exc.Is(ErrReadTimeout))
	MustTrue(t, errors.Is(ex, ErrReadTimeout))
	Equal(t, Exception(syscall.EACCES, "").(*exception).Is(fs.ErrPermission), syscall.EACCES.Is(fs.ErrPermission))

	// ErrEOF is considered as ErrConnClosed
	eof := Exception(ErrEOF, "").(*exception)
	MustTrue(t, eof.Is(ErrConnClosed))
	MustTrue(t, !eof.Is(ErrReadTimeout))

	// Unwrap returns the underlying errno
	Equal(t, errors.Unwrap(ex), ErrReadTimeout)

	// Timeout / Temporary are delegated to the wrapped errno
	MustTrue(t, exc.Timeout())
	MustTrue(t, Exception(ErrDialTimeout, "").(*exception).Timeout())
	MustTrue(t, Exception(ErrWriteTimeout, "").(*exception).Timeout())
	Equal(t, Exception(syscall.ECONNRESET, "").(*exception).Timeout(), syscall.ECONNRESET.Timeout())
	Equal(t, Exception(syscall.ECONNRESET, "").(*exception).Temporary(), syscall.ECONNRESET.Temporary())

	var ne net.Error = exc
	MustTrue(t, ne.Timeout())
	MustTrue(t, ne.Error() != "")

	// non-errno errors are wrapped only when a suffix is given
	raw := errors.New("raw")
	Equal(t, Exception(raw, ""), raw)
	Equal(t, Exception(raw, "suffix").Error(), "raw suffix")

	// Error() composes errnos and suffix
	Equal(t, ex.Error(), "connection read timeout suffix")
	Equal(t, Exception(syscall.EPIPE, "when flush").Error(), "broken pipe when flush")
	Equal(t, Exception(ErrConnClosed, "").Error(), "connection has been closed")
	Equal(t, Exception(ErrEOF, "").Error(), "EOF")
	Equal(t, Exception(ErrUnsupported, "").Error(), "netpoll does not support")
	Equal(t, Exception(ErrWriteTimeout, "").Error(), "connection write timeout")
	Equal(t, Exception(ErrDialNoDeadline, "").Error(), "dial no deadline")
	Equal(t, Exception(ErrConcurrentAccess, "").Error(), "concurrent connection access")
}

func TestNetFDExtra(t *testing.T) {
	// SetKeepAlive is a no-op for non-TCP connections
	unixFD := &netFD{fd: -1, network: "unix"}
	MustNil(t, unixFD.SetKeepAlive(1))

	// second <= 0 is also a no-op
	tcpFD := &netFD{fd: -1, network: "tcp"}
	MustNil(t, tcpFD.SetKeepAlive(0))

	// deadlines are not supported by netpoll's fd
	MustTrue(t, errors.Is(tcpFD.SetDeadline(time.Now()), ErrUnsupported))
	MustTrue(t, errors.Is(tcpFD.SetReadDeadline(time.Now()), ErrUnsupported))
	MustTrue(t, errors.Is(tcpFD.SetWriteDeadline(time.Now()), ErrUnsupported))

	// SetKeepAlive on a real TCP socket
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	MustNil(t, err)
	defer ln.Close()
	f, err := ln.(*net.TCPListener).File()
	MustNil(t, err)
	defer f.Close()
	MustNil(t, (&netFD{fd: int(f.Fd()), network: "tcp"}).SetKeepAlive(1))

	// Read on a nonblocking socket without pending data returns EAGAIN -> (0, nil)
	r, w := GetSysFdPairs()
	defer syscall.Close(w)
	MustNil(t, syscall.SetNonblock(r, true))
	rfd := &netFD{fd: r}
	sbuf := make([]byte, 8)
	n, err := rfd.Read(sbuf)
	MustNil(t, err)
	Equal(t, n, 0)

	// Write on a nonblocking socket whose peer never reads returns EAGAIN -> (0, nil)
	MustNil(t, syscall.SetNonblock(w, true))
	wfd := &netFD{fd: w}
	chunk := make([]byte, block32k)
	var blocked bool
	for i := 0; i < 1024 && !blocked; i++ {
		n, err = wfd.Write(chunk)
		MustNil(t, err)
		blocked = n == 0
	}
	Assert(t, blocked, "expected EAGAIN on a full socket buffer")

	// Close is idempotent
	MustNil(t, rfd.Close())
	MustNil(t, rfd.Close())

	// Close reports the error returned by the underlying syscall
	r1, w1 := GetSysFdPairs()
	bad := &netFD{fd: r1}
	Assert(t, bad.fd > 2)
	MustNil(t, syscall.Close(r1))
	MustNil(t, syscall.Close(w1))
	MustTrue(t, bad.Close() != nil)
}

func TestListenerExtra(t *testing.T) {
	// UDP is not supported
	for _, network := range []string{"udp", "udp4", "udp6"} {
		_, err := CreateListener(network, "127.0.0.1:0")
		MustTrue(t, errors.Is(err, ErrUnsupported))
	}

	// an invalid address propagates the error of net.Listen
	_, err := CreateListener("tcp", "256.256.256.256:0")
	MustTrue(t, err != nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	MustNil(t, err)
	nln, err := ConvertListener(ln)
	MustNil(t, err)
	defer nln.Close()
	MustTrue(t, nln.Addr() != nil)
	Equal(t, nln.Addr().String(), ln.Addr().String())
	Equal(t, nln.Fd(), nln.(*listener).fd)

	// an argument which is already a Listener is returned as-is
	same, err := ConvertListener(nln)
	MustNil(t, err)
	Equal(t, same, nln)

	// unsupported listener type
	_, err = ConvertListener(forkExtraListener{})
	MustTrue(t, err != nil)
}

type forkExtraListener struct{}

func (forkExtraListener) Accept() (net.Conn, error) { return nil, nil }
func (forkExtraListener) Close() error              { return nil }
func (forkExtraListener) Addr() net.Addr            { return &net.TCPAddr{} }

func TestAddrExtra(t *testing.T) {
	// TCPAddr with a nil receiver
	var nilTCP *TCPAddr
	MustTrue(t, nilTCP.isWildcard())
	MustNil(t, nilTCP.opAddr())
	Equal(t, nilTCP.family(), syscall.AF_INET)
	sa, err := nilTCP.sockaddr(syscall.AF_INET)
	MustNil(t, err)
	MustNil(t, sa)

	wildcard := &TCPAddr{TCPAddr: net.TCPAddr{IP: net.IPv4zero, Port: 80}}
	MustTrue(t, wildcard.isWildcard())
	MustTrue(t, wildcard.opAddr() != nil)
	Equal(t, wildcard.family(), syscall.AF_INET)

	v4 := &TCPAddr{TCPAddr: net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 80}}
	MustTrue(t, !v4.isWildcard())
	Equal(t, v4.family(), syscall.AF_INET)
	local := v4.toLocal("tcp").(*TCPAddr)
	MustTrue(t, local.IP.Equal(net.IPv4(127, 0, 0, 1)))
	Equal(t, local.Port, 80)
	Equal(t, v4.toLocal("tcp6").(*TCPAddr).IP.String(), net.IPv6loopback.String())

	Equal(t, loopbackIP("").String(), "127.0.0.1")
	Equal(t, loopbackIP("tcp").String(), "127.0.0.1")
	Equal(t, loopbackIP("tcp6").String(), "::1")

	s4, err := v4.sockaddr(syscall.AF_INET)
	MustNil(t, err)
	Equal(t, s4.(*syscall.SockaddrInet4).Port, 80)

	v6 := &TCPAddr{TCPAddr: net.TCPAddr{IP: net.ParseIP("::1"), Port: 81}}
	Equal(t, v6.family(), syscall.AF_INET6)
	s6, err := v6.sockaddr(syscall.AF_INET6)
	MustNil(t, err)
	Equal(t, s6.(*syscall.SockaddrInet6).Port, 81)

	// ipToSockaddr error branches
	_, err = ipToSockaddr(syscall.AF_INET, net.ParseIP("::1"), 0, "")
	MustTrue(t, err != nil)
	_, err = ipToSockaddr(syscall.AF_INET6, net.IP{1, 2, 3}, 0, "")
	MustTrue(t, err != nil)
	_, err = ipToSockaddr(0x7fff, net.IPv4zero, 0, "")
	MustTrue(t, err != nil)

	// wildcard addresses are replaced by the zero address of the family
	wsa6, err := ipToSockaddr(syscall.AF_INET6, nil, 0, "")
	MustNil(t, err)
	MustTrue(t, wsa6 != nil)
	wsa4, err := ipToSockaddr(syscall.AF_INET, nil, 0, "")
	MustNil(t, err)
	MustTrue(t, wsa4 != nil)

	// ResolveTCPAddr
	ra, err := ResolveTCPAddr("tcp", "127.0.0.1:8080")
	MustNil(t, err)
	Equal(t, ra.Port, 8080)
	MustTrue(t, !ra.isWildcard())
	_, err = ResolveTCPAddr("tcp", "not a valid address")
	MustTrue(t, err != nil)

	// UnixAddr with a nil receiver
	var nilUDS *UnixAddr
	MustTrue(t, nilUDS.isWildcard())
	MustNil(t, nilUDS.opAddr())
	Equal(t, nilUDS.family(), syscall.AF_UNIX)
	sa, err = nilUDS.sockaddr(syscall.AF_UNIX)
	MustNil(t, err)
	MustNil(t, sa)

	uds := &UnixAddr{UnixAddr: net.UnixAddr{Name: "fork-extra.sock", Net: "unix"}}
	MustTrue(t, !uds.isWildcard())
	MustTrue(t, uds.opAddr() != nil)
	Equal(t, uds.family(), syscall.AF_UNIX)
	MustTrue(t, uds.toLocal("unix").(*UnixAddr) == uds)
	su, err := uds.sockaddr(syscall.AF_UNIX)
	MustNil(t, err)
	Equal(t, su.(*syscall.SockaddrUnix).Name, "fork-extra.sock")

	// ResolveUnixAddr
	ru, err := ResolveUnixAddr("unix", "fork-extra.sock")
	MustNil(t, err)
	Equal(t, ru.Name, "fork-extra.sock")
	_, err = ResolveUnixAddr("tcp", "127.0.0.1:1")
	MustTrue(t, err != nil)
}

func TestTimeoutOptionsExtra(t *testing.T) {
	evl, err := NewEventLoop(
		func(ctx context.Context, connection Connection) error { return nil },
		WithReadTimeout(time.Second),
		WithWriteTimeout(2*time.Second),
		WithIdleTimeout(3*time.Second),
	)
	MustNil(t, err)
	el, ok := evl.(*eventLoop)
	MustTrue(t, ok)
	Equal(t, el.opts.readTimeout, time.Second)
	Equal(t, el.opts.writeTimeout, 2*time.Second)
	Equal(t, el.opts.idleTimeout, 3*time.Second)
}

func TestNetpollConfigExtra(t *testing.T) {
	Initialize() // lazily initializes the pollers, safe to call multiple times

	// invalid poller number is rejected
	MustTrue(t, SetNumLoops(0) != nil)

	// a negative load balance is ignored
	MustNil(t, Configure(Config{LoadBalance: -1}))

	origRunner := runner.RunTask
	defer func() { runner.RunTask = origRunner }()

	MustNil(t, Configure(Config{
		PollerNum:    int(pollmanager.numLoops),
		LoadBalance:  RoundRobin,
		LoggerOutput: io.Discard,
		Runner:       func(ctx context.Context, f func()) { f() },
	}))
	MustTrue(t, runner.RunTask != nil)

	MustNil(t, SetNumLoops(int(pollmanager.numLoops)))
	MustNil(t, SetLoadBalance(RoundRobin))
	SetLoggerOutput(io.Discard)
	SetRunner(func(ctx context.Context, f func()) { go f() })
	MustNil(t, DisableGopool())
	MustTrue(t, runner.RunTask != nil)
}

// forkExtraReader implements Reader but not io.Reader.
type forkExtraReader struct{ Reader }

// forkExtraWriter implements Writer but not io.Writer.
type forkExtraWriter struct{ Writer }

// forkExtraReadWriter implements ReadWriter but not io.ReadWriter.
type forkExtraReadWriter struct {
	Reader
	Writer
}

// forkExtraIOReader implements both Reader and io.Reader.
type forkExtraIOReader struct{ Reader }

func (*forkExtraIOReader) Read(p []byte) (int, error) { return 0, io.EOF }

// forkExtraIOWriter implements both Writer and io.Writer.
type forkExtraIOWriter struct{ Writer }

func (*forkExtraIOWriter) Write(p []byte) (int, error) { return len(p), nil }

// forkExtraIOReadWriter implements both ReadWriter and io.ReadWriter.
type forkExtraIOReadWriter struct {
	Reader
	Writer
}

func (*forkExtraIOReadWriter) Read(p []byte) (int, error)  { return 0, io.EOF }
func (*forkExtraIOReadWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestNocopyConstructorsExtra(t *testing.T) {
	mock := &MockIOReadWriter{
		read:  func(p []byte) (int, error) { return 0, io.EOF },
		write: func(p []byte) (int, error) { return len(p), nil },
	}
	MustTrue(t, NewReader(mock) != nil)
	MustTrue(t, NewWriter(mock) != nil)
	MustTrue(t, NewReadWriter(mock) != nil)

	// fast path: the argument already implements the io interface
	ior := &forkExtraIOReader{}
	MustTrue(t, NewIOReader(ior) == ior)
	iow := &forkExtraIOWriter{}
	MustTrue(t, NewIOWriter(iow) == iow)
	iorw := &forkExtraIOReadWriter{}
	MustTrue(t, NewIOReadWriter(iorw) == iorw)

	// slow path: wrap into the io adapter
	MustTrue(t, NewIOReader(&forkExtraReader{}) != nil)
	MustTrue(t, NewIOWriter(&forkExtraWriter{}) != nil)
	MustTrue(t, NewIOReadWriter(&forkExtraReadWriter{}) != nil)

	// the writer adapter is usable
	n, err := NewIOWriter(NewWriter(mock)).Write([]byte("hello"))
	MustNil(t, err)
	Equal(t, n, 5)
}

func TestSetAlignedAllocatorExtra(t *testing.T) {
	var allocs, frees int64
	SetAlignedAllocator(
		func(n int) []byte {
			atomic.AddInt64(&allocs, 1)
			return make([]byte, n)
		},
		func(b []byte) { atomic.AddInt64(&frees, 1) },
	)
	defer SetAlignedAllocator(nil, nil)

	buf := NewLinkBuffer()
	_, err := buf.WriteBinary([]byte("aligned"))
	MustNil(t, err)
	MustNil(t, buf.Flush())
	Equal(t, string(buf.Bytes()), "aligned")
	Assert(t, atomic.LoadInt64(&allocs) > 0, "expected the aligned allocator to be used")

	MustNil(t, buf.Close())
	Assert(t, atomic.LoadInt64(&frees) > 0, "expected buffers to be returned to the aligned allocator")
}

func TestConnectionExtraOps(t *testing.T) {
	r, w := GetSysFdPairs()
	rconn, wconn := &connection{}, &connection{}
	MustNil(t, rconn.init(&netFD{fd: r}, nil))
	MustNil(t, wconn.init(&netFD{fd: w}, nil))

	msg := []byte("hello world")

	// Skip
	n, err := wconn.Write(msg)
	MustNil(t, err)
	MustNil(t, rconn.Skip(n))
	Equal(t, rconn.Len(), 0)

	// ReadString
	n, err = wconn.Write(msg)
	MustNil(t, err)
	s, err := rconn.ReadString(n)
	MustNil(t, err)
	Equal(t, s, string(msg))

	// ReadBinary
	n, err = wconn.Write(msg)
	MustNil(t, err)
	b, err := rconn.ReadBinary(n)
	MustNil(t, err)
	Equal(t, string(b), string(msg))

	// ReadByte
	n, err = wconn.Write([]byte("x"))
	MustNil(t, err)
	Equal(t, n, 1)
	b1, err := rconn.ReadByte()
	MustNil(t, err)
	Equal(t, b1, byte('x'))

	// Slice
	n, err = wconn.Write(msg)
	MustNil(t, err)
	sr, err := rconn.Slice(n)
	MustNil(t, err)
	Equal(t, sr.Len(), n)
	MustNil(t, sr.Release())

	// MallocLen / Malloc / MallocAck
	p, err := wconn.Malloc(4)
	MustNil(t, err)
	Equal(t, wconn.MallocLen(), 4)
	copy(p, []byte("abcd"))
	MustNil(t, wconn.MallocAck(2))
	Equal(t, wconn.MallocLen(), 2)
	MustNil(t, wconn.Flush())
	MustNil(t, rconn.Skip(2))

	// WriteString + WriteByte
	wn, err := wconn.WriteString("str")
	MustNil(t, err)
	Equal(t, wn, 3)
	MustNil(t, wconn.WriteByte('b'))
	MustNil(t, wconn.Flush())
	MustNil(t, rconn.Skip(4))

	// WriteDirect
	MustNil(t, wconn.WriteDirect([]byte("direct"), 0))
	MustNil(t, wconn.Flush())
	MustNil(t, rconn.Skip(6))

	// Append
	tmp := NewLinkBuffer(block1k)
	_, err = tmp.WriteString("tail")
	MustNil(t, err)
	MustNil(t, wconn.Append(tmp))
	MustNil(t, wconn.Flush())
	MustNil(t, rconn.Skip(4))

	// state changes
	bare := &connection{}
	bare.setState(connStateConnected)
	Equal(t, bare.getState(), connState(connStateConnected))

	MustNil(t, wconn.Close())
	for rconn.IsActive() || wconn.IsActive() {
		runtime.Gosched()
	}
}

func TestConnDetach(t *testing.T) {
	ln := createTestTCPListener(t)
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	conn, err := DialConnection("tcp", ln.Addr().String(), time.Second)
	MustNil(t, err)
	tcpConn, ok := conn.(*TCPConnection)
	MustTrue(t, ok)

	fd := tcpConn.Fd()
	Assert(t, fd > 2)

	// Detach detaches the connection from the poller but keeps the fd open
	MustNil(t, tcpConn.Detach())
	MustTrue(t, !tcpConn.IsActive())

	peer := <-accepted
	defer peer.Close()
	MustNil(t, peer.SetReadDeadline(time.Now().Add(5*time.Second)))

	// the detached fd is still usable
	MustNil(t, syscall.SetNonblock(fd, true))
	n, err := syscall.Write(fd, []byte("detach"))
	MustNil(t, err)
	Equal(t, n, 6)

	buf := make([]byte, 6)
	n, err = io.ReadFull(peer, buf)
	MustNil(t, err)
	Equal(t, string(buf), "detach")

	MustNil(t, syscall.Close(fd))
}

func TestServerHupAndFdErrExtra(t *testing.T) {
	// OnHup notifies the quit callback
	var quit error
	s := newServer(nil, &options{}, func(err error) { quit = err })
	MustNil(t, s.OnHup(nil))
	MustTrue(t, quit != nil)

	// isOutOfFdErr only matches EMFILE/ENFILE
	MustTrue(t, isOutOfFdErr(syscall.EMFILE))
	MustTrue(t, isOutOfFdErr(syscall.ENFILE))
	MustTrue(t, !isOutOfFdErr(syscall.EAGAIN))
	MustTrue(t, !isOutOfFdErr(errors.New("not an errno")))

	// mapErr maps the context errors to the historical internal values
	Equal(t, mapErr(context.Canceled), errCanceled)
	Equal(t, mapErr(context.DeadlineExceeded), errIOTimeout)
	Equal(t, mapErr(io.EOF), io.EOF)
}

func TestSetPanicHandlerExtra(t *testing.T) {
	// keep the panic propagating out of the gopool worker
	runner.SetPanicHandler(func(ctx context.Context, v interface{}) {
		panic(v)
	})
}

func TestZCReaderWriterExtra(t *testing.T) {
	rd := &MockIOReadWriter{
		read: func(p []byte) (int, error) {
			return copy(p, []byte("ab\ncdefghij")), nil
		},
	}
	r := newZCReader(rd)
	pk, err := r.Peek(1)
	MustNil(t, err)
	Equal(t, len(pk), 1)
	Equal(t, r.Len(), 11)

	line, err := r.Until('\n')
	MustNil(t, err)
	Equal(t, string(line), "ab\n")

	s, err := r.ReadString(2)
	MustNil(t, err)
	Equal(t, s, "cd")

	bin, err := r.ReadBinary(3)
	MustNil(t, err)
	Equal(t, string(bin), "efg")

	ch, err := r.ReadByte()
	MustNil(t, err)
	Equal(t, ch, byte('h'))

	sub, err := r.Slice(2)
	MustNil(t, err)
	Equal(t, sub.Len(), 2)
	Equal(t, r.Len(), 0)
	MustNil(t, sub.Release())
	MustNil(t, r.Release())

	wr := &MockIOReadWriter{
		write: func(p []byte) (int, error) { return len(p), nil },
	}
	w := newZCWriter(wr)
	n, err := w.WriteString("ab")
	MustNil(t, err)
	Equal(t, n, 2)
	n, err = w.WriteBinary([]byte("cd"))
	MustNil(t, err)
	Equal(t, n, 2)
	MustNil(t, w.WriteByte('e'))
	p, err := w.Malloc(3)
	MustNil(t, err)
	Equal(t, len(p), 3)
	Equal(t, w.MallocLen(), 8)
	MustNil(t, w.MallocAck(8))
	Equal(t, w.MallocLen(), 8)
	MustNil(t, w.WriteDirect([]byte("fg"), 0))
	MustNil(t, w.Append(NewLinkBuffer(block1k)))
}

func TestLinkBufferUntilExtra(t *testing.T) {
	buf := NewLinkBuffer(block1k)
	_, err := buf.WriteString("nodelim")
	MustNil(t, err)
	MustNil(t, buf.Flush())

	// no delimiter in the buffer
	_, err = buf.Until('\n')
	MustTrue(t, errors.Is(err, untilErr))

	_, err = buf.WriteString("a\nb")
	MustNil(t, err)
	MustNil(t, buf.Flush())

	line, err := buf.Until('\n')
	MustNil(t, err)
	Equal(t, string(line), "nodelima\n")

	_, err = buf.Until('\n')
	MustTrue(t, errors.Is(err, untilErr))

	MustTrue(t, buf.memorySize() > 0)
	MustNil(t, buf.Close())

	node := newLinkBufferNode(0)
	MustTrue(t, node.IsEmpty())
	node.Reset()
	MustTrue(t, node.IsEmpty())
	MustNil(t, node.Release())
}

func TestLoadBalanceExtra(t *testing.T) {
	polls := []Poll{pollmanager.Pick()}

	random := newLoadbalance(Random, polls)
	Equal(t, random.LoadBalance(), Random)
	MustTrue(t, random.Pick() != nil)
	random.Rebalance(polls)
	MustTrue(t, random.Pick() != nil)

	round := newLoadbalance(RoundRobin, polls)
	Equal(t, round.LoadBalance(), RoundRobin)
	MustTrue(t, round.Pick() != nil)
	round.Rebalance(polls)
	MustTrue(t, round.Pick() != nil)

	// an unknown method falls back to round-robin
	Equal(t, newLoadbalance(LoadBalance(99), polls).LoadBalance(), RoundRobin)
}

func TestManagerCloseExtra(t *testing.T) {
	// nothing was started, so Close only resets the manager
	m := newManager(1)
	MustNil(t, m.Close())
	Equal(t, len(m.polls), 0)
}

func TestPollTriggerExtra(t *testing.T) {
	p := pollmanager.Pick()
	// the first trigger writes to the eventfd, the following ones are coalesced
	MustNil(t, p.Trigger())
	MustNil(t, p.Trigger())
}

func TestClosedConnectionExtra(t *testing.T) {
	r, w := GetSysFdPairs()
	defer syscall.Close(w)

	conn := &connection{}
	MustNil(t, conn.init(&netFD{
		fd:         r,
		remoteAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
	}, new(options)))
	MustTrue(t, conn.IsActive())
	MustNil(t, conn.Close())
	MustTrue(t, !conn.IsActive())

	closed := func(err error) bool { return errors.Is(err, ErrConnClosed) }
	_, err := conn.Malloc(1)
	MustTrue(t, closed(err))
	MustTrue(t, closed(conn.MallocAck(1)))
	tmp := NewLinkBuffer(block1k)
	_, err = tmp.WriteString("x")
	MustNil(t, err)
	MustTrue(t, closed(conn.Append(tmp)))
	_, err = conn.WriteString("x")
	MustTrue(t, closed(err))
	_, err = conn.WriteBinary([]byte("x"))
	MustTrue(t, closed(err))
	MustTrue(t, closed(conn.WriteDirect([]byte("x"), 0)))
	MustTrue(t, closed(conn.WriteByte('x')))
	MustTrue(t, closed(conn.Flush()))
	_, err = conn.Write([]byte("x"))
	MustTrue(t, closed(err))

	// timeout / deadline setters
	MustNil(t, conn.SetIdleTimeout(0))
	MustNil(t, conn.SetIdleTimeout(time.Second))
	MustNil(t, conn.SetDeadline(time.Time{}))
	MustNil(t, conn.SetReadDeadline(time.Time{}))
	MustNil(t, conn.SetWriteDeadline(time.Time{}))
	MustNil(t, conn.SetDeadline(time.Now()))
	MustNil(t, conn.SetWriteDeadline(time.Now()))

	// an empty read is a no-op even on a closed connection
	n, err := conn.Read(nil)
	MustNil(t, err)
	Equal(t, n, 0)

	// a non-empty read reports the close
	MustNil(t, conn.SetReadDeadline(time.Time{}))
	_, err = conn.Read(make([]byte, 1))
	MustTrue(t, closed(err))

	// an expired read deadline is reported as a read timeout
	MustNil(t, conn.SetReadDeadline(time.Now()))
	_, err = conn.Read(make([]byte, 1))
	MustTrue(t, errors.Is(err, ErrReadTimeout))
}