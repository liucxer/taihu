# ecc + gbp 适用规则 × 现有 spec 去重

> 输入：`triage-ecc.md` 的 40 条 + `triage-gbp.md` 的 32 条 = 72
> 输出：已覆盖 23 / 部分覆盖 23 / 新增 25 / 措辞可改善 1，合计 = 72
>
> 判定依据：`.trellis/spec/` 下 22 个非 guides 文件（3 741 行中的 2 630 行）的**正文**，逐条读，不按
> `taihu-spec-inventory.md` 的一句话索引判（索引会把「提到了」误当「规定了」）。
> `guides/` 层按本轮约定**一律不作已覆盖依据**（它是 Trellis 工具自带内容），因此
> 「DRY / 边界只校验一次 / 抽象门槛」这类只写在 guides 里的通则，在本文里算**未覆盖**。

---

## 一、新增（25 条）

| 来源编号 | 规则一句话 | taihu 锚点 | 拟落 spec 文件 | 为什么现有 spec 没覆盖 |
|----------|-----------|-----------|---------------|----------------------|
| ecc-002 | Accept interfaces, return structs | `internal/storage/storage.go:31`（返结构体）/ `internal/cluster/register.go:11`（收接口） | `architecture/api-surface.md` | 现有 api-surface.md 只规定「标识符默认私有」「对外面文件只放导出内容」，没有任何一条讲**构造函数的签名形态**（收接口 / 返结构体）。`interfaces-and-reexport.md` §2 讲的是「接口何时导出」，不是「参数与返回值该是接口还是具体类型」 |
| ecc-009 | Functional Options 构造对象 | `internal/storage/options.go:6-8`、`internal/device/options.go:6-8` | `architecture/api-surface.md` | 全 spec grep `Option` 只命中 `NewWithOptions` 的举例与 `api-surface.md` 的迁出表，**没有一条规定配置 API 该用变参 Option**。三个 `options.go` 的形态是代码事实，未成文 |
| ecc-011 | 依赖注入用构造函数，不用全局 | `internal/storage/storage.go:31`、`internal/transport/server.go:50` | `architecture/api-surface.md` | 现有只有 `cli/index.md:54`（全局参数经包级 `global` 读取）与 `cli/index.md:60-71`（测试缝隙用包级变量），两者都是**包级可变状态的正例**，方向相反；装配方式（构造函数参数 vs 全局）无任何条款 |
| ecc-021 | KISS：清晰优先于机巧，避免过早优化 | `internal/transport/client.go:117-121`（为正确性停用更快的零拷贝路径） | `architecture/code-style.md` | 全 spec grep `KISS` 零命中。`code-style.md` 管注释/命名/包装/日志/import 五块，没有「取向」类条款 |
| ecc-023 | YAGNI：不预建用不到的抽象与特性 | `internal/transport/batch.go:15`、`internal/cluster/kv.go:8`（最小存储接口） | `architecture/code-style.md` 或 `transport/interfaces-and-reexport.md` | 全 spec grep `YAGNI` 零命中。最接近的「抽象门槛」只在 `guides/code-reuse-thinking-guide.md:87-96`（按约定不计） |
| ecc-024 | 文件 200-400 行典型、800 行软上限 | `internal/device/device.go:813`（813 行，唯一越线） | `architecture/code-style.md` + `architecture/index.md` 的 Quality Check | 全 spec grep `行数` 只命中 `api-surface.md:19` 的 diffstat 举例（「别拿净行数当成效指标」），**没有任何文件长度约束**；`architecture/index.md:35` 的 Quality Check 只列命令 |
| ecc-025 | 每一层显式处理错误，绝不静默吞掉 | `internal/transport/server.go:212` `_ = c.writeFrame(...)`（139 处 `_ =`，无一处注释说明） | `architecture/code-style.md`（并入「非测试代码禁用清单」） | 全 spec grep `忽略` / `吞` 只命中 `metadata-and-compaction.md:106`（`Ref` 对未知 segID 静默忽略——那是**被规定的行为**，不是禁吞错条款）。error-model.md 讲错误体系与包装，不讲「不得丢弃返回值」 |
| ecc-028 | 少深嵌套，尽早 return（guard clause） | `internal/storage/storage.go:245-247`、`:256-259`、`:306` | `architecture/code-style.md` | 全 spec grep `嵌套` 只命中 `file-splitting.md` 的「嵌套 go.mod」与 `buffer-and-concurrency.md:202` 的「不做嵌套持有锁」，都不是控制流嵌套 |
| ecc-029 | 魔法数字改具名常量 | 正例 `internal/layout/layout.go:14`；反例 `internal/metastore/meta.go:31-33`（`b[1:]`/`b[9:]`/`b[17:]` 编码解码各写一遍） | 通则落 `architecture/code-style.md`；磁盘格式那半落 `engine/metadata-and-compaction.md` | **部分场景有**：`engine/index.md:22`（th-117「整盘段数不再硬编码」）禁的是「段数写死」这一个值；`transport/wire-protocol.md:19-33`（th-267）管的是「尺寸常量互相派生」。**通盘看仍算新增**：没有任何一条说「数字字面量必须具名常量」，磁盘编码的字节偏移尤其没有 |
| ecc-030 | 长函数拆成职责单一的小块 | `internal/device/device.go:439`（`AppendBatch` 126 行）、`internal/transport/client.go:121`（`Get` 112 行） | `architecture/code-style.md` | 同 ecc-024：无任何函数长度约束 |
| ecc-036 | 测试结构 AAA（Arrange-Act-Assert） | `internal/storage/storage_test.go:50-60` | `testing/unit-tests.md` | `unit-tests.md` 规则 4 规定表驱动形态、规则 7 规定辅助函数命名，**没有一条讲用例体的三段结构**。triage 自己也标了「事实符合但未成文」 |
| ecc-038 | 提交前硬性检查：无硬编码密钥 | `configs/taihu-server.example.sh:12-16`（全部留空）；`grep -rniE 'secret\|password\|apikey' internal/ pkg/ cmd/` 零命中 | `architecture/code-style.md`（并入「非测试代码禁用清单」） | `code-style.md:28-37` 的禁用清单只有 TODO/FIXME/XXX/nolint，**无凭据项**；`cli/index.md:88` 只说部署模板要同步变量清单，不说不得写死密钥 |
| ecc-064 | 先规划：产出 PRD / design / task_list | 每个 Trellis 任务目录固定 `prd.md` + `design.md` + `implement.md` | **不建议落 taihu spec**（Trellis 工作流已承担） | 现有 spec 里最接近的是 `architecture/index.md:19-31` 的 Pre-Development Checklist，那是「动手前逐条自检」的**检查项**，不是「先产出规划工件」的条款。该要求目前由 `.trellis/workflow.md` 的 task 机制承担，不在 297 条内 |
| ecc-072 | Hook 分三类：PreToolUse / PostToolUse / Stop | `.claude/settings.json:16/38/56`（实际注册了 SessionStart / PreToolUse / UserPromptSubmit） | **不建议落 taihu spec**（Claude Code harness 配置） | 全 spec grep `Hook` 零命中。这是 harness 层配置约定，写进仓库规范会与 `.claude/settings.json` 形成第二事实源 |
| ecc-073 | 自动放行权限慎用，改用 `allowedTools` 白名单 | `.claude/settings.local.json:3`（3 条精确到命令的白名单） | **不建议落 taihu spec**（同上） | 全 spec 无权限模型条款。现状（逐条精确匹配、无通配）是配置事实，不宜抄成仓库规范 |
| ecc-074 | 多步任务用 TodoWrite 跟踪进度 | `.trellis/workflow.md:40` 的 Task System（每任务一个目录 + 5 件工件） | **不建议落 taihu spec**（Trellis 工作流已承担） | `architecture/index.md:19` 的 Pre-Development Checklist 是逐条自检，不是进度外化机制；现有 297 条里没有一条讲任务跟踪 |
| gbp-019 | 永远不要忽略 error 返回值，真不需要时加注释 | 反例 `internal/transport/server_shm_linux.go:427`（`_ = rerr`，上一行刚拿到错误） | `architecture/code-style.md`（与 ecc-025 同条） | 同 ecc-025：全 spec 没有「不得丢弃 error」的条款。`unit-tests.md:185`、`sdk/public-api.md:150` 里的 `_ = c.Close()` 是测试收尾的既定写法，不构成通则 |
| gbp-024 | channel 缓冲应为 0 或 1，大缓冲需书面论证 | 正例 `internal/transport/frame.go:31-32`（`streamInCap = 8` 带论证）；无论证 `pkg/taihu-client/index.go:39`（`make(chan indexItem, 4096)` 一个字都没解释） | `engine/buffer-and-concurrency.md` 或 `transport/wire-protocol.md` | 全 spec grep `chan ` 在非 guides 文件里零命中；`buffer-and-concurrency.md` 管的是 bufpool 的分桶与 `maxKeep`、完成泵的 pending 语义，**没有一条讲 channel 容量怎么定、要不要写理由** |
| gbp-026 | 用 errgroup 简化并发任务与错误传播 | 反例 `internal/benchkit/run.go:168`→`:212`（手写 WaitGroup + err chan）、`pkg/taihu-client/storage.go:221`→`:236`；全仓 errgroup 零命中 | `engine/buffer-and-concurrency.md` | 全 spec grep `errgroup` / `WaitGroup` 零命中。现有条款只覆盖具体并发结构（完成泵退出条件 th-128、batchWait 出口 th-132、锁纪律第五节），没有「fan-out 的错误怎么汇聚」这一层 |
| gbp-033 | 结构体初始化一律用字段名，不用位置字面量 | `internal/transport/server.go:273`、`internal/storage/storage.go:121`、`internal/aio/aio_linux.go:217` | `architecture/code-style.md` | 全 spec 无任何结构体字面量写法条款；`metadata-and-compaction.md` 管的是磁盘编码布局，不是 Go 字面量 |
| gbp-034 | 用函数式选项设计灵活的配置 API | `internal/storage/options.go:7/16-17/23/29`、`internal/device/options.go:7/26/33` | `architecture/api-surface.md`（与 ecc-009 同条） | 同 ecc-009 |
| gbp-037 | 利用零值可用性，让自定义类型的零值有意义 | `internal/transport/server.go:45`、`internal/storage/options.go:16-17`、`internal/cluster/client.go:39`、`internal/transport/batch.go:28` | `architecture/code-style.md` | 全 spec grep `零值` **零命中**。`api-surface.md`、`code-style.md` 都没提类型的零值语义 |
| gbp-038 | 用类型嵌入做组合，但不要在公开 API 里嵌入 | 全仓无导出结构体嵌入；嵌入只在 `cmd/taihu/cmd/bench_single.go:106`、`pkg/taihu-client/testutil_test.go:15-16` | `architecture/api-surface.md` | 现有唯一涉及嵌入的条款是 `sdk/public-api.md:137-148`（共享 fake 用「嵌入接口 + 按需注入错误」），那是**测试 fake 的写法**，不是「公开 API 不得嵌入」 |
| gbp-047 | 基础类型转换用 strconv 而不是 fmt | 正例 `internal/aio/aio_uring_linux.go:406`、`cmd/taihu/cmd/server.go:101`；反例 `cmd/taihu/cmd/helpers.go:137` | `cli/command-and-output.md` 或 `architecture/code-style.md` | 全 spec grep `strconv` **零命中**。`command-and-output.md:91` 只规定「字节数一律过 humanBytes，不要自己除 1024」，不涉及 `%d` 与 strconv 的取舍 |
| gbp-048 | 已知大小时预分配 slice 与 map 容量 | `internal/cluster/register.go:52`、`internal/aio/aio_linux.go:215`、`internal/metastore/kv_pebble.go:171`、`pkg/taihu-client/registry.go:21` | `architecture/code-style.md` 或 `engine/buffer-and-concurrency.md` | 全 spec grep `预分配` / `make([]` 零命中。现有性能类条款集中在 bufpool 分桶与 compaction 阈值，无内存预分配约定 |
| gbp-053 | 用 revive 做可配置 lint，含复杂度/长度上限 | 反例：全仓无 `revive.toml`，且**无任何**复杂度/函数长度/参数个数门禁 | `architecture/index.md` 的 Quality Check 节（与 ecc-024/030/031 同一批） | 门禁类条款（th-026 / th-207 / th-217）只枚举了现有的四条命令，**全文没有任何代码度量上限**；这条若并入等于新增一类治理规则 |

