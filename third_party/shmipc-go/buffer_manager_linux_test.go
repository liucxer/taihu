//go:build linux

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
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBufferList_ConcurrentPutPop(t *testing.T) {
	capPerBuffer := uint32(10)
	bufferNum := uint32(10)
	mem := make([]byte, countBufferListMemSize(bufferNum, capPerBuffer))
	l, err := createFreeBufferList(bufferNum, capPerBuffer, mem, 0)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var finishedWg sync.WaitGroup
	var startWg sync.WaitGroup
	concurrency := 100
	finishedWg.Add(concurrency)
	startWg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer finishedWg.Done()
			//put and pop
			startWg.Done()
			<-start
			for j := 0; j < 10000; j++ {
				var err error
				var b *bufferSlice
				b, err = l.pop()
				for err != nil {
					time.Sleep(time.Millisecond)
					b, err = l.pop()
				}
				assert.Equal(t, capPerBuffer, b.cap)
				assert.Equal(t, 0, b.size())
				assert.Equal(t, false, b.hasNext(), "offset:%d next:%d", b.offsetInShm, b.nextBufferOffset())
				l.push(b)
			}
		}()
	}
	startWg.Wait()
	close(start)
	finishedWg.Wait()
	assert.Equal(t, bufferNum, uint32(*l.size))
}

// 覆盖 getGlobalBufferManager / getGlobalBufferManagerWithMemFd 的错误分支。
func TestGlobalBufferManagerErrors(t *testing.T) {
	dir := t.TempDir()

	// create=false 且文件不存在
	bm, err := getGlobalBufferManager(filepath.Join(dir, "not_exist_buffer"), 1<<20, false,
		[]*SizePercentPair{{4096, 100}})
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferManager)(nil), bm)

	// 段太小：bufferRegionCap 不足以放下一个 buffer
	bm, err = getGlobalBufferManager(filepath.Join(dir, "tiny_buffer"), 4096, true,
		[]*SizePercentPair{{4096, 100}})
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferManager)(nil), bm)

	// percent 之和超过 100（createBufferManager 校验）
	bm, err = getGlobalBufferManager(filepath.Join(dir, "bad_percent_buffer"), 1<<20, true,
		[]*SizePercentPair{{4096, 60}, {8192, 60}})
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferManager)(nil), bm)

	// 非法的 memFd：Fstat 失败
	bm, err = getGlobalBufferManagerWithMemFd("bad_fd_buffer", -1, 0, false, nil)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferManager)(nil), bm)

	// memFd 合法但内存布局是空的：listNum == 0
	fd, err := MemfdCreate("empty_layout_buffer", 0)
	assert.Equal(t, nil, err)
	defer syscall.Close(fd)
	assert.Equal(t, nil, syscall.Ftruncate(fd, 1<<20))
	bm, err = getGlobalBufferManagerWithMemFd("empty_layout_buffer", fd, 0, false, nil)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferManager)(nil), bm)
}
