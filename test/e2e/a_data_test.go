//go:build e2e

package e2e

// A 组：数据正确性（最高优先级）。全部内容按 sha256 + 逐字节双重校验，且刻意覆盖
// 对齐/非对齐、块边界、块间跨读、尺寸矩阵、覆盖写、删除语义与多块大对象。
//
// 主路径用直连数据面客户端（internal/rpcclient，覆盖全部尺寸/区间/覆盖写语义，
// 不受 SDK「key 只写一次」约束）；TestASDKPath 用集群 SDK 复走关键项。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/rpcclient"
	taihuclient "github.com/liucxer/taihu/pkg/taihu-client"
)

// TestA1Block4MiB 4MiB 整块（= 传输 ChunkSize / 磁盘整块）写读回逐字节一致。
func TestA1Block4MiB(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	const size = 4 << 20
	data := randBytes(t, size)
	key := h.key("a1/block4m")
	putExact(t, c, key, data)

	// 再读一次（第二次读命中不同缓冲路径：首读走多帧/零拷贝移交），内容必须一致。
	getExact(t, c, key, data)
}

// TestA2SizeMatrix 尺寸矩阵：0/1/非对齐/块边界/多块，全部写读回一致。
func TestA2SizeMatrix(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	sizes := []int{0, 1, 4095, 4096, 4097, 1 << 20, 4<<20 - 1, 4 << 20, 4<<20 + 1, 8 << 20, 16 << 20}
	for _, n := range sizes {
		t.Run(fmt.Sprintf("size-%d", n), func(t *testing.T) {
			data := randBytes(t, n)
			key := h.key(fmt.Sprintf("a2/size-%d", n))
			putExact(t, c, key, data)
		})
	}
}

// TestA3Incompressible 不可压缩随机内容（每 key 独立随机），确保不是"碰巧全零/可压缩"
// 掩盖了截断或错误填充。
func TestA3Incompressible(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	for i := 0; i < 8; i++ {
		data := randBytes(t, 1<<20+i*997)
		key := h.key(fmt.Sprintf("a3/rand-%d", i))
		putExact(t, c, key, data)
	}
}

// TestA4RangeRead 区间读矩阵：对齐/非对齐/块边界/跨块/尾部/零长，以及越界读必须报错
// （不得静默截断返回短数据）。
func TestA4RangeRead(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	const total = 8 << 20
	data := pattern("A4", total)
	key := h.key("a4/obj")
	if err := c.Put(h.ctx, key, total, data); err != nil {
		t.Fatalf("Put(%s, %d): %v", key, total, err)
	}

	cases := []struct {
		name      string
		off, size int64
	}{
		{"zero-len-at-0", 0, 0},
		{"head-1", 0, 1},
		{"head-block", 0, 4 << 20},
		{"unaligned-1-4095", 1, 4095},
		{"block-edge-last-byte", 4095, 1},
		{"across-4k-boundary", 4095, 4097},
		{"across-block-boundary-short", 4<<20 - 1, 4097},
		{"across-block-boundary-to-eof", 4<<20 - 1, 4 << 20},
		{"second-block-full", 4 << 20, 4 << 20},
		{"unaligned-in-second-block", 4<<20 + 1, 4<<20 - 1},
		{"tail-last-byte", total - 1, 1},
		{"tail-unaligned", total - 4097, 4097},
		{"zero-len-at-eof", total, 0},
		{"full-object", 0, total},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getRange(t, c, key, tc.off, tc.size, data)
		})
	}

	t.Run("beyond-eof-must-error", func(t *testing.T) {
		got, rel, err := c.Get(h.ctx, key, total, 1)
		if err == nil {
			if rel != nil {
				rel()
			}
			t.Fatalf("读越界应报错，却返回 %d 字节", len(got))
		}
	})
	t.Run("off-beyond-eof-must-error", func(t *testing.T) {
		got, rel, err := c.Get(h.ctx, key, total+1, -1)
		if err == nil {
			if rel != nil {
				rel()
			}
			t.Fatalf("off 越界应报错，却返回 %d 字节", len(got))
		}
	})
}