**新增一栏的说明**（三条不属于「写进 taihu spec 就有收益」的）：

- `ecc-064` / `ecc-072` / `ecc-073` / `ecc-074` 的「未覆盖」是**结构性的**：它们要规范的对象是 Trellis 任务机制与 Claude Code harness 配置，不是 taihu 代码。triage 把它们判为「适用」是因为能在仓库里指出锚点（Trellis 工件确实在跑、`.claude/settings*.json` 确实注册了三类 hook），但锚点存在 ≠ 该写进 `.trellis/spec/`。建议在最终 spec 里**只保留一句指向**（如 `architecture/index.md` 的 Pre-Development Checklist 里加一条「任务规划工件见 .trellis/workflow.md」），不另立规则。
- `ecc-029`：我原本判「新增」，因为通盘确实没有「数字必须具名常量」的规则；但 `engine/index.md:22`（th-117）已在「段数」这一处禁了硬编码。两栏都说得通，**最终放新增**，理由是 th-117 管的是「值从设备算出」，不是「字面量必须具名」——落条时应避免与 th-117 重复。

---

## 二、部分覆盖（23 条）

| 来源编号 | 规则一句话 | 现有规则 | 缺的是哪部分 |
|----------|-----------|---------|-------------|
| ecc-015 | 一律 `context.Context` + `WithTimeout`，`defer cancel()` | `cli/index.md:54`（规则 3：「超时统一用 `ctxWithTimeout`」，`root.go:111-113`） | 现有条款**只管 CLI 层**。缺「所有阻塞 / 跨进程调用必须接 `ctx` 并从父 ctx 派生超时，不得自造 `context.Background()` 断链」这条通则——`internal/transport/server.go:83` 正是反例（已在 `conflicts.md` C-03 立卷） |
| ecc-017 | 测试必须带 `-race` | `testing/e2e-tests.md:103-111`（规则 6：e2e **不使用** `-race`，给了 aarch64 链接失败的理由） | 现有条款只处理了 e2e 侧且结论是「禁用」。缺「单测 / 开发循环应跑 `-race`」以及是否进 `make check` 的裁决。**注意方向**：采纳等于给 `Makefile` 加目标，属工具链决策（triage 已标为需人裁决） |
| ecc-022 | DRY：重复逻辑抽公共函数 | `cli/index.md:31`（规则 5：压测优先复用 `benchkit.Run`）、`cli/command-and-output.md:97`（规则 11）、`:101`（规则 13：不得产出第三份副本） | 现有三条都是**压测域的实例条款**。缺全仓通则（「发现自己在抄另一处逻辑 → 抽公共实现」）。该通则目前只在 `guides/code-reuse-thinking-guide.md:36-38`，按本轮约定不计 |
| ecc-026 | 只在系统边界做输入校验，schema 化校验，快速失败 | `transport/wire-protocol.md:54-70`（th-269：`Parse*` 先校验长度声明再分配）、`cli/index.md:90`（th-096：集中 `validate()`，`RunE` 先调再干活） | 覆盖了「协议解码边界」与「CLI 入参边界」两处**具体边界**的校验写法。缺「校验只做在系统边界、内部不重复校验」与「校验入口 schema 化」的通则（「Validate once at the entry point」目前只在 `guides/cross-layer-thinking-guide.md:66`） |
| ecc-029 | 魔法数字改具名常量 | `engine/index.md:22`（th-117：整盘段数不得硬编码）、`transport/wire-protocol.md:19-33`（th-267：尺寸常量互相派生） | 已有两条都是**单点约束**（段数、协议尺寸常量）。缺通用条款（数字/字符串字面量一律具名常量），磁盘编码的字节偏移（`meta.go:31-33` / `:43-45`）尤其无规可依 |
| ecc-031 | 收尾质量检查单（函数 <50 行、文件 <800 行、嵌套 ≤4、有错误处理、无硬编码、无 mutation） | `architecture/index.md:35-63` `## Quality Check`（改完必跑 `make check` / `make check-linux` + 两条分层复核 grep） | 「收尾检查单」这个**框架**已经存在，但里面全是命令，**判据一条都没有**（行数、嵌套、错误处理、硬编码、mutation）。缺的是清单项本身。另：mutation 一项与零拷贝前提冲突，见 `conflicts.md` C-02 |
| ecc-033 | 三类测试全部必需：单元 / 集成 / E2E | `testing/index.md:3`（明文「本仓库的测试分**两级**」，且解释了集成语义被 `test/e2e/` 吸收）、`testing/e2e-tests.md:7-16` | 现有条款给出了**两级**的边界与理由，等于只覆盖了「单元 + E2E」两级。缺「集成测试」这一独立形态——taihu 是刻意不设（e2e 同时起真实 server + 裸盘 + TiKV），落条时应写成刻意偏离而不是补一层目录 |
| ecc-037 | 测试命名描述行为而非实现 | `testing/unit-tests.md:79-96`（th-245：表驱动用例名是中文短句）、`testing/e2e-tests.md:43-47`（th-257：`<字母><数字><CamelCase>` + 组号对应） | 已有两条都是**命名格式**约定。缺「名字要讲断言的不变式 / 行为而不是实现」这条判据（triage 指出部分表驱动用例名描述的是**输入**：「空 key 零长度」） |
| ecc-039 | 所有用户输入必须校验 | 同 ecc-026：`transport/wire-protocol.md:54-70`、`cli/index.md:90` | 缺「凡是外部输入（协议负载、CLI flag、SDK 入参）都必须校验、且失败即返回错误」的通则。现有条款是按入口各写一条，没有「所有入口」的覆盖承诺（`internal/storage` 的入参校验就无条款） |
| ecc-048 | 强制评审触发点（写完代码后 / 合并前 / 安全敏感 / 架构改动） | `architecture/index.md:35-63`（Quality Check：改完必跑的机器门禁） | 覆盖了「评审要执行什么」。缺「评审是必过环节、什么时点触发」——Trellis 的 implement → check 派发在 `.claude/agents/trellis-check.md` 与 workflow 里，**297 条 spec 内没有对应条款** |
| ecc-050 | 评审检查单（可读、<50 行、内聚、≤4 层嵌套、错误显式、无密钥、无调试语句、有新测试、覆盖率 ≥80%） | `architecture/index.md:35-63` `## Quality Check` | 同 ecc-031：检查单的**壳**在，**判据全缺**。其中「覆盖率 ≥80%」是否采纳另见 ecc-032（triage 判不适用）与 gbp-040（判不适用）——仓库不按百分比管理覆盖率 |
| ecc-055 | 重点排查项（>50 行函数、>800 行文件、>4 层嵌套、缺错误处理、mutation、缺测试） | `architecture/index.md:35-63` | 与 ecc-031 / ecc-050 是**同一条上游内容的不同切面**（收尾检查单 / 评审检查单 / 排查项），三条在 taihu 侧都指向同一个缺口：`architecture/index.md` 的 Quality Check 缺代码度量判据。落条时建议**合并成一条**，不要产出三份平行清单 |
| ecc-057 | Repository 模式：数据访问藏在统一接口后 | `transport/interfaces-and-reexport.md:55-76`（§3 编译期断言是常态，表里含 `var _ Store = (*pebbleStore)(nil)`）、`engine/metadata-and-compaction.md:43-65`（`Store` 逐方法契约） | 现有条款把「实现必须满足统一接口」写实了，但**没有一条说「数据访问一律经 `Store` 抽象、不得绕过直连 pebble」**。triage 也提醒不宜按字面引入 `findAll/findById` 命名（taihu 无 domain 层） |
| ecc-066 | 写完代码立刻评审，先处理 CRITICAL/HIGH 再 MEDIUM | 同 ecc-048：`architecture/index.md:35-63` | 缺「时序」这一半（评审紧跟实现，不是可选项）。至于 CRITICAL/HIGH/MEDIUM 分级本身属 ecc-052，triage 已判不适用 |
| gbp-023 | 正确使用 channel：超时、非阻塞、fan-out/fan-in | `transport/wire-protocol.md:163`（th-277：流通道 `in` 永不 close，只由 `finish()` 关 `done`）、`engine/buffer-and-concurrency.md:138`（th-132：`batchWait` 多一个 `<-d.pumpDone` 出口） | 已有的是**逐个 channel 的具体语义**（生命周期、出口）。缺 channel 使用的通则：`select` 超时 / 非阻塞回投 / fan-out 收尾的标准形态与容量选择 |
| gbp-025 | 用 context 传播取消信号与超时 | `cli/index.md:54`（同 ecc-015） | 同 ecc-015：只有 CLI 层一条，缺全仓通则与「子超时从父 ctx 派生」这条（`server.go:83` 断开父子链，C-03） |
| gbp-027 | 正确使用 sync 包原语（Mutex / RWMutex / Once / Pool / Map） | `engine/buffer-and-concurrency.md:83-93`（th-121：`bufpool` 刻意否决 `sync.Pool` 并有实测理由）、`:135-139`（th-133：`Device.mu` 保护字段写在声明处）、第五节锁纪律（th-149~151） | Mutex 与 Pool 两侧覆盖充分（含一条有实测依据的偏离）。缺 `sync.RWMutex` / `sync.Once` / `sync.Map` 的使用约定——`frame.go:51` 的 `once`、`kv_mem.go:12` 的 `RWMutex`、`aio_uring_linux.go:184` 的 `uringOverflowOnce` 都无条款 |
| gbp-028 | 用 `go test -race` 检测竞争，CI 必须开启 | `testing/e2e-tests.md:103-111`（规则 6） | 同 ecc-017：缺单测侧 `-race` 与门禁。**另缺**上游的「CI 必须开启」半边——taihu 无 CI（`conflicts.md` 与 triage 的 ecc-049/067 已判不适用），落条时须显式写明这是刻意偏离 |
| gbp-032 | 接收者命名 1-2 字母，同一类型不混用值/指针接收者 | `architecture/code-style.md:47-56`（规则 5：receiver 用单字母、同一类型恒定同一字母，带实测表） | naming 那一半覆盖得很实（有逐包统计表）。缺「同一类型不得混用值接收者与指针接收者」这半句——规则 5 管的是**字母**恒定，不管接收者**种类** |
| gbp-035 | 用 defer 做资源释放与解锁，注意 LIFO 与求值时机 | `engine/buffer-and-concurrency.md:44-51`（`Device.Append` 的 `defer bufpool.Put(buf)` 并解释为何安全）、`:51`（Compactor 同形）、`testing/unit-tests.md:176`（**测试**里用 `t.Cleanup` 而不是 `defer`） | 现有条款都是**具体场景的 defer 用法**（且带理由），方向对。缺通则（资源释放/解锁一律 defer）与上游点名的那两个陷阱：LIFO 顺序、`defer` 参数的求值时机。测试侧 `t.Cleanup` 的取舍也应一并写明，免得两条看起来打架 |
| gbp-039 | 正确使用空白标识符 `_`（忽略返回值、副作用导入、编译期断言） | `transport/interfaces-and-reexport.md:55-76`（th-290：`var _ 接口 = (*实现)(nil)` 是常态，列了 7 处） | 编译期断言那一半覆盖得很好。缺「忽略返回值必须写注释说明为什么可以忽略」这一半（与 gbp-019 / ecc-025 同源，反例 `server_shm_linux.go:427`） |
| gbp-049 | 用 golangci-lint 做综合检查并给出推荐配置 | `architecture/layering.md:58-67`（th-026：`make check` 是提交前唯一必跑的综合门禁）、`platform/build-verification.md:7-26`（th-207 双平台 vet） | 「综合检查入口」这个主题已有，且定义得比上游更具体（四条命令 + 双平台）。缺的是 linter 覆盖面：errcheck / gosimple / staticcheck / revive 这几类在 taihu 适用，而 noctx / bodyclose / sqlclosecheck / gosec 这类服务向 linter 明确不适用。**是否引入需人裁决**（`prd.md:71` 本轮不引入新工具） |
| gbp-052 | 用 staticcheck 做深度静态检查 | `architecture/layering.md:58-67`（go vet 进 `make check`）、`platform/build-verification.md:7-26`（darwin + linux 两个视角各一次 vet）、`architecture/code-style.md:28-37`（禁止用 `//nolint` 压 vet 告警） | 「静态检查」主题已有且口径比上游严（双平台 + 禁 nolint）。缺 staticcheck 这一**层**（比 vet 深的那类问题）。同 gbp-049，属工具链决策 |

