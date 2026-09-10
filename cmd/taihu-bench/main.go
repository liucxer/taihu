// Command taihu-bench 是 Storage 层的性能测试工具，按设计文档 §9 实现。
//
// 支持两种模式：
//   - write：并发写 count 个不同 key 的 object（每个 size 字节）。
//   - read：并发读 count 个不同 key 的 object（整对象读），每个 key 全进程只读一次。
//
// 读模式创建 Storage 后先 LoadCache 预热元数据，剔除 pebble 读对带宽的影响。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime/pprof"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/pkg/taihu"
)

type config struct {
	mode        string
	size        int64
	threads     int
	count       int
	prefix      string
	dbDir       string
	devPath     string
	reportEvery time.Duration
	latency     bool
	cpuProfile  string
	memProfile  string
}

func parseFlags() *config {
	c := &config{}
	flag.StringVar(&c.mode, "mode", "", "write | read")
	flag.Int64Var(&c.size, "size", 4096, "object size in bytes")
	flag.IntVar(&c.threads, "threads", 1, "number of concurrent goroutines")
	flag.IntVar(&c.count, "count", 1000, "total number of distinct objects")
	flag.StringVar(&c.prefix, "keys-prefix", "bench", "key prefix, keys are <prefix>/<seq>")
	flag.StringVar(&c.dbDir, "db", "", "pebble metadata directory")
	flag.StringVar(&c.devPath, "dev", "", "raw device path")
	flag.DurationVar(&c.reportEvery, "report-interval", 2*time.Second, "progress report interval")
	flag.BoolVar(&c.latency, "latency", false, "record per-op latency (write and read)")
	flag.StringVar(&c.cpuProfile, "cpuprofile", "", "write cpu profile to this file (pprof)")
	flag.StringVar(&c.memProfile, "memprofile", "", "write memory profile to this file (pprof)")
	flag.Parse()
	return c
}

func main() {
	c := parseFlags()
	if err := c.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "taihu-bench:", err)
		os.Exit(2)
	}

	ctx := context.Background()

	// pprof cpu profile
	var cpuFile *os.File
	if c.cpuProfile != "" {
		var err error
		cpuFile, err = os.Create(c.cpuProfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cpuprofile: %v\n", err)
			os.Exit(1)
		}
		if err := pprof.StartCPUProfile(cpuFile); err != nil {
			fmt.Fprintf(os.Stderr, "StartCPUProfile: %v\n", err)
			os.Exit(1)
		}
	}

	s, err := taihu.NewStorage(ctx, c.dbDir, c.devPath)
	if err != nil {
		stopCPUProfile(cpuFile)
		fmt.Fprintf(os.Stderr, "NewStorage: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = s.Close() }()

	if c.mode == "read" {
		if err := s.LoadCache(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "LoadCache: %v\n", err)
			os.Exit(1)
		}
	}

	var ops atomic.Int64
	lat := newLatencyCollector()
	pr := newProgress(&ops, c.count, c.reportEvery)
	pr.start()

	start := time.Now()
	fin := make(chan error, c.threads)
	for i := 0; i < c.threads; i++ {
		s0, e0 := partitionRange(c.count, c.threads, i)
		go func(s0, e0 int) {
			fin <- runWorker(ctx, s, c, s0, e0, &ops, lat)
		}(s0, e0)
	}
	var runErr error
	for i := 0; i < c.threads; i++ {
		if err := <-fin; err != nil && runErr == nil {
			runErr = err
		}
	}
	elapsed := time.Since(start)
	pr.stop()

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "run error: %v\n", runErr)
		os.Exit(1)
	}

	stopCPUProfile(cpuFile)
	writeMemProfile(c)

	report(c, ops.Load(), lat, elapsed)
}

// stopCPUProfile 结束 CPU profile（若开启了）。
func stopCPUProfile(cpuFile *os.File) {
	if cpuFile == nil {
		return
	}
	pprof.StopCPUProfile()
	_ = cpuFile.Close()
}

// writeMemProfile 若指定了 -memprofile，写入内存 profile。
func writeMemProfile(c *config) {
	if c.memProfile == "" {
		return
	}
	f, err := os.Create(c.memProfile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memprofile: %v\n", err)
		return
	}
	defer func() { _ = f.Close() }()
	_ = pprof.WriteHeapProfile(f)
}

