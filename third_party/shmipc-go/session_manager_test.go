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
	"math/rand"
	"net"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func testSessionMgrConf(t *testing.T) *SessionManagerConfig {
	t.Helper()
	// 使用 DevShmFile 模式，但把共享内存/队列文件放在 t.TempDir() 下（TMPDIR 是 xfs），
	// 既不污染 /dev/shm，又覆盖文件模式下的 create/mapping 路径。
	dir := t.TempDir()
	conf := DefaultConfig()
	conf.MemMapType = MemMapTypeDevShmFile
	conf.ShareMemoryPathPrefix = filepath.Join(dir, "shmipc_sm")
	conf.QueuePath = filepath.Join(dir, "shmipc_queue")

	return &SessionManagerConfig{
		Config:            conf,
		Address:           filepath.Join(dir, "ipc_sm.sock"),
		Network:           "unix",
		SessionNum:        10,
		MaxStreamNum:      5,
		StreamMaxIdleTime: 10 * time.Second,
	}
}

func newClientServerByNewClientSession(t *testing.T, conf *SessionManagerConfig) (*Session, *Session) {
	t.Helper()
	conf.MemMapType = MemMapTypeMemFd

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: conf.Address, Net: "unix"})
	if err != nil {
		t.Fatalf("listen uds failed:%s", err.Error())
	}
	t.Cleanup(func() { _ = ln.Close() })

	serverCh := make(chan *Session, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		// Server 返回时握手（含 mmap）已经完成。
		server, err := Server(conn, conf.Config)
		if err != nil {
			errCh <- err
			return
		}
		serverCh <- server
	}()

	client, err := newClientSession(1, 0, 0, conf)
	if err != nil {
		t.Fatalf("newClientSession failed:%s", err.Error())
	}

	var server *Session
	select {
	case server = <-serverCh:
	case err := <-errCh:
		t.Fatalf("create server session failed:%s", err.Error())
	case <-time.After(60 * time.Second):
		t.Fatalf("create server session timeout")
	}
	return client, server
}

func TestStreamPool_Put(t *testing.T) {
	client, server := newClientServerByNewClientSession(t, testSessionMgrConf(t))
	defer client.Close()
	defer server.Close()
	sp := newStreamPool(1)
	sp.session.Store(client)

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open stream failed:%s", err.Error())
	}
	defer stream.Close()

	stream2, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open stream failed:%s", err.Error())
	}
	defer stream2.Close()

	id := stream.id
	sp.putOrCloseStream(stream)
	assert.Equal(t, uint32(streamOpened), stream.state)
	// ring full close the second one
	sp.putOrCloseStream(stream2)
	assert.Equal(t, uint32(streamClosed), stream2.state)
	// get the previous one
	stream, _ = sp.getOrOpenStream()
	assert.Equal(t, id, stream.id)
	// add some bytes to recvbuf making reset failure
	stream.recvBuf = newEmptyLinkedBuffer(stream.session.bufferManager)
	_ = stream.recvBuf.WriteString("test")
	sp.putOrCloseStream(stream)
	// reset fail, make it closed
	assert.Equal(t, uint32(streamClosed), stream.state)
	// now the stream was closed, try put it again
	sp.putOrCloseStream(stream)
	// closed stream will not put into streamPool
	assert.Equal(t, sp.pop() == nil, true)
}

func TestStreamPool_Get(t *testing.T) {
	client, server := newClientServerByNewClientSession(t, testSessionMgrConf(t))
	defer client.Close()
	defer server.Close()
	sp := newStreamPool(2)
	sp.session.Store(client)

	stream1, _ := client.OpenStream()
	stream2, _ := client.OpenStream()
	assert.Equal(t, uint32(streamOpened), atomic.LoadUint32(&stream1.state))
	assert.Equal(t, uint32(streamOpened), atomic.LoadUint32(&stream2.state))
	// record id
	id1 := stream1.id
	id2 := stream2.id

	// test normal put and get
	sp.putOrCloseStream(stream1)
	sp.putOrCloseStream(stream2)
	stream1, _ = sp.getOrOpenStream()
	stream2, _ = sp.getOrOpenStream()
	assert.Equal(t, id1, stream1.id)
	assert.Equal(t, id2, stream2.id)

	// test put and get, when a stream is closed
	stream1.Close()
	sp.putOrCloseStream(stream1)
	sp.putOrCloseStream(stream2)
	stream1, _ = sp.getOrOpenStream()
	stream2, _ = sp.getOrOpenStream()
	assert.NotEqual(t, id1, stream1.id)
	assert.NotEqual(t, id2, stream2.id)
	assert.Equal(t, id2, stream1.id)

	// test get, if a stream is closed after it was put into streamPool
	id1 = stream1.id
	id2 = stream2.id
	sp.putOrCloseStream(stream1)
	sp.putOrCloseStream(stream2)
	stream2.Close()
	stream1, _ = sp.getOrOpenStream()
	stream2, _ = sp.getOrOpenStream()
	assert.Equal(t, id1, stream1.id)
	assert.NotEqual(t, id2, stream2.id)

	// test get, if a session is unhealthy
	sp.putOrCloseStream(stream1)
	client.openCircuitBreaker()
	stream, err := sp.getOrOpenStream()
	assert.Equal(t, (*Stream)(nil), stream)
	assert.Equal(t, ErrSessionUnhealthy, err)
}

