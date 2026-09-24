# 修正 `internal/aio` 审计发现的规范偏差

## Goal

对 `internal/aio` 做逐条规范符合性审计后，一次把发现的偏差做平。**先修正规范自身说错 / 说宽 / 说漏的地方，再按修正后的规范修代码与测试** —— 顺序不能反，否则代码会去迁就一条本身有错的规则。

范围已确认（2026-09-22）：**spec + 代码都改**。长函数走**在 spec 里登记例外 + 写明可检验理由**，不拆。

## 来源与现状（已实测，2026-09-22）

### 审计方法

对 `internal/aio` 逐条比对 8 个 spec 层（architecture 6 文件 + engine + platform + testing + guides）的全部规则。派了两个分层只读审计代理（architecture+engine / platform+testing+guides）交叉核对，**代理产出只作线索**，本文件登记的每一条都由我亲自重跑命令复现。审计中撤回过两条自造的假发现，见 Notes。

### 覆盖率基线（可复现）

```bash
go test -coverprofile=/tmp/aio_base.cover ./internal/aio/
```

`go test -cover` 报 **48.2%**；按 profile 逐文件求和得 **66/137 = 48.2%**，逐位吻合。macOS 上实际参与编译的 5 个文件：

| 文件 | 覆盖/语句 | 覆盖率 | 平台 |
|---|---|---|---|
| `aio.go` | 6/36 | 16.7% | **无 gate（平台无关）** |
| `aio_internal.go` | 2/7 | 28.6% | **无 gate** |
| `aio_internal.go` | **0/8** | **0.0%** | **无 gate** |
| `aio_fallback_other.go` | 55/81 | 67.9% | `//go:build !linux` |
| `probe_other.go` | 3/5 | 60.0% | `//go:build !linux` |

**平台无关三文件合计 8/51 = 15.7%。** Linux 专属的 1017 行（`aio_libaio_linux.go` 237 + `aio_uring_linux.go` 662 + `probe_linux.go` 118）在 macOS 上不编译、不进分母。

### 关键事实（每条都已复现）

