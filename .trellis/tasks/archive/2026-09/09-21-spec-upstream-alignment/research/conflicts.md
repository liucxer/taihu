# 冲突清单 —— 评审门

> **状态：已裁决（2026-09-22）。** 四份上游清单全部收回：Uber 134 条（适用 122 / 冲突 4 / 不适用 8）、ECC 79 条（40 / 3 / 36）、cexll 53 条（32 / 3 / 18），合计 266 条；taihu 现有 spec 盘点 297 条 `th-` 规则。三分类完成后，194 条适用规则已对既有规则去重（`dedup-uber.md`、`dedup-ecc-gbp.md`）：**已覆盖 38 / 部分覆盖 37 / 新增 118 / 措辞可改善 1**，其中新增的 118 条里 **41 条有反例**、77 条仅正例。
>
> **裁决方式**：每条末尾有 `裁决：` 一行。裁决为「改 taihu」的本轮**不改代码**，只落 spec 待办。

排序按「是否影响真实行为」：**A 类影响真实行为** → B 类影响 spec 措辞 → C 类影响工具链。

## 裁决汇总

| # | 议题 | 裁决 | 落地方式 |
|---|------|------|----------|
| C-01 | 原子操作该用哪个包 | 维持 taihu（`sync/atomic`） | `uber-038` 判不适用 —— Go 1.25 起 stdlib 已有类型化原子量 |
| C-02 | 不可变性 vs 零拷贝缓冲复用 | 拆两半：**复用 = 刻意偏离**（CPU 36% 实测）；**零拷贝移交 = 已向上游收敛** | spec 写为「刻意偏离」并附实测依据 |
| C-03 | 「一律 5s 超时」 | 维持 taihu | spec 写明各超时点的实际取值与判据 |
| C-04 | 可变全局状态 | 承认 `internal/transport/frame.go:25` 的 `init()` 操纵 netpoll 全局分配器 | 走「写规则 + 标注既有例外」 |
| C-05 | 日志第四通道：`internal/device` 写 `os.Stderr` | **本轮记待办，另开任务改代码** | spec 只标注 device 为已知例外；`git diff --stat -- '*.go'` 保持为空 |
| C-06 | 操作前缀两套写法，标题与自己的例子矛盾 | 承认两套并写明各自场合 | 同时修正 `code-style.md:84` 的自相矛盾正例 |
| C-07 | `guides/` 整层是英文的 Trellis 工具文档 | 清内容、保留层 | 三文件对 taihu 文件路径**零引用**（只引 Trellis 工具路径），清空后该层由本任务新写 |
| C-08 | `commits.md` 硬编码计数证伪 | 修正 | 实测 `docs` 42 / `feat` 28 / `perf` 22 / `refactor` 17 / `fix` 16 / `chore` 11 / `revert` 3 / `style` 2 / `test` 1；清单漏列 `revert`/`test`/`init`/`bench` |
| C-09 | 62 处裸 basename 引用（约 23 处真歧义） | 部分：引用完整性由脚本守住；**23 处裸 basename 未补齐** | `verify_spec_refs.py` 850 有效 / 0 不存在 / 0 越界；存量引用未动，见本条末尾的「未做的原因」 |
| C-10 | `Makefile` 注释引用三个已不存在的路径 | 只记待办，不碰 | 本任务不动 `Makefile` |
| C-11 | 缺 spec 引用完整性校验（脚本在 `/tmp`，重启即失） | 修 | 脚本随任务入库到 `research/verify_spec_refs.py` |

四项 P3 约定裁决另见 `../implement.md` 的「P2 评审门产出」：go-style.md 全开 76 条 / 有反例规则照写并标注既有例外 / F-03 另开任务 / TDD 采纳为硬规则。

---

## A 类：影响真实行为

### C-01 —— 原子操作该用哪个包

