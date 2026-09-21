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
	"math/rand"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
)

func TestBufferManager_CreateAndMapping(t *testing.T) {
	//create
	mem := make([]byte, 32<<20)
	bm1, err := createBufferManager([]*SizePercentPair{
		{4096, 70},
		{16 * 1024, 20},
		{64 * 1024, 10},
	}, "", mem, 0)
	if err != nil {
		t.Fatal("create buffer manager failed, err=" + err.Error())
	}

	allocateFunc := func(bm *bufferManager) {
		for i := 0; i < 10; i++ {
			_, err := bm.allocShmBuffer(4096)
			assert.Equal(t, nil, err)
			_, err = bm.allocShmBuffer(16 * 1024)
			assert.Equal(t, nil, err)
			_, err = bm.allocShmBuffer(64 * 1024)
			assert.Equal(t, nil, err)
		}
	}
	allocateFunc(bm1)

	//mapping
	bm2, err := mappingBufferManager("", mem, 0)
	if err != nil {
		t.Fatal("mapping buffer manager failed, err=" + err.Error())
	}

	for i := range bm1.lists {
		assert.Equal(t, *bm1.lists[i].capPerBuffer, *bm2.lists[i].capPerBuffer)
		assert.Equal(t, *bm1.lists[i].size, *bm2.lists[i].size)
		assert.Equal(t, bm1.lists[i].offsetInShm, bm2.lists[i].offsetInShm)
	}

	allocateFunc(bm2)

	for i := range bm1.lists {
		assert.Equal(t, *bm1.lists[i].capPerBuffer, *bm2.lists[i].capPerBuffer)
		assert.Equal(t, *bm1.lists[i].size, *bm2.lists[i].size)
		assert.Equal(t, bm1.lists[i].offsetInShm, bm2.lists[i].offsetInShm)
	}
}

// 覆盖 createBufferManager 的入参校验与 percent 校验分支。
func TestBufferManager_CreateWithBadArgs(t *testing.T) {
	// mem 比 offset 还小
	bm, err := createBufferManager([]*SizePercentPair{{4096, 100}}, "", make([]byte, 8), 8)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferManager)(nil), bm)

	// percent 之和超过 100
	bm, err = createBufferManager([]*SizePercentPair{{4096, 60}, {8192, 60}}, "", make([]byte, 8<<20), 0)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferManager)(nil), bm)

	// 段太小，第一个列表的 bufferNum 为 0
	bm, err = createBufferManager([]*SizePercentPair{{64 << 10, 100}}, "", make([]byte, 64<<10), 0)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferManager)(nil), bm)
}

// 覆盖 mappingBufferManager 的入参/元数据校验分支。
func TestBufferManager_MappingBadMem(t *testing.T) {
	mem := make([]byte, 8<<20)
	bm, err := createBufferManager([]*SizePercentPair{{4096, 100}}, "", mem, 0)
	assert.Equal(t, nil, err)
	assert.NotEqual(t, (*bufferManager)(nil), bm)

	// mem 太小
	m, err := mappingBufferManager("", make([]byte, 4), 0)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferManager)(nil), m)

	// listNum == 0
	tiny := make([]byte, 4096)
	m, err = mappingBufferManager("", tiny, 0)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferManager)(nil), m)
}