| # | 事实 | 证据 |
|---|---|---|
| F1 | `TestParseMode` 测的 `ParseMode` **平台无关**，但测试文件带 `//go:build linux` | `aio.go:112` 定义；`internal/aio/aio_linux_test.go:1` 是 `//go:build linux`，`TestParseMode` 在 `:11-44`；调用方只有 `aio.go:139`（env 覆盖）与 `cmd/taihu/cmd/root.go:118` |
| F2 | `ParseMode` 占 5 条语句，macOS 上 0 命中 | profile 中该函数所有块 count=0 |
| F3 | `aio_internal.go` 在 macOS 上是 **0/8** | 消费方 `Probe()` 的 **8 个调用点全部**落在 linux 门控文件：`internal/aio/aio_linux_test.go:101,115,126`、`internal/aio/aio_linux_test.go:87,88`、`aio_backends_linux_test.go:30`（测试侧），`aio.go:165`、`device.go:83`（生产侧） |
| F4 | `AppendBatch` 实际 **126 行**（`:439-564`），规范写「133 行（至 `:571`）」 | 花括号配平：声明 `:439`、闭合 `:564`；`:565` 空行、`:566-571` 是**下一个函数 `ReadAt`（`:572-599`）的文档注释**，被算进去了 |
| F5 | 全仓非测试代码有 **21 个**函数超 50 行，规则 17 的例外表只登记 **1 个** | 花括号配平扫描；最长 `device.go:439 AppendBatch` 126 行 |
| F6 | 规则 14 记的 `internal/aio \| 11` 里 **6 处是 `unsafe.Sizeof` 编译期断言**，真 error 丢弃 5 处 | `aio_uring_linux.go:141-146` 六条断言 vs `:215,263,303,306,309` 五处真丢弃 |
| F7 | 规则 14 的 grep 口径**漏掉一种形态** | `probe_linux.go:29` 的 `defer func() { _ = unix.Close(fd) }()` 中 `_ =` 不在行首，被 `^[[:space:]]*_ =` 漏掉 → 真丢弃实为 **6 处** |
| F8 | §六 自称「实测 35 处 `go` 语句」，**总数准确**，但按锚点只能重建 20 处 | 精算 35 处完全吻合；未被点名的 11 处里 `client.go:28`、`registry.go:74`、`storage.go:94` 形态可归入已有类别，`aio_fallback_other.go:98` **连形态都归不进 6.1–6.5** |
| F9 | §六 末尾自定：「不在这节里的『不遵守』不是偏离，是遗漏 —— 按缺陷处理」 | `buffer-and-concurrency.md:320` |
| F10 | 规则 3 的 7 个计数**混用了两种口径** | 声明范围是「五目录、排除 `third_party`」（`unit-tests.md:57`），但 `t.Run(` 65 / `t.Helper()` 155 / `t.TempDir()` 97 / `t.Cleanup(` 64 都是**含 `third_party` 的整仓数**（五目录实为 55/132/81/57）；同一张表的 `func Benchmark` 写 0、备注说 16 个全在 `third_party` —— **那一行用的是五目录口径** |
| F11 | 规则 7「资源用 `t.Cleanup` 而不是 `defer`」与仓库主流相反 | `defer X.Close()` **90 处** vs `t.Cleanup(` **57 处**（五目录，`*_test.go`）；`internal/aio` 内 18 处 `defer …Close()`、3 处 `t.Cleanup`，同一文件两种风格并存 |
| F12 | 两处锚点 off-by-N | `build-verification.md:63` 引 `aio_libaio_linux.go:10-13` → 实际 `:11-14`；`unit-tests.md:324` 引 `aio_backends_linux_test.go:9-13` → 实际 `:9-11` |
| F13 | 规则 7 的单侧理由不完整 | `file-splitting.md:140` 只说 `aio_uring_linux.go` 被 `newIOUringRing`/`probe` 挡住，漏了该文件另提供 `checkIOPoll`（`:20`）与 `ringQueueDepth`（`:411`），对手在 `probe_other.go:17`/`:25` |
| F14 | 规则 12 的 aio 例外叙述过宽 | `unit-tests.md:321,327` 写「本机根本量不到这个包」「测不到，不是不达标」「不要把 48.2% 当成待补的缺口去追」—— 但平台无关的 51 条语句在 macOS 上**可测**，实测 15.7% |
| F15 | **`maxEvents` 上界在非 Linux 平台上不被拦截** —— 实测 `NewWithOptions(65537, {Mode: ModeLibAIO})` 与 `{Mode: ModeAuto}` **返回 nil error** | `aio_fallback_other.go:32` 只判 `maxEvents <= 0`，而 Linux 两侧（`aio_libaio_linux.go:57`、`aio_uring_linux.go:201`）判 `maxEvents <= 0 \|\| maxEvents > 1<<16`。文档声明的合法区间是 `[1, 65536]`（`aio_internal.go:18`、`aio.go:135`）。临时探针实测后已删 |
| F16 | `internal/aio/aio_linux_test.go` **整体**无法脱离 linux 门控 | 摘掉 tag 后编译失败：`undefined: uringRing`（`:104`、`:147` 引 `*uringRing`，该类型定义在 `aio_uring_linux.go`）。而**单独**摘出 `TestParseMode` 到无 tag 文件则 **PASS** |
| F17 | 修掉 F15 后，`TestNewWithOptionsInvalidMaxEvents` **也能脱离门控** | 临时给 `aio_fallback_other.go:32` 补上界后，把该测试摘到无 tag 文件，macOS 上 **PASS**；补丁已还原，工作区确认干净 |

## Requirements

### R1 —— 平台门控纠正（代码）

依据 F1/F2/F16：`internal/aio/aio_linux_test.go` 的 `//go:build linux` 现在是**一刀切**，既关住了真需要门控的测试，也关住了平台无关的测试。按实测结论逐个处置：