| 项 | 内容 |
|----|------|
| 上游主张 | `uber-038`：原子操作优先用 `go.uber.org/atomic` 而非 `sync/atomic` 裸类型，理由是类型安全、有 `atomic.Bool` |
| taihu 现状 | 9 个文件 import `sync/atomic`（`internal/transport/frame.go:11`、`stats.go:7`、`batch.go:20`、`server_shm_linux.go:22`、`internal/rpcclient/dial.go:9`、`internal/aio/aio_uring_linux.go:11`、`internal/device/device.go:20`、`internal/benchkit/run.go:11`、`pkg/taihu-client/picker.go:5`）。`go.uber.org/atomic v1.10.0` 只在 `go.mod:79` 作为 **indirect** 依赖存在（zap 带进来的） |
| 上游理由是否仍成立 | **不成立。** `go.mod` 声明 `go 1.25.0`；标准库自 Go 1.19 起就有类型化的 `atomic.Uint64` / `atomic.Int64` / `atomic.Bool`。taihu 已经在用（如 `pkg/taihu-client/picker.go` 的 `atomic.Uint64`、`internal/transport/stats.go` 的 `atomic.Int64`）。uber 那条规则主要是为了给当时的 Go 版本补类型安全，其动机已被标准库吸收 |
| 采纳的代价 | 为「标准库已有且更好」的能力新增一个**直接**依赖。这与 `internal/aio/aio.go:1` 写明并作为设计目标之一的「**纯 Go 实现，不依赖任何外部库**」直接对撞 |
| 建议 | **维持 taihu**，记入刻意偏离。但**同时**把 taihu 内部的两套写法统一掉（见下方「顺带发现」） |

**顺带发现（与上游无关，是 taihu 自身的不一致）**：仓库同时存在两套原子 API 写法 ——
- 类型化：`atomic.Uint64`（`pkg/taihu-client/picker.go:74`、`:157`）、`atomic.Int64`（`internal/transport/stats.go:16-19`）
- 裸函数：13 处 `atomic.LoadInt64/StoreInt64/AddUint64...`

三份上游都没有直接规定这一点。这属于「一个主题两套写法」，是 spec 该定死的对象。**此项独立于 C-01 的裁决**：即便维持 taihu，也建议新增一条规则统一到类型化写法。

裁决：**维持 taihu** —— 判 `uber-038` **不适用**：规则已被标准库取代（Go 1.19+ 的 `sync/atomic` 类型化 API），不是「我们不做」而是「上游这条已过时」。
落地：`guides/index.md` §4.4 第 1 行；`implement.md` 的「不采纳」清单。
旁记：本仓库「两套原子写法并存」（类型化 `atomic.Uint64` vs 裸函数 `atomic.LoadInt64`）本轮**未立规则** —— 它是 taihu 自身的风格统一问题，三份上游都没覆盖。也正因如此，我不能拿上游条目当依据替它拍板，记为下一轮候选。

---

### C-02 —— 不可变性 vs 零拷贝缓冲复用

| 项 | 内容 |
|----|------|
| 上游主张 | `ecc-020`（**CRITICAL** 优先级）：默认不可变，避免就地修改 |
| taihu 现状 | 零拷贝与缓冲就地复用是核心设计。`internal/transport/frame.go:18-24` 的注释写明「客户端 Get 可直接移交该缓冲给调用方（**零拷贝**），并经由 `bufpool.Put` 安全归还」「每帧独占一个节点，读满一帧后 book 剩余容量为 0、节点不再被复用」；`internal/bufpool/bufpool.go:12-19` 记录了**否决 `sync.Pool` 的实测依据**（读路径冷分配占服务端 CPU 36%） |
| 双方理由 | 上游的不可变性是应用层默认值（避免共享可变状态引发的 bug）。taihu 的复用是**存储引擎的性能前提** —— 每帧一次分配会被 O_DIRECT 对齐要求和 IO 吞吐放大 |
| **初稿的立论需要修正** | 初稿写的是「taihu 用『移交所有权 + 归还』的显式协议替代不可变性」，读起来像**整片地**刻意偏离。核到 `internal/transport/client.go:110-121` 之后，实情更细、也更有意思 —— 见下方「两半」 |
| **第一半：缓冲复用** —— 真·刻意偏离 | `internal/bufpool/bufpool.go:12-19` 记了否决 `sync.Pool` 的**实测依据**（GC 清空导致 4M/8M 大缓冲整批重分配，读路径冷分配占服务端 CPU 36%）。这半维持 taihu，偏离理由引实测数据即可 |
| **第二半：零拷贝移交** —— **taihu 已经站在上游那侧** | `internal/transport/client.go:117-121` 逐字记着：曾用 netpoll 零拷贝移交 `TakeTry`（整响应恰一帧时直接移交收流节点缓冲），**实测该路径存在静默数据错配 —— 移交后整块缓冲被归还池并复用，内容被后续收流覆盖**；仅收紧「帧独占整块」（`base==0`）仍会复现，**故 TCP 路径已停用**，netpoll 侧保留护栏与单测；shm 数据面的单帧移交不受影响 |
| **这条证据的分量** | 上游 `ecc-020` 担心的正是「共享可变状态引发的静默错误」，而 taihu **真的踩过这个坑并因此回退了零拷贝** —— 只不过回退得**有边界**：TCP 路径退回「恰一次用户态拷贝」，shm 路径（帧独占、无跨流复用）保留零拷贝。这不是「我们不在乎不可变性」，而是**把不可变性用在需要的地方，把复用留给证明安全的地方** |
| 建议 | **维持 taihu，但 C-02 的措辞按「两半」重写**：缓冲复用落成「刻意偏离」（理由是 CPU 36% 实测）；零拷贝移交落成「**与上游同向、且已有实测驱动的收敛**」—— 把 `client.go:117-121` 作为**这条规则在 taihu 的正面锚点**，而不是反例。`design.md` §3.3 要求偏离理由具体到可检验，这条恰好正反两面都有实测 |

