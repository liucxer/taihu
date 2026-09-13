// Command taihu 是 taihu 对象的统一命令行工具（cobra 子命令树）：
// 服务端（server）、性能压测（bench storage/cluster/single）、
// 运维/客户端（cluster/key/instance/client）与版本（version）。
package main

import (
	"os"

	"github.com/liucxer/taihu/cmd/taihu/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
