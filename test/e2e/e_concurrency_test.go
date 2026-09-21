//go:build e2e

package e2e

// E 组：并发/竞态（不使用 -race：aarch64 上 `go test -race` 链接失败）。
//
// 核心不变式是「不撕裂」：并发写同一 key 时，任何一次读要么报 NotFound（仅 Delete
// 窗口内），要么返回**某一次写入的完整内容**，绝不能是两次写的混合/半截。断言方式为
// 逐字节比对候选内容集合（各 writer 内容长度刻意不同，撕裂读几乎必然与全部候选都不等）。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/rpcclient"
)

// eRunPool 以 workers 个并发执行 fn(0..n-1)。fn 内用 t.Errorf 报告（并发安全），
// 不用 t.Fatalf（非并发安全），以保证其余任务继续执行、尽可能多地暴露问题。
func eRunPool(t *testing.T, workers, n int, fn func(i int)) {
	t.Helper()
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}

// eRPCTimeout 单次 RPC 上限：本组用例的核心不变式是「响应必须到达」，正常路径下
// 永不触发；仅当传输层丢响应时才生效——把「永久挂起」转成一条明确的失败，
// 避免整个用例被 20 分钟包超时打断（那样拿不到任何诊断信息）。
const eRPCTimeout = 30 * time.Second

// eCtx 返回带 eRPCTimeout 上限的上下文，调用方负责 cancel。
func eCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), eRPCTimeout)
}

// TestE1ConcurrentSameKeyNoTear 多个并发写者反复覆盖同一 key，同时多个读者持续读：
// 任何一次成功读都必须是某个写者的完整内容。
//
// 全部写者使用**同一长度**且为单 chunk（L < ChunkSize）：这样 Stat 对任何已提交版本
// 都返回同一 size，读路径不存在「size 与被读版本不匹配」的合法分支，因此任何内容
// 不符都是真正的撕裂。
//
// 若写者长度各异，`Get(key, 0, -1)` 的 Stat 与 GetData 是两次独立 RPC（见
// transport.Conn.Get），size 可能取自版本 A 而数据取自版本 B，返回「A 的前缀」——
// 这是 API 的 TOCTOU（读至结尾非快照语义），不是内存撕裂；多 chunk 的版本混合
// 另见 TestE1MultiChunkNoTear。
func TestE1ConcurrentSameKeyNoTear(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{devSize: 32 << 30})
	key := h.key("e1/hot")

	const (
		writers    = 4
		readers    = 4
		iterations = 30
	)
	// 长度刻意取非 4K 对齐值（覆盖对齐平移路径），且全部写者相同。
	const body = 1<<20 + 1 // 1MiB+1：单 chunk（< ChunkSize 4MiB）

	contents := make([][]byte, writers)
	sums := make([]string, writers)
	for i := range contents {
		contents[i] = pattern(fmt.Sprintf("E1-w%d", i), body)
		sums[i] = sha256Hex(contents[i])
	}

	// 先落一次基线，避免"key 尚不存在"的 NotFound 干扰判定。
	{
		c := h.dialDirect(srv, 1)
		if err := c.Put(h.ctx, key, int64(len(contents[0])), contents[0]); err != nil {
			t.Fatalf("基线 Put: %v", err)
		}
	}

	var (
		tears    atomic.Int64
		reads    atomic.Int64
		errs     atomic.Int64
		firstErr atomic.Value // string，避免 atomic.Value 混用不同类型
	)
	stopCh := make(chan struct{})

	var rwg sync.WaitGroup
	for r := 0; r < readers; r++ {
		c := h.dialDirect(srv, 1)
		rwg.Add(1)
		go func(c *rpcclient.Storage) {
			defer rwg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
				}
				ctx, cancel := eCtx()
				data, rel, err := c.Get(ctx, key, 0, -1)
				cancel()
				if err != nil {
					// 同长度覆盖写下读路径无合法失败分支（key 从不删除）：
					// 任何错误都是缺陷，必须计数（而非静默跳过）。
					errs.Add(1)
					firstErr.Store(err.Error())
					continue
				}
				got := sha256Hex(data)
				matched := false
				for _, s := range sums {
					if got == s {
						matched = true
						break
					}
				}
				if !matched {
					tears.Add(1)
					t.Errorf("撕裂读：len=%d sha=%s（候选 %v）", len(data), got, sums)
				}
				reads.Add(1)
				rel()
			}
		}(c)
	}

	var wwg sync.WaitGroup
	for w := 0; w < writers; w++ {
		c := h.dialDirect(srv, 2)
		w := w
		wwg.Add(1)
		go func() {
			defer wwg.Done()
			for i := 0; i < iterations; i++ {
				ctx, cancel := eCtx()
				err := c.Put(ctx, key, int64(len(contents[w])), contents[w])
				cancel()
				if err != nil {
					t.Errorf("writer %d 第 %d 次 Put: %v", w, i, err)
					return
				}
			}
		}()
	}
	wwg.Wait()
	time.Sleep(300 * time.Millisecond)
	close(stopCh)
	rwg.Wait()

	if reads.Load() == 0 {
		t.Fatalf("并发期间无任何成功读（err=%d firstErr=%v）", errs.Load(), firstErr.Load())
	}
	if tears.Load() != 0 || errs.Load() != 0 {
		t.Fatalf("同长度并发覆盖写同 key：撕裂 %d 次 / 错误 %d 次（成功读 %d 次，firstErr=%v）",
			tears.Load(), errs.Load(), reads.Load(), firstErr.Load())
	}

	// 收尾：最终内容必须是某一个写者的完整内容。
	final := h.dialDirect(srv, 1)
	got, rel, err := final.Get(h.ctx, key, 0, -1)
	if err != nil {
		t.Fatalf("最终 Get: %v", err)
	}
	defer rel()
	for i, s := range sums {
		if sha256Hex(got) == s {
			t.Logf("最终内容来自 writer %d（共 %d 次成功读）", i, reads.Load())
			return
		}
	}
	t.Fatalf("最终内容不是任何一次写入的完整内容: len=%d sha=%s（候选 %v）", len(got), sha256Hex(got), sums)
}