裁决：**维持 taihu，按「两半」分写**：缓冲就地复用落成「刻意偏离」，零拷贝移交落成「与上游同向的收敛」。
落地：`engine/buffer-and-concurrency.md` 末尾 `## 刻意偏离上游规则` 第 1 条（缓冲复用，引 `internal/bufpool/bufpool.go:10-17` 的服务端 CPU 36% 实测）与第 2 条（零拷贝移交的边界：TCP 停用、shm 保留）。
**为什么拆两半**：上游那条主张的正确部分（不把共享缓冲交给所有权不明的代码）taihu 是遵守的，只有「一读一写共享同一块缓冲」这个前提不成立 —— 整条判「偏离」会掩盖这层区别。

---

### C-03 —— 「一律 5s 超时」

| 项 | 内容 |
|----|------|
| 上游主张 | `ecc-015`：`context.Context` 传超时，示例用统一的 5s |
| **核实结论** | **taihu 基本已遵守**，`context` 在 8 个包里有 176 处使用，传播链完整。但揪出一处真实例外 |
| 已遵守的证据 | ① 超时是**参数**而非常量：`internal/cluster/client.go:39` 与 `pkg/taihu-client/registry.go:59` 的 `5 * time.Second` 都是 `if timeout <= 0` 的**兜底默认值**，正常路径下由调用方传入。② CLI 层完整传播：`cmd/taihu/cmd/root.go:112` 是 `context.WithTimeout(parent, global.timeout)`，`--timeout` 可配（`cmd/taihu/cmd/root.go:91`，默认 5s）。③ 子超时从父 ctx 派生：`cmd/taihu/cmd/cluster.go:279`、`:295` 的 `WithTimeout(ctx, 3*time.Second)` |
| **真实例外** | `internal/transport/server.go:83` —— `GracefulStop()` 里写死 `context.WithTimeout(context.Background(), 5*time.Second)`。三个问题叠加：**方法不接参数**（调用方无从控制）、**用 `context.Background()`**（与任何父 ctx 断开，不受 CLI `--timeout` 影响）、**无注释**说明为什么是 5s |
| 该例的性质 | 不是「上游对、taihu 错」那么简单。关停路径**确实需要一个兜底期限**（否则 `GracefulStop` 可能永久挂住），所以有界是刻意的；问题在于**这个界不可配、来路不明**。且注意 taihu 在别处对同类问题给过理由（`bufpool` 否决 `sync.Pool` 有实测数据），这里却没有任何说明 |
| 建议 | 分两步，都不在本轮：<br>① **spec 层**（本轮可做）：新增一条规则 —— 关停 / Graceful 路径的超时必须**要么可配、要么写明为什么是该值**。这条能指到 `internal/transport/server.go:83` 这个真实锚点，符合准入条件。<br>② **代码层**（本轮只记待办）：把 `GracefulStop` 改成 `GracefulStop(ctx)` 或接受一个 `timeout` 参数 |

