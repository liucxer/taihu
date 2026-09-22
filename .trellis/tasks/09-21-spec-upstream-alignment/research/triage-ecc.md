# ecc 逐条三分类

> 来源：`research/everything-claude-code-go.md`（79 条：语言层 ecc-001~019 共 19 条，通用层 ecc-020~079 共 60 条）
> 分类：**适用 40 条 / 冲突 3 条 / 不适用 36 条，合计 79**。
>
> **判定口径**（与 `../design.md` §3.2、本目录 `triage.md` 的分类规则一致）：
>
> - **适用** —— 能在 taihu 代码里指出**已核实的** `file:line`，且规则方向不与 taihu 明示的设计前提对立。锚点可以是**正例**（taihu 已这么做）或**反例**（taihu 此处没做到，规则方向仍成立）。
> - **冲突** —— 规则与 taihu **明示的**做法对立（代码注释 / 既有 spec / Makefile 门禁 / 事实上的工作流），采纳即需改动既有设计或写成刻意偏离。这类需人裁决。
> - **不适用** —— 规则以 taihu 不具备的项目形态为前提（HTTP/REST/gRPC 服务、SQL/ORM、容器/K8s、CI、ECC 自己的 agent 与 skill 名册及检索工具链），或**在 taihu 代码里既无正例也无反例**（`design.md` §3.2 推论）。
>
> 与既有清单的关系：`ecc-020` 与 `ecc-015` 已在 `conflicts.md` 立卷（C-02 / C-03），本文不重复论证，只标归属并指向。

---

## 一、适用（40 条）