**部分覆盖一栏的集中说明**：

- `ecc-031` / `ecc-050` / `ecc-055` 是**同一条上游内容的三次切分**，在 taihu 侧指向同一个缺口（`architecture/index.md:35` 的 Quality Check 只有命令、没有度量判据）。三行判定相同不是重复劳动，而是提醒：**落条时只写一条**。
- `ecc-026` / `ecc-039` 同理（「校验」的两个切面，缺的都是同一句通则）。
- `ecc-015` / `gbp-025`、`ecc-017` / `gbp-028`、`ecc-048` / `ecc-066` 也都是成对出现。

---

## 三、措辞可改善（1 条）

| 来源编号 | 规则一句话 | 现有规则 | 上游的表述好在哪 |
|----------|-----------|---------|-----------------|
| ecc-060 | 提交信息 `<type>: <description>`，type ∈ feat/fix/refactor/docs/test/chore/perf/ci | `architecture/commits.md:7-15`（th-072：标题格式 `type(scope): <中文标题>` + 「已用过的 type：`refactor`(11)、`fix`(7)、`perf`(6)、`docs`(6)、`feat`(5)、`chore`(4)、`style`(1)」） | 上游给的是**封闭词表**（规范该长什么样），taihu 给的是**实测统计快照**（规范曾经长什么样）——后者会随 git log 变化而失真，**且现在已经失真**：清单里没有 `test`，而 `fed67b4 test: 补齐 82 个单测文件…` 就是 `test:`；`revert`(3) 也不在表里。改写方向是「把统计表换成允许集合」，并把 taihu 实际在用的 `style` / `revert` 一并纳入（不要照抄上游那 8 个字面量），同时保留 th-072 已有的「中文标题 + scope 用包名」两条硬约定 |