裁决：**维持 taihu** —— 但把「有界是可接受的」升级成规则的三级优先序，而不是简单记为例外。
落地：`cli/index.md` 规则 9「超时必须可配，或者写明为什么是这个值」：① 暴露成参数 → ② 从父 ctx 派生 → ③ 写死但注释说明。
`internal/transport/server.go:83` 登记为**唯一已知例外**（①②③ 全不符合），spec 只记录、不改代码。
代码层待办（本轮不做，另开任务）：把 `GracefulStop()` 改成接 `ctx` 或接 `timeout` 参数。

---

### C-04 —— 可变全局状态

| 项 | 内容 |
|----|------|
| 上游主张 | `uber-039`：避免可变全局变量，改依赖注入（原文理由与「测试时替换时钟」相关） |
| taihu 现状 | `internal/transport/frame.go:25` 的 `init()` 设置三个包级配置：`netpoll.SetAlignedAllocator(bufpool.Get, bufpool.Put)` / `SetInputAlignedAllocator(bufpool.GetExact, bufpool.PutExact)` / `SetInputNodeSize(protocol.InputNodeSize)`。另有 12 处 `init()`，绝大多数在 `cmd/taihu/cmd/`（cobra 命令注册，属该框架的既有形态） |
| 双方理由 | 上游反对的是「全局可变 + 无纪律地随时改」。taihu 这三行是**启动期一次性装配**，且必须在任何 netpoll 使用之前完成 —— `init()` 恰好提供了这个顺序保证。这是「用 init 换顺序保证」的刻意选择，不是疏漏 |
| 附注 | 上游另两条相关规则 taihu **已遵守**：`uber-042`（init 不得做 I/O）—— 这三行只是赋值，无 I/O；`uber-053`（init 不得起 goroutine）—— 无 goroutine |
| 建议 | **维持 taihu**，记入刻意偏离。偏离理由写清「init 在这里承担的是装配顺序保证」 |

裁决：**维持 taihu** —— 走「写规则 + 显式标注既有例外」。
落地：`architecture/go-style.md` 规则 12（`init()` 的约束）后的 ⚠️ 既有例外注，写明 `internal/transport/frame.go:25` 那三行是**启动期一次性装配**，`init()` 在此承担的是顺序保证。
同源问题另记：同文件 `## 刻意偏离上游规则` 第 2 条（包级函数变量作测试缝隙，`cmd/taihu/cmd/helpers.go:32`/`:63`）—— 两者同属「可变包级状态」，但被上游两条不同规则分别命中，故分两处落地。

---

### C-05 —— 日志第四通道：库代码 `internal/device` 直接写 `os.Stderr`

| 项 | 内容 |
|----|------|
| **勘误（我自己写错的）** | 本条初稿把上游主张记到 `ecc-006` 名下，写成「ECC 的 logging 规则」。**这是错的** —— 我核对后确认 `ecc-006` 是「PostToolUse hook：编辑 `.go` 后自动 gofmt/goimports」，与日志无关；且 ECC 的 79 条里**没有任何一条**是日志规则。已删除该错误引用 |
| 上游主张 | **无。本条不是上游冲突，是 taihu 自己的 spec 与自己的代码打架**（来源：`triage.md` F-03） |
| taihu 现状 | `architecture/code-style.md` 规则 10~12 断言「**本仓库只有三条日志通道**」，规则 11 收尾还专门写了「这是 CLI/服务端进程入口的特权，**库代码不要模仿**」。而 `internal/device/device.go:155`、`:295`、`:299` 三处 `fmt.Fprintf(os.Stderr, ...)` 恰恰是库代码 |
| 双方理由 | 若这是刻意的：`device.go:155` 在完成泵 `pump()` 的热路径（`log` 带互斥锁与时间戳），`:293` 的 `logComplRetry` 已自带「避免刷屏」节流 —— 说明作者对这里的日志成本有过考虑。但**代码里没有任何注释说明为什么绕过 `log`**，这是推测不是证据 |
| 另一处不一致 | 这三处是**中文**文案，而通道二的库内诊断（`taihu: aio `、`compaction: `）一律英文前缀。规则 8 只允许「面向用户的 CLI 文案」用中文 |
| 选项 | **(a)** spec 承认第四条通道并写明理由与边界（热路径 / 绕锁 / 节流），同时定死它的文案语言；**(b)** `device.go` 改走通道二，spec 不动 |
| 我的建议 | **(b)**。理由：若 (a)，理由得由作者补，我写不出可检验的版本（`design.md` §3.3 要求「理由具体到可检验」）；而 (b) 的代价很小 —— 三处都是诊断输出，不参与控制流。**但 (a) 若你记得当年的理由，请告诉我，(a) 更尊重既有决策** |

