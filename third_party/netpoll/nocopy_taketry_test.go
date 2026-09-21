// Copyright 2024 CloudWeGo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package netpoll

import (
	"encoding/binary"
	"sync"
	"testing"
)

// testPool 模拟 bufpool 精确尺寸池：按 len==cap 分桶复用，记录 put 次数。
type testPool struct {
	mu   sync.Mutex
	fl   map[int][][]byte
	puts []int
}

func newTestPool() *testPool { return &testPool{fl: make(map[int][][]byte)} }

func (p *testPool) get(n int) []byte {
	p.mu.Lock()
	if l := p.fl[n]; len(l) > 0 {
		b := l[len(l)-1]
		p.fl[n] = l[:len(l)-1]
		p.mu.Unlock()
		return b
	}
	p.mu.Unlock()
	b := make([]byte, n)
	return b[:n:n]
}

func (p *testPool) put(b []byte) {
	if len(b) < cap(b) {
		b = b[:cap(b)]
	}
	p.mu.Lock()
	p.puts = append(p.puts, cap(b))
	p.fl[len(b)] = append(p.fl[len(b)], b)
	p.mu.Unlock()
}

func (p *testPool) putCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.puts)
}

// setupTakeTest 注入精确尺寸分配器与节点容量上限，返回清理函数。
func setupTakeTest(t *testing.T, nodeSize int) *testPool {
	t.Helper()
	pool := newTestPool()
	SetInputAlignedAllocator(pool.get, pool.put)
	SetInputNodeSize(nodeSize)
	t.Cleanup(func() {
		SetInputAlignedAllocator(nil, nil)
		SetInputNodeSize(0)
	})
	return pool
}

// putFrame 写入一帧 [len(4)][sid(4)][op(1)][payload]，返回线上字节数。
func putFrame(dst []byte, sid uint32, op byte, payload []byte) int {
	lenField := 4 + 1 + len(payload) // sid(4)+op(1)+payload
	binary.BigEndian.PutUint32(dst[0:4], uint32(lenField))
	binary.BigEndian.PutUint32(dst[4:8], sid)
	dst[8] = op
	copy(dst[9:], payload)
	return 4 + lenField
}

// consumeHeader 模拟 transport 读循环：跳过长度字段、取 sid、读 op。
func consumeHeader(r Reader) {
	_ = r.Skip(4)
	_, _ = r.Next(4)
	_, _ = r.ReadByte()
}

// booker 是 fillInput 所需的最小接口：race 构建下 LinkBuffer=SafeLinkBuffer，
// 不能写死 *UnsafeLinkBuffer（book 经内嵌提升，两种构建都满足）。
type booker interface {
	book(bookSize, maxSize int) []byte
	bookAck(n int) (length int, err error)
}

// fillInput 模拟一次 readv：book 分配节点、写入 n 字节并 ack。
func fillInput(lb booker, bookSize, n int) []byte {
	p := lb.book(bookSize, bookSize)
	p = p[:n]
	_, _ = lb.bookAck(n)
	return p
}

// frameSlice 取出一帧子 Reader 并消费帧头（单节点路径）。
func frameSlice(t *testing.T, lb Reader, wire int) Reader {
	t.Helper()
	sub, err := lb.Slice(wire)
	if err != nil {
		t.Fatalf("Slice(%d): %v", wire, err)
	}
	consumeHeader(sub)
	return sub
}

// takeTry 对 Reader 做 TakeTry 断言并调用（契约：buf 负载切片 + full 整块缓冲）。
func takeTry(r Reader) ([]byte, []byte, bool) {
	tt, ok := r.(interface{ TakeTry() ([]byte, []byte, bool) })
	if !ok {
		return nil, nil, false
	}
	return tt.TakeTry()
}

