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

### 3. 非测试代码的禁用清单：TODO / FIXME / XXX / nolint / 凭据

实测（仓库根目录执行）：

```bash
grep -rn 'TODO\|FIXME\|XXX' --include='*.go' . | grep -v third_party/   # 无输出
grep -rn 'nolint' --include='*.go' . | grep -v third_party/             # 无输出
grep -rniE 'secret|password|passwd|apikey|api_key|token[[:space:]]*[:=]|BEGIN [A-Z ]*PRIVATE KEY' \
  --include='*.go' internal pkg cmd examples | grep -v _test.go         # 无输出
```

**这不是"习惯了不写"，而是硬约束**：

- **未完成的工作写进注释等于埋雷** —— 要么当轮做完，要么开 Trellis task
- **禁止用 `//nolint` 压掉 vet 告警** —— `Makefile:22` 的 `go vet ./...` 是 `make check` 的一部分，压掉就等于门禁失效
- **不得硬编码凭据**（上游 `ecc-038`）—— 密钥/口令/令牌一律从环境变量或配置文件进，不落进源码。taihu 现状是**零命中**：连部署模板 `configs/taihu-server.example.sh` 里的 `LISTEN` / `DB` / `DEV` / `NAME` / `PD` 也全部留空由环境填。**提交前扫一遍上面第三条命令**，它是唯一能在 review 之外自动兜住凭据的检查。

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

### 8. `fmt.Errorf("op: %w", err)`，op 前缀的形态按**场合**分两套

全仓库非测试代码 37 处 `: %w"` 包装。op 前缀存在两套写法，**不是随机混用，边界很清楚**：

| 场合 | 前缀形态 | 实例 |
|------|----------|------|
| **库代码**（`internal/`、`pkg/`） | 小写业务动作名，**一律如此** | `internal/cluster/kv_tikv.go:106` `tikv txnkv connect: %w`、`internal/storage/compact.go:191` `compact: short read …` |
| **CLI 入口**（`cmd/taihu/cmd/`），指代具体 Go 构造步骤 | 导出名原样，不转小写 | `cmd/taihu/cmd/server.go:122` `DeviceCapacity %s: %w`、`:148` `NewStorage: %w`、`bench_storage.go:100` `StartCPUProfile: %w`、`bench_storage.go:126` `LoadCache: %w` |
| **CLI 入口**，描述业务动作 | 小写短语 | `cmd/taihu/cmd/key.go:95` `list instances: %w`、`run error: %w`、`cluster register: %w` |

库代码侧用 grep 可机械判定「合不合规」：

```bash
# 库代码里以大写开头的 op 前缀 —— 期望只有下面标注的那一处
grep -rnE 'fmt\.Errorf\("[A-Z]' --include='*.go' internal pkg | grep -v '_test.go'
```

实测输出**恰好一行**：`internal/rpcclient/dial.go:33` 的 `fmt.Errorf("DialPoolMulti: empty addrs")`。这是库代码侧的**唯一历史例外**（规则写小写，此处不改，新代码不要照抄）。

CLI 侧之所以留导出名，是因为那些前缀直接命名**用户正要执行的那一步**，用导出名能让人一眼对上命令输出；而 `list instances` 这类描述的是业务动作，用小写更顺。**判据是「前缀指代的是 Go 函数，还是业务动作」，不是「文件在哪个目录」** —— `cmd/taihu/cmd/key.go` 里两套都在。

无 err 可包时也不加句号（`empty addrs`，不是 `empty addrs.`）。

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

非测试代码 import 标准库 `"log"` 的位置只有 5 个文件：`cmd/taihu/cmd/server.go:11`、`internal/aio/aio_internal.go:5`、`internal/aio/aio_uring_linux.go:5`、`internal/storage/compact.go:7`、`examples/taihu-client/main.go:21`。

其中 **`internal/` 下恰好 5 处 `log.Printf`，全部带显式前缀**：

