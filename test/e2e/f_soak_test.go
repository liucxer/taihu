//go:build e2e

package e2e

// F 组：长稳 + 性能回归。用户口径：**带宽 / 延迟 / CPU 三维**。
//
// 重要前提（决定了本组能断言什么）：e2e 的块设备是 `losetup` 挂在 /var/tmp 稀疏文件上的
// loop 设备，性能远低于真实 NVMe 且抖动大，因此**不做绝对带宽断言**。本组只做可在 loop
// 上稳定复现的回归护栏：
//   - F1 稳定性：长稳后客户端/服务端资源（goroutine / fd / RSS）回落，无错误、无 panic，
//     客户端观测延迟有界（延迟维度）。
//   - F2 扩展性：4 并发客户端吞吐不得劣于单客户端（禁止串行化回归），并输出三维数据。
//   - F3 后端等价：`--io-uring=on|off` 两种 AIO 后端都能跑、数据正确、吞吐同量级，
//     并在日志中如实报告所选后端。
//
// 关于 "slow request"：该字样来自上层 EFS_nefs FUSE 客户端日志（本仓库 doc/ 性能报告），
// L1（taihu 自身）看不到，故 F1 改为断言「服务端日志无 error/panic + 客户端自测延迟有界」。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/rpcclient"
)

// ---------------------------------------------------------------- 环境/度量工具

// fDuration 读环境变量覆盖的秒数，缺省 def 秒。
func fDuration(t *testing.T, env string, def time.Duration) time.Duration {
	t.Helper()
	if s := envStr(env); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			t.Fatalf("%s=%q 非法", env, s)
		}
		return time.Duration(n) * time.Second
	}
	return def
}

// fLogContains 服务端日志是否含子串（日志由 harness 落到 srv.logPath）。
func fLogContains(logPath, substr string) bool {
	b, err := os.ReadFile(logPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), substr)
}

// fLogBytes 读取服务端日志全文（失败返回空串）。
func fLogBytes(logPath string) string {
	b, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	return string(b)
}

// fLogLine 返回日志中第一条含 substr 的行（已 TrimSpace）；无则空串。
// 用于证据留存：F3 要求「日志如实报告实际生效的后端」，所以记录**真实行**而非期望串。
func fLogLine(logPath, substr string) string {
	for _, line := range strings.Split(fLogBytes(logPath), "\n") {
		if strings.Contains(line, substr) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// fProcField 读 /proc/<pid>/status 的某字段数值（kB 原样返回）。
func fProcStatusKB(pid int, field string) (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, field+":") {
			f := strings.Fields(line)
			if len(f) < 2 {
				return 0, fmt.Errorf("字段 %s 格式异常: %q", field, line)
			}
			return strconv.ParseInt(f[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("字段 %s 不存在", field)
}

// fProcRSSMiB 服务端进程常驻内存（MiB）。
func fProcRSSMiB(pid int) int64 {
	kb, err := fProcStatusKB(pid, "VmRSS")
	if err != nil {
		return -1
	}
	return kb / 1024
}

// fProcThreads 服务端进程线程数。
func fProcThreads(pid int) int64 {
	n, err := fProcStatusKB(pid, "Threads")
	if err != nil {
		return -1
	}
	return n
}

// fProcCPUSeconds 进程累计 CPU 秒（utime+stime）。
//
// /proc/<pid>/stat 第 1、2 字段是 pid 与 (comm)，comm 可能含空格/括号，故先按最后一个
// ')' 截断，fields[0] 即第 3 字段（state），fields[k] 对应第 k+3 字段。utime/stime 是
// 第 14/15 字段 → fields[11]/fields[12]（**不是** fields[13]/fields[14]，后者是
// cutime/cstime，即被 wait 的子进程 CPU；本进程不 fork 时恒为 0，会把 CPU 维度读成 0）。
func fProcCPUSeconds(pid int) float64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 > len(s) {
		return -1
	}
	f := strings.Fields(s[i+2:])
	if len(f) < 13 {
		return -1
	}
	ut, e1 := strconv.ParseFloat(f[11], 64)
	st, e2 := strconv.ParseFloat(f[12], 64)
	if e1 != nil || e2 != nil {
		return -1
	}
	return (ut + st) / 100.0 // USER_HZ 恒为 100（/proc 时间字段单位，与内核 HZ 无关）
}

// fMachineCPU 读 /proc/stat 首行，返回整机累计 CPU 秒：busy=(user+nice+system+irq+
// softirq)（即「usr+sys」口径），idle=(idle+iowait)。返回 -1,-1 表示读取失败。
// 仅用于给出「整机 usr+sys 占了几核」这一维，不作为任何「CPU 是否瓶颈」的论据。
func fMachineCPU() (busy, idle float64) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return -1, -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)[1:]
		at := func(i int) float64 {
			if i >= len(f) {
				return 0
			}
			v, _ := strconv.ParseFloat(f[i], 64)
			return v
		}
		// 字段序：user nice system idle iowait irq softirq steal ...
		return (at(0) + at(1) + at(2) + at(5) + at(6)) / 100.0,
			(at(3) + at(4)) / 100.0
	}
	return -1, -1
}

