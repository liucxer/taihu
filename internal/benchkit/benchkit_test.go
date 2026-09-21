package benchkit

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件覆盖 benchkit 全部导出与未导出路径：Config.Validate / KeyFor / PartitionRange /
// Run（串行与 pipeline 两条执行循环、write|read|delete 三模式、错误传播）/
// progress（ticker 与 stop 两条分支）/ latencyCollector / report / CPU profile 生命周期。
// 数据面用内存 fake Store（无网络、无磁盘、无 TiKV），全部确定性、秒级。

// benchTestStore 内存 Store：key→数据；putErr/getErr/delErr 注入错误，
// shortRead 让 Get 返回比请求短的缓冲（覆盖短读校验分支）。
type benchTestStore struct {
	mu        sync.Mutex
	data      map[string][]byte
	putErr    error
	getErr    error
	delErr    error
	shortRead bool
	closes    int
}

func newBenchTestStore() *benchTestStore {
	return &benchTestStore{data: make(map[string][]byte)}
}

// seed 预置 size 字节的数据（读/删模式用）。
func (s *benchTestStore) seed(prefix string, count int, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < count; i++ {
		s.data[KeyFor(prefix, i)] = make([]byte, size)
	}
}

func (s *benchTestStore) Put(_ context.Context, key string, size int64, in []byte) error {
	if s.putErr != nil {
		return s.putErr
	}
	if int64(len(in)) < size {
		return errors.New("short data")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = append([]byte(nil), in[:size]...)
	return nil
}

func (s *benchTestStore) Get(_ context.Context, key string, off, size int64) ([]byte, func(), error) {
	if s.getErr != nil {
		return nil, nil, s.getErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	if !ok {
		return nil, nil, errors.New("not found: " + key)
	}
	if off < 0 || off > int64(len(v)) {
		return nil, nil, errors.New("bad range")
	}
	end := off + size
	if end > int64(len(v)) {
		end = int64(len(v))
	}
	out := append([]byte(nil), v[off:end]...)
	if s.shortRead && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out, func() {}, nil
}

func (s *benchTestStore) Delete(_ context.Context, key string) error {
	if s.delErr != nil {
		return s.delErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (s *benchTestStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return nil
}

// TestConfigValidate 覆盖 Validate 的全部分支（合法三模式 + 各非法参数）。
func TestConfigValidate(t *testing.T) {
	ok := func() Config {
		return Config{Mode: "write", Size: 4096, Threads: 2, Count: 8, Pipeline: 1}
	}
	for _, mode := range []string{"write", "read", "delete"} {
		c := ok()
		c.Mode = mode
		if err := c.Validate(); err != nil {
			t.Fatalf("mode %s: unexpected err %v", mode, err)
		}
	}
	cases := []struct {
		name  string
		mut   func(c *Config)
		parts string
	}{
		{"bad-mode", func(c *Config) { c.Mode = "load" }, "invalid -mode"},
		{"empty-mode", func(c *Config) { c.Mode = "" }, "invalid -mode"},
		{"neg-size", func(c *Config) { c.Size = -1 }, "-size must be >= 0"},
		{"zero-threads", func(c *Config) { c.Threads = 0 }, "-threads must be > 0"},
		{"neg-threads", func(c *Config) { c.Threads = -3 }, "-threads must be > 0"},
		{"zero-count", func(c *Config) { c.Count = 0 }, "-count must be > 0"},
		{"neg-count", func(c *Config) { c.Count = -5 }, "-count must be > 0"},
		{"zero-pipeline", func(c *Config) { c.Pipeline = 0 }, "-pipeline must be >= 1"},
		{"neg-pipeline", func(c *Config) { c.Pipeline = -2 }, "-pipeline must be >= 1"},
	}
	for _, tc := range cases {
		c := ok()
		tc.mut(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.parts) {
			t.Fatalf("%s: err=%v want contains %q", tc.name, err, tc.parts)
		}
	}
}

// TestKeyForAndPartitionRange 覆盖 key 生成与区间切分（含 count<threads、越界 i、负值）。
func TestKeyForAndPartitionRange(t *testing.T) {
	if got := KeyFor("rbench", 7); got != "rbench/7" {
		t.Fatalf("KeyFor=%q", got)
	}
	if got := KeyFor("", 0); got != "/0" {
		t.Fatalf("KeyFor empty prefix=%q", got)
	}

	// 边界：count<threads（前 count 段各 1 个、其余空）、i>=threads 越界、
	// count==0、count 为负（区间退化到负边界但不 panic）、单线程单 key。
	cases := []struct {
		count, threads, i int
		ws, we            int
	}{
		{10, 3, 0, 0, 4},
		{10, 3, 1, 4, 7},
		{10, 3, 2, 7, 10},
		{10, 3, 3, 10, 13},
		{2, 5, 0, 0, 1},
		{2, 5, 1, 1, 2},
		{2, 5, 2, 2, 2},
		{2, 5, 4, 2, 2},
		{0, 4, 0, 0, 0},
		{-1, 2, 0, -1, -1},
		{1, 1, 0, 0, 1},
	}
	for _, tc := range cases {
		s, e := PartitionRange(tc.count, tc.threads, tc.i)
		if s != tc.ws || e != tc.we {
			t.Fatalf("PartitionRange(%d,%d,%d)=(%d,%d) want (%d,%d)",
				tc.count, tc.threads, tc.i, s, e, tc.ws, tc.we)
		}
	}

	// 全覆盖性：任意 count/threads 下各段互不相交且并集恰为 [0,count)。
	for count := 0; count <= 17; count++ {
		for threads := 1; threads <= 6; threads++ {
			seen := make(map[int]bool, count)
			for i := 0; i < threads; i++ {
				s, e := PartitionRange(count, threads, i)
				if e < s {
					t.Fatalf("count=%d threads=%d i=%d: end<start", count, threads, i)
				}
				for k := s; k < e; k++ {
					if seen[k] {
						t.Fatalf("count=%d threads=%d: key %d 被多个 worker 处理", count, threads, k)
					}
					seen[k] = true
				}
			}
			if len(seen) != count {
				t.Fatalf("count=%d threads=%d: 覆盖 %d 个 key", count, threads, len(seen))
			}
		}
	}
}

// TestProgressStartStop 覆盖 progress 的 ticker 分支与 stop 分支。
func TestProgressStartStop(t *testing.T) {
	var ops atomic.Int64
	p := newProgress(&ops, 10, time.Millisecond)
	if p.total != 10 || p.ops != &ops {
		t.Fatalf("newProgress fields: %+v", p)
	}
	p.start()
	for i := 0; i < 4; i++ {
		ops.Add(1)
	}
	time.Sleep(30 * time.Millisecond) // 至少触发一轮 ticker（打印分支）
	p.stop()                          // 阻塞至 goroutine 退出（stop 分支）

	// 未等到任何 tick 就停止：stopCh 先就绪的分支。
	var ops2 atomic.Int64
	p2 := newProgress(&ops2, 3, time.Hour)
	p2.start()
	p2.stop()
	select {
	case <-p2.doneCh:
	default:
		t.Fatal("stop 返回后 doneCh 必须已关闭")
	}
}

// TestLatencyCollectorAndReport 覆盖 add 与 report 的分位/空值/关闭延迟三条路径。
func TestLatencyCollectorAndReport(t *testing.T) {
	lat := newLatencyCollector()
	for _, d := range []time.Duration{3 * time.Millisecond, time.Millisecond, 2 * time.Millisecond} {
		lat.add(d)
	}
	if len(lat.vals) != 3 {
		t.Fatalf("collector vals=%d want 3", len(lat.vals))
	}

	cfg := Config{Mode: "read", Size: 4096, Threads: 2, Count: 3, Latency: true}
	report(cfg, 3, lat, 20*time.Millisecond, "taihu bench x", "endpoint=x") // 非空：打印 p50/p90/p99
	if lat.vals[0] != 3*time.Millisecond {
		t.Fatal("report 不得就地排序调用方数据")
	}

	// 空样本：跳过分位打印（len(vals) == 0 分支）。
	report(cfg, 0, newLatencyCollector(), 20*time.Millisecond, "taihu bench x", "endpoint=x")

	// 未开启延迟统计：不进分位分支。
	cfg.Latency = false
	report(cfg, 3, lat, 20*time.Millisecond, "taihu bench x", "endpoint=x")
}

// TestStartStopCPUProfile 覆盖 StartCPUProfile 的空路径/成功/创建失败/重复启动错误与 StopCPUProfile。
func TestStartStopCPUProfile(t *testing.T) {
	f, err := StartCPUProfile("")
	if f != nil || err != nil {
		t.Fatalf("StartCPUProfile(\"\")=(%v,%v) want (nil,nil)", f, err)
	}
	StopCPUProfile(nil) // 空操作

	dir := t.TempDir()
	first, err := StartCPUProfile(filepath.Join(dir, "cpu1.prof"))
	if err != nil {
		t.Fatalf("StartCPUProfile: %v", err)
	}
	// 已在采样中：第二次启动必须报错，且不泄漏文件句柄。
	second, err := StartCPUProfile(filepath.Join(dir, "cpu2.prof"))
	if err == nil || second != nil {
		t.Fatalf("重复 StartCPUProfile=(%v,%v) want (nil,err)", second, err)
	}
	StopCPUProfile(first)

	// os.Create 失败（目标是目录）。
	if f, err := StartCPUProfile(dir); err == nil || f != nil {
		t.Fatalf("StartCPUProfile(dir)=(%v,%v) want (nil,err)", f, err)
	}
}

// TestRunSerialModes 覆盖 Run + runWorker 的串行路径（write/read/delete、latency 开关、
// count<threads、report 汇总），含延迟分位与进度打印。
func TestRunSerialModes(t *testing.T) {
	for _, mode := range []string{"write", "read", "delete"} {
		for _, latency := range []bool{false, true} {
			st := newBenchTestStore()
			st.seed("p", 12, 32)
			cfg := Config{
				Mode: mode, Size: 32, Threads: 3, Count: 12, Prefix: "p",
				ReportEvery: time.Millisecond, Latency: latency, Pipeline: 1,
			}
			if err := Run(context.Background(), st, cfg, "taihu bench test", "endpoint=mem"); err != nil {
				t.Fatalf("mode=%s latency=%v: %v", mode, latency, err)
			}
			if mode == "write" {
				st.mu.Lock()
				n := len(st.data)
				st.mu.Unlock()
				if n != 12 {
					t.Fatalf("write 后应有 12 个对象，实际 %d", n)
				}
			}
			if mode == "delete" {
				st.mu.Lock()
				n := len(st.data)
				st.mu.Unlock()
				if n != 0 {
					t.Fatalf("delete 后应为空，实际 %d", n)
				}
			}
		}
	}

	// count < threads：多余 worker 拿到空区间，不得 panic，report 仍输出。
	st := newBenchTestStore()
	cfg := Config{Mode: "write", Size: 8, Threads: 6, Count: 2, Prefix: "q", ReportEvery: time.Hour, Pipeline: 1}
	if err := Run(context.Background(), st, cfg, "taihu bench test", "endpoint=mem"); err != nil {
		t.Fatalf("count<threads: %v", err)
	}

	// count == threads 且 size == 0（零长对象）路径。
	st0 := newBenchTestStore()
	cfg0 := Config{Mode: "write", Size: 0, Threads: 4, Count: 4, Prefix: "z", ReportEvery: time.Hour, Pipeline: 1}
	if err := Run(context.Background(), st0, cfg0, "taihu bench test", "endpoint=mem"); err != nil {
		t.Fatalf("size=0: %v", err)
	}
}

// TestRunPipelinedModes 覆盖 runPipelinedWorker（Pipeline>1）的三模式与区间边界。
func TestRunPipelinedModes(t *testing.T) {
	for _, mode := range []string{"write", "read", "delete"} {
		st := newBenchTestStore()
		st.seed("pp", 9, 16)
		cfg := Config{
			Mode: mode, Size: 16, Threads: 2, Count: 9, Prefix: "pp",
			ReportEvery: time.Hour, Latency: true, Pipeline: 3,
		}
		if err := Run(context.Background(), st, cfg, "taihu bench test", "endpoint=mem"); err != nil {
			t.Fatalf("pipelined mode=%s: %v", mode, err)
		}
	}

	// 空区间（count < threads）在 pipeline 模式下不得启动无效 op。
	st := newBenchTestStore()
	cfg := Config{Mode: "read", Size: 16, Threads: 4, Count: 1, Prefix: "pp",
		ReportEvery: time.Hour, Pipeline: 2}
	st.seed("pp", 1, 16)
	if err := Run(context.Background(), st, cfg, "taihu bench test", "endpoint=mem"); err != nil {
		t.Fatalf("pipelined count<threads: %v", err)
	}
}

// TestRunWorkerErrors 覆盖 Run 的错误传播：非流水线与流水线两实现的短读/存储错误，
// 以及错误优先返回后仍停机（progress 不泄漏）。
func TestRunWorkerErrors(t *testing.T) {
	base := Config{Mode: "write", Size: 4096, Threads: 2, Count: 4, Prefix: "e", ReportEvery: time.Hour, Pipeline: 1}

	// 串行 write 失败。
	st := newBenchTestStore()
	st.putErr = errors.New("put boom")
	if err := Run(context.Background(), st, base, "taihu bench test", "endpoint=mem"); err == nil ||
		!strings.Contains(err.Error(), "put boom") {
		t.Fatalf("write err=%v want put boom", err)
	}

	// 串行 read：存储报错。
	stGet := newBenchTestStore()
	stGet.getErr = errors.New("get boom")
	cfg := base
	cfg.Mode = "read"
	if err := Run(context.Background(), stGet, cfg, "taihu bench test", "endpoint=mem"); err == nil ||
		!strings.Contains(err.Error(), "get boom") {
		t.Fatalf("read err=%v want get boom", err)
	}

	// 串行 read：短读（len != size 且无错误）。
	stShort := newBenchTestStore()
	stShort.seed("e", 4, 64)
	cfg = base
	cfg.Mode = "read"
	cfg.Size = 64
	stShort.shortRead = true
	if err := Run(context.Background(), stShort, cfg, "taihu bench test", "endpoint=mem"); err == nil ||
		!strings.Contains(err.Error(), "short read") {
		t.Fatalf("short read err=%v want short read", err)
	}

	// 串行 delete 失败。
	stDel := newBenchTestStore()
	stDel.delErr = errors.New("del boom")
	cfg = base
	cfg.Mode = "delete"
	if err := Run(context.Background(), stDel, cfg, "taihu bench test", "endpoint=mem"); err == nil ||
		!strings.Contains(err.Error(), "del boom") {
		t.Fatalf("delete err=%v want del boom", err)
	}

	// 流水线 read 短读。
	stPShort := newBenchTestStore()
	stPShort.seed("e", 4, 64)
	stPShort.shortRead = true
	cfg = base
	cfg.Mode = "read"
	cfg.Size = 64
	cfg.Pipeline = 3
	cfg.Latency = true
	if err := Run(context.Background(), stPShort, cfg, "taihu bench test", "endpoint=mem"); err == nil ||
		!strings.Contains(err.Error(), "short read") {
		t.Fatalf("pipelined short read err=%v", err)
	}

	// 流水线 write 失败。
	stPWrite := newBenchTestStore()
	stPWrite.putErr = errors.New("batch put boom")
	cfg = base
	cfg.Pipeline = 2
	if err := Run(context.Background(), stPWrite, cfg, "taihu bench test", "endpoint=mem"); err == nil ||
		!strings.Contains(err.Error(), "batch put boom") {
		t.Fatalf("pipelined write err=%v", err)
	}
}

// TestRunContextAndStoreClose 覆盖多线程下 ctx 传递与 Close 可调用（Store 契约）。
func TestRunContextAndStoreClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := newBenchTestStore()
	cfg := Config{Mode: "write", Size: 1024, Threads: 4, Count: 16, Prefix: "c",
		ReportEvery: time.Hour, Latency: true, Pipeline: 2}
	if err := Run(ctx, st, cfg, "taihu bench test", "endpoint=mem"); err != nil {
		t.Fatalf("Run with cancellable ctx: %v", err)
	}
	if err := st.Close(); err != nil || st.closes != 1 {
		t.Fatalf("Close=%v closes=%d", err, st.closes)
	}
}

// TestReportZeroElapsed 覆盖 elapsed 为 0 的极端输入（吞吐计算不得 panic）。
func TestReportZeroElapsed(t *testing.T) {
	cfg := Config{Mode: "write", Size: 1, Threads: 1, Count: 1, Latency: true}
	report(cfg, 0, newLatencyCollector(), 0, "taihu bench test", "endpoint=mem")
}