**为什么只有 1 条**：其余「同主题」条目都够不上这一栏，因为它们缺的是**内容**而不是**措辞**（ecc-050 / ecc-031 / ecc-055 缺判据、ecc-015 缺通则、ecc-033 缺一层测试、gbp-032 缺半句约束），已归入「部分覆盖」。另有两条接近但被我挡回的：

- `ecc-004` / `gbp-016`（错误包装）：现有 `code-style.md:79-88`（th-049）的**正文与它自己举的正例自相矛盾**（`:84` 引的 `fmt.Errorf("DialPoolMulti: empty addrs")` 是大写前缀，违反同一条的「op 前缀小写英文」；index 的 §2.2 已把 th-049 列为 8 处反例）。这不是「上游表述更好」——上游那条更宽（不要求小写），照它改会丢掉一条有用的约束；正确动作是**修 th-049 自己的正例**，属 spec 内部一致性修复，不属本栏。
- `ecc-001` / `gbp-050`（gofmt/goimports）：`cli/index.md:96`（th-097）把 gofmt 写成「**本层**所有 .go」，而全仓口径只在 `architecture/index.md:40` 的 `make check` 输出里隐含。这是 scope 写法问题，但 `architecture/layering.md:58-67`（th-026）已经把 `make check`（含 check-fmt）定成提交前唯一必跑命令，全仓口径实际成立，故仍归「已覆盖」。