裁决：**本轮记待办，另开任务改代码**；spec 侧只把它标注为已知例外。
落地：`architecture/code-style.md` 的「⚠️ 已知例外：`internal/device` 事实上存在一个第四通道」小节，逐条列出 `internal/device/device.go:155`、`:295`、`:299` 与规则 11/12 的三点偏差，并写明**不就地承认成第四条通道**的理由（`:288` 的 `logComplRetry` 已经节流，但没有任何注释说明它为什么绕过 `log`）。
代码层待办（本轮不做）：三处改成走 `log` 通道，前缀与文案语言一并对齐。`git diff --stat -- '*.go'` 保持为空。

---

### C-06 —— 操作前缀有两套写法，且 spec 标题与自己的例子矛盾

| 项 | 内容 |
|----|------|
| 上游主张 | 无。**这条完全是 taihu 自身的 spec 缺陷** —— 是对撞过程中顺出来的（详见 `triage.md` F-04） |
| taihu 现状 | `architecture/code-style.md:79` 规则 8 标题「op 前缀**小写英文**」，同一条规则 `:84` 的第二个例子却是大写 CamelCase 的 `DialPoolMulti: empty addrs` |
| 代码实况 | 7 处大写（`internal/rpcclient/dial.go:33`；`cmd/taihu/cmd/bench_storage.go:100,108,119,126`；`cmd/taihu/cmd/server.go:122,148`），约 30 处小写词组。大写的 7 处**规律一致**：op 取的是被调用 API 的名字（`NewStorage`、`DeviceCapacity`），不是动作描述 |
| 选项 | **(a)** 承认两套并写明各自场合（库内步骤用小写词组 / 调用具名 API 用其名）；**(b)** 统一到小写词组，7 处列待办 |
| 我的建议 | **(a)**。它描述的是代码里真实存在且自洽的规律，且 7 处集中在 CLI 装配路径上，「报错里直接给出失败的那个 API 名」对运维反而更有用。选 (b) 的话这 7 处要改代码，与本案「只落 spec」的范围也不符 |
| 附带小瑕疵 | `internal/rpcclient/dial.go:33` 是静态字符串（无 `%w`、无 err 可包），按惯例应为 `errors.New`。与 op 前缀之争无关，是否顺手记待办由裁决定 |

裁决：**承认两套并写明各自场合**（选项 a）—— 不统一到小写：那要改 7 处 CLI 代码，与「本任务只落 spec」的范围冲突。
落地：`architecture/code-style.md` 规则 8 重写为三行场合表（库代码小写动作名 / CLI 指代 Go 构造步骤用导出名 / CLI 描述业务动作用小写短语），判据写成「前缀指代的是 Go 函数，还是业务动作 —— 不是文件在哪个目录」。原 `:84` 那处自相矛盾的正例随重写一并修正，`internal/rpcclient/dial.go:33` 明确标注为**库代码侧的唯一历史例外**。
附带小瑕疵（`dial.go:33` 应为 `errors.New`）本轮未改代码，仅记于此。

---

## B 类：影响 spec 措辞

### C-07 —— `guides/` 整层是英文，且内容是 Trellis 工具仓库自己的开发文档

