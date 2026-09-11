// Command taihu-cli 是 taihu 对象的命令行运维/客户端工具（taihu-cli 设计文档 v2；
// 合并原 taihu-client 的 put/get/delete/stat）。
// 子命令：cluster（list/status/index）、key（put/get/delete/stat/meta/list）、
// instance segments、client（list/info）、version。
package main

import (
	"os"

	"github.com/liucxer/taihu/cmd/taihu-cli/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
