# 全仓库代码约定

> 本仓库的代码风格是按"读代码的人"设计的：注释讲为什么、命名不缩写、日志有固定通道、import 按来源分组；这些约定跨所有包，改任何一层都适用。

---

## 注释

### 1. 中文整句，讲"为什么"而不是"是什么"

注释默认是中文完整句子。包文档常带**设计文档交叉引用**，把代码锚回设计依据：

- `internal/transport/server.go:1` —— `// Package transport 实现 taihu 的 netpoll 传输层（设计文档_v3 的远程访问层改造，以 netpoll + LinkBuffer 取代 gRPC/HTTP-2）。`
- `internal/rpcclient/dial.go:1` —— `// Package rpcclient 提供 taihu 客户端（设计文档_v3 §5，netpoll 传输层改造）。`
- `cmd/taihu/cmd/server.go:1` —— 引 `设计文档_v3 §4`；`cmd/taihu/cmd/bench_storage.go:1` 引 `设计文档 §9`。

引用设计文档时**要带小节号**（`§4` / `§5.1` / `§7.1`），让人能直接翻到出处。

### 2. 用 Markdown 粗体强调关键判断

不是全篇加粗，只加在"这句是约束、不是描述"的地方：

- `internal/ierr/ierr.go:1` —— `// Package ierr 定义 taihu 存储的公共错误 —— **唯一事实源**。`
- `internal/rpcclient/reexport.go:19` —— `// ...只是给同一个类型在本包内起一个外部可引用的名字，**签名与行为完全不变**。`
- `internal/metastore/meta.go:118` —— `// ...**刻意不让 protocol import 本包** —— 本包依赖...`
- `internal/aio/aio_uring_linux.go:324` —— 讲 ring 结构体偏移语义时用 `**字节偏移**`。

### 3. 非测试代码里没有 TODO / FIXME / XXX / nolint

实测（仓库根目录执行）：

```bash
grep -rn 'TODO\|FIXME\|XXX' --include='*.go' . | grep -v third_party/   # 无输出
grep -rn 'nolint' --include='*.go' . | grep -v third_party/             # 无输出
```

**这不是"习惯了不写"，而是硬约束**：未完成的工作写进注释等于埋雷，要么当轮做完，要么开 Trellis task。禁止用 `//nolint` 压掉 vet 告警 —— `Makefile:22` 的 `go vet ./...` 是 `make check` 的一部分，压掉就等于门禁失效。

### 4. 非测试代码不 `panic()`

实测 `grep -rn 'panic(' --include='*.go' . | grep -v third_party/ | grep -v '_test.go'` 无输出。库代码一律返回 `error`；`panic` 只出现在 `_test.go` 里（测试辅助函数遇到不可恢复的装配错误时才用）。

---

## 命名

### 5. receiver 用单字母，同一类型恒定同一字母

实测各包 receiver 分布，同一类型从不换字母：

| 包 | receiver |
|----|----------|
| `internal/storage` | `func (s *Storage)` × 24（`storage.go` 内全部） |
| `internal/transport` | `func (s *Server)` × 9、`func (c *Conn)` × 2（`server.go`） |
| `internal/aio` | `func (r *ring)` × 15、`func (r *uringRing)` × 13 |
| `internal/rpcclient` | `func (s *Storage)` × 14、`func (w *PutWriter)` × 2 |
| `internal/cluster` | `func (k *TiKVKV)` × 9、`func (k *MemoryKV)` × 8 |

### 6. 首字母缩写全大写，不写 `ClientId`

正确形态：`ClientID`（`pkg/taihu-client/config.go:71`）、`ioUringIO`（`cmd/taihu/cmd/root.go:36`）、`ioUringSQE`（`internal/aio/aio_uring_linux.go:75`）、`printJSON`（`cmd/taihu/cmd/helpers.go:118`）。

实测 `grep -rn 'ClientId\|ServerId\|InstanceId\|Url\b\|Http\b\|Json\b' --include='*.go' internal/ pkg/ cmd/ | grep -v '_test.go'` 无输出 —— 没有一处写成 `ClientId` 这类形式。

注意 `pkg/taihu-client/` 目录名带连字符，但包名是 `taihuclient`（`pkg/taihu-client/config.go:7`）；所有 17 个 `.go` 文件一致。

### 7. 平台专有文件用 `_linux` / `_other` 后缀

例如 `internal/aio/aio_uring_linux.go`、`internal/transport/server_shm_linux.go` / `server_shm_other.go`、`internal/rpcclient/dial_shm_linux.go` / `dial_shm_other.go`。后缀名本身表达平台约束，同一平台的两个文件（`_linux` 实现 + `_other` 桩）成对出现。

`_linux` 侧**靠后缀即可**，不必再写 `//go:build linux`（实测 10 个 `_linux.go` 里 9 个没写，只有 `internal/rpcclient/putwriter_linux.go:1` 冗余带了一条）。但 `_other` 侧**必须**显式写 `//go:build !linux` —— `other` 不是 Go 认可的 GOOS，光靠后缀会导致同一平台上 `_linux` 与 `_other` 两份实现同时进编译、报重复声明（实测 7 个 `_other.go` 全部带 tag）。

文件名与 build tag 的完整规则（含为什么不能反过来写、以及 `_test` 后缀必须排在 GOOS 之后）见 `.trellis/spec/platform/file-splitting.md`。

---

## 错误包装措辞

### 8. `fmt.Errorf("op: %w", err)`，op 前缀小写英文