func (c *config) validate() error {
	switch c.mode {
	case "write", "read":
	default:
		return fmt.Errorf("invalid -mode %q: must be write or read", c.mode)
	}
	switch {
	case c.size < 0:
		return fmt.Errorf("-size must be >= 0")
	case c.threads <= 0:
		return fmt.Errorf("-threads must be > 0")
	case c.count <= 0:
		return fmt.Errorf("-count must be > 0")
	case c.dbDir == "" || c.devPath == "":
		return fmt.Errorf("-db and -dev are required")
	}
	return nil
}

// partitionRange 把 [0,count) 均分到 threads 段，返回第 i 段的 [start,end)。
// 段互不相交 → 每个 key 恰好被一个 worker 处理一次（读不重复的关键保证）。
func partitionRange(count, threads, i int) (int, int) {
	base := count / threads
	rem := count % threads
	start := i*base + min(i, rem)
	end := start + base
	if i < rem {
		end++
	}
	return start, end
}

// keyFor 生成 <prefix>/<seq>，写与读共用同一生成规则，保证 key 可对齐。
func (c *config) keyFor(seq int) string {
	return fmt.Sprintf("%s/%d", c.prefix, seq)
}

// runWorker 处理区间 [s0,e0)：write 逐个 Put，read 逐个 Get 整对象。
func runWorker(ctx context.Context, s *taihu.Storage, c *config, s0, e0 int, ops *atomic.Int64, lat *latencyCollector) error {
	payload := make([]byte, int(c.size))
	for k := s0; k < e0; k++ {
		key := c.keyFor(k)
		t0 := time.Now()
		var err error
		switch c.mode {
		case "write":
			err = s.Put(ctx, key, c.size, payload)
		case "read":
			var got []byte
			got, err = s.ReadAt(ctx, key, 0, c.size)
			if err == nil && int64(len(got)) != c.size {
				err = fmt.Errorf("key %s: short read %d != %d", key, len(got), c.size)
			}
			if got != nil {
				bufpool.Put(got) // ReadAt 返回池化缓冲，须归还
			}
		}
		if c.latency {
			lat.add(time.Since(t0))
		}
		if err != nil {
			return err
		}
		ops.Add(1)
	}
	return nil
}

// progress 周期性打印已完成 op 数。
type progress struct {
	ops    *atomic.Int64
	total  int
	every  time.Duration
	stopCh chan struct{}
	doneCh chan struct{}
}

func newProgress(ops *atomic.Int64, total int, every time.Duration) *progress {
	return &progress{ops: ops, total: total, every: every}
}

func (p *progress) start() {
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	go func() {
		defer close(p.doneCh)
		last := time.Now()
		var lastN int64
		t := time.NewTicker(p.every)
		defer t.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case now := <-t.C:
				n := p.ops.Load()
				rate := float64(n-lastN) / now.Sub(last).Seconds()
				fmt.Printf("  %s: %d/%d (%.1f ops/s)\n", now.Format("15:04:05"), n, p.total, rate)
				last, lastN = now, n
			}
		}
	}()
}

func (p *progress) stop() {
	close(p.stopCh)
	<-p.doneCh
}

// latencyCollector 记录每次读耗时，用于分位统计。
type latencyCollector struct {
	mu   sync.Mutex
	vals []time.Duration
}

func newLatencyCollector() *latencyCollector { return &latencyCollector{} }

func (l *latencyCollector) add(d time.Duration) {
	l.mu.Lock()
	l.vals = append(l.vals, d)
	l.mu.Unlock()
}

func report(c *config, done int64, lat *latencyCollector, elapsed time.Duration) {
	totalBytes := done * c.size
	opsPerSec := float64(done) / elapsed.Seconds()
	bw := float64(totalBytes) / elapsed.Seconds() / (1024 * 1024)

	fmt.Printf("\n==== taihu-bench %s ====\n", c.mode)
	fmt.Printf("size=%d threads=%d count=%d\n", c.size, c.threads, c.count)
	fmt.Printf("objects=%d bytes=%d elapsed=%s\n", done, totalBytes, elapsed.Round(time.Millisecond))
	fmt.Printf("throughput: %8.2f ops/s  %8.2f MiB/s\n", opsPerSec, bw)
	if c.latency {
		lat.mu.Lock()
		vals := make([]time.Duration, len(lat.vals))
		copy(vals, lat.vals)
		lat.mu.Unlock()
		sort.Slice(vals, func(a, b int) bool { return vals[a] < vals[b] })
		if len(vals) > 0 {
			p := func(q float64) time.Duration { return vals[int(q*float64(len(vals)-1))] }
			fmt.Printf("latency: p50=%.2fms p90=%.2fms p99=%.2fms\n",
				p(0.50).Seconds()*1e3, p(0.90).Seconds()*1e3, p(0.99).Seconds()*1e3)
		}
	}
}
