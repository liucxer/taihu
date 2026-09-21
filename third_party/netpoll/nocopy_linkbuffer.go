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
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/bytedance/gopkg/lang/dirtmake"
)

// BinaryInplaceThreshold marks the minimum value of the nocopy slice length,
// which is the threshold to use copy to minimize overhead.
const BinaryInplaceThreshold = block4k

// LinkBufferCap that can be modified marks the minimum value of each node of LinkBuffer.
var LinkBufferCap = block4k

var untilErr = errors.New("link buffer read slice cannot find delim")

var (
	_ Reader = &LinkBuffer{}
	_ Writer = &LinkBuffer{}
)

// NewLinkBuffer size defines the initial capacity, but there is no readable data.
func NewLinkBuffer(size ...int) *LinkBuffer {
	buf := &LinkBuffer{}
	var l int
	if len(size) > 0 {
		l = size[0]
	}
	node := newLinkBufferNode(l)
	buf.head, buf.read, buf.flush, buf.write = node, node, node, node
	return buf
}

// UnsafeLinkBuffer implements ReadWriter.
type UnsafeLinkBuffer struct {
	length     int64
	mallocSize int

	head  *linkBufferNode // release head
	read  *linkBufferNode // read head
	flush *linkBufferNode // malloc head
	write *linkBufferNode // malloc tail

	// buf allocated by Next when cross-package, which should be freed when release
	caches [][]byte

	// for `Peek` only, avoid creating too many []byte in `caches`
	// fix the issue when we have a large buffer and we call `Peek` multiple times
	cachePeek []byte
}

// Len implements Reader.
func (b *UnsafeLinkBuffer) Len() int {
	l := atomic.LoadInt64(&b.length)
	return int(l)
}

// IsEmpty check if this LinkBuffer is empty.
func (b *UnsafeLinkBuffer) IsEmpty() (ok bool) {
	return b.Len() == 0
}

// ------------------------------------------ implement copy reader ------------------------------------------

// ReadCopy copies up to len(p) bytes from the buffer directly into p without
// exposing the underlying buffer to user code (flagReadExposed is not set),
// and without any intermediate allocation or copy—the only copy is into p
// itself. After copying, it releases consumed nodes where readExposed is false.
// Nodes with readExposed are left for the next Release call.
func (b *UnsafeLinkBuffer) ReadCopy(p []byte) (n int, err error) {
	l := len(p)
	if l == 0 || b.Len() == 0 {
		return 0, nil
	}
	if has := b.Len(); has < l {
		l = has
	}
	b.recalLen(-l)

	// copy from nodes
	for ack := l; ack > 0; {
		if b.read.Len() == 0 {
			b.read = b.read.next
			continue
		}
		rd := b.read.Len()
		if rd >= ack {
			n += copy(p[n:], b.read.buf[b.read.off:b.read.off+ack])
			b.read.off += ack
			break
		}
		n += copy(p[n:], b.read.buf[b.read.off:])
		ack -= rd
		b.read = b.read.next
	}

	// advance read past empty nodes
	for b.read != b.flush && b.read.Len() == 0 {
		b.read = b.read.next
	}
	// release consumed nodes that are not readExposed.
	// exposed nodes stay in the chain so Release() can free them later.
	//
	// Example: [exposed/consumed] → [not-exposed/consumed] → [read/partial]
	// After:   head → [exposed] → [read/partial]
	//          the middle node is detached and released.
	var prev *linkBufferNode
	newHead := b.read
	for cur := b.head; cur != b.read; {
		next := cur.next
		if cur.readExposed() {
			if prev == nil {
				newHead = cur
			}
			prev = cur
		} else {
			cur.Release()
			if prev != nil {
				prev.next = next
			}
		}
		cur = next
	}
	b.head = newHead
	return n, nil
}

// ------------------------------------------ implement zero-copy reader ------------------------------------------

// Next implements Reader.
func (b *UnsafeLinkBuffer) Next(n int) (p []byte, err error) {
	if n <= 0 {
		return
	}
	// check whether enough or not.
	if b.Len() < n {
		return p, fmt.Errorf("link buffer next[%d] not enough", n)
	}
	b.recalLen(-n) // re-cal length

	// single node
	if b.isSingleNode(n) {
		b.read.setFlag(flagReadExposed)
		return b.read.Next(n), nil
	}
	// multiple nodes
	var pIdx int
	if block1k < n && n <= mallocMax {
		p = malloc(n, n)
		b.caches = append(b.caches, p)
	} else {
		p = dirtmake.Bytes(n, n)
	}
	var l int
	for ack := n; ack > 0; ack = ack - l {
		l = b.read.Len()
		if l >= ack {
			pIdx += copy(p[pIdx:], b.read.Next(ack))
			break
		} else if l > 0 {
			pIdx += copy(p[pIdx:], b.read.Next(l))
		}
		b.read = b.read.next
	}
	_ = pIdx
	return p, nil
}

