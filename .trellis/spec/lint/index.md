# lint —— 静态检查（gbp 类 9）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/lint-*.md`，5 条。
> 2 条适用 / 3 条不适用。
>
> **这一层与其它层最大的不同：本层的"适用"意味着"已被 Makefile 强制"。** 下面每一条都标注了它是**门禁**还是**约定**——这个区别决定了你在本地该怎么跑它。

## 本仓库实际启用的检查（只有三个）

`check` 目标本体只有两行依赖加一行 vet（`Makefile:21-22`）：

```make
check: check-fmt check-layering check-sdk-only   # Makefile:21
	go vet ./...                                    # Makefile:22
```

- `check-fmt`：`gofmt -l .` 后 `grep -v '^third_party/'`，有输出即 exit 1（`Makefile:25-28`）；
- `check-layering`：grep 源码查 `internal/` 反向依赖 `pkg/`，唯一白名单 `pkg/ierr`（`Makefile:52-58`）；
- `check-sdk-only`：grep 源码查 `pkg/` 非测试代码依赖存储引擎内部包（`Makefile:60-66`）；
- `go vet ./...`：本机（darwin）全量 vet（`Makefile:22`）；
- 另有**不挂在 `check` 依赖里、需手动跑**的跨平台目标 `check-linux`：GOOS=linux 的 `go vet` + `go build`（amd64/arm64）+ `go test -c`（`Makefile:73-78`）。

