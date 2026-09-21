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
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// readAllWithTimeout 读满 n 字节，超时即失败，避免偶发阻塞把整个测试进程挂死。
func readAllWithTimeout(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	type readResult struct {
		data []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		data := make([]byte, n)
		_, err := io.ReadFull(r, data)
		ch <- readResult{data: data, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read %d bytes failed:%s", n, r.err.Error())
		}
		return r.data
	case <-time.After(30 * time.Second):
		t.Fatalf("read %d bytes timeout", n)
		return nil
	}
}

// 覆盖高层 net.Listener API（net_listener.go）以及 Stream/streamWrapper 的 net.Conn 适配方法。
// 上游 v0.2.0 没有对应的测试用例（其单测之外被丢弃），这里补齐。
func TestNetListenerAndStreamWrapper(t *testing.T) {
	dir := t.TempDir()
	udsPath := filepath.Join(dir, "net_listener.sock")

	ln, err := ListenWithBacklog(udsPath, 8)
	if err != nil {
		t.Fatalf("ListenWithBacklog failed:%s", err.Error())
	}
	assert.Equal(t, "unix", ln.Addr().Network())

	conf := testConf()
	smConf := &SessionManagerConfig{
		Config:            conf,
		Network:           "unix",
		Address:           udsPath,
		SessionNum:        1,
		MaxStreamNum:      4,
		StreamMaxIdleTime: 10 * time.Second,
	}
	client, err := newClientSession(1, 0, 0, smConf)
	if err != nil {
		t.Fatalf("newClientSession failed:%s", err.Error())
	}
	defer client.Close()

	acceptCh := make(chan net.Conn, 1)
	acceptErrCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			acceptErrCh <- err
			return
		}
		acceptCh <- conn
	}()

	// 客户端写数据：listener 内部的 session 收到后会把这个 stream 投递到 backlog。
	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed:%s", err.Error())
	}
	reqPayload := "hello shmipc"
	if _, err = stream.Write([]byte(reqPayload)); err != nil {
		t.Fatalf("stream.Write failed:%s", err.Error())
	}

	var conn net.Conn
	select {
	case conn = <-acceptCh:
	case err := <-acceptErrCh:
		t.Fatalf("listener.Accept failed:%s", err.Error())
	case <-time.After(30 * time.Second):
		t.Fatalf("listener.Accept timeout")
	}
	assert.Equal(t, "unix", conn.LocalAddr().Network())
	assert.NotEqual(t, nil, conn.RemoteAddr())
	assert.Equal(t, reqPayload, string(readAllWithTimeout(t, conn, len(reqPayload))))

	// 反向：wrapper 写，客户端 stream 读。
	respPayload := "world"
	if _, err = conn.Write([]byte(respPayload)); err != nil {
		t.Fatalf("conn.Write failed:%s", err.Error())
	}
	assert.Equal(t, respPayload, string(readAllWithTimeout(t, stream, len(respPayload))))

	// Stream 上的 net.Conn 适配方法
	assert.Equal(t, "unix", stream.LocalAddr().Network())
	assert.NotEqual(t, nil, stream.RemoteAddr())
	assert.Equal(t, uint32(2), stream.StreamID())
	assert.Nil(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	assert.Nil(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	assert.Nil(t, conn.SetWriteDeadline(time.Now().Add(10*time.Second)))

	assert.Nil(t, conn.Close())
	// streamWrapper.Close 幂等
	assert.Nil(t, conn.Close())

	_ = stream.Close()
	_ = client.Close()
	assert.Nil(t, ln.Close())

	// Listen 走的是默认 backlog，单独覆盖一次。
	ln2, err := Listen(filepath.Join(dir, "net_listener2.sock"))
	if err != nil {
		t.Fatalf("Listen failed:%s", err.Error())
	}
	assert.Nil(t, ln2.Close())
}