// Peek does not have an independent lifecycle, and there is no signal to
// indicate that Peek content can be released, so Peek will not introduce mcache for now.
func (b *UnsafeLinkBuffer) Peek(n int) (p []byte, err error) {
	if n <= 0 {
		return
	}
	// check whether enough or not.
	if b.Len() < n {
		return p, fmt.Errorf("link buffer peek[%d] not enough", n)
	}
	// single node
	if b.isSingleNode(n) {
		b.read.setFlag(flagReadExposed)
		return b.read.Peek(n), nil
	}

	// multiple nodes

	// try to make use of the cap of b.cachePeek, if can't, free it.
	if b.cachePeek != nil && cap(b.cachePeek) < n {
		free(b.cachePeek)
		b.cachePeek = nil
	}
	if b.cachePeek == nil {
		b.cachePeek = malloc(0, n) // init with zero len, will append later
	}
	p = b.cachePeek
	if len(p) >= n {
		// in case we peek smaller than last time,
		// we can return cache data directly.
		// we will reset cachePeek when Next or Skip, no worries about stale data
		return p[:n], nil
	}

	// How it works >>>>>>
	// [ -------- node0 -------- ][ --------- node1 --------- ]  <- b.read
	// [ --------------- p --------------- ]
	//                                     ^ len(p)     ^ n here
	//                           ^ scanned
	// `scanned` var is the len of last nodes which we scanned and already copied to p
	// `len(p) - scanned` is the start pos of current node for p to copy from
	// `n - len(p)` is the len of bytes we're going to append to p
	// 		we copy `len(node1)` - `len(p) - scanned` bytes in case node1 doesn't have enough data
	for scanned, node := 0, b.read; len(p) < n; node = node.next {
		l := node.Len()
		if scanned+l <= len(p) { // already copied in p, skip
			scanned += l
			continue
		}
		start := len(p) - scanned // `start` must be smaller than l coz `scanned+l <= len(p)` is false
		copyn := n - len(p)
		if nodeLeftN := l - start; copyn > nodeLeftN {
			copyn = nodeLeftN
		}
		p = append(p, node.Peek(l)[start:start+copyn]...)
		scanned += l
	}
	b.cachePeek = p
	return p[:n], nil
}

// Skip implements Reader.
func (b *UnsafeLinkBuffer) Skip(n int) (err error) {
	if n <= 0 {
		return
	}
	// check whether enough or not.
	if b.Len() < n {
		return fmt.Errorf("link buffer skip[%d] not enough", n)
	}
	b.recalLen(-n) // re-cal length

	var l int
	for ack := n; ack > 0; ack = ack - l {
		l = b.read.Len()
		if l >= ack {
			b.read.off += ack
			break
		}
		b.read = b.read.next
	}
	return nil
}

// Release the node that has been read.
// b.flush == nil indicates that this LinkBuffer is created by LinkBuffer.Slice
func (b *UnsafeLinkBuffer) Release() (err error) {
	for b.read != b.flush && b.read.Len() == 0 {
		b.read = b.read.next
	}
	for b.head != b.read {
		node := b.head
		b.head = b.head.next
		node.Release()
	}
	for i := range b.caches {
		free(b.caches[i])
		b.caches[i] = nil
	}
	b.caches = b.caches[:0]
	if b.cachePeek != nil {
		free(b.cachePeek)
		b.cachePeek = nil
	}
	return nil
}

// ReadString implements Reader.
func (b *UnsafeLinkBuffer) ReadString(n int) (s string, err error) {
	if n <= 0 {
		return
	}
	// check whether enough or not.
	if b.Len() < n {
		return s, fmt.Errorf("link buffer read string[%d] not enough", n)
	}
	p := b.readBinary(n)
	return unsafe.String(unsafe.SliceData(p), len(p)), nil
}

// ReadBinary implements Reader.
func (b *UnsafeLinkBuffer) ReadBinary(n int) (p []byte, err error) {
	if n <= 0 {
		return
	}
	// check whether enough or not.
	if b.Len() < n {
		return p, fmt.Errorf("link buffer read binary[%d] not enough", n)
	}
	return b.readBinary(n), nil
}

