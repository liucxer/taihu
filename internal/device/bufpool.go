package device

import (
	"math/bits"
	"sync"
)

// 对齐缓冲池：按 2 的幂分桶（4KB~8GB），复用 O_DIRECT 读/写所需的 4K 对齐堆缓冲，
// 消除每次 make 大块缓冲带来的高频堆分配与 GC 压力
// （见 doc/461d092 读性能测试报告 §6.1：读路径 GC 热点 gcDrain ~64%）。
//
// 约定：
//   - get 返回 4K 对齐、len>=n 的切片，len 为 2 的幂（分桶容量）；
//   - put 必须原样归还 get 返回的切片（未 reslice），按 len 回到原桶。
const (
	// logBlockSize = log2(4096) = 12，O_DIRECT 对齐粒度（与 layout.BlockSize 一致）。
	logBlockSize = 12
	// maxBufBucket 覆盖到 2^33 = 8GB >= SegmentSizeBytes。
	maxBufBucket = logBlockSize + 21 // 33，8GB
)

var bufPool = newAlignedBufPool()

// alignedBufPool 是对齐缓冲桶池，各桶独立 sync.Pool，可并行 Get/Put。
type alignedBufPool struct {
	pools [maxBufBucket - logBlockSize + 1]*sync.Pool
}

func newAlignedBufPool() *alignedBufPool {
	return &alignedBufPool{}
}

// bufBucket 返回 n 向上取 2 的幂（下限 4KB、上限 8GB）对应的桶索引。
func bufBucket(n int) int {
	if n <= 0 {
		return 0
	}
	b := bits.Len64(uint64(n - 1)) // n>0 时向上取整 2 幂的指数
	if b < logBlockSize {
		b = logBlockSize
	}
	return b - logBlockSize
}

// get 返回 4K 对齐、len>=n 的缓冲。缓冲取自池，池空时新分配。
func (p *alignedBufPool) get(n int) []byte {
	b := bufBucket(n)
	pool := p.pools[b]
	if pool == nil {
		pool = &sync.Pool{}
		p.pools[b] = pool // 并发 lazy init 幂等
	}
	if v := pool.Get(); v != nil {
		return v.([]byte)
	}
	return alignedBuffer(int(int64(1) << uint(b+logBlockSize)))
}

// put 将 get 返回的原始切片归还池。非池产物（nil 等）被忽略。
func (p *alignedBufPool) put(buf []byte) {
	if buf == nil {
		return
	}
	b := bufBucket(len(buf))
	if b > maxBufBucket-logBlockSize {
		return
	}
	p.pools[b].Put(buf)
}