// fProcIOStats 是 /proc/<pid>/io 的 syscall/字节计数快照（syscall 分布维度的数据源）。
type fProcIOStats struct {
	syscr, syscw          int64 // read/write syscall 次数
	rchar, wchar          int64 // 经 read/write 的字节（含缓存命中）
	readBytes, writeBytes int64 // 真正落到存储层的字节
}

// fProcReadIO 读 /proc/<pid>/io；任一字段缺失保持 -1。
func fProcReadIO(pid int) fProcIOStats {
	out := fProcIOStats{-1, -1, -1, -1, -1, -1}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/io", pid))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "syscr:":
			out.syscr = v
		case "syscw:":
			out.syscw = v
		case "rchar:":
			out.rchar = v
		case "wchar:":
			out.wchar = v
		case "read_bytes:":
			out.readBytes = v
		case "write_bytes:":
			out.writeBytes = v
		}
	}
	return out
}

// fCores 把 CPU 秒换算成「平均占用核数」。
func fCores(cpuSeconds float64, el time.Duration) float64 {
	if cpuSeconds < 0 || el <= 0 {
		return -1
	}
	return cpuSeconds / el.Seconds()
}

// fCPUProfilePath 输出 CPU profile 的落盘位置（放在用例目录之外，避免 t.Cleanup
// RemoveAll 时被删；供跑完后 `go tool pprof -top` 取 syscall/热点分布）。
func fCPUProfilePath(t *testing.T, name string) string {
	return filepath.Join(workdirRoot(t), "f-"+name+".pprof")
}

// fStartCPUProfile 从服务端 pprof 抓 seconds 秒 CPU profile 到 out，返回等待函数。
// 抓取在后台 goroutine 进行，与调用方的压测窗口重叠；失败仅记日志，不影响判定
// （CPU profile 只是三维里的证据，取不到不该让用例红）。
func fStartCPUProfile(t *testing.T, port int, seconds int, out string) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		url := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/profile?seconds=%d", port, seconds)
		cl := &http.Client{Timeout: time.Duration(seconds+30) * time.Second}
		resp, err := cl.Get(url)
		if err != nil {
			t.Logf("抓 CPU profile 失败(%s): %v", url, err)
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			t.Logf("读 CPU profile 失败: %v", err)
			return
		}
		if err := os.WriteFile(out, b, 0o644); err != nil {
			t.Logf("写 CPU profile %s 失败: %v", out, err)
			return
		}
		t.Logf("CPU profile 已落盘: %s (%d 字节)", out, len(b))
	}()
	return func() { <-done }
}

// fFdCount 进程已打开 fd 数。
func fFdCount(pid int) int {
	ents, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return -1
	}
	return len(ents)
}

var fPprofPortRe = regexp.MustCompile(`pprof listening on :(\d+)`)