| 项 | 内容 |
|----|------|
| 上游主张 | 无。**仓库自身的约定问题**（详见 `triage.md` F-05） |
| taihu 现状 | 25 个 spec 文件中只有 `guides/` 下 3 个是 0 个中文字符，其余 22 个全中文 |
| 更关键的事实 | `guides/cross-layer-thinking-guide.md` 的小节标题是 `Cross-Platform Template Consistency`、`Generated Runtime Template Upgrade Consistency`、`Mode-Detection Probe Checklist` —— 讲的是 **Trellis 模板生成器**，与 taihu 无关。且这三组小节在该文件里**逐字重复出现两遍**（`:126`/`:223`、`:141`/`:238`、`:197`/`:267`），是模板拼接的指纹 |
| 性质 | 不是语言偏好问题：**这一层的内容不是 taihu 的规则**。留着它，spec 就有一整层与代码无关、还会重复的规则，违背本任务 `design.md` §3.2 的准入条件（指不到 taihu 锚点的不进正文） |
| 选项 | **(a)** 删掉 `guides/` 层（本任务的溯源表改放 `architecture/index.md` 或单独 `provenance.md`）；**(b)** 清掉工具自带内容，只保留真正跨层的 taihu 规则，并译成中文；**(c)** 原样保留 |
| 我的建议 | **(b)** —— 但先确认 `guides/` 里有没有**任何**一条真属于 taihu 的规则。若有，它多半该归到已存在的 8 层里，`guides/` 本层就可以整体删掉（即 (a) 的实质）。**(c) 不建议**：留着会让「本仓库只有 8 层规则」这个前提不成立 |

裁决：**清内容、保留层** —— 核过 `guides/` 三文件对 taihu 文件路径零引用，通篇讲的是 Trellis 模板生成器自己的开发流程，不属于 taihu 的规则。
落地：`guides/` 三个文件**全部重写**为中文的 taihu 规则：`code-reuse-thinking-guide.md`（复用与单一事实源）、`cross-layer-thinking-guide.md`（跨层与跨边界）、`index.md`（思考触发器 + 五种假阳性 + 溯源表 + 不适用附录）。
**层保留的理由**：「动手前先搜什么」这类动作型指南在 Taihu 有真实对象（§2 记的漏改 `64eaec3` 是真事），删掉整层会丢掉唯一的跨层入口。溯源表因此落在 [index.md](../../../spec/guides/index.md)，未另建 `provenance.md`。

---

### C-08 —— `commits.md` 的两处硬编码计数已证伪，且具名了错误的范例

| 项 | 内容 |
|----|------|
| 上游主张 | 无。**仓库自身的 spec 缺陷**（详见 `triage.md` F-06） |
| taihu 现状 | `architecture/commits.md:9` 写「现有 **143 条**提交」（写下时点实为 144，今天 145）；`:75` 写「全仓库里**只有 2 条**带 `行为不变：`（逐条比对所得：`c50473a`、`522ae0c`）」 |
| 实测 | 行首为 `行为不变：` 的是 **3 条**：`c50473a`、`3bc5f91`、`876b75c`。**`522ae0c` 不在其中** —— 它的「行为不变」是正文 bullet 里的一句（`- 行为不变：正常关机路径照旧…`），不是收尾的独立行 |
| 后果 | 读者照 `:75` 去 `git show 522ae0c` 学这条约定，看到的是反例；而真正合规的 `3bc5f91` 是**纯新增文件**的提交，拿它当「只搬不改」的范例也不对 |
| 选项 | **(a)** 改成判据式表述（不写数字，写「`git log --format=%B \| grep -c '^行为不变：'` 可重跑」）+ 另选真正合规的范例；**(b)** 只更新数字为 145 / 3 |
| 我的建议 | **(a)**。这两条从写下的那一刻就在腐烂（`:9` 当时已差 1），更新数字只是把腐烂推迟一次。根因是**把当时的计数快照写死进 spec** |

裁决：**改成判据式表述 + 更正范例**（选项 a）—— 只更新数字是把腐烂推迟一次。
落地：`architecture/commits.md:9` 改 145 条；`:24`「42 条带 scope 的提交，其中 3 条为多 scope」；`:86`「145 条里只有 4 条带『行为不变：』」；`:97` 新增「⚠️ 不能用 `git log --grep='行为不变'` 计数 —— 命中的还包含正文里引用了这个词组的提交」。
原 `522ae0c`（它的「行为不变」在正文 bullet 里、不在收尾独立行）已换掉。type 清单的计数与漏列项（`revert`/`test`/`init`/`bench`）一并订正。

---

### C-09 —— spec 的 62 处裸 basename 引用，其中约 23 处真歧义

