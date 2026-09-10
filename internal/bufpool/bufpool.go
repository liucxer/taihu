// Package bufpool 提供按 2 的幂分桶（4KB~8GB）的 4K 对齐缓冲池，供各层热路径复用，
// 消除每次 make 大块缓冲带来的高频堆分配与 GC 压力（设计 v3 中 server/rpcclient 的 IO 缓冲
// 与 device 的 O_DIRECT 对齐缓冲共用本池）。
//
// 约定：
//   - Get 返回 4K 对齐、len>=n 的切片，len 为 2 的幂（分桶容量）；
//   - Put 归还 Get 返回的原始切片或其子切片均可：按 cap 归一化回整桶容量再入桶，
//     保证再次 Get 到的是长度完整的桶容量缓冲（清空由消费方按需处理）。
//
// 实现说明：桶内采用自管理 freelist（互斥锁 + LIFO 栈），而非 sync.Pool。
// sync.Pool 会在每次 GC 时清空其中的对象，导致 4M/8M 大缓冲被整批丢弃、
// 每轮重走对齐分配（mallocgcLarge）与清零（memclr），实测读路径该冷分配
// 约占服务端 CPU 36%。自管理 freelist 不受 GC 影响：大缓冲常驻长期复用，
// 首次分配完成清零后，后续 Get 零分配、零清零；每桶以 maxKeep 上限约束驻留内存。
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
	// maxKeep 每桶常驻缓冲数量上限：约束驻留内存（如 4M 桶 32×4M=128MB），
	// 超出上限的归还缓冲直接丢弃交由 GC 回收。
	maxKeep = 32
)

// bytePool 单桶 freelist：mu 串行化 Get/Put，LIFO 复用最近归还的缓冲（缓存热）。
type bytePool struct {
	mu       sync.Mutex
	freelist [][]byte
}

// alignedBufPool 是对齐缓冲桶池，各桶独立 freelist，可并行 Get/Put。
type alignedBufPool struct {
	pools [maxBufBucket - logBlockSize + 1]*bytePool
}

var pool = newAlignedPool()

// newAlignedPool 一次性构建全部桶，避免运行期并发懒初始化（poolFor 写有时序竞态，
// netpoll 对齐分配器会让多个 goroutine 同时首次触达同一新桶而触发 -race）。此后
// poolFor 退化为纯读，线程安全。
func newAlignedPool() *alignedBufPool {
	p := &alignedBufPool{}
	for i := range p.pools {
		p.pools[i] = new(bytePool)
	}
	return p
}

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

// get 返回 4K 对齐、len>=n 的缓冲。优先复用 freelist 尾部（LIFO），
// 池空时才对齐分配一次（唯一一次清零成本）。
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

// put 将切片归还池：先按 cap 归一化到整桶容量（子切片也能回到正确桶），再入桶。
// 桶内驻留达到 maxKeep 上限时丢弃该缓冲（交由 GC 回收），避免驻留内存无限增长。
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

// poolFor 懒初始化获取桶（并发调用幂等，可能重复建桶但无正确性影响）。
func (p *alignedBufPool) poolFor(b int) *bytePool {
	bp := p.pools[b]
	if bp == nil {
		bp = new(bytePool)
		p.pools[b] = bp
	}
	return bp
}

// alignedBuffer 返回长度 n 且首地址按 4K 对齐的字节切片，
// 以满足 O_DIRECT 的缓冲对齐要求。多分配 BlockSize(4K) 用于对齐回退并切回正确长度。
//
// 返回值用三索引切片（cap == n）确保归还方按 cap 归一化后落在与 Get 一致的桶：
// 若 cap 保留为 (n+4096)-start（>n），Put 归一化后 len>n 会落入高一档桶，
// 导致 Get(n) 永远取不到本桶缓冲而持续冷分配（实测复用率 0 的根因）。
func alignedBuffer(n int) []byte {
	const blockSize = 4096
	backing := make([]byte, n+blockSize)
	start := int(uintptr(unsafe.Pointer(&backing[0])) % uintptr(blockSize))
	if start != 0 {
		start = blockSize - start
	}
	return backing[start : start+n : start+n]
}

// ---------- 精确尺寸对齐池（零拷贝 Take 收流节点/移交缓冲共用） ----------

// exactSizePool 按 len（==cap）分桶的精确尺寸对齐池：容量不按 2 幂取整。
// 服务对象是 netpoll 收流节点与客户端 Get 零拷贝移交缓冲：两者容量一致（=帧长），
// 节点缓冲经 TakeTry 移交后由调用方 PutExact 归还，即可被后续 Get/收流节点复用。
type exactSizePool struct {
	mu       sync.Mutex
	freelist map[int][][]byte
}

var exactPool = &exactSizePool{freelist: make(map[int][][]byte)}

// GetExact 返回 4K 对齐、len==n、cap==n 的精确尺寸缓冲（不按 2 幂取整）。
// n<=0 返回 nil。供 netpoll 收流节点（book 一帧一节点）与 Get 移交缓冲共用。
func GetExact(n int) []byte {
	if n <= 0 {
		return nil
	}
	exactPool.mu.Lock()
	if fl := exactPool.freelist[n]; len(fl) > 0 {
		buf := fl[len(fl)-1]
		exactPool.freelist[n] = fl[:len(fl)-1]
		exactPool.mu.Unlock()
		return buf
	}
	exactPool.mu.Unlock()
	return alignedBuffer(n)
}

// PutExact 归还精确尺寸缓冲：按 cap 归一化到原始容量再入对应桶，
// 使任意 cap==原始容量的子切片也能归位复用（TakeTry 场景归还整块 full，
// 其 len==cap==节点容量，直接按节点容量归桶）。
// 非池产物（nil 等）被忽略。
func PutExact(buf []byte) {
	if buf == nil || cap(buf) == 0 {
		return
	}
	if len(buf) < cap(buf) {
		buf = buf[:cap(buf)]
	}
	n := len(buf)
	exactPool.mu.Lock()
	fl := exactPool.freelist[n]
	if len(fl) < maxKeep {
		exactPool.freelist[n] = append(fl, buf)
	}
	exactPool.mu.Unlock()
}
