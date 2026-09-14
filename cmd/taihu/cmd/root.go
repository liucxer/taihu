// Package cmd 实现 taihu 命令的子命令树（cobra）。
package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pingcap/log"
	"github.com/spf13/cobra"
	"go.uber.org/zap/zapcore"

	"github.com/liucxer/taihu/internal/aio"
	"github.com/liucxer/taihu/pkg/taihu"
)

// silenceTiKVLog 抑制 tikv client-go（pingcap/log）的 INFO/WARN 刷屏：CLI 输出
// 保持纯净，连通性失败由命令自身报错。
func init() {
	log.SetLevel(zapcore.ErrorLevel)
}

// global 全局生效参数（root persistent flags），各子命令经 helpers 读取。
var global = struct {
	pd         string        // TiKV PD 地址列表（逗号分隔）
	tikvCA     string        // TiKV TLS CA 证书路径（与 -tikv-cert/-tikv-key 同用；空=明文）
	tikvCert   string        // TiKV TLS 客户端证书路径
	tikvKey    string        // TiKV TLS 客户端私钥路径
	clientName string        // 本客户端标识（标注 LOCAL、客户端清单过滤用）
	timeout    time.Duration // 单次交互超时
	json       bool          // 机器可读 JSON 输出

	// 磁盘异步 IO 后端（server 与 bench storage 生效，以命令行为准）。
	ioUring   string // auto|on|off
	ioUringIO bool   // io_uring 的 IOPOLL 模式（仅 io_uring 后端生效，默认关）
}{timeout: 5 * time.Second, ioUring: "auto"}

// rootCmd 根命令：无子命令时打印帮助。
var rootCmd = &cobra.Command{
	Use:   "taihu",
	Short: "taihu 对象数据库统一命令行工具",
	Long: `taihu：服务端（server）、性能压测（bench storage/cluster/single）、
运维/客户端（cluster/key/instance/client）与版本（version）的统一入口。

分层：
  taihu server ...                 启动 taihu 对象服务端（daemon）
  taihu bench storage ...          本地裸盘 Storage 层压测（不走网络）
  taihu bench cluster ...          集群端到端压测（走 TiKV 定位实例）
  taihu bench single ...           单机直通压测（不走 TiKV，直连 taihu-server）
  taihu cluster list|status|...    集群/实例/索引运维
  taihu key put|get|delete|...     对象读写删与定位
  taihu instance segments          实例 segment 信息
  taihu client list|info           SDK 客户端清单与工具环境信息
  taihu version                    显示版本

集群类命令需要 -pd 指向 TiKV PD（实例/索引/客户端注册区所在）；无 TiKV 环境可用
-addr/instance 直连单个实例。`,
	Example: `  # 启动服务端
  taihu server -listen 10.0.0.1 -db /mnt/db -dev /dev/nvme0n1 -server-name TAIHU-0 -pd 100.71.128.11:2379

  # 集群端到端压测（写）
  taihu --pd 100.71.128.11:2379,100.71.128.12:2379 bench cluster -mode write -client-name t11 -size 4194304 -count 40000

  # 集群运维
  taihu --pd 100.71.128.11:2379 cluster status

  # 对象读写
  echo hello | taihu --pd 100.71.128.11:2379 key put -key hello
  taihu --pd 100.71.128.11:2379 key get -key hello`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute 执行根命令（命令错误已打印，返回后 main 按码退出）。
func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "taihu:", err)
		return err
	}
	return nil
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVar(&global.pd, "pd", "", "TiKV PD 地址列表（逗号分隔），集群类命令必需")
	pf.StringVar(&global.tikvCA, "tikv-ca", "", "TiKV TLS CA 证书路径（与 -tikv-cert/-tikv-key 同用；空=明文）")
	pf.StringVar(&global.tikvCert, "tikv-cert", "", "TiKV TLS 客户端证书路径")
	pf.StringVar(&global.tikvKey, "tikv-key", "", "TiKV TLS 客户端私钥路径")
	pf.StringVar(&global.clientName, "client-name", "", "本客户端标识（标注 LOCAL、客户端清单过滤用；传本机 hostname 可高亮本机实例）")
	pf.DurationVar(&global.timeout, "timeout", 5*time.Second, "单次交互超时")
	pf.BoolVar(&global.json, "json", false, "机器可读 JSON 输出")

	pf.StringVar(&global.ioUring, "io-uring", "auto",
		"磁盘异步 IO 后端：auto=内核支持 io_uring 则用（其余回退 libaio），on=强制 io_uring（不支持则启动失败），off=强制 libaio")
	pf.BoolVar(&global.ioUringIO, "io-uring-iopoll", false,
		"io_uring 启用 IOPOLL 轮询模式（仅 -io-uring 生效；需块设备 /sys/class/block/<dev>/queue/io_poll=1）")

	rootCmd.AddCommand(
		versionCmd,
		serverCmd,
		benchCmd,
		clusterCmd,
		keyCmd,
		instanceCmd,
		clientCmd,
	)
}

// ctxWithTimeout 返回带 timeout 的上下文（全局 -timeout）。
func ctxWithTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, global.timeout)
}

// aioOptions 把全局 -io-uring / -io-uring-iopoll 映射为 NewStorage 的选项。
// 取值非法（非 auto|on|off）时在此报错，而不是静默按默认值跑。
func aioOptions() ([]taihu.Option, error) {
	m, err := aio.ParseMode(global.ioUring)
	if err != nil {
		return nil, err
	}
	return []taihu.Option{
		taihu.WithAIOMode(m),
		taihu.WithAIOIOPoll(global.ioUringIO),
	}, nil
}
