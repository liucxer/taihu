// Package benchkit 提供 taihu 压测工具（taihu bench cluster / taihu bench single）的
// 公共压测骨架：数据面接口、公共参数、区间切分、执行循环与结果汇总。
// 各 bench 工具只负责构建数据面（集群 rpccluster.Storage 或直连 rpcclient.Storage），
// 压测语义统一：key 集合 <prefix>/<seq>，读模式区间切分保证每 key 全进程只读一次。
package benchkit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Config 压测公共配置（各 bench 工具特有的 dial/集群参数由各自 flag 定义）。
type Config struct {
	// Mode 测试模式：write | read | delete。
	Mode string
	// Size 对象大小（字节）。
	Size int64
	// Threads 并发 goroutine 数。
	Threads int
	// Count 对象总数（key 为 <prefix>/<seq>）。
	Count int
	// Prefix key 前缀。
	Prefix string
	// ReportEvery 进度打印间隔。
	ReportEvery time.Duration
	// Latency 记录每 op 延迟并输出 p50/p90/p99。
	Latency bool
	// Pipeline 每个 worker 保持的在途 op 数（read/write 用 bounded-pump 并发下发，
	// 让单线程也能同时持有多个在途请求，摊薄同步往返的 per-op 等待；1 表示串行逐 op）。
	Pipeline int
}

// Store 压测数据面抽象：集群 rpccluster.Storage 与直连 rpcclient.Storage
// 实现同一套 Put/Get/Delete 签名，压测逻辑不关心底层是 shm 还是 TCP。
type Store interface {
	Put(ctx context.Context, key string, size int64, in []byte) error
	Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error)
	Delete(ctx context.Context, key string) error
	Close() error
}

// Validate 校验公共参数。
func (c Config) Validate() error {
	switch c.Mode {
	case "write", "read", "delete":
	default:
		return fmt.Errorf("invalid -mode %q: must be write, read or delete", c.Mode)
	}
	switch {
	case c.Size < 0:
		return fmt.Errorf("-size must be >= 0")
	case c.Threads <= 0:
		return fmt.Errorf("-threads must be > 0")
	case c.Count <= 0:
		return fmt.Errorf("-count must be > 0")
	case c.Pipeline < 1:
		return fmt.Errorf("-pipeline must be >= 1")
	}
	return nil
}

// KeyFor 返回第 seq 个对象的 key。
func KeyFor(prefix string, seq int) string {
	return fmt.Sprintf("%s/%d", prefix, seq)
}

// PartitionRange 把 [0,count) 均分到 threads 段，每 key 恰好被一个 worker 处理一次。
func PartitionRange(count, threads, i int) (int, int) {
	base := count / threads
	rem := count % threads
	start := i*base + min(i, rem)
	end := start + base
	if i < rem {
		end++
	}
	return start, end
}

// Run 执行压测循环并输出汇总报告。toolName 用于报告标题，endpoint 用于标注
// 被测端点（由调用方按模式构造）。返回 nil 表示无错误。
func Run(ctx context.Context, s Store, cfg Config, toolName, endpoint string) error {
	var ops atomic.Int64
	lat := newLatencyCollector()
	pr := newProgress(&ops, cfg.Count, cfg.ReportEvery)
	pr.start()

	start := time.Now()
	fin := make(chan error, cfg.Threads)
	for i := 0; i < cfg.Threads; i++ {
		s0, e0 := PartitionRange(cfg.Count, cfg.Threads, i)
		go func(s0, e0 int) {
			fin <- runWorker(ctx, s, cfg, s0, e0, &ops, lat)
		}(s0, e0)
	}
	var runErr error
	for i := 0; i < cfg.Threads; i++ {
		if err := <-fin; err != nil && runErr == nil {
			runErr = err
		}
	}
	elapsed := time.Since(start)
	pr.stop()

	if runErr != nil {
		return runErr
	}
	report(cfg, ops.Load(), lat, elapsed, toolName, endpoint)
	return nil
}

// runWorker write 逐个 Put，read 逐个 Get 整对象。Pipeline>1 时改用
// runPipelinedWorker 让单 worker 同时保持多个在途 op。
func runWorker(ctx context.Context, s Store, cfg Config, s0, e0 int, ops *atomic.Int64, lat *latencyCollector) error {
	if cfg.Pipeline > 1 {
		return runPipelinedWorker(ctx, s, cfg, s0, e0, ops, lat)
	}
	payload := make([]byte, int(cfg.Size))
	for j := range payload {
		payload[j] = byte(j & 0xff)
	}
	for k := s0; k < e0; k++ {
		key := KeyFor(cfg.Prefix, k)
		t0 := time.Now()
		var err error
		switch cfg.Mode {
		case "write":
			err = s.Put(ctx, key, cfg.Size, payload)
		case "read":
			var got []byte
			var rel func()
			got, rel, err = s.Get(ctx, key, 0, cfg.Size)
			if err == nil && int64(len(got)) != cfg.Size {
				err = fmt.Errorf("key %s: short read %d != %d", key, len(got), cfg.Size)
			}
			if got != nil {
				rel() // Get 返回私有缓冲，校验后即归还
			}
		case "delete":
			err = s.Delete(ctx, key)
		}
		if cfg.Latency {
			lat.add(time.Since(t0))
		}
		if err != nil {
			return err
		}
		ops.Add(1)
	}
	return nil
}

// runPipelinedWorker 每个 worker 以 bounded-pump 方式并发下发一整个区间的 op：固定
// Pipeline 个 goroutine 轮流原子摘取下一个 key，使单 worker 同时保持 Pipeline 个在途
// op。对单个 4MiB 对象（每请求只有 1 次磁盘 DMA），这是把"每线程在途=1"提升为
// "每线程在途=Pipeline"，从而加深磁盘队列、摊薄同步往返等待。读模式用；语义与
// Run/runWorker 完全一致（区间切分、短读校验、Get 缓冲即取即还、延迟与计数）。
func runPipelinedWorker(ctx context.Context, s Store, cfg Config, s0, e0 int, ops *atomic.Int64, lat *latencyCollector) error {
	payload := make([]byte, int(cfg.Size))
	for j := range payload {
		payload[j] = byte(j & 0xff)
	}
	var idx int64 = int64(s0 - 1)
	var firstErr error
	var errMu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < cfg.Pipeline; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&idx, 1))
				if i >= e0 {
					return
				}
				key := KeyFor(cfg.Prefix, i)
				t0 := time.Now()
				var got []byte
				var rel func()
				var err error
				switch cfg.Mode {
				case "write":
					err = s.Put(ctx, key, cfg.Size, payload)
				case "read":
					got, rel, err = s.Get(ctx, key, 0, cfg.Size)
					if err == nil && int64(len(got)) != cfg.Size {
						err = fmt.Errorf("key %s: short read %d != %d", key, len(got), cfg.Size)
					}
					if got != nil {
						rel()
					}
				case "delete":
					err = s.Delete(ctx, key)
				}
				if cfg.Latency {
					lat.add(time.Since(t0))
				}
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					return
				}
				ops.Add(1)
			}
		}()
	}
	wg.Wait()
	return firstErr
}
