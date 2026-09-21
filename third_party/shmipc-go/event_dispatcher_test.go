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
	"bytes"
	"fmt"
	"math/rand"
	"net"
	"runtime"
	"testing"
	"time"
)

var (
	expectData []byte
	writevData [][]byte
	done       = make(chan struct{})
)

type serverConnCallback struct {
	t          *testing.T
	readBuffer []byte
}

type clientConnCallback struct {
	t *testing.T
}

func (c *clientConnCallback) onEventData(buf []byte, conn eventConn) error { return nil }
func (c *clientConnCallback) onRemoteClose()                               { fmt.Println("client onRemoteClose") }
func (c *clientConnCallback) onLocalClose()                                { fmt.Println("client onLocalClose") }

func (c *serverConnCallback) onEventData(buf []byte, conn eventConn) error {
	c.readBuffer = append(c.readBuffer, buf...)
	conn.commitRead(len(buf))
	if len(c.readBuffer) == len(expectData) {
		if !bytes.Equal(c.readBuffer, expectData) {
			c.t.Errorf("received data mismatch, got %d bytes", len(c.readBuffer))
		}
		close(done)
	}
	return nil
}

func (c *serverConnCallback) onRemoteClose() { fmt.Println("server onRemoteClose") }
func (c *serverConnCallback) onLocalClose()  { fmt.Println("server onLocalClose") }

var _ eventConnCallback = &serverConnCallback{}
var _ eventConnCallback = &clientConnCallback{}

// fillTestingData 构造单次 write 与批量 writev 的数据。
// 上游为 1020 个最大 1MB 的包（可能占用 ~512MB），这里降到 64 个最大 64KB 的包：
// 同样覆盖 writev 多 iovec 与跨批次收包路径，但内存占用与耗时可控。
func fillTestingData() {
	const msgN = 64
	writevData = make([][]byte, msgN)
	expectData = make([]byte, 0, 64*1024*msgN)
	for i := 0; i < msgN; i++ {
		writevData[i] = make([]byte, rand.Intn(64*1024))
		rand.Read(writevData[i])
		expectData = append(expectData, writevData[i]...)
	}
}

func Test_EventDispatcher(t *testing.T) {
	ensureDefaultDispatcherInit()
	fillTestingData()
	d := defaultDispatcher

	// 用 127.0.0.1:0 取临时端口，避免与其它测试/进程争用固定端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed:%s", err.Error())
	}
	defer ln.Close()

	serverConnCh := make(chan eventConn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			t.Errorf("accept failed:%s", err.Error())
			return
		}
		fd, err := getConnDupFd(conn)
		if err != nil {
			t.Errorf("getConnDupFd failed:%s", err.Error())
			return
		}
		conn.Close()
		serverConn := d.newConnection(fd)
		if err := serverConn.setCallback(&serverConnCallback{
			t:          t,
			readBuffer: make([]byte, 0, len(expectData)),
		}); err != nil {
			t.Errorf("setCallback failed:%s", err.Error())
			return
		}
		serverConnCh <- serverConn
		runtime.KeepAlive(fd)
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial failed:%s", err.Error())
	}
	fd, err := getConnDupFd(conn)
	if err != nil {
		t.Fatalf("getConnDupFd failed:%s", err.Error())
	}
	conn.Close()
	clientConn := d.newConnection(fd)
	if err := clientConn.setCallback(&clientConnCallback{t}); err != nil {
		t.Fatalf("setCallback failed:%s", err.Error())
	}
	runtime.KeepAlive(fd)

	if err := clientConn.write(writevData[0]); err != nil {
		t.Fatalf("write failed:%s", err.Error())
	}
	if err := clientConn.writev(writevData[1:]...); err != nil {
		t.Fatalf("writev failed:%s", err.Error())
	}

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("timeout: received %d bytes, expect %d", len(expectData), len(expectData))
	}

	clientConn.close()
	select {
	case serverConn := <-serverConnCh:
		serverConn.close()
	case <-time.After(5 * time.Second):
		t.Fatalf("server connection was not established")
	}
}