| 编号 | 规则一句话 | taihu 锚点（file:line，已核实） | 正例/反例 |
|------|-----------|-------------------------------|----------|
| ecc-001 | `gofmt` / `goimports` 强制，不讨论风格 | `Makefile:26` `@bad=$$(gofmt -l . \| grep -v '^third_party/' \|\| true);` | **正例**：`make check-fmt` 是 `make check` 的一环（`Makefile:21`）。注意 taihu 只跑 `gofmt`，**没有** `goimports`；`third_party/` 按上游格式豁免（`Makefile:24`） |
| ecc-002 | Accept interfaces, return structs | 返回具体类型 `internal/storage/storage.go:31` `func NewStorage(...) (*Storage, error)`；接受接口 `internal/cluster/register.go:11` `func Register(ctx context.Context, kv KV, info *InstanceInfo) error` | **正例**：两侧都有。反例向：`internal/aio/aio.go:136` `NewWithOptions(...) (Ring, error)` 返回接口（因为三后端共用），`internal/transport/server.go:50` `NewServer(storage *storage.Storage)` 收具体类型 |
| ecc-003 | 接口保持小（1-3 方法） | 正例 `internal/rpcclient/putwriter.go:14-17` `putStream`（2 方法）；反例 `internal/cluster/kv.go:11-21` `KV`（8 方法）、`internal/aio/aio.go:68-88` `Ring`（6 方法）、`internal/metastore/store.go:20` 的 `Store`（13 个导出方法） | **反例主导**：taihu 的宽接口是刻意的「能力集」抽象（`internal/cluster/kv_tikv.go:28-30` 注释：能力集与 client-go 具体类型逐一对齐） |
| ecc-004 | 错误一律 `fmt.Errorf("...: %w", err)` 包裹上下文 | `internal/cluster/kv_tikv.go:106` `return nil, fmt.Errorf("tikv txnkv connect: %w", err)` | **正例**：非测试代码 45 处 `%w`，措辞规范见 `.trellis/spec/architecture/code-style.md` 规则 8。附带反例：`internal/storage/compact.go:152` 与 `:176` 用 `err == ierr.ErrConflict` / `err == ierr.ErrNotFound` 而非 `errors.Is`（全仓 `errors.Is/As` 仅 7 处） |
| ecc-006 | PostToolUse hook：改 `.go` 后自动 `gofmt`/`goimports` | `Makefile:26-28`（`check-fmt`） | **正例（等价机制）**：taihu 靠 `make check` 而非 hook 兜底；`.claude/settings.json` 里没有 PostToolUse 段（只有 `:16` SessionStart、`:38` PreToolUse、`:56` UserPromptSubmit） |
| ecc-007 | PostToolUse hook：改 `.go` 后跑 `go vet` | `Makefile:22` `go vet ./...`（`check` 目标的最后一行） | **正例（等价机制）**：同上，靠 `make check` 而非 hook；另有 `Makefile:65-70` 的 `make check-linux` 做跨平台 vet |
| ecc-009 | Functional Options 模式构造对象 | `internal/storage/options.go:6-8` `type Option func(*options)`，调用点 `internal/storage/storage.go:31` `opts ...Option` | **正例**：`internal/device/options.go:6-8` 同形；注释写明动机「新增配置项时在此扩展，既有调用点无需改动签名」 |
| ecc-010 | 接口定义在使用方，而非实现方 | `internal/benchkit/run.go:38-44` `type Store interface` —— benchkit 是**消费方**，两个实现（`taihuclient.Storage`、`rpcclient.Storage`）在别的包 | **正例**（教科书形态）。反例：`internal/rpcclient/objectstore.go:16` 刻意把接口写在**实现侧**，注释给了理由「taihuclient 依赖本包，反向声明会成环」 |
| ecc-011 | 依赖注入用构造函数，不用全局 | `internal/storage/storage.go:31`（ctx/dir/dev/layout/opts 全走参数）、`internal/transport/server.go:50` `NewServer(storage *storage.Storage)` | **正例**为主。反例：`cmd/taihu/cmd/helpers.go:32` `var kvConnect = connectKVReal` 是包级可变变量（测试缝隙，注释已说明生产路径恒为 `connectKVReal`） |
| ecc-015 | 一律 `context.Context` + `WithTimeout`，`defer cancel()` | 正例 `cmd/taihu/cmd/root.go:112` `return context.WithTimeout(parent, global.timeout)`（`--timeout` 可配见 `:91`）；反例 `internal/transport/server.go:83` `ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)` | **正例**为主，**反例**一处（已在 `conflicts.md` C-03 立卷，此处不重复裁决） |
| ecc-016 | 标准 `go test` + 表驱动测试 | `internal/transport/protocol/protocol_test.go:81-103`（`cases := []struct{...}` + `t.Run(tc.name, ...)`） | **正例**：`.trellis/spec/testing/unit-tests.md:77` 规则 4 把它定为默认形态；全仓 `t.Run(` 65 处、`_test.go` 66 个 |
| ecc-017 | 测试必须带 `-race` | 正例 `internal/device/device_test.go:149` 注释「校验完成泵分发正确性（-race）」；`internal/storage/compact_test.go:94`、`internal/metastore/segments_test.go:354` 同形 | **正例**为主。边界反例：`test/e2e/e_concurrency_test.go:5`「E 组：并发/竞态（**不使用** `-race`：aarch64 上 `go test -race` 链接失败）」——有理由的例外；`Makefile` 里也没有 `-race` 目标 |
| ecc-021 | KISS：清晰优先于机巧，避免过早优化 | `internal/transport/client.go:117-121` 「曾用过 netpoll 的零拷贝移交 TakeTry……实测该路径存在静默数据错配……**故 TCP 路径已停用**」 | **正例**：为了正确性砍掉更快的零拷贝路径，是「清晰/正确优先」的实证 |
| ecc-022 | DRY：重复逻辑抽公共函数 | `internal/bufpool/bufpool.go:1-3`「供各层热路径复用……server/rpcclient 的 IO 缓冲与 device 的 O_DIRECT 对齐缓冲**共用本池**」；`cmd/taihu/cmd/helpers.go:16`「共享 helper」 | **正例**：跨包复用有实测依据（`internal/bufpool/bufpool.go:12-14` 记了 36% CPU 的冷分配） |
| ecc-023 | YAGNI：不预先构建用不到的抽象与特性 | `internal/transport/batch.go:15`「（与 shmBatchReader.run 一致，**不做**主动关闭；Server 停机路径不依赖队列排空）」；`internal/cluster/kv.go:8`「KV 是集群注册与索引所需的**最小**存储接口」 | **正例**（锚点偏弱，见 §附「拿不准」） |
| ecc-024 | 多小文件优于少大文件；200-400 行典型，800 行软上限 | 反例 `internal/device/device.go:813`（`wc -l` = **813 行**，唯一超过 800 的非测试文件） | **反例**：81 个非测试 `.go` 共 13245 行（均值约 163 行），绝大多数落在 200-400 区间内，只有 `device.go` 越线 |
| ecc-025 | 每一层显式处理错误，绝不静默吞掉 | 正例 `internal/ierr/ierr.go:1`「定义 taihu 存储的公共错误 —— **唯一事实源**」；反例 `internal/transport/server.go:212` `_ = c.writeFrame(...)`（全仓非测试代码 139 处 `_ =`，`server.go` 的响应写几乎全被丢弃，**无一处注释说明为何忽略**） | **反例**（局部）：丢弃的是回帧失败（对端已断），但没有任何注释把「为什么可以丢」写下来 |
| ecc-026 | 只在系统边界做输入校验，schema 化校验，快速失败 | `internal/transport/protocol/protocol.go:252` `if kl > MaxKeyLen {`（`:48` 定义 `MaxKeyLen = 1 << 16`，注释「防止畸形长度字段放大内存」）；`internal/benchkit/run.go:46` `func (c Config) Validate() error` | **正例**：协议解码边界 + API 入参边界（`internal/storage/storage.go:245`）两层都有；`:272`、`:296`、`:491` 是同类校验 |
| ecc-027 | 命名自解释；缩写全大写；常量/类型视觉可分 | `pkg/taihu-client/config.go:71` `ClientID string`（不是 `ClientId`） | **正例**：`.trellis/spec/architecture/code-style.md` 规则 6 把这条写成硬约定并给了实测判据。ECC 自己也把这条标为「可被语言层覆盖」的唯一一条，taihu 的形态与覆盖结果一致 |
| ecc-028 | 少深嵌套，尽早 return | `internal/storage/storage.go:245-247`：`if off < 0 \|\| off > meta.Size {` → `return nil, ierr.ErrInvalidRange`；`:256-259`、`:306` 同形（guard clause + 提前返回） | **正例**：`storage.go` 的读路径整体是这个形态 |
| ecc-029 | 魔法数字改具名常量 | 正例 `internal/layout/layout.go:14` `const BlockSize int64 = 4096`、`internal/bufpool/bufpool.go:23-31`（`logBlockSize`/`maxBufBucket`/`maxKeep` 带理由）；反例 `internal/metastore/meta.go:31-33` 编码侧写死 `b[1:]` / `b[9:]` / `b[17:]`，解码侧 `:43-45` 再写一遍 | **两边都有**：布局常量做得好，**磁盘格式**是反例（本目录 `triage.md` A-01 已立论） |
| ecc-030 | 长函数拆成职责单一的小块 | 反例 `internal/device/device.go:439` `func (d *Device) AppendBatch(...)`（结束于 `:564`，**126 行**）；`internal/transport/client.go:121` `Get`（112 行） | **反例**：前 15 长的函数里 9 个 >70 行（`internal/transport/server_shm_linux.go:403`、`internal/device/device.go:651` 等） |
| ecc-031 | 收尾质量检查单（函数 <50 行、文件 <800 行、嵌套 ≤4、有错误处理、无硬编码、无 mutation） | `internal/device/device.go:439`（126 行函数）与 `:813`（813 行文件）；`internal/storage/storage.go:277` `n := copy(data, data[skip:skip+want])`（池化缓冲**原地左移**） | **反例**（三项）：行数两项见上；「无 mutation」与 taihu 的零拷贝前提冲突（同 `ecc-020`，冲突清单 C-02） |
| ecc-033 | 三类测试全部必需：单元 / 集成 / E2E | `.trellis/spec/testing/index.md:3`「本仓库的测试分**两级**：与实现同目录的单元测试（`go test ./...` 默认跑），以及需要真机（裸盘 + TiKV）的 `test/e2e/` 端到端套件」；`test/e2e/harness_test.go:1-6`（真 server + 真裸盘 + 真 TiKV） | **正例**（形态有出入）：taihu 是两级不是三级 —— 集成语义被 `test/e2e/` 吸收（它同时起真实 server 与真实 TiKV）。**不宜按字面要求再开一层 integration 目录** |
| ecc-036 | 测试结构优先 Arrange-Act-Assert | `internal/storage/storage_test.go:50-60` `TestStoragePutGetRoundTrip`：`newTestStorage(t)`（Arrange）→ `s.Put(...)`（Act）→ `s.ReadAt(...)` + 手写断言（Assert） | **正例**（隐含而非明文规定，见 §附「拿不准」） |
| ecc-037 | 测试命名描述行为而非实现 | `test/e2e/e_concurrency_test.go:67` `func TestE1ConcurrentSameKeyNoTear(...)`；`.trellis/spec/testing/e2e-tests.md:45` 收录的 `TestE1MultiChunkNoTear` / `TestD4SegmentWatermarkReclaim` | **正例**：命名讲断言的不变式（NoTear）。部分表驱动用例名描述的是**输入**（`internal/transport/protocol/protocol_test.go:87` 「空 key 零长度」）而非行为 |
| ecc-038 | 提交前硬性检查：无硬编码密钥 | `configs/taihu-server.example.sh:12-16`（`LISTEN=""` / `DB=""` / `DEV=""` / `NAME=""` / `PD=""`，全部留空由环境填） | **正例（弱）**：`grep -rniE 'secret\|password\|apikey\|token *[:=]' internal/ pkg/ cmd/`（非测试）**零命中**；仓库里没有凭据可泄 |
| ecc-039 | 所有用户输入必须校验 | `internal/transport/protocol/protocol.go:252`（key 长度上限）、`internal/storage/storage.go:245`（`off` 越界→`ierr.ErrInvalidRange`）、`internal/transport/protocol/protocol.go:296` | **正例**：数据面入口（协议解码）与存储 API 入口两层都校验，且失败即返回 `ierr` sentinel |
| ecc-045 | 错误信息不得泄露敏感数据 | `internal/transport/protocol/protocol.go:113` `func MapStorageErr(err error) ErrCode` 与 `:129` `func MapCode(c ErrCode) error` —— 跨进程只传 **4 字节错误码**（`:151` `EncCode`），错误字符串留在服务端 | **正例**：比「不泄露」更强 —— 结构上不传字符串。详见 `.trellis/spec/architecture/error-model.md`「跨进程传 code 不传字符串」 |
| ecc-048 | 强制评审触发点（写完代码后 / 合并前 / 安全敏感改动 / 架构改动） | `.claude/agents/trellis-check.md:4` 「Code quality check expert. Reviews code changes against specs and self-fixes issues.」 | **正例**：评审是 Trellis 工作流里的独立环节（implement → check），不是可选动作 |
| ecc-050 | 评审检查单（可读、<50 行、文件内聚、≤4 层嵌套、错误显式、无密钥、无调试语句、有新测试、覆盖率 ≥80%） | `.trellis/spec/architecture/index.md:35` `## Quality Check`（实测执行的是 `make check` + `make check-linux` + `go test ./...`，**不含**行数 / 嵌套 / 覆盖率判据） | **反例（缺口）**：检查单存在但没有「函数/文件行数」项，故 `internal/device/device.go:439` 的 126 行函数能长期存在 |
| ecc-055 | 重点排查项（质量）：>50 行函数、>800 行文件、>4 层嵌套、缺错误处理、mutation、缺测试 | `internal/device/device.go:439`（126 行）、`internal/device/device.go:813`（813 行）、`internal/storage/storage.go:277`（原地 `copy`） | **反例**：三项都在仓库里实际存在；「mutation」一项按字面会误伤零拷贝设计 |
| ecc-057 | Repository 模式：数据访问藏在统一接口后 | `internal/metastore/store.go:20` `type Store interface`（GetMapping/PutMapping/DeleteMapping/IterMapping/Batch*…）；实现 `internal/metastore/kv_pebble.go:33` `type pebbleStore struct` | **正例（结构同形）**：`internal/storage` 依赖 `metastore.Store` 抽象，实现是 pebble。**但不宜按字面引入 `findAll/findById` 命名** —— taihu 没有 domain 层，这条只应落成「存储抽象与实现分离」的表述 |
| ecc-060 | 提交信息 `<type>: <description>`，type ∈ feat/fix/refactor/docs/test/chore/perf/ci | `.trellis/spec/architecture/commits.md:7` `## 1. 标题格式：\`type(scope): <中文标题>\`` | **正例**：`git log` 145 条的 type 分布（docs 42 / feat 28 / perf 22 / refactor 17 / fix 16 / chore 11）与 ECC 清单基本重合；差异：taihu 多带 scope 且标题为中文，另用过 `style`(2) / `revert`(3)，**从未用过 `ci`**（本仓库无 CI） |
| ecc-062 | 不自动添加 `Co-Authored-By` 尾注 | `.trellis/spec/architecture/commits.md:3`（正文约定里没有任何 trailer 位置）；实证 `git log --format=%B \| grep -ci 'co-authored-by'` = **0** | **正例**：`.trellis/config.yaml:35-38` 同时关掉了脚本自动提交（`session_auto_commit: false`），提交信息是人写的 |
| ecc-064 | 先规划：产出 PRD / architecture / system_design / tech_doc / task_list | `.trellis/tasks/09-21-spec-upstream-alignment/prd.md:1`（需求）+ 同目录 `design.md:1`（设计）+ 同目录 `implement.md`（任务清单）—— 每个 Trellis 任务目录固定这三件 | **正例**：机制不是 ECC 的 planner agent，而是 Trellis 任务工件；产物形态一一对应 |
| ecc-066 | 写完代码立刻评审，先处理 CRITICAL/HIGH 再 MEDIUM | `.claude/agents/trellis-check.md:4`（评审 + **自修**）；`.trellis/spec/architecture/index.md:35` 的 Quality Check 是它的执行依据 | **正例**：与 `ecc-048` 同一锚点，区别在「立刻」这一时序 —— Trellis 的 check 阶段紧跟 implement |
| ecc-072 | Hook 分三类：PreToolUse / PostToolUse / Stop | `.claude/settings.json:38` `"PreToolUse": [`（matcher `Task`/`Agent` 注入子 agent 上下文） | **正例（只用了一类）**：仓库实际注册了 SessionStart（`:16`）、PreToolUse（`:38`）、UserPromptSubmit（`:56`）三类事件，**没有** PostToolUse / Stop |
| ecc-073 | 自动放行权限慎用；绝不用 `--dangerously-skip-permissions`，改用 `allowedTools` | `.claude/settings.local.json:3` `"allow": [`（3 条精确到具体命令的白名单，如 `"Read(//tmp/**)"`） | **正例**：白名单是逐条精确匹配，没有通配放行 |
| ecc-074 | 多步任务用 TodoWrite 跟踪进度，暴露漏项/多余项/粒度错误 | `.trellis/workflow.md:40` `### Task System`（每任务一个目录，含 `task.json` / `prd.md` / `design.md` / `implement.md` / `implement.jsonl` / `check.jsonl`） | **正例**：比 TodoWrite 更重的外部化跟踪；`.trellis/spec/architecture/index.md:19` 另有 `## Pre-Development Checklist`（开发前逐条自检） |

