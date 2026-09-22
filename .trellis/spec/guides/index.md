# 思考指南（guides）

> 本层**不规定代码该怎么写** —— 那是其余 7 层的事。它管**动手之前该想什么、以及 rule 本身从哪来**。
>
> 与其它层的分工：其余 6 个主题层回答「这条规则是什么」；本层回答「什么时候该去想它」「它的出处是哪条上游规则」「哪些上游规则我们看过但没采纳」。

---

## 本层文件

| 文件 | 用途 |
|---|---|
| [code-reuse-thinking-guide.md](./code-reuse-thinking-guide.md) | 动手写新东西前的搜索纪律：本仓库的单一事实源清单、一次真实漏改（`64eaec3`）、什么时候**不**该抽象 |
| [cross-layer-thinking-guide.md](./cross-layer-thinking-guide.md) | 一次改动要穿过几层：flag 的 5 段链、error 跨进程的 5 层、枚举值的磁盘位置、以及一张「改了 A，另一半在 B」查表 |
| 本文 §三 | **溯源表** —— 上游编号 → spec 文件:节 的反向索引；外加「已符合、本轮未新增规则」的 78 条 |
| 本文 §四 | **不适用附录** —— 60 条不适用 / 10 条冲突裁决 / 2 条已登记缺口，一行一条说明为什么 |

---

## 一、思考触发器

不是检查清单（检查清单在各层的 Pre-Development Checklist），而是「出现下面这些信号时，去读哪份文件」。

| 信号 | 去读 |
|---|---|
| 我要改一个常量 / 配置值 / 枚举 | 先搜它的**解析侧**，不要只改写入侧 —— [cross-layer-thinking-guide.md](./cross-layer-thinking-guide.md) §8 |
| 同一个值在两个地方各定义了一份 | [code-reuse-thinking-guide.md](./code-reuse-thinking-guide.md) §1 的事实源表 |
| 我加的参数，调用方不止一个 | 每个调用方都要接上线，编译器**不会**替你查 —— [cross-layer-thinking-guide.md](./cross-layer-thinking-guide.md) §2 |
| 我在改磁盘格式 / 线上格式 | 编解码两侧一起看；[buffer-and-concurrency.md](../engine/buffer-and-concurrency.md) 与 [wire-protocol.md](../transport/wire-protocol.md) |
| 我在写新的 `_linux.go` 导出符号 | 先问 `_other.go` 要不要同名的 —— [file-splitting.md](../platform/file-splitting.md) |
| 我在导出签名里加了 `internal/` 类型 | 补 re-export alias，**漏了不报错** —— [api-surface.md](../architecture/api-surface.md) |
| 我在热路径上加「优化」 | 没有实测数字就不要进 —— [buffer-and-concurrency.md](../engine/buffer-and-concurrency.md) §七 |
| 我想抽象出公共函数 | 先看「什么时候**不**抽象」—— [code-reuse-thinking-guide.md](./code-reuse-thinking-guide.md) §5 |
| 我准备写一个「和已有代码很像」的新东西 | 先跑 [code-reuse-thinking-guide.md](./code-reuse-thinking-guide.md) §3 的三问 |
| 我打算在 spec 里新增一条规则 | 先过这里的 §三 与 §四：它可能已经在上游被收过，或者已知与本仓库形态不符 |

---

## 二、核对结论时的五种假阳性

本仓库的 spec 是靠「逐条核到 `file:line`」建立的（`.trellis/tasks/archive/2026-09/09-21-spec-upstream-alignment/design.md` §3.2 的准入条件）。下面五种是这个过程里**真出现过**的误判，不是假想的。

| # | 形态 | 真实实例 | 判据 |
|---|------|----------|------|
| 1 | **从截断的输出下结论** | 一份 `grep -A6` 的输出显示 `internal/cluster/kv_mem.go` 的 `Get` 直接返回了内部切片；读完整函数才发现 `:37` 有 `append([]byte(nil), v...)` —— 差点把一条**正例**报成违反 | 判断「有没有违反规则」必须读**完整**的函数，不能读窗口 |
| 2 | **凭记忆写锚点** | 曾把 `cmd/taihu/cmd/bench_storage.go` 误记成 benchkit 包下的同名文件（那里根本没有）；`internal/rpcclient` 下也从未有过一个叫 `client.go` 的文件，却曾被当成引用写出来 | 每条 `file:line` 写进 spec 前用 `sed -n '<line>p'` 回读一遍 |
| 3 | **把「沾边」当「覆盖」** | 判「errgroup 规则适用」的依据是 `go.mod` 里有 `golang.org/x/sync` —— 实际那是 **indirect** 依赖，代码零使用 | 依赖清单里有 ≠ 代码在用；必须落到调用点 |
| 4 | **假设机制存在** | 曾断言 `internal/transport/batch.go` 的 `batchWriter` 有 `Close()` 或队列关闭式的停止机制；实际**两者都没有**，`run` 是无退出条件的循环 | 写「某处有 X 机制」之前先 grep `X` |
| 5 | **数字来自宽松的 grep** | 曾报「139 处 `_ =`」，严格按**行首**计数实际是 **96**；`%w` 的库/CLI 分布也曾少算一个文件 | spec 里每处计数都要附上**可复现的命令**，让别人能重跑 |

**共同点**：五条都是「结论比证据跑得快」。修正成本很低（回读一行、重跑一次 grep），但错误进了 spec 就会长期误导后来的人 —— 上游原话：**错误模板比空模板危害更大**。

---

## 三、溯源表（上游编号 → spec 落点）

本轮把三份上游规范逐条对撞后择优并入现有 8 层。下表是**反向索引**：想知道某条上游规则落在本仓库 spec 的哪一节，查这里；想在某条规则里知道它的出处，看规则尾部的 `（上游 xxx-NNN）` 标记。

三份来源与编号前缀：

- `uber-` —— Uber Go Style Guide，134 条
- `ecc-` —— Everything Claude Code 的 go 规则，79 条
- `gbp-` —— cexll/golang-base-practices-skills，53 条

以上三份合计 266 条。**另有第四份来源**（2026-09-22 补入，不在那 266 条内），见下表末尾。

### 已并入（116 条）

下表的文件名是省略目录的简写：go-style.md 即 [go-style.md](../architecture/go-style.md)，unit-tests.md 即 [unit-tests.md](../testing/unit-tests.md)，buffer-and-concurrency.md 即 [buffer-and-concurrency.md](../engine/buffer-and-concurrency.md)，其余同理（各层入口见本文件顶部的索引）。

#### `uber-` （100 条）

