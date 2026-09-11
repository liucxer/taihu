// Command taihu-shm-bench 是共享内存（shmipc）直连的单客户端单服务端读写压测工具。
// 与 taihu-rpc-bench 同语义：key 集合 <prefix>/<seq>，read 前须先用相同前缀 write 灌好数据；
// 差异：仅支持 -shm（同机 unix socket）直连单个 taihu-server，无集群路由/TiKV 依赖，
// 专用于标定单实例共享内存路径的读写带宽与延迟。
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

	"github.com/liucxer/taihu/internal/transport"
	"github.com/liucxer/taihu/pkg/rpcclient"
)

// dataStore 压测数据面抽象：rpcclient.Storage（shmipc 连接池）实现。
type dataStore interface {
	Put(ctx context.Context, key string, size int64, in []byte) error
	Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error)
	Delete(ctx context.Context, key string) error
	Close() error
}

// writeChunk 零拷贝写单次 Reserve 上限（与服务端 chunkSize 一致，4MiB）。
const writeChunk = 1 << 22

type config struct {
	shm         string
	mode        string
	size        int64
	threads     int
	sessions    int
	count       int
	prefix      string
	reportEvery time.Duration
	duration    time.Duration
	latency     bool
	verify      bool
	zeroCopy    bool
	cpuProfile  string
}

func parseFlags() *config {
	c := &config{}
	flag.StringVar(&c.shm, "shm", "", "taihu-server unix socket path (shmipc shared-memory IPC, required)")
	flag.StringVar(&c.mode, "mode", "", "write | read | delete")
	flag.Int64Var(&c.size, "size", 4096, "object size in bytes")
	flag.IntVar(&c.threads, "threads", 1, "number of concurrent goroutines")
	flag.IntVar(&c.sessions, "sessions", 1, "number of shmipc sessions (shared-memory buffers)")
	flag.IntVar(&c.count, "count", 1000, "total number of distinct objects (keys are <prefix>/<seq%%count> in -duration mode)")
	flag.StringVar(&c.prefix, "keys-prefix", "shmbench", "key prefix, keys are <prefix>/<seq>")
	flag.DurationVar(&c.reportEvery, "report-interval", 2*time.Second, "progress report interval")
	flag.DurationVar(&c.duration, "duration", 0, "run for this duration looping keys mod count (e.g. 60s); 0 = run each key once")
	flag.BoolVar(&c.latency, "latency", false, "record per-op latency")
	flag.BoolVar(&c.verify, "verify-content", false, "read: verify payload pattern byte(i&0xff) (sampled every 64KiB + head/tail)")
	flag.BoolVar(&c.zeroCopy, "zero-copy-write", true, "write: use shm zero-copy Reserve (direct shared-memory write, no memcpy)")
	flag.StringVar(&c.cpuProfile, "cpuprofile", "", "write cpu profile to this file (pprof)")
	flag.Parse()
	return c
}