| 测试 | 处置 | 依据 |
|---|---|---|
| `TestParseMode`（`:11-44`） | **移到不带 tag 的新测试文件** | F16 实测：单独摘出后 macOS 上 PASS |
| `TestNewWithOptionsInvalidMaxEvents`（`:47-57`） | **在 R2 完成后移出**（依赖 R2） | F17 实测：修掉上界后 PASS；该测试只调 `NewWithOptions`/`r.Close()`，不引用 linux-only 类型 |
| `TestNewWithOptionsEnvOverride`（`:61-121`） | **留在 linux 门控内** | 引 `*uringRing`（`:106`），该类型 macOS 上不存在 |
| `TestNewWithOptionsAuto`（`:125-`） | **留在 linux 门控内** | 引 `*uringRing`（`:147`） |

- `internal/aio/aio_linux_test.go` 在移走前两个测试后**必须保留文件头注释**，写明「本文件仍需 linux 门控，因为剩余两个测试引用 `*uringRing`（定义在 `aio_uring_linux.go`）」—— 现无任何说明，下一个人会以为它和 `TestParseMode` 一样是误门控。
- 移出的测试放进**新的不带 tag 的文件**，文件名沿用仓内 `<subject>_test.go` 惯例。
- 目标：`aio.go` 覆盖率由 6/36 升到 **≥11/36**（`ParseMode` 的 5 条语句被覆盖），全包 ≥51.8%（R2 完成后更高）。

### R2 —— `maxEvents` 上界的平台一致性（代码，行为变更）

- 修 F15：`aio_fallback_other.go:32` 的 `if maxEvents <= 0` → `if maxEvents <= 0 || maxEvents > 1<<16`，与 `aio_libaio_linux.go:57`、`aio_uring_linux.go:201` 对齐，也与文档声明的 `[1, 65536]` 对齐。
- **这是本任务唯一的行为变更**，必须单独说清影响面：
  - 非 Linux 平台上，`NewWithOptions(65537, …)` 由「静默建出队列」变为「返回 `errInvalidMaxEvents`」；
  - 唯一生产调用方传编译期常量 `aioDepth = 256`（`internal/device/device.go:33,95`），**无生产影响**；
  - 行为变更是**收紧**，把三个平台统一到文档已声明的契约上。
- 该条在 `api-surface.md:232` 被登记为「待裁定的代码缺陷」。本任务裁定为**修**，并把该处登记同步改为「已修，见本任务」。
- 此修复是 R1 中 `TestNewWithOptionsInvalidMaxEvents` 移出门控的**前置条件**，顺序不可颠倒。

### R3 —— `aio_internal.go` 补测（代码）

- 该文件平台无关（无 gate），但当前 **0/8**，成因是 F3：`Probe()` 的全部调用点都落在 linux 门控文件里。
- 补一个**不带 build tag** 的测试，覆盖缓存的真实语义（`aio_internal.go:12-16` 的三件套 + `:23` 的 `probeCached`）：冷缓存穿透、热缓存命中、并发调用。
- **不得为便于测试而改动 `aio_internal.go` 的生产逻辑。** 该文件也**没有**测试缝隙（`probe()` 是平台文件里的函数，不是可替换的包级变量），所以断言必须走仓内**既有的白盒手法**：`internal/aio/aio_linux_test.go:128-139` 已经开了先例 —— 在 `probeMu` 保护下直接读写 `probeInfo`/`probeDone`，用 `t.Cleanup` 还原。本条沿用该手法，不新造缝隙。
- 断言必须**平台无关**（该文件无 gate，测试会在两个平台都跑）：例如「热缓存下预置的哨兵值必须被原样返回」——它同时证明了「没有发生穿透」。

### R4 —— 规则 12 的 `internal/aio` 例外叙述收窄（spec）

- 改写 `unit-tests.md:321-327` 的例外小节，把「本机根本量不到这个包」拆成两半：
  - **Linux 专属的 1017 行**（66%）在 macOS 上不编译、不进分母 —— 这部分确实量不到，属例外，保留；
  - **平台无关的 51 条语句**（`aio.go` + `aio_internal.go` + `aio_internal.go`）**在 macOS 上可测**，当前 8/51 = 15.7%，**受 ≥80% 规则约束，不是例外**。
