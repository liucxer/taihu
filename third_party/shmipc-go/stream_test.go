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
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func newClientServerWithNoCheck(t *testing.T, conf *Config) (client *Session, server *Session) {
	t.Helper()
	return testClientServerConfig(t, conf)
}

func TestStream_Close(t *testing.T) {
	c := testConf()
	c.QueueCap = 8
	client, server := newClientServerWithNoCheck(t, c)
	defer server.Close()
	defer client.Close()

	hadForceCloseNotifyCh := make(chan struct{})
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		s, err := server.AcceptStream()
		if err != nil {
			t.Errorf("open stream failed:,err:%s", err.Error())
			return
		}
		_, err = s.BufferReader().ReadByte()
		assert.Equal(t, nil, err)
		<-hadForceCloseNotifyCh
		_, err = s.BufferReader().ReadByte()
		assert.Equal(t, ErrEndOfStream, err)
		assert.Equal(t, 1, server.GetActiveStreamCount())
		assert.Equal(t, uint32(streamHalfClosed), s.state)
		s.Close()
		assert.Equal(t, 0, server.GetActiveStreamCount())
		assert.Equal(t, uint32(streamClosed), s.state)
	}()

	s, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open stream failed:,err:%s", err.Error())
	}

	buf := s.BufferWriter()

	_ = buf.WriteByte(1)
	if err = s.Flush(false); err != nil {
		t.Fatalf("stream write buf failed:,err:%s", err.Error())
	}
	s.Close()
	_ = buf.WriteString("1")
	// try to write after stream closed
	err = s.Flush(false)
	assert.Equal(t, ErrStreamClosed, err)
	time.Sleep(time.Millisecond * 50)
	close(hadForceCloseNotifyCh)

	select {
	case <-doneCh:
	case <-time.After(30 * time.Second):
		t.Fatal("TestStream_Close timeout")
	}
}

func TestStream_ClientFallback(t *testing.T) {
	conf := testConf()
	conf.ShareMemoryBufferCap = 1 << 20
	client, server := testClientServerConfig(t, conf)

	defer client.Close()
	defer server.Close()

	done := make(chan struct{})
	mockData := make([][]byte, 2000)
	dataSize := 1024
	for i := range mockData {
		mockData[i] = make([]byte, dataSize)
		rand.Read(mockData[i])
	}
	go func() {
		defer close(done)
		s, err := server.AcceptStream()
		if err != nil {
			t.Errorf("server.AcceptStream failed:%s", err.Error())
			return
		}
		defer s.Close()
		reader := s.BufferReader()
		for i := range mockData {
			get, err := reader.ReadBytes(dataSize)
			if err != nil {
				t.Errorf("ReadBytes failed:%s", err.Error())
				return
			}
			if !assert.Equal(t, mockData[i], get, "i:%d", i) {
				return
			}
		}
	}()

	s, err := client.OpenStream()
	if err != nil {
		t.Fatalf("client.OpenStream failed:%s", err.Error())
	}
	defer s.Close()

	writer := s.sendBuf

	for i := range mockData {
		if _, err = writer.WriteBytes(mockData[i]); err != nil {
			t.Fatalf("Malloc(dataSize) failed:%s", err.Error())
		}
	}
	assert.Equal(t, false, writer.isFromShareMemory())

	err = s.Flush(true)
	if err != nil {
		t.Fatalf("writeBuf failed:%s", err.Error())
	}

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("TestStream_ClientFallback timeout")
	}
}

func TestStream_ServerFallback(t *testing.T) {
	conf := testConf()
	conf.ShareMemoryBufferCap = 1 << 20
	client, server := testClientServerConfig(t, conf)
	defer client.Close()
	defer server.Close()

	done := make(chan struct{})
	mockData := make([][]byte, 2000)
	dataSize := 1024
	for i := range mockData {
		mockData[i] = make([]byte, dataSize)
		rand.Read(mockData[i])
	}
	go func() {
		defer close(done)
		s, err := server.AcceptStream()
		if err != nil {
			t.Errorf("server.AcceptStream failed:%s", err.Error())
			return
		}
		defer s.Close()

		writer := s.sendBuf
		for i := range mockData {
			if _, err = writer.WriteBytes(mockData[i]); err != nil {
				t.Errorf("Malloc(dataSize) failed:%s", err.Error())
				return
			}
		}
		assert.Equal(t, false, writer.isFromShareMemory())

		if err = s.Flush(true); err != nil {
			t.Errorf("writeBuf failed:%s", err.Error())
		}
	}()

	s, err := client.OpenStream()
	if err != nil {
		t.Fatalf("client.OpenStream failed:%s", err.Error())
	}
	defer s.Close()
	writer := s.sendBuf

	// build conn
	if _, err = writer.WriteBytes(mockData[0]); err != nil {
		t.Fatalf("Malloc(dataSize) failed:%s", err.Error())
	}

	if err = s.Flush(true); err != nil {
		t.Fatalf("writeBuf failed:%s", err.Error())
	}

	reader := s.BufferReader()
	for i := range mockData {
		get, err := reader.ReadBytes(dataSize)
		if err != nil {
			t.Fatalf("ReadBytes failed:%s", err.Error())
		}
		if !assert.Equal(t, mockData[i], get, "i:%d", i) {
			return
		}
	}

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("TestStream_ServerFallback timeout")
	}
}

