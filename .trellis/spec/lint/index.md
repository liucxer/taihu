# lint —— 静态检查（gbp 类 9）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/lint-*.md`，5 条。
> 2 条适用 / 3 条不适用。
>
> **这一层与其它层最大的不同：本层的"适用"意味着"已被 Makefile 强制"。** 下面每一条都标注了它是**门禁**还是**约定**——这个区别决定了你在本地该怎么跑它。

## 本仓库实际启用的检查（只有三个）

```make
check: check-fmt check-layering check-sdk-only
	go vet ./...

check-fmt:      # gofmt -l . 排除 third_party/，有输出即 exit 1
check-linux:    # GOOS=linux 的 go vet + go build(amd64/arm64) + go test -c
```

**全仓库对 `golangci-lint` / `staticcheck` / `revive` 的提及数为 0**（已核实 `grep -rniE 'golangci|staticcheck|revive'` 覆盖 `Makefile` / `*.yml` / `*.yaml` / `*.md` / `*.sh`），三者**均未安装**。这不是遗漏，见下面各自条目的理由。

## 逐条裁决

### gbp-049 · Code Formatting（MEDIUM）— **适用 · 门禁**

**规则**：`gofmt` / `goimports` 统一格式；提交前格式化；CI 中校验。

**对 taihu：适用，已由 `make check-fmt` 强制。**

```make
check-fmt:
	@bad=$$(gofmt -l . | grep -v '^third_party/' || true); \
	if [ -n "$$bad" ]; then echo "以下文件未通过 gofmt："; echo "$$bad"; exit 1; fi
```

**要点一：`third_party/` 的豁免是刻意的。** Makefile 里写明了理由——「`third_party/` 是上游 fork，保持与上游一致的格式，不纳入本地 gofmt 校验」。**不要为了"让 check-fmt 覆盖全部"而格式化 `third_party/`**：那会让 fork 与上游产生无意义的 diff，后续回迁上游补丁时每处都冲突。

**要点二：只查 `gofmt`，不查 `goimports`。** `gofmt` 管格式，`goimports` 额外管 import 分组与增删。本仓库没接 `goimports`，所以 import 的分组顺序是**约定**不是门禁。既有代码的写法（标准库一组 / 第三方一组）按现状跟随即可。

**⚠️ 一个潜在风险：PATH 上的 `gofmt` 与工具链版本不一致。**

实测本机：

| 项 | 值 |
|---|---|
| `go version` | `go1.25.0` |
| `go.mod` 的 `go` 指令 | `go 1.25.0` |
| `go env GOROOT` | `.../toolchain@v0.0.1-go1.25.0.darwin-amd64` |
| **PATH 上的 `gofmt`** | **`/Users/liucx/sdk/go1.23.3/bin/gofmt`** |

`make check-fmt` 调的是**裸 `gofmt`**，走 PATH，所以它用的是 **1.23.3 的 gofmt**，而编译用的是 1.25.0 的工具链。

**当前两者结论一致**（实测两个 gofmt 对本仓库都报告 0 个非规范文件），所以**现在不是故障，是潜在风险**：gofmt 的格式化规则会随版本变化（如 Go 1.19 改过文档注释的格式），一旦某次升级引入规则差异，可能出现「本地 `make check` 绿、CI 红」或反过来。**升级 Go 工具链后顺手复核一次 `make check-fmt` 的两个 gofmt 结论是否仍然一致**，不必现在改 PATH。

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
check:        go vet ./...                                        # 本机（darwin）
check-linux:  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./...  # 跨到 linux
```

**要点一：`check-linux` 里的 `go vet` 是必须的，不是重复。** Makefile 的注释解释了原因——本仓库有大量平台专有代码（`unix.POLLIN` 仅 linux 存在、`Syscall6` 参数个数、结构体 size/offset 断言、`_test` 后缀必须排在 GOOS 之后），**这些错误在 macOS 上编得过、只在 linux 上暴露**。在 darwin 上跑 `go vet ./...` **看不见** `//go:build linux` 的文件。

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

**改过 `internal/aio` / `internal/device` / `internal/rpcclient` / `internal/transport` 的任何一处，两条都要跑。** 只跑 `make check` 是本仓库最常见的一类"本地绿、Linux 炸"。
