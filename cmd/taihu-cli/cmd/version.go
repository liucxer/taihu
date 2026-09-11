package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/version"
)

// versionCmd 打印工具版本（commit_日期）。
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "显示版本号",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("taihu-cli %s\n", version.String())
	},
}