func main() {
	c := parseFlags()
	if err := c.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "taihu-shm-bench:", err)
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

	// 共享内存直连：sessions 条 shmipc 会话（≈连接数，SessionManager round-robin）。
	s, err := rpcclient.DialShmPool(ctx, c.shm, c.sessions)
	if err != nil {
		stopCPUProfile(cpuFile)
		fmt.Fprintf(os.Stderr, "dial shm %s: %v\n", c.shm, err)
		os.Exit(1)
	}
	defer s.Close()

	var ops atomic.Int64
	lat := newLatencyCollector()
	total := c.count
	if c.duration > 0 {
		total = -1 // duration 模式无固定总量
	}
	pr := newProgress(&ops, total, c.reportEvery)
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
	// 收帧尺寸统计（验证每帧整块 4MiB、TakeTry 零拷贝命中率）。
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
	if c.shm == "" {
		return fmt.Errorf("-shm is required (taihu-server unix socket path)")
	}
	switch {
	case c.size < 0:
		return fmt.Errorf("-size must be >= 0")
	case c.threads <= 0:
		return fmt.Errorf("-threads must be > 0")
	case c.sessions <= 0:
		return fmt.Errorf("-sessions must be > 0")
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

// putOne 写一个对象：零拷贝（NewPut/Reserve/Commit，payload 直接生成在共享内存，
// 免 memcpy）或普通 Put。零拷贝路径一旦开始（header 已发）失败直接返回，不回退
// （避免残留流）；NewPut 本身失败（非 shm 连接等）回退普通 Put。
func putOne(ctx context.Context, s dataStore, c *config, key string, payload []byte) error {
	if c.zeroCopy {
		if pw, ok := s.(*rpcclient.Storage); ok {
			if w, err := pw.NewPut(ctx, key, c.size); err == nil {
				for off := int64(0); off < c.size; {
					n := int64(min(int64(writeChunk), c.size-off))
					buf, rerr := w.Reserve(int(n))
					if rerr != nil {
						return rerr
					}
					for j := range buf {
						buf[j] = byte((off + int64(j)) & 0xff)
					}
					off += n
				}
				return w.Commit()
			}
			// NewPut 失败（非 shm / 连接异常）→ 回退普通 Put。
		}
	}
	return s.Put(ctx, key, c.size, payload)
}

// runWorker write 逐个 Put，read 逐个 Get 整对象。
// -duration 模式：循环执行直到超时（key = <prefix>/<seq%count>，每线程从 s0 步进 threads，
// count 为 threads 倍数时各线程覆盖互不重叠的 key 子集）；非 duration 模式每个 key 恰好执行一次。
func runWorker(ctx context.Context, s dataStore, c *config, s0, e0 int, ops *atomic.Int64, lat *latencyCollector) error {
	payload := make([]byte, int(c.size))
	for j := range payload {
		payload[j] = byte(j & 0xff)
	}
	start := time.Now()
	k := s0
	for {
		if c.duration > 0 {
			if time.Since(start) >= c.duration {
				return nil
			}
		} else if k >= e0 {
			return nil
		}
		key := c.keyFor(k % c.count)
		t0 := time.Now()
		var err error
		switch c.mode {
		case "write":
			err = putOne(ctx, s, c, key, payload)
		case "read":
			var got []byte
			var rel func()
			got, rel, err = s.Get(ctx, key, 0, c.size)
			if err == nil && int64(len(got)) != c.size {
				err = fmt.Errorf("key %s: short read %d != %d", key, len(got), c.size)
			}
			if err == nil && c.verify && len(got) > 0 {
				// 抽样校验写 payload 模式 byte(i&0xff)：每 64KiB 一个样本 + 首/尾字节。
				last := len(got) - 1
				for j := 0; j < len(got); j += 64 * 1024 {
					if got[j] != byte(j&0xff) {
						err = fmt.Errorf("key %s: content mismatch at %d", key, j)
						break
					}
				}
				if err == nil && (got[0] != 0 || got[last] != byte(last&0xff)) {
					err = fmt.Errorf("key %s: content mismatch at head/tail", key)
				}
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
		if c.duration > 0 {
			k += c.threads // 循环模式按线程步进，避免并发线程争同一 key
		} else {
			k++
		}
	}
}

// progress 周期性打印已完成 op 数。
type progress struct {
	ops       *atomic.Int64
	total     int
	every     time.Duration
	startTime time.Time
	stopCh    chan struct{}
	doneCh    chan struct{}
}

func newProgress(ops *atomic.Int64, total int, every time.Duration) *progress {
	return &progress{ops: ops, total: total, every: every}
}

func (p *progress) start() {
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	p.startTime = time.Now()
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
				if p.total < 0 {
					// duration 模式：显示已运行时间与实时速率。
					fmt.Printf("  %s: running %.0fs (%.1f ops/s)\n", now.Format("15:04:05"), now.Sub(p.startTime).Seconds(), rate)
				} else {
					fmt.Printf("  %s: %d/%d (%.1f ops/s)\n", now.Format("15:04:05"), n, p.total, rate)
				}
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

	fmt.Printf("\n==== taihu-shm-bench %s ====\n", c.mode)
	fmt.Printf("endpoint=shm:%s size=%d threads=%d sessions=%d count=%d\n", c.shm, c.size, c.threads, c.sessions, c.count)
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