// readBinary cannot use mcache, because the memory allocated by readBinary will not be recycled.
func (b *UnsafeLinkBuffer) readBinary(n int) (p []byte) {
	b.recalLen(-n) // re-cal length

	// single node
	if b.isSingleNode(n) {
		p = dirtmake.Bytes(n, n)
		copy(p, b.read.Next(n))
		return p
	}
	p = dirtmake.Bytes(n, n)
	// multiple nodes
	var pIdx int
	var l int
	for ack := n; ack > 0; ack = ack - l {
		l = b.read.Len()
		if l >= ack {
			pIdx += copy(p[pIdx:], b.read.Next(ack))
			break
		} else if l > 0 {
			pIdx += copy(p[pIdx:], b.read.Next(l))
		}
		b.read = b.read.next
	}
	_ = pIdx
	return p
}

// ReadByte implements Reader.
func (b *UnsafeLinkBuffer) ReadByte() (p byte, err error) {
	// check whether enough or not.
	if b.Len() < 1 {
		return p, errors.New("link buffer read byte is empty")
	}
	b.recalLen(-1) // re-cal length
	for {
		if b.read.Len() >= 1 {
			return b.read.Next(1)[0], nil
		}
		b.read = b.read.next
	}
}

// Until returns a slice ends with the delim in the buffer.
func (b *UnsafeLinkBuffer) Until(delim byte) (line []byte, err error) {
	n := b.indexByte(delim, 0)
	if n < 0 {
		return nil, untilErr
	}
	return b.Next(n + 1)
}

// Slice returns a new LinkBuffer, which is a zero-copy slice of this LinkBuffer,
// and only holds the ability of Reader.
//
// Slice will automatically execute a Release.
func (b *UnsafeLinkBuffer) Slice(n int) (r Reader, err error) {
	if n <= 0 {
		return NewLinkBuffer(0), nil
	}
	// check whether enough or not.
	if b.Len() < n {
		return r, fmt.Errorf("link buffer readv[%d] not enough", n)
	}
	b.recalLen(-n) // re-cal length

	// just use for range
	p := new(LinkBuffer)
	p.length = int64(n)

	defer func() {
		// set to read-only
		p.flush = p.flush.next
		p.write = p.flush
	}()

	// single node
	if b.isSingleNode(n) {
		b.read.setFlag(flagReadExposed)
		node := b.read.Refer(n)
		p.head, p.read, p.flush = node, node, node
		return p, nil
	}
	// multiple nodes
	l := b.read.Len()
	b.read.setFlag(flagReadExposed)
	node := b.read.Refer(l)
	b.read = b.read.next

	p.head, p.read, p.flush = node, node, node
	for ack := n - l; ack > 0; ack = ack - l {
		l = b.read.Len()
		if l >= ack {
			b.read.setFlag(flagReadExposed)
			p.flush.next = b.read.Refer(ack)
			p.flush = p.flush.next
			break
		} else if l > 0 {
			b.read.setFlag(flagReadExposed)
			p.flush.next = b.read.Refer(l)
			p.flush = p.flush.next
		}
		b.read = b.read.next
	}
	return p, b.Release()
}