---

## 二、冲突（3 条）

| 编号 | 规则一句话 | taihu 现状（file:line，已核实） | 为什么冲突 |
|------|-----------|-------------------------------|-----------|
| ecc-020 | **CRITICAL** 不可变性：永远创建新对象，绝不原地修改既有对象 | `internal/storage/storage.go:277` `n := copy(data, data[skip:skip+want])`（池化缓冲**原地左移**，注释在 `:276`）；`internal/bufpool/bufpool.go:102-120` `put` 把切片归还后由下一次 `get`（`:88-100`）原样复用；`internal/transport/frame.go:18-24` 注释写明「客户端 Get 可直接移交该缓冲给调用方（**零拷贝**），并经由 `bufpool.Put` 安全归还」 | 缓冲复用与就地改写是 taihu 的**性能前提**（`internal/bufpool/bufpool.go:12-14` 记了否决 `sync.Pool` 的实测依据：读路径冷分配占服务端 CPU 36%）。taihu 用「移交所有权 + 显式归还」替代不可变性。**已在 `conflicts.md` C-02 立卷，待裁决** |
| ecc-034 | TDD 强制流程：先写测试（RED）→ 跑到失败 → 最小实现（GREEN）→ 重构 | `fed67b4 test: 补齐 82 个单测文件与 e2e 套件，third_party fork 回迁上游测试` —— 测试作为**独立一次提交**事后「补齐」；`9bcc797 fix(device): 完成侧瞬时错误重试、设备 O_EXCL 独占打开…` 改了 `internal/device/device.go` 等 6 个文件（+143/-15），**0 个 `_test.go`** | taihu 的实际做法是「实现先行、测试在同一次改动或之后补齐」；`grep -rn 'TDD' .trellis/ doc/` 除本任务的上游研究文件外**零命中**，即仓库从未声明过 TDD 次序。采纳需改工作流，或写成刻意偏离 |
| ecc-061 | PR 流程：分析完整提交历史、`git diff <base>...HEAD`、写完整摘要、新分支 `push -u` | 无 `file:line` 锚点，实证：`git branch -a` 只有 `main`（无任何 feature/PR 分支）；`git rev-list --count HEAD` = 145，其中 merge 提交仅 1 条（`94edd49 Merge pull request #1 from liucxer/feature/read-consistency-and-test-suite`） | taihu 是个人 GitHub 仓库，实际工作流是**直接提交并推送 `main`**（`origin` = `git@github.com:liucxer/taihu.git`）。规则的「开分支 → 提 PR → 评审」在无第二评审人的单仓里没有落点。注意规则的另一半「写完整摘要」taihu **做到了**（`commits.md` 要求正文讲「为什么」+ 纯重构带「行为不变：」核对段） |