| 项 | 内容 |
|----|------|
| 起点 | research agent 报了 1 处（`wire-protocol.md` 的 `server.go:342`）。**我实测后发现是 62 处** —— agent 低估了两个数量级 |
| 冲突 basename | 仓库里有 6 组同名文件：`client.go`（`cmd/taihu/cmd/`、`internal/cluster/`、`internal/transport/`，**3 个**）、`server.go`（`cmd/taihu/cmd/`、`internal/transport/`）、`storage.go`（`pkg/taihu-client/`、`internal/storage/`）、`instance.go`（`cmd/taihu/cmd/`、`internal/cluster/`）、`reexport.go`（`pkg/taihu-client/`、`internal/rpcclient/`）、`version.go`（`cmd/taihu/cmd/`、`internal/version/`） |
| 实测口径 | 62 处「该文件名在**本文件内从未被目录限定过**」的裸引用，分布 6 个 spec 文件 |
| **不必改的 39 处** | `cli/index.md`（11）、`cli/command-and-output.md`（16）、`transport/wire-protocol.md`（12）—— 这三个文件**整篇只讲一个目录**（`cmd/taihu/cmd/` 或 `internal/transport/`），层语境已把 basename 定死，读者不会误解 |
| **真歧义的约 23 处** | `sdk/public-api.md` 的 `storage.go` **17 处**、`sdk/index.md` 的 `storage.go` 7 处与 `reexport.go` 3 处、`architecture/layering.md` 的 `storage.go` 3 处。<br>**为什么这几个是真歧义**：SDK 文档与分层文档**恰好同时讨论这两侧** —— `pkg/taihu-client/storage.go` 与 `internal/storage/storage.go` 都是它们的主题；`reexport.go` 同理（`internal/rpcclient/reexport.go` 与 `pkg/taihu-client/reexport.go` 正是 `interfaces-and-reexport.md` 的论述对象）。读者无法靠语境排除 |
| **最误导的一处** | `layering.md:109` 与 `sdk/index.md:32-34`：这两处是**导入行清点**，写成 `| internal/cluster | config.go:13、index.go:7、… storage.go:11 … |` —— 排版上像是「`internal/cluster` 目录下的 storage.go」，实际指的是 **`pkg/taihu-client/storage.go:11`**（即「`pkg/taihu-client` 的哪个文件导入了 `internal/cluster`」）。而 `internal/cluster/storage.go` **根本不存在** |
| 性质 | **引用本身是对的**（562 条全量校验 0 缺失 / 0 越界），问题在**可核验性** —— 读者要试错才能确定指的是哪个文件，AI 更会猜错 |
| 选项 | **(a)** 新增一条 spec 规则：「6 组同名 basename 的引用必须带目录前缀」，并把现存 23 处补齐；**(b)** 只落规则、存量不动（新写的照规则来）；**(c)** 不动 |
| 我的建议 | **(a)** —— 23 处是机械替换，零语义风险；且不补的话规则自己就带着 23 个反例，那是 spec 最该避免的样子 |

裁决：**部分落地：引用完整性由脚本守住；23 处裸 basename 本轮未补齐。**
已做：`verify_spec_refs.py` 随任务入库，全 spec `path:line` 检查 **850 条有效 / 0 不存在 / 0 越界**（本轮收尾口径）。
未做：`sdk/public-api.md`（`storage.go` 17 处）、`sdk/index.md`（`storage.go` 7 处、`reexport.go` 3 处）、`architecture/layering.md`（`storage.go` 3 处）仍靠上下文定死，未加目录前缀。
**未做的原因**：这 23 处是「可核验性」问题，不是引用错误 —— 引用本身都对。改法是把它们指向 `pkg/taihu-client/storage.go`，属机械替换但会动 SDK 层整份文档；本轮未获明确裁决，故留原文并在此登记。
**下一轮最该先动的一处**：`sdk/index.md:32-34` 与 [layering.md](../../../spec/architecture/layering.md) 的导入行清点 —— 行标写的是 `internal/cluster`，列出的却是 `pkg/taihu-client` 的文件。这不是简写问题，是标错了。

---

## C 类：影响工具链

### C-10 —— `Makefile` 注释引用三个已不存在的路径

