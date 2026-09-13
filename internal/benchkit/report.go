package benchkit

import (
	"fmt"
	"os"
	"runtime/pprof"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

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

// report 输出压测汇总：端点、对象数、吞吐与延迟分位。
func report(cfg Config, done int64, lat *latencyCollector, elapsed time.Duration, toolName, endpoint string) {
	totalBytes := done * cfg.Size
	opsPerSec := float64(done) / elapsed.Seconds()
	bw := float64(totalBytes) / elapsed.Seconds() / (1024 * 1024)

	fmt.Printf("\n==== %s %s ====\n", toolName, cfg.Mode)
	fmt.Printf("endpoint=%s size=%d threads=%d count=%d\n", endpoint, cfg.Size, cfg.Threads, cfg.Count)
	fmt.Printf("objects=%d bytes=%d elapsed=%s\n", done, totalBytes, elapsed.Round(time.Millisecond))
	fmt.Printf("throughput: %8.2f ops/s  %8.2f MiB/s\n", opsPerSec, bw)
	if cfg.Latency {
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

// StartCPUProfile 创建 profile 文件并启动 CPU 采样；path 为空返回 (nil, nil)。
// 用毕必须 StopCPUProfile 收尾。
func StartCPUProfile(path string) (*os.File, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// StopCPUProfile 停止 CPU 采样并关闭文件；f 为 nil 时为空操作。
func StopCPUProfile(f *os.File) {
	if f == nil {
		return
	}
	pprof.StopCPUProfile()
	_ = f.Close()
}