| 位置 | 前缀 | 内容 |
|------|------|------|
| `internal/aio/aio_internal.go:33` | `taihu: aio ` | 启动时打印后端 / sq / cq / kernel |
| `internal/aio/aio_internal.go:37` | `taihu: aio ` | 回退分支打印后端 / kernel / 回退原因 |
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

### ⚠️ 已知例外：`internal/device` 事实上存在一个第四通道

**这三处破坏了本节开头那句「不要开第四条」—— 它们是库代码，却直接写 `os.Stderr`：**

| 位置 | 场合 | 文案 |
|------|------|------|
| `internal/device/device.go:155` | 完成泵 `pump()` 的硬错误（`ring.Wait` 返回非 `ErrTimeout`） | 英文，前缀 `taihu: ` |
| `internal/device/device.go:295` | `logComplRetry` 的**放弃**分支（重试预算耗尽） | **中文** |
| `internal/device/device.go:299` | `logComplRetry` 的**重试**分支 | **中文** |

与规则 11 / 12 的三点偏差，逐条对得上：

1. **device 是库代码**，不享有规则 11 末尾那句「这是 CLI/服务端进程入口的特权」；
2. **前缀是 `taihu: ` 而不是通道二的 `taihu: aio `** —— 读日志的人无法从前缀判断出问题在 `device` 还是 `aio`；
3. **`:295` / `:299` 是中文文案**，而通道二的库内诊断（`taihu: aio `、`compaction: `）一律英文 —— 规则 9 只允许「面向用户的 CLI 文案」用中文。

之所以没被前面的排查发现：`grep` 通道二时按 `import "log"` 找文件，而这三处**根本不 import `log`**（走 `fmt.Fprintf` + `os.Stderr`），所以按「谁 import 了 log」清点永远漏掉它们。

**本仓库的处置是「登记为待办，不在本轮改」**：正确做法是改成通道二（`log.Printf("taihu: device ...")`，前缀与文案语言一并对齐），但那是一次代码改动 —— 任务 `09-21-spec-upstream-alignment` 明确不碰任何 `.go` 文件（见其 `implement.md` 的「不做的事」）。

**为什么不就地把它承认成「第四条通道」**：理由写不出来。`pump()` 确实在热路径上（标准库 `log` 带互斥锁与时间戳），但代码里没有一行注释说明为什么绕过 `log`；而同一文件的 `logComplRetry`（`:288`）**自己就做了刷屏节流**（「每条 IO 仅在首次重试与预算耗尽时各打一行」）—— 说明作者对这里的日志成本有过考虑，只是没落到注释里。没有可检验的理由就不能立规则（`.trellis/tasks/09-21-spec-upstream-alignment/design.md` §3.3）。**新增日志一律照上面三条通道来，不要拿这三处当先例。**

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

---

## 错误处理的其余约定

规则 8、9 讲错误**怎么措辞**；本节讲错误的**去向** —— 吞不吞、什么时候原样返回、送到别的系统时长什么样。

### 14. 不静默吞掉 error；确需丢弃时必须 `_ =` 并写明理由

上游 `gbp-019` 给的例外形态本身就是带注释的：`_ = conn.Close() // 已日志`。也就是说 **丢弃是被允许的，缺的是那行理由**。

实测非测试代码行首丢弃语句（可复现）：

```bash
grep -rnE '^[[:space:]]*_ =' --include='*.go' internal pkg cmd examples | grep -v '_test.go' | wc -l
# 96
```

分布：

| 目录 | 处数 |
|------|------|
| `internal/transport` | **60** |
| `internal/aio` | 11 |
| `cmd/taihu` | 8 |
| `pkg/taihu-client` | 6 |
| 其余（`storage`/`metastore`/`device`/`cluster`/`benchkit`/`rpcclient`） | 11 |

**96 处里只有 2 处写了理由**，且用的是同一个词：

- `internal/storage/admin.go:45` —— `_ = s.db.IterMapping(ctx, …) // 对象数（尽力而为）`
- `pkg/taihu-client/index.go:59` —— `_ = m.kv.BatchPut(context.Background(), batch) // 尽力而为`

