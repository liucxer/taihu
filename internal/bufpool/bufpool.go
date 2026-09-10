// Package bufpool 提供按 2 的幂分桶（4KB~8GB）的 4K 对齐缓冲池，供各层热路径复用，
// 消除每次 make 大块缓冲带来的高频堆分配与 GC 压力（设计 v3 中 server/rpcclient 的 IO 缓冲
// 与 device 的 O_DIRECT 对齐缓冲共用本池）。
//
// 约定：
//   - Get 返回 4K 对齐、len>=n 的切片，len 为 2 的幂（分桶容量）；
//   - Put 归还 Get 返回的原始切片或其子切片均可：按 cap 归一化回整桶容量再入桶，
//     保证再次 Get 到的是长度完整的桶容量缓冲（清空由消费方按需处理）。
package bufpool

import (
	"math/bits"
	"sync"
	"unsafe"
)

const (
	// logBlockSize = log2(4096) = 12，O_DIRECT 对齐粒度。
	logBlockSize = 12
	// maxBufBucket 覆盖到 2^33 = 8GB >= SegmentSizeBytes。
	maxBufBucket = logBlockSize + 21 // 33，8GB
)

// alignedBufPool 是对齐缓冲桶池，各桶独立 sync.Pool，可并行 Get/Put。
type alignedBufPool struct {
	pools [maxBufBucket - logBlockSize + 1]*sync.Pool
}

var pool = &alignedBufPool{}

// Get 返回 4K 对齐、len>=n 的缓冲。缓冲取自池，池空时新分配。
func Get(n int) []byte {
	if n < 0 {
		n = 0
	}
	return pool.get(n)
}

// Put 将 Get 返回的切片（或其子切片）归还池。非池产物（nil 等）被忽略。
func Put(buf []byte) {
	if buf == nil {
		return
	}
	pool.put(buf)
}

// bufBucket 返回 n 向上取 2 的幂（下限 4KB、上限 8GB）对应的桶索引。
func bufBucket(n int) int {
	b := bits.Len64(uint64(n - 1)) // n>0 时向上取整 2 幂的指数
	if b < logBlockSize {
		b = logBlockSize
	}
	return b - logBlockSize
}

// get 返回 4K 对齐、len>=n 的缓冲。缓冲取自池，池空时新分配。
func (p *alignedBufPool) get(n int) []byte {
	b := bufBucket(n)
	bp := p.pools[b]
	if bp == nil {
		bp = &sync.Pool{}
		p.pools[b] = bp // 并发 lazy init 幂等
	}
	if v := bp.Get(); v != nil {
		return v.([]byte)
	}
	return alignedBuffer(int(int64(1) << uint(b+logBlockSize)))
}

// put 将切片归还池：先按 cap 归一化到整桶容量（子切片也能回到正确桶），再入桶。
// 非池产物（nil 等）被忽略。
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
	bp := p.pools[b]
	if bp == nil {
		bp = &sync.Pool{}
		p.pools[b] = bp // cap 归一化可能落到从未 Get 过的桶，按需懒创建
	}
	bp.Put(buf)
}

// alignedBuffer 返回长度 n 且首地址按 4K 对齐的字节切片，
// 以满足 O_DIRECT 的缓冲对齐要求。多分配 BlockSize(4K) 用于对齐回退并切回正确长度。
func alignedBuffer(n int) []byte {
	const blockSize = 4096
	backing := make([]byte, n+blockSize)
	start := int(uintptr(unsafe.Pointer(&backing[0])) % uintptr(blockSize))
	if start != 0 {
		start = blockSize - start
	}
	return backing[start : start+n]
}