// TakeTry 尝试把剩余可读数据作为整块缓冲移交（零拷贝）：
//   - 数据须位于单一节点内（b.head==b.read 且 b.read.next==nil；Slice 生成的
//     子 Reader 会把 flush 置 nil，故以链表末节点判定单节点）；
//   - 该节点须为收流精确对齐节点（flagInputAligned，容量=帧长，ack 后不再写入）；
//   - 节点已被本帧完整消费（origin 无剩余未读数据）；
//   - 整帧须独占整个节点（base==0，即 len(node.buf)==len(origin.buf)）：一个节点
//     可能承载多帧，末帧虽已写满且 origin 读完，整块前段仍属同块内其它帧的数据，
//     一并交出后会被复用覆盖，故只认「整块独属本帧」；
//   - 节点已写满（origin.malloc == cap）：未写满节点 book 仍会复用其缓冲，
//     移交后被再写入会造成缓冲归属错乱，必须回退拷贝路径；
//   - 节点上无其它存活引用（origin.refer 仅剩本 Reader）：同节点可能承载多帧，
//     前序帧的子 Reader 尚未 Release（另一路 RPC 正在读它的负载）时移交，
//     缓冲经调用方归还后立刻被后续收流复用覆盖，前序读者会读到被改写的数据
//     （跨 RPC 数据错配），同样必须回退拷贝路径。
//
// 成功时返回 (buf, full, true)：buf 为负载切片（len==剩余负载），底层为整块精确
// 对齐缓冲；full 为整块缓冲（len==cap==节点容量，PutExact 按此归桶）。调用方用毕
// 必须经 SetInputAlignedAllocator 的 put 归还 full（不可归还 buf：其为子切片，
// cap 已被截断）；且此后不得再调用本 Reader 的 Release（缓冲所有权已移交，避免
// 双归还）。作为防御，origin 会被标记为不可复用（flagUnmanaged），即使发生越界
// Release 也只会回收节点对象、不会再次归还缓冲（缓冲归还唯一路径是调用方的 put）。
// 不满足任一条件返回 (nil, nil, false)，调用方回退 ReadCopy/Next 拷贝路径。
func (b *UnsafeLinkBuffer) TakeTry() (buf, full []byte, ok bool) {
	if b.head != b.read || b.read == nil || b.read.next != nil {
		return nil, nil, false
	}
	node := b.read
	origin := node.origin
	if origin == nil || !origin.getFlag(flagInputAligned) {
		return nil, nil, false
	}
	if origin.Len() != 0 {
		return nil, nil, false // origin 仍有未读数据（多帧同节点），移交会丢数据
	}
	// 独占性检查（base==0）：整帧须恰好覆盖整块 origin 缓冲才允许移交。
	// 收流按 bookSize 成块读入，一个节点可能承载多帧；此时即使 origin 已读完、
	// 节点已写满且 refer==2，整块缓冲的前段仍属于同块内其它帧的数据，把整块
	// 一并交出去、归还池后被后续收流复用覆盖，就会改写那些帧的内容。
	// base = len(origin.buf) - len(node.buf)，只认 base==0（整块独属本帧）。
	if len(node.buf) != len(origin.buf) {
		return nil, nil, false
	}
	if origin.malloc != cap(origin.buf) {
		return nil, nil, false // 节点未写满，book 仍会复用其缓冲
	}
	// 所有权移交的原子性：必须先置 flagUnmanaged，再做引用检查。
	// 主 Reader 的 Release 可能与本函数并发：它先判定 refer 是否归零、再判 reusable()
	// 决定是否回收缓冲。若本函数在其判定之后才置标志，Release 会把缓冲当普通节点
	// 归还进池，而调用方此时正持有它——后续收流复用同一缓冲即静默改写调用方数据。
	// 先置标志可保证：任何能把 refer 递减到 0 的 Release 都必然看到「已移交」而跳过回收。
	origin.setFlag(flagUnmanaged)
	// 其它存活引用检查：origin.refer 计数 = 1（origin 自身）+ 存活 Refer 节点数，
	// 本 Reader 是唯一存活引用时恰为 2。多于一者说明同节点上还有未 Release 的帧
	// 子 Reader 在引用该缓冲，移交后缓冲会被复用覆盖（见函数注释）；少于一者说明
	// 主 Reader 已释放该节点，缓冲可能已被回收，同样不能移交。
	if atomic.LoadInt32(&origin.refer) != 2 {
		// 未移交：撤销标记，恢复该节点的常规回收路径。
		// 本 Reader 仍被本函数持有，故 refer 在此期间不会归零，撤销不会造成缓冲丢失。
		origin.unsetFlag(flagUnmanaged)
		return nil, nil, false
	}
	// 负载起点在 origin 缓冲内的偏移：origin 被本帧完整消费，故
	// base = len(origin.buf) - len(node.buf)，负载 = node.buf[node.off:]。
	base := len(origin.buf) - len(node.buf)
	start := base + node.off
	// 三索引切片 cap 界取 start+Len()：结果 len==cap==负载长（子切片不可独立归池，
	// 归还统一走 full）。
	return origin.buf[start : start+node.Len() : start+node.Len()], origin.buf, true
}

// ------------------------------------------ implement zero-copy writer ------------------------------------------

// Malloc pre-allocates memory, which is not readable, and becomes readable data after submission(e.g. Flush).
func (b *UnsafeLinkBuffer) Malloc(n int) (buf []byte, err error) {
	if n <= 0 {
		return
	}
	b.mallocSize += n
	b.growth(n)
	return b.write.Malloc(n), nil
}

// MallocLen implements Writer.
func (b *UnsafeLinkBuffer) MallocLen() (length int) {
	return b.mallocSize
}