func TestBufferManager_ReadBufferSlice(t *testing.T) {
	mem := make([]byte, 1<<20)
	bm, err := createBufferManager([]*SizePercentPair{
		{Size: uint32(4096), Percent: 100},
	}, "", mem, 0)
	assert.Equal(t, nil, err)

	s, err := bm.allocShmBuffer(4096)
	assert.Equal(t, nil, err)
	data := make([]byte, 4096)
	rand.Read(data)
	assert.Equal(t, 4096, s.append(data...))
	assert.Equal(t, 4096, s.size())
	s.update()

	s2, err := bm.readBufferSlice(s.offsetInShm)
	assert.Equal(t, nil, err)
	assert.Equal(t, s.capacity(), s2.capacity())
	assert.Equal(t, s.size(), s2.size())

	getData, err := s2.read(4096)
	assert.Equal(t, nil, err)
	assert.Equal(t, data, getData)

	s3, err := bm.readBufferSlice(s.offsetInShm + 1<<20)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferSlice)(nil), s3)

	// fork 的 4K 对齐布局下，1MB 段里该列表只有 1 个 buffer（stride 8K），
	// 不存在「下一个 buffer 起始 = +4096」的情形。这里直接在段内构造一个
	// cap 越界的 buffer 头，覆盖 readBufferSlice 的第二处校验
	// （bufEndOffset > len(mem)）——这正是「共享内存被写坏」时要拦住的场景。
	badOffset := bm.lists[0].bufferRegionOffsetInShm + alignSize
	assert.Equal(t, true, int(badOffset)+bufferHeaderSize < len(mem))
	*(*uint32)(unsafe.Pointer(&mem[badOffset+bufferCapOffset])) = uint32(len(mem))
	s4, err := bm.readBufferSlice(badOffset)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferSlice)(nil), s4)
}

func TestBufferManager_AllocRecycle(t *testing.T) {
	//allocBuffer
	mem := make([]byte, 1<<20)
	bm, err := createBufferManager([]*SizePercentPair{
		{Size: 4096, Percent: 50},
		{Size: 8192, Percent: 50},
	}, "", mem, 0)
	assert.Equal(t, nil, err)

	// fork 改了布局：buffer 按 align4K(cap+header) 步进、列表起始按 alignListOffset 对齐，
	// 因此段内可用容量不再是上游的 mem - 固定开销。这里按布局定义独立算一遍空闲容量
	// （同时校验 remainSize 的语义）。
	list0Offset := alignListOffset(uint32(bufferManagerHeaderSize))
	bufferRegionCap := uint64(len(mem)) - uint64(list0Offset) - uint64(bufferListHeaderSize)*2
	numOf4096 := uint32(bufferRegionCap*50/100) / align4K(4096+bufferHeaderSize)
	numOf8192 := uint32(bufferRegionCap*50/100) / align4K(8192+bufferHeaderSize)
	assert.Equal(t, numOf4096*4096+numOf8192*8192, bm.remainSize())
	assert.Equal(t, numOf4096, *bm.lists[0].cap)
	assert.Equal(t, numOf8192, *bm.lists[1].cap)

	numOfSlice := bm.sliceSize()
	buffers := make([]*bufferSlice, 0, 1024)
	for {
		buf, err := bm.allocShmBuffer(4096)
		if err != nil {
			break
		}
		buffers = append(buffers, buf)
	}
	for i := range buffers {
		bm.recycleBuffer(buffers[i])
	}
	buffers = buffers[:0]

	//allocBuffers, recycleBuffers
	slices := newSliceList()
	size := bm.allocShmBuffers(slices, 256*1024)
	assert.Equal(t, int(size), 256*1024)
	linkedBufferSlices := newEmptyLinkedBuffer(bm)
	for slices.size() > 0 {
		linkedBufferSlices.appendBufferSlice(slices.popFront())
	}
	linkedBufferSlices.done(false)
	bm.recycleBuffers(linkedBufferSlices.sliceList.popFront())
	assert.Equal(t, numOfSlice, bm.sliceSize())

	// nil 入参不应 panic
	bm.recycleBuffer(nil)
	bm.recycleBuffers(nil)
	// 超出最大 slice 尺寸时直接失败
	_, err = bm.allocShmBuffer((1 << 20) + 1)
	assert.Equal(t, ErrNoMoreBuffer, err)
}

