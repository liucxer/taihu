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
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestQueueManager_MemFd(t *testing.T) {
	qm1, err := createQueueManagerWithMemFd("ut_queue_"+t.Name(), 8192)
	assert.Equal(t, nil, err)
	qm2, err := mappingQueueManagerMemfd("ut_queue_"+t.Name(), qm1.memFd)
	assert.Equal(t, nil, err)
	// qm2 与 qm1 共享同一个 memFd，只解除映射，不重复 close（qm1.unmap 负责 close）。
	defer func() { _ = syscall.Munmap(qm2.mem) }()
	defer qm1.unmap()

	assert.Equal(t, nil, qm1.sendQueue.put(queueElement{seqID: 7}))
	e, err := qm2.recvQueue.pop()
	assert.Equal(t, nil, err)
	assert.Equal(t, uint32(7), e.seqID)
}
