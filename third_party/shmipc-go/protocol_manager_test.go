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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestProtocolCompatibilityForNetUnixConn(t *testing.T) {
	testProtocolCompatibility(t, testConf())
}

// 文件（/dev/shm 或任意文件系统）共享内存模式走 protocolInitializerV2，
// 与 MemFd 的 V3 版本协商路径不同，单独覆盖。
func TestProtocolCompatibilityForDevShmFile(t *testing.T) {
	dir := t.TempDir()
	conf := testConf()
	conf.MemMapType = MemMapTypeDevShmFile
	conf.ShareMemoryPathPrefix = filepath.Join(dir, "shmipc_proto")
	conf.QueuePath = filepath.Join(dir, "shmipc_proto_queue")
	testProtocolCompatibility(t, conf)
}

func testProtocolCompatibility(t *testing.T, conf *Config) {
	t.Helper()
	clientConn, serverConn := testUdsConn(t)

	serverCh := make(chan error, 1)
	go func() {
		sconf := *conf
		server, err := Server(serverConn, &sconf)
		if err != nil {
			serverCh <- err
			return
		}
		serverCh <- server.Close()
	}()

	client, err := newSession(conf, clientConn, true)
	assert.Equal(t, true, err == nil, err)
	if err == nil {
		_ = os.Remove(conf.ShareMemoryPathPrefix + bufferPathSuffix)
		_ = os.Remove(conf.QueuePath)
		_ = client.Close()
	}

	select {
	case err := <-serverCh:
		assert.Nil(t, err)
	case <-time.After(60 * time.Second):
		t.Fatal("testProtocolCompatibility timeout")
	}
}

func TestCreateProtoVersionInitializer(t *testing.T) {
	s := &Session{config: DefaultConfig()}
	h := header(make([]byte, headerSize))

	for _, version := range []uint8{2, 3} {
		initializer, err := createProtoVersionInitializer(s, version, h)
		assert.Nil(t, err)
		assert.Equal(t, version, initializer.Version())
	}

	initializer, err := createProtoVersionInitializer(s, 1, h)
	assert.Nil(t, initializer)
	assert.NotNil(t, err)
}

func TestProtocolAdaptorGetProtocolInitializer(t *testing.T) {
	// client + MemMapTypeDevShmFile 直接返回 V2（无需任何 IO）
	client := &Session{
		isClient: true,
		config:   &Config{MemMapType: MemMapTypeDevShmFile},
	}
	initializer, err := newProtocolAdaptor(client).getProtocolInitializer()
	assert.Nil(t, err)
	assert.Equal(t, uint8(2), initializer.Version())

	// server 端从 conn 上读首个 event header，由版本决定 initializer
	conn1, conn2 := testConn(t)
	defer conn1.Close()
	defer conn2.Close()

	h := header(make([]byte, headerSize))
	h.encode(headerSize, maxSupportProtoVersion, typeExchangeProtoVersion)
	if _, err = conn2.Write(h); err != nil {
		t.Fatalf("write first event header failed:%s", err.Error())
	}
	fd, err := getConnDupFd(conn1)
	assert.Nil(t, err)
	defer fd.Close()

	server := &Session{
		isClient: false,
		config:   DefaultConfig(),
		connFd:   int(fd.Fd()),
	}
	initializer, err = newProtocolAdaptor(server).getProtocolInitializer()
	assert.Nil(t, err)
	assert.Equal(t, maxSupportProtoVersion, initializer.Version())
}