// TestE1MultiChunkNoTear 多 chunk 对象（8MiB+1 → 3 个响应帧）的并发覆盖写不撕裂。
//
// 与 E1 的区别在于一次 GET 会产生多个数据帧：服务端 handleGet 逐 chunk 调用
// storage.ReadAt，而 ReadAt **每个 chunk 都重新解析** key→(seg, off, size) 映射。
// 若并发 Put 的映射提交落在两个 chunk 之间，同一响应内前面的 chunk 来自旧版本、
// 后面的来自新版本 —— 长度正确、内容却是两版本混合（chunk 级撕裂）。
// 这正是本用例要暴露的不变式：一次 GET 必须来自单一版本。
func TestE1MultiChunkNoTear(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{devSize: 32 << 30})
	key := h.key("e1/multichunk")

	const (
		writers    = 2
		readers    = 4
		iterations = 20
	)
	const body = 8<<20 + 1 // 8MiB+1：3 个 chunk（4MiB + 4MiB + 1B）

	contents := make([][]byte, writers)
	sums := make([]string, writers)
	for i := range contents {
		contents[i] = pattern(fmt.Sprintf("E1m-w%d", i), body)
		sums[i] = sha256Hex(contents[i])
	}

	w0 := h.dialDirect(srv, 1)
	if err := w0.Put(h.ctx, key, int64(len(contents[0])), contents[0]); err != nil {
		t.Fatalf("基线 Put: %v", err)
	}

	var (
		tears    atomic.Int64
		reads    atomic.Int64
		errs     atomic.Int64
		firstErr atomic.Value
	)
	stopCh := make(chan struct{})
	var rwg sync.WaitGroup
	for r := 0; r < readers; r++ {
		c := h.dialDirect(srv, 1)
		rwg.Add(1)
		go func(c *rpcclient.Storage) {
			defer rwg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
				}
				ctx, cancel := eCtx()
				data, rel, err := c.Get(ctx, key, 0, -1)
				cancel()
				if err != nil {
					errs.Add(1)
					firstErr.Store(err.Error())
					continue
				}
				got := sha256Hex(data)
				matched := false
				for _, s := range sums {
					if got == s {
						matched = true
						break
					}
				}
				if !matched {
					tears.Add(1)
					t.Errorf("chunk 级撕裂读：len=%d sha=%s（候选 %v）", len(data), got, sums)
				}
				reads.Add(1)
				rel()
			}
		}(c)
	}

	var wwg sync.WaitGroup
	for w := 0; w < writers; w++ {
		c := h.dialDirect(srv, 1)
		w := w
		wwg.Add(1)
		go func() {
			defer wwg.Done()
			for i := 0; i < iterations; i++ {
				ctx, cancel := eCtx()
				err := c.Put(ctx, key, int64(len(contents[w])), contents[w])
				cancel()
				if err != nil {
					t.Errorf("writer %d 第 %d 次 Put: %v", w, i, err)
					return
				}
			}
		}()
	}
	wwg.Wait()
	time.Sleep(300 * time.Millisecond)
	close(stopCh)
	rwg.Wait()

	if reads.Load() == 0 {
		t.Fatalf("并发期间无任何成功读（err=%d firstErr=%v）", errs.Load(), firstErr.Load())
	}
	if tears.Load() != 0 {
		t.Fatalf("多 chunk 并发覆盖写出现 chunk 级撕裂 %d 次（成功读 %d 次，err=%d）："+
			"一次 GET 内混合了两个版本 —— 服务端 handleGet 逐 chunk 重新解析映射",
			tears.Load(), reads.Load(), errs.Load())
	}
	if errs.Load() != 0 {
		t.Fatalf("同长度并发覆盖写出现读错误 %d 次（firstErr=%v）", errs.Load(), firstErr.Load())
	}
	t.Logf("多 chunk 并发读 %d 次，无撕裂、无错误", reads.Load())
}