---

## 三、不适用（36 条）

| 编号 | 规则一句话 | 为什么不适用 |
|------|-----------|--------------|
| ecc-005 | 详细 Go 惯用法以 skill `golang-patterns` 为准（下沉引用） | 指向 ECC 自带的 skill 制品，taihu 未安装该 skill 包；taihu 的等价物是 `.trellis/spec/engine/`、`architecture/code-style.md` 等分层 spec |
| ecc-008 | PostToolUse hook：对改动的包跑 `staticcheck` | taihu 未使用 `staticcheck`（无配置文件、无 CI、`Makefile:21-22` 的 `check` 只跑 gofmt / 两条分层 grep / `go vet`），且本轮明确「不引入新工具」（`.trellis/tasks/09-21-spec-upstream-alignment/prd.md:71`）。代码里无正例也无反例 |
| ecc-012 | 详细 Go 模式（并发、错误处理、包组织）以 skill `golang-patterns` 为准 | 同 ecc-005 |
| ecc-013 | 密钥从环境变量读（`os.Getenv`），缺失即启动失败（`log.Fatal`） | taihu 无凭据面：非测试代码 `os.Getenv` 仅 1 处（`internal/aio/aio.go:138`，读 AIO 后端模式），`log.Fatal` **零命中**。TiKV TLS 走 CLI flag 传证书路径（`internal/cluster/kv_tikv.go:14-19`），不是密钥 |
| ecc-014 | 用 `gosec ./...` 做静态安全扫描 | 工具未使用，无锚点；同 ecc-008 的「不引入新工具」约束 |
| ecc-018 | 覆盖率用 `go test -cover ./...` | `Makefile:10` 的 `.PHONY` 无 `test`/`cover` 目标，仓库不按百分比管理覆盖率（`triage.md` 3.1 对 `gbp-040` 的同类裁决）。无正例也无反例 |
| ecc-019 | 详细 Go 测试模式以 skill `golang-testing` 为准 | 同 ecc-005 |
| ecc-032 | 最低测试覆盖率 80% | 无覆盖率工具链、无门禁（同 ecc-018）；taihu 的验收口径是「`go test ./...` + `make check` + `make check-linux`」（`.trellis/spec/testing/index.md:35-47` 的 Quality Check，三条命令里没有任何覆盖率参数）而非百分比 |
| ecc-035 | 测试失败排查顺序：用 tdd-guide agent、检查测试隔离、核对 mock、改实现而非改测试 | 依赖 ECC 的 `tdd-guide` subagent 与 mock 文化；taihu 自有测试零 mock 框架（`testify` 在自有代码零引用），失败排查无对应流程 |
| ecc-040 | 防 SQL 注入：参数化查询 | taihu 无 SQL/ORM：`grep -rn 'database/sql'` 全仓零命中；元数据走 pebble API（`internal/metastore/kv_pebble.go`），集群注册走 TiKV TxnKV（`internal/cluster/kv_tikv.go`），都是 key-value API，不存在语句拼接 |
| ecc-041 | 防 XSS：HTML 转义 | 无 HTML/模板渲染，无浏览器侧产物 |
| ecc-042 | 开启 CSRF 防护 | 无 cookie / 表单 / 会话，无浏览器客户端 |
| ecc-043 | 校验认证/授权 | 数据面与注册区都没有认证授权层（`grep -rni 'auth\|token\|permission' internal/transport/ pkg/taihu-client/` 非测试零命中）；跨进程信任建立在网络可达与 TiKV 注册之上 |
| ecc-044 | **所有端点**加限流 | 无 HTTP 端点；自研帧协议（`internal/transport/protocol`）没有 per-client 限流，也不需要 —— 流控由 `Ring.ErrFull`（`internal/aio/aio.go:38`，注释在 `:37`「提交队列已满（io_submit 返回 EAGAIN），应先 Wait 取回完成事件后重试」）与在途队列这类背压机制承担 |
| ecc-046 | 密钥绝不硬编码，只用环境变量/密钥管理服务；启动时校验存在；泄露即轮换 | 同 ecc-013：taihu 无密钥管理面 |
| ecc-047 | 安全事件响应协议：STOP → security-reviewer agent → 先修 CRITICAL → 轮换密钥 → 全库排查 | 依赖 ECC 的 `security-reviewer` agent 与「已泄露密钥」这一前提；taihu 两者都没有 |
| ecc-049 | 请求评审前置条件：CI 全绿、冲突已解、分支与目标分支同步 | 无 CI（`.github/` 不存在，无 `.gitlab-ci.yml`）；且本机 `go test ./...` 已知**不是全绿**（`.trellis/spec/platform/index.md:42-45`、`:103` 记了 7 个失败全在 Linux 专有路径上），「CI 全绿」这个门槛在 taihu 无对应物 |
| ecc-051 | 安全评审触发点：认证授权、用户输入、数据库查询、文件系统操作、外部 API、密码学、支付 | 该清单以 Web/应用安全为前提，taihu 无其中任何一面（见 ecc-040~046） |
| ecc-052 | 严重度分级：CRITICAL 阻断 / HIGH 警告 / MEDIUM 提示 / LOW 可选 | taihu 没有代码缺陷严重度分级体系。注意不要与本目录 `conflicts.md:7`（`research/conflicts.md`）的「A 类影响真实行为 / B 类影响 spec 措辞 / C 类影响工具链」混淆 —— 那是**冲突条目的影响面**分级，不是代码评审的缺陷严重度 |
| ecc-053 | 通过标准：无 CRITICAL 且无 HIGH 才 approve | 依赖 ecc-052 的分级体系，taihu 无 |
| ecc-054 | 重点排查项（安全）：硬编码凭据、SQL 注入、XSS、路径穿越、CSRF、认证绕过 | 同一组 Web 安全项；taihu 无路径穿越面（用户不提供文件路径：key 是 KV 的 key，设备路径来自 CLI flag） |
| ecc-056 | 重点排查项（性能）：N+1 查询、缺分页、无界查询、缺缓存 | 无数据库查询、无 HTTP 分页。taihu 的性能关注点是另一套：零拷贝生命周期、缓冲池命中、`io_submit` 批量粒度、段级 compaction（`internal/storage/compact.go`），与这四项无交集 |
| ecc-058 | API 响应统一信封：`success` + `data` + `error` + 分页 metadata | 无 REST/JSON API 对外。线上格式是自研帧：`internal/transport/protocol/protocol.go:51` `const FrameHeaderLen = 5`（streamID 4B + op 1B），没有 `success`/分页字段；CLI 的 `-json` 输出是对象原样序列化（`cmd/taihu/cmd/helpers.go:118` `printJSON`），不套信封 |
| ecc-059 | 新功能优先找久经考验的 skeleton 项目克隆后在其结构内迭代 | 该规则以「有可克隆的同形态上游项目」为前提；taihu 是存储引擎，与 ECC 假想的 Web 服务模板形态不同。taihu 的对应物是向自己既有分层与 spec 收敛（`internal/` 各包按资源与机制分层） |
| ecc-063 | 实现前强制 Research & Reuse：`gh search repos` / `gh search code` → Context7/官方文档 → Exa | 规则以 ECC 特有检索工具链为前提；taihu 无此流程（最接近的心智是 `.trellis/spec/guides/code-reuse-thinking-guide.md`，但它是复用心智而非检索步骤，且该文件内容另有问题见 `triage.md` F-05/C-07） |
| ecc-065 | 走 TDD：用 `tdd-guide` agent，RED→GREEN→IMPROVE，验证 80%+ 覆盖 | 依赖 ECC 的 `tdd-guide` subagent 与 80% 覆盖率门槛（后者见 ecc-032）。TDD 次序本身已在 ecc-034 记为冲突 |
| ecc-067 | 提交前复查：CI 全绿、无冲突、分支同步，通过后才请求评审 | 同 ecc-049（无 CI、无分支模型） |
| ecc-068 | 模型选择：Haiku 做轻量高频、Sonnet 做主开发、Opus 做架构决策 | ECC 对其 harness 的模型编排建议，taihu 仓库无对应约定，代码里无锚点 |
| ecc-069 | 上下文管理：不在最后 20% 上下文里做大重构/跨文件实现/复杂调试 | ECC 的上下文预算管理，taihu 无对应约定。（最接近的是 `.trellis/workflow.md:9`「Persist everything — …conversations get compacted, files don't」，但那讲的是**外部化持久化**，不是剩余的上下文预算） |
| ecc-070 | 复杂任务开 Extended Thinking + Plan Mode，多轮批判，按角色拆子 agent | harness 级操作建议，非仓库规则，代码里无锚点 |
| ecc-071 | 构建失败处理：用 `build-error-resolver` agent，逐条分析、增量修复、每步验证 | 依赖 ECC 的 `build-error-resolver` agent；taihu 的构建失败处理就是 `make check` / `make check-linux`（`Makefile:21-22`、`:65-70`） |
| ecc-075 | Agent 名册以 plugin-scoped `subagent_type` 调用（`ecc:planner`、`ecc:code-reviewer` 等 68 个） | 指 ECC 插件的 agent 名册；taihu 用的是 Trellis 的 `.claude/agents/{trellis-implement,trellis-check,trellis-research}.md`，没有 `ecc:*` 命名空间 |
| ecc-076 | 无需用户提示即应调用 agent 的四种情形（复杂特性→planner、刚写完码→code-reviewer、修 bug→tdd-guide、架构决策→architect） | 依赖 ecc-075 的名册（`planner`/`tdd-guide`/`architect` 均不存在）。Trellis 的 implement/check 派发逻辑与这四类映射不重合 |
| ecc-077 | 独立操作一律并行发起多个 agent | ECC 的多 agent 编排约定；taihu 的 Trellis 派发是单链路（implement → check → research），仓库内无并行派发的证据 |
| ecc-078 | 委托完成契约：最终消息即交付物，禁止以「等待后台 agent」收尾 | ECC agent-team 编排约定，属 harness 行为规范，taihu 仓库里无对应物 |
| ecc-079 | 复杂问题用分角色子 agent 做多视角分析（事实核查/资深工程/安全/一致性/冗余） | 同 ecc-078：ECC 的多角色子 agent 编排，taihu 无 |

