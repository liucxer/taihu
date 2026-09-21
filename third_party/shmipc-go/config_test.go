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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func Test_VerifyConfig(t *testing.T) {
	config := DefaultConfig()
	// shm too small, err
	config.ShareMemoryBufferCap = 1
	err := VerifyConfig(config)
	assert.NotEqual(t, nil, err)
	config.ShareMemoryBufferCap = 1 << 20

	config.BufferSliceSizes = []*SizePercentPair{}
	err = VerifyConfig(config)
	assert.NotEqual(t, nil, err)
	config.BufferSliceSizes = []*SizePercentPair{
		{4096, 70},
		{16 << 10, 20},
		{64 << 10, 9},
	}
	err = VerifyConfig(config)
	assert.NotEqual(t, nil, err)

	config.BufferSliceSizes = []*SizePercentPair{
		{4096, 70},
		{16 << 10, 20},
		{64 << 10, 11},
	}
	err = VerifyConfig(config)
	assert.NotEqual(t, nil, err)

	config.BufferSliceSizes = []*SizePercentPair{
		{4096, 70},
		{16 << 10, 20},
		{defaultShareMemoryCap, 11},
	}
	err = VerifyConfig(config)
	assert.NotEqual(t, nil, err)

	config.BufferSliceSizes = []*SizePercentPair{
		{4096, 70},
		{16 << 10, 20},
		{64 << 10, 10},
	}
	err = VerifyConfig(config)
	assert.Equal(t, nil, err)

	// arm 上 QueueCap 必须是 8 的倍数、Size 必须是 4 的倍数
	if isArmArch() {
		config.QueueCap = 7
		assert.NotEqual(t, nil, VerifyConfig(config))
		config.QueueCap = defaultQueueCap
		config.BufferSliceSizes = []*SizePercentPair{
			{4095, 70},
			{16 << 10, 20},
			{64 << 10, 10},
		}
		assert.NotEqual(t, nil, VerifyConfig(config))
		config.BufferSliceSizes = []*SizePercentPair{
			{4096, 70},
			{16 << 10, 20},
			{64 << 10, 10},
		}
		assert.Equal(t, nil, VerifyConfig(config))
	}

	// path 不能为空
	config.QueuePath = ""
	assert.NotEqual(t, nil, VerifyConfig(config))
	config = DefaultConfig()
	assert.Equal(t, nil, VerifyConfig(config))
}

func Test_CreateCSByWrongConfig(t *testing.T) {
	conn1, conn2 := testConn(t)
	config := DefaultConfig()
	config.ShareMemoryBufferCap = 1
	c, err := newSession(config, conn1, true)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*Session)(nil), c)

	ok := make(chan struct{})
	go func() {
		s, err := Server(conn2, config)
		assert.NotEqual(t, nil, err)
		assert.Equal(t, (*Session)(nil), s)
		close(ok)
	}()
	<-ok
}

func Test_CreateCSWithoutConfig(t *testing.T) {
	conn1, conn2 := testConn(t)
	// 本用例刻意用 nil config 覆盖 DefaultConfig 分支，而 DefaultConfig 的 shm 路径是
	// 固定的 /dev/shm/shmipc*（不是随机名）。清掉同名前一次异常退出留下的残留，
	// 避免 O_EXCL 建文件失败；这几个路径只有本用例会创建。
	for _, p := range []string{DefaultConfig().ShareMemoryPathPrefix + bufferPathSuffix, DefaultConfig().QueuePath} {
		_ = os.Remove(p)
	}
	ok := make(chan struct{})
	go func() {
		s, err := Server(conn2, nil)
		assert.Equal(t, nil, err)
		assert.NotEqual(t, (*Session)(nil), s)
		if err == nil {
			defer s.Close()
		}
		close(ok)
	}()

	c, err := newSession(nil, conn1, true)
	assert.Equal(t, nil, err)
	if err == nil {
		defer c.Close()
	}
	assert.NotEqual(t, (*Session)(nil), c)
	time.Sleep(time.Second)
	<-ok
}