// TestTakeTrySingleFrame 整响应恰一帧：TakeTry 零拷贝移交，缓冲与收流缓冲同底，
// 且子 Reader/主 Reader 的越界 Release 不会二次归还缓冲。
func TestTakeTrySingleFrame(t *testing.T) {
	const nodeSize = 4 + 5 + 4 // 线上总长：4B len + 5B 头 + 4B 负载
	pool := setupTakeTest(t, nodeSize)

	lb := NewLinkBuffer(0)
	payload := []byte{0x11, 0x22, 0x33, 0x44}
	buf := fillInput(lb, nodeSize, nodeSize)
	putFrame(buf, 7, 0x06, payload) // opGetData

	sub := frameSlice(t, lb, nodeSize)
	if sub.Len() != len(payload) {
		t.Fatalf("payload len = %d, want %d", sub.Len(), len(payload))
	}
	taken, full, ok := takeTry(sub)
	if !ok {
		t.Fatalf("TakeTry failed for single full frame")
	}
	if string(taken) != string(payload) {
		t.Fatalf("taken = %x, want %x", taken, payload)
	}
	// buf 为负载切片（len==cap==负载长），full 为整块缓冲（len==cap==节点容量）。
	if cap(taken) != len(payload) {
		t.Fatalf("cap(taken) = %d, want %d", cap(taken), len(payload))
	}
	if len(full) != nodeSize || cap(full) != nodeSize {
		t.Fatalf("full len/cap = %d/%d, want %d", len(full), cap(full), nodeSize)
	}
	// 零拷贝：负载与收流节点缓冲同底（偏移 9 = 4 len + 5 头），full 与节点缓冲同底。
	if &taken[0] != &buf[9] {
		t.Fatalf("taken buffer not zero-copy backed by input node")
	}
	if &full[0] != &buf[0] {
		t.Fatalf("full buffer not backed by input node")
	}

	// 调用方归还整块缓冲（对应 transport 的 PutExact）。
	pool.put(full)
	if pool.putCount() != 1 {
		t.Fatalf("put count = %d, want 1", pool.putCount())
	}
	// 防御：越界 Release 子 Reader / 主 Reader 均不得二次归还缓冲。
	if err := sub.Release(); err != nil {
		t.Fatalf("sub.Release: %v", err)
	}
	if err := lb.Release(); err != nil {
		t.Fatalf("lb.Release: %v", err)
	}
	if pool.putCount() != 1 {
		t.Fatalf("double return: puts = %d, want 1", pool.putCount())
	}
}

// TestTakeTryUnfilledNode 节点未写满（book 仍会复用其缓冲）时拒绝移交，
// 之后 book 确实复用该节点写入后续数据——证明移交拒绝避免了缓冲归属错乱。
func TestTakeTryUnfilledNode(t *testing.T) {
	const nodeSize = 4 + 5 + 4
	pool := setupTakeTest(t, nodeSize)

	lb := NewLinkBuffer(0)
	wire := 10 // payload 1 字节：线上 4+5+1
	buf := fillInput(lb, nodeSize, wire)
	putFrame(buf, 7, 0x06, []byte{0x01})

	sub := frameSlice(t, lb, wire)
	if _, _, ok := takeTry(sub); ok {
		t.Fatalf("TakeTry succeeded on unfilled node, want refuse")
	}
	// 回退拷贝路径仍可用。
	p, err := sub.Next(1)
	if err != nil || p[0] != 0x01 {
		t.Fatalf("fallback Next: p=%v err=%v", p, err)
	}
	if err := sub.Release(); err != nil {
		t.Fatalf("sub.Release: %v", err)
	}
	if pool.putCount() != 0 {
		t.Fatalf("put count = %d, want 0 (no take)", pool.putCount())
	}
	// 主 Reader 释放后，book 复用同节点剩余容量写入后续数据（安全：未移交）。
	n2 := fillInput(lb, nodeSize, nodeSize-wire)
	if len(n2) != nodeSize-wire {
		t.Fatalf("reuse node fill len = %d", len(n2))
	}
	if pool.putCount() != 0 {
		t.Fatalf("put count = %d, want 0", pool.putCount())
	}
}

