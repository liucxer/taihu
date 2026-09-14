// Sub-command taihu bench cluster 是 taihuclient 集群端到端压测工具（设计文档_v3 §8）。
// 与单机版 taihu bench single 同语义：key 集合 <prefix>/<seq>，读模式区间切分保证每 key 全进程只读一次。
// 集群模式：SDK 查 TiKV（-pd）定位实例后选路；-transport 控制数据面传输
// ——auto（默认，同机走 shm、跨节点走 TCP）/ rpc（强制 TCP，含同机）/ shm（强制共享内存）。
// read 前须先用相同前缀 write 灌好数据。
package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/benchkit"
	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/transport"
	"github.com/liucxer/taihu/pkg/taihu-client"
)

// benchClusterCmd 集群端到端压测（走 TiKV 定位实例）。
var benchClusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "集群端到端压测（走 TiKV 定位实例，taihuclient）",
	Long: `taihu bench cluster：taihuclient 集群端到端压测。

必传参数：
  -mode write|read|delete        测试模式
  -client-name                   客户端标识（根级参数；集群注册/同机判定用）
  -pd <PD...>                    TiKV PD 地址（根级参数，逗号分隔）

可选参数：
  -transport auto|rpc|shm        数据面传输：auto=同机shm/跨节点TCP（默认），rpc=强制TCP（含同机），shm=强制共享内存
  -write-routing local|round-robin  写路由算法：local 优先本地（默认）/ round-robin 轮询全部实例
  -conns                         每 TCP 地址连接数（shm 会话数；多 IP 实例总连接=地址数×conns）
  -preload                       读模式：先预热 RouteCache 再计时（剔除每 key 查 TiKV 开销）

说明：
  同机也可用 -transport rpc 测 RPC 性能；read 前须先用相同前缀 write 灌好数据。`,
	Example: `  # 跨节点自动选路写（同机 shm / 跨节点 TCP）
  taihu --pd 100.71.128.11:2379,100.71.128.12:2379 bench cluster -mode write -client-name t11 -size 4194304 -count 40000 -threads 32 -latency

  # 同机强制 RPC 读（预热路由缓存，剔除每 key 查 TiKV 开销）
  taihu --pd 100.71.128.11:2379 --client-name t12 bench cluster -mode read -transport rpc -count 40000 -threads 32 -latency -preload`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c := &clusterBenchConfig{}
		c.transport, _ = cmd.Flags().GetString("transport")
		c.writeRouting, _ = cmd.Flags().GetString("write-routing")
		c.conns, _ = cmd.Flags().GetInt("conns")
		c.preload, _ = cmd.Flags().GetBool("preload")
		c.cpuProfile, _ = cmd.Flags().GetString("cpuprofile")
		c.Mode, _ = cmd.Flags().GetString("mode")
		c.Size, _ = cmd.Flags().GetInt64("size")
		c.Threads, _ = cmd.Flags().GetInt("threads")
		c.Prefix, _ = cmd.Flags().GetString("keys-prefix")
		c.Count, _ = cmd.Flags().GetInt("count")
		c.ReportEvery, _ = cmd.Flags().GetDuration("report-interval")
		c.Latency, _ = cmd.Flags().GetBool("latency")

		c.clientName = global.clientName
		c.tikvPD = global.pd
		c.tikvCA = global.tikvCA
		c.tikvCert = global.tikvCert
		c.tikvKey = global.tikvKey

		if err := c.validate(); err != nil {
			return err
		}

		ctx := context.Background()

		cpuFile, err := benchkit.StartCPUProfile(c.cpuProfile)
		if err != nil {
			return fmt.Errorf("cpuprofile: %w", err)
		}

		// 集群模式：先查 TiKV 定位实例（taihuclient；-transport 控制数据面传输方式）。
		kv, kerr := cluster.NewTiKVKV(ctx, strings.Split(c.tikvPD, ","), cluster.TLSConfig{
			CA: c.tikvCA, Cert: c.tikvCert, Key: c.tikvKey,
		})
		if kerr != nil {
			benchkit.StopCPUProfile(cpuFile)
			return fmt.Errorf("tikv %s: %w", c.tikvPD, kerr)
		}
		defer kv.Close()
		s, err := taihuclient.NewCluster(taihuclient.ClusterConfig{
			KV:         kv,
			ClientName: c.clientName,
			// 每地址连接数：同机实例 shm 会话数、跨节点每 TCP 地址连接数（-conns；多 IP 实例总连接数=地址数×conns）。
			Conns: c.conns,
			// 写路由算法（-write-routing）：local 默认优先本地，round-robin 轮询全部实例。
			WriteRouting: c.writeRouting,
			// 数据面传输方式（-transport）：auto 同机 shm / 跨节点 TCP，rpc 强制 TCP（含同机），shm 强制共享内存。
			Transport: c.transport,
			// 压测场景无真实远端源：miss 即记为未命中（回源兜底语义不参与压测带宽）。
			Source: func(ctx context.Context, key string) ([]byte, error) {
				return nil, os.ErrNotExist
			},
		})
		if err != nil {
			benchkit.StopCPUProfile(cpuFile)
			return fmt.Errorf("cluster %s: %w", c.clientName, err)
		}
		defer s.Close()

		// 预热路由缓存（集群读模式）：逐 key 查索引填充 RouteCache，消除冷启动的
		// "每 key 查 TiKV"开销，只验证热缓存下的数据面带宽。
		if c.preload && c.Mode == "read" {
			keys := make([]string, 0, c.Count)
			for k := 0; k < c.Count; k++ {
				keys = append(keys, benchkit.KeyFor(c.Prefix, k))
			}
			s.PreloadRoute(ctx, keys)
			fmt.Printf("preloaded %d keys into RouteCache\n", len(keys))
		}

		if err := benchkit.Run(ctx, s, c.Config, "taihu bench cluster",
			fmt.Sprintf("cluster(client=%s,transport=%s)", c.clientName, c.transport)); err != nil {
			benchkit.StopCPUProfile(cpuFile)
			return fmt.Errorf("run error: %w", err)
		}

		benchkit.StopCPUProfile(cpuFile)
		// 客户端收帧尺寸统计（验证每帧是否整块 4MiB、TakeTry 命中率）。
		fmt.Printf("==== frame stats ====\n%s", transport.StatsString())
		return nil
	},
}

