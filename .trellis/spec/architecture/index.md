# 架构层约定

> 本层管 taihu 仓库的**结构性不变量**：目录分层与依赖方向、包的对外面、全仓库代码风格、错误体系、提交信息格式 —— 这些规则跨包生效，任何一次改动都可能踩到。

---

## 本层文件

| 文件 | 用途 |
|------|------|
| [repo-layout.md](./repo-layout.md) | **顶层目录的清单与归属**：对照 golang-standards/project-layout 逐项核对（采用 9 / 不适用 10），每项说明为什么；**新增顶层目录前的判据表**；唯一既有例外 `doc/` 而非 `docs/`。与 layering.md 的分工：那边管依赖方向，这边管目录本身 |
| [layering.md](./layering.md) | 依赖方向不变量：`internal/` 不得 import `pkg/`、SDK 不得直接依赖存储引擎；两条 `make` 门禁的实现与理由；包依赖全貌 |
| [api-surface.md](./api-surface.md) | 包的对外面：标识符默认私有、只被测试用的删掉、**对外面文件里只有导出内容**（未导出项一律迁出，有机械判据）；删符号的**判据是「可达」不是「被引用」**（配置取值 / 返回值类型 / 接口满足三条都够不着才是真死代码）；**边界上不交出内部状态**（返回内部容器给副本，与 `bufpool` 的显式归还契约例外）；**接受接口、返回结构体**（11 个构造函数里 10 个返回具体类型）；**依赖走构造函数**（59 处包级 `var` 的分类清单） |
| [code-style.md](./code-style.md) | 全仓库代码约定：中文注释与「为什么」取向、命名（receiver 单字母、首字母缩写全大写）、错误包装措辞（op 前缀按场合分两套）、三条日志通道、import 分组；**错误去向**（96 处 `_ =` 的丢弃纪律、无上下文时原样返回、送进日志要可辨）；**规模与复杂度**（800 行 / 文件、`internal/device/device.go` 的两个已标注例外、KISS/YAGNI）；**常量与零值**（魔法数字、零值可用）；**一致性优先**元原则 |
| [go-style.md](./go-style.md) | **Go 语言层规则**（与 code-style 的分工：那边管本仓库特有约定，这边管语言本身的规则怎么落地）：接收者与接口满足性、mutex 不内嵌、入参防御性拷贝、`defer`、channel 缓冲容量、枚举零值、时间一律走 `time` 包、类型断言 comma-ok、内嵌意图、`init()` 约束、`strconv`/原始字符串、容器容量提示、声明分组、包名与导入别名、文件内声明顺序、减少嵌套、变量作用域、nil slice、裸参数、初始化形态、Functional Options |
| [error-model.md](./error-model.md) | 错误体系：`internal/ierr` 唯一事实源、包内未导出 sentinel、跨进程传 code 不传字符串、`internal/rpcclient` → `pkg/taihu-client` 的 re-export 链；**每个错误只处理一次**（log 与 return 二选一，判据是有无调用方）、**四种处置方式按契约选**、`%w` 是默认且被包装的错误要文档化并测试（149 处 `errors.Is` 断言） |
| [commits.md](./commits.md) | 提交信息：`type(scope): 中文标题` + 中文 bullet 正文 + 纯重构的「行为不变：」核对段 |

---

## Pre-Development Checklist

动手前逐条自检：

- [ ] 我知道这次改动落在哪一层吗？先读 [layering.md](./layering.md) 确认新代码该进 `internal/` 还是 `pkg/`。
- [ ] 我要新增的 import 会不会打破方向不变量？改完必须 `make check-layering` 与 `make check-sdk-only` 都能过。
- [ ] 我新增的标识符真的需要导出吗？删除某个已有标识符时，我是按**引用计数**还是按**可达性**判的？先读 [api-surface.md](./api-surface.md) —— `internal/aio` 的 `ModeLibAIO` 与 `Info` 在生产代码里零引用，却都删不得。
- [ ] 我往包的对外面文件（如 `internal/aio/aio.go`）里塞私有 helper 了吗？那里只应出现导出名 —— 未导出的常量 / 变量 / 函数要另立文件，判据见 [api-surface.md](./api-surface.md) 规则 4。
- [ ] 我要加的日志走哪条通道？先读 [code-style.md](./code-style.md) 的「日志三通道」，不要新开第四种。
- [ ] 我这次写的 Go 代码踩到语言层的坑了吗？加/改方法接收者、`defer`、`make(chan` 给容量、用 `time` 类型、写 `init()`、类型断言、内嵌、`nil` slice 判空 —— 任一命中就去读 [go-style.md](./go-style.md) 对应规则。
- [ ] 我要返回的错误是库错误（`internal/ierr` 的 sentinel）还是包内控制信号（未导出 sentinel）？见 [error-model.md](./error-model.md)。
- [ ] 我要暴露给外部调用方的类型出现在导出签名里吗？若是，去 `internal/rpcclient/reexport.go` / `pkg/taihu-client/reexport.go` 补 alias。
- [ ] 改动了平台相关代码（`_linux` / `_other` 后缀文件）吗？本机是 darwin，务必跑 `make check-linux`。
- [ ] 提交信息准备好了吗？格式见 [commits.md](./commits.md)；纯重构请预留「行为不变：」核对段。