// MallocAck will keep the first n malloc bytes and discard the rest.
func (b *UnsafeLinkBuffer) MallocAck(n int) (err error) {
	if n < 0 {
		return fmt.Errorf("link buffer malloc ack[%d] invalid", n)
	}
	b.mallocSize = n
	b.write = b.flush

	var l int
	for ack := n; ack > 0; ack = ack - l {
		l = b.write.malloc - len(b.write.buf)
		if l >= ack {
			b.write.malloc = ack + len(b.write.buf)
			break
		}
		b.write = b.write.next
	}
	// discard the rest
	for node := b.write.next; node != nil; node = node.next {
		node.malloc, node.refer, node.buf = node.off, 1, node.buf[:node.off]
	}
	return nil
}

// Flush will submit all malloc data and must confirm that the allocated bytes have been correctly assigned.
func (b *UnsafeLinkBuffer) Flush() (err error) {
	b.mallocSize = 0
	// FIXME: The tail node must not be larger than 8KB to prevent Out Of Memory.
	if cap(b.write.buf) > pagesize {
		b.write.next = newLinkBufferNode(0)
		b.write = b.write.next
	}
	var n int
	for node := b.flush; node != b.write.next; node = node.next {
		delta := node.malloc - len(node.buf)
		if delta > 0 {
			n += delta
			node.buf = node.buf[:node.malloc]
		}
	}
	b.flush = b.write
	// re-cal length
	b.recalLen(n)
	return nil
}

// Append implements Writer.
func (b *UnsafeLinkBuffer) Append(w Writer) (err error) {
	buf, ok := w.(*LinkBuffer)
	if !ok {
		return errors.New("unsupported writer which is not LinkBuffer")
	}
	return b.WriteBuffer(buf)
}

// WriteBuffer will not submit(e.g. Flush) data to ensure normal use of MallocLen.
// you must actively submit before read the data.
// The argument buf can't be used after calling WriteBuffer. (set it to nil)
func (b *UnsafeLinkBuffer) WriteBuffer(buf *LinkBuffer) (err error) {
	if buf == nil {
		return
	}
	bufLen, bufMallocLen := buf.Len(), buf.MallocLen()
	if bufLen+bufMallocLen <= 0 {
		return nil
	}
	b.write.next = buf.read
	b.write = buf.write

	// close buf, prevents reuse.
	for buf.head != buf.read {
		nd := buf.head
		buf.head = buf.head.next
		nd.Release()
	}
	for buf.write = buf.write.next; buf.write != nil; {
		nd := buf.write
		buf.write = buf.write.next
		nd.Release()
	}
	buf.length, buf.mallocSize, buf.head, buf.read, buf.flush, buf.write = 0, 0, nil, nil, nil, nil

	// DON'T MODIFY THE CODE BELOW UNLESS YOU KNOW WHAT YOU ARE DOING !
	//
	// You may encounter a chain of bugs and not be able to
	// find out within a week that they are caused by modifications here.
	//
	// After release buf, continue to adjust b.
	b.write.next = nil
	if bufLen > 0 {
		b.recalLen(bufLen)
	}
	b.mallocSize += bufMallocLen
	return nil
}

// WriteString implements Writer.
func (b *UnsafeLinkBuffer) WriteString(s string) (n int, err error) {
	if len(s) == 0 {
		return
	}
	buf := unsafe.Slice(unsafe.StringData(s), len(s))
	return b.WriteBinary(buf)
}

// WriteBinary implements Writer.
func (b *UnsafeLinkBuffer) WriteBinary(p []byte) (n int, err error) {
	n = len(p)
	if n == 0 {
		return
	}
	b.mallocSize += n

	// TODO: Verify that all nocopy is possible under mcache.
	if n > BinaryInplaceThreshold {
		// expand buffer directly with nocopy
		b.write.next = newLinkBufferNode(0)
		b.write = b.write.next
		b.write.buf, b.write.malloc = p[:0], n
		return n, nil
	}
	// here will copy
	b.growth(n)
	buf := b.write.Malloc(n)
	return copy(buf, p), nil
}