| 上游编号 | 落点（spec 文件 :: 节） |
|---|---|
| `uber-001` | go-style.md 规则 2 |
| `uber-002` | go-style.md 规则 1 |
| `uber-005` | go-style.md 规则 1 |
| `uber-006` | go-style.md 规则 1 |
| `uber-007` | go-style.md 规则 3 |
| `uber-008` | go-style.md 规则 3 |
| `uber-009` | go-style.md 规则 4 |
| `uber-010` | api-surface.md 规则 6 |
| `uber-011` | go-style.md 规则 5 |
| `uber-012` | go-style.md 规则 5 |
| `uber-013` | go-style.md 规则 6 |
| `uber-014` | go-style.md 规则 7 |
| `uber-015` | go-style.md 规则 7 |
| `uber-016` | go-style.md 规则 8 |
| `uber-017` | go-style.md 规则 8 |
| `uber-018` | go-style.md 规则 8 |
| `uber-019` | go-style.md 规则 8 |
| `uber-020` | go-style.md 规则 8 |
| `uber-021` | go-style.md 规则 8 |
| `uber-025` | code-style.md 第 15 条 |
| `uber-026` | error-model.md 第 10 条 |
| `uber-029` | code-style.md 第 16 条 |
| `uber-031` | error-model.md 第 2 条 |
| `uber-032` | error-model.md 第 8 条 |
| `uber-033` | error-model.md 第 9 条 |
| `uber-034` | go-style.md 规则 9 |
| `uber-037` | unit-tests.md 规则 9 |
| `uber-040` | go-style.md 规则 10 |
| `uber-041` | go-style.md 规则 11 |
| `uber-042` | go-style.md 规则 12 |
| `uber-043` | go-style.md 规则 12 |
| `uber-044` | go-style.md 规则 12 |
| `uber-047` | metadata-and-compaction.md 版本字节与字段布局：两条规则 |
| `uber-048` | buffer-and-concurrency.md 6.3 批处理 worker：既无停止信号，也无 join 点（已知例外） ／ buffer-and-concurrency.md 六、goroutine 生命周期纪律 |
| `uber-049` | buffer-and-concurrency.md 六、goroutine 生命周期纪律 |
| `uber-051` | buffer-and-concurrency.md 6.1 后台循环：`stopCh` + `done` 成对，这是本仓库的招牌形态 |
| `uber-052` | buffer-and-concurrency.md 6.1 后台循环：`stopCh` + `done` 成对，这是本仓库的招牌形态 |
| `uber-053` | go-style.md 规则 12 |
| `uber-054` | buffer-and-concurrency.md 6.1 后台循环：`stopCh` + `done` 成对，这是本仓库的招牌形态 ／ buffer-and-concurrency.md 6.5 无 `init()` 里起 goroutine |
| `uber-055` | buffer-and-concurrency.md 6.1 后台循环：`stopCh` + `done` 成对，这是本仓库的招牌形态 ／ buffer-and-concurrency.md 6.4 进程入口与基准工具：不适用上述约束 |
| `uber-056` | buffer-and-concurrency.md 七、性能指引只适用于热路径 |
| `uber-057` | go-style.md 规则 13 |
| `uber-058` | go-style.md 规则 13 |
| `uber-059` | go-style.md 规则 14 |
| `uber-060` | go-style.md 规则 14 |
| `uber-061` | go-style.md 规则 14 |
| `uber-062` | go-style.md 附：上游编号 → 本文件规则 对照 ／ go-style.md 附：不采纳的上游条目 |
| `uber-063` | code-style.md 第 21 条 |
| `uber-065` | go-style.md 规则 15 |
| `uber-066` | go-style.md 规则 15 |
| `uber-067` | go-style.md 规则 15 |
| `uber-069` | go-style.md 规则 16 |
| `uber-070` | go-style.md 规则 16 |
| `uber-071` | go-style.md 规则 16 |
| `uber-072` | go-style.md 规则 16 |
| `uber-073` | go-style.md 规则 16 |
| `uber-074` | go-style.md 规则 16 |
| `uber-077` | go-style.md 规则 16 |
| `uber-078` | go-style.md 规则 17 |
| `uber-079` | go-style.md 规则 17 |
| `uber-080` | go-style.md 规则 17 |
| `uber-081` | go-style.md 规则 17 |
| `uber-082` | go-style.md 规则 17 |
| `uber-083` | go-style.md 规则 18 |
| `uber-084` | go-style.md 规则 18 |
| `uber-085` | go-style.md 规则 19 |
| `uber-086` | go-style.md 规则 19 |
| `uber-087` | go-style.md 规则 19 |
| `uber-089` | go-style.md 规则 10 |
| `uber-090` | go-style.md 规则 10 |
| `uber-091` | api-surface.md 规则 8 |
| `uber-092` | go-style.md 规则 10 |
| `uber-093` | go-style.md 规则 3 |
| `uber-094` | go-style.md 规则 19 |
| `uber-095` | go-style.md 规则 19 |
| `uber-096` | go-style.md 规则 20 |
| `uber-097` | go-style.md 规则 20 |
| `uber-098` | go-style.md 规则 20 |
| `uber-099` | go-style.md 规则 20 |
| `uber-100` | go-style.md 规则 19 |
| `uber-101` | go-style.md 规则 19 |
| `uber-102` | go-style.md 规则 19 |
| `uber-103` | go-style.md 规则 21 |
| `uber-104` | go-style.md 规则 21 |
| `uber-105` | go-style.md 规则 13 |
| `uber-106` | go-style.md 规则 22 |
| `uber-107` | unit-tests.md 规则 8 |
| `uber-108` | go-style.md 规则 22 |
| `uber-109` | go-style.md 规则 22 |
| `uber-110` | go-style.md 规则 22 |
| `uber-111` | go-style.md 规则 22 |
| `uber-112` | go-style.md 规则 22 |
| `uber-113` | go-style.md 规则 22 |
| `uber-119` | unit-tests.md 规则 8 |
| `uber-120` | unit-tests.md 规则 8 |
| `uber-122` | unit-tests.md 规则 8 |
| `uber-123` | unit-tests.md 规则 8 |
| `uber-124` | unit-tests.md 规则 8 |
| `uber-128` | go-style.md 规则 23 |
| `uber-129` | go-style.md 规则 23 |
#### `ecc-` （13 条）

| 上游编号 | 落点（spec 文件 :: 节） |
|---|---|
| `ecc-002` | api-surface.md 规则 7 |
| `ecc-011` | api-surface.md 规则 7 |
| `ecc-015` | index.md 参数写法与校验 |
| `ecc-021` | code-style.md 第 18 条 |
| `ecc-023` | code-style.md 第 18 条 |
| `ecc-024` | code-style.md 第 17 条 |
| `ecc-029` | code-style.md 第 19 条 |
| `ecc-030` | code-style.md 第 17 条 |
| `ecc-031` | index.md 收尾前的自检单 |
| `ecc-036` | unit-tests.md 规则 11 |
| `ecc-018` | unit-tests.md 规则 12 |
| `ecc-032` | unit-tests.md 规则 12 |
| `ecc-038` | code-style.md 第 3 条 |
#### `gbp-` （3 条）