// fPprofPort 从服务端日志解析 pprof 监听端口。
func fPprofPort(t *testing.T, logPath string) int {
	t.Helper()
	var port int
	waitFor(t, 20*time.Second, "服务端日志出现 pprof 监听端口", func() bool {
		m := fPprofPortRe.FindStringSubmatch(fLogBytes(logPath))
		if m == nil {
			return false
		}
		port, _ = strconv.Atoi(m[1])
		return port > 0
	})
	return port
}

var fGoroutineTotalRe = regexp.MustCompile(`goroutine profile: total (\d+)`)

// fServerGoroutines 经 pprof 读取服务端 goroutine 数（非 Linux 侧计数只能这样拿）。
func fServerGoroutines(t *testing.T, port int) int64 {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/goroutine?debug=1", port)
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("读 pprof 响应: %v", err)
	}
	m := fGoroutineTotalRe.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("pprof 响应无 goroutine 总数: %.200s", body)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// fLatency 并发安全的延迟采样（容量固定，溢出丢弃；只用于百分位，无需全量）。
type fLatency struct {
	mu   sync.Mutex
	samp []time.Duration
	cap  int
}

func newFLatency(capacity int) *fLatency {
	return &fLatency{samp: make([]time.Duration, 0, capacity), cap: capacity}
}

func (l *fLatency) add(d time.Duration) {
	l.mu.Lock()
	if len(l.samp) < l.cap {
		l.samp = append(l.samp, d)
	}
	l.mu.Unlock()
}

// stats 返回 (样本数, p50, p99, max)。
func (l *fLatency) stats() (int, time.Duration, time.Duration, time.Duration) {
	l.mu.Lock()
	s := append([]time.Duration(nil), l.samp...)
	l.mu.Unlock()
	if len(s) == 0 {
		return 0, 0, 0, 0
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	pick := func(q float64) time.Duration {
		i := int(float64(len(s)-1) * q)
		return s[i]
	}
	return len(s), pick(0.50), pick(0.99), s[len(s)-1]
}

// ---------------------------------------------------------------- 公共数据准备

// fPrepareObjects 预写 n 个 4MiB 整块对象（顺序写，避免把写路径算进读吞吐）。
func fPrepareObjects(t *testing.T, c *rpcclient.Storage, h *harness, n int) ([]string, [][]byte) {
	t.Helper()
	keys := make([]string, n)
	contents := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = h.key(fmt.Sprintf("f/obj-%04d", i))
		contents[i] = pattern(fmt.Sprintf("F-%d", i), 4<<20)
		if err := c.Put(context.Background(), keys[i], int64(len(contents[i])), contents[i]); err != nil {
			t.Fatalf("预热 Put(%s): %v", keys[i], err)
		}
	}
	return keys, contents
}

// fReadAll 以 len(clients) 路并发把 keys 整对象读 rounds 轮，校验内容并返回
// (总字节, 总耗时, 错误数)。lat 非空时按次采样整对象 Get 延迟（延迟维度）。
func fReadAll(t *testing.T, clients []*rpcclient.Storage, keys []string, contents [][]byte, rounds int, lat *fLatency) (int64, time.Duration, int64) {
	t.Helper()
	var (
		bytesRead atomic.Int64
		errs      atomic.Int64
		wg        sync.WaitGroup
	)
	perWorker := (len(keys) + len(clients) - 1) / len(clients)
	start := time.Now()
	for w, c := range clients {
		lo := w * perWorker
		hi := lo + perWorker
		if hi > len(keys) {
			hi = len(keys)
		}
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(c *rpcclient.Storage, lo, hi int) {
			defer wg.Done()
			ctx := context.Background()
			for r := 0; r < rounds; r++ {
				for i := lo; i < hi; i++ {
					t0 := time.Now()
					got, rel, err := c.Get(ctx, keys[i], 0, -1)
					if err != nil {
						errs.Add(1)
						t.Errorf("Get(%s): %v", keys[i], err)
						continue
					}
					if !bytes.Equal(got, contents[i]) {
						errs.Add(1)
						t.Errorf("Get(%s) 内容不一致: len=%d/%d", keys[i], len(got), len(contents[i]))
					}
					rel()
					if lat != nil {
						lat.add(time.Since(t0))
					}
					bytesRead.Add(int64(len(got)))
				}
			}
		}(c, lo, hi)
	}
	wg.Wait()
	return bytesRead.Load(), time.Since(start), errs.Load()
}