// WriteDirect cannot be mixed with WriteString or WriteBinary functions.
func (b *UnsafeLinkBuffer) WriteDirect(extra []byte, remainLen int) error {
	n := len(extra)
	if n == 0 || remainLen < 0 {
		return nil
	}
	// find origin
	origin := b.flush
	malloc := b.mallocSize - remainLen // calculate the remaining malloc length
	for t := origin.malloc - len(origin.buf); t < malloc; t = origin.malloc - len(origin.buf) {
		malloc -= t
		origin = origin.next
	}
	// Add the buf length of the original node
	// `malloc` is the origin buffer offset that already malloced, the extra buffer should be inserted after that offset.
	malloc += len(origin.buf)

	// Create dataNode and newNode and insert them into the chain
	// dataNode wrap the user buffer extra, and newNode wrap the origin left netpoll buffer
	// - originNode{buf=origin, off=0, malloc=malloc, readonly=true} : non-reusable
	// - dataNode{buf=extra, off=0, malloc=len(extra), readonly=true} : non-reusable
	// - newNode{buf=origin, off=malloc, malloc=origin.malloc, readonly=false} : reusable
	dataNode := newLinkBufferNode(0) // zero node will be set by readonly mode
	dataNode.buf, dataNode.malloc = extra[:0], n

	if remainLen > 0 {
		// split a single buffer node to originNode and newNode
		newNode := newLinkBufferNode(0)
		newNode.off = malloc
		newNode.buf = origin.buf[:malloc]
		newNode.malloc = origin.malloc
		newNode.unsetFlag(flagUnmanaged)
		origin.malloc = malloc
		origin.setFlag(flagUnmanaged)

		// link nodes
		dataNode.next = newNode
		newNode.next = origin.next
		origin.next = dataNode
	} else {
		// link nodes
		dataNode.next = origin.next
		origin.next = dataNode
	}

	// adjust b.write
	for b.write.next != nil {
		b.write = b.write.next
	}

	b.mallocSize += n
	return nil
}

// WriteByte implements Writer.
func (b *UnsafeLinkBuffer) WriteByte(p byte) (err error) {
	dst, err := b.Malloc(1)
	if len(dst) == 1 {
		dst[0] = p
	}
	return err
}

// Close will recycle all buffer.
func (b *UnsafeLinkBuffer) Close() (err error) {
	atomic.StoreInt64(&b.length, 0)
	b.mallocSize = 0
	// just release all
	b.Release()
	for node := b.head; node != nil; {
		nd := node
		node = node.next
		nd.Release()
	}
	b.head, b.read, b.flush, b.write = nil, nil, nil, nil
	return nil
}

// ------------------------------------------ implement connection interface ------------------------------------------

// Bytes returns all the readable bytes of this LinkBuffer.
func (b *UnsafeLinkBuffer) Bytes() []byte {
	node, flush := b.read, b.flush
	if node == flush {
		return node.buf[node.off:]
	}
	n := 0
	p := dirtmake.Bytes(b.Len(), b.Len())
	for ; node != flush; node = node.next {
		if node.Len() > 0 {
			n += copy(p[n:], node.buf[node.off:])
		}
	}
	n += copy(p[n:], flush.buf[flush.off:])
	return p[:n]
}

// GetBytes will read and fill the slice p as much as possible.
// If p is not passed, return all readable bytes.
func (b *UnsafeLinkBuffer) GetBytes(p [][]byte) (vs [][]byte) {
	node, flush := b.read, b.flush
	if len(p) == 0 {
		n := 0
		for ; node != flush; node = node.next {
			n++
		}
		node = b.read
		p = make([][]byte, n)
	}
	var i int
	for i = 0; node != flush && i < len(p); node = node.next {
		if node.Len() > 0 {
			node.setFlag(flagReadExposed)
			p[i] = node.buf[node.off:]
			i++
		}
	}
	if i < len(p) {
		flush.setFlag(flagReadExposed)
		p[i] = flush.buf[flush.off:]
		i++
	}
	return p[:i]
}

// newInputLinkBufferNode 创建收流节点：缓冲精确为 size（4K 对齐，非分桶），
// 容量==帧长。读满一帧后 l==0，节点永不再被追加（一帧一节点），
// 且被完整消费的节点可经 TakeTry 零拷贝移交调用方。
func newInputLinkBufferNode(size int) *linkBufferNode {
	node := linkedPool.Get().(*linkBufferNode)
	node.off, node.malloc, node.refer = 0, 0, 1
	atomic.StoreInt32(&node.mode, int32(defaultLinkBufferMode))
	node.buf = inputAllocGet(size)
	node.buf = node.buf[:0]
	node.setFlag(flagInputAligned)
	return node
}

