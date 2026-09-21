/*
 * Copyright 2023 CloudWeGo Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package shmipc

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// testConn / testUdsConn build a connected unix-socket pair. The socket file
// lives under t.TempDir() so that concurrent test binaries never collide and
// the file is removed by the testing framework.
func testConn(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	return testUdsConn(t)
}

func testUdsConn(t *testing.T) (client *net.UnixConn, server *net.UnixConn) {
	t.Helper()

	udsPath := filepath.Join(t.TempDir(), "shmipc.sock")
	addr := &net.UnixAddr{Name: udsPath, Net: "unix"}

	readyCh := make(chan struct{})
	serverCh := make(chan *net.UnixConn, 1)
	errCh := make(chan error, 1)

	go func() {
		ln, err := net.ListenUnix("unix", addr)
		if err != nil {
			errCh <- fmt.Errorf("create listener failed:%w", err)
			close(readyCh)
			return
		}
		close(readyCh)
		s, err := ln.AcceptUnix()
		_ = ln.Close()
		if err != nil {
			errCh <- fmt.Errorf("accept conn failed:%w", err)
			return
		}
		serverCh <- s
	}()

	<-readyCh
	select {
	case err := <-errCh:
		t.Fatalf("testUdsConn failed:%s", err.Error())
	default:
	}

	var err error
	client, err = net.DialUnix("unix", nil, addr)
	if err != nil {
		t.Fatalf("dial uds failed:%s", err.Error())
	}

	select {
	case server = <-serverCh:
	case err := <-errCh:
		t.Fatalf("testUdsConn failed:%s", err.Error())
	case <-time.After(10 * time.Second):
		t.Fatalf("testUdsConn accept timeout")
	}
	return client, server
}

func testConf() *Config {
	conf := DefaultConfig()
	conf.MemMapType = MemMapTypeMemFd
	conf.ConnectionWriteTimeout = 25000 * time.Millisecond
	conf.ShareMemoryPathPrefix = "/Volumes/RAMDisk/shmipc.test"
	if runtime.GOOS == "linux" {
		// memfd 名字只用于日志和全局 bufferManager 表的 key，随机后缀保证用例之间互不干扰。
		conf.ShareMemoryPathPrefix = "/dev/shm/shmipc.test_" + strconv.Itoa(int(rand.Int63()))
	}
	conf.ShareMemoryBufferCap = 32 * 1024 * 1024 // 32M
	// 测试用例只关心协议行为，日志丢弃以避免污染输出。
	conf.LogOutput = io.Discard
	return conf
}

func testClientServer(t *testing.T) (*Session, *Session) {
	t.Helper()
	return testClientServerConfig(t, testConf())
}

func testClientServerConfig(t *testing.T, conf *Config) (*Session, *Session) {
	t.Helper()

	clientConn, serverConn := testConn(t)
	type sessionResult struct {
		session *Session
		err     error
	}
	serverCh := make(chan sessionResult, 1)

	go func() {
		serverConf := *conf
		s, sErr := newSession(&serverConf, serverConn, false)
		serverCh <- sessionResult{s, sErr}
	}()

	clientConf := *conf
	client, cErr := newSession(&clientConf, clientConn, true)
	if cErr != nil {
		t.Fatalf("create client session failed:%s", cErr.Error())
	}

	select {
	case r := <-serverCh:
		if r.err != nil {
			t.Fatalf("create server session failed:%s", r.err.Error())
		}
		return client, r.session
	case <-time.After(60 * time.Second):
		t.Fatalf("create server session timeout")
	}
	return nil, nil
}

func TestSession_OpenStream(t *testing.T) {
	// case1: session closed
	client, server := testClientServer(t)

	client.Close()
	server.Close()
	assert.Equal(t, true, client.IsClosed())
	stream, err := client.OpenStream()
	assert.Equal(t, (*Stream)(nil), stream)
	assert.Equal(t, ErrSessionShutdown, err)
	// client's Close will cause server's Close
	assert.Equal(t, true, server.IsClosed())

	//case2: CircuitBreaker triggered
	client2, server2 := testClientServer(t)
	client2.openCircuitBreaker()
	stream2, err := client2.OpenStream()
	assert.Equal(t, (*Stream)(nil), stream2)
	assert.Equal(t, ErrSessionUnhealthy, err)
	client2.Close()
	server2.Close()

	// case3: stream exist
	client3, server3 := testClientServer(t)
	client3.streams[client3.nextStreamID+1] = newStream(client3, client3.nextStreamID+1)
	stream3, err := client3.OpenStream()
	assert.Equal(t, (*Stream)(nil), stream3)
	assert.Equal(t, ErrStreamsExhausted, err)
	client3.Close()
	server3.Close()
}

func TestSession_AcceptStreamNormally(t *testing.T) {
	done := make(chan struct{})
	notifyRead := make(chan struct{})
	client, server := testClientServer(t)
	defer client.Close()
	defer server.Close()

	// case1: accept stream normally
	go func() {
		defer close(done)
		cStream, err := client.OpenStream()
		if err != nil {
			t.Errorf("Failed to open stream:%s", err.Error())
			return
		}
		defer cStream.Close()
		// only when we actually send something, the server can
		// aware that a new stream created, therefore we need to
		// send a byte to notify server
		_ = cStream.BufferWriter().WriteString("1")
		cStream.Flush(true)
		// wait resp
		<-notifyRead

		respData, err := cStream.BufferReader().ReadBytes(1)
		if err != nil {
			t.Errorf("Failed to read bytes:%s", err.Error())
			return
		}
		assert.Equal(t, "1", string(respData))
	}()

	sStream, err := server.AcceptStream()
	if err != nil {
		t.Fatalf("Failed to accept stream:%s", err.Error())
	}
	defer sStream.Close()
	respData, err := sStream.BufferReader().ReadBytes(1)
	if err != nil {
		t.Fatalf("Failed to read bytes:%s", err.Error())
	}
	assert.Equal(t, "1", string(respData))

	// write back
	_ = sStream.BufferWriter().WriteString("1")
	sStream.Flush(true)
	close(notifyRead)
	<-done
}

func TestSession_AcceptStreamWhenSessionClosed(t *testing.T) {
	client, server := testClientServer(t)
	defer client.Close()
	defer server.Close()

	// case 2: accept when session closed
	cStream2, err := client.OpenStream()
	if err != nil {
		t.Fatalf("Failed to malloc buf:%s", err.Error())
	}
	_ = cStream2.BufferWriter().WriteString("1")
	_ = cStream2.Flush(true)

	// now shutdown session
	_ = client.Close()
	_ = server.Close()
	assert.Equal(t, uint32(1), server.shutdown)

	// 关闭后 AcceptStream 必须返回 ErrSessionShutdown。关闭前已经 in-flight 的 stream
	// 可能残留在 acceptCh 里（select 两个 case 都就绪时是随机的），所以这里循环取值：
	// 先消化掉残留的 stream，最终必须拿到 shutdown 错误。
	deadline := time.Now().Add(10 * time.Second)
	for {
		stream, err := server.AcceptStream()
		if err != nil {
			assert.Equal(t, ErrSessionShutdown, err)
			break
		}
		assert.NotEqual(t, (*Stream)(nil), stream)
		if time.Now().After(deadline) {
			t.Fatal("AcceptStream hadn't returned shutdown error in time")
		}
	}
}

func TestSendData_Small(t *testing.T) {
	client, server := testClientServer(t)
	defer client.Close()
	defer server.Close()
	wg := &sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		stream, err := server.AcceptStream()
		if err != nil {
			t.Errorf("accept err: %v", err)
			return
		}

		if server.GetActiveStreamCount() != 1 {
			t.Errorf("num of streams is %d", server.GetActiveStreamCount())
			return
		}

		size := 0
		for size < 4*100 {
			bs, err := stream.BufferReader().ReadBytes(4)
			size += 4
			if err != nil {
				t.Errorf("read err: %v", err)
				return
			}
			if string(bs) != "test" {
				t.Errorf("bad: %s", string(bs))
				return
			}
		}

		if err := stream.Close(); err != nil {
			t.Errorf("err: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		stream, err := client.OpenStream()
		if err != nil {
			t.Errorf("err: %v", err)
			return
		}

		if client.GetActiveStreamCount() != 1 {
			t.Errorf("bad")
			return
		}

		for i := 0; i < 100; i++ {
			_, err := stream.BufferWriter().WriteBytes([]byte("test"))
			if err != nil {
				t.Errorf("err: %v", err)
				return
			}
			err = stream.Flush(false)
			if err != nil {
				t.Errorf("err: %v", err)
				return
			}
		}

		if err := stream.Close(); err != nil {
			t.Errorf("err: %v", err)
		}
	}()

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(time.Second * 60):
		t.Fatal("timeout")
	}

	if client.GetActiveStreamCount() != 0 {
		t.Fatalf("bad, streams:%d", client.GetActiveStreamCount())
	}
	if server.GetActiveStreamCount() != 0 {
		t.Fatalf("bad")
	}
}

func TestSendData_Large(t *testing.T) {
	client, server := testClientServer(t)
	defer client.Close()
	defer server.Close()

	const (
		sendSize = 2 * 1024 * 1024
		recvSize = 4 * 1024
	)

	data := make([]byte, recvSize)
	for idx := range data {
		data[idx] = byte(idx % 256)
	}

	wg := &sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		stream, err := server.AcceptStream()
		if err != nil {
			t.Errorf("err: %v", err)
			return
		}
		defer stream.Close()
		for hadRead := 0; hadRead < sendSize; hadRead++ {
			if bt, err := stream.BufferReader().ReadByte(); bt != byte(hadRead%256) || err != nil {
				t.Errorf("bad: %v %v", hadRead, bt)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		stream, err := client.OpenStream()
		if err != nil {
			t.Errorf("err: %v", err)
			return
		}

		for i := 0; i < sendSize/recvSize; i++ {
			_, _ = stream.BufferWriter().WriteBytes(data)
			if err = stream.Flush(true); err != nil {
				t.Errorf("err: %v", err)
				return
			}
		}

		if err := stream.Close(); err != nil {
			t.Errorf("err: %v", err)
		}
	}()

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(60 * time.Second):
		t.Fatal("timeout")
	}
}

func TestManyStreams(t *testing.T) {
	client, server := testClientServer(t)
	defer server.Close()
	defer client.Close()

	wg := &sync.WaitGroup{}
	const writeSize = 8
	acceptor := func(i int) {
		defer wg.Done()
		stream, err := server.AcceptStream()
		if err != nil {
			t.Errorf("err: %v", err)
			return
		}
		defer stream.Close()

		for {
			stream.SetReadDeadline(time.Now().Add(1 * time.Second))
			buf := stream.BufferReader()
			n, err := buf.Discard(writeSize)
			if err == ErrEndOfStream {
				return
			}
			if err == io.EOF || err == ErrTimeout {
				return
			}
			if err != nil {
				t.Errorf("err: %v", err)
				return
			}
			if buf.Len() != 0 {
				t.Errorf("err!0: %d n:%d", buf.Len(), n)
				return
			}
		}
	}
	sender := func(i int) {
		defer wg.Done()
		stream, err := client.OpenStream()
		if err != nil {
			t.Errorf("err: %v", err)
			return
		}
		defer stream.Close()

		var msg [writeSize]byte
		if _, err := stream.BufferWriter().WriteBytes(msg[:]); err != nil {
			t.Errorf("err: %s", err.Error())
			return
		}
		if err = stream.Flush(true); err != nil {
			t.Errorf("err: %v", err)
			return
		}
	}

	for i := 0; i < 1; i++ {
		wg.Add(2)
		go acceptor(i)
		go sender(i)
	}

	wg.Wait()
}

type mockMonitor struct {
	emitCount  int32
	flushCount int32
}

func (m *mockMonitor) OnEmitSessionMetrics(PerformanceMetrics, StabilityMetrics, ShareMemoryMetrics, *Session) {
	atomic.AddInt32(&m.emitCount, 1)
}

func (m *mockMonitor) Flush() error {
	atomic.AddInt32(&m.flushCount, 1)
	return nil
}

// 覆盖 Session/Stream 的访问器、getStreamById/GetMetrics，以及 monitorLoop 的关闭路径。
func TestSessionAccessorsAndMonitor(t *testing.T) {
	conf := testConf()
	monitor := &mockMonitor{}
	conf.Monitor = monitor
	client, server := testClientServerConfig(t, conf)
	defer client.Close()
	defer server.Close()

	assert.Equal(t, true, client.IsClient())
	assert.Equal(t, false, server.IsClient())
	assert.NotEqual(t, "", client.ID())
	assert.Equal(t, "unix", client.LocalAddr().Network())
	assert.NotEqual(t, nil, client.RemoteAddr())

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed:%s", err.Error())
	}
	defer stream.Close()
	assert.Equal(t, client, stream.Session())
	assert.Equal(t, stream, client.getStreamById(stream.id))
	assert.Equal(t, (*Stream)(nil), client.getStreamById(stream.id+1000))

	_, _, smm := client.GetMetrics()
	assert.NotEqual(t, uint64(0), smm.CapacityOfShareMemoryInBytes)

	// session 关闭时 monitorLoop 会 emit 一次并 flush。
	_ = server.Close()
	_ = client.Close()
	deadline := time.Now().Add(10 * time.Second)
	for atomic.LoadInt32(&monitor.flushCount) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.True(t, atomic.LoadInt32(&monitor.emitCount) >= 1)
	assert.True(t, atomic.LoadInt32(&monitor.flushCount) >= 1)
}