---

## 四、已覆盖（23 条）

| 来源编号 | 规则一句话 | 对应现有规则 | 为什么算同一条 |
|----------|-----------|-------------|---------------|
| ecc-001 | `gofmt` / `goimports` 强制，不讨论风格 | `architecture/layering.md:58-67`（th-026：提交前必跑 `make check` = check-fmt + check-layering + check-sdk-only + go vet）；`cli/index.md:96`（th-097：所有 .go 必须 gofmt 干净，`third_party/` 例外）；`architecture/code-style.md:159-181`（th-058：import 三段式分组） | 「格式不得讨论、必须机器干净」的要求已由 `make check` 的 check-fmt 子目标写死，且 import 分组另有明文规则。**唯一不在覆盖内的是 `goimports` 这个工具本身**（全仓零采用、`code-style.md:181` 自认分组靠人守）——那是自动化程度的差异，不是规则缺失 |
| ecc-003 | 接口保持小（1-3 方法） | `transport/interfaces-and-reexport.md:7-37`（th-287：内部接缝必须是小接口（2~6 方法）、未导出、声明在使用它的文件里） | 同一条要求（接口要窄），taihu 只把上界定为 6 且限「内部接缝」。差异是刻意的：`cluster.KV`(8) / `aio.Ring`(6) / `metastore.Store`(13) 是**能力集**抽象，注释里写明了与 client-go 具体类型逐一对齐的理由 |
| ecc-004 | 错误一律 `fmt.Errorf("...: %w", err)` 包裹上下文 | `architecture/code-style.md:79-88`（th-049：`fmt.Errorf("op: %w", err)`，op 前缀小写英文、不加句号）；`architecture/error-model.md:36-50`（th-061：区分错误类别用 sentinel + `errors.Is`） | 逐字同一条。triage 附带的「`compact.go:152/176` 用 `==` 而非 `errors.Is`」那半个缺口，th-061 也已规定 |
| ecc-006 | PostToolUse hook：改 `.go` 后自动 gofmt/goimports | `architecture/layering.md:58-67`（th-026）+ `cli/index.md:96`（th-097） | **要求同一、机制不同**：上游靠 harness hook 兜底，taihu 靠 `make check` 门禁兜底，两者都是「改完 .go 不可能留下未格式化的代码」。仓库刻意不用 PostToolUse（`.claude/settings.json` 只注册 SessionStart / PreToolUse / UserPromptSubmit） |
| ecc-007 | PostToolUse hook：改 `.go` 后跑 `go vet` | `architecture/layering.md:58-67`（th-026 末行 `go vet ./...`）+ `platform/build-verification.md:7-26`（th-207：darwin 与 linux 两个视角各跑一次 vet） | 同 ecc-006：要求同一（改完必须过 vet），taihu 的覆盖面比上游更宽（跨平台双视角） |
| ecc-010 | 接口定义在使用方，而非实现方 | `transport/interfaces-and-reexport.md:7-26`（th-287：「声明在使用它的文件里」）；`:39-53`（§2 把反例 `ObjectStore` 声明在实现侧的理由写死了） | th-287 的「声明在使用它的文件里」就是「定义在使用方」。跨模块那处偏离（`rpcclient.ObjectStore`，注释理由是反向声明会成环）已被 spec 明文记录并用两侧 `var _` 断言兜住，属已自洽处理 |
| ecc-016 | 标准 `go test` + 表驱动测试 | `testing/unit-tests.md:77-114`（th-245：表驱动 `cases := []struct{...}` + `t.Run(` 是逻辑/纯函数测试的默认形态）；`testing/index.md:11`（标准库 `testing` 一手到底） | 表驱动与「标准库一手到底」两侧都有明文条款，且给了实例与反例边界（纯映射类可省 `name`） |
| ecc-027 | 命名自解释；缩写全大写；常量/类型视觉可分 | `architecture/code-style.md:58-65`（th-046：首字母缩写全大写，不写 `ClientId`/`Url`/`Json`，带 grep 判据） | 这条可判定的部分就是「缩写怎么办」，th-046 逐字覆盖并给了机械判据。「命名自解释」「视觉可分」无判据、无从违反，不另计 |
| ecc-045 | 错误信息不得泄露敏感数据 | `architecture/error-model.md:72-97`（th-066：跨进程只传 4 字节大端 code，不传 error 字符串）；`transport/wire-protocol.md:72-101`（th-271 同口径 + 三个映射函数的用法边界） | 同一条且更强：taihu 不是「注意别泄露」，而是**结构上不传字符串** |
| ecc-062 | 不自动添加 `Co-Authored-By` 尾注 | `architecture/commits.md:119-129`（th-082：本仓库不做自动提交，`session_auto_commit: false`，「提交信息是人工产物，格式由人保证」） | 同一条：上游禁的是「工具擅自加 trailer」，taihu 禁的是「工具擅自提交」——后者的范围覆盖前者。实证 `git log --format=%B \| grep -ci 'co-authored-by'` = 0 |
| gbp-016 | 用 `fmt.Errorf("...: %w", err)` 包装错误保留调用链 | `architecture/code-style.md:79-88`（th-049） | 与 ecc-004 同一条 |
| gbp-017 | 用包级哨兵错误表达可预期错误条件 | `architecture/error-model.md:7-32`（th-059：`internal/ierr` 是唯一事实源，6 个 sentinel 逐个列表）；`:54-66`（th-062：包内控制信号用未导出 sentinel，命名一律 `err` 前缀） | 逐字覆盖且更严：taihu 把「哪一层用哪种 sentinel」也定死了（导出的进 `ierr`、包内的用小写未导出、平台桩各持一份是刻意的） |
| gbp-018 | 需要携带额外信息时定义自定义错误类型 | `architecture/error-model.md:36-50`（th-061：不得新增**导出**的自定义 error 类型；需要诊断细节时用未导出类型） | 同一条。taihu 收窄了那一半（导出类型被刻意否决，`uringParamError` 是唯一实例且不导出），这不是「没覆盖」而是**有理由的收紧**，spec 原文写明了 |
| gbp-021 | panic 仅用于不可恢复错误，recover 只在 defer 中有效 | `architecture/code-style.md:39-41`（规则 4：非测试代码不 `panic()`，库代码一律返回 `error`，带 grep 判据） | 「panic 只用于不可恢复」的落地形态就是「非测试代码零 panic」。recover 那半在 taihu 无落点（无框架边界），非测试代码 recover 零命中 |
| gbp-022 | 每个 goroutine 都必须有明确退出条件，避免泄漏 | `sdk/index.md:88`（th-223：新增后台 goroutine 时确认 `Close()` 能停掉它）；`engine/buffer-and-concurrency.md:122-140`（th-128：完成泵的唯一性与退出条件 `closed && m 空 && inSubmit==0`）；`engine/metadata-and-compaction.md:193`（th-196：`Stop` 必须 `close(c.stop)` + `wg.Wait()` 后才返回） | 同一条，且给出的是**标准形态**（`stop` channel + `WaitGroup` + `close` 后 `Wait`）与逐处的退出条件，比上游的一句话更可执行 |
| gbp-029 | 遵循 Go 命名惯例（包名、缩写、接口 `-er`） | `architecture/code-style.md:58-65`（th-046：缩写全大写）；`sdk/index.md:10-21`（th-215：目录名 `taihu-client` 与包名 `taihuclient` 的不一致必须保持，改包名 = 改对外契约） | 包名与缩写两半都有明文条款且带判据。「接口 `-er`」这半无落点（taihu 的 `ObjectStore` / `Ring` / `KV` / `Store` 都不用 `-er`），但那是命名风格偏好、无判据，不另计 |
| gbp-030 | 按 Go 惯例写文档注释，错误字符串小写无句点 | `architecture/code-style.md:79-88`（th-049：op 前缀小写英文、不加句号）；`architecture/error-model.md:54-66`（th-062 命名 `err` 前缀 + th-064：名字不足以自解释时必须写文档注释）；`architecture/code-style.md:9-17`（th-040：注释讲「为什么」） | 错误字符串小写无句点在 th-049 逐字；文档注释的写法在 th-040 / th-064（`error-model.md:66` 明说「不强制每条都配，但名字不自解释时必须写」）。注释**语言**是中文是 th-040 的刻意约定，与上游不冲突 |
| gbp-031 | 定义小接口并按需组合（接口隔离），接口由消费方定义 | `transport/interfaces-and-reexport.md:7-37`（th-287：小接口 + 声明在使用方）；`:55-76`（th-290：`var _ 接口 = (*实现)(nil)` 是常态，新增实现第一件事是补断言） | 主干（小接口 + 编译期断言）逐字覆盖；「接口由消费方定义」在内部接缝上是硬正例，跨模块那处偏离有代码注释里的理由并被断言兜住（见 ecc-010） |
| gbp-041 | 用表驱动测试提升可维护性 | `testing/unit-tests.md:77-114`（th-245） | 与 ecc-016 同一条，逐字 |
| gbp-042 | 通过接口抽象 + mockgen 做依赖注入与 mock 测试 | `testing/unit-tests.md:190-205`（规则 7：共享 fake 放 `testutil_test.go`，用「嵌入接口 + 按需注入错误」写法）；`transport/interfaces-and-reexport.md:161-166`（th-297：测试必须用假 TiKV 接口注入错误路径）；`sdk/public-api.md:152`（th-240：外部后端走间接层变量） | 上游规则本身给了 mockgen 与手写两条路，taihu 走的正是**手写那条**，且形态被三条 spec 条款写死（嵌入接口、只覆盖打断的路径、只允许出现在 `_test.go`）。所以是「已覆盖」而非「缺 mockgen」 |
| gbp-043 | 测试辅助函数必须调 `t.Helper()`，校验逻辑留在测试里 | `testing/unit-tests.md:176`（th-250：辅助函数体内第一句恒为 `t.Helper()`，带实例 `transport_shm_test.go:71-79`） | 逐字同一条。triage 的 2 处反例（`f_soak_test.go:223/230`）落在 e2e 层——那是 th-250 的**作用域问题**（规则只在 `unit-tests.md` 声明，e2e 文件未复述），补一句「本规则同样适用于 `test/e2e/`」即可，不必新增规则 |
| gbp-050 | 用 gofmt + goimports 保持格式一致并分组 import | `cli/index.md:96`（th-097 gofmt 干净、`third_party/` 例外）；`architecture/code-style.md:159-181`（th-058：import 三段式分组，含 `third_party/` 归第三方组的判据）；`platform/file-splitting.md:163-174`（th-205：`third_party/` 免 gofmt） | 与 ecc-001 同一条：gofmt 有机器门禁、import 分组有明文规则（且 spec 自己提醒「`make check-fmt` 不会替你发现分组错了」）。`goimports` 工具未采用是事实，但规则要求（格式一致 + 分组）已覆盖 |
| gbp-051 | 用 `go vet` 做静态分析发现潜在 bug | `architecture/layering.md:58-67`（th-026：`make check` 末行 `go vet ./...`）；`platform/build-verification.md:7-26`（th-207：`check-linux` 第一条就是 linux 视角的 vet）；`architecture/code-style.md:28-37`（禁 `//nolint` 压 vet 告警） | 逐字覆盖且比上游更严（双平台各一遍 vet + 禁止压告警 + 改平台代码必跑 `check-linux`） |