---

## 附：自检

### 1. 三类条数合计

| 类别 | 条数 |
|------|------|
| 适用 | **40**（语言层 12：001-004/006/007/009-011/015-017；通用层 28） |
| 冲突 | **3**（020、034、061） |
| 不适用 | **36** |
| **合计** | **79** ✅ |

编号连续性核对（逐条数过，无跳号无重号）：
- 适用：001,002,003,004,006,007,009,010,011,015,016,017,021,022,023,024,025,026,027,028,029,030,031,033,036,037,038,039,045,048,050,055,057,060,062,064,066,072,073,074 = 40
- 冲突：020,034,061 = 3
- 不适用：005,008,012,013,014,018,019,032,035,040,041,042,043,044,046,047,049,051,052,053,054,056,058,059,063,065,067,068,069,070,071,075,076,077,078,079 = 36
- 40 + 3 + 36 = 79

### 2. 我实际跑过的验证命令（关键几条）

```bash
# 语言层锚点
grep -n 'gofmt' Makefile                      # :24,:26,:27 → ecc-001/006
grep -n '' Makefile | sed -n '18,35p'         # :21 check、:22 go vet → ecc-007
sed -n '6,20p' internal/storage/options.go    # :6 type Option func(*options) → ecc-009
sed -n '31p' internal/storage/storage.go      # NewStorage(...) (*Storage, error) → ecc-002/011
sed -n '11p' internal/cluster/register.go     # Register(ctx, kv KV, ...) → ecc-002
sed -n '11p' internal/cluster/kv.go           # type KV interface（8 方法）→ ecc-003
sed -n '14p' internal/layout/layout.go        # const BlockSize int64 = 4096 → ecc-029
sed -n '31,33p' internal/metastore/meta.go    # b[1:] / b[9:] / b[17:] → ecc-029 反例
sed -n '152p;176p' internal/storage/compact.go # err == ierr.ErrConflict / ErrNotFound → ecc-004
sed -n '277p' internal/storage/storage.go     # copy(data, data[skip:skip+want]) → ecc-020/031/055
sed -n '245,247p' internal/storage/storage.go # guard clause + early return → ecc-028
sed -n '439p;564p' internal/device/device.go  # AppendBatch 起止（126 行）→ ecc-030/050/055
wc -l internal/device/device.go               # 813 → ecc-024/031
find internal pkg cmd examples -name '*.go' ! -name '*_test.go' | wc -l   # 81 → ecc-024
grep -n 'MaxKeyLen\|kl > MaxKeyLen' internal/transport/protocol/protocol.go # :48,:252 → ecc-026/039
grep -n 'func MapStorageErr\|func MapCode\|FrameHeaderLen' internal/transport/protocol/protocol.go # :113,:129,:51 → ecc-045/058
grep -rn 'func Test' test/e2e/*.go | grep NoTear          # → ecc-037
grep -rn 'staticcheck\|gosec\|golangci' --include='*.go' . | grep -v third_party  # 零命中 → ecc-008/014
grep -rn 'os.Getenv' --include='*.go' internal/ pkg/ cmd/ | grep -v _test.go      # 仅 aio.go:138 → ecc-013
grep -rn 'log.Fatal' --include='*.go' internal/ pkg/ cmd/                          # 零命中 → ecc-013
grep -rn 'database/sql' --include='*.go' . | grep -v third_party                   # 零命中 → ecc-040
grep -rni 'auth\|token\|permission' internal/transport/ pkg/taihu-client/ | grep -v _test  # 零命中 → ecc-043
grep -rn '_ = ' --include='*.go' internal/ pkg/ cmd/ | grep -v _test | wc -l       # 139 → ecc-025
grep -rn 'errors.Is\|errors.As' --include='*.go' internal/ pkg/ cmd/ | grep -v _test | wc -l  # 7 → ecc-004
# 工作流 / 工具链
git branch -a ; git log --merges ; git rev-list --count HEAD          # → ecc-061
git log --format=%s | sed 's/(.*//;s/:.*//' | sort | uniq -c | sort -rn  # → ecc-060
git log --format=%B | grep -ci 'co-authored-by'                        # 0 → ecc-062
git show --stat 9bcc797 | tail -20                                     # 6 文件 0 个 _test.go → ecc-034
git show --stat fed67b4 | head -5                                      # 「补齐 82 个单测文件」→ ecc-034
grep -n 'PreToolUse\|PostToolUse\|Stop' .claude/settings.json           # 仅 :38 PreToolUse → ecc-072
grep -n 'allow' .claude/settings.local.json                             # :3 → ecc-073
grep -n 'Plan before code\|Task System' .trellis/workflow.md            # :7,:40 → ecc-064/074
```