// TestTakeTryMultiFrameNode 多帧同节点：非末帧拒绝移交（origin 仍有未读数据），
// 末帧（节点写满、引用唯一）也拒绝——整块缓冲前段属于帧A，本帧非独占（base != 0）。
func TestTakeTryMultiFrameNode(t *testing.T) {
	const nodeSize = 30 // 帧A wire 10 + 帧B wire 20
	pool := setupTakeTest(t, nodeSize)

	lb := NewLinkBuffer(0)
	buf := fillInput(lb, nodeSize, nodeSize)
	wa := putFrame(buf, 1, 0x06, []byte{0xAA})
	wb := putFrame(buf[wa:], 2, 0x06, []byte("hello-taket")) // 11 字节负载
	if wa+wb != nodeSize {
		t.Fatalf("wa+wb = %d, want %d", wa+wb, nodeSize)
	}

	// 帧A（非末帧）：拒绝移交。
	subA := frameSlice(t, lb, wa)
	if _, _, ok := takeTry(subA); ok {
		t.Fatalf("TakeTry succeeded for non-last frame, want refuse")
	}
	pA, _ := subA.Next(1)
	if pA[0] != 0xAA {
		t.Fatalf("frame A payload = %x", pA)
	}
	if err := subA.Release(); err != nil {
		t.Fatalf("subA.Release: %v", err)
	}

	// 帧B（末帧，节点写满、origin 已读完、引用唯一）：仍非独占（base = wb != 0），
	// 拒绝移交并回退拷贝路径——整块缓冲前段是帧A 的数据，不能一并交出去。
	subB := frameSlice(t, lb, wb)
	if subB.Len() != 11 {
		t.Fatalf("frame B payload len = %d, want 11", subB.Len())
	}
	if _, _, ok := takeTry(subB); ok {
		t.Fatalf("TakeTry succeeded for non-exclusive last frame (base != 0), want refuse")
	}
	pB, err := subB.Next(11)
	if err != nil || string(pB) != "hello-taket" {
		t.Fatalf("fallback subB: p=%q err=%v, want %q", pB, err, "hello-taket")
	}
	if err := subB.Release(); err != nil {
		t.Fatalf("subB.Release: %v", err)
	}
	if pool.putCount() != 0 {
		t.Fatalf("put count = %d, want 0 (no take)", pool.putCount())
	}
}

// TestTakeTryMultiNodeFrame 帧跨多节点（maxSize 小于帧长，连接建链后的爬坡期）：
// Slice 生成多节点子 Reader，TakeTry 拒绝，回退拷贝路径可读。
func TestTakeTryMultiNodeFrame(t *testing.T) {
	const nodeSize = 20
	setupTakeTest(t, nodeSize)

	lb := NewLinkBuffer(0)
	// 帧 wire = 16，跨两个 8B 节点。
	var raw [16]byte
	wire := putFrame(raw[:], 3, 0x06, []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03})
	if wire != 16 {
		t.Fatalf("wire = %d, want 16", wire)
	}
	n1 := fillInput(lb, 8, 8)
	copy(n1, raw[:8])
	n2 := fillInput(lb, 8, 8)
	copy(n2, raw[8:])

	sub := frameSlice(t, lb, wire)
	if _, _, ok := takeTry(sub); ok {
		t.Fatalf("TakeTry succeeded for multi-node frame, want refuse")
	}
	p, err := sub.Next(wire - 9) // 负载长
	if err != nil || len(p) != 7 || p[0] != 0xDE {
		t.Fatalf("fallback Next: len=%d err=%v", len(p), err)
	}
	if err := sub.Release(); err != nil {
		t.Fatalf("sub.Release: %v", err)
	}
}

// TestTakeTryPoolRecycle 移交缓冲经 put 归还后，可被后续 book 复用（池循环成立）。
func TestTakeTryPoolRecycle(t *testing.T) {
	const nodeSize = 4 + 5 + 4
	pool := setupTakeTest(t, nodeSize)

	lb := NewLinkBuffer(0)
	buf := fillInput(lb, nodeSize, nodeSize)
	putFrame(buf, 7, 0x06, []byte{0x11, 0x22, 0x33, 0x44})
	sub := frameSlice(t, lb, nodeSize)
	_, full, ok := takeTry(sub)
	if !ok {
		t.Fatalf("TakeTry failed")
	}
	pool.put(full) // 调用方归还整块缓冲

	// 后续收流：book 应从池中复用同一缓冲（cap == nodeSize）。
	lb2 := NewLinkBuffer(0)
	next := fillInput(lb2, nodeSize, nodeSize)
	if cap(next) != nodeSize {
		t.Fatalf("recycled node cap = %d, want %d", cap(next), nodeSize)
	}
	if len(next) > 0 && &next[0] != &full[0] {
		t.Fatalf("book did not reuse taken buffer")
	}
}

