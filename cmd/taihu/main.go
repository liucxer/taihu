// Command taihu 是 taihu 对象的统一命令行工具（cobra 子命令树）：
// 服务端（server）、性能压测（bench storage/cluster/single）、
// 运维/客户端（cluster/key/instance/client）与版本（version）。
package main

import (
	"os"

	"github.com/liucxer/taihu/cmd/taihu/cmd"
)

// run 以 args（不含程序名）为命令行参数执行 CLI，返回进程退出码。
// 抽成函数只为让「参数装配 + 退出码」可被单测覆盖：生产路径 main 传入
// os.Args[1:]，args 非 nil，等价于原先直接在 main 里调 cmd.Execute()。
func run(args []string) int {
	if args == nil {
		args = os.Args[1:]
	}
	os.Args = append([]string{os.Args[0]}, args...)
	if err := cmd.Execute(); err != nil {
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:]))
}