func TestBufferList_PutPop(t *testing.T) {
	capPerBuffer := uint32(4096)
	bufferNum := uint32(1000)
	mem := make([]byte, countBufferListMemSize(bufferNum, capPerBuffer))

	l, err := createFreeBufferList(bufferNum, capPerBuffer, mem, 0)
	if err != nil {
		t.Fatal(err)
	}

	buffers := make([]*bufferSlice, 0, 1024)
	originSize := l.remain()
	for i := 0; l.remain() > 0; i++ {
		b, err := l.pop()
		if err != nil {
			t.Fatal(err)
		}
		buffers = append(buffers, b)
		assert.Equal(t, capPerBuffer, b.cap)
		assert.Equal(t, 0, b.size())
		assert.Equal(t, false, b.hasNext())
	}

	for i := range buffers {
		l.push(buffers[i])
	}

	assert.Equal(t, originSize, l.remain())
	for i := 0; l.remain() > 0; i++ {
		b, err := l.pop()
		if err != nil {
			t.Fatal(err)
		}
		buffers = append(buffers, b)
		assert.Equal(t, capPerBuffer, b.cap)
		assert.Equal(t, 0, b.size())
		assert.Equal(t, false, b.hasNext())
	}
}

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

func TestBufferList_CreateAndMappingFreeBufferList(t *testing.T) {
	capPerBuffer := uint32(10)
	bufferNum := uint32(10)
	mem := make([]byte, countBufferListMemSize(bufferNum, capPerBuffer))
	l, err := createFreeBufferList(0, capPerBuffer, mem, 0)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferList)(nil), l)

	mem = make([]byte, countBufferListMemSize(bufferNum, capPerBuffer))
	l, err = createFreeBufferList(bufferNum+1, capPerBuffer, mem, 0)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferList)(nil), l)

	mem = make([]byte, countBufferListMemSize(bufferNum, capPerBuffer))
	l, err = createFreeBufferList(bufferNum, capPerBuffer, mem, 0)
	assert.Equal(t, nil, err)
	assert.NotEqual(t, (*bufferList)(nil), l)

	testMem := make([]byte, 10)
	ml, err := mappingFreeBufferList(testMem, 0)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferList)(nil), ml)

	ml, err = mappingFreeBufferList(mem, 10)
	assert.NotEqual(t, nil, err)
	assert.Equal(t, (*bufferList)(nil), ml)

	ml, err = mappingFreeBufferList(mem, 0)
	assert.Equal(t, nil, err)
	assert.NotEqual(t, (*bufferList)(nil), ml)

	if err != nil {
		t.Fatalf("fail to mapping bufferlist:%s", err.Error())
	}
}

// TestBufferList_PushOffsetOutOfRange 覆盖 fork 在 BufferList.push 里新增的越界防御：
// offset 异常（下溢或越界）时只丢弃该 slice，不得破坏链表（size/counter/tail 不变），
// 且链表之后仍能正常 pop/push。
func TestBufferList_PushOffsetOutOfRange(t *testing.T) {
	capPerBuffer := uint32(4096)
	bufferNum := uint32(8)
	mem := make([]byte, countBufferListMemSize(bufferNum, capPerBuffer))
	l, err := createFreeBufferList(bufferNum, capPerBuffer, mem, 0)
	assert.Equal(t, nil, err)

	checkIntact := func(b *bufferSlice) {
		originSize := *l.size
		originCounter := *l.counter
		originTail := *l.tail

		l.push(b)

		// 防御性丢弃：链表状态一个字节都不该被改动
		assert.Equal(t, originSize, *l.size, "size must not change")
		assert.Equal(t, originCounter, *l.counter, "counter must not change")
		assert.Equal(t, originTail, *l.tail, "tail must not change")
		// slice 已被归还到池里（清零），不会残留脏状态
		assert.Equal(t, uint32(0), b.offsetInShm)
		assert.Equal(t, uint32(0), b.cap)
		assert.Equal(t, true, b.data == nil)
	}

	// case1: offsetInShm < bufferRegionOffsetInShm → newTail 下溢成大数
	b1, err := l.pop()
	assert.Equal(t, nil, err)
	b1.offsetInShm = l.bufferRegionOffsetInShm - 1
	checkIntact(b1)

	// case2: newTail == len(bufferRegion) → 走第二个条件（newTail + header > len）
	b2, err := l.pop()
	assert.Equal(t, nil, err)
	b2.offsetInShm = l.bufferRegionOffsetInShm + uint32(len(l.bufferRegion))
	checkIntact(b2)

	// case3: newTail 本身越界
	b3, err := l.pop()
	assert.Equal(t, nil, err)
	b3.offsetInShm = l.bufferRegionOffsetInShm + uint32(len(l.bufferRegion)) + 4096
	checkIntact(b3)

	// 链表仍然可用：剩余的 buffer 还能正常 pop/push
	assert.Equal(t, int32(bufferNum-3), *l.size)
	for i := 0; i < 4; i++ {
		b, err := l.pop()
		assert.Equal(t, nil, err)
		l.push(b)
	}
	assert.Equal(t, int32(bufferNum-3), *l.size)
}

