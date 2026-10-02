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
	"net"
	"path/filepath"
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