---

## Quality Check

改完在仓库根目录执行：

```bash
make check        # check-fmt + check-layering + check-sdk-only + go vet ./...
make check-linux  # 平台专有代码改动后必跑（darwin 编得过不代表 linux 编得过）
```

`make check` 的四个子目标全部以 `OK` 结尾才算过：

```
check-fmt: OK
check-layering: OK
check-sdk-only: OK
go vet ./...
```

分层相关的单点复核（门禁之外，用来确认自己没绕过）：

```bash
# internal/ 不得 import pkg/ —— 应无输出
grep -rn '"github.com/liucxer/taihu/pkg/' --include='*.go' internal/

# pkg/ 非测试代码的 taihu 内部依赖 —— 应只有 internal/cluster、internal/rpcclient、internal/version
grep -rn '"github.com/liucxer/taihu/' --include='*.go' pkg/ | grep -v '_test.go'
```

`make check-linux` 覆盖 darwin 看不见的部分（`//go:build linux` 文件、`unix.POLLIN`、`Syscall6` 参数个数、结构体 size/offset 断言），定义见 `Makefile:60-70`。

### 收尾前的自检单

门禁过了不等于收尾完成 —— 下面几条机器查不出来，逐条过（上游 `ecc-031`）：

| # | 检查项 | 阈值 | 本仓库现状 |
|---|--------|------|------------|
| 1 | 单函数行数 | < 50 行 | ⚠️ 已知例外：`internal/device/device.go:439` 的 `AppendBatch` 133 行。**新函数照 50 行来**，超过就拆 |
| 2 | 单文件行数 | < 800 行 | ⚠️ 已知例外：`internal/device/device.go` 813 行。**新文件照 800 行以内来** |
| 3 | 嵌套层数 | ≤ 4 层 | 先处理错误与特殊分支、提前返回 —— 见 [go-style.md](./go-style.md) 规则 18 |
| 4 | 错误都处理了 | 无静默吞掉 | 丢弃必须 `_ =` + 理由 —— 见 [code-style.md](./code-style.md) 规则 14 |
| 5 | 无硬编码值 | 无魔数 / 无凭据 | 见 [code-style.md](./code-style.md) 规则 3 与规则 19 |
| 6 | 无意外 mutation | 边界上不交出内部状态 | 见 [api-surface.md](./api-surface.md) 规则 6 |

第 1、2 项的例外**是登记在册的，不是可以再扩的** —— 新增代码不再享受同样豁免。

### 「通过」的标准

**没有 CRITICAL、也没有 HIGH 问题才算通过**（上游 `ecc-053`）。`make check` 全绿只是必要条件：它覆盖不了上表 6 条，也覆盖不了语义正确性。发现 CRITICAL / HIGH 时**先修再报完成**，不要带着已知问题交接。

### 工具链现状（已知缺口，未采纳）

上游 `uber-133` / `uber-134` 建议至少引入 `errcheck` / `goimports` / `revive` / `staticcheck`，并用 `golangci-lint` 作为 runner。**本仓库目前都没装**：

| 上游建议 | 本仓库现状 |
|----------|------------|
| `govet` | ✅ `Makefile:22` 的 `go vet ./...` 已在 `make check` 里 |
| `goimports` | ⛔ 用 `gofmt -l` 替代（`Makefile:26`）—— 只管格式，不管 import 分组与增删 |
| `errcheck` | ⛔ 无。**这是与 [code-style.md](./code-style.md) 规则 14 最相关的一个**：96 处 `_ =` 目前靠人守 |
| `revive` / `staticcheck` | ⛔ 无 |
| `golangci-lint` | ⛔ 无 `.golangci.yml`，lint 入口就是 `make check` |

**本轮明确不引入**（见任务 `09-21-spec-upstream-alignment` 的「不做的事」）。若将来引入，`errcheck` 应当**最先** —— 它能把规则 14 从"靠人守"变成"门禁挡"，且需要先为既有的 96 处 `_ =` 写好豁免配置。

⚠️ spec 禁止 `//nolint`（[code-style.md](./code-style.md) 规则 3），所以引入 linter 时要同步定下"告警怎么处置"—— 不能靠就地压掉。