两条 grep 门禁各自查什么、为什么这么查，见下面的[两条分层门禁的语义](#两条分层门禁的语义grep-实现2026-10-02-核对)。

**全仓库对 `golangci-lint` / `staticcheck` / `revive` 的提及数为 0**（已核实 `grep -rniE 'golangci|staticcheck|revive'` 覆盖 `Makefile` / `*.yml` / `*.yaml` / `*.md` / `*.sh`），三者**均未安装**。这不是遗漏，见下面各自条目的理由。

## 两条分层门禁的语义（grep 实现，2026-10-02 核对）

两条门禁的设计理由直接写在 Makefile 注释里：`pkg/` 只放两类包——对外 SDK（`pkg/taihu-client`）与引擎/SDK 共享的错误唯一事实源（`pkg/ierr`），其余一律下沉 `internal/`（`Makefile:30-36`，注释里还记录了 `bufpool` 2026-10-02 从 `pkg/` 迁回 `internal/bufpool` 的经过）；两个方向都要守，少一条边界就会被绕回来（`Makefile:38-42`）。两条都**刻意 grep 源码而不用 `go list`**：`go list` 在 darwin 上看不见 `//go:build linux` 的文件，会「本机通过、Linux 上炸」；grep 对 build tag 无感，代价是注释/字符串里的路径字面量会误报（`Makefile:44-47`）。

**check-layering（`Makefile:52-58`）——守「`internal/` 不得反向依赖 `pkg/`」。** 实际命令是 grep `internal/` 下全部 `*.go` 中的 `"github.com/liucxer/taihu/pkg/` import（`Makefile:53`），再 `grep -v` 掉唯一白名单 `"github.com/liucxer/taihu/pkg/ierr"`（`Makefile:54`）。违反即成环，也说明引擎代码又爬回了对外目录。新增的 HTTP 管理面同样守这条：`internal/web/web.go:19` 与 `internal/web/web_test.go:17` 对 `pkg/` 的唯一依赖就是白名单内的 `pkg/ierr`。

**check-sdk-only（`Makefile:60-66`）——守「`pkg/` 库代码不得依赖存储引擎」。** 禁止集正则写死在 `Makefile:61`：`internal/{storage,device,aio,bufpool,layout}` 五个引擎内部包；随后 `grep -v '_test.go'` 排除全部测试文件（`Makefile:62`）。测试例外的理由见 `Makefile:49-51`：同模块端到端接线测试需要起真实引擎 + server，合法。当前**唯一**实际命中该例外的文件是 `pkg/taihu-client/storage_multiaddr_test.go`：它在 `pkg/taihu-client/storage_multiaddr_test.go:12-15` import 了 `internal/storage` 与 `internal/layout`（外加不在禁止集里的 `internal/cluster`、`internal/transport`）——其中 `storage`、`layout` 正是禁止集成员，靠 `_test.go` 排除才不挡门禁。

**写代码时容易踩的旧路径（已不存在，勿再引用）**：`pkg/bufpool` 已迁回 `internal/bufpool`；没有 `pkg/rpcclient`，RPC 客户端在 `internal/rpcclient`；错误事实源是 `pkg/ierr`，不存在 `internal/ierr`。`internal/aio` 的平台文件名也已改：现在是 `ring_libaio_linux.go` / `ring_fallback_other.go` / `ring_uring_linux.go`，旧名 `aio_libaio_linux.go`、`aio_fallback_other.go`、`aio_uring_params_linux.go`、`aio_internal.go` 均已不存在。

## 逐条裁决

### gbp-049 · Code Formatting（MEDIUM）— **适用 · 门禁**

**规则**：`gofmt` / `goimports` 统一格式；提交前格式化；CI 中校验。

**对 taihu：适用，已由 `make check-fmt` 强制。**

```make
# Makefile:24 注释：third_party/ 是上游 fork，保持与上游一致的格式，不纳入本地 gofmt 校验。
check-fmt:                                                                   # Makefile:25
	@bad=$$(gofmt -l . | grep -v '^third_party/' || true); \                 # Makefile:26
	if [ -n "$$bad" ]; then echo "以下文件未通过 gofmt："; echo "$$bad"; exit 1; fi; \  # Makefile:27
	echo "check-fmt: OK"                                                      # Makefile:28
```

**要点一：`third_party/` 的豁免是刻意的。** Makefile 里写明了理由——「`third_party/` 是上游 fork，保持与上游一致的格式，不纳入本地 gofmt 校验」（`Makefile:24`）。**不要为了"让 check-fmt 覆盖全部"而格式化 `third_party/`**：那会让 fork 与上游产生无意义的 diff，后续回迁上游补丁时每处都冲突。顺带一个 2026-10-02 的实测数据：`third_party/shmipc-go/` 也是全仓**唯一** import `stretchr/testify` 的地方——`grep -rl 'stretchr/testify' --include='*.go' .` 命中 13 个文件全部在该目录下（上轮核对为 10 个，随 fork 回迁上游测试增长），自有代码 0。

**要点二：只查 `gofmt`，不查 `goimports`。** `gofmt` 管格式，`goimports` 额外管 import 分组与增删。本仓库没接 `goimports`，所以 import 的分组顺序是**约定**不是门禁。既有代码的写法（标准库一组 / 第三方一组）按现状跟随即可。

**⚠️ 版本风险（2026-10-02 重测）：PATH 上的 gofmt 已与工具链同源，剩下的风险是「将来 gofmt 输出规则变化导致门禁红」。**

实测本机：

| 项 | 值 | 取证命令 |
|---|---|---|
| 工具链版本 | `go1.26.1 darwin/arm64` | `go version` |
| `go.mod` 语言版本 | `go 1.23.0`（无 `toolchain` 指令） | `go.mod:3` |
| `go env GOROOT` | `/opt/homebrew/opt/go/libexec`（Homebrew Go 1.26.1） | `go env GOROOT` |
| PATH 上的 `gofmt` | `/opt/homebrew/opt/go/libexec/bin/gofmt`；`which -a` 的第二个 `/opt/homebrew/bin/gofmt` 是指向同一 1.26.1 的软链 | `which -a gofmt` |

`make check-fmt` 调的是**裸 `gofmt`**、走 PATH（`Makefile:26`），命中的就是 GOROOT 内这一个 1.26.1 gofmt，编译与格式化同源。**旧版 spec 记录的「PATH 上 gofmt 1.23.3 与工具链 1.25.0 不一致」风险已随环境更换消失**；`go.mod` 的语言版本也已降为 `go 1.23.0`（`go.mod:3`），注意语言版本**不锁定** gofmt 的输出规则——门禁只认 PATH 上这一个 gofmt。

**当前门禁绿**：2026-10-02 实测 `gofmt -l .` 经 `grep -v '^third_party/'` 后零输出（等价地，`gofmt -l internal cmd pkg` 也零输出，exit 0）。

**剩余风险只有一个**：gofmt 的格式化规则会随工具链版本变化（如 Go 1.19 改过文档注释格式），将来升级 Go 后可能出现「昨天还绿的代码今天 `make check` 在 check-fmt 变红」。**处置一律以 `make check` 打印的文件清单为准**：对清单文件跑 `gofmt -w` 后整组提交，不要手改格式，也不要为了消红把自有目录加进 `third_party/` 式豁免。升级工具链后顺手先跑一次 `make check-fmt` 确认即可，无需提前改 PATH。

### gbp-050 · golangci-lint Configuration（HIGH）— **不适用（未引入）**

**规则**：用 `.golangci.yml` 配置聚合 linter；启用 `errcheck` / `govet` / `staticcheck` / `ineffassign` / `unused`、`gofmt` / `goimports`；设 `issues.exclude-rules`；CI 中 `golangci-lint run`。

**对 taihu：不适用（未引入）。** 仓库无 `.golangci.yml` / `.golangci.yaml`，全仓零提及，本机未安装。

**为什么没引入（这是判断，不是既成事实的转述）**：`golangci-lint` 的价值在于**把多个 linter 聚合起来并允许按项目裁剪规则**。本仓库当前的三个检查各有明确的取舍理由（见 gbp-049 / 051），而且 `check-layering` / `check-sdk-only` 这两条**是 grep 实现的、`golangci-lint` 表达不了的**——它们检查的是跨包 import 方向，不是单个文件内部的模式。

**所以引入 `golangci-lint` 不是"补齐缺的 linter"，而是一次需要论证的取舍**：它带来的主要是 `errcheck` 与 `staticcheck` 的覆盖面。**若引入，必须同时说明它和现有 `go vet` 的职责边界**——两套并存比用哪一套更糟。

**一条与本仓库直接相关的判断依据**：`third_party/` 是 fork，任何"全仓扫描"的 linter 都必须先排除它（同 gbp-049 的豁免理由），否则 fork 的上游风格会持续报错。

### gbp-051 · Static Analysis（HIGH · govet）— **适用 · 门禁**

**规则**：用 `go vet` 做静态分析，CI 中必跑；重点检查 `printf` 格式串、`lostcancel`、`copylocks`、`nilness`、`shadow` 等。

**对 taihu：适用，已由两条 Makefile 目标强制。**

```make
# 本机（darwin）：
check:      go vet ./...                                        # Makefile:22
# 跨到 linux（Makefile:73-78，四行命令）：
check-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./...           # Makefile:74
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...         # Makefile:75
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...         # Makefile:76
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o /dev/null ./...  # Makefile:77
```

**要点一：`check-linux` 里的 `go vet` 是必须的，不是重复。** Makefile 的注释解释了原因——本仓库有大量平台专有代码（`unix.POLLIN` 仅 linux 存在、`Syscall6` 参数个数、结构体 size/offset 断言、`_test` 后缀必须排在 GOOS 之后，见 `Makefile:68-72`），**这些错误在 macOS 上编得过、只在 linux 上暴露**。在 darwin 上跑 `go vet ./...` **看不见** `//go:build linux` 的文件。

2026-10-02 核对，平台专有代码当前全部围绕「shmipc-go 仅支持 Linux」这一事实做拆分，真实文件清单：

| 无 tag 主文件调用的符号 | Linux 实现（文件名后缀 `_linux.go` 即隐式约束） | 非 Linux 占位 |
|---|---|---|
| shm 服务端 | `internal/transport/server_shm_linux.go:12`（`package transport`） | `internal/transport/server_shm_other.go:1`（`//go:build !linux`） |
| shm 客户端 | `internal/transport/client_shm_linux.go:8`（仅 Linux；只被同样 linux-only 的拨号代码引用，故无 other 占位） | — |
| RPC 拨号连接池 | `internal/rpcclient/dial_shm_linux.go:3` | `internal/rpcclient/dial_shm_other.go:1` |
| bench single 拨号 | `cmd/taihu/cmd/bench_single_shm_linux.go:1`（显式 `//go:build linux`） | `cmd/taihu/cmd/bench_single_shm_other.go:1`（`//go:build !linux`，新增） |

**真实案例（2026-10-02）**：无 build tag 的 `cmd/taihu/cmd/bench_single.go:79` 曾直接调用 linux-only 的 `transport.DialShm`（定义在 `internal/transport/client_shm_linux.go`），darwin 编译断裂、`make check` 在本机才看得见。修复就是把这一调用按平台拆成 `dialShmBenchStore`：linux 版在 `cmd/taihu/cmd/bench_single_shm_linux.go:14-15` 转调 `transport.DialShm`，非 linux 版在 `cmd/taihu/cmd/bench_single_shm_other.go:13-14` 返回 `ierr.ErrShmUnsupported`（哨兵定义于 `pkg/ierr/ierr.go:46-47`），主文件始终无 tag。**改 shm 拨号链时照这个模式拆，不要给主文件加 tag。**

**改动任何平台相关代码后，`make check` 全绿不足以判定通过——必须再跑 `make check-linux`。**

**要点二：`lostcancel` 与本仓库的具体关联**见 [concurrency/](../concurrency/index.md) 的 gbp-025——`WithTimeout` / `WithCancel` 的 `cancel` 必须 `defer cancel()`。

**要点三**：`go vet` **不检查未使用的 error 返回值**（那是 `errcheck` 的职责）。所以「丢弃 error 必须有理由」这条**靠评审与 [error/](../error/index.md) 的 gbp-019 保证，不靠工具**。

### gbp-052 · Customizable Linter（MEDIUM · revive）— **不适用（未引入）**

**规则**：用 `revive` 替代 `golint`，通过 `revive.toml` 逐条启停规则并设严重级别。

**对 taihu：不适用（未引入）。** 全仓零提及，未安装。

**关联说明**：`revive` 覆盖的很大一块是**命名与注释的规范**（导出符号必须有注释、命名风格、接收者命名等）。本仓库这些领域**已经有书面规则**，见 [idiomatic/](../idiomatic/index.md) 的 gbp-029 / 030 / 032——它们由 spec 与评审保证，不由 linter 保证。

**这个分工是刻意的**：本仓库的命名规则比 `revive` 的默认规则**更具体**（如「同一类型的接收者字母必须恒定」这种跨方法的一致性，`revive` 判不了）。**引一个判不了你想判的东西的 linter，只会产生噪声。**

### gbp-053 · Advanced Static Checking（HIGH · staticcheck）— **不适用（未引入）**

**规则**：用 `staticcheck` 做深度静态分析；`SA*` 抓真实 bug（如 `defer` 里参数求值、无效的 `time.Sleep`）、`ST*` 抓风格、`QF*` 是快速修复建议。

**对 taihu：不适用（未引入）。** 全仓零提及，未安装。

**这是本层最值得单独说明的一条**，因为 `staticcheck` 检出的 `SA*` 类**是真实 bug 而 `go vet` 抓不到**。当前没引入意味着**这类缺陷本仓库没有工具兜底**。

**它与 `internal/aio` 的具体关联**：`SA*` 里有一条针对 `unsafe.Pointer` 转换的检查，而本仓库 `internal/aio` 的 Linux 路径有大量 `unsafe` 用法（结构体 size/offset 断言、`uringRing` 字段布局、mmap 共享内存）。**`staticcheck` 在这类代码上会产生误报**，这是引入前必须实测的——不要假设"开了就全是收益"。

**若引入**，与 gbp-050 同理：先排除 `third_party/`，并说明与 `go vet` 的职责边界。

---

## 本层结论：门禁 vs 约定

| 条目 | 状态 | 谁保证 |
|---|---|---|
| gbp-049 gofmt | ✅ **门禁** | `make check` → `check-fmt` |
| gbp-051 go vet | ✅ **门禁** | `make check` + `make check-linux` |
| gbp-050 golangci-lint | ⛔ 未引入 | 需先论证与 `go vet` 的边界 |
| gbp-052 revive | ⛔ 未引入 | 命名/注释规则由 spec 与评审保证 |
| gbp-053 staticcheck | ⛔ 未引入 | **`SA*` 类缺陷当前无工具兜底** |

**"未引入"在这里是决策，不是待办。** 但它们也不是永久否决——引入任一条都需要先回答上面写的那个问题（与现有检查的职责边界、`third_party/` 的排除、`unsafe` 区域的误报）。

## 本地怎么跑

```bash
make check          # gofmt + 两条分层门禁 + 本机 go vet
make check-linux    # 跨平台编译核对（改了平台相关代码后必跑）
```

**改过 `internal/aio` / `internal/device` / `internal/rpcclient` / `internal/transport` / `internal/web` / `cmd/taihu/cmd` 的任何一处，两条都要跑。** 后两个是 2026-10-02 补进清单的：HTTP 管理面 `internal/web` 全部文件无 build tag（如 `internal/web/web.go:7`），其接线方 `cmd/taihu/cmd/web.go:9` 同样无 tag，darwin/linux 两边都编译，一边漏编就是平台事故；`cmd/taihu/cmd` 则是刚发生过 `bench_single.go` 直接调 linux-only 符号导致 darwin 断裂的地方（案例见上面 gbp-051）。只跑 `make check` 是本仓库最常见的一类"本地绿、Linux 炸"。