`internal/transport` 的 60 处形态高度集中：

| 形态 | 处数 | 文件 |
|------|------|------|
| `_ = c.writeFrame(` | 35 | `server.go` 24、`server_admin.go` 11 |
| `_ = shmWriteFrame(` | 33 中的主部 | `server_shm_linux.go` |
| `_ = …Close(` / `SetDeadline(` / `Release(` | 其余 | 收尾与停机路径 |

**这些丢弃在语义上是对的**：35 处 `writeFrame` 里有 30 处紧跟 `return`（实测），是**响应侧**回帧 —— 写失败只可能因为连接已断或已超时，此时没有补救动作，也没有"上报给谁"的接收方，调用方的 `return` 会走到既有的关闭路径。典型形态见 `internal/transport/server.go:211-213`：

```go
if err != nil {
    _ = c.writeFrame(st.id, protocol.OpResp, protocol.EncCode(protocol.CodeInvalidArgument))
    return
}
```

**但规则要求把理由写下来**，不能靠读代码的人自己推：

- 同一文件内**第一处**丢弃必须注释说明本文件丢弃的通用理由，后续同类可省
- 与"尽力而为"不同类的情形要单独说明 —— 尤其 `internal/transport/server_shm_linux.go:425-427`，先 `_, rerr := s.shmWriteDataFrameBatch(…)` 把错误接出来、**隔一个空行**再 `_ = rerr` 显式丢弃；这种"接出来又丢掉"的写法必须带注释，否则读者无法判断是刻意还是漏改

### 15. 没有额外上下文可加时，原样 `return err`

上游 `uber-025`。包一层只是重复调用者已知的信息时，直接返回原错误，别造噪音。

实测两处正例（都是把下层错误原样上抛，不做 `fmt.Errorf` 包装）：

- `internal/cluster/register.go:14` —— `return err`
- `internal/cluster/client.go:48` —— `return err`

判据：包一层之后**错误字符串是否变得更可诊断**。变不了就别包。

### 16. 错误送进别的系统（日志 / 监控）时要一眼可辨

上游 `uber-029`：错误以非 `error` 类型到达其他系统时，要能看出它是错误 —— 日志里给 `err=` 标签，文案上给 "Failed" 之类的标识。

实测同一文件里两种做法并存，**好例子在 `internal/storage/compact.go:72`**：

```go
log.Printf("compaction: moved=%d err=%v", moved, err)
```

`err=%v` 标签让人能直接 grep `err=` 捞出所有错误行。反例是 `cmd/taihu/cmd/server.go:183` 的 `log.Printf("GetDiskCapacity: %v", err)` —— 值打了，但除了位置没有别的标记表明这是错误。

新写日志时用 `err=%v` 形态。`log.Printf` 的通道约束见上文规则 11。

---

## 规模与复杂度

### 17. 文件与函数都往小里拆；具体阈值

上游 `ecc-024`（源文件 200–400 行典型，800 行软上限；测试/生成/厂商文件豁免）与 `ecc-030`（长函数拆成职责单一的小块）。

仓库现状 —— **这两条是"写规则 + 标注既有例外"的典型**：

| 维度 | 现状 | 上游建议 | 处置 |
|------|------|----------|------|
| 单文件行数 | `internal/device/device.go` **813 行**，为全仓最大 | 800 行软上限 | 已越界，标注为例外；**新增文件照 800 行以内来** |
| 单函数行数 | `internal/device/device.go:439` 的 `AppendBatch` **133 行**（至 `:571`，下一个 `func` 在 `:572`） | 拆成职责单一小块 | 已知例外，同上 |

`internal/device/device.go` 是单一完成泵的实现（见 `.trellis/spec/engine/`），其结构由"一个 goroutine 串行处理所有完成事件"的设计决定，不宜机械拆分 —— **这是可验证的理由，不是"来不及拆"**。新代码不要拿它当先例。

### 18. KISS / YAGNI：清晰优先于机巧，不为没有调用方的需求建抽象