### 3. 我拿不准的条目

| 编号 | 归属 | 拿不准什么 |
|------|------|-----------|
| ecc-023 | 适用 | 锚点（`internal/transport/batch.go:15`「不做主动关闭」、`internal/cluster/kv.go:8`「最小接口」）证明的是「没有多做事」，而 YAGNI 说的是「不预建抽象」。二者不完全等价。**若评审认为算硬凑，请改成不适用** |
| ecc-036 | 适用 | taihu 的 spec **没有**明文规定 Arrange-Act-Assert，我是从 `internal/storage/storage_test.go:50-60` 的代码形态归纳的（Arrange→Act→Assert 在 66 个测试文件里普遍成立）。属「事实符合但未成文」 |
| ecc-024 / ecc-031 / ecc-050 / ecc-055 | 适用 | 四条都以同一组反例（`device.go` 813 行、`AppendBatch` 126 行）为锚点，有重复计数的成分。若评审倾向去重，可只保留 `ecc-024`（文件）与 `ecc-030`（函数），把 `ecc-031/050/055` 并入 |
| ecc-033 | 适用 | taihu 是**两级**测试（单测 + e2e）而非规则要求的三级（单元/集成/E2E）。我判「集成语义被 e2e 吸收」，但这与「三类全部必需」的字面有张力；若评审从严，可改判不适用 |
| ecc-057 | 适用 | 我把 `metastore.Store` + `pebbleStore` 判为 Repository 模式的**结构同形**（无 domain 层）。若评审认为「无领域层即不成立」，请改判不适用 |
| ecc-061 | 冲突 | 「直推 main」这一做法我只能从 `git branch -a`（只有 main）与 145 条提交仅 1 条 merge 推断，仓库内没有一处**明文**写「本仓库不开 PR」。若认为证据不足以称「明示的做法对立」，可降为不适用（`ecc-063` 那一类） |
| ecc-038 | 适用 | 锚点 `configs/taihu-server.example.sh` 是**部署参数**模板（空值），不是密钥。它证明的是「仓库里没有凭据」，而不是「密钥走了专门通道」 |