type clusterBenchConfig struct {
	clientName   string
	tikvPD       string
	tikvCA       string
	tikvCert     string
	tikvKey      string
	transport    string
	writeRouting string
	conns        int
	preload      bool
	cpuProfile   string
	benchkit.Config
}

func init() {
	f := benchClusterCmd.Flags()
	f.String("transport", taihuclient.TransportAuto, "data-plane transport: auto (shm same-host / rpc cross-host) | rpc (force TCP, incl. same-host) | shm (force shared memory, same-host only)")
	f.String("write-routing", "", "write routing algorithm: local (default, prefer local instance) | round-robin (all instances)")
	f.Int("conns", 4, "connections per TCP address (shmipc SessionNum for shm / local: per address, multi-IP instance builds addrs x conns, round-robin)")
	f.Bool("preload", false, "read: preload RouteCache before timing")
	f.String("cpuprofile", "", "write cpu profile to this file (pprof)")
	f.String("mode", "", "write | read | delete")
	f.Int64("size", 4096, "object size in bytes")
	f.Int("threads", 1, "number of concurrent goroutines")
	f.String("keys-prefix", "rbench", "key prefix, keys are <prefix>/<seq>")
	f.Int("count", 1000, "total number of distinct objects")
	f.Duration("report-interval", 2*time.Second, "progress report interval")
	f.Bool("latency", false, "record per-op latency")
}

func (c *clusterBenchConfig) validate() error {
	switch c.transport {
	case taihuclient.TransportAuto, taihuclient.TransportRPC, taihuclient.TransportShm:
	default:
		return fmt.Errorf("invalid -transport %q: must be %s, %s or %s",
			c.transport, taihuclient.TransportAuto, taihuclient.TransportRPC, taihuclient.TransportShm)
	}
	if c.clientName == "" {
		return fmt.Errorf("-client-name is required")
	}
	if c.tikvPD == "" {
		return fmt.Errorf("-client-name requires -pd")
	}
	if err := c.Config.Validate(); err != nil {
		return err
	}
	return nil
}