上游 `ecc-021`（选能用的最简单方案，避免过早优化，清晰优先于机巧）与 `ecc-023`（不预先构建用不到的抽象）。

**正例（本仓库最值得引用的一条）** —— `internal/transport/client.go:117-120` 记录了**主动放弃**一条更快路径的决策：

```go
// 曾用过 netpoll 的零拷贝移交 TakeTry（整响应恰一帧时直接移交收流节点缓冲，零拷贝）。
// 实测该路径存在静默数据错配（移交后整块缓冲被归还池并复用，内容被后续收流覆盖），
// 仅收紧「帧独占整块」（base==0）仍会复现，故 TCP 路径已停用，netpoll 侧保留
// base==0 护栏与单测；shm 数据面的单帧移交不受影响（recordRxDataFrame 的 zeroCopy）。
```

性能优化必须先有**实测**支撑，且要像这样把否决理由留在代码里 —— 否则下一个人会把同一条路径再加回来。

**YAGNI 正例**：

- `internal/cluster/kv.go:8` —— `// KV 是集群注册与索引所需的最小存储接口。` 接口只放当下调用方真正需要的方法
- `internal/transport/batch.go:15` —— 明确写下**不做**的事：`// （与 shmBatchReader.run 一致，不做主动关闭；Server 停机路径不依赖队列排空）`

新增接口 / 选项 / 抽象前先问有没有调用方；没有就等有了再加（`internal/storage/options.go:5-7` 的变参 Option 就是为"后加选项不改签名"准备的，见 `.trellis/spec/architecture/go-style.md` 的 Functional Options 规则）。

---

## 常量与零值

### 19. 魔法数字改具名常量

上游 `ecc-029`。

正例 —— `internal/layout/layout.go:14`：

```go
const BlockSize int64 = 4096
```

反例（同类的魔数直接写死在表达式里）—— `internal/metastore/meta.go` 的序列化偏移：

- `:31` —— `binary.LittleEndian.PutUint64(b[1:], uint64(m.SegmentID))`
- `:33` —— `b[17:]` 与 `:43` 的 `b[9:17]`

这三处的 `1` / `9` / `17` 是记录布局的字段偏移，与 `internal/layout/layout.go` 里的常量是同一类东西。**新代码按 `internal/layout/layout.go:14` 的形态写**；`internal/metastore/meta.go` 现状标注为例外，不在本轮改动范围。

### 20. 让零值可用，省掉一半构造逻辑

上游 `gbp-037`：利用零值可用性简化代码，并让自定义类型的零值有意义。

正例（本仓库已在用）：

- `internal/cluster/kv_mem.go:12` —— `mu sync.RWMutex`，零值即可用，无需构造函数初始化
- `internal/storage/options.go:16-17` —— `defaultOptions()` 只显式设置真正需要非零值的字段，其余留给零值：

```go
func defaultOptions() options {
    return options{aioMode: aio.ModeAuto}   // aioIOPoll 留零值 false，正是想要的默认
}
```

**判据**：如果一个字段的默认值恰好是它的零值，就不要在构造函数里再写一遍 —— 那行代码不表达任何意图，却让人以为它很重要。选项的 `WithXxx` 形态见 `.trellis/spec/architecture/go-style.md`。

---

## 元原则

### 21. 一致性优先于任何单条规则

上游 `uber-063` 原文："Above all else, **be consistent**." 上游同时自我声明：部分规则是情境性、主观的（`uber-064`）。

这条不是免责条款，是**排序规则**：本文件两条写法并存时（如规则 8 的 op 前缀两套形态、规则 16 的日志两种做法），**按既有邻近代码的写法来**，不要在同一个文件里引入第三种。真觉得既有写法错了，先改既有代码，再改规则 —— 不要新老混着长。

本文件自身即为示范：规则 8 没有把 `DialPoolMulti` 一处例外抹平成"必须小写"，也没有放任两套混用，而是把边界（前缀指代 Go 函数还是业务动作）写清楚 —— 规则要描述真实状况，不是描述理想状况。