| 上游编号 | 落点（spec 文件 :: 节） |
|---|---|
| `gbp-019` | code-style.md 第 14 条 |
| `gbp-037` | code-style.md 第 20 条 |
| `gbp-038` | api-surface.md 规则 8 |

#### 第四份来源：`project-layout`（2026-09-22 新增）

[golang-standards/project-layout](https://github.com/golang-standards/project-layout)。**它不在上面那 266 条里** —— 是后续补的一条工程结构规则引入的来源，编号前缀不适用（原文没有编号体系），按下表逐节对应。

| 上游内容 | 落点（spec 文件 :: 节） |
|---|---|
| 顶层目录清单与各自定义 | repo-layout.md 规则 1 |
| 「目录名与可执行文件名一致」「`/src` 不该有」等判据 | repo-layout.md 规则 1 |
| 新增目录时怎么选 | repo-layout.md 规则 2 |

⚠️ 引用它时必须连带说明**它自称非官方标准**（原文："NOT an official standard defined by the core Go dev team"）—— 本仓库采用它的理由是命名共识广，不是它权威。

### 已符合、本轮未新增规则（78 条）

这 78 条**没有新增任何 spec 条文**，但原因各不相同 —— 分开列，免得下次又把它们当成「漏掉的」。

#### 甲 本仓库已有规则覆盖（64 条）

判据：该上游主张的**实质内容**已经写在下面这一节里，本轮读到了、确认覆盖，因此不另起条文。做法是逐条在本仓库 spec 里搜关键词、再把命中的整节读一遍（不是搜到词就算）。

| 上游编号 | 已有的落点（省略 `architecture/` 前缀） |
|---|---|
| `uber-003` | transport/interfaces-and-reexport.md :: 3. 编译期满足性断言是常态 |
| `uber-004` | transport/interfaces-and-reexport.md :: 3. 编译期满足性断言是常态 |
| `uber-022` | error-model.md :: 2. 自定义 error 类型只有一处，且不导出 |
| `uber-023` | error-model.md :: 5. 对外 re-export 只走两层，且**故意少一个** |
| `uber-024` | error-model.md :: 9. 四种处置方式，按契约选 |
| `uber-030` | error-model.md :: 3. 包内控制信号用未导出 sentinel |
| `uber-035` | code-style.md :: 4. 非测试代码不 `panic()` |
| `uber-036` | code-style.md :: 4. 非测试代码不 `panic()` |
| `uber-045` | cli/command-and-output.md :: 退出码与错误出口 |
| `uber-046` | cli/command-and-output.md :: 退出码与错误出口 |
| `uber-047` | cli/command-and-output.md :: 机器可读输出（`--json`） |
| `uber-068` | code-style.md :: 13. 三段式：标准库 / 第三方（含 `third_party/` fork）/ 本项目 |
| `uber-088` | error-model.md :: 3. 包内控制信号用未导出 sentinel |
| `uber-117` | testing/index.md :: Pre-Development Checklist |
| `uber-118` | testing/unit-tests.md :: 规则 8：表测试保持"输入 → 期望"的单层形态 |
| `uber-132` | index.md :: 工具链现状（已知缺口，未采纳） |
| `ecc-001` | cli/index.md :: 格式化 |
| `ecc-003` | code-style.md :: 18. KISS / YAGNI：清晰优先于机巧，不为没有调用方的需求建抽象 |
| `ecc-004` | error-model.md :: 10. `%w` 是默认选择，且被包装的错误是契约的一部分 |
| `ecc-009` | go-style.md :: 规则 23：预见会扩展的公开构造器用 Functional Options |
| `ecc-010` | transport/index.md :: Pre-Development Checklist |
| `ecc-015` | cli/index.md :: 参数写法与校验 |
| `ecc-016` | testing/index.md :: 本层是什么 |
| `ecc-017` | testing/e2e-tests.md :: 规则 6：e2e 不使用 `-race` |
| `ecc-022` | guides/code-reuse-thinking-guide.md :: 5. 什么时候**不**抽象 |
| `ecc-025` | code-style.md :: 14. 不静默吞掉 error；确需丢弃时必须 `_ =` 并写明理由 |
| `ecc-026` | transport/wire-protocol.md :: 4. `Parse*` 的防御顺序：先校验长度声明，再分配 |
| `ecc-027` | code-style.md :: 6. 首字母缩写全大写，不写 `ClientId` |
| `ecc-028` | go-style.md :: 规则 18：减少嵌套——先处理错误与特殊分支，提前返回 |
| `ecc-033` | testing/index.md :: 本层是什么 |
| `ecc-039` | transport/index.md :: Pre-Development Checklist |
| `ecc-045` | error-model.md :: 4. 跨进程边界传 code 不传字符串 |
| `ecc-050` | index.md :: 收尾前的自检单 |
| `ecc-055` | index.md :: 收尾前的自检单 |
| `ecc-057` | engine/metadata-and-compaction.md :: 二、`Store` 接口的明确契约 |
| `ecc-060` | commits.md :: 1. 标题格式：`type(scope): <中文标题>` |
| `ecc-062` | commits.md :: 9. 本仓库不做自动提交 |
| `gbp-016` | code-style.md :: 8. `fmt.Errorf("op: %w", err)`，op 前缀的形态按**场合**分两套 |
| `gbp-017` | error-model.md :: 1. `internal/ierr` 是唯一事实源 |
| `gbp-018` | error-model.md :: 2. 自定义 error 类型只有一处，且不导出 |
| `gbp-021` | code-style.md :: 4. 非测试代码不 `panic()` |
| `gbp-022` | engine/buffer-and-concurrency.md :: 六、goroutine 生命周期纪律 |
| `gbp-024` | go-style.md :: 规则 6：channel 默认为无缓冲或容量 1；更大的容量必须写明理由 |
| `gbp-025` | cli/index.md :: 参数写法与校验 |
| `gbp-027` | go-style.md :: 规则 3：mutex 是非指针命名字段，且**绝不内嵌** |
| `gbp-028` | testing/e2e-tests.md :: 规则 6：e2e 不使用 `-race` |
| `gbp-029` | go-style.md :: 规则 16：包名默认不重命名，别名为避冲突而存在 |
| `gbp-030` | code-style.md :: 8. `fmt.Errorf("op: %w", err)`，op 前缀的形态按**场合**分两套 |
| `gbp-031` | transport/index.md :: Pre-Development Checklist |
| `gbp-032` | code-style.md :: 5. receiver 用单字母，同一类型恒定同一字母 |
| `gbp-033` | go-style.md :: 规则 22：结构体与容器的初始化形态 |
| `gbp-034` | go-style.md :: 规则 23：预见会扩展的公开构造器用 Functional Options |
| `gbp-035` | go-style.md :: 规则 5：资源清理用 `defer` |
| `gbp-039` | code-style.md :: 14. 不静默吞掉 error；确需丢弃时必须 `_ =` 并写明理由 |
| `gbp-041` | testing/index.md :: Pre-Development Checklist |
| `gbp-042` | testing/unit-tests.md :: 规则 7：测试辅助函数带主体前缀，共享 fake 放 `testutil_test.go` |
| `gbp-043` | testing/unit-tests.md :: 规则 7：测试辅助函数带主体前缀，共享 fake 放 `testutil_test.go` |
| `gbp-047` | go-style.md :: 规则 13：字符串与基本类型的互转、以及固定字符串的字节化 |
| `gbp-048` | go-style.md :: 规则 14：容器尽量给容量提示 |
| `gbp-049` | index.md :: 工具链现状（已知缺口，未采纳） |
| `gbp-050` | code-style.md :: 13. 三段式：标准库 / 第三方（含 `third_party/` fork）/ 本项目 |
| `gbp-051` | platform/build-verification.md :: 规则 1：`make check-linux` 就是这四条命令，逐条对上 |
| `gbp-052` | index.md :: 工具链现状（已知缺口，未采纳） |
| `gbp-053` | index.md :: 工具链现状（已知缺口，未采纳） |

#### 乙 本仓库尚未覆盖，但也不构成一条可写的规则（5 条）

| 上游编号 | 上游主张 | 为什么没落成条文 |
|---|---|---|
| `uber-028` | 为返回的错误添加上下文时保持简洁，避免 "failed to" 这类堆叠短语 | 原文自述是「keep it succinct」，不含可判定的判据。本仓库已有可检验的那一半 —— [code-style.md](../architecture/code-style.md) 规则 8 定死了 op 前缀的形态，这条不增加内容 |
| `uber-114` | 在 `Printf` 系列函数之外声明格式字符串时，应定义为 `const` 以便 `go vet` 静态分析 | 本仓库的格式化调用点用的都是**直接字面量**（`grep` 无「先存变量再传给 Printf」的写法），触发条件不存在。`go vet` 本身已在 `make check` 里 |
| `uber-115` | 声明 Printf 风格函数时尽量用预定义名，`go vet` 才能识别 | 本仓库没有自定义的 Printf 风格函数，全是标准库 `fmt` 家族 —— 无锚点 |
| `uber-116` | 无法用预定义名时，自定义名以 `f` 结尾（`Wrapf`），并加 `-printfuncs` | 同上：没有自定义 Printf 风格函数 |
| `ecc-037` | 测试命名描述行为而非实现（"returns empty array when no markets match query"） | 本仓库的测试命名聚焦「被测主体 + 场景」（见 [unit-tests.md](../testing/unit-tests.md) 规则 1 的 `TestXxx_Yyy` 形态与 e2e 的 `TestA1CamelCase`），描述的是**本仓库的哪个行为**；直接套用英文长句命名会与既有 82 个测试文件的命名惯例冲突，而收益（可读性）在本仓库已由「同包同目录 + 主体前缀」达成 |

#### 丙 属于 harness / 工具链形态，不是仓库规则（9 条）

| 上游编号 | 上游主张 | 为什么不是本仓库的规则 |
|---|---|---|
| `uber-050` | 用 `go.uber.org/goleak` 做 goroutine 泄漏测试 | 未安装的第三方工具，同 §四 的「本轮不引入新工具」约束。本仓库对 goroutine 生命周期的约束走**代码规范**：见 [buffer-and-concurrency.md](../engine/buffer-and-concurrency.md) 六（`stopCh` + `done` 配对） |
| `ecc-006` | PostToolUse hook：编辑 `.go` 后自动 `gofmt` / `goimports` | 本仓库的等价物是 `make check` 的 `check-fmt`（**人工触发**）。装 hook 会改变本仓库的改动节奏，不在本轮范围 |
| `ecc-007` | PostToolUse hook：编辑 `.go` 后跑 `go vet` | 同上，等价物是 `Makefile` 的 `go vet ./...` |
| `ecc-048` | 强制评审触发点：写完代码后、提交共享分支前、架构变更、合并 PR 前 | 无第二评审人（个人仓库直推 `main`，同 §四 的 `ecc-061`）。本仓库的「评审」位置由 Trellis 的 check 步骤承担 |
| `ecc-064` | 先规划：用 planner agent 产出 PRD / architecture / system_design / task_list | 依赖 ECC 的 `planner` agent；本仓库的等价物是 Trellis 的 `prd.md` / `design.md` / `implement.md` 三件套 |
| `ecc-066` | 写完代码立刻用 code-reviewer agent | 依赖 ECC 的 `code-reviewer`；本仓库的等价物是 `trellis-check` |
| `ecc-072` | Hook 分三类：PreToolUse / PostToolUse / Stop | Claude Code harness 的通用行为说明，不描述本仓库的任何代码形态 |
| `ecc-073` | 自动放行权限慎用，改用 `allowedTools` | harness 权限配置建议，与本仓库代码无关 |
| `ecc-074` | 多步任务用 TodoWrite 跟踪进度 | harness 使用建议，与本仓库代码无关 |

> **甲与「已并入」的重叠**：`ecc-015` 与 `uber-047` 同时出现在 §三 的已并入表与本节甲表里 —— 前者的**规则本体**是本轮新写的 [cli/index.md](../cli/index.md) 规则 9，后者的规则本体已写在 [metadata-and-compaction.md](../engine/metadata-and-compaction.md)，而本节甲表记的是它们在别处**另有**一处既有覆盖。两处都不重复规定同一件事。

### 不采纳

见 §四，以及 [go-style.md](../architecture/go-style.md) 末尾的「附：不采纳的上游条目」。

---

## 四、不适用附录

三份上游共 **266 条**（uber 134 / ecc 79 / gbp 53），逐条对撞后的去向：

| 去向 | 条数 | 在哪 |
|---|---|---|
| 已并入 spec | 116 | §三 溯源表 |
| 已符合，本轮未因此新增规则 | 78 | §三 的上一小节 |
| 不适用 | 60 | §4.1 – §4.3 |
| 判为冲突，维持 taihu 现状 | 10 | §4.4 |
| 未采纳，但已登记为已知缺口 | 2 | §4.5 |

「不适用」的判据是**本仓库的形态**，不是「这条不好」。每行都给出形态层面的理由（多数附可复跑的判据），不写「暂不需要」这类无法证伪的话。

### 4.1 `uber-` —— 8 条

| 上游编号 | 上游主张 | 为什么不适用 |
|---|---|---|
| `uber-027` | 用 `%v` 遮蔽底层错误，调用方无法匹配，日后可改回 `%w` | 正反例都没有：`grep -rnE 'fmt\.Errorf\("[^"]*%v", *err\)' internal pkg cmd` 全仓零命中。taihu 的策略是「可匹配的一律 `%w`、其余原样返回」，不存在需要刻意遮蔽底层错误的场景 |
| `uber-064` | 推行规范建议在包或更大粒度上变更 | 这是「把规范落到代码库」的实施粒度建议，属过程性规则，不描述任何代码形态；源码里没有对应锚点 |
| `uber-075` | 例外：测试函数可含下划线做用例分组（`TestMyFunction_WhatIsBeingTested`） | `grep -rn 'func Test[A-Za-z0-9]*_'` 零命中 —— 这个例外从未被使用，既无正例也无反例 |
| `uber-076` | 包名与导入路径末段不符时必须用导入别名 | 全仓唯一一处导入别名（`internal/cluster/kv_tikv.go:8` 的 `tikverr`）原因不是路径不符：`github.com/tikv/client-go/v2/error` 的包名与路径末段一致（都叫 `error`），别名是为避免遮蔽内置名（那条属 `uber-041`）。规则描述的场景在本仓库不存在 |
| `uber-121` | `test depth` 的定义（一次测试中连续断言的个数） | 这是为 `uber-119` / `uber-120` / `uber-122` 服务的**术语定义**，本身不构成可执行规则，没有可指认的正反例 |
| `uber-125` | 表测试 vs 独立测试没有严格准则，可读性/可维护性优先 | 元建议，原文自述 "no strict guidelines"，不含可判定的要求；其精神已由 `uber-119` / `uber-123` / `uber-124` 在 taihu 的具体锚点覆盖 |
| `uber-126` | 并行测试 / 特殊循环必须显式在循环作用域内复制循环变量 | 前提在本项目不成立：全仓 `t.Parallel()` 零命中；且 `go.mod` 声明 `go 1.25.0`，循环变量自 Go 1.22 起已按迭代独立，该规则要防的捕获问题在语言层已消除 |
| `uber-127` | 用了 `t.Parallel()` 时必须声明作用域限定在本次迭代的 `tt` | 同 `uber-126`：触发条件（`t.Parallel()`）在本仓库不存在 |

### 4.2 `ecc-` —— 34 条

#### 指向 ECC 自带制品（本仓库未安装）

| 上游编号 | 上游主张 | 为什么不适用 |
|---|---|---|
| `ecc-005` | 详细 Go 惯用法以 skill `golang-patterns` 为准 | 指向 ECC 自带的 skill 制品，taihu 未安装该技能包。等价物是 `.trellis/spec/engine/`、[code-style.md](../architecture/code-style.md) 等分层 spec |
| `ecc-012` | 详细 Go 模式（并发 / 错误处理 / 包组织）以 skill `golang-patterns` 为准 | 同 `ecc-005` |
| `ecc-019` | 详细 Go 测试模式以 skill `golang-testing` 为准 | 同 `ecc-005`；测试侧等价物是 [unit-tests.md](../testing/unit-tests.md) |
| `ecc-075` | Agent 名册以 plugin-scoped `subagent_type` 调用（`ecc:planner` 等 68 个） | 指 ECC 插件的 agent 名册。本仓库用的是 `.claude/agents/{trellis-implement,trellis-check,trellis-research}.md`，没有 `ecc:*` 命名空间 |
| `ecc-076` | 无需用户提示即应调用 agent 的四种情形 | 依赖 `ecc-075` 的名册（`planner` / `tdd-guide` / `architect` 均不存在）。Trellis 的 implement/check 派发与这四类映射不重合 |

#### 工具链：本仓库未安装，且本轮明确不引入（`prd.md:71`）

| 上游编号 | 上游主张 | 为什么不适用 |
|---|---|---|
| `ecc-008` | PostToolUse hook：对改动的包跑 `staticcheck` | 本仓库未使用 `staticcheck`（无配置文件、无 CI；[Makefile](../../../Makefile) 的 `check` 只跑 gofmt / 两条分层 grep / `go vet`）。代码里既无正例也无反例 |
| `ecc-014` | 用 `gosec ./...` 做静态安全扫描 | 工具未使用，无锚点；同 `ecc-008` 的「不引入新工具」约束 |
（`ecc-018` / `ecc-032` 原列在此处判为不适用，2026-09-22 改判并并入 [unit-tests.md](../testing/unit-tests.md) 规则 12 —— 见 §三 溯源表。）

#### 无 CI、无分支模型的流程条目

| 上游编号 | 上游主张 | 为什么不适用 |
|---|---|---|
| `ecc-049` | 请求评审前置条件：CI 全绿、冲突已解、分支与目标分支同步 | 本仓库无 CI（`.github/` 不存在，无 `.gitlab-ci.yml` 等）；且本机 `go test ./...` 已知**不是全绿**（[platform/index.md](../platform/index.md) 记了 Linux 专有路径上的失败），「CI 全绿」这个门槛在 taihu 无对应物 |
| `ecc-067` | 提交前复查：CI 全绿、无冲突、分支同步，通过后才请求评审 | 同 `ecc-049` |

#### Web / 应用安全面：本仓库没有这一面

| 上游编号 | 上游主张 | 为什么不适用 |
|---|---|---|
| `ecc-013` | 密钥从环境变量读（`os.Getenv`），缺失即启动失败（`log.Fatal`） | 无凭据面：非测试代码 `os.Getenv` 仅 1 处（`internal/aio/aio.go:138`，读 AIO 后端模式），`log.Fatal` 零命中。TiKV TLS 走 CLI flag 传证书**路径**：`internal/cluster/kv_tikv.go:15-20` 的 `TLSConfig` 三个字段都是 PEM 文件路径，由 `cmd/taihu/cmd/server.go:112-114` 从 `global.tikvCA/tikvCert/tikvKey` 填入 —— 全程没有密钥内容本身 |
| `ecc-040` | 防 SQL 注入：参数化查询 | 无 SQL / ORM：`grep -rn 'database/sql'` 全仓零命中。元数据走 pebble API（`internal/metastore/kv_pebble.go`），集群注册走 TiKV TxnKV（`internal/cluster/kv_tikv.go`），都是 key-value API，不存在语句拼接 |
| `ecc-041` | 防 XSS：HTML 转义 | 无 HTML / 模板渲染，无浏览器侧产物 |
| `ecc-042` | 开启 CSRF 防护 | 无 cookie / 表单 / 会话，无浏览器客户端 |
| `ecc-043` | 校验认证 / 授权 | 数据面与注册区都没有认证授权层（`grep -rni 'auth\|token\|permission' internal/transport/ pkg/taihu-client/` 非测试零命中）；跨进程信任建立在网络可达与 TiKV 注册之上 |
| `ecc-044` | **所有端点**加限流 | 无 HTTP 端点。自研帧协议没有 per-client 限流，也不需要 —— 流控由 `Ring.ErrFull`（`internal/aio/aio.go:38`，注释在 `:37`「提交队列已满（io_submit 返回 EAGAIN），应先 Wait 取回完成事件后重试」）与在途队列这类背压机制承担 |
| `ecc-046` | 密钥绝不硬编码，启动时校验存在，泄露即轮换 | 同 `ecc-013`：无密钥管理面 |
| `ecc-051` | 安全评审触发点：认证授权、用户输入、数据库查询、文件系统操作、外部 API、密码学、支付 | 清单以 Web / 应用安全为前提，本仓库无其中任何一面（见上表各行） |
| `ecc-054` | 重点排查项（安全）：硬编码凭据、SQL 注入、XSS、路径穿越、CSRF、认证绕过 | 同一组 Web 安全项；本仓库也无路径穿越面 —— 用户不提供文件路径（key 是 KV 的 key，设备路径来自 CLI flag） |
| `ecc-056` | 重点排查项（性能）：N+1 查询、缺分页、无界查询、缺缓存 | 无数据库查询、无 HTTP 分页。本仓库的性能关注点是另一套：零拷贝生命周期、缓冲池命中、`io_submit` 批量粒度、段级 compaction，与这四项无交集 |
| `ecc-058` | API 响应统一信封：`success` + `data` + `error` + 分页 metadata | 无 REST / JSON API 对外。线上格式是自研帧（`internal/transport/protocol/protocol.go:51` `const FrameHeaderLen = 5`），没有 `success` / 分页字段；CLI 的 `-json` 输出是对象原样序列化（`cmd/taihu/cmd/helpers.go:118` `printJSON`），不套信封 |

#### 安全分级与响应协议：本仓库没有这套体系

| 上游编号 | 上游主张 | 为什么不适用 |
|---|---|---|
| `ecc-047` | 安全事件响应协议：STOP → security-reviewer agent → 先修 CRITICAL → 轮换密钥 → 全库排查 | 依赖 ECC 的 `security-reviewer` agent 与「已泄露密钥」这一前提；本仓库两者都没有 |
| `ecc-052` | 严重度分级：CRITICAL 阻断 / HIGH 警告 / MEDIUM 提示 / LOW 可选 | 本仓库没有代码缺陷严重度分级体系。**注意不要混淆**：`.trellis/tasks/archive/2026-09/09-21-spec-upstream-alignment/research/conflicts.md` 里的「A 类 / B 类 / C 类」是**冲突条目的影响面**分级，不是代码评审的缺陷严重度 |
| `ecc-053` | 通过标准：无 CRITICAL 且无 HIGH 才 approve | 依赖 `ecc-052` 的分级体系，本仓库无 |
| `ecc-065` | 走 TDD：用 `tdd-guide` agent，RED→GREEN→IMPROVE，验证 80%+ 覆盖 | 依赖 ECC 的 `tdd-guide` subagent，本仓库无此 agent —— **不适用的只剩「交给哪个 agent」这一半**。另外两半都已各自裁决并采纳：TDD 的次序见 [unit-tests.md](../testing/unit-tests.md) 规则 10，80% 覆盖率门槛见同文件规则 12 |

#### agent / harness 编排约定：与本仓库的 Trellis 工作流不是一套

| 上游编号 | 上游主张 | 为什么不适用 |
|---|---|---|
| `ecc-035` | 测试失败排查顺序：用 tdd-guide agent、检查测试隔离、核对 mock、改实现而非改测试 | 依赖 `tdd-guide` subagent 与 mock 文化；本仓库自有测试零 mock 框架（`testify` 在自有代码零引用，见 [unit-tests.md](../testing/unit-tests.md)），失败排查无对应流程 |
| `ecc-063` | 实现前强制 Research & Reuse：`gh search repos` / `gh search code` → Context7 / 官方文档 → Exa | 以 ECC 特有检索工具链为前提。最接近的心智是 [code-reuse-thinking-guide.md](./code-reuse-thinking-guide.md)，但它是**复用心智**而非检索步骤清单 |
| `ecc-068` | 模型选择：Haiku 做轻量高频、Sonnet 做主开发、Opus 做架构决策 | ECC 对其 harness 的模型编排建议，本仓库无对应约定，代码里无锚点 |
| `ecc-069` | 上下文管理：不在最后 20% 上下文里做大重构 / 跨文件实现 / 复杂调试 | ECC 的上下文预算管理。最接近的是 `.trellis/workflow.md` 的「Persist everything —— conversations get compacted, files don't」，但那讲的是**外部化持久化**，不是剩余上下文预算 |
| `ecc-070` | 复杂任务开 Extended Thinking + Plan Mode，多轮批判，按角色拆子 agent | harness 级操作建议，非仓库规则，代码里无锚点 |
| `ecc-071` | 构建失败处理：用 `build-error-resolver` agent，逐条分析、增量修复、每步验证 | 依赖 ECC 的 `build-error-resolver` agent。本仓库的构建失败处理就是 `make check` / `make check-linux` |
| `ecc-077` | 独立操作一律并行发起多个 agent | ECC 的多 agent 编排约定；本仓库的 Trellis 派发是单链路（implement → check → research），仓库内无并行派发的证据 |
| `ecc-078` | 委托完成契约：最终消息即交付物，禁止以「等待后台 agent」收尾 | ECC agent-team 编排约定，属 harness 行为规范，本仓库里无对应物 |
| `ecc-079` | 复杂问题用分角色子 agent 做多视角分析 | 同 `ecc-078` |

#### 项目形态不符

| 上游编号 | 上游主张 | 为什么不适用 |
|---|---|---|
| `ecc-059` | 新功能优先找久经考验的 skeleton 项目克隆后在其结构内迭代 | 以「有可克隆的同形态上游项目」为前提；本仓库是存储引擎，与 ECC 假想的 Web 服务模板形态不同。它的对应物是向自己既有的分层与 spec 收敛 |

### 4.3 `gbp-` —— 18 条

#### HTTP 框架与服务形态（4 条，整块摘除）

| 上游编号 | 上游主张 | 为什么不适用 |
|---|---|---|
| `gbp-001` | 小到中型项目、简单 REST API 应选 Gin | 讲 HTTP Web 框架选型。本仓库无 HTTP 服务端：数据面是自实现的二进制帧协议（`internal/transport/protocol`），控制面走 TiKV |
| `gbp-002` | 复杂微服务应选 Go-Kratos | Kratos 提供 gRPC / HTTP 双协议、服务发现、配置中心。本仓库无 gRPC、无 RPC 框架、无配置中心（配置走 cobra flag） |
| `gbp-003` | 日志 / 鉴权 / 限流抽成 middleware | 正反例全建立在 Gin 的 `gin.Context` / JWT 上。本仓库无 HTTP handler 栈、无鉴权层；跨层关注点的形态是传输层帧读写与日志三通道（[code-style.md](../architecture/code-style.md) 规则 10-12），不是 middleware |
| `gbp-004` | 服务必须支持优雅停机，等在途请求处理完 | 规则的载体是 `http.Server` + `srv.Shutdown(ctx)`。本仓库有**对应物**（`cmd/taihu/cmd/server.go:256` 的 `sig := make(chan os.Signal, 1)` 与 `internal/transport/server.go` 的 `GracefulStop()`），但那条线索的价值在「超时是否可配」，已单独采纳，见 [cli/index.md](../cli/index.md) 规则 9 |

#### Database & ORM（5 条，整块摘除）

| 上游编号 | 上游主张 | 为什么不适用 |
|---|---|---|
| `gbp-005` | GORM 初始化必须配 logger + 连接池 + PrepareStmt | 本仓库是存储引擎**本身**，不是 ORM 的使用方。持久化是自研的 `internal/layout` + `internal/metastore`（Pebble）+ `internal/storage`，磁盘格式自己定，没有 GORM 可初始化 |
| `gbp-006` | GORM Hook 只放简单自动化逻辑 | 同上：无 GORM，无 `BeforeCreate` / `AfterFind` 生命周期 |
| `gbp-007` | 多次写操作必须包在 `db.Transaction` 里 | 无 SQL、无 ORM 事务。原子性靠 Pebble 的 `Batch`（`internal/metastore/kv_pebble.go`）与 `BatchPutCommit`，语义与 `db.Transaction` 不是一回事 |
| `gbp-008` | 用 Goose 做受版本控制的 schema 迁移 | 无关系型 schema，也无迁移工具。要求存储引擎用 Goose 做版本化迁移，是**把被实现物当成依赖**；磁盘格式的版本兼容有自研机制（当前存在的实际缺陷已单列，见 [cross-layer-thinking-guide.md](./cross-layer-thinking-guide.md) §5） |
| `gbp-009` | 生产环境必须显式配置连接池四个参数 | 无数据库连接池。形态相近的是 `bufpool`（`internal/bufpool/bufpool.go`），但那是 4K 对齐的内存桶、且**刻意否决了 `sync.Pool`**（`internal/bufpool/bufpool.go:10-17` 写明了实测依据：GC 清空导致 4M/8M 大缓冲每轮重分配，实测冷分配约占服务端 CPU 36%），与 `SetMaxOpenConns` 一族没有对应关系 |

#### DDD 分层（6 条，整块摘除）

| 上游编号 | 上游主张 | 为什么不适用 |
|---|---|---|
| `gbp-010` | 按 `cmd/ internal/{domain,application,infrastructure,interfaces} pkg/` 分层 | 本仓库按**资源与机制**分层（`internal/{aio,bufpool,device,layout,metastore,storage,...}`），不是按 DDD 业务域分层。改成 domain/application/infrastructure/interfaces 会与 [layering.md](../architecture/layering.md) 以及 `Makefile` 的 `check-layering` / `check-sdk-only` 门禁**直接冲突** |
| `gbp-011` | 领域层放纯业务逻辑，不依赖外部框架 | 以 `User` 实体 / `Email` 值对象 / `Repository` 接口为例的业务领域建模。本仓库没有业务域模型（没有 User 这类实体），它的「领域」是段 / 对象 / 映射，且必须贴着设备与内核（`internal/device`、`internal/aio`），「不依赖外部」在这里是不成立的约束 |
| `gbp-012` | 应用层编排用例，按 CQRS 分离 Command 与 Query | 无 CQRS、无用例 Handler。编排面是 RPC 服务端（`internal/transport/server.go` 的 handlePut / handleGet / …），与 Command/Query 分离是两套建模 |
| `gbp-013` | 基础设施层实现领域接口，并做 model ↔ entity 转换 | 以 GORM 实现 `UserRepository` + `toDomain` / `toModel` 互转为例。本仓库无 ORM，无 entity / model 双层映射 |
| `gbp-014` | 接口层把外部请求转成应用层的 command/query | 以 Gin handler + DTO + `ShouldBindJSON` 为例。本仓库无 HTTP 接口层；外部请求的入口是二进制帧解析 |
| `gbp-015` | 用依赖注入解耦，推荐 Google Wire | 无 Wire、无 `wire.Build`（全仓 `wire` 零命中）。装配是显式构造函数参数（如 `transport.NewServer(storage)`）+ 少量 `Option` 变参（`internal/storage/options.go`），这是刻意的：装配点少且都在 CLI 层，代码生成得不偿失 |

#### 单条不适用（3 条）

| 上游编号 | 上游主张 | 为什么不适用 |
|---|---|---|
| `gbp-020` | 定义统一的 API 错误响应结构体与错误码 | 无 REST 接口、无 JSON 错误响应体。本仓库有**更强的对应物** —— 跨进程边界传 code 不传字符串，`ErrCode` 枚举 + `MapStorageErr` / `MapCode` 在 `internal/transport/protocol/protocol.go`，且已有「服务端不 `MapCode`、客户端不 `MapStorageErr`」的用法边界（[error-model.md](../architecture/error-model.md)）。不适用的是它的具体形态（HTTP 状态码映射） |
| `gbp-040` | 生产代码目标 99% 测试覆盖率 + 按分层设目标 | 「99% 硬指标」不适合按层设，本仓库也不按百分比管理（现状约 2 万行测试 / 1.3 万行非测试，比例高但不是门禁）。其分层目标表（domain 100% / application 99% / …）建立在 DDD 分层上，而那一层不适用（见 `gbp-010`） |
| `gbp-045` | 用 build tag + testcontainers 写集成测试 | 正例是起真实 MySQL 容器 + `httptest.NewServer`，本仓库无 MySQL、无 HTTP。**注意** build tag 那半本仓库是有的且做得更细（`//go:build e2e` 与 `//go:build linux` 两组，见 [file-splitting.md](../platform/file-splitting.md) 与 [e2e 相关章节](../testing/index.md)），但那属另一个主题，不靠本条并入 |

### 4.4 判为冲突、逐条裁决的 10 条

这 10 条与前述「不适用」不同：它们**各有可取之处**，只是与本仓库已成形且有理有据的做法相撞。逐条裁决见 `.trellis/tasks/archive/2026-09/09-21-spec-upstream-alignment/research/conflicts.md`，下表是结论。

| 上游编号 | 上游主张 | 裁决 | 落地 |
|---|---|---|---|
| `uber-038` | 原子操作优先用 `go.uber.org/atomic` 而非 `sync/atomic` 裸类型 | **不采纳 —— 上游规则已过时** | `go.uber.org/atomic` 的能力（`atomic.Bool` / `atomic.Int64` 等类型化 API）自 Go 1.19 起已由标准库完整覆盖，本仓库 `go.mod` 是 `go 1.25.0`，且已在用（`internal/transport/stats.go:16-19`）。采纳等于为「标准库已有且更好」的能力引入一个直接依赖，与 `internal/aio/aio.go:1` 写明的「纯 Go 实现，不依赖任何外部库」相悖 |
| `uber-039` | 避免可变全局变量，改用依赖注入 | **维持 taihu** | 包级函数变量 `newTiKVKV`（`cmd/taihu/cmd/helpers.go:63`、`pkg/taihu-client/tikv.go:35`）是**刻意的测试缝隙**，`cmd/taihu/cmd/server.go:111` 的注释写明「生产恒为 `cluster.NewTiKVKV`」。改掉它会波及 `connectKV` 等一批调用点签名 |
| `uber-130` | Option 用「带未导出方法的接口」而非闭包 | **维持 taihu** | 本仓库用的是闭包式 `type Option func(*options)`（`internal/storage/options.go:7`、`internal/device/options.go:7`）——「未导出 `options` struct」这一半与上游一致，只有 Option 的载体不同 |
| `uber-131` | 相比闭包更推荐 Option 接口（可比较、可实现 `fmt.Stringer`） | **维持 taihu** | 每包的选项各 2 个，既不需要在测试里比较选项，也不需要 `fmt.Stringer`。改成接口式会改掉两个包的公开 API |
| `ecc-020` | 不可变性：永远创建新对象，绝不原地修改 | **维持 taihu ⇒ 落为「刻意偏离」** | 见 [buffer-and-concurrency.md](../engine/buffer-and-concurrency.md) 末尾的 `## 刻意偏离上游规则`。两半分别裁决：缓冲**复用**是刻意偏离（否决 `sync.Pool` 有 36% CPU 的实测依据）；零拷贝**移交**则相反 —— 本仓库已因实测到的静默数据错配而回退 TCP 路径（`internal/transport/client.go:117-121`），是与上游同向的收敛 |
| `ecc-034` | TDD 强制流程：RED → GREEN → REFACTOR | **采纳为硬规则** | 落在 [unit-tests.md](../testing/unit-tests.md) 规则 10。`fed67b4`（一次提交补齐 81 个 `_test.go`）与 `9bcc797`（改 6 个文件、0 个 `_test.go`）记为**待改进的历史惯性**，不作为范例 |
| `ecc-061` | PR 流程：分析提交历史 → `git diff <base>...HEAD` → 写摘要 → 新分支 `push -u` | **不适用** | 个人仓库，`git branch -a` 只有 `main`，145 个提交里只有 1 个 merge —— 规则的「开分支 → 提 PR → 评审」在无第二评审人的单仓里没有落点。规则的**另一半**（写完整摘要）本仓库做到了，见 [commits.md](../architecture/commits.md) |
| `gbp-036` | slice / map 用法；其中断言「边遍历边 `delete` 是未定义行为」 | **部分采纳：前半照收，后半显式剔除** | **上游这半句是错的**：Go 语言规范明确允许在 `range` 期间删除（只是新增的条目不保证被遍历到）。照抄会把 `internal/cluster/kv_mem.go:52`、`internal/aio/aio_other.go:157`、`internal/device/device.go:161` 三处**正确**代码判成违规。前半（预分配）已由 [go-style.md](../architecture/go-style.md) 规则 14 覆盖 |
| `gbp-044` | 用 `go test -bench` 量化性能，配合 `benchstat` 对比 | **维持 taihu ⇒ 落为「刻意偏离」** | 见 [unit-tests.md](../testing/unit-tests.md) 末尾的 `## 刻意偏离上游规则`。本仓库的性能测量走 `internal/benchkit/` 自研 harness 与 e2e 的 F/G 组，自有代码 `func Benchmark` 零命中 —— 理由是可检验的：`go test -bench` 压不满真实 IO，也覆盖不了 shm / TCP 两条数据面 |
| `gbp-046` | 用 testify 让断言更清晰 | **维持 taihu ⇒ 落为「刻意偏离」** | 见 [unit-tests.md](../testing/unit-tests.md) 末尾的 `## 刻意偏离上游规则`。`go.mod` 里的 `stretchr/testify v1.9.0` 只为 `third_party/shmipc-go` 的 13 个 fork 测试而存在，自有代码零引用 |

### 4.5 未采纳，但已登记为已知缺口（2 条）

这两条的上游主张本仓库**认可**，只是本轮明确不引入工具（`implement.md` 的「不做的事」）。它们没被丢掉，而是写进了 [architecture/index.md](../architecture/index.md) 的「工具链现状（已知缺口，未采纳）」表，作为下一轮的输入。

| 上游编号 | 上游主张 | 登记在哪 |
|---|---|---|
| `uber-133` | 用工具自动检查未处理的 error（`errcheck`） | [index.md](../architecture/index.md) 的工具链现状表 |
| `uber-134` | 用 `golangci-lint` 作为统一 lint runner | 同上 |

该表同时写明：若将来引入，`errcheck` 应当**最先** —— 它能把 [code-style.md](../architecture/code-style.md) 规则 14 的 96 处 `_ =` 从「靠人守」变成「门禁挡」。

### 4.6 60 条为什么「整块」不适用

按条数看，60 条不是零散的意见不合，而是三块**与本仓库形态无关的领域**：

| 块 | 条数 | 共同前提在本仓库不成立的原因 |
|---|---|---|
| Web / 应用安全（框架、ORM、认证、限流、XSS/CSRF/SQLi、响应信封） | ~28 | 本仓库的对外形态是**自研二进制帧协议**，不是 HTTP + REST + SQL。攻击面（用户输入的文件路径、SQL 语句、浏览器会话）都不存在 |
| agent / harness / CI 编排约定 | ~13 | 那些规则描述的是**另一套工具链**（ECC 的 68 个 agent、`tdd-guide`、CI 全绿门槛）。本仓库的对应物是 Trellis 的三阶段工作流与 `make check` |
| DDD 分层与领域建模 | ~6 | 本仓库按资源与机制分层，且分层本身有 `Makefile` 门禁在管；改成 DDD 是与既有门禁**冲突**，不是补充 |

剩下的十几条是「触发条件不存在」（如 `t.Parallel()`、下划线测试名、`%v` 遮蔽）—— 这类规则不是不好，是在本仓库**没有可指认的锚点**，按 `design.md` §3.2 的准入条件不能进 spec。

**这三块共同的空白**：266 条上游规则里，**零条**涉及 mmap/msync、fsync/O_DIRECT/崩溃一致性/WAL、io_uring/libaio/IOPOLL、on-disk 布局与校验和、LSM/B-tree/compaction、cgo/x-sys、掉电与坏块测试。也就是说本仓库 spec 里最有价值的部分（`engine/` 那几层）**没有任何上游可以参照** —— 它只能从 taihu 自己的代码里长出来。这从反面印证了准入条件的必要性。