**已覆盖一栏里值得注意的两点**：

- `ecc-006` / `ecc-007` 判「已覆盖」的依据是**要求**而不是**机制**。若本轮的意图是「把 ECC 的 hook 写法搬进 taihu」，那两条应改判「新增（工具链）」，并在 `prd.md:71`「本轮不引入新工具」的约束下不予采纳。我这里按「要求是否已被现有 spec 表达」判。
- `gbp-042` / `gbp-050` 都涉及「上游提到一个 taihu 没用的工具（mockgen / goimports）」，但两条上游规则**自身都含不带该工具的路径**（手写 mock / 人守分组），taihu 走的是那条路径且已被 spec 写死，故判已覆盖。

---

## 附：自检

### 1. 四类合计 = 72

| 类别 | ecc | gbp | 合计 |
|------|-----|-----|------|
| 已覆盖 | 10 | 13 | **23** |
| 部分覆盖 | 14 | 9 | **23** |
| 新增 | 15 | 10 | **25** |
| 措辞可改善 | 1 | 0 | **1** |
| **合计** | **40** | **32** | **72** ✅ |

逐条编号核对（对着 `triage-ecc.md` §一、`triage-gbp.md` §一 的清单逐个数）：

- **ecc 40 条**：001,002,003,004,006,007,009,010,011,015,016,017,021,022,023,024,025,026,027,028,029,030,031,033,036,037,038,039,045,048,050,055,057,060,062,064,066,072,073,074
  - 已覆盖 10：001,003,004,006,007,010,016,027,045,062
  - 部分覆盖 14：015,017,022,026,029,031,033,037,039,048,050,055,057,066
  - 新增 15：002,009,011,021,023,024,025,028,030,036,038,064,072,073,074
  - 措辞 1：060
  - 10+14+15+1 = 40 ✅，且每个编号只出现一次
