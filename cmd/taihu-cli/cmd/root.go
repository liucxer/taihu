// Package cmd 实现 taihu-cli 的子命令树（cobra）。
package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pingcap/log"
	"github.com/spf13/cobra"
	"go.uber.org/zap/zapcore"
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
}{timeout: 5 * time.Second}

// rootCmd 根命令：无子命令时打印帮助。
var rootCmd = &cobra.Command{
	Use:   "taihu-cli",
	Short: "taihu 对象数据库命令行运维工具",
	Long: `taihu-cli：查询集群/实例/segment 信息、key 读写删与定位、SDK 客户端清单。
集群类命令需要 -pd 指向 TiKV PD（实例/索引/客户端注册区所在）；无 TiKV 环境可用
-addr/instance 直连单个实例。`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute 执行根命令（命令错误已打印，返回后 main 按码退出）。
func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "taihu-cli:", err)
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

	rootCmd.AddCommand(
		versionCmd,
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
