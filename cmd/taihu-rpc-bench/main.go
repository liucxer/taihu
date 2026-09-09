// Command taihu-rpc-bench 是 rpcclient 跨节点端到端压测工具（设计文档_v3 §8）。
// 与本地 taihu-bench 同语义：key 集合 <prefix>/<seq>，读模式区间切分保证每 key 全进程只读一次。
// 区别：无 -db/-dev，改为 -addr 指向 taihu-server；read 前须先用相同前缀 write 灌好数据。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/pprof"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/liucxer/taihu/pkg/rpcclient"
)

type config struct {
	addr        string
	mode        string
	size        int64
	threads     int
	conns       int
	count       int
	prefix      string
	reportEvery time.Duration
	latency     bool
	cpuProfile  string
}

func parseFlags() *config {
	c := &config{}
	flag.StringVar(&c.addr, "addr", "", "taihu-server address (required)")
	flag.StringVar(&c.mode, "mode", "", "write | read")
	flag.Int64Var(&c.size, "size", 4096, "object size in bytes")
	flag.IntVar(&c.threads, "threads", 1, "number of concurrent goroutines")
	flag.IntVar(&c.conns, "conns", 1, "number of client gRPC connections (DialPool)")
	flag.IntVar(&c.count, "count", 1000, "total number of distinct objects")
	flag.StringVar(&c.prefix, "keys-prefix", "rbench", "key prefix, keys are <prefix>/<seq>")
	flag.DurationVar(&c.reportEvery, "report-interval", 2*time.Second, "progress report interval")
	flag.BoolVar(&c.latency, "latency", false, "record per-op latency")
	flag.StringVar(&c.cpuProfile, "cpuprofile", "", "write cpu profile to this file (pprof)")
	flag.Parse()
	return c
}

func main() {
	c := parseFlags()
	if err := c.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "taihu-rpc-bench:", err)
		os.Exit(2)
	}

	ctx := context.Background()

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

	s, err := rpcclient.DialPool(ctx, c.addr, c.conns)
	if err != nil {
		stopCPUProfile(cpuFile)
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", c.addr, err)
		os.Exit(1)
	}
	defer s.Close()

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
	report(c, ops.Load(), lat, elapsed)
}

func stopCPUProfile(cpuFile *os.File) {
	if cpuFile == nil {
		return
	}
	pprof.StopCPUProfile()
	_ = cpuFile.Close()
}

func (c *config) validate() error {
	switch c.mode {
	case "write", "read", "rawread":
	default:
		return fmt.Errorf("invalid -mode %q: must be write or read", c.mode)
	}
	if c.addr == "" {
		return fmt.Errorf("-addr is required")
	}
	switch {
	case c.size < 0:
		return fmt.Errorf("-size must be >= 0")
	case c.threads <= 0:
		return fmt.Errorf("-threads must be > 0")
	case c.conns <= 0:
		return fmt.Errorf("-conns must be > 0")
	case c.count <= 0:
		return fmt.Errorf("-count must be > 0")
	}
	return nil
}

// partitionRange 把 [0,count) 均分到 threads 段，每 key 恰好被一个 worker 处理一次。
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

func (c *config) keyFor(seq int) string {
	return fmt.Sprintf("%s/%d", c.prefix, seq)
}

// runWorker write 逐个 Put，read 逐个 Get 整对象并拉满流。
func runWorker(ctx context.Context, s *rpcclient.Storage, c *config, s0, e0 int, ops *atomic.Int64, lat *latencyCollector) error {
	payload := make([]byte, int(c.size))
	for j := range payload {
		payload[j] = byte(j & 0xff)
	}
	for k := s0; k < e0; k++ {
		key := c.keyFor(k)
		t0 := time.Now()
		var err error
		switch c.mode {
		case "write":
			err = s.Put(ctx, key, c.size, &sliceReader{b: payload})
		case "read":
			var n int64
			rc, gerr := s.Get(ctx, key, 0, c.size)
			if gerr == nil {
				n, err = io.Copy(io.Discard, rc)
				_ = rc.Close()
			} else {
				err = gerr
			}
			if err == nil && n != c.size {
				err = fmt.Errorf("key %s: short read %d != %d", key, n, c.size)
			}
		case "rawread":
			var n int64
			rs, gerr := s.GetRaw(ctx, key, 0, c.size)
			if gerr == nil {
				for {
					b, rerr := rs.Next()
					if rerr == io.EOF {
						break
					}
					if rerr != nil {
						err = rerr
						break
					}
					n += int64(len(b))
				}
				_ = rs.Close()
			} else {
				err = gerr
			}
			if err == nil && n != c.size {
				err = fmt.Errorf("key %s: short read %d != %d", key, n, c.size)
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

// sliceReader 复用同一底层缓冲，避免每次循环重新分配 payload。
type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
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

	fmt.Printf("\n==== taihu-rpc-bench %s ====\n", c.mode)
	fmt.Printf("addr=%s size=%d threads=%d count=%d\n", c.addr, c.size, c.threads, c.count)
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