| 项 | 内容 |
|----|------|
| 事实 | `Makefile:39`、`:43-44` 的注释引用 `internal/transport/server_shm.go`（实为 `server_shm_linux.go`）、`pkg/rpcclient`、`pkg/rpccluster`（已收进 `pkg/taihu-client`）。三个路径 `ls` 均不存在 |
| 影响面 | **仅注释**。`check-sdk-only` 按前缀 grep 实现，门禁不红 |
| 与既有 spec 的关系 | 三个 spec 文件已把旧路径标为历史路径，**但 Makefile 没改** |
| 选项 | **(a)** 本轮顺手改注释（不碰任何逻辑）；**(b)** 只记待办 |
| 我的建议 | **(a)** —— 但注意 `implement.md` 明写「本轮不碰 Makefile」。若你选 (a)，那是一次**明确的例外**，需要你点头；选 (b) 则我落成待办条目 |

裁决：**只记待办，不碰 `Makefile`** —— 任务范围明写「不动 `Makefile`」；改注释虽无逻辑风险，仍属计划外改动。
落地：本轮 `Makefile` **零改动**（`git diff --stat -- Makefile` 为空）。
待办内容即本条：`Makefile:39`、`:43-44` 的注释引用了三个已不存在的路径 —— `internal/transport/server_shm.go`（实为 `server_shm_linux.go`）、`pkg/rpcclient`、`pkg/rpccluster`。影响面仅注释，`check-sdk-only` 按前缀 grep 实现，门禁不红，故不阻塞。

---

### C-11 —— 缺 spec 引用完整性校验（本任务临时脚本在 `/tmp`，重启即失）

| 项 | 内容 |
|----|------|
| 背景 | 本轮改 spec 时，`file:line` 引用会随代码变动漂移。上一轮 `aio` 收敛导致**约 30 处引用偏移**，是靠一个临时脚本（`research/verify_spec_refs.py`）找出来的：561 处引用 0 缺失 / 0 越界 |
| 现状 | 该脚本本轮才从 `/tmp` 复制进 `research/`，**但仍在任务目录里，不是仓库资产**。下次改代码又会重蹈覆辙 |
| 选项 | **(a)** 本轮把脚本落到仓库（如 `scripts/check-spec-refs.py`）并挂进 `make check`；**(b)** 只把「引用会漂移」这条写进 spec，提醒改代码的人手动核对 |
| 我的建议 | **(b)**，本轮不做 (a)。理由：`make check` 是代码门禁，加一个只校验 markdown 的检查会改变它的语义与耗时；且脚本目前是**启发式**的（自查时产生 18 条警告，逐条核实**全是误报**），误报率未经打磨。落 spec 提醒更诚实 |
| 附注 | 若选 (a)，脚本需要先降误报 —— 现状会让人养成「忽略红字」的习惯 |

裁决：**脚本随任务入库，但本轮不挂进 `make check`**。
已做：`verify_spec_refs.py` 落到任务目录（原先只在 `/tmp`，重启即失），本轮每次改动后都跑。
未做：没落到仓库级 `scripts/`、没挂进门禁。理由与原建议一致 —— `make check` 是**代码**门禁，加一个只校验 markdown 的步骤会改变它的语义与耗时；且脚本目前是启发式的（自查时它报过一批「文件不存在」，逐条核实**全是误报**，成因是把 `guides/` 里写成本仓库根相对路径的反引号引用当成了真引用）。
**下一轮若采纳**：先治误报清单（对策已定：只认含 `/` 或位于白名单的 token），再挂门禁。

---

## 复核对齐用：taihu 已遵守的上游规则（无需裁决，可直接并入 spec 或标注「已符合」）

这些是初步抽查中确认**已经符合**的上游规则，列在这里是为了避免重复讨论：

| 上游编号 | 规则 | taihu 现状 |
|----------|------|-----------|
| `uber-035` | 生产代码必须避免 panic，出错返回 error | 已符合：`grep -rn 'panic(' internal/ pkg/ cmd/` 在非测试代码里**零命中** |
| `uber-008` | 不要内嵌 mutex，用命名字段 | 已符合：`internal/` 下无非测试结构体内嵌 `sync.Mutex`/`RWMutex` |
| `uber-042` | `init()` 中不得做 I/O | 已符合：唯一的非 cmd `init()`（`internal/transport/frame.go:25`）只做赋值 |
| `uber-053` | `init()` 中不得启动 goroutine | 已符合：全部 12 处 `init()` 均无 goroutine |

> 注：这些是**抽查**结论，不是全量验证。`taihu-spec-inventory.md` 回来后若有出入，以那份为准。
