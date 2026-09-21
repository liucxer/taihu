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
	"testing"
)

func TestLogColor(t *testing.T) {
	SetLogLevel(levelTrace)
	defer SetLogLevel(levelWarn)

	internalLogger.tracef("this is tracef %s", "hello world")
	internalLogger.trace("this is trace")

	internalLogger.infof("this is infof %s", "hello world")
	internalLogger.info("this is info")

	internalLogger.debugf("this is debugf %s", "hello world")
	internalLogger.debug("this is debug")

	internalLogger.warnf("this is warnf %s", "hello world")
	internalLogger.warn("this is warn")

	internalLogger.errorf("this is errorf %s", "hello world")
	internalLogger.error("this is error")
}

// 覆盖 logger 的 level 过滤分支：级别设为 levelNoPrint 时所有日志都应被丢弃。
func TestLogLevelFilter(t *testing.T) {
	defer SetLogLevel(levelWarn)
	SetLogLevel(levelNoPrint)

	internalLogger.tracef("filtered tracef %s", "hello world")
	internalLogger.trace("filtered trace")
	internalLogger.infof("filtered infof %s", "hello world")
	internalLogger.info("filtered info")
	internalLogger.debugf("filtered debugf %s", "hello world")
	internalLogger.debug("filtered debug")
	internalLogger.warnf("filtered warnf %s", "hello world")
	internalLogger.warn("filtered warn")
	internalLogger.errorf("filtered errorf %s", "hello world")
	internalLogger.error("filtered error")

	// SetLogLevel 对非法（过大）级别不做处理
	SetLogLevel(levelNoPrint + 1)
	internalLogger.error("should not be printed by levelNoPrint")
}

// 覆盖 debug.go 中的共享内存/队列排查工具。
// 注意：这些工具按 `capPerBuffer + bufferHeaderSize` 的步长遍历共享内存，
// fork 的布局是按 align4K 对齐的，所以这里特意选取 Size=4076（4076+20 == align4K(4076+20) == 4096），
// 使得排查工具的步长与真实布局一致，从而安全地走完全部遍历逻辑。
func TestDebugBufferAndQueueDetail(t *testing.T) {
	dir := t.TempDir()

	// 文件不存在：只打印错误后返回
	DebugBufferListDetail(filepath.Join(dir, "not_exist_shm"))
	DebugQueueDetail(filepath.Join(dir, "not_exist_queue"))

	shmPath := filepath.Join(dir, "debug_shm")
	bm, err := getGlobalBufferManager(shmPath, 1<<20, true, []*SizePercentPair{{4076, 100}})
	if err != nil {
		t.Fatalf("create buffer manager failed:%s", err.Error())
	}
	defer addGlobalBufferManagerRefCount(shmPath, -1)

	// 1) 全部空闲：没有内存泄漏
	DebugBufferListDetail(shmPath)

	// 2) 借出一个 buffer，制造“泄漏”现场，覆盖 printLeakShareMemory 的遍历
	if _, err = bm.allocShmBuffer(1024); err != nil {
		t.Fatalf("allocShmBuffer failed:%s", err.Error())
	}
	DebugBufferListDetail(shmPath)

	queuePath := filepath.Join(dir, "debug_queue")
	qm, err := createQueueManager(queuePath, 64)
	if err != nil {
		t.Fatalf("create queue manager failed:%s", err.Error())
	}
	DebugQueueDetail(queuePath)
	qm.unmap()
}