- **gbp 32 条**：016,017,018,019,021,022,023,024,025,026,027,028,029,030,031,032,033,034,035,037,038,039,041,042,043,047,048,049,050,051,052,053
  - 已覆盖 13：016,017,018,021,022,029,030,031,041,042,043,050,051
  - 部分覆盖 9：023,025,027,028,032,035,039,049,052
  - 新增 10：019,024,026,033,034,037,038,047,048,053
  - 措辞 0
  - 13+9+10 = 32 ✅，且每个编号只出现一次

### 2. 我读过正文的 spec 文件（22 个，guides 层 3 个按约定未计入覆盖依据）

| 层 | 文件 | 读法 |
|---|---|---|
| architecture | `index.md`、`code-style.md`、`error-model.md`、`commits.md`、`layering.md`、`api-surface.md` | 全文逐行 |
| cli | `index.md`、`command-and-output.md` | 全文逐行 |
| engine | `index.md`、`buffer-and-concurrency.md` | 全文逐行 |
| platform | `index.md`、`build-verification.md`、`file-splitting.md` | 全文逐行 |
| sdk | `index.md`、`public-api.md` | 全文逐行 |
| testing | `index.md`、`unit-tests.md`、`e2e-tests.md` | 全文逐行 |
| transport | `index.md`、`interfaces-and-reexport.md` | 全文逐行 |
| transport | `wire-protocol.md` | 定向读（§1 纯函数、§2 常量、§4 校验顺序、§5 错误码、§9 dispatch 所有权、§15 shm 降级、§16 链路统计）—— 该文件 296 行，其余是帧格式细节，与本 72 条无关 |
| engine | `metadata-and-compaction.md` | 定向读（§Store 契约 43-65、§段状态机 81-106、§compaction 188-197）—— 其余是段状态迁移细节 |

另外为确认「未覆盖」，在 spec 全库（排除 `guides/`）grep 过以下关键词，全部**零命中或只命中无关语境**：`Option`（仅 `NewWithOptions` 举例）、`WithTimeout`（仅 `cli/index.md:54`）、`零值`（0）、`嵌入`（仅测试 fake 两处）、`忽略`（仅「静默忽略」的被规定行为）、`Hook`（0）、`覆盖率`/`coverage`（0）、`chan `（0）、`errgroup`（0）、`WaitGroup`（0）、`sync.Once`/`RWMutex`（0）、`goimports`（0）、`魔法数字`（0）、`KISS`/`YAGNI`（0）、`嵌套`（仅「嵌套 go.mod」「嵌套持有锁」）、`行数`（仅 diffstat 举例）、`预分配`（0）、`strconv`（0）。

