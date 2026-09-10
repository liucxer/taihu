// Package tbpool 是 taihu internal/bufpool 的 grpc module 内副本（third_party/grpc 为
// 独立 module，无法 import 上层 taihu 包）。提供 4K 对齐的 2 的幂分桶缓冲池
// （自管理 freelist，不受 GC 清空），并附带 FrameBuffer（引用计数的 mem.Buffer 包装），
// 供 transport 收帧（handleData）使用：让帧数据缓冲直接落在对齐桶中，
// 应用层（taihu rpc.RawFrame.Take）可经 duck-type Raw() 零拷贝移交，免除收流聚合拷贝。
package tbpool

import (
	"math/bits"
	"sync"
	"sync/atomic"
	"unsafe"

	"google.golang.org/grpc/mem"
)

const (
	// logBlockSize = log2(4096) = 12，O_DIRECT 对齐粒度。
	logBlockSize = 12
	// maxBufBucket 覆盖到 2^33 = 8GB。
	maxBufBucket = logBlockSize + 21 // 33，8GB
	// maxKeep 每桶常驻缓冲数量上限：约束驻留内存（如 4M 桶 32×4M=128MB）。
	maxKeep = 32
)

// bytePool 单桶 freelist：mu 串行化 Get/Put，LIFO 复用。
type bytePool struct {
	mu       sync.Mutex
	freelist [][]byte
}

// alignedBufPool 是 4K 对齐缓冲桶池，各桶独立 freelist。
type alignedBufPool struct {
	pools [maxBufBucket - logBlockSize + 1]*bytePool
}

var pool = &alignedBufPool{}

// Get 返回 4K 对齐、len>=n 的缓冲（len 为 2 的幂桶容量）。
func Get(n int) []byte {
	if n < 0 {
		n = 0
	}
	return pool.get(n)
}

// Put 将 Get 返回的切片（或其子切片）归还池。
func Put(buf []byte) {
	if buf == nil {
		return
	}
	pool.put(buf)
}

// bufBucket 返回 n 向上取 2 的幂（下限 4KB）对应的桶索引。
func bufBucket(n int) int {
	b := bits.Len64(uint64(n - 1))
	if b < logBlockSize {
		b = logBlockSize
	}
	return b - logBlockSize
}

func (p *alignedBufPool) get(n int) []byte {
	b := bufBucket(n)
	bp := p.poolFor(b)
	bp.mu.Lock()
	if m := len(bp.freelist); m > 0 {
		buf := bp.freelist[m-1]
		bp.freelist = bp.freelist[:m-1]
		bp.mu.Unlock()
		return buf
	}
	bp.mu.Unlock()
	return alignedBuffer(int(int64(1) << uint(b+logBlockSize)))
}

func (p *alignedBufPool) put(buf []byte) {
	if cap(buf) == 0 {
		return
	}
	if len(buf) < cap(buf) {
		buf = buf[:cap(buf)]
	}
	b := bufBucket(len(buf))
	if b > maxBufBucket-logBlockSize {
		return
	}
	bp := p.poolFor(b)
	bp.mu.Lock()
	if len(bp.freelist) < maxKeep {
		bp.freelist = append(bp.freelist, buf)
		bp.mu.Unlock()
		return
	}
	bp.mu.Unlock()
}

func (p *alignedBufPool) poolFor(b int) *bytePool {
	bp := p.pools[b]
	if bp == nil {
		bp = new(bytePool)
		p.pools[b] = bp
	}
	return bp
}

// alignedBuffer 返回长度 n 且首地址按 4K 对齐的字节切片。
// 三索引切片（cap == n）保证 Put 按 cap 归一化后落回与 Get 一致的桶。
func alignedBuffer(n int) []byte {
	const blockSize = 4096
	backing := make([]byte, n+blockSize)
	start := int(uintptr(unsafe.Pointer(&backing[0])) % uintptr(blockSize))
	if start != 0 {
		start = blockSize - start
	}
	return backing[start : start+n : start+n]
}

// frameAdapter 将本池适配为 mem.BufferPool（FrameBuffer 生命周期归池用）。
type frameAdapter struct{}

func (frameAdapter) Get(n int) *[]byte {
	b := Get(n)
	return &b
}

func (frameAdapter) Put(b *[]byte) {
	if b != nil {
		Put(*b)
	}
}

// FrameBuffer 包装一块本池对齐缓冲为 mem.Buffer（引用计数，归零归还 tbpool）。
// Raw() 暴露底层切片，供 taihu rpc.RawFrame.Take 零拷贝移交调用方。
type FrameBuffer struct {
	mem.Buffer
	raw  []byte
	refs atomic.Int32
}

// NewFrameBuffer 基于一块本池对齐切片创建 FrameBuffer（引用计数初始 1）。
func NewFrameBuffer(raw []byte) *FrameBuffer {
	b := &FrameBuffer{Buffer: mem.NewBuffer(&raw, frameAdapter{}), raw: raw}
	b.refs.Store(1)
	return b
}

func (b *FrameBuffer) Ref() { b.refs.Add(1) }

func (b *FrameBuffer) Free() {
	if b.refs.Add(-1) == 0 && b.raw != nil {
		Put(b.raw)
		b.raw = nil
	}
}

// Raw 返回底层池切片（len 即创建时长度，数据视图完整）。
func (b *FrameBuffer) Raw() []byte { return b.raw }

// CopyToFrame 将 src 拷贝进一块本池对齐缓冲并包装为 FrameBuffer 返回。
// 数据拷贝发生在此（等价 handleData 的 mem.Copy），后续可被 Take 零拷贝移交。
func CopyToFrame(src []byte) *FrameBuffer {
	raw := Get(len(src))
	copy(raw[:len(src)], src)
	return NewFrameBuffer(raw[:len(src)])
}