- 删除或改写「测不到，不是不达标」「不要把 48.2% 当成待补的缺口去追」这类**无限定的**句子 —— 它们与同文件规则 12 的「≥80%」直接矛盾，会压制合理的工作。
- 保留「48.2% 这个数字不能读成测试写得少」这一层意思，但必须把「哪些部分可测」写清楚。
- 本轮 R1/R2/R3 完成后，把实测的新数字回填进该小节与规则 12 的包表。

### R5 —— 规则 17 的例外表准确性（spec）

- 修正 F4：`code-style.md:318` 的 `AppendBatch`「**133 行**（至 `:571`）」→ **126 行（`:439-564`）**；保留「下一个 `func` 在 `:572`」这句（它是对的）。
- 修掉这条错误的**测量口径**：规范应写明「函数跨度按花括号配平计，`end - start + 1`，**不得**量到下一个函数的文档注释」—— 这正是 133 的成因。
- 处理 F5：全仓 21 个超 50 行的函数，例外表只登记 1 个。按已确认的方式 —— **在例外表里逐条登记并写明可检验理由**，不拆函数。`internal/aio` 的三处（`mapUringRing` 77 行 `:222-298`、`submit` 77 行 `:483-559`、`Wait` 56 行 `:589-644`）理由必须具体到「这段为什么不能拆」，照 `device.go` 先例（「单一完成泵的设计决定」）的分量写。
- 例外表同时要写清**判据**：什么样的理由算合格。现状是表里只有一行，读者无从判断下一条该不该进。

### R6 —— 规则 14 的 `_ =` 计数口径（spec）

- 修正 F6：`code-style.md` 的分布表里 `internal/aio | 11` 要拆开 —— 11 处按现 grep 口径命中，其中 **6 处是 `unsafe.Sizeof` 编译期断言、不是 error 丢弃**，真 error 丢弃 5 处（并注明实为 6 处，见下条）。
- 修正 F7：现有 grep `^[[:space:]]*_ =` **漏掉 `defer func() { _ = x.Close() }()` 这类形态**（`probe_linux.go:29`）。补一条并列的 grep 或在正文写明该口径的盲区。
- 正文指出「编译期断言混入本口径」这一**系统性偏差**（不止 aio 一处），避免下一个人把该表的数字当成 error 丢弃总数。

### R7 —— `engine` §六 goroutine 归类补全（spec）

- F8/F9：§六 的 35 处总数是对的，但按锚点只能重建 20 处；其中 **`internal/aio/aio_fallback_other.go:98` 是唯一连形态都归不进 6.1–6.5 的一处**（每次 `Submit` 起一个 goroutine，`Close()`（`:190-196`）只置 `closed = true`、`nil` 掉 `inflight`，**无 join 点**）。
- 二选一，在 `design.md` 里定案并说明取舍：
  - **(a) 归类** —— 在 §6 追加一类，给出可检验理由；
  - **(b) 补 join 点** —— 给 `aio_fallback_other.go` 的 `ring` 加 `sync.WaitGroup`，`Close` 里 `wg.Wait()`，使其落回 6.1 的表。
- 无论选哪条，**顺便点名** `internal/transport/client.go:28`、`pkg/taihu-client/registry.go:74`、`pkg/taihu-client/storage.go:94` 三处 —— 它们形态上可归入已有类别（6.1 / 6.1 / 6.4）却没被点名。
- §六 的锚点约定要统一或写明：现在 6.1 的表锚在 `go` 那一行，紧随其后的 WaitGroup 段落锚在 `WaitGroup` **声明**行（`segments.go:34`、`compact.go:43`、`server_shm_linux.go:46,660`、`run.go:168`、`storage.go:222`、`server.go:270`），两种锚法混用导致「35 处」无法从锚点重建。