### 3. 我拿不准的条目

| 编号 | 我判的 | 拿不准什么 |
|------|--------|-----------|
| ecc-002 | 新增 | 我按「没有一条规定构造函数签名形态」判新增。若认为 `interfaces-and-reexport.md` §2「只在跨模块边界导出接口」已隐含同一取向，应改判部分覆盖。**倾向维持新增**：那条管的是接口的可见性，不管参数该收接口还是具体类型 |
| ecc-003 | 已覆盖 | th-287 的上界是「2~6 方法」而上游是「1~3」，且 taihu 的三个导出接口（`KV` 8 / `Store` 13）明确超出。若审阅者认为「导出接口不受 th-287 约束就等于没这条规则」，应改判部分覆盖 |
| ecc-010 | 已覆盖 | 同 ecc-003：th-287 的「声明在使用它的文件里」限定在**内部接缝**，跨模块那处是刻意反例（有理由、有断言兜底）。我判已覆盖是因为偏离已被 spec 自洽记录；若要求规则必须无例外，应改判部分覆盖 |
| ecc-023 | 新增 | triage 自己标了这条锚点偏弱（证明的是「没有多做事」，不是「不预建抽象」）。我按「spec 里没有任何 YAGNI 条款」判新增，但沿用了这个弱锚点。**若不接受该锚点，本条应退回不适用** |
| ecc-026 / ecc-039 | 部分覆盖 | 两条的「缺口」都落在 guides 层（`cross-layer-thinking-guide.md:66` 的 Validate once at the entry point）。按本轮约定 guides 不计，所以是缺口；若审阅者认为 guides 层算数，两条都应改判已覆盖。**这正是 guides 层定位问题的一个具体体现** |
| ecc-029 | 新增 | th-117（「整盘段数不得硬编码」）与这条方向一致、只是窄。已覆盖 / 部分覆盖 / 新增三档都说得通，我在正文两处都标了。**倾向新增 + 与 th-117 合并改写** |
| ecc-031 / ecc-050 / ecc-055 | 部分覆盖（三条） | 三条是同一条上游内容的三次切分，判定必须一致。我把它们都判「部分覆盖」是因为 `architecture/index.md:35` 的 `## Quality Check` 确实是「收尾质量检查」这个主题的现有载体、只是缺判据。若审阅者认为「只有命令、没有判据」等于没有这条规则，三条应改判新增（总数变成 已覆盖 23 / 部分覆盖 20 / 新增 28 / 措辞 1） |
| ecc-033 | 部分覆盖 | taihu 是**两级**测试且明文解释过，与上游「三类必需」的字面有张力。我判部分覆盖（缺 integration 一层）；若按 triage 的建议「集成语义被 e2e 吸收、不宜按字面再开一层」，也可判已覆盖 |
| ecc-048 / ecc-066 | 部分覆盖 | 现有载体是 `architecture/index.md:35` 的 Quality Check（机器门禁）与 `.claude/agents/trellis-check.md`（非 spec 文件）。若审阅者认为评审流程属 Trellis 工作流、不该进 spec，两条应改判不适用或新增（不落 spec） |
| ecc-064 / ecc-072 / ecc-073 / ecc-074 | 新增 | 四条我判新增（现有 spec 确实没有），但**拟落 spec = 不建议落**——它们是 Trellis / harness 层机制，已在运行。若审阅者认为「已被工具承担 = 已覆盖」，四条应改判已覆盖（总数变成 已覆盖 27 / 新增 21） |
| ecc-057 | 部分覆盖 | Repository 模式在 taihu 是**结构同形但无 domain 层**（triage 已标拿不准）。我按「有 `Store` 抽象但没有『不得绕过』条款」判部分覆盖；若认为「有接口 + 有实现 + 有断言」即等价于该模式，应改判已覆盖 |
| gbp-023 / gbp-027 | 部分覆盖 | 两条都是「上游给一个宽泛主题（channel 用法 / sync 原语），taihu 有若干具体条款」。判部分覆盖是因为通则确实缺；若审阅者按「主题已触及即算覆盖」的口径，两条应改判已覆盖 |
| gbp-026 | 新增 | triage 自己标了这是边界情形（锚点是「未采用某库」而非「做错了」）。我判新增（缺「fan-out 错误怎么汇聚」的条款）；若审阅者认为「不用 errgroup」本身不构成缺口，本条应退回不适用 |
| gbp-032 | 部分覆盖 | 只缺「不得混用值/指针接收者」半句。若认为 th-045 的「同一类型恒定同一字母」已足以涵盖接收者的一致性，应改判已覆盖 |
| gbp-035 | 部分覆盖 | 现有条款都是具体场景的 defer 用法（且方向一致），缺的是通则与 LIFO/求值时机陷阱。若认为「用法已被规定」即算覆盖，应改判已覆盖 |
| gbp-038 | 新增 | 现有唯一涉及嵌入的条款（`sdk/public-api.md:137`）是测试 fake 的写法，不是「公开 API 不得嵌入」。若审阅者认为「测试 fake 已说明嵌入该怎么用」，应改判部分覆盖 |
| gbp-049 / gbp-052 / gbp-053 | 部分覆盖×2 + 新增×1 | 三者的共同问题是「反例 = 未采用某工具」。我把 049/052 判部分覆盖（静态检查主题已有 go vet 条款），把 053 判新增（复杂度/长度上限是全新一类判据）。这个切分是主观的；若统一处理，三条要么都算「部分覆盖（缺工具覆盖面）」、要么都算「新增（新工具链）」。**且三条都要否并入是纯工具链决策，须人裁决** |
| ecc-060 | 措辞可改善 | 上游的封闭词表不含 taihu 在用的 `style`(2) / `revert`(3)，照抄会禁掉既有做法。我判「形式更好、内容要本地化」。若审阅者认为「上游词表与 taihu 实际不符 → 归部分覆盖更合适」，本条应改判部分覆盖 |

> 另记：**措辞可改善只有 1 条（J=1）**，这在本轮不算异常。上游 72 条里绝大多数要么是与 taihu 逐字同规则（23 条已覆盖），要么是 taihu 缺内容而非缺措辞（23 条部分覆盖 + 25 条新增）。唯一的例外是 ecc-060：同一条规则，但 taihu 用的是「实测统计」写法，会过期（且已经过期）。
