// Sub-command taihu bench storage 是 Storage 层的性能测试工具，按设计文档 §9 实现。
//
// 支持两种模式：
//   - write：并发写 count 个不同 key 的 object（每个 size 字节）。
//   - read：并发读 count 个不同 key 的 object（整对象读），每个 key 全进程只读一次。
//
// 读模式创建 Storage 后先 LoadCache 预热元数据，剔除 pebble 读对带宽的影响。
package cmd

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime/pprof"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/pkg/bufpool"
)

type storageBenchConfig struct {
	mode        string
	size        int64
	threads     int
	count       int
	prefix      string
	dbDir       string
	devPath     string
	reportEvery time.Duration
	latency     bool
	randOrder   bool
	cpuProfile  string
	memProfile  string
}

// deviceCapacity 是 device.DeviceCapacity 的测试缝隙（与 helpers.go 的 kvConnect 同款）：
// 生产路径恒为真实 BLKGETSIZE64 裸盘容量查询，行为不变；单测环境无裸盘，
// 替换为按普通文件大小近似，使命令级测试能端到端跑通 RunE 的全部流程。
var deviceCapacity = device.DeviceCapacity

// benchStorageCmd 本地裸盘 Storage 层压测。
var benchStorageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Storage 层本地裸盘读写压测（-db/-dev 直连盘，不走网络）",
	Long: `taihu bench storage：本地裸盘 Storage 层压测（设计文档 §9）。

必传参数：
  --mode write|read               测试模式
  --db <dir>                      pebble 元数据目录
  --dev <path>                    裸设备路径

说明：
  读模式创建 Storage 后先 LoadCache 预热元数据，剔除 pebble 读对带宽的影响；
  key 为 <prefix>/<seq>，read 前须先用相同前缀 write 灌好数据。
  长选项必须用双横线（--mode），单横线会被 pflag 当作 shorthand 解析。
  裸设备须独占：不能与正在运行的 taihu server 共用同一分区。`,
	Example: `  # 4M 对象并发写（裸盘直写）
  taihu bench storage --mode write --db /mnt/db --dev /dev/nvme0n1 --size 4194304 --count 40000 --threads 32 --latency

  # 4M 对象并发读（先 LoadCache 预热）
  taihu bench storage --mode read --db /mnt/db --dev /dev/nvme0n1 --size 4194304 --count 40000 --threads 32 --latency --cpuprofile /tmp/storage.cpu`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c := &storageBenchConfig{}
		c.mode, _ = cmd.Flags().GetString("mode")
		c.size, _ = cmd.Flags().GetInt64("size")
		c.threads, _ = cmd.Flags().GetInt("threads")
		c.count, _ = cmd.Flags().GetInt("count")
		c.prefix, _ = cmd.Flags().GetString("keys-prefix")
		c.dbDir, _ = cmd.Flags().GetString("db")
		c.devPath, _ = cmd.Flags().GetString("dev")
		c.reportEvery, _ = cmd.Flags().GetDuration("report-interval")
		c.latency, _ = cmd.Flags().GetBool("latency")
		c.randOrder, _ = cmd.Flags().GetBool("rand")
		c.cpuProfile, _ = cmd.Flags().GetString("cpuprofile")
		c.memProfile, _ = cmd.Flags().GetString("memprofile")

		if err := c.validate(); err != nil {
			return err
		}

		ctx := context.Background()

		// pprof cpu profile
		var cpuFile *os.File
		if c.cpuProfile != "" {
			var err error
			cpuFile, err = os.Create(c.cpuProfile)
			if err != nil {
				return fmt.Errorf("cpuprofile: %w", err)
			}
			if err := pprof.StartCPUProfile(cpuFile); err != nil {
				return fmt.Errorf("StartCPUProfile: %w", err)
			}
		}

		// 读取设备真实容量并计算布局（段数不再硬编码 2048）。
		capacity, err := deviceCapacity(c.devPath)
		if err != nil {
			stopCPUProfile(cpuFile)
			return fmt.Errorf("DeviceCapacity: %w", err)
		}
		l := layout.ComputeLayout(capacity, layout.DefaultSegmentSizeBytes)
		aioOpts, err := aioOptions()
		if err != nil {
			stopCPUProfile(cpuFile)
			return err
		}
		s, err := storage.NewStorage(ctx, c.dbDir, c.devPath, l, aioOpts...)
		if err != nil {
			stopCPUProfile(cpuFile)
			return fmt.Errorf("NewStorage: %w", err)
		}
		defer func() { _ = s.Close() }()

		if c.mode == "read" {
			if err := s.LoadCache(ctx); err != nil {
				stopCPUProfile(cpuFile)
				return fmt.Errorf("LoadCache: %w", err)
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
			stopCPUProfile(cpuFile)
			return fmt.Errorf("run error: %w", runErr)
		}

		stopCPUProfile(cpuFile)
		writeMemProfile(c)

		report(c, ops.Load(), lat, elapsed)
		return nil
	},
}

func init() {
	f := benchStorageCmd.Flags()
	f.String("mode", "", "write | read")
	f.Int64("size", 4096, "object size in bytes")
	f.Int("threads", 1, "number of concurrent goroutines")
	f.Int("count", 1000, "total number of distinct objects")
	f.String("keys-prefix", "bench", "key prefix, keys are <prefix>/<seq>")
	f.String("db", "", "pebble metadata directory")
	f.String("dev", "", "raw device path")
	f.Duration("report-interval", 2*time.Second, "progress report interval")
	f.Bool("latency", false, "record per-op latency (write and read)")
	f.Bool("rand", false, "read in random order (each key still read exactly once)")
	f.String("cpuprofile", "", "write cpu profile to this file (pprof)")
	f.String("memprofile", "", "write memory profile to this file (pprof)")
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
func writeMemProfile(c *storageBenchConfig) {
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

func (c *storageBenchConfig) validate() error {
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
func (c *storageBenchConfig) keyFor(seq int) string {
	return fmt.Sprintf("%s/%d", c.prefix, seq)
}

// runWorker 处理区间 [s0,e0)：write 逐个 Put，read 逐个 Get 整对象。
// randOrder 时 read 在区间内按随机顺序访问 key（每个 key 仍恰好被访问一次）。
func runWorker(ctx context.Context, s *storage.Storage, c *storageBenchConfig, s0, e0 int, ops *atomic.Int64, lat *latencyCollector) error {
	payload := make([]byte, int(c.size))

	// 随机读：先构造区间内 key 序列并打乱（seq 即 key 序号）。
	seqs := make([]int, e0-s0)
	for i := range seqs {
		seqs[i] = s0 + i
	}
	if c.mode == "read" && c.randOrder {
		rand.Shuffle(len(seqs), func(i, j int) { seqs[i], seqs[j] = seqs[j], seqs[i] })
	}

	for _, k := range seqs {
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

func report(c *storageBenchConfig, done int64, lat *latencyCollector, elapsed time.Duration) {
	totalBytes := done * c.size
	opsPerSec := float64(done) / elapsed.Seconds()
	bw := float64(totalBytes) / elapsed.Seconds() / (1024 * 1024)

	fmt.Printf("\n==== taihu bench storage %s ====\n", c.mode)
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