### R8 —— 两处锚点 off-by-N（spec）

- F12：`build-verification.md:63` 的 `internal/aio/aio_libaio_linux.go:10-13` → **`:11-14`**。
- F12：`unit-tests.md:324` 的 `aio_backends_linux_test.go:9-13` → **`:9-11`**。
- 修完用 `verify_spec_refs.py` 复验，确认 0 不存在 / 0 越界。该脚本是上一任务 `09-21-spec-upstream-alignment` 的 research 产物（`.trellis/tasks/archive/2026-09/09-21-spec-upstream-alignment/research/verify_spec_refs.py`），**R13 禁止把它提升为仓库工具**，原地调用即可。

### R9 —— 规则 7 的单侧理由补全（spec）

- F13：`file-splitting.md:140` 补上 `checkIOPoll`（`aio_uring_linux.go:20`）与 `ringQueueDepth`（`:411`），并写明它们的对手**不在 `aio_fallback_other.go` 而在 `probe_other.go:17`/`:25`** —— 即「单侧文件的符号可以由**另一个** `_other.go` 兜底」，这层容易被下一个读者误读成「单侧文件可以随便加符号」。

### R10 —— 规则 3 的计数口径（spec）

- F10：`unit-tests.md:57-70` 的表格**混用两种口径**。把 8 个数字按**声明的那一种**（五目录、排除 `third_party`）重算为 **0 / 0 / 0 / 0 / 55 / 132 / 81 / 57**，或保留整仓数并把 `:57` 的范围句改成实际口径并说明**两个口径都给了**。
- 无论选哪种，**必须把 `func Benchmark` 那行的 0 与整仓 16 的关系写清**（现备注已说明，但同一张表里两个口径并存会让读者算不平）。
- 补上可复现命令 —— 这张表现在一条命令都没有，而它正是 `guides/index.md` §二 假阳性 #5「数字来自宽松的 grep」的活体样本。

### R11 —— 规则 7 的 `t.Cleanup` 措辞（spec）

- F11：`unit-tests.md:176` 的「资源用 `t.Cleanup` 释放而不是 `defer`」是**绝对口径**，但仓库主流相反（90 : 57），`internal/aio` 自身也两种并存。
- 二选一（在 `design.md` 里定案）：
  - **(a) 收窄措辞** —— 改成「**helper 里借出并返回给调用方**的资源用 `t.Cleanup`（这样调用方不必写 `defer`）」，与它自己引的 `transport_shm_test.go:71-79`（`shmDial` 借出 `*ShmConn`）严格对齐。改后 aio 现状合规，无需改 18 处代码；
  - **(b) 坚持绝对口径** —— 则必须一次性改全仓 90 处，**不能只改 aio**。
- 倾向 (a)。

### R12 —— 轻量代码偏差（代码）

逐条都是行为不变的改动，但多数落在 `internal/aio` 的热路径或热路径邻域（`Submit`/`Wait`/`submitBatch`），改动后必须过 `make check-linux`，并在提交信息里按 `engine/buffer-and-concurrency.md` §七「热路径上的决策必须有实测支撑」的要求说清这些改动为什么不影响性能。

| 项 | 位置 | 改法 |
|---|---|---|
| `_ =` 无理由 | `aio_uring_linux.go:215`（本文件第一处）、`probe_linux.go:29` | 在第一处上方加一行**通用理由**（清理路径的 `Close`/`Munmap` 失败无补救动作，错误已由主返回值表达），后续同类可省 —— 这正是规则 14 的要求 |
| 位置式结构体字面量 | `aio_uring_linux.go:329-342`（4 字段 × 13 条）、`:385-389`（3 字段 × 4 条） | 改为具名字段。17 条位置式字面量一旦字段顺序调整会**静默错位**，而同包其余 8 处结构体字面量全部具名 |
| 手写 `Unlock` | `aio_libaio_linux.go:121,160,168,174` | 改 `defer r.mu.Unlock()`。同包 `aio_uring_linux.go:489-490` 与 `aio_internal.go:24-25` 已是 `defer` 写法；规则 5 明写「不要以 defer 慢为由回避」 |
| `if/else` 两分支赋同一变量 | `aio_fallback_other.go:107-113` | 让 `if` 只选 syscall，`res = result(n, err)` 提到分支外。规则 18 / `uber-084` 的反例形态一字不差 |
| `mu` 无保护范围注释 | `aio_fallback_other.go:17` | 加 `// 保护 seq / inflight / closed`。同包另两把锁（`aio_libaio_linux.go:49`、`aio_uring_linux.go:158`）都写了 |