// TestTakeTryNoReclaimBeforeCallerPut 移交成功后、调用方 put 之前，缓冲绝不能被
// 归还池：一旦提前归还，后续收流的 book 会取到同一缓冲并写入新帧，把调用方正在读的
// 数据静默改写（E3 内容错配的成因类别）。同时锁定「主/子 Reader 随后释放也不得
// 二次归还」的不变式。
func TestTakeTryNoReclaimBeforeCallerPut(t *testing.T) {
	const nodeSize = 4 + 5 + 4
	pool := setupTakeTest(t, nodeSize)
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	lb := NewLinkBuffer(0)
	buf := fillInput(lb, nodeSize, nodeSize)
	putFrame(buf, 7, 0x06, payload)
	sub := frameSlice(t, lb, nodeSize)

	taken, full, ok := takeTry(sub)
	if !ok {
		t.Fatalf("TakeTry failed for single full frame")
	}
	base := pool.putCount()

	// 读循环随后位移主 Reader 并释放已消费节点（并发语义下的典型序列）。
	if err := lb.Release(); err != nil {
		t.Fatalf("lb.Release: %v", err)
	}
	if n := pool.putCount(); n != base {
		t.Fatalf("taken buffer reclaimed before caller put: puts=%d, want %d", n, base)
	}
	// 后续收流不得复用该缓冲：调用方数据必须保持不变。
	next := fillInput(lb, nodeSize, nodeSize)
	if &next[0] == &full[0] {
		t.Fatalf("subsequent book reused the taken buffer while caller still holds it")
	}
	if string(taken) != string(payload) {
		t.Fatalf("taken payload overwritten: got %x, want %x", taken, payload)
	}
	// 调用方归还：恰一次入池。
	pool.put(full)
	if n := pool.putCount(); n != base+1 {
		t.Fatalf("puts = %d, want %d", n, base+1)
	}
	// 防御：越界 Release 子 Reader 不得二次归还。
	if err := sub.Release(); err != nil {
		t.Fatalf("sub.Release: %v", err)
	}
	if n := pool.putCount(); n != base+1 {
		t.Fatalf("double return after take: puts=%d, want %d", n, base+1)
	}
}

// TestTakeTryRefusedAfterMainRelease 主 Reader 已释放该节点（refer 降为 1，缓冲
// 可能已被回收）时必须拒绝移交；且拒绝路径必须撤销 flagUnmanaged，否则该节点缓冲
// 既不再被 netpoll 回收、也无从移交，形成池泄漏。
func TestTakeTryRefusedAfterMainRelease(t *testing.T) {
	const nodeSize = 4 + 5 + 4
	pool := setupTakeTest(t, nodeSize)

	lb := NewLinkBuffer(0)
	buf := fillInput(lb, nodeSize, nodeSize)
	putFrame(buf, 7, 0x06, []byte{0x11, 0x22, 0x33, 0x44})
	sub := frameSlice(t, lb, nodeSize)

	// 再收一帧（新节点），随后主 Reader 释放已消费的节点 A → A.refer 降为 1。
	buf2 := fillInput(lb, nodeSize, nodeSize)
	putFrame(buf2, 8, 0x06, []byte{0x55, 0x66, 0x77, 0x88})
	if err := lb.Release(); err != nil {
		t.Fatalf("lb.Release: %v", err)
	}
	if _, _, ok := takeTry(sub); ok {
		t.Fatalf("TakeTry succeeded after main reader released the node, want refuse")
	}
	// 读取负载后释放子 Reader：节点缓冲应正常回收进池（证明 unmanaged 已撤销）。
	if _, err := sub.Next(4); err != nil {
		t.Fatalf("sub.Next: %v", err)
	}
	if err := sub.Release(); err != nil {
		t.Fatalf("sub.Release: %v", err)
	}
	if n := pool.putCount(); n != 1 {
		t.Fatalf("refused take left the buffer unmanaged: puts=%d, want 1", n)
	}
}