### 4. 顺带发现（与分类无关，供交叉核对）

- `conflicts.md` 的 **C-05 把「统一走 logger、不要各处直接写 stderr」记到了 `ecc-006` 名下**，但 `ecc-006` 实际是「PostToolUse hook 跑 gofmt」，79 条里**没有任何一条**是日志规则（ECC 的 `rules/common/hooks.md` 讲的是 hook 分类，`ecc-072`）。C-05 自己也写了「这条不是冲突的来源」—— 结论（日志第四通道）不受影响，但**编号引用是错的**，建议改标为「无对应上游编号，taihu 自身 spec 缺陷」。
- 本任务 `triage.md` 的 §一 A-02 与 `conflicts.md` C-03 都已经处理过 `ecc-015`。本文按既有口径把它归**适用**（正例为主 + 一处反例），不重复裁决。
- ECC 的 `ecc-020`（不可变，CRITICAL）与 taihu 的零拷贝前提正面相撞这一点，本轮由 `conflicts.md` C-02 承接；补充一条 C-02 没写的证据：`internal/transport/client.go:117-121` 记录了**同源的一次真实事故**（netpoll 零拷贝移交因「移交后整块缓冲被归还池并复用，内容被后续收流覆盖」而静默数据错配，TCP 路径已停用）。这说明 taihu 对缓冲复用的风险是**知情且有纪律**的（用「一帧独占一节点 + 显式归还」兜底），而不是无意识地可变 —— 这条可以作为 C-02 裁决「维持 taihu」时的理由素材。
