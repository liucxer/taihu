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

import "testing"

// TestTakeTrySharedNodeLiveReader 回归护栏：多帧同节点时，若前序帧的子 Reader 仍存活
// （另一路 RPC 正在读它的负载），末帧不得 TakeTry 移交整块节点缓冲。
//
// 危害：节点 buf 内两帧 A、B，B 为末帧且节点写满 → 未加保护时 TakeTry 会把整块缓冲
// 移交出去；调用方用毕 PutExact 归还，池立刻把该缓冲分配给后续收流节点，新到达数据
// 覆盖 A 的负载，A 读到被改写的内容（跨 RPC 数据错配）。故契约要求此时拒绝移交，
// 回退 ReadCopy/Next 拷贝路径（见 TakeTry 注释「节点上无其它存活引用」）。
func TestTakeTrySharedNodeLiveReader(t *testing.T) {
	const nodeSize = 30 // 帧A wire 10 + 帧B wire 20
	pool := setupTakeTest(t, nodeSize)

	lb := NewLinkBuffer(0)
	buf := fillInput(lb, nodeSize, nodeSize)
	wa := putFrame(buf, 1, 0x06, []byte{0xAA})
	wb := putFrame(buf[wa:], 2, 0x06, []byte("hello-taket"))
	if wa+wb != nodeSize {
		t.Fatalf("wa+wb = %d, want %d", wa+wb, nodeSize)
	}

	// 帧A：子 Reader 取出后**不 Release**（模拟另一路 RPC 正在处理该帧）。
	subA := frameSlice(t, lb, wa)
	// 帧B：末帧，节点恰好写满。
	subB := frameSlice(t, lb, wb)

	if _, full, ok := takeTry(subB); ok {
		// 移交发生：整块缓冲经调用方归还后立刻被后续收流复用，存活读者 subA 的数据被覆盖。
		pool.put(full)
		next := fillInput(lb, nodeSize, nodeSize)
		if &next[0] != &buf[0] {
			t.Fatalf("池未复用被移交缓冲，用例未覆盖目标路径")
		}
		for i := range next {
			next[i] = 0x5A // 新收流数据覆盖整块缓冲
		}
		pA, _ := subA.Next(1)
		t.Fatalf("节点仍有存活读者时 TakeTry 移交了整块缓冲：subA 读到 %#x（want 0xAA）；"+
			"应对 origin.refer 存活引用做检查并回退拷贝路径", pA[0])
	}

	// 拒绝移交后：两帧各自可正常读出（拷贝路径不受影响）。
	pA, err := subA.Next(1)
	if err != nil || pA[0] != 0xAA {
		t.Fatalf("fallback subA payload = %v err=%v, want 0xAA", pA, err)
	}
	pB, err := subB.Next(11)
	if err != nil || string(pB) != "hello-taket" {
		t.Fatalf("fallback subB payload = %q err=%v, want %q", pB, err, "hello-taket")
	}
	if err := subA.Release(); err != nil {
		t.Fatalf("subA.Release: %v", err)
	}
	if err := subB.Release(); err != nil {
		t.Fatalf("subB.Release: %v", err)
	}
	if pool.putCount() != 0 {
		t.Fatalf("put count = %d, want 0 (no take)", pool.putCount())
	}
}