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
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var (
	firstMsg  = "hello start"
	secondMsg = "hello restart"

	_ ListenCallback  = &listenCbImpl{}
	_ StreamCallbacks = &streamCbImpl{}
)

type listenCbImpl struct {
	firstMsgDone  chan struct{}
	secondMsgDone chan struct{}
}

func (l listenCbImpl) OnNewStream(s *Stream) {
	if err := s.SetCallbacks(streamCbImpl{stream: s, firstMsgDone: l.firstMsgDone, secondMsgDone: l.secondMsgDone}); err != nil {
		fmt.Printf("OnNewStream SetCallbacks error %+v\n", err)
	}
}

func (l listenCbImpl) OnShutdown(reason string) {
}

type streamCbImpl struct {
	stream        *Stream
	firstMsgDone  chan struct{}
	secondMsgDone chan struct{}
}

func (s streamCbImpl) OnData(reader BufferReader) {
	_, _ = reader.Peek(1)
	ret, err := reader.ReadString(reader.Len())
	if err != nil {
		fmt.Println("streamCbImpl OnData err ", err)
		return
	}

	if ret == firstMsg {
		_ = s.stream.BufferWriter().WriteString(ret)
		s.stream.Flush(false)
		s.stream.ReleaseReadAndReuse()

		s.firstMsgDone <- struct{}{}
	}

	if ret == secondMsg {
		_ = s.stream.BufferWriter().WriteString(ret)
		s.stream.Flush(false)
		s.stream.ReleaseReadAndReuse()

		s.secondMsgDone <- struct{}{}
	}
}

func (s streamCbImpl) OnLocalClose() {
}

func (s streamCbImpl) OnRemoteClose() {
}

func getSessionManagerConfig(t *testing.T, udsPath string) *SessionManagerConfig {
	t.Helper()
	conf := DefaultSessionManagerConfig()
	conf.Address = udsPath
	conf.Network = "unix"
	conf.SessionNum = 2
	conf.StreamMaxIdleTime = 30 * time.Second
	conf.MemMapType = MemMapTypeMemFd
	conf.rebuildInterval = time.Millisecond * 300
	return conf
}

func genListenerByConfig(t *testing.T, udsPath string, cb ListenCallback) *Listener {
	t.Helper()
	config := NewDefaultListenerConfig(udsPath, "unix")
	listener, err := NewListener(cb, config)
	if err != nil {
		t.Fatalf("NewListener error %+v", err)
	}
	return listener
}

// mock hot restart, more details in the directory example/hot_restart_test
// step1. start one server and one client
// step2. client send the message `hello start`
// step3. after server receive message, which will start a new server and do hot restart.
// step4. client send the message `hello restart`
// step5. the new server will receive the message `hello restart`
func TestHotRestart(t *testing.T) {
	// 必须在 TestSM_GlobalCreation 之前运行（文件名顺序保证），否则 globalSM 已被占用。
	if GlobalSessionManager() != nil {
		t.Skip("global SessionManager had been initialized by other test")
	}

	udsPath := filepath.Join(t.TempDir(), "hot_restart_test.sock")
	firstMsgDone := make(chan struct{}, 1)
	secondMsgDone := make(chan struct{}, 1)
	oldListenerExit := make(chan struct{})

	oldListener := genListenerByConfig(t, udsPath, listenCbImpl{firstMsgDone: firstMsgDone, secondMsgDone: secondMsgDone})
	oldListener.SetUnlinkOnClose(false)
	go func() {
		defer close(oldListenerExit)
		runErr := oldListener.Run()
		assert.Nil(t, runErr)
	}()

	sessionManager, err := InitGlobalSessionManager(getSessionManagerConfig(t, udsPath))
	assert.NotNil(t, sessionManager)
	assert.Nil(t, err)
	defer sessionManager.Close()

	// send first message
	stream, err := sessionManager.GetStream()
	assert.NotNil(t, stream)
	assert.Nil(t, err)
	err = stream.BufferWriter().WriteString(firstMsg)
	assert.Nil(t, err)
	err = stream.Flush(false)
	assert.Nil(t, err)
	ret, err := stream.BufferReader().ReadString(len(firstMsg))
	assert.Nil(t, err)
	assert.Equal(t, firstMsg, ret)
	sessionManager.PutBack(stream)

	select {
	case <-firstMsgDone:
	case <-time.After(30 * time.Second):
		t.Fatal("wait first message done timeout")
	}

	// after handle first message, and then do hot restart
	newListener := genListenerByConfig(t, udsPath, listenCbImpl{firstMsgDone: firstMsgDone, secondMsgDone: secondMsgDone})
	newListener.SetUnlinkOnClose(false)
	newListenerExit := make(chan struct{})
	go func() {
		defer close(newListenerExit)
		runErr := newListener.Run()
		assert.Nil(t, runErr)
	}()
	assert.Nil(t, oldListener.HotRestart(1024))

	// wait old server reply
	deadline := time.Now().Add(30 * time.Second)
	for !oldListener.IsHotRestartDone() {
		if time.Now().After(deadline) {
			t.Fatal("hot restart hadn't done in time")
		}
		time.Sleep(time.Millisecond * 50)
	}
	assert.Nil(t, oldListener.Close())

	select {
	case <-oldListenerExit:
	case <-time.After(30 * time.Second):
		t.Fatal("old listener run hadn't exited")
	}

	stream, err = sessionManager.GetStream()
	assert.NotNil(t, stream)
	assert.Nil(t, err)
	err = stream.BufferWriter().WriteString(secondMsg)
	assert.Nil(t, err)
	err = stream.Flush(false)
	assert.Nil(t, err)
	ret, err = stream.BufferReader().ReadString(len(secondMsg))
	assert.Nil(t, err)
	assert.Equal(t, secondMsg, ret)
	sessionManager.PutBack(stream)

	select {
	case <-secondMsgDone:
	case <-time.After(30 * time.Second):
		t.Fatal("wait second message done timeout")
	}
	assert.Nil(t, newListener.Close())
	select {
	case <-newListenerExit:
	case <-time.After(30 * time.Second):
		t.Fatal("new listener run hadn't exited")
	}
}