func BenchmarkBufferList_PutPop(b *testing.B) {
	capPerBuffer := uint32(10)
	bufferNum := uint32(10000)
	mem := make([]byte, countBufferListMemSize(bufferNum, capPerBuffer))
	l, err := createFreeBufferList(bufferNum, capPerBuffer, mem, 0)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		buf, err := l.pop()
		if err != nil {
			b.Fatal(err)
		}
		l.push(buf)
	}
}

func BenchmarkBufferList_PutPopParallel(b *testing.B) {
	capPerBuffer := uint32(1)
	bufferNum := uint32(100 * 10000)
	mem := make([]byte, countBufferListMemSize(bufferNum, capPerBuffer))
	l, err := createFreeBufferList(bufferNum, capPerBuffer, mem, 0)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var err error
			var buf *bufferSlice
			buf, err = l.pop()
			for err != nil {
				time.Sleep(time.Millisecond)
				buf, err = l.pop()
			}
			l.push(buf)
		}
	})
}

func TestCreateFreeBufferList(t *testing.T) {
	_, err := createFreeBufferList(4294967295, 4294967295, []byte{'w'}, 4294967279)
	assert.NotNil(t, err)
}

// 覆盖 sizePercentPairs 的排序（Less/Swap）：入参乱序时，getGlobalBufferManager
// 会先 sort.Sort 再建列表，因此 bm.lists 应按 Size 升序。
func TestBufferManager_SortPairs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sort_pairs_shm")
	bm, err := getGlobalBufferManager(path, 1<<20, true, []*SizePercentPair{
		{Size: 64 * 1024, Percent: 10},
		{Size: 4096, Percent: 70},
		{Size: 16 * 1024, Percent: 20},
	})
	assert.Equal(t, nil, err)
	assert.NotEqual(t, (*bufferManager)(nil), bm)
	defer addGlobalBufferManagerRefCount(path, -1)

	assert.Equal(t, 3, len(bm.lists))
	assert.Equal(t, uint32(4096), *bm.lists[0].capPerBuffer)
	assert.Equal(t, uint32(16*1024), *bm.lists[1].capPerBuffer)
	assert.Equal(t, uint32(64*1024), *bm.lists[2].capPerBuffer)
	assert.Equal(t, uint32(4096), bm.minSliceSize)
	assert.Equal(t, uint32(64*1024), bm.maxSliceSize)

	// 二次获取命中全局 map，refCount 递增；释放两次后应被清理。
	bm2, err := getGlobalBufferManager(path, 1<<20, false, nil)
	assert.Equal(t, nil, err)
	assert.Equal(t, bm, bm2)
	addGlobalBufferManagerRefCount(path, -1)
	addGlobalBufferManagerRefCount(path, -1)
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