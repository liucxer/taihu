package cmd

import (
	"github.com/spf13/cobra"
)

// benchCmd 性能压测父命令：storage（本地裸盘）/ cluster（集群端到端）/ single（单机直通）。
var benchCmd = &cobra.Command{
	Use:   "bench",
	Short: "性能压测：storage（本地裸盘）/ cluster（集群端到端）/ single（单机直通）",
	Long: `taihu bench：性能压测入口，三个子命令：
  storage    Storage 层本地裸盘压测（不走网络，-db/-dev 直连盘）
  cluster    集群端到端压测（走 TiKV 定位实例，taihuclient）
  single     单机直通压测（不走 TiKV，直连 taihu-server，rpcclient）`,
	Example: `  # 本地裸盘压测
  taihu bench storage -mode write -db /mnt/db -dev /dev/nvme0n1 -size 4194304 -count 40000

  # 集群端到端压测
  taihu --pd 100.71.128.11:2379 bench cluster -mode write -client-name t11 -size 4194304 -count 40000

  # 单机直通压测
  taihu bench single -mode read -transport shm -shm /dev/TAIHU-0 -count 40000`,
}

func init() {
	benchCmd.AddCommand(benchStorageCmd, benchClusterCmd, benchSingleCmd)
}