// ---------------------------------------------------------------- F1 长稳

// TestF1SoakStability 长稳压测（默认 60s，E2E_SOAK_SECONDS 可覆盖）：混合读/区间读/覆盖写，
// 期间统计延迟；结束后断言资源回落、无错误、服务端日志无 panic。
func TestF1SoakStability(t *testing.T) {
	dur := fDuration(t, "E2E_SOAK_SECONDS", 60*time.Second)
	h := newHarness(t)
	srv := h.startServer(serverOpts{devSize: 32 << 30})
	pid := srv.cmd.Process.Pid

	prep := h.dialDirect(srv, 8)
	const nObj = 128 // 128 × 4MiB = 512MiB
	keys, contents := fPrepareObjects(t, prep, h, nObj)

	pprofPort := fPprofPort(t, srv.logPath)

	// 基线：客户端侧 goroutine/fd + 服务端 RSS/线程/goroutine。
	baseClientGor := runtime.NumGoroutine()
	baseClientFD := fFdCount(os.Getpid())
	baseSrvRSS := fProcRSSMiB(pid)
	baseSrvGor := fServerGoroutines(t, pprofPort)
	cpuBefore := fProcCPUSeconds(pid)

	const workers = 8
	clients := make([]*rpcclient.Storage, workers)
	for i := range clients {
		c, err := rpcclient.DialPool(h.ctx, srv.info.Addr, 2)
		if err != nil {
			t.Fatalf("DialPool #%d: %v", i, err)
		}
		clients[i] = c
	}

	// CPU 三维基线：整机 usr+sys、服务端/客户端进程、服务端 syscall 计数。
	mBusy0, _ := fMachineCPU()
	cliCPU0 := fProcCPUSeconds(os.Getpid())
	io0 := fProcReadIO(pid)

	var (
		ops       atomic.Int64
		readBytes atomic.Int64
		errs      atomic.Int64
		lat       = newFLatency(200000)
		wg        sync.WaitGroup
		poolEnd   = time.Now().Add(dur)
	)
	// 长稳窗口内抓一份服务端 CPU profile（syscall/热点分布维度的证据；只记日志、不参与判定）。
	stopProf := fStartCPUProfile(t, pprofPort, int(dur.Seconds()), fCPUProfilePath(t, "f1-server-cpu"))
	soakStart := time.Now()
	for w := 0; w < workers; w++ {
		c := clients[w]
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			i := w
			for time.Now().Before(poolEnd) {
				i = (i + 1) % nObj
				start := time.Now()
				got, rel, err := c.Get(ctx, keys[i], 0, -1)
				if err != nil {
					errs.Add(1)
					t.Errorf("soak Get(%s): %v", keys[i], err)
					continue
				}
				if !bytes.Equal(got, contents[i]) {
					errs.Add(1)
					t.Errorf("soak Get(%s) 内容不一致: len=%d/%d", keys[i], len(got), len(contents[i]))
				}
				rel()
				lat.add(time.Since(start))
				readBytes.Add(int64(len(got)))
				ops.Add(1)

				// 每 8 次读插一次非对齐区间读（覆盖尾部/跨块路径）。
				if ops.Load()%8 == 0 {
					const roff, rsize = 4<<20 - 8192, 4097
					s, rel2, err := c.Get(ctx, keys[i], roff, rsize)
					if err != nil {
						errs.Add(1)
						t.Errorf("soak 区间读(%s): %v", keys[i], err)
					} else {
						if !bytes.Equal(s, contents[i][roff:roff+rsize]) {
							errs.Add(1)
							t.Errorf("soak 区间读(%s) 内容不一致: len=%d want %d", keys[i], len(s), rsize)
						}
						rel2()
					}
					ops.Add(1)
				}
				// 每 32 次读插一次覆盖写（写路径同样进长稳）。
				if ops.Load()%32 == 0 {
					if err := c.Put(ctx, keys[i], int64(len(contents[i])), contents[i]); err != nil {
						errs.Add(1)
						t.Errorf("soak Put(%s): %v", keys[i], err)
					}
					ops.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	stopProf()

	n, p50, p99, maxLat := lat.stats()
	elapsed := time.Since(soakStart)
	cpuDuring := fProcCPUSeconds(pid) - cpuBefore
	cliCPU := fProcCPUSeconds(os.Getpid()) - cliCPU0
	mBusy1, _ := fMachineCPU()
	io1 := fProcReadIO(pid)
	bwMiBs := float64(readBytes.Load()) / elapsed.Seconds() / (1 << 20)
	t.Logf("F1: 时长=%s ops=%d 错误=%d 延迟样本=%d p50=%s p99=%s max=%s 整读带宽=%.1f MiB/s",
		elapsed.Round(time.Millisecond), ops.Load(), errs.Load(), n, p50.Round(time.Microsecond), p99.Round(time.Microsecond), maxLat.Round(time.Millisecond), bwMiBs)
	t.Logf("F1 CPU: 服务端 %.2f 核·秒(%.2f 核) 客户端 %.2f 核·秒(%.2f 核) 整机 usr+sys %.2f 核·秒(%.2f 核)",
		cpuDuring, fCores(cpuDuring, elapsed), cliCPU, fCores(cliCPU, elapsed),
		mBusy1-mBusy0, fCores(mBusy1-mBusy0, elapsed))
	t.Logf("F1 syscall(服务端 /proc/io 增量): read=%d write=%d rchar=%dMiB wchar=%dMiB read_bytes=%dMiB write_bytes=%dMiB",
		io1.syscr-io0.syscr, io1.syscw-io0.syscw, (io1.rchar-io0.rchar)>>20, (io1.wchar-io0.wchar)>>20,
		(io1.readBytes-io0.readBytes)>>20, (io1.writeBytes-io0.writeBytes)>>20)
	t.Logf("F1 口径(durable): 每次 Get 走完 server→块设备→回帧并逐字节校验后才计入；Put 后由后续 Get 校验，无应用层缓存值")

	if errs.Load() != 0 {
		t.Fatalf("长稳期间出现 %d 个错误", errs.Load())
	}
	if ops.Load() == 0 {
		t.Fatal("长稳期间未完成任何操作")
	}
	if maxLat > 5*time.Second {
		t.Fatalf("长稳出现超长延迟 max=%s（p99=%s）", maxLat, p99)
	}

	// 资源回落：关闭数据面连接后，客户端 goroutine/fd 与服务端 goroutine/RSS 不得持续膨胀。
	for _, c := range clients {
		_ = c.Close()
	}
	settle := 3 * time.Second
	deadline := time.Now().Add(20 * time.Second)
	var cliGor, cliFD, srvGor, srvRSS int64
	for {
		time.Sleep(settle)
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
	t.Logf("F1 资源: 客户端 goroutine %d→%d fd %d→%d | 服务端 goroutine %d→%d RSS %dMiB→%dMiB 线程=%d",
		baseClientGor, cliGor, baseClientFD, cliFD, baseSrvGor, srvGor, baseSrvRSS, srvRSS, fProcThreads(pid))

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

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ---------------------------------------------------------------- F2 扩展性

// TestF2Scaling 单客户端 vs 4 客户端读同一对象集：4 并发吞吐不得劣化（回归护栏，防串行化），
// 并输出带宽/延迟/CPU 三维。
func TestF2Scaling(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{devSize: 32 << 30})
	pid := srv.cmd.Process.Pid

	prep := h.dialDirect(srv, 8)
	const nObj = 64 // 256MiB
	keys, contents := fPrepareObjects(t, prep, h, nObj)

	// f2Stat 一次测量的三维数据：带宽 / 延迟 / （CPU 在调用侧按窗口差分）。
	type f2Stat struct {
		bw            float64
		el            time.Duration
		n             int
		p50, p99, max time.Duration
	}
	measure := func(nClients, rounds int) f2Stat {
		clients := make([]*rpcclient.Storage, nClients)
		for i := range clients {
			c, err := rpcclient.DialPool(h.ctx, srv.info.Addr, 2)
			if err != nil {
				t.Fatalf("DialPool #%d: %v", i, err)
			}
			clients[i] = c
		}
		defer func() {
			for _, c := range clients {
				_ = c.Close()
			}
		}()
		// 预热一轮，避免首次触碰的 page cache / 连接建立计入。
		if _, _, errs := fReadAll(t, clients, keys, contents, 1, nil); errs != 0 {
			t.Fatalf("预热出现 %d 个错误", errs)
		}
		lat := newFLatency(200000)
		bytesRead, el, errs := fReadAll(t, clients, keys, contents, rounds, lat)
		if errs != 0 {
			t.Fatalf("扩展性测量出现 %d 个错误", errs)
		}
		n, p50, p99, maxLat := lat.stats()
		return f2Stat{float64(bytesRead) / el.Seconds() / (1 << 20), el, n, p50, p99, maxLat}
	}

	mBusy0, _ := fMachineCPU()
	cliCPU0 := fProcCPUSeconds(os.Getpid())
	io0 := fProcReadIO(pid)
	cpu0 := fProcCPUSeconds(pid)
	s1 := measure(1, 4)
	cpu1 := fProcCPUSeconds(pid)
	s4 := measure(4, 4)
	cpu4 := fProcCPUSeconds(pid)
	cliCPU := fProcCPUSeconds(os.Getpid()) - cliCPU0
	mBusy1, _ := fMachineCPU()
	io1 := fProcReadIO(pid)

	t.Logf("F2 单客户端: %.1f MiB/s (%s) 延迟 n=%d p50=%s p99=%s max=%s", s1.bw, s1.el.Round(time.Millisecond), s1.n, s1.p50.Round(time.Microsecond), s1.p99.Round(time.Microsecond), s1.max.Round(time.Millisecond))
	t.Logf("F2 4 客户端: %.1f MiB/s (%s) 延迟 n=%d p50=%s p99=%s max=%s | 吞吐加速比 %.2fx",
		s4.bw, s4.el.Round(time.Millisecond), s4.n, s4.p50.Round(time.Microsecond), s4.p99.Round(time.Microsecond), s4.max.Round(time.Millisecond), s4.bw/s1.bw)
	t.Logf("F2 CPU: 服务端 单=%.2f 核(%.2f 核·秒) 4=%.2f 核(%.2f 核·秒) | 客户端进程 全程=%.2f 核 | 整机 usr+sys=%.2f 核·秒(%.2f 核)",
		fCores(cpu1-cpu0, s1.el), cpu1-cpu0, fCores(cpu4-cpu1, s4.el), cpu4-cpu1,
		fCores(cliCPU, s1.el+s4.el), mBusy1-mBusy0, fCores(mBusy1-mBusy0, s1.el+s4.el))
	t.Logf("F2 syscall(服务端 /proc/io 增量): read=%d write=%d rchar=%dMiB wchar=%dMiB read_bytes=%dMiB write_bytes=%dMiB",
		io1.syscr-io0.syscr, io1.syscw-io0.syscw, (io1.rchar-io0.rchar)>>20, (io1.wchar-io0.wchar)>>20,
		(io1.readBytes-io0.readBytes)>>20, (io1.writeBytes-io0.writeBytes)>>20)
	t.Logf("F2 口径(durable): 每轮测量均先预热一轮，再统计整对象 Get 全链路字节/耗时；带宽=回帧字节/墙钟，无应用层缓存值")

	// 回归护栏：并发不得劣化（loop 设备带宽上限低，故只断言不倒退）。
	if s4.bw < s1.bw*0.95 {
		t.Fatalf("4 客户端吞吐 %.1f MiB/s 劣于单客户端 %.1f MiB/s（并发路径疑似串行化）", s4.bw, s1.bw)
	}
}

// ---------------------------------------------------------------- F3 AIO 双后端

// TestF3AIOBackends `--io-uring=on`（io_uring）与 `--io-uring=off`（libaio）两种 AIO 后端：
// 都能启动、日志如实报告后端、数据正确、吞吐同量级（差距 < 2 倍）。
func TestF3AIOBackends(t *testing.T) {
	type result struct {
		mode     string
		bw       float64
		srvCPU   float64
		observed string
	}
	var results []result

	for _, tc := range []struct{ flag, wantLog string }{
		{"on", "aio backend=io_uring"},
		{"off", "aio backend=libaio"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			h := newHarness(t)
			srv := h.startServer(serverOpts{ioUring: tc.flag, devSize: 16 << 30})
			waitFor(t, 20*time.Second, "服务端日志报告 AIO 后端 %q", func() bool {
				return fLogContains(srv.logPath, tc.wantLog)
			})
			pid := srv.cmd.Process.Pid
			observed := fLogLine(srv.logPath, tc.wantLog)

			c := h.dialDirect(srv, 4)
			keys, contents := fPrepareObjects(t, c, h, 32) // 128MiB

			clients := []*rpcclient.Storage{c}
			mBusy0, _ := fMachineCPU()
			cliCPU0 := fProcCPUSeconds(os.Getpid())
			io0 := fProcReadIO(pid)
			cpu0 := fProcCPUSeconds(pid)
			lat := newFLatency(100000)
			bytesRead, el, errs := fReadAll(t, clients, keys, contents, 5, lat)
			srvCPU := fProcCPUSeconds(pid) - cpu0
			cliCPU := fProcCPUSeconds(os.Getpid()) - cliCPU0
			mBusy1, _ := fMachineCPU()
			io1 := fProcReadIO(pid)
			if errs != 0 {
				t.Fatalf("io-uring=%s 读取出错 %d 次", tc.flag, errs)
			}

			// 正确性：逐对象整读校验（写路径与读路径都要经过所选后端）。
			for i := range keys {
				getExact(t, c, keys[i], contents[i])
			}

			n, p50, p99, maxLat := lat.stats()
			bw := float64(bytesRead) / el.Seconds() / (1 << 20)
			t.Logf("F3 io-uring=%s 带宽=%.1f MiB/s (%s) 延迟 n=%d p50=%s p99=%s max=%s",
				tc.flag, bw, el.Round(time.Millisecond), n, p50.Round(time.Microsecond), p99.Round(time.Microsecond), maxLat.Round(time.Millisecond))
			t.Logf("F3 io-uring=%s CPU: 服务端 %.2f 核·秒(%.2f 核) 客户端 %.2f 核·秒(%.2f 核) 整机 usr+sys %.2f 核·秒(%.2f 核)",
				tc.flag, srvCPU, fCores(srvCPU, el), cliCPU, fCores(cliCPU, el),
				mBusy1-mBusy0, fCores(mBusy1-mBusy0, el))
			t.Logf("F3 io-uring=%s syscall(服务端 /proc/io 增量): read=%d write=%d rchar=%dMiB read_bytes=%dMiB",
				tc.flag, io1.syscr-io0.syscr, io1.syscw-io0.syscw, (io1.rchar-io0.rchar)>>20, (io1.readBytes-io0.readBytes)>>20)
			t.Logf("F3 io-uring=%s 后端日志证据: %s", tc.flag, observed)
			results = append(results, result{mode: tc.flag, bw: bw, srvCPU: srvCPU, observed: observed})
		})
	}

	if len(results) != 2 {
		t.Fatalf("期望 2 个后端结果，实得 %d", len(results))
	}
	a, b := results[0], results[1]
	lo, hi := a.bw, b.bw
	if lo > hi {
		lo, hi = hi, lo
	}
	t.Logf("F3 汇总: io_uring=on %.1f MiB/s vs off %.1f MiB/s（比值 %.2f）", a.bw, b.bw, hi/lo)
	if lo <= 0 {
		t.Fatalf("后端吞吐为 0: on=%.1f off=%.1f", a.bw, b.bw)
	}
	if hi/lo >= 2.0 {
		t.Fatalf("两 AIO 后端吞吐差距过大（io_uring=on %.1f MiB/s, off %.1f MiB/s）", a.bw, b.bw)
	}
}
