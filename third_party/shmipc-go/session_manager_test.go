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