// book will grow and malloc buffer to hold data.
//
// bookSize: The size of data that can be read at once.
// maxSize: The maximum size of data between two Release(). In some cases, this can
//
//	guarantee all data allocated in one node to reduce copy.
func (b *UnsafeLinkBuffer) book(bookSize, maxSize int) (p []byte) {
	// 收流节点容量上限：钳制到单帧长，避免自适应 maxSize 膨胀为多帧节点
	// （多帧节点既破坏零拷贝 Take 的前置条件，也放大 readv 单次拷贝面）。
	if inputNodeSize > 0 && maxSize > inputNodeSize {
		maxSize = inputNodeSize
	}
	l := cap(b.write.buf) - b.write.malloc
	// grow linkBuffer
	if l == 0 {
		l = maxSize
		if inputAllocGet != nil && maxSize > 0 {
			// 精确尺寸对齐节点：容量==maxSize，读满一帧后 l==0，天然一帧一节点。
			b.write.next = newInputLinkBufferNode(maxSize)
		} else {
			b.write.next = newLinkBufferNode(maxSize)
		}
		b.write = b.write.next
	}
	if l > bookSize {
		l = bookSize
	}
	return b.write.Malloc(l)
}

// bookAck will ack the first n malloc bytes and discard the rest.
//
// length: The size of data in inputBuffer. It is used to calculate the maxSize
func (b *UnsafeLinkBuffer) bookAck(n int) (length int, err error) {
	b.write.malloc = n + len(b.write.buf)
	b.write.buf = b.write.buf[:b.write.malloc]
	b.flush = b.write

	// re-cal length
	length = b.recalLen(n)
	return length, nil
}

// calcMaxSize will calculate the data size between two Release()
func (b *UnsafeLinkBuffer) calcMaxSize() (sum int) {
	for node := b.head; node != b.read; node = node.next {
		sum += len(node.buf)
	}
	sum += len(b.read.buf)
	return sum
}

// resetTail will reset tail node or add an empty tail node to
// guarantee the tail node is not larger than 8KB
func (b *UnsafeLinkBuffer) resetTail(maxSize int) {
	if maxSize <= pagesize {
		// no need to reset a small buffer tail node
		return
	}
	// set nil tail
	b.write.next = newLinkBufferNode(0)
	b.write = b.write.next
	b.flush = b.write
}

// indexByte returns the index of the first instance of c in buffer, or -1 if c is not present in buffer.
func (b *UnsafeLinkBuffer) indexByte(c byte, skip int) int {
	size := b.Len()
	if skip >= size {
		return -1
	}
	var unread, n, l int
	node := b.read
	for unread = size; unread > 0; unread -= n {
		l = node.Len()
		if l >= unread { // last node
			n = unread
		} else { // read full node
			n = l
		}

		// skip current node
		if skip >= n {
			skip -= n
			node = node.next
			continue
		}
		i := bytes.IndexByte(node.Peek(n)[skip:], c)
		if i >= 0 {
			return (size - unread) + skip + i // past_read + skip_read + index
		}
		skip = 0 // no skip bytes
		node = node.next
	}
	return -1
}

// ------------------------------------------ private function ------------------------------------------

// recalLen re-calculate the length
func (b *UnsafeLinkBuffer) recalLen(delta int) (length int) {
	if delta < 0 && len(b.cachePeek) > 0 {
		// b.cachePeek will contain stale data if we read out even a single byte from buffer,
		// so we need to reset it or the next Peek call will return invalid bytes.
		b.cachePeek = b.cachePeek[:0]
	}
	return int(atomic.AddInt64(&b.length, int64(delta)))
}

// growth directly create the next node, when b.write is not enough.
func (b *UnsafeLinkBuffer) growth(n int) {
	if n <= 0 {
		return
	}
	// the memory of readonly node if not malloc by us so should skip them
	for b.write.getFlag(flagUnmanaged) || cap(b.write.buf)-b.write.malloc < n {
		if b.write.next == nil {
			b.write.next = newLinkBufferNode(n)
			b.write = b.write.next
			return
		}
		b.write = b.write.next
	}
}

// isSingleNode determines whether reading needs to cross nodes.
// isSingleNode will move b.read to latest non-empty node if there is a zero-size node
// Must require b.Len() > 0
func (b *UnsafeLinkBuffer) isSingleNode(readN int) (single bool) {
	if readN <= 0 {
		return true
	}
	l := b.read.Len()
	for l == 0 && b.read != b.flush {
		b.read = b.read.next
		l = b.read.Len()
	}
	return l >= readN
}

// memorySize return the real memory size in bytes the LinkBuffer occupied
func (b *LinkBuffer) memorySize() (bytes int) {
	for node := b.head; node != nil; node = node.next {
		bytes += cap(node.buf)
	}
	for _, c := range b.caches {
		bytes += cap(c)
	}
	bytes += cap(b.cachePeek)
	return bytes
}

// ------------------------------------------ implement link node ------------------------------------------