func TestStreamPool_Close(t *testing.T) {
	client, server := newClientServerByNewClientSession(t, testSessionMgrConf(t))
	defer client.Close()
	defer server.Close()
	sp := newStreamPool(2)
	assert.Equal(t, (*Session)(nil), sp.Session())
	sp.session.Store(client)
	assert.Equal(t, client, sp.Session())

	stream1, _ := client.OpenStream()
	stream2, _ := client.OpenStream()
	sp.putOrCloseStream(stream1)
	sp.putOrCloseStream(stream2)
	sp.close()
	assert.Equal(t, uint32(streamClosed), stream1.state)
	assert.Equal(t, uint32(streamClosed), stream2.state)
}

func TestSM_NewClientSession(t *testing.T) {
	conf := testSessionMgrConf(t)
	done := make(chan struct{})
	mockDataLen := 10
	mockData := make([]byte, mockDataLen)
	rand.Read(mockData)
	client, server := newClientServerByNewClientSession(t, conf)
	defer client.Close()
	defer server.Close()

	go func() {
		defer close(done)
		stream, err := server.AcceptStream()
		if err != nil {
			t.Errorf("accept stream failed %s", err.Error())
			return
		}
		defer stream.Close()
		buf := stream.BufferReader()
		reqData, err := buf.ReadBytes(mockDataLen)
		if err != nil {
			t.Errorf("readBuf failed %s", err.Error())
			return
		}
		assert.Equal(t, mockData, reqData)
	}()

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("client open stream failed:%s", err.Error())
	}
	defer stream.Close()

	if _, err = stream.BufferWriter().WriteBytes(mockData); err != nil {
		t.Fatalf("buffer writeString failed:%s", err.Error())
	}

	if err = stream.Flush(true); err != nil {
		t.Fatalf("stream Flush failed:%s", err.Error())
	}

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("TestSM_NewClientSession timeout")
	}
}

func TestSM_Background(t *testing.T) {
	config := testSessionMgrConf(t)
	config.Config.rebuildInterval = time.Millisecond * 300
	config.SessionNum = 1
	// 这里必须用 MemFd：DevShmFile 模式下 ShareMemoryPathPrefix/QueuePath 是磁盘上的真实文件，
	// session 的 Close() 是异步回收（defer 到 dispatcher 上执行），重建时会撞上
	// "queue was existed"；且旧 session 的异步回收会误删新 session 刚建好的同名文件。
	config.MemMapType = MemMapTypeMemFd

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: config.Address, Net: "unix"})
	if err != nil {
		t.Fatalf("listen uds failed:%s", err.Error())
	}
	t.Cleanup(func() { _ = ln.Close() })

	// peer 端：持续 accept，逐个建立 server session。任何一个连接握手失败都只丢弃该连接，
	// 不能退出 accept 循环，否则 uds 文件会被 unlink，导致后续 rebuilt session 全部 dial 失败。
	var (
		serverMu sync.Mutex
		servers  []*Session
	)
	acceptLoopDone := make(chan struct{})
	go func() {
		defer close(acceptLoopDone)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := Server(conn, config.Config)
			if err != nil {
				_ = conn.Close()
				continue
			}
			serverMu.Lock()
			servers = append(servers, server)
			serverMu.Unlock()
		}
	}()
	acceptedCount := func() int {
		serverMu.Lock()
		defer serverMu.Unlock()
		return len(servers)
	}
	t.Cleanup(func() {
		serverMu.Lock()
		defer serverMu.Unlock()
		for _, s := range servers {
			_ = s.Close()
		}
	})

	sm, err := NewSessionManager(config)
	if err != nil {
		t.Fatalf("Create session manager failed:%s", err.Error())
	}
	defer sm.Close()

	// NewSessionManager 返回代表首个 session 的握手（含 mmap）已经完成。
	s1 := sm.pools[0].Session()
	if s1 == nil {
		t.Fatal("session pool has no session")
	}
	// now sm has 1 session, try to close it
	_ = s1.Close()
	assert.Equal(t, true, atomic.LoadUint32(&s1.shutdown) == 1)

	// 等待 background goroutine 重建 session，最长等 10s（rebuildInterval=300ms）。
	var s2 *Session
	deadline := time.Now().Add(10 * time.Second)
	for {
		s2 = sm.pools[0].Session()
		if s2 != nil && s2 != s1 && atomic.LoadUint32(&s2.shutdown) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session hadn't been rebuilt in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.Equal(t, uint32(0), atomic.LoadUint32(&s2.shutdown))

	// peer 端也应完成了 rebuilt session 的 accept + 握手。
	deadline = time.Now().Add(10 * time.Second)
	for acceptedCount() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("peer accepted %d sessions, expect at least 2", acceptedCount())
		}
		time.Sleep(20 * time.Millisecond)
	}

	_ = sm.Close()
	_ = ln.Close()
	<-acceptLoopDone
}