// TestE2ConcurrentDeleteGet 一个写者反复 Delete→Put 交替，多个读者并发读：
// 任何一次读要么 ErrNotFound，要么是完整内容；其它错误一律失败。
//
// a/b 取**同一长度**（单 chunk）：读路径不存在「Stat 的 size 与被读版本不匹配」的
// 合法分支，因此除 Delete 窗口的 ErrNotFound 外，任何失败或内容不符都是缺陷。
func TestE2ConcurrentDeleteGet(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	key := h.key("e2/churn")

	a := pattern("E2-a", 2<<20)
	b := pattern("E2-b", 2<<20)
	sumA, sumB := sha256Hex(a), sha256Hex(b)

	w := h.dialDirect(srv, 1)
	if err := w.Put(h.ctx, key, int64(len(a)), a); err != nil {
		t.Fatalf("初始 Put: %v", err)
	}

	var (
		notFound  atomic.Int64
		reads     atomic.Int64
		tears     atomic.Int64
		badErrors atomic.Int64
	)
	stopCh := make(chan struct{})
	var rwg sync.WaitGroup
	for r := 0; r < 4; r++ {
		c := h.dialDirect(srv, 1)
		rwg.Add(1)
		go func(c *rpcclient.Storage) {
			defer rwg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
				}
				ctx, cancel := eCtx()
				data, rel, err := c.Get(ctx, key, 0, -1)
				cancel()
				switch {
				case errors.Is(err, rpcclient.ErrNotFound):
					notFound.Add(1)
				case err != nil:
					badErrors.Add(1)
					t.Errorf("并发 Delete/Get 出现非 NotFound 错误: %v", err)
				default:
					got := sha256Hex(data)
					if got != sumA && got != sumB {
						tears.Add(1)
						t.Errorf("撕裂读：len=%d sha=%s（候选 a=%s b=%s）", len(data), got, sumA, sumB)
					}
					reads.Add(1)
					rel()
				}
			}
		}(c)
	}

	for i := 0; i < 40; i++ {
		ctx, cancel := eCtx()
		if err := w.Delete(ctx, key); err != nil && !errors.Is(err, rpcclient.ErrNotFound) {
			t.Errorf("第 %d 轮 Delete: %v", i, err)
		}
		body := a
		if i%2 == 1 {
			body = b
		}
		if err := w.Put(ctx, key, int64(len(body)), body); err != nil {
			t.Errorf("第 %d 轮 Put: %v", i, err)
		}
		cancel()
	}
	time.Sleep(300 * time.Millisecond)
	close(stopCh)
	rwg.Wait()

	if reads.Load() == 0 {
		t.Fatalf("并发期间无任何成功读（NotFound=%d）", notFound.Load())
	}
	if tears.Load() != 0 || badErrors.Load() != 0 {
		t.Fatalf("并发 Delete/Get 异常：撕裂=%d 非 NotFound 错误=%d", tears.Load(), badErrors.Load())
	}
	t.Logf("成功读 %d 次，NotFound %d 次", reads.Load(), notFound.Load())

	// 收尾：最终对象必须完整可读（b，最后一轮写入）。
	getExact(t, w, key, b)
}