// TODO
func TestStream_RandomPackageSize(t *testing.T) {
}

func TestStream_HalfClose(t *testing.T) {
	conf := testConf()
	conf.ShareMemoryBufferCap = 1 << 20
	client, server := testClientServerConfig(t, conf)
	defer server.Close()
	defer client.Close()

	sbuf := make([]byte, 4096)

	wg := &sync.WaitGroup{}

	acceptor := func(i int) {
		defer wg.Done()
		stream, err := server.AcceptStream()
		if err != nil {
			t.Errorf("err: %v", err)
			return
		}
		defer stream.Close()

		buf := stream.BufferReader()
		_, err = buf.Discard(len(sbuf))
		if err == ErrStreamClosed {
			return
		}
		if err == io.EOF || err == ErrTimeout {
			t.Logf("ret err:%s", err)
			return
		}
		if err != nil {
			t.Errorf("err: %v", err)
			return
		}
		if buf.Len() != 0 {
			t.Errorf("err!0: %d", buf.Len())
			return
		}
		// after read
		buf.ReleasePreviousRead()
		wbuf := stream.BufferWriter()
		_, _ = wbuf.WriteBytes(sbuf)
		if err = stream.Flush(true); err != nil {
			t.Errorf("err: %v", err)
		}
	}
	sender := func(i int) {
		defer wg.Done()
		var err error
		stream, err := client.OpenStream()
		if err != nil {
			t.Errorf("err: %v", err)
			return
		}
		defer stream.Close()

		buf := stream.sendBuf
		_, _ = buf.WriteBytes(sbuf)
		if err = stream.Flush(true); err != nil {
			t.Errorf("err: %v", err)
			return
		}
		if _, err = stream.BufferReader().ReadByte(); err != nil {
			t.Errorf("err: %v", err)
		}
	}

	// 2 is ok, more maybe dead lock, wait shm
	for i := 0; i < 2; i++ {
		wg.Add(2)
		go acceptor(i)
		go sender(i)
	}

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(60 * time.Second):
		t.Fatal("TestStream_HalfClose timeout")
	}
}

