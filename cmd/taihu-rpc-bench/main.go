// Command taihu-rpc-bench 是 rpccluster 跨节点端到端压测工具（设计文档_v3 §8）。
// 与本地 taihu-bench 同语义：key 集合 <prefix>/<seq>，读模式区间切分保证每 key 全进程只读一次。
// 集群模式（-client-name + -tikv-pd）：SDK 按"客户端/服务端 hostname 是否一致"自动选路
// ——同机走共享内存（shmipc）、跨节点走 TCP；无需也不接受 -addr/-shm。
// read 前须先用相同前缀 write 灌好数据。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/transport"
	"github.com/liucxer/taihu/pkg/rpccluster"
)

// dataStore 压测数据面抽象：集群 rpccluster.Storage 实现同一套 Put/Get/Delete 签名，
// 压测逻辑不关心底层是 shm 还是 TCP。数据面传输由 SDK 按 hostname 自动判定。
type dataStore interface {
	Put(ctx context.Context, key string, size int64, in []byte) error
	Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error)
	Delete(ctx context.Context, key string) error
	Close() error
}

type config struct {
	clientName  string
	tikvPD      string
	mode        string
	size        int64
	threads     int
	conns       int
	count       int
	prefix      string
	reportEvery time.Duration
	latency     bool
	cpuProfile  string
	preload     bool
}

func parseFlags() *config {
	c := &config{}
	flag.StringVar(&c.clientName, "client-name", "", "this client name (required; client identifier, a.k.a. -node)")
	flag.StringVar(&c.tikvPD, "tikv-pd", "", "comma-separated TiKV PD addresses (required)")
	flag.StringVar(&c.mode, "mode", "", "write | read | delete")
	flag.Int64Var(&c.size, "size", 4096, "object size in bytes")
	flag.IntVar(&c.threads, "threads", 1, "number of concurrent goroutines")
	flag.IntVar(&c.conns, "conns", 1, "number of client connections (shmipc SessionNum for local / TCP DialPool for remote)")
	flag.IntVar(&c.count, "count", 1000, "total number of distinct objects")
	flag.StringVar(&c.prefix, "keys-prefix", "rbench", "key prefix, keys are <prefix>/<seq>")
	flag.DurationVar(&c.reportEvery, "report-interval", 2*time.Second, "progress report interval")
	flag.BoolVar(&c.latency, "latency", false, "record per-op latency")
	flag.StringVar(&c.cpuProfile, "cpuprofile", "", "write cpu profile to this file (pprof)")
	flag.BoolVar(&c.preload, "preload", false, "read: preload RouteCache before timing (cluster mode)")
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

	var s dataStore
	var err error
	// 集群模式：走 rpccluster（SDK 按客户端/服务端 hostname 一致 → shm，否则 TCP）。
	kv, kerr := cluster.NewTiKVKV(ctx, strings.Split(c.tikvPD, ","))
	if kerr != nil {
		stopCPUProfile(cpuFile)
		fmt.Fprintf(os.Stderr, "tikv %s: %v\n", c.tikvPD, kerr)
		os.Exit(1)
	}
	defer kv.Close()
	s, err = rpccluster.NewCluster(rpccluster.ClusterConfig{
		KV:         kv,
		ClientName: c.clientName,
		// 每实例连接数：同机实例 shm 会话数、跨节点 TCP 连接数（-conns）。
		Conns: c.conns,
		// 压测场景无真实远端源：miss 即记为未命中（回源兜底语义不参与压测带宽）。
		Source: func(ctx context.Context, key string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
	})
	if err != nil {
		stopCPUProfile(cpuFile)
		fmt.Fprintf(os.Stderr, "cluster %s: %v\n", c.clientName, err)
		os.Exit(1)
	}
	defer s.Close()

	// 预热路由缓存（集群读模式）：逐 key 查索引填充 RouteCache，消除冷启动的
	// "每 key 查 TiKV"开销，只验证热缓存下的数据面带宽。
	if c.preload && c.mode == "read" {
		if cs, ok := s.(*rpccluster.Storage); ok {
			keys := make([]string, 0, c.count)
			for k := 0; k < c.count; k++ {
				keys = append(keys, c.keyFor(k))
			}
			cs.PreloadRoute(ctx, keys)
			fmt.Printf("preloaded %d keys into RouteCache\n", len(keys))
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
	report(c, ops.Load(), lat, elapsed)
	// 客户端收帧尺寸统计（验证每帧是否整块 4MiB、TakeTry 命中率）。
	fmt.Printf("==== frame stats ====\n%s", transport.StatsString())
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
	case "write", "read", "delete":
	default:
		return fmt.Errorf("invalid -mode %q: must be write, read or delete", c.mode)
	}
	// 集群模式：-client-name 与 -tikv-pd 必传；不接受 -addr/-shm（传输由 SDK 按 hostname 自动判定）。
	if c.clientName == "" {
		return fmt.Errorf("-client-name is required (cluster mode)")
	}
	if c.tikvPD == "" {
		return fmt.Errorf("-client-name requires -tikv-pd")
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

// runWorker write 逐个 Put，read 逐个 Get 整对象。
func runWorker(ctx context.Context, s dataStore, c *config, s0, e0 int, ops *atomic.Int64, lat *latencyCollector) error {
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
			err = s.Put(ctx, key, c.size, payload)
		case "read":
			var got []byte
			var rel func()
			got, rel, err = s.Get(ctx, key, 0, c.size)
			if err == nil && int64(len(got)) != c.size {
				err = fmt.Errorf("key %s: short read %d != %d", key, len(got), c.size)
			}
			if got != nil {
				rel() // Get 返回私有缓冲，校验后即归还
			}
		case "delete":
			err = s.Delete(ctx, key)
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

	fmt.Printf("\n==== taihu-rpc-bench %s ====\n", c.mode)
	fmt.Printf("endpoint=cluster(client=%s) size=%d threads=%d count=%d\n", c.clientName, c.size, c.threads, c.count)
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