// TestE3BulkConcurrentMixed 1 万 key 高并发混合读写 → 全量校验 → 抽删 1/3 → 再全量校验，
// 并用服务端前缀枚举核对存活集合与对象清单完全一致。
func TestE3BulkConcurrentMixed(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{devSize: 32 << 30})
	c := h.dialDirect(srv, 8)

	const (
		n       = 10000
		workers = 64
	)
	keys := make([]string, n)
	contents := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = h.key(fmt.Sprintf("e3/k-%05d", i))
		contents[i] = pattern(fmt.Sprintf("E3-%d", i), (i%17)*512+1)
	}

	// 阶段 1：高并发混合写+读（每个 worker 写完立刻读回校验）。
	eRunPool(t, workers, n, func(i int) {
		ctx, cancel := eCtx()
		defer cancel()
		if err := c.Put(ctx, keys[i], int64(len(contents[i])), contents[i]); err != nil {
			t.Errorf("Put(%s): %v", keys[i], err)
			return
		}
		got, rel, err := c.Get(ctx, keys[i], 0, -1)
		if err != nil {
			t.Errorf("Get(%s): %v", keys[i], err)
			return
		}
		defer rel()
		if !bytes.Equal(got, contents[i]) {
			t.Errorf("Get(%s) 内容不一致: len=%d/%d", keys[i], len(got), len(contents[i]))
		}
	})

	// 阶段 2：并发全量校验（含区间读抽查）。
	// 诊断用：内容 sha → key 序号，用于判断错配内容是否来自其它对象。
	shaIndex := make(map[string]int, n)
	for i := range contents {
		shaIndex[sha256Hex(contents[i])] = i
	}
	eRunPool(t, workers, n, func(i int) {
		ctx, cancel := eCtx()
		defer cancel()
		st, err := c.Stat(ctx, keys[i])
		if err != nil || st != int64(len(contents[i])) {
			st2, err2 := c.Stat(ctx, keys[i])
			t.Errorf("Stat(%s) = %d, %v; want %d（复测 = %d, %v）", keys[i], st, err, len(contents[i]), st2, err2)
			return
		}
		got, rel, err := c.Get(ctx, keys[i], 0, st) // 显式 size：避免 Get 内部 Stat 取到脏值时越界
		if err != nil {
			t.Errorf("Get(%s): %v", keys[i], err)
			return
		}
		defer rel()
		if !bytes.Equal(got, contents[i]) {
			// 区分「落盘数据错」与「读路径错」：立刻复读一次，并判断错配内容归属。
			originDesc := "未知内容（疑似缓冲复用/脏数据）"
			if j, ok := shaIndex[sha256Hex(got)]; ok {
				originDesc = fmt.Sprintf("实为 k-%05d 的内容（跨对象错配）", j)
			}
			again, rel2, err2 := c.Get(ctx, keys[i], 0, -1)
			againOK := false
			if err2 == nil {
				againOK = bytes.Equal(again, contents[i])
				rel2()
			}
			head := got
			if len(head) > 16 {
				head = head[:16]
			}
			t.Errorf("Get(%s) 内容不一致: len=%d/%d %s；复读一致=%v(复读 err=%v) got[0:16]=%x",
				keys[i], len(got), len(contents[i]), originDesc, againOK, err2, head)
		}
	})

	// 阶段 3：并发抽删 i%3==0（共 3334 个）。
	eRunPool(t, workers, n, func(i int) {
		if i%3 != 0 {
			return
		}
		ctx, cancel := eCtx()
		defer cancel()
		if err := c.Delete(ctx, keys[i]); err != nil {
			t.Errorf("Delete(%s): %v", keys[i], err)
		}
	})

	// 阶段 4：并发再校验（已删必 NotFound，存活必完整）。
	survivors := 0
	for i := 0; i < n; i++ {
		if i%3 != 0 {
			survivors++
		}
	}
	eRunPool(t, workers, n, func(i int) {
		ctx, cancel := eCtx()
		defer cancel()
		// 统一先显式 Stat 再按显式 size Get：size<0 的读至结尾会在 Get 内部再做一次
		// Stat，若该 Stat 返回脏值则会以巨大 size 走 bufpool.Get 越界 panic。
		st, err := c.Stat(ctx, keys[i])
		if i%3 == 0 {
			if !errors.Is(err, rpcclient.ErrNotFound) {
				t.Errorf("已删 %s Stat err = %v, want ErrNotFound", keys[i], err)
			}
			return
		}
		if err != nil {
			t.Errorf("存活 %s Stat: %v", keys[i], err)
			return
		}
		if st != int64(len(contents[i])) {
			t.Errorf("存活 %s Stat = %d, want %d", keys[i], st, len(contents[i]))
			return
		}
		got, rel, err := c.Get(ctx, keys[i], 0, st)
		if err != nil {
			t.Errorf("存活 %s Get: %v", keys[i], err)
			return
		}
		defer rel()
		if !bytes.Equal(got, contents[i]) {
			t.Errorf("存活 %s 内容不一致: len=%d/%d", keys[i], len(got), len(contents[i]))
		}
	})

	// 服务端前缀枚举：存活集合必须与清单完全一致。
	names, err := c.ListKeys(h.ctx, h.keyPrefix+"e3/")
	if err != nil {
		t.Fatalf("ListKeys(%s): %v", h.keyPrefix+"e3/", err)
	}
	if len(names) != survivors {
		t.Fatalf("ListKeys 返回 %d 个 key, want %d", len(names), survivors)
	}
	seen := make(map[string]struct{}, len(names))
	for _, k := range names {
		seen[k] = struct{}{}
	}
	for i := 0; i < n; i++ {
		_, ok := seen[keys[i]]
		if (i%3 != 0) != ok {
			t.Fatalf("ListKeys 存活集合不符：key=%s 期望存在=%v 实际=%v", keys[i], i%3 != 0, ok)
		}
	}
}
