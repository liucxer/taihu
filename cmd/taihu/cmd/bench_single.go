// Sub-command taihu bench single 是单机（直通）端到端压测工具：不查 TiKV，直接与
// taihu-server 通信。与集群版 taihu bench cluster 同语义：key 集合 <prefix>/<seq>，
// 读模式区间切分保证每 key 全进程只读一次。
// 通信方式由 -transport 决定：rpc 用 -addr（TCP 地址，逗号分隔多地址）、
// shm 用 -shm（unix socket 路径）。read 前须先用相同前缀 write 灌好数据。
package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/benchkit"
	"github.com/liucxer/taihu/internal/transport"
	"github.com/liucxer/taihu/pkg/rpcclient"
)

// benchSingleCmd 单机直通压测（不走 TiKV，直连 taihu-server）。
var benchSingleCmd = &cobra.Command{
	Use:   "single",
	Short: "单机直通压测（不走 TiKV，直连 taihu-server，rpcclient）",
	Long: `taihu bench single：单机直通端到端压测。

必传参数：
  -mode write|read|delete        测试模式
  -transport rpc|shm             通信方式
  -transport rpc 时：-addr         taihu-server TCP 地址（逗号分隔多地址）
  -transport shm 时：-shm          taihu-server unix socket 路径

说明：
  不查 TiKV、无集群路由/索引；read 前须先用相同前缀 write 灌好数据。`,
	Example: `  # 直通 RPC 写
  taihu bench single -mode write -transport rpc -addr 100.71.128.12:50051 -size 4194304 -count 40000 -threads 32 -latency

  # 直通共享内存读
  taihu bench single -mode read -transport shm -shm /dev/TAIHU-0 -count 40000 -threads 32 -latency`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c := &singleBenchConfig{}
		c.transport, _ = cmd.Flags().GetString("transport")
		c.addr, _ = cmd.Flags().GetString("addr")
		c.shm, _ = cmd.Flags().GetString("shm")
		c.conns, _ = cmd.Flags().GetInt("conns")
		c.cpuProfile, _ = cmd.Flags().GetString("cpuprofile")
		c.Mode, _ = cmd.Flags().GetString("mode")
		c.Size, _ = cmd.Flags().GetInt64("size")
		c.Threads, _ = cmd.Flags().GetInt("threads")
		c.Prefix, _ = cmd.Flags().GetString("keys-prefix")
		c.Count, _ = cmd.Flags().GetInt("count")
		c.ReportEvery, _ = cmd.Flags().GetDuration("report-interval")
		c.Latency, _ = cmd.Flags().GetBool("latency")
		c.Pipeline, _ = cmd.Flags().GetInt("pipeline")

		if err := c.validate(); err != nil {
			return err
		}

		ctx := context.Background()

		cpuFile, err := benchkit.StartCPUProfile(c.cpuProfile)
		if err != nil {
			return fmt.Errorf("cpuprofile: %w", err)
		}

		// 直通模式：不查 TiKV，直接与 taihu-server 通信（rpcclient 层，无集群路由/索引）。
		var s benchkit.Store
		switch c.transport {
		case "rpc":
			s, err = rpcclient.DialPoolMulti(ctx, strings.Split(c.addr, ","), c.conns)
		case "shm":
			s, err = rpcclient.DialShmPool(ctx, c.shm, c.conns)
		}
		if err != nil {
			benchkit.StopCPUProfile(cpuFile)
			return fmt.Errorf("dial(%s): %w", c.transport, err)
		}
		defer s.Close()

		var endpoint string
		if c.transport == "rpc" {
			endpoint = fmt.Sprintf("single(rpc:%s)", c.addr)
		} else {
			endpoint = fmt.Sprintf("single(shm:%s)", c.shm)
		}
		if err := benchkit.Run(ctx, s, c.Config, "taihu bench single", endpoint); err != nil {
			benchkit.StopCPUProfile(cpuFile)
			return fmt.Errorf("run error: %w", err)
		}

		benchkit.StopCPUProfile(cpuFile)
		// 客户端收帧尺寸统计（验证每帧是否整块 4MiB、TakeTry 命中率）。
		fmt.Printf("==== frame stats ====\n%s", transport.StatsString())
		return nil
	},
}

type singleBenchConfig struct {
	transport  string
	addr       string
	shm        string
	conns      int
	cpuProfile string
	benchkit.Config
}

func init() {
	f := benchSingleCmd.Flags()
	f.String("transport", "", "data-plane transport: rpc (TCP, with -addr) | shm (shared memory, with -shm)")
	f.String("addr", "", "taihu-server TCP address(es), comma-separated (with -transport rpc)")
	f.String("shm", "", "taihu-server unix socket path (with -transport shm)")
	f.Int("conns", 4, "connections per TCP address (shmipc SessionNum for shm; multi-address builds addrs x conns, round-robin)")
	f.String("cpuprofile", "", "write cpu profile to this file (pprof)")
	f.String("mode", "", "write | read | delete")
	f.Int64("size", 4096, "object size in bytes")
	f.Int("threads", 1, "number of concurrent goroutines")
	f.String("keys-prefix", "sbench", "key prefix, keys are <prefix>/<seq>")
	f.Int("count", 1000, "total number of distinct objects")
	f.Duration("report-interval", 2*time.Second, "progress report interval")
	f.Bool("latency", false, "record per-op latency")
	f.Int("pipeline", 1, "per-worker in-flight ops (1 = serial; >1 deepens disk queue for single-block objects)")
}

func (c *singleBenchConfig) validate() error {
	switch c.transport {
	case "rpc":
		if c.addr == "" {
			return fmt.Errorf("-transport rpc requires -addr (taihu-server TCP address)")
		}
	case "shm":
		if c.shm == "" {
			return fmt.Errorf("-transport shm requires -shm (taihu-server unix socket)")
		}
	default:
		return fmt.Errorf("-transport must be rpc or shm (got %q)", c.transport)
	}
	if err := c.Config.Validate(); err != nil {
		return err
	}
	return nil
}
