//go:build e2e

package e2e

// G 组：长稳（长时程压测）。与 F1 的关系与区别：
//
//	F1 是 60s 口径的稳定性冒烟（延迟/资源回落/无 panic），按 60s 写死的三处设计在 3h 下会失真：
//	  1. fLatency 容量固定且**溢出即丢弃** → 长稳只覆盖最早一小段，延迟维度失效；
//	  2. fStartCPUProfile 覆盖整个观测窗口 → 3h profile 的服务端采样缓冲本身吃内存，
//	     反过来污染 RSS 泄漏断言；
//	  3. 只在结束时对比一次基线 RSS → 察觉不到「每分钟几 KB」的缓慢泄漏。
//
//	故 G1 独立实现：**窗口化**延迟采样 + 周期性 timeline 采样 + 首末段趋势断言 +
//	周期性（每 30min）抓 60s CPU / 瞬时 heap profile。
//
// 负载为全量混合：整块读 / 非对齐区间读 / 覆盖写 / Delete+Put churn / 多 chunk 热点键
// （8MiB+1 并发覆盖写 + 整对象读逐字节校验，把 TestE1MultiChunkNoTear 的不变式放进长稳）。
// churn 与多 chunk 是长稳的核心价值：`ReadAtMeta` 的 GET 级段引用若漏 Unref，段永不回收，
// 水位会持续上涨并最终 ErrNoSpace —— 这是 60s 用例探不到的。
//
// 时长：缺省 60s（仅作采样机制与用例自身的冒烟，会随全量 e2e 一起跑），
// 3h 长稳用 E2E_LONG_SOAK_SECONDS=10800 覆盖（与 F1 的 E2E_SOAK_SECONDS 相互独立）。
//
// 断言口径（loop 设备抖动大，不做绝对带宽断言）：
//   - 硬：0 撕裂/0 内容错配 / 0 错误 / 无 >5s 超长延迟 / 服务端日志无 panic / 资源回落 /
//     水位不持续越限（连续 ≥3 个采样点 ≥90%）/ 无 ErrNoSpace；
//   - 趋势（样本足够时）：末段 p99 ≤ 首段 ×3、末段 RSS ≤ 首段 ×1.5+256MiB、goroutine/fd 有界；
//   - 软：CPU 核数、syscall 分布、profile 热点只记日志与 CSV。
//
// 水位口径必须是**字节**：`Segments` 的 Total 是「已分配段数」（随分配动态增长），
// (Total-Free)/Total 只是空闲池占比，不是容量水位（冒烟里会出现 100% → 50% 的假锯齿）。
// 真口径取自集群注册记录 `cluster.InstanceInfo`（Capacity = 段大小×段数、Used = 已写物理字节，
// 由服务端心跳上报），与客户端选路用的水位同源。
// 判定取「持续越限」而非峰值：段粒度 8GiB / 设备 32GiB ⇒ 水位天然有 25% 量级的锯齿，
// 健康运行也会短暂冲高；而回收失效会让水位**长期贴顶**。
//
// 运行（3h，必须后台化：9527 代理 /exec 有 1800s 超时，前台跑会被掐断）：
//
//	setsid nohup env TMPDIR=/var/tmp E2E_PD=<pd> E2E_LONG_SOAK_SECONDS=10800 \
//	  go test -tags e2e -run TestG1LongSoakStability -timeout 4h -v ./test/e2e/ \
//	  > /var/tmp/g1-soak.log 2>&1 < /dev/null &

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/rpcclient"
)

const (
	gSampleEvery = 30 * time.Second       // timeline 采样周期
	gHotSize     = 8<<20 + 1              // 多 chunk 热点键（3 帧：4MiB+4MiB+1B）
	gSoakObjSize = 4 << 20                // 主读集对象大小（整块）
	gSoakObjs    = 128                    // 主读集对象数（512MiB）
	gChurnObjs   = 32                     // churn 键池
	gChurnBody   = 1 << 20                // churn 对象大小
	gChurnEvery  = 50 * time.Millisecond  // churn 限速
	gHotEvery    = 100 * time.Millisecond // 热点键覆盖写限速（10 次/s 已足以高频命中跨 chunk 竞态）
	gProfEvery   = 30 * time.Minute       // 长稳下周期抓 profile 的间隔
	gProfChurn   = 60                     // 每次 profile 覆盖的秒数
)