func TestStream_SendQueueFull(t *testing.T) {
	conf := testConf()
	conf.QueueCap = 8 // can only contain 1 queue elem(12B)
	client, server := testClientServerConfig(t, conf)
	defer client.Close()
	defer server.Close()

	done := make(chan struct{})
	dataSize := 10
	mockDataLength := 500
	mockData := make([][]byte, mockDataLength)
	for i := range mockData {
		mockData[i] = make([]byte, dataSize)
		mockData[i][0] = byte(i)
	}

	s, err := client.OpenStream()
	if err != nil {
		t.Fatalf("client.OpenStream failed:%s", err.Error())
	}
	defer s.Close()

	// write in a short time to full the send queue
	for i := 0; i < mockDataLength; i++ {
		writer := s.BufferWriter()
		if _, err = writer.WriteBytes(mockData[i]); err != nil {
			t.Fatalf("Malloc(dataSize) failed:%s", err.Error())
		}
		if err = s.Flush(true); err != nil {
			t.Fatalf("writeBuf failed:%s", err.Error())
		}
	}

	go func() {
		defer close(done)
		s, err := server.AcceptStream()
		if err != nil {
			t.Errorf("server.AcceptStream failed:%s", err.Error())
			return
		}
		defer s.Close()
		for i := 0; i < mockDataLength; i++ {
			reader := s.BufferReader()
			get, err := reader.ReadBytes(dataSize)
			if err != nil {
				t.Errorf("ReadBytes failed:%s", err.Error())
				return
			}
			if !assert.Equal(t, mockData[i], get, "i:%d", i) {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("TestStream_SendQueueFull timeout")
	}
}

func TestStream_SendQueueFullTimeout(t *testing.T) {
	conf := testConf()
	conf.QueueCap = 8 // can only contain 1 queue elem(12B)
	client, server := testClientServerConfig(t, conf)
	defer client.Close()
	defer server.Close()

	dataSize := 10
	mockDataLength := 500
	mockData := make([][]byte, mockDataLength)
	for i := range mockData {
		mockData[i] = make([]byte, dataSize)
		rand.Read(mockData[i])
	}

	s, err := client.OpenStream()
	if err != nil {
		t.Fatalf("client.OpenStream failed:%s", err.Error())
	}
	// if send queue is full, it will trigger timeout immediately
	_ = s.SetWriteDeadline(time.Now())
	defer s.Close()

	// write in a short time to full the send queue
	for i := 0; i < mockDataLength; i++ {
		writer := s.BufferWriter()
		if _, err = writer.WriteBytes(mockData[i]); err != nil {
			t.Fatalf("Malloc(dataSize) failed:%s", err.Error())
		}
		err = s.Flush(true)
		if err != nil {
			// must be timeout err here
			assert.Equal(t, ErrTimeout, err)
			// reset deadline
			s.SetDeadline(time.Now().Add(time.Microsecond * 10))
		}
	}
}

// reset                           92.9%
func TestStream_Reset(t *testing.T) {
	conf := testConf()
	client, server := testClientServerConfig(t, conf)
	defer client.Close()
	defer server.Close()

	done := make(chan struct{})
	notifySend := make(chan struct{})
	notifyRead := make(chan struct{})
	notifyClosed := make(chan struct{})
	// use linked buffer by the way
	dataSize := 64<<10 + 1
	mockDataLength := 4
	mockData := make([][]byte, mockDataLength)
	for i := range mockData {
		mockData[i] = make([]byte, dataSize)
		rand.Read(mockData[i])
	}

	go func() {
		defer close(done)
		s, err := server.AcceptStream()
		if err != nil {
			t.Errorf("server.AcceptStream failed:%s", err.Error())
			return
		}
		defer s.Close()
		reader := s.BufferReader()
		for i := 0; i < mockDataLength/2; i++ {
			get, err := reader.ReadBytes(dataSize)
			if err != nil {
				t.Errorf("ReadBytes failed:%s", err.Error())
				return
			}
			if !assert.Equal(t, mockData[i], get, "i:%d", i) {
				return
			}
		}
		// read done, reset will be successful
		err = s.reset()
		assert.Equal(t, nil, err)
		close(notifySend)
		<-notifyRead

		// test reuse
		reader = s.BufferReader()
		for i := mockDataLength / 2; i < mockDataLength; i++ {
			get, err := reader.ReadBytes(dataSize)
			if err != nil {
				t.Errorf("ReadBytes failed:%s", err.Error())
				return
			}
			if !assert.Equal(t, mockData[i], get, "i:%d", i) {
				return
			}
		}

		<-notifyClosed
		err = s.reset()
		assert.Equal(t, ErrStreamClosed, err)
	}()

	s, err := client.OpenStream()
	if err != nil {
		t.Fatalf("client.OpenStream failed:%s", err.Error())
	}

	for i := 0; i < mockDataLength; i++ {
		if i == mockDataLength/2 {
			<-notifySend
		}
		if _, err := s.BufferWriter().WriteBytes(mockData[i]); err != nil {
			t.Fatalf("Malloc(dataSize) failed:%s", err.Error())
		}
		if err = s.Flush(false); err != nil {
			t.Fatalf("writeBuf failed:%s", err.Error())
		}
	}
	close(notifyRead)
	s.Close()
	close(notifyClosed)

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("TestStream_Reset timeout")
	}
}

func TestStream_fillDataToReadBuffer(t *testing.T) {
	conf := testConf()
	client, server := testClientServerConfig(t, conf)
	defer client.Close()
	defer server.Close()

	stream, _ := client.OpenStream()
	size := 8192
	slice := newBufferSlice(nil, make([]byte, size), 0, false)
	err := stream.fillDataToReadBuffer(bufferSliceWrapper{fallbackSlice: slice})
	assert.Equal(t, 1, len(stream.pendingData.unread))
	stream.Close()
	err = stream.fillDataToReadBuffer(bufferSliceWrapper{fallbackSlice: slice})
	assert.Equal(t, 0, len(stream.pendingData.unread))
	assert.Equal(t, nil, err)
}

// TODO: SwapBufferForReuse              66.7%
func TestStream_SwapBufferForReuse(t *testing.T) {
}