func TestSM_GlobalCreation(t *testing.T) {
	config := testSessionMgrConf(t)
	// we need not create really connection here
	// this work have been done in background test
	config.SessionNum = 0
	gsm, err := InitGlobalSessionManager(config)
	assert.Nil(t, err)
	assert.NotNil(t, gsm)
	gsm2 := GlobalSessionManager()
	assert.Equal(t, gsm, gsm2)
	assert.Nil(t, GlobalSessionManager().Close())
}

func TestSM_GetAndPutStream(t *testing.T) {
	config := testSessionMgrConf(t)
	notifyConn := make(chan struct{})
	done := make(chan struct{})
	sessionPairNum := 1
	config.SessionNum = sessionPairNum
	wg := &sync.WaitGroup{}
	wg.Add(sessionPairNum)

	go func() {
		ln, _ := net.ListenUnix("unix", &net.UnixAddr{Name: config.Address, Net: "unix"})
		servers := make([]*Session, sessionPairNum)
		defer ln.Close()

		defer func() {
			for _, s := range servers {
				s.Close()
			}
		}()

		close(notifyConn)
		for i := 0; i < sessionPairNum; i++ {
			conn, err := ln.Accept()
			if err != nil {
				t.Errorf("accept failed:%s", err.Error())
				return
			}
			server, err := Server(conn, config.Config)
			if err != nil {
				t.Errorf("Server error:%s", err.Error())
				return
			}
			servers[i] = server
			wg.Done()
		}
		<-done
	}()

	<-notifyConn
	sm, err := NewSessionManager(config)
	if err != nil {
		t.Fatalf("Create session manager failed:%s", err.Error())
	}
	// wait until all pre init done
	wg.Wait()

	s, err := sm.GetStream()
	if err != nil {
		t.Fatalf("Create session manager failed:%s", err.Error())
	}
	assert.Equal(t, uint32(2), s.id)
	s2, err := sm.GetStream()
	if err != nil {
		t.Fatalf("Create session manager failed:%s", err.Error())
	}
	assert.Equal(t, uint32(3), s2.id)
	// put them back
	sm.PutBack(s)
	sm.PutBack(s2)
	// get again
	s3, err := sm.GetStream()
	assert.Nil(t, err)
	s4, err := sm.GetStream()
	assert.Nil(t, err)
	assert.Equal(t, s, s3)
	assert.Equal(t, s2, s4)

	sm.Close()
	close(done)
}

func TestStreamPool_PutAndPopWithConcurrently(t *testing.T) {
	pool := newStreamPool(4096)
	expectedStreamN := uint32(0)
	var wg sync.WaitGroup
	concurrency := 200
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			for n := 0; n < 2000; n++ {
				s := pool.pop()
				if s == nil {
					s = &Stream{pool: pool, id: atomic.AddUint32(&expectedStreamN, 1)}
				}
				runtime.Gosched()
				assert.Equal(t, nil, pool.push(s))
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, pool.tail-pool.head, uint64(expectedStreamN))

	verify := make(map[uint32]bool)
	for s := pool.pop(); s != nil; s = s.pool.pop() {
		verify[s.id] = true
	}
	assert.Equal(t, expectedStreamN, uint32(len(verify)))
}