### R13 —— 范围约束

- **不改** `internal/aio` 之外的任何生产代码 —— 本任务只修 `internal/aio` 与承载这些规则的 spec 文件。
- **不拆**任何超 50 行的函数（已确认走登记例外）。
- **不改**规则 12 里 `internal/aio` 以外那 16 个包的覆盖率数字。
- **不引入新工具**（linter、覆盖率门禁脚本等）。
- 不因「顺便」修改与本任务无关的既有规则。

### 明确不在本任务范围

审计还浮出若干**存疑项**（规范文本自身有张力、两种读法都成立），本任务只登记、不裁定：

- `aio.go:38,41` 的 `ErrFull`/`ErrTimeout` 是导出 sentinel，与 `error-model.md` §1「`internal/ierr` 是唯一事实源」的首句有张力（该文件列举的消费方不含 aio，且 `api-surface.md:170` 把这类 sentinel 归入「可接受」）。
- `aio.go:136` 用 `Options` 结构体而非本仓主流的 `...Option` 闭包形态。
- `aio.go:186-192` 的 `Info` 三字段（`KernelRelease`/`SQEntries`/`CQEntries`）今天仍零读取 —— 规范早已登记为「待裁定」，本轮**仍未裁定**。
- `aio_uring_linux.go:153` 的 `uringRing` 与 `:200` 的 `newIOUringRing` 同概念两种拼法；`aio_uring_linux.go:302,305,308,568` 的 `[]byte` 与 `nil` 比较。
- `internal/aio` 内有 6 个测试文件同时带 `_<goos>_test.go` 后缀与显式 `//go:build` tag（其中 3 个在 aio）。**这不是违反**（`file-splitting.md` 未禁止），只是与「多数只靠后缀」的现状不同，仅记录。

## Acceptance Criteria

- [ ] `TestParseMode` 位于**不带 build tag** 的测试文件；`TestNewWithOptionsInvalidMaxEvents` 在 R2 完成后同样移出
- [ ] `internal/aio/aio_linux_test.go` 保留文件头注释，写明剩余测试为何仍需 linux 门控（引 `*uringRing`）
- [ ] `aio_fallback_other.go:32` 已加上界校验，与 `aio_libaio_linux.go:57` / `aio_uring_linux.go:201` 一致；`api-surface.md:232` 的「待裁定」已同步改为「已修」
- [ ] `go test -cover ./internal/aio/` 的全包覆盖率由 48.2% 上升；`aio_internal.go` 由 0/8 变为非零；`aio.go` ≥ 11/36
- [ ] `aio_internal.go` 的新测试**不带 build tag**，且在 macOS 与 linux 两个平台上都通过（`make check` + `make check-linux` 覆盖判据）
- [ ] `aio_internal.go` 的生产逻辑**未被改动**（`git diff` 中该文件只有测试文件的新增，没有实现改动）
- [ ] 规则 12 的 aio 例外小节明确区分「Linux 专属 1017 行（量不到）」与「平台无关 51 条语句（可测，受 ≥80% 约束）」，无「不要把 48.2% 当成待补的缺口去追」这类无限定句
- [ ] 规则 12 的包表中的 `internal/aio` 数字与 R1/R2/R3 后的实测一致
- [ ] `code-style.md` 的 `AppendBatch` 记为 **126 行**，且规则正文写明跨度的测量口径（花括号配平、含端行、不含下一函数的文档注释）
- [ ] 规则 17 的例外表覆盖全仓 21 个超 50 行函数，每条有可检验理由，并写明「什么样的理由算合格」的判据
- [ ] 规则 14 的分布表把「编译期断言」与「error 丢弃」分开；正文写明 `^[[:space:]]*_ =` 口径的盲区（漏 `defer func() { _ = ... }()`）
- [ ] `engine/buffer-and-concurrency.md` §六：`aio_fallback_other.go:98` 已归类或已补 join 点；`client.go:28`、`registry.go:74`、`storage.go:94` 已被点名；锚点约定不统一一事已在正文写明
- [ ] 两处 off-by-N 锚点已改（`build-verification.md:63` → `:11-14`；`unit-tests.md:324` → `:9-11`）
- [ ] `file-splitting.md` 规则 7 已补 `checkIOPoll` / `ringQueueDepth` 与其对手位置
- [ ] `unit-tests.md` 规则 3 的表格口径自洽，附可复现命令
- [ ] 规则 7 的 `t.Cleanup` 措辞已定案并改到自洽（收窄 或 全仓统一，二选一）
- [ ] `internal/aio` 的 5 处轻量偏差（R12）全部改完，每条是行为不变的
- [ ] `make check` 与 `make check-linux` 均 exit 0
- [ ] `python3 .trellis/tasks/archive/2026-09/09-21-spec-upstream-alignment/research/verify_spec_refs.py .`：0 不存在 / 0 越界；`To be filled by the team` 占位残留 0
- [ ] `git status --porcelain` 里没有本任务之外的改动
- [ ] 全仓 `go test ./...` 的**失败集合**与改动前完全一致（本机既有 7 个平台性失败，见 `.trellis/spec/platform/build-verification.md:103-108`；不许新增、也不许「顺手」修掉）

## Notes

### 审计中撤回的两条自造假发现（记录在案）

1. **「§6.4 的五个锚点是错的」** —— 我查到 `internal/transport/server.go` 的 `:196/233/240/255/273` 都不是 `go` 语句就下了结论，但规范写的是 **`cmd/taihu/cmd/server.go`**。实际核实：该文件恰好 5 处 `go`，位置全对，`serveWG` 也确在 `:270`。**规范是对的，我查错了文件。**
2. **「`unit-tests.md` 规则 12 的 137 条语句对不上」** —— 我的按文件拆分脚本先在 `cov[f] += c` 里加错了量（加命中次数而非语句数），算出 46/137 就以为规范写错。修成 `cov[f] += st` 后得 66/137 = 48.2%，与 `go test -cover` 逐位吻合。**规范是对的，脚本是错的。**

两条都属同一类：**先怀疑规范、后核对工具**。记在这里是因为本任务的产出正是「修正规范」，如果不写明，下一个人会重复同样的误判。

### R2 是本任务唯一的行为变更

其余 12 条需求不是「改 spec 文字」就是「行为不变的重构 / 补测试」。R2 会把非 Linux 平台上 `NewWithOptions(65537, …)` 的行为由「静默建出队列」改成「报错」。影响面已在 R2 正文写清（唯一生产调用方传 `aioDepth = 256`），但**这是需要在 review 时单独过目的一条**。

### 与 `09-22-spec-coverage-and-layout` 的关系

规则 12 由上一任务引入，本任务修正它的例外叙述 —— **两者是同一段文本的先后两版**，不是重复。上一任务的范围被明确限定为「补两条规则」，审计发现的新工作另开本任务，符合本仓「不因顺便修改无关规则」的惯例。

### 本机既有失败（不在本任务范围）

`go test ./...` 在 macOS 上有 7 个既有失败（2 个在 `cmd/taihu/cmd`、其余在 `third_party/shmipc-go` 等），**工作区干净时就如此**，完整清单见 `.trellis/spec/platform/build-verification.md:103-108` 与 `.trellis/spec/cli/index.md:144-149`。本任务的验收口径是「失败集合与改动前完全一致」，不是「全绿」。