// TestA5Overwrite 覆盖写：同 key 反复写、大小来回变（含 0 与跨块），每次都必须是
// 最后一次写入的完整内容（旧段存活计数随之回退，不得读到旧数据）。
func TestA5Overwrite(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	key := h.key("a5/obj")
	sizes := []int{4 << 20, 1, 8192, 4<<20 + 4096, 0, 4097, 8 << 20}
	for i, n := range sizes {
		data := pattern(fmt.Sprintf("A5-%d", i), n)
		if err := c.Put(h.ctx, key, int64(n), data); err != nil {
			t.Fatalf("第 %d 次覆盖写 (size=%d): %v", i, n, err)
		}
		getExact(t, c, key, data)
	}

	// 删后重写同 key：必须重新可读。
	if err := c.Delete(h.ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	again := pattern("A5-recreate", 4<<20+1)
	putExact(t, c, key, again)
}

// TestA6Delete delete 语义：删后 Get/Stat 必 ErrNotFound、重复删幂等（仍报 NotFound
// 且不破坏其它 key）、删后重写可用。
func TestA6Delete(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	key := h.key("a6/victim")
	neighbor := h.key("a6/neighbor")
	vdata := pattern("A6-victim", 4<<20)
	ndata := pattern("A6-neighbor", 64<<10)
	putExact(t, c, key, vdata)
	putExact(t, c, neighbor, ndata)

	if err := c.Delete(h.ctx, key); err != nil {
		t.Fatalf("Delete(%s): %v", key, err)
	}

	if _, rel, err := c.Get(h.ctx, key, 0, -1); !errors.Is(err, rpcclient.ErrNotFound) {
		if rel != nil {
			rel()
		}
		t.Fatalf("删后 Get err = %v, want ErrNotFound", err)
	}
	if _, err := c.Stat(h.ctx, key); !errors.Is(err, rpcclient.ErrNotFound) {
		t.Fatalf("删后 Stat err = %v, want ErrNotFound", err)
	}
	// 注意：零长读（size==0）不走服务端、直接返回空，故对不存在的 key 也"成功"；
	// 这是传输层的既定短路语义（见 internal/transport/client.go Get），不在本处断言。

	// 重复删：幂等（仍报 NotFound），不产生副作用。
	if err := c.Delete(h.ctx, key); !errors.Is(err, rpcclient.ErrNotFound) {
		t.Fatalf("重复 Delete err = %v, want ErrNotFound", err)
	}

	// 邻近 key 未受影响。
	getExact(t, c, neighbor, ndata)

	// 删后重写同 key 可用。
	reborn := pattern("A6-reborn", 4097)
	putExact(t, c, key, reborn)
}

// TestA7MultiBlock 多块大对象（16MiB / 64MiB）：整对象一致 + 跨块区间抽查。
func TestA7MultiBlock(t *testing.T) {
	for _, size := range []int{16 << 20, 64 << 20} {
		t.Run(fmt.Sprintf("%dMiB", size>>20), func(t *testing.T) {
			h := newHarness(t)
			srv := h.startServer(serverOpts{devSize: 32 << 30})
			c := h.dialDirect(srv, 1)

			data := randBytes(t, size)
			key := h.key("a7/obj")
			putExact(t, c, key, data)

			// 抽查：首块尾-1 起跨 2 块（必须跨 4MiB 块边界）、中段非对齐、末块。
			getRange(t, c, key, 4<<20-1, 8<<20+2, data)
			getRange(t, c, key, (int64(size)>>1)+1, 4<<20-3, data)
			getRange(t, c, key, int64(size)-(4<<20), 4<<20, data)
		})
	}
}

// TestA8MixedBatch 混合批量：多种尺寸批量写 → 全量读回 → 抽删一半 → 再全量读回，
// 并用服务端前缀枚举（admin ListKeys）核对存活集合。
func TestA8MixedBatch(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	const n = 24
	keys := make([]string, n)
	contents := make([][]byte, n)
	sizes := []int{1, 4096, 64 << 10, 4 << 20, 4<<20 + 4097, 8 << 20}
	for i := 0; i < n; i++ {
		keys[i] = h.key(fmt.Sprintf("a8/obj-%02d", i))
		contents[i] = pattern(fmt.Sprintf("A8-%d", i), sizes[i%len(sizes)])
		putExact(t, c, keys[i], contents[i])
	}

	// 全量读回。
	for i := range keys {
		getExact(t, c, keys[i], contents[i])
	}

	// 抽删偶数下标。
	for i := 0; i < n; i += 2 {
		if err := c.Delete(h.ctx, keys[i]); err != nil {
			t.Fatalf("Delete(%s): %v", keys[i], err)
		}
	}

	// 再全量校验。
	for i := range keys {
		if i%2 == 0 {
			if _, rel, err := c.Get(h.ctx, keys[i], 0, -1); !errors.Is(err, rpcclient.ErrNotFound) {
				if rel != nil {
					rel()
				}
				t.Fatalf("已删 %s Get err = %v, want ErrNotFound", keys[i], err)
			}
			continue
		}
		getExact(t, c, keys[i], contents[i])
	}

	// 服务端前缀枚举：只剩奇数下标。
	names, err := c.ListKeys(h.ctx, h.keyPrefix)
	if err != nil {
		t.Fatalf("ListKeys(%s): %v", h.keyPrefix, err)
	}
	want := make(map[string]bool, n/2)
	for i := 1; i < n; i += 2 {
		want[keys[i]] = true
	}
	if len(names) != len(want) {
		t.Fatalf("ListKeys 返回 %d 个 key, want %d: %v", len(names), len(want), names)
	}
	for _, k := range names {
		if !want[k] {
			t.Fatalf("ListKeys 返回了不该存在的 key %q", k)
		}
	}
}

// TestASDKPath 集群 SDK 关键路径：尺寸矩阵子集 + 区间读 + 删除语义 + 索引一致性。
// SDK 的回源回调设为「未知 key 即 NotFound」，与真实部署（上层提供源）一致。
func TestASDKPath(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	st := h.newSDK(func(cfg *taihuclient.ClusterConfig) {
		cfg.Source = func(ctx context.Context, key string) ([]byte, error) {
			return nil, rpcclient.ErrNotFound
		}
	})
	waitFor(t, 15*time.Second, "SDK 发现本次实例（addr=%s）", func() bool { return st.HasLive() }, srv.info.Addr)

	for _, n := range []int{0, 1, 4096, 4097, 4 << 20, 4<<20 + 1} {
		data := pattern(fmt.Sprintf("ASDK-%d", n), n)
		putExact(t, st, h.key(fmt.Sprintf("asdk/size-%d", n)), data)
	}

	const total = 8 << 20
	big := pattern("ASDK-range", total)
	rkey := h.key("asdk/range")
	putExact(t, st, rkey, big)
	getRange(t, st, rkey, 1, 4095, big)
	getRange(t, st, rkey, 4<<20-1, 4<<20, big)
	getRange(t, st, rkey, total-1, 1, big)

	// 索引区应已记录全部已写 key（索引为 100ms 周期的异步批量写，故轮询等待）。
	const wantIdx = 7
	waitFor(t, 15*time.Second, "索引区记录 %d 个 key", func() bool {
		ks, err := st.ListIndexKeys(h.ctx, h.keyPrefix+"asdk/")
		return err == nil && len(ks) == wantIdx
	}, wantIdx)

	// 删除语义：Get/Stat 报 NotFound，且索引被清。
	if err := st.Delete(h.ctx, rkey); err != nil {
		t.Fatalf("SDK Delete: %v", err)
	}
	if _, rel, err := st.Get(h.ctx, rkey, 0, -1); !errors.Is(err, rpcclient.ErrNotFound) {
		if rel != nil {
			rel()
		}
		t.Fatalf("SDK 删后 Get err = %v, want ErrNotFound", err)
	}
	if _, err := st.Stat(h.ctx, rkey); !errors.Is(err, rpcclient.ErrNotFound) {
		t.Fatalf("SDK 删后 Stat err = %v, want ErrNotFound", err)
	}
	const wantLeft = 6
	waitFor(t, 15*time.Second, "删后索引收敛到 %d 个 key", func() bool {
		ks, err := st.ListIndexKeys(h.ctx, h.keyPrefix+"asdk/")
		if err != nil {
			return false
		}
		for _, k := range ks {
			if k == rkey {
				return false // 残留被删 key
			}
		}
		return len(ks) == wantLeft
	}, wantLeft)
}

// TestASDKSourceRebuild 回源重建：索引与本地实例都没有的 key，经 Source 拉到整对象后
// 按请求区间返回，并回写本地缓存（第二次读走本地实例）。
func TestASDKSourceRebuild(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})

	full := pattern("ASDK-source", 4<<20+123)
	var calls int
	st := h.newSDK(func(cfg *taihuclient.ClusterConfig) {
		cfg.Source = func(ctx context.Context, key string) ([]byte, error) {
			calls++
			return full, nil
		}
	})
	waitFor(t, 15*time.Second, "SDK 发现本次实例（addr=%s）", func() bool { return st.HasLive() }, srv.info.Addr)

	key := h.key("asdk/source-only")
	got, rel, err := st.Get(h.ctx, key, 4097, 4096)
	if err != nil {
		t.Fatalf("Get(回源): %v", err)
	}
	exp := full[4097 : 4097+4096]
	if sha256Hex(got) != sha256Hex(exp) {
		rel()
		t.Fatalf("回源区间内容不一致: got len=%d want len=%d", len(got), len(exp))
	}
	rel()
	if calls != 1 {
		t.Fatalf("Source 调用次数 = %d, want 1", calls)
	}

	// 回源路径对"超出对象末尾"的请求按对象末尾截断（getFromSource 显式 clamp，
	// 与直连传输层「越界报错」不同）。用一个从未写过的新 key 保证只走回源路径
	// （已回写的 key 会命中索引 → 走传输层 → 越界报错，语义不同）。
	got, rel, err = st.Get(h.ctx, h.key("asdk/source-tail"), int64(len(full))-1, 4096)
	if err != nil {
		t.Fatalf("Get(回源, 尾部越界): %v", err)
	}
	if len(got) != 1 || got[0] != full[len(full)-1] {
		rel()
		t.Fatalf("回源尾部截断不符: len=%d want 1", len(got))
	}
	rel()
	callsAfterWarm := calls

	// 回源时已本地优先回写：第二次整对象读必须命中本地实例，不再回源。
	waitFor(t, 15*time.Second, "回写后的索引可见", func() bool {
		ks, err := st.ListIndexKeys(h.ctx, key)
		return err == nil && len(ks) == 1
	})
	getExact(t, st, key, full)
	if calls != callsAfterWarm {
		t.Fatalf("回写后仍回源（Source 调用 %d → %d 次）", callsAfterWarm, calls)
	}
}