// newLinkBufferNode create or reuse linkBufferNode.
// Nodes with size <= 0 are marked as readonly, which means the node.buf is not allocated by this mcache.
func newLinkBufferNode(size int) *linkBufferNode {
	node := linkedPool.Get().(*linkBufferNode)
	// reset node offset
	node.off, node.malloc, node.refer = 0, 0, 1
	atomic.StoreInt32(&node.mode, int32(defaultLinkBufferMode))
	if size <= 0 {
		node.setFlag(flagUnmanaged)
		return node
	}
	if size < LinkBufferCap {
		size = LinkBufferCap
	}
	if alignedAllocGet != nil {
		// 由对齐分配器提供整块缓冲（bufpool），与 node.malloc 交互前先置空 len。
		node.buf = alignedAllocGet(size)
		node.buf = node.buf[:0]
		node.setFlag(flagAligned)
		return node
	}
	node.buf = malloc(0, size)
	return node
}

var linkedPool = sync.Pool{
	New: func() interface{} {
		return &linkBufferNode{
			refer: 1, // comes with 1 reference
		}
	},
}

type linkBufferNode struct {
	buf    []byte          // buffer
	off    int             // read-offset
	malloc int             // write-offset
	refer  int32           // reference count
	mode   int32           // mode store all bool bit status (atomic: 读循环与 Take 并发读写标志位)
	origin *linkBufferNode // the root node of the extends
	next   *linkBufferNode // the next node of the linked buffer
}

func (node *linkBufferNode) Len() (l int) {
	return len(node.buf) - node.off
}

func (node *linkBufferNode) IsEmpty() (ok bool) {
	return node.off == len(node.buf)
}

func (node *linkBufferNode) Reset() {
	if node.origin != nil || atomic.LoadInt32(&node.refer) != 1 {
		return
	}
	node.off, node.malloc = 0, 0
	node.buf = node.buf[:0]
}

func (node *linkBufferNode) Next(n int) (p []byte) {
	off := node.off
	node.off += n
	return node.buf[off:node.off:node.off]
}

func (node *linkBufferNode) Peek(n int) (p []byte) {
	return node.buf[node.off : node.off+n : node.off+n]
}

func (node *linkBufferNode) Malloc(n int) (buf []byte) {
	malloc := node.malloc
	node.malloc += n
	return node.buf[malloc:node.malloc:node.malloc]
}

// Refer holds a reference count at the same time as Next, and releases the real buffer after Release.
// The node obtained by Refer is read-only.
func (node *linkBufferNode) Refer(n int) (p *linkBufferNode) {
	p = newLinkBufferNode(0)
	p.buf = node.Next(n)

	if node.origin != nil {
		p.origin = node.origin
	} else {
		p.origin = node
	}
	atomic.AddInt32(&p.origin.refer, 1)
	return p
}

// Release consists of two parts:
// 1. reduce the reference count of itself and origin.
// 2. recycle the buf when the reference count is 0.
func (node *linkBufferNode) Release() (err error) {
	if node.origin != nil {
		node.origin.Release()
	}
	// release self
	if atomic.AddInt32(&node.refer, -1) == 0 {
		// readonly nodes cannot recycle node.buf, other node.buf are recycled to mcache.
		if node.reusable() {
			switch {
			case node.getFlag(flagInputAligned) && inputAllocPut != nil:
				inputAllocPut(node.buf)
			case node.getFlag(flagAligned) && alignedAllocPut != nil:
				alignedAllocPut(node.buf)
			default:
				free(node.buf)
			}
		}
		node.buf, node.origin, node.next = nil, nil, nil
		linkedPool.Put(node)
	}
	return nil
}

func (node *linkBufferNode) getFlag(flag uint8) bool {
	return atomic.LoadInt32(&node.mode)&int32(flag) > 0
}

func (node *linkBufferNode) setFlag(flag uint8) {
	atomic.OrInt32(&node.mode, int32(flag))
}

func (node *linkBufferNode) unsetFlag(flag uint8) {
	atomic.AndInt32(&node.mode, ^int32(flag))
}

// reusable reports whether the node's buffer memory is owned by the LinkBuffer and can be recycled.
// Called during Release to decide if node.buf should be returned to mcache via free.
func (node *linkBufferNode) reusable() bool {
	return atomic.LoadInt32(&node.mode)&int32(flagUnmanaged) == 0
}

// readExposed reports whether the node's buffer has been returned directly to user code
// via a zero-copy Reader method and may still be referenced externally.
func (node *linkBufferNode) readExposed() bool {
	return atomic.LoadInt32(&node.mode)&int32(flagReadExposed) > 0
}