全仓库非测试代码 37 处 `: %w"` 包装，主导形态是「小写操作名 + 冒号 + `%w`」：

- `internal/cluster/kv_tikv.go:106` —— `return nil, fmt.Errorf("tikv txnkv connect: %w", err)`
- `internal/rpcclient/dial.go:33` —— `return nil, fmt.Errorf("DialPoolMulti: empty addrs")`（无 err 时也不加句号）
- `internal/storage/compact.go:191` —— `return 0, fmt.Errorf("compact: short read %s: got %d, want %d", key, len(buf), meta.Size)`

跨库边界的错误再用 `taihu: ` 前缀标明来源（非测试代码 41 处 `fmt.Errorf("taihu: ...")`）。

**唯一的例外是给操作人的环境指引**：`internal/aio/aio_uring_linux.go:24,27` 刻意用中文写清前置条件，因为这条错误要被人直接照着执行：

```go
return fmt.Errorf("aio: IOPOLL 需要 %s 为 1（当前为 %q），请先执行: echo 1 > %s",
```

### 9. 面向用户的 CLI 文案用中文

`cmd/taihu/cmd/key.go:80` 给的是可直接照做的中文提示，而不是英文错误码：

```go
return nil, nil, nil, fmt.Errorf("需要一个目标：-addr ADDR / -instance NAME（配合 -pd）/ -pd（集群路由）")
```

同文件 `key.go:95` 走 `fmt.Errorf("list instances: %w", err)` —— 内部包装仍是英文小写前缀，中文只出现在终端用户读到的文案上。

---

## 日志三通道

**本仓库只有三条日志通道，新增日志必须落进其中一条，不要开第四条。**

### 10. 通道一：`github.com/pingcap/log` 只用来压级别，不用来打日志

全仓库只有 `cmd/taihu/cmd/root.go:10` 一个文件 import 它，且只有一次调用 —— `root.go:20-22` 的 `init()`：

```go
// silenceTiKVLog 抑制 tikv client-go（pingcap/log）的 INFO/WARN 刷屏：CLI 输出
// 保持纯净，连通性失败由命令自身报错。
func init() {
	log.SetLevel(zapcore.ErrorLevel)
}
```

它**不是**本仓库的诊断输出通道，只是为了压掉 tikv client-go 的 INFO 刷屏。

### 11. 通道二：诊断输出走标准库 `log`，库内统一带显式前缀

非测试代码 import 标准库 `"log"` 的位置只有 5 个文件：`cmd/taihu/cmd/server.go:11`、`internal/aio/aio.go:31`、`internal/aio/aio_uring_linux.go:5`、`internal/storage/compact.go:7`、`examples/taihu-client/main.go:21`。

其中 **`internal/` 下恰好 5 处 `log.Printf`，全部带显式前缀**：

| 位置 | 前缀 | 内容 |
|------|------|------|
| `internal/aio/aio.go:203` | `taihu: aio ` | 启动时打印后端 / sq / cq / kernel |
| `internal/aio/aio.go:207` | `taihu: aio ` | 回退分支打印后端 / kernel / 回退原因 |
| `internal/aio/aio_uring_linux.go:573` | `taihu: aio ` | CQ 溢出告警 |
| `internal/storage/compact.go:72` | `compaction: ` | 单轮搬移进度与错误 |
| `internal/storage/compact.go:165` | `compaction: ` | 单轮结束统计 |

**前缀是包级或子系统级**（`taihu: aio `、`compaction: `），不是文件名 —— 读日志的人据此判断出问题的是哪个子系统。

`cmd/taihu/cmd/server.go` 另有 12 处标准库 `log.Printf` / `log.Println`（`139,183,197,228,234,236,245,246,251,259,267,276`）：面向运维的启动/生命周期信息，前缀不统一，部分用中文（如 `server.go:228` 的「shmipc 仅 Linux 支持，本平台跳过」）。**这是 CLI/服务端进程入口的特权**，库代码不要模仿。

### 12. 通道三：周期性统计用 `[stat]` 前缀

`cmd/taihu/cmd/server.go:245-251` 在 5 秒 ticker 里逐行打 `[stat]`：

```go
log.Printf("[stat] disk-io 4MiB=%d other=%d bytes4MiB=%d bytesOther=%d", io4M, ioOther, b4M, bOther)
log.Printf("[stat] segments free=%d active=%d full=%d reclaiming=%d",
	st.SegmentStats()[metastore.SegmentStateFree],
	...
log.Printf("[stat] %s", transport.StatsString())
```

`[stat]` 是给日志抓取用的机器可读标记 —— 周期性指标一律走它，不要混进上面的诊断前缀。

---

## import 分组

### 13. 三段式：标准库 / 第三方（含 `third_party/` fork）/ 本项目

组间空行分隔。`internal/transport/client.go:5-15` 是最典型的形态：

```go
import (
	"context"
	"fmt"
	"time"

	"github.com/liucxer/taihu/third_party/netpoll"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/ierr"
	"github.com/liucxer/taihu/internal/transport/protocol"
)
```

`third_party/` 下的 fork（`netpoll`、`shmipc-go`）**归第三方组**，不归本项目组 —— 因为在 `go.mod` 语义里它们是本模块代码，但在人的认知里它们是上游 fork。`cmd/taihu/cmd/key.go:3-14` 把 `github.com/spf13/cobra`（`:9`）放第三方组、`pkg/taihu-client`（`:13`）放本项目组，是同一规则的 CLI 版。

gofmt 只管缩进不管分组，这条靠人守；`make check-fmt` 不会替你发现分组错了。