// gServerGoroutines 与 fServerGoroutines 同义，但**不 Fatalf**：采样器跑在独立 goroutine，
// 那里调用 t.Fatalf 是非法用法（会 panic 并跳过其余清理）。
func gServerGoroutines(port int) (int64, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/goroutine?debug=1", port)
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	m := fGoroutineTotalRe.FindStringSubmatch(string(body))
	if m == nil {
		return 0, fmt.Errorf("pprof 响应无 goroutine 总数: %.200s", body)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// gCounters 长稳期间的分类计数（所有 worker 共享）。
type gCounters struct {
	ops      atomic.Int64
	bytes    atomic.Int64
	errs     atomic.Int64
	mism     atomic.Int64
	tears    atomic.Int64
	notFound atomic.Int64
	noSpace  atomic.Int64
}

// record 记一次错误并按错误类型分类（ErrNotFound 在 churn 的 Delete 窗口内是合法的，
// 由调用方决定是否计数）。
func (c *gCounters) record(err error) {
	c.errs.Add(1)
	switch {
	case errors.Is(err, rpcclient.ErrNotFound):
		c.notFound.Add(1)
	case errors.Is(err, rpcclient.ErrNoSpace):
		c.noSpace.Add(1)
	}
}

// gLat 窗口化延迟采样：可在任意时刻整批换出（fLatency 容量耗尽即丢弃，长稳下不可用）。
type gLat struct {
	mu   sync.Mutex
	samp []time.Duration
}

func (l *gLat) add(d time.Duration) {
	l.mu.Lock()
	l.samp = append(l.samp, d)
	l.mu.Unlock()
}

// take 取出并清空当前窗口的样本。
func (l *gLat) take() []time.Duration {
	l.mu.Lock()
	s := l.samp
	l.samp = nil
	l.mu.Unlock()
	return s
}

// gQuantiles 返回 (样本数, p50, p99, max)。
func gQuantiles(s []time.Duration) (int, time.Duration, time.Duration, time.Duration) {
	if len(s) == 0 {
		return 0, 0, 0, 0
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	pick := func(q float64) time.Duration { return s[int(float64(len(s)-1)*q)] }
	return len(s), pick(0.50), pick(0.99), s[len(s)-1]
}

// gSample 一个采样点的全部观测（CSV 一行，同时用于趋势断言）。
type gSample struct {
	ops, errs, tears, mism         int64
	n                              int
	p50, p99, max                  time.Duration
	srvRSS, srvThr, srvGor, srvFD  int64
	cliGor, cliFD                  int64
	srvCPU, cliCPU, machineCoreSec float64
	syscr, syscw                   int64
	rcharMiB, wcharMiB             int64
	segTotal, segFree, segFull     int64
	segReclaiming, segObjCount     int64
	waterPct                       float64 // 字节水位百分比（InstanceInfo.Used/Capacity）
	waterUsedMiB, waterCapMiB      int64
}

var gCSVHeader = []string{
	"t_s", "ops", "errs", "tears", "mism", "lat_n", "p50_ms", "p99_ms", "max_ms",
	"srv_rss_mib", "srv_threads", "srv_goroutines", "srv_fd", "cli_goroutines", "cli_fd",
	"srv_core_sec", "cli_core_sec", "machine_core_sec", "srv_syscr", "srv_syscw",
	"srv_rchar_mib", "srv_wchar_mib", "seg_total", "seg_free", "seg_full", "seg_reclaiming",
	"seg_objcount", "water_pct", "water_used_mib", "water_cap_mib",
}

func (s gSample) csvRow(t0 time.Time) []string {
	ms := func(d time.Duration) string { return fmt.Sprintf("%.2f", float64(d)/float64(time.Millisecond)) }
	return []string{
		fmt.Sprintf("%.0f", time.Since(t0).Seconds()),
		fmt.Sprintf("%d", s.ops), fmt.Sprintf("%d", s.errs), fmt.Sprintf("%d", s.tears), fmt.Sprintf("%d", s.mism),
		fmt.Sprintf("%d", s.n), ms(s.p50), ms(s.p99), ms(s.max),
		fmt.Sprintf("%d", s.srvRSS), fmt.Sprintf("%d", s.srvThr), fmt.Sprintf("%d", s.srvGor), fmt.Sprintf("%d", s.srvFD),
		fmt.Sprintf("%d", s.cliGor), fmt.Sprintf("%d", s.cliFD),
		fmt.Sprintf("%.2f", s.srvCPU), fmt.Sprintf("%.2f", s.cliCPU), fmt.Sprintf("%.2f", s.machineCoreSec),
		fmt.Sprintf("%d", s.syscr), fmt.Sprintf("%d", s.syscw),
		fmt.Sprintf("%d", s.rcharMiB), fmt.Sprintf("%d", s.wcharMiB),
		fmt.Sprintf("%d", s.segTotal), fmt.Sprintf("%d", s.segFree), fmt.Sprintf("%d", s.segFull),
		fmt.Sprintf("%d", s.segReclaiming), fmt.Sprintf("%d", s.segObjCount),
		fmt.Sprintf("%.1f", s.waterPct), fmt.Sprintf("%d", s.waterUsedMiB), fmt.Sprintf("%d", s.waterCapMiB),
	}
}

// gInstanceWater 从集群注册记录读本实例的**字节**水位。
//
// Capacity = 段大小×段数（布局总容量），Used = 已写物理字节（Full/Reclaiming 段计整段、
// Active 段计已写偏移），Available = Capacity − Used —— 这正是客户端选路用的水位口径。
// 与 `Segments` 的 Total 不同：Total 是「已分配段数」，会随分配动态增长，拿它算占比只得到
// 空闲池占比，不是容量水位。
//
// 采样器跑在独立 goroutine，故本函数**不 Fatalf**：读失败时只置 ok=false，由调用方记日志。
func gInstanceWater(h *harness, name string) (pct float64, usedMiB, capMiB int64, ok bool, err error) {
	info, gerr := cluster.GetInstance(h.ctx, h.raw, name)
	if gerr != nil {
		return 0, 0, 0, false, gerr
	}
	if info == nil || info.Capacity <= 0 {
		return 0, 0, 0, false, nil
	}
	return 100 * float64(info.Used) / float64(info.Capacity), info.Used >> 20, info.Capacity >> 20, true, nil
}

// gTrendAvg 把样本按时间等分 4 段，返回首段与末段某指标的均值；样本不足（<8）返回 ok=false
// （短时长冒烟跑不出趋势，只记录不断言）。
func gTrendAvg(s []gSample, get func(gSample) float64) (first, last float64, ok bool) {
	if len(s) < 8 {
		return 0, 0, false
	}
	q := len(s) / 4
	var fs, ls float64
	for _, x := range s[:q] {
		fs += get(x)
	}
	for _, x := range s[len(s)-q:] {
		ls += get(x)
	}
	return fs / float64(q), ls / float64(q), true
}

// gFetchHeapProfile 抓一次瞬时 heap profile（失败只记日志：profile 是证据不是判定）。
func gFetchHeapProfile(t *testing.T, port int, out string) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/heap", port)
	cl := &http.Client{Timeout: 60 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		t.Logf("抓 heap profile 失败(%s): %v", url, err)
		return
	}
	defer resp.Body.Close()
	f, err := os.Create(out)
	if err != nil {
		t.Logf("创建 %s 失败: %v", out, err)
		return
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		t.Logf("写 heap profile %s 失败: %v", out, err)
		return
	}
	t.Logf("heap profile 已落盘: %s (%d 字节)", out, n)
}

// TestG1LongSoakStability 长稳：时长/负载/采样/断言见文件头注释。
func TestG1LongSoakStability(t *testing.T) {
	dur := fDuration(t, "E2E_LONG_SOAK_SECONDS", 60*time.Second)
	h := newHarness(t)
	srv := h.startServer(serverOpts{devSize: 32 << 30})
	pid := srv.cmd.Process.Pid
	pprofPort := fPprofPort(t, srv.logPath)

	// ---------------- 数据准备 ----------------
	prep := h.dialDirect(srv, 8)
	keys, contents := fPrepareObjects(t, prep, h, gSoakObjs)

	// 多 chunk 热点键：两版本等长（8MiB+1）、内容不同，任何一次读必须是某一版完整内容。
	hotKey := h.key("g/hot-multichunk")
	hotA := pattern("G-hot-a", gHotSize)
	hotB := pattern("G-hot-b", gHotSize)
	hotSums := []string{sha256Hex(hotA), sha256Hex(hotB)}
	if err := prep.Put(h.ctx, hotKey, int64(len(hotA)), hotA); err != nil {
		t.Fatalf("基线 Put(%s): %v", hotKey, err)
	}

	// churn 键池：Delete→Put 交替，持续制造段回收压力（也是删除泄漏的探针）。
	churnKeys := make([]string, gChurnObjs)
	churnBody := pattern("G-churn", gChurnBody)
	for i := range churnKeys {
		churnKeys[i] = h.key(fmt.Sprintf("g/churn-%02d", i))
		if err := prep.Put(h.ctx, churnKeys[i], int64(len(churnBody)), churnBody); err != nil {
			t.Fatalf("churn 预热 Put(%s): %v", churnKeys[i], err)
		}
	}

	statC := h.dialDirect(srv, 1)
	hotW := h.dialDirect(srv, 1)
	hotR := h.dialDirect(srv, 1)
	churnC := h.dialDirect(srv, 1)

	const readers = 6
	clients := make([]*rpcclient.Storage, readers)
	for i := range clients {
		c, err := rpcclient.DialPool(h.ctx, srv.info.Addr, 2)
		if err != nil {
			t.Fatalf("DialPool #%d: %v", i, err)
		}
		clients[i] = c
	}

	// ---------------- 基线 ----------------
	baseClientGor := runtime.NumGoroutine()
	baseClientFD := fFdCount(os.Getpid())
	baseSrvRSS := fProcRSSMiB(pid)
	baseSrvGor := fServerGoroutines(t, pprofPort)
	baseSrvFD := int64(fFdCount(pid))
	cpuBefore := fProcCPUSeconds(pid)
	cliCPU0 := fProcCPUSeconds(os.Getpid())
	mBusy0, _ := fMachineCPU()
	io0 := fProcReadIO(pid)

	// timeline CSV：在启动 worker 之前创建，避免此处 Fatalf 时留下仍在跑、
	// 后续会调用 t.Errorf 的 goroutine（测试已结束时的 t.Errorf 会 panic）。
	csvPath := filepath.Join(workdirRoot(t), "g1-timeline-"+h.tag+".csv")
	csvFile, err := os.Create(csvPath)
	if err != nil {
		t.Fatalf("创建 timeline %s: %v", csvPath, err)
	}
	defer csvFile.Close()
	cw := csv.NewWriter(csvFile)
	_ = cw.Write(gCSVHeader)
	cw.Flush()

	var (
		cnt     gCounters
		lat     = &gLat{}
		maxSeen atomic.Int64 // 全局最大延迟（纳秒），覆盖采样间隔内的尖峰
		wg      sync.WaitGroup
	)
	stop := time.Now().Add(dur)
	t0 := time.Now()

	// ---------------- 工作负载 ----------------
	// 混合读者：整块读 + 每 8 次一次非对齐区间读 + 每 32 次一次覆盖写。
	for w := 0; w < readers; w++ {
		c := clients[w]
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			i := w
			for time.Now().Before(stop) {
				i = (i + 1) % gSoakObjs
				start := time.Now()
				got, rel, err := c.Get(ctx, keys[i], 0, -1)
				if err != nil {
					cnt.record(err)
					t.Errorf("长稳 Get(%s): %v", keys[i], err)
					continue
				}
				if !bytes.Equal(got, contents[i]) {
					cnt.mism.Add(1)
					t.Errorf("长稳 Get(%s) 内容不一致: len=%d/%d", keys[i], len(got), len(contents[i]))
				}
				rel()
				d := time.Since(start)
				lat.add(d)
				gTrackMax(&maxSeen, d)
				cnt.bytes.Add(int64(len(got)))
				cnt.ops.Add(1)

				if cnt.ops.Load()%8 == 0 {
					const roff, rsize = gSoakObjSize - 8192, 4097
					s, rel2, err := c.Get(ctx, keys[i], roff, rsize)
					if err != nil {
						cnt.record(err)
						t.Errorf("长稳 区间读(%s): %v", keys[i], err)
					} else {
						if !bytes.Equal(s, contents[i][roff:roff+rsize]) {
							cnt.mism.Add(1)
							t.Errorf("长稳 区间读(%s) 内容不一致: len=%d want %d", keys[i], len(s), rsize)
						}
						rel2()
					}
					cnt.ops.Add(1)
				}
				if cnt.ops.Load()%32 == 0 {
					if err := c.Put(ctx, keys[i], int64(len(contents[i])), contents[i]); err != nil {
						cnt.record(err)
						t.Errorf("长稳 Put(%s): %v", keys[i], err)
					}
					cnt.ops.Add(1)
				}
			}
		}()
	}

	// 热点键写者：两个等长版本交替覆盖写（触发跨 chunk 的映射替换）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := context.Background()
		bodies := [][]byte{hotA, hotB}
		for n := 0; time.Now().Before(stop); n++ {
			b := bodies[n%2]
			if err := hotW.Put(ctx, hotKey, int64(len(b)), b); err != nil {
				cnt.record(err)
				t.Errorf("长稳 热点键 Put: %v", err)
				return
			}
			cnt.ops.Add(1)
			time.Sleep(gHotEvery)
		}
	}()

	// 热点键读者：整对象读逐字节判定「必须等于某一版完整内容」（多 chunk 撕裂护栏）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := context.Background()
		for time.Now().Before(stop) {
			start := time.Now()
			data, rel, err := hotR.Get(ctx, hotKey, 0, -1)
			if err != nil {
				cnt.record(err)
				t.Errorf("长稳 热点键 Get: %v", err)
				continue
			}
			sum := sha256Hex(data)
			if sum != hotSums[0] && sum != hotSums[1] {
				cnt.tears.Add(1)
				t.Errorf("长稳期间多 chunk 撕裂读：len=%d sha=%s（候选 %v）", len(data), sum, hotSums)
			}
			rel()
			d := time.Since(start)
			lat.add(d)
			gTrackMax(&maxSeen, d)
			cnt.bytes.Add(int64(len(data)))
			cnt.ops.Add(1)
		}
	}()

	// churn：Delete→Put 交替（限速，保证 3h 既有足够回收压力又不淹没读路径）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := context.Background()
		for i := 0; time.Now().Before(stop); i++ {
			k := churnKeys[i%gChurnObjs]
			if err := churnC.Delete(ctx, k); err != nil && !errors.Is(err, rpcclient.ErrNotFound) {
				cnt.record(err)
				t.Errorf("长稳 churn Delete(%s): %v", k, err)
			}
			if err := churnC.Put(ctx, k, int64(len(churnBody)), churnBody); err != nil {
				cnt.record(err)
				t.Errorf("长稳 churn Put(%s): %v", k, err)
			}
			cnt.ops.Add(2)
			time.Sleep(gChurnEvery)
		}
	}()

	// ---------------- timeline 采样 ----------------
	samples := make([]gSample, 0, int(dur/gSampleEvery)+2)
	samplerDone := make(chan struct{})
	prev := struct {
		ops, syscr, syscw, rchar, wchar int64
		srvCPU, cliCPU, mBusy           float64
	}{ops: cnt.ops.Load(), syscr: io0.syscr, syscw: io0.syscw, rchar: io0.rchar, wchar: io0.wchar,
		srvCPU: cpuBefore, cliCPU: cliCPU0, mBusy: mBusy0}

	go func() {
		defer close(samplerDone)
		tick := time.NewTicker(gSampleEvery)
		defer tick.Stop()
		for range tick.C {
			now := time.Now()
			if now.After(stop.Add(gSampleEvery)) {
				return
			}
			s := gSample{}
			s.n, s.p50, s.p99, s.max = gQuantiles(lat.take())
			ops := cnt.ops.Load()
			s.ops = ops - prev.ops
			prev.ops = ops
			s.errs, s.tears, s.mism = cnt.errs.Load(), cnt.tears.Load(), cnt.mism.Load()

			s.srvRSS, s.srvThr, s.srvFD = fProcRSSMiB(pid), fProcThreads(pid), int64(fFdCount(pid))
			if n, gerr := gServerGoroutines(pprofPort); gerr == nil {
				s.srvGor = n
			} else {
				t.Logf("采样服务端 goroutine 数失败（只影响本行该字段）: %v", gerr)
			}
			s.cliGor, s.cliFD = int64(runtime.NumGoroutine()), int64(fFdCount(os.Getpid()))

			srvCPU, cliCPU := fProcCPUSeconds(pid), fProcCPUSeconds(os.Getpid())
			mBusy, _ := fMachineCPU()
			s.srvCPU, s.cliCPU, s.machineCoreSec = srvCPU-prev.srvCPU, cliCPU-prev.cliCPU, mBusy-prev.mBusy
			prev.srvCPU, prev.cliCPU, prev.mBusy = srvCPU, cliCPU, mBusy

			io1 := fProcReadIO(pid)
			s.syscr, s.syscw = io1.syscr-prev.syscr, io1.syscw-prev.syscw
			s.rcharMiB, s.wcharMiB = (io1.rchar-prev.rchar)>>20, (io1.wchar-prev.wchar)>>20
			prev.syscr, prev.syscw, prev.rchar, prev.wchar = io1.syscr, io1.syscw, io1.rchar, io1.wchar

			if sum, _, serr := statC.Segments(context.Background()); serr == nil {
				s.segTotal, s.segFree, s.segFull = sum.Total, sum.Free, sum.Full
				s.segReclaiming, s.segObjCount = sum.Reclaiming, sum.ObjectCount
			} else {
				t.Logf("采样 Segments 失败（只影响本行的段计数字段）: %v", serr)
			}
			// 水位用字节口径（见文件头）：整机段计数只是诊断，不参与判定。
			if pct, uMiB, cMiB, ok, werr := gInstanceWater(h, srv.name); ok {
				s.waterPct, s.waterUsedMiB, s.waterCapMiB = pct, uMiB, cMiB
			} else if werr != nil {
				t.Logf("采样实例水位失败（只影响本行水位字段）: %v", werr)
			} else {
				t.Logf("采样实例水位：注册记录缺失或 Capacity=0（只影响本行水位字段）")
			}

			samples = append(samples, s)
			_ = cw.Write(s.csvRow(t0))
			cw.Flush()
			t.Logf("G1 采样 t=%s ops=%d errs=%d tears=%d p50=%s p99=%s max=%s | 服务端 RSS=%dMiB gor=%d thr=%d fd=%d | 客户端 gor=%d fd=%d | CPU 服务端=%.2f 客户端=%.2f 整机=%.2f 核·秒 | 水位=%.1f%%(%d/%dMiB) 段 total=%d free=%d full=%d reclaiming=%d obj=%d",
				time.Since(t0).Round(time.Second), s.ops, s.errs, s.tears,
				s.p50.Round(time.Microsecond), s.p99.Round(time.Microsecond), s.max.Round(time.Millisecond),
				s.srvRSS, s.srvGor, s.srvThr, s.srvFD, s.cliGor, s.cliFD,
				s.srvCPU, s.cliCPU, s.machineCoreSec,
				s.waterPct, s.waterUsedMiB, s.waterCapMiB,
				s.segTotal, s.segFree, s.segFull, s.segReclaiming, s.segObjCount)
		}
	}()

	// 周期性 profile：长稳（≥20min）时每 30min 抓 60s CPU profile + 瞬时 heap；
	// 短时长冒烟不抓（避免拖长全量 e2e）。
	// 两个通道职责分开：profStop 只由主流程关闭（goroutine 只读），profDone 只由 profiler
	// goroutine 关闭（主流程只读）。早期版本让 profiler 同时读/关 profDone、主流程又关一次，
	// 短时长分支已 close 时二次 close 直接 panic（`close of closed channel`）。
	profStop := make(chan struct{})
	profDone := make(chan struct{})
	if dur >= 20*time.Minute {
		go func() {
			defer close(profDone)
			tick := time.NewTicker(gProfEvery)
			defer tick.Stop()
			for i := 1; ; i++ {
				select {
				case <-tick.C:
					wait := fStartCPUProfile(t, pprofPort, gProfChurn, fCPUProfilePath(t, fmt.Sprintf("g1-server-cpu-%02d", i)))
					wait() // 就地等待，避免跨 goroutine 传递等待函数
					gFetchHeapProfile(t, pprofPort, fCPUProfilePath(t, fmt.Sprintf("g1-server-heap-%02d", i)))
				case <-profStop:
					return
				}
			}
		}()
	} else {
		close(profDone) // 短时长不抓 profile：直接置为已完成
	}

	wg.Wait()
	elapsed := time.Since(t0)
	close(profStop) // 幂等：恰好关一次；长稳分支最坏在此多等一次抓取（≤gProfChurn+30s）
	<-profDone
	<-samplerDone

	// ---------------- 汇总 ----------------
	bandwidth := float64(cnt.bytes.Load()) / elapsed.Seconds() / (1 << 20)
	t.Logf("G1 汇总: 时长=%s ops=%d 错误=%d(NotFound=%d NoSpace=%d) 错配=%d 撕裂=%d 读带宽=%.1f MiB/s",
		elapsed.Round(time.Second), cnt.ops.Load(), cnt.errs.Load(), cnt.notFound.Load(), cnt.noSpace.Load(),
		cnt.mism.Load(), cnt.tears.Load(), bandwidth)
	t.Logf("G1 timeline CSV: %s（采样 %d 点）", csvPath, len(samples))

	maxLat := time.Duration(maxSeen.Load())
	if maxLat > 5*time.Second {
		t.Fatalf("长稳出现超长延迟 max=%s", maxLat)
	}

	// 错误/一致性：硬断言。
	if cnt.tears.Load() != 0 || cnt.mism.Load() != 0 {
		t.Fatalf("长稳出现 撕裂=%d 内容错配=%d（读带宽 %.1f MiB/s）", cnt.tears.Load(), cnt.mism.Load(), bandwidth)
	}
	if cnt.errs.Load() != 0 {
		t.Fatalf("长稳期间出现 %d 个错误（NotFound=%d NoSpace=%d）", cnt.errs.Load(), cnt.notFound.Load(), cnt.noSpace.Load())
	}
	if cnt.ops.Load() == 0 {
		t.Fatal("长稳期间未完成任何操作")
	}

	// 水位：字节口径（Capacity=段大小×段数、Used=已写物理字节，由服务端心跳上报）。
	// 判「持续越限」而非峰值：段粒度 8GiB / 设备 32GiB ⇒ 水位天然有 25% 量级的锯齿，
	// 健康运行也会短暂冲高；回收失效会让水位长期贴顶，故连续 ≥3 点（≥90s）≥90% 才算失败。
	maxWater, run, peakRun := 0.0, 0, 0
	for _, s := range samples {
		if s.waterPct > maxWater {
			maxWater = s.waterPct
		}
		if s.waterPct >= 90 {
			run++
			if run > peakRun {
				peakRun = run
			}
		} else {
			run = 0
		}
	}
	if len(samples) > 0 {
		last := samples[len(samples)-1]
		t.Logf("G1 水位: 峰值 %.1f%%（末行 %.1f%% = %d/%dMiB），连续 ≥90%% 最长 %d 个采样点",
			maxWater, last.waterPct, last.waterUsedMiB, last.waterCapMiB, peakRun)
	}

	if cnt.noSpace.Load() > 0 {
		sum, _, _ := statC.Segments(context.Background())
		t.Fatalf("长稳期间出现 ErrNoSpace %d 次（段 Total=%d Free=%d Full=%d Reclaiming=%d）：疑似回收不生效（删除泄漏/引用未归零）\n--- log tail ---\n%s",
			cnt.noSpace.Load(), sum.Total, sum.Free, sum.Full, sum.Reclaiming, srv.logTail())
	}
	if peakRun >= 3 {
		t.Fatalf("长稳水位持续越限: 连续 %d 个采样点 ≥90%%（峰值 %.1f%%）：疑似段回收失效", peakRun, maxWater)
	}

	// 趋势（样本足够时）：防缓慢退化。
	if first, last, ok := gTrendAvg(samples, func(x gSample) float64 { return float64(x.p99) / float64(time.Millisecond) }); ok {
		t.Logf("G1 趋势 p99: 首段均 %.2fms → 末段均 %.2fms", first, last)
		if last > first*3+1 {
			t.Fatalf("长稳延迟退化: p99 首段均 %.2fms → 末段均 %.2fms（阈值 ×3）", first, last)
		}
	}
	if first, last, ok := gTrendAvg(samples, func(x gSample) float64 { return float64(x.srvRSS) }); ok {
		t.Logf("G1 趋势 服务端 RSS: 首段均 %.1fMiB → 末段均 %.1fMiB（基线 %dMiB）", first, last, baseSrvRSS)
		if last > first*1.5+256 {
			t.Fatalf("长稳服务端 RSS 疑似泄漏: 首段均 %.1fMiB → 末段均 %.1fMiB", first, last)
		}
	}
	if first, last, ok := gTrendAvg(samples, func(x gSample) float64 { return float64(x.srvGor) }); ok {
		t.Logf("G1 趋势 服务端 goroutine: 首段均 %.1f → 末段均 %.1f（基线 %d）", first, last, baseSrvGor)
		if last > first+50 {
			t.Fatalf("长稳服务端 goroutine 持续增长: 首段均 %.1f → 末段均 %.1f", first, last)
		}
	}
	if first, last, ok := gTrendAvg(samples, func(x gSample) float64 { return float64(x.srvFD) }); ok {
		t.Logf("G1 趋势 服务端 fd: 首段均 %.1f → 末段均 %.1f（基线 %d）", first, last, baseSrvFD)
		if last > first+32 {
			t.Fatalf("长稳服务端 fd 持续增长: 首段均 %.1f → 末段均 %.1f", first, last)
		}
	}
	if first, last, ok := gTrendAvg(samples, func(x gSample) float64 { return float64(x.cliGor) }); ok {
		t.Logf("G1 趋势 客户端 goroutine: 首段均 %.1f → 末段均 %.1f（基线 %d）", first, last, baseClientGor)
		if last > first+20 {
			t.Fatalf("长稳客户端 goroutine 持续增长: 首段均 %.1f → 末段均 %.1f", first, last)
		}
	}
	if first, last, ok := gTrendAvg(samples, func(x gSample) float64 { return float64(x.cliFD) }); ok {
		t.Logf("G1 趋势 客户端 fd: 首段均 %.1f → 末段均 %.1f（基线 %d）", first, last, baseClientFD)
		if last > first+16 {
			t.Fatalf("长稳客户端 fd 持续增长: 首段均 %.1f → 末段均 %.1f", first, last)
		}
	}

	// 资源回落：关闭数据面连接后，客户端 goroutine/fd 与服务端 goroutine/RSS 不得持续膨胀。
	all := []*rpcclient.Storage{hotW, hotR, churnC, statC, prep}
	all = append(all, clients...)
	for _, c := range all {
		_ = c.Close()
	}
	deadline := time.Now().Add(20 * time.Second)
	var cliGor, cliFD, srvGor, srvRSS int64
	for {
		time.Sleep(3 * time.Second)
		cliGor = int64(runtime.NumGoroutine())
		cliFD = int64(fFdCount(os.Getpid()))
		srvGor = fServerGoroutines(t, pprofPort)
		srvRSS = fProcRSSMiB(pid)
		if cliGor <= int64(baseClientGor)+20 && srvGor <= baseSrvGor+50 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
	}
	t.Logf("G1 资源: 客户端 goroutine %d→%d fd %d→%d | 服务端 goroutine %d→%d RSS %dMiB→%dMiB fd %d→%d 线程=%d",
		baseClientGor, cliGor, baseClientFD, cliFD, baseSrvGor, srvGor, baseSrvRSS, srvRSS, baseSrvFD, fFdCount(pid), fProcThreads(pid))

	if cliGor > int64(baseClientGor)+20 {
		t.Fatalf("客户端 goroutine 未回落: %d → %d（基线 %d）", baseClientGor, cliGor, baseClientGor)
	}
	if baseClientFD > 0 && cliFD > int64(baseClientFD)+16 {
		t.Fatalf("客户端 fd 未回落: %d → %d", baseClientFD, cliFD)
	}
	if srvGor > baseSrvGor+50 {
		t.Fatalf("服务端 goroutine 未回落: %d → %d", baseSrvGor, srvGor)
	}
	if baseSrvRSS > 0 && srvRSS > baseSrvRSS*2+256 {
		t.Fatalf("服务端 RSS 疑似泄漏: %dMiB → %dMiB", baseSrvRSS, srvRSS)
	}

	log := fLogBytes(srv.logPath)
	for _, bad := range []string{"panic:", "runtime error:", "CQ 溢出"} {
		if strings.Contains(log, bad) {
			t.Fatalf("服务端日志出现 %q（尾部）：\n%s", bad, tailStr(log, 2000))
		}
	}
}

// gTrackMax 记录长稳期间的最大延迟（覆盖采样间隔内的尖峰）。
func gTrackMax(maxSeen *atomic.Int64, d time.Duration) {
	for {
		cur := maxSeen.Load()
		if int64(d) <= cur {
			return
		}
		if maxSeen.CompareAndSwap(cur, int64(d)) {
			return
		}
	}
}