// 覆盖 Listener 的 Addr/Accept 以及重复 Close 的幂等分支（Accept 仅为适配 net.Listener 接口）。
func TestListenerAddrAndAccept(t *testing.T) {
	udsPath := filepath.Join(t.TempDir(), "listener_addr.sock")
	l := genListenerByConfig(t, udsPath, listenCbImpl{})
	assert.Equal(t, "unix", l.Addr().Network())

	conn, err := l.Accept()
	assert.Equal(t, nil, conn)
	assert.NotEqual(t, nil, err)

	// 未 Run 时 Close 也应正常返回
	assert.Nil(t, l.Close())
	assert.Nil(t, l.Close())
}

// 覆盖 HotRestart 的进行中分支、checkHotRestart 的超时分支与 resetState。
// 用一个人造 Session 顶替真实 session：把 writing 置 1 走 hotRestart 的 slow path，
// 只把事件塞进 sendCh（不需要真实 eventConn / 对端），因而永远收不到 ack，
// 从而在 hotRestartCheckTimeout(2s) 后走到超时 → resetState 分支。
func TestListener_HotRestartTimeout(t *testing.T) {
	udsPath := filepath.Join(t.TempDir(), "hot_restart_timeout.sock")
	l := genListenerByConfig(t, udsPath, listenCbImpl{})
	defer l.Close()

	sess := &Session{
		logger:                newLogger("fake server session", io.Discard),
		name:                  "fake_session",
		handshakeDone:         true,
		state:                 defaultState,
		writing:               1, // 强制走 slow path，避免依赖 eventConn
		sendCh:                make(chan sendReady, 1),
		notifyContinueWriteCh: make(chan struct{}, 1),
	}
	l.sessions.sessionMu.Lock()
	l.sessions.data[sess] = struct{}{}
	l.sessions.sessionMu.Unlock()
	defer func() {
		l.sessions.sessionMu.Lock()
		delete(l.sessions.data, sess)
		l.sessions.sessionMu.Unlock()
	}()

	assert.Equal(t, nil, l.HotRestart(2048))
	assert.Equal(t, false, l.IsHotRestartDone())
	// 已在 hot restart 中，重复发起应被拒绝
	assert.Equal(t, ErrHotRestartInProgress, l.HotRestart(2049))

	deadline := time.Now().Add(15 * time.Second)
	for !l.IsHotRestartDone() {
		if time.Now().After(deadline) {
			t.Fatal("hot restart hadn't been reset in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	l.mu.Lock()
	assert.Equal(t, defaultState, l.state)
	assert.Equal(t, 0, l.hotRestartAckCount)
	l.mu.Unlock()

	l.sessions.sessionMu.Lock()
	assert.Equal(t, defaultState, sess.state)
	l.sessions.sessionMu.Unlock()
}