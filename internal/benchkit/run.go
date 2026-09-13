// Package benchkit 提供 taihu 压测工具（taihu bench cluster / taihu bench single）的
// 公共压测骨架：数据面接口、公共参数、区间切分、执行循环与结果汇总。
// 各 bench 工具只负责构建数据面（集群 rpccluster.Storage 或直连 rpcclient.Storage），
// 压测语义统一：key 集合 <prefix>/<seq>，读模式区间切分保证每 key 全进程只读一次。
package benchkit

import (
	"context"
	"fmt"
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

// runWorker write 逐个 Put，read 逐个 Get 整对象。
func runWorker(ctx context.Context, s Store, cfg Config, s0, e0 int, ops *atomic.Int64, lat *latencyCollector) error {
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
