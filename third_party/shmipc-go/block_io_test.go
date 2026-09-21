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

func TestBlockReadFullAndBlockWriteFull(t *testing.T) {
	content := "hello,shmipc!"
	// Create a local Unix socket listener
	laddr, err := net.ResolveUnixAddr("unix", filepath.Join(t.TempDir(), "testBlockRWFull.sock"))
	if err != nil {
		t.Fatalf("failed to resolve unix address: %v\n", err)
	}
	listener, err := net.ListenUnix("unix", laddr)
	if err != nil {
		t.Fatalf("failed to listen unix: %v\n", err)
	}
	defer listener.Close()

	writeDone := make(chan struct{})
	// Start a goroutine to accept a connection and write data
	go func() {
		defer close(writeDone)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("failed to accept connection: %v\n", err)
			return
		}
		defer conn.Close()
		fd, err := getConnDupFd(conn)
		if err != nil {
			t.Errorf("failed to getConnDupFd: %v\n", err)
			return
		}
		defer fd.Close()

		// Write data using blockWriteFull
		data := []byte(content)
		if err := blockWriteFull(int(fd.Fd()), data); err != nil {
			t.Errorf("failed to write data: %v\n", err)
		}
	}()

	// Dial the Unix socket and read data
	conn, err := net.DialUnix("unix", nil, laddr)
	if err != nil {
		t.Fatalf("failed to dial unix: %v\n", err)
	}
	defer conn.Close()
	fd, err := getConnDupFd(conn)
	if err != nil {
		t.Fatalf("failed to getConnDupFd: %v\n", err)
	}
	defer fd.Close()

	// 等到对端 blockWriteFull 返回，数据已进入本端接收队列，避免非阻塞 fd 上读到 EAGAIN。
	<-writeDone

	// Read data using blockReadFull
	buf := make([]byte, 1024)
	if err := blockReadFull(int(fd.Fd()), buf[:len(content)]); err != nil {
		t.Fatalf("failed to read data: %v\n", err)
	}
	// Check if the read data is correct
	assert.Equal(t, buf[:len(content)], []byte(content))
}

func TestBlockReadFullEOF(t *testing.T) {
	conn1, conn2 := testConn(t)
	// 对端关闭后，读操作应当返回 io.EOF
	assert.Nil(t, conn2.Close())

	fd, err := getConnDupFd(conn1)
	assert.Nil(t, err)
	defer fd.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		err = blockReadFull(int(fd.Fd()), make([]byte, 8))
		if err == io.EOF || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	assert.Equal(t, io.EOF, err)
	_ = conn1.Close()
}

func TestBlockWriteFullOnClosedFd(t *testing.T) {
	conn1, conn2 := testConn(t)
	defer conn2.Close()
	defer conn1.Close()

	fd, err := getConnDupFd(conn1)
	assert.Nil(t, err)
	assert.Nil(t, fd.Close())
	assert.NotNil(t, blockWriteFull(int(fd.Fd()), []byte("x")))
}