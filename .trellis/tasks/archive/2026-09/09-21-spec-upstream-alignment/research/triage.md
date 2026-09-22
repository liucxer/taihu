# 三分类对撞表

> **状态：四份来源已全部收回**（`uber-` 134 条、`ecc-` 79 条、`gbp-` 53 条、`th-` 297 条）= 上游 266 条 + 现有 spec 297 条。本文件记录**已对真实代码核实过**的条目；全量 266 条的三分类台账见 §六（进行中）。
>
> **核实纪律**：凡是写进本文件的断言，我都亲自在真实代码/仓库上跑过。已有一处 research agent 的结论**不可复现**（见 F-06 附注）、一处口径有出入（见 F-08）—— **agent 的产出是线索，不是证据**。
>
> 分类规则见 `../design.md`：**适用**（有真实锚点且不矛盾）/ **冲突**（与现有 spec 或代码行为矛盾）/ **不适用**（面向应用/微服务）。

---

## 零、三份来源的可信度不等（影响采纳时的权重）

三份来源的**权威性差别很大**，对撞前必须先说清楚，否则会出现「上游说 X」但说不清是哪个上游、凭什么。

| 来源 | 权威性 | 依据 |
|------|--------|------|
| Uber Go Style Guide | **高** | 工业界广泛引用；有明确的生成源（`stitchmd` 自动从 `src/` 生成 `style.md`）；内容自洽且对自身局限有说明（多处标 `Exception`、`no strict guidelines`）。已知瑕疵：Introduction 建议用 `golint`，而同文 Linting 一节已说 `revive` 是其后继 —— 该段过时 |
| ECC（`affaan-m/ECC`） | **中** | 有实际工程内容（`golang-patterns` / `golang-testing` 两 skill 共 31 KB 是实质知识）。但**语言层是「指针层」**：`rules/golang/` 5 个文件合计仅 2890 字节，且 README 与前文自相矛盾（详见 `everything-claude-code-go.md` §6 记的 3 处上游不一致） |
| golang-base-practices-skills | **低 —— 需降权使用** | 见下方三条 |

### 为什么 golang-base-practices-skills 要降权

1. **它宣称的「依据」无法逐条验证。** 仓库自述 53 条规则援引 Effective Go / Google Go Style Guide / Uber Go Style Guide / Go Code Review Comments。但实际情况是：53 个规则文件结构统一、**没有任何 `references:` 字段**，正文也不逐条援引；四份资料只在**包级别**被整体声明过一次（`SKILL.md` 第 8 行 + `README.md` Overview）。全库唯一一处点名具体上游的是 `idiomatic-embedding.md` 里的一行 `(Uber Style)`，即 `gbp-038`。
   → **实际影响：无法按「出处」筛选本来源的规则，只能按内容自行判断。** 这也意味着它不能作为「Uber 或 Google 也这么规定」的论据 —— 那层转述关系不存在。
2. **仓库活跃度低且内容为 AI 协作生成。** 只有 2 个 commit，最后一次带 `Co-Authored-By: SWE-Agent.ai`。
3. **快照会过期。** 本次锁定在 `master` 分支 commit `26426d2b6ab9cb21d5cf5973a57a4406503573ee`（注意：仓库默认分支是 **`master` 不是 `main`**，用 `main` 的 URL 会全部 404）。

**处理方式**：本来源的规则仍参与对撞，但**不与另外两份等权**。一条规则若**只有** gbp 支持、Uber 与 ECC 都不涉及，则它进入 spec 前需要更强的理由（taihu 自己的锚点足够硬才收）—— 因为它的来源权威性撑不起「上游都这么规定」这个说法。

---

## 一、适用（已核实，可直接并入）

### A-01 —— 磁盘 / 线上格式的字段布局用命名常量表达

| 项 | 内容 |
|----|------|
| 上游来源 | 由 `uber-047` 引出。原文主张「被序列化的结构体字段必须打 tag，序列化形式是系统间契约，改名会静默破坏它」 |
| 上游表述是否可直接用 | **不能。** taihu 的二进制格式全部走 `encoding/binary` 手工编解码（8 个文件），不用反射，`json` tag 只覆盖 25 处 JSON；tag 在这里没有承载物 |
| 但意图完全适用 | 上游的意图是「**序列化形式是契约，改动会静默破坏**」。taihu 有磁盘格式（`internal/metastore`）、线上格式（`internal/transport/protocol`）、内核 ABI（`internal/aio`）三类，全都是契约 |
| taihu 的三种锁定强度 | 见下表 —— **这正是本规则的价值：它把已经存在的两档做法，补上缺的第三档** |

| 契约类型 | taihu 现有机制 | 强度 |
|----------|----------------|------|
| 内核 ABI | `unsafe.Sizeof` / `unsafe.Offsetof` **编译期断言**（`internal/aio/aio_uring_linux.go:141-146`） | **最强** —— 改错结构体编译不过 |
| 线上格式 | **命名常量表达布局** + round-trip 测试。`internal/transport/protocol/protocol.go:51` 的 `FrameHeaderLen = 5`、`:63` 的 `SegItemLen = 8 + 1 + 8 + 8`（字段宽度写进常量表达式，改字段立刻可见）；`protocol_test.go` 有 12 个 `TestXxxRoundTrip` | 中 |
| 磁盘格式 | **两侧各写一套魔法数字**：编码 `internal/metastore/meta.go:31-33` 用 `b[1:]` / `b[9:]` / `b[17:]`，解码 `:42-44` 用 `b[1:9]` / `b[9:17]` / `b[17:25]`。有 round-trip 测试（`meta_test.go:6`） | **最弱** |

**拟并入的规则**（措辞待定，落点待定）：

> 磁盘 / 线上格式的字段布局必须用**命名常量**表达，不得在编解码两侧各写一遍魔法数字。
> round-trip 测试可以兜住「编码解码写得不一致」，但**兜不住「两侧一起改错」** —— 那种情况下测试仍绿，而磁盘上的旧数据静默读不出来。线上格式两端同时升级即可，磁盘格式没有这个机会。

（该规则涉及三个包，按 `design.md` §3.1 的归位原则，承载体需要确认 —— 候选是 `engine/index.md` 或新开一个 `engine/on-disk-format.md`。）

---

### A-02 —— 关停路径的超时必须可配或写明理由

| 项 | 内容 |
|----|------|
| 上游来源 | `ecc-015`（context 传超时） |
| taihu 锚点 | **反例**：`internal/transport/server.go:83` —— `GracefulStop()` 里 `context.WithTimeout(context.Background(), 5*time.Second)`，方法不接参数、与父 ctx 断开、无注释 |
| 正例 | `cmd/taihu/cmd/root.go:112` 的 `context.WithTimeout(parent, global.timeout)`，`--timeout` 可配 |
| 与冲突清单的关系 | 该条的裁决在 `conflicts.md` 的 C-03，此处只记「适用 + 有锚点」 |

---

## 二、冲突

见 [`conflicts.md`](./conflicts.md)（评审门，需人逐条裁决）。当前已记录 C-01 ~ C-04。

---

## 三、不适用

### 3.1 `golang-base-practices-skills`：**前 3 个类别整块摘除**（15 条）

该技能包 53 条里，**18~21 条**面向应用/微服务。其中两个类别可以**整类摘除，共 11 条**：

| 编号区间 | 类别 | 为什么不适用 |
|----------|------|--------------|
| `gbp-005` ~ `gbp-009` | Database &amp; ORM（GORM / Goose / SQL 连接池） | taihu 是存储引擎**本身**，不是 ORM 的使用方。它的持久化是自研的 `internal/layout` + `internal/metastore` + `internal/storage`，磁盘格式自己定。要求它「用 Goose 做版本化迁移」是把被实现物当成依赖 |
| `gbp-010` ~ `gbp-015` | DDD Project Structure（四层目录 / Wire 依赖注入） | taihu 的分层是 `internal/{aio,bufpool,device,layout,metastore,storage}` —— 按**资源与机制**分层，不是按 DDD 的业务域分层。改成 `domain/application/infrastructure/interfaces` 与既有的 `layering.md` 直接冲突 |

另有 4 条单条不适用：

| 编号 | 为什么不适用 |
|------|--------------|
| `gbp-001` ~ `gbp-004` | Gin / Kratos 框架选型、JWT 中间件、`http.Server` 优雅停机 —— taihu 无 HTTP 服务端，自研二进制协议在 `internal/transport/protocol` |
| `gbp-020` | REST 错误响应体格式 —— 无 REST 接口；跨边界错误走 `internal/transport/protocol/protocol.go:110-147` 的 **code 不走字符串** |
| `gbp-040` | 「99% 覆盖率硬指标 + 按应用分层设目标」 —— 硬指标不适合按层设；taihu 现状是 20,139 行测试 / 13,254 行非测试（比例高但不按百分比管理） |
| `gbp-045` | testcontainers 起 MySQL 做 HTTP 端到端 —— 无 MySQL、无 HTTP |

若把「部分可迁移但示例是服务向」的 `gbp-042` / `gbp-046` / `gbp-049` / `gbp-052` 也算入，合计 **21 条**。

### 3.2 ECC

待全量对撞。已确认其自带 `examples/go-microservice-CLAUDE.md` 自述 Stack 为「Go 1.22+, PostgreSQL, gRPC + REST (grpc-gateway), Docker, sqlc, Wire」，架构为「Clean architecture with domain, repository, service, and handler layers」—— 该示例所对应的规则整体不适用。

### 3.3 三份来源共同的一个**大空白**（值得单独记）

经全量核对，ECC 的 79 条规则 + 2 个 skill 的**所有**标题块中，**零**内容涉及：
mmap/msync、fsync/O_DIRECT/崩溃一致性/WAL、io_uring/libaio/IOPOLL、on-disk 布局与校验和、LSM/B-tree/compaction、cgo/x-sys、掉电与坏块测试。

**这件事本身的含义**：taihu spec 里最有价值的部分（`engine/` 那几层），**没有任何一份上游来源可以参照**。本轮并入的绝大部分会是语言层与工程层的通用规则；引擎层规则仍只能从 taihu 自己的代码里长出来。这直接支持了 `design.md` §3.2 的准入条件 —— 指不到 taihu 锚点的规则一律不写。

---

## 四、附带发现（taihu 自身问题，由对撞引出，与上游无关）

这一节记的是**对撞过程中顺出来的 taihu 自身问题**。它们不是「上游说该怎样怎样」，而是上游规则逼着我们去看某个角落时，发现那里本来就有毛病。上游不为此负责，但 spec 应该管。

### F-01（**已核实为真缺陷**）—— 磁盘格式写了版本字节，但解码侧从不读它

| 项 | 内容 |
|----|------|
| 位置 | `internal/metastore/meta.go` |
| 事实 | `encode()` 在三处写入版本字节：`:30`（ObjectMeta）、`:57`（WriteCursor）、`:94`（SegmentMeta），值来自 `:14` 的 `metaVersion = byte(0)`。但解码侧 `decodeObjectMeta`（`:38-46`）**只校验 `len(b) != objectMetaLen`**，随后直接读字段，**从不读 `b[0]`** |
| 证据 | 全文件 `metaVersion` 仅出现 4 次：定义 `:14` + 三处 encode（`:30`、`:57`、`:94`）。解码路径**零使用** |
| 后果 | 版本字节被写进磁盘、占着字节，却**不提供任何检测能力**。格式若变更且总长度不变，旧数据会被静默按新布局解读，读出的字段值是错的但不报错 |
| 为什么 round-trip 测试兜不住 | `meta_test.go:6` 的 `TestCodecRoundTripAndDecodeErrors` 两侧用同一份代码，同版本，永远一致。它验证的是「编解码实现自洽」，不是「与磁盘上既有的字节兼容」 |
| 部分缓解 | 长度校验（`:39-41`）能在**字段增删**时拦住；但同长度的字段重排 / 语义变更拦不住 |
| 与 A-01 的关系 | 同源。A-01 讲「布局要用命名常量表达」，F-01 讲「版本号要真的被校验」—— 两者是同一个契约意识的两个方面 |
| 建议 | 本轮只记，不改代码。落成 spec 待办 + 一条「磁盘格式的版本字节必须在解码侧校验」的规则（锚点即此处） |

### F-02 —— 原子操作有两套写法并存

| 项 | 内容 |
|----|------|
| 位置 | 类型化写法：`pkg/taihu-client/picker.go:74`、`:157`（`atomic.Uint64`）、`internal/transport/stats.go:16-19`（`atomic.Int64`）；裸函数写法：13 处 `atomic.LoadInt64` / `StoreInt64` / `AddUint64` 等 |
| 性质 | 不是错误，是「一个主题两套写法」。三份上游都没有直接规定这一点 |
| 与冲突清单的关系 | 独立于 C-01 的裁决 —— 即便 C-01 裁决「维持 taihu（用标准库）」，也建议统一到类型化写法（Go 1.19+ 类型化 API 是标准库推荐的现代形态，且 `go.mod` 是 `go 1.25.0`） |
| 建议 | 新增一条规则统一；具体统一到哪一侧、以及要不要列反例清单，待裁决 |

### F-03（**已核实为真**）—— 「日志三通道」被库代码破了：`internal/device` 直接写 `os.Stderr`

| 项 | 内容 |
|----|------|
| 位置 | `internal/device/device.go:155`、`:295`、`:299` |
| 与 spec 的关系 | `architecture/code-style.md` 规则 10~12 开宗明义「**本仓库只有三条日志通道**」（pingcap/log 压级别 / 标准库 log + 显式前缀 / `[stat]`）。规则 11 进一步断言「`internal/` 下**恰好 5 处** `log.Printf`，全部带显式前缀」 |
| 事实 | 库包 `internal/device` 有 **3 处 `fmt.Fprintf(os.Stderr, ...)`** —— 既不是 `log`、也没有 `[stat]`、也没走 pingcap/log。这是**第四条通道**，而且开在库代码里，正是规则 11 末句劝退的那种（「这是 CLI/服务端进程入口的特权，**库代码不要模仿**」） |
| 全仓分布 | 非测试代码 `os.Stderr` 共 7 处：库代码 3 处（全在 `device.go`），CLI 4 处（`root.go:78` 顶层错误出口、`helpers.go:121`、`bench_storage.go:197`、`key.go:224` 命令输出）—— **CLI 那 4 处是合理的，`device.go` 那 3 处才是反例** |
| 是否刻意 | **无法判定。** 没有任何注释说明为什么绕过 `log`。两处可疑的理由：(a) `:155` 在完成泵 `pump()` 的热路径上，`log` 有互斥锁与时间戳开销；(b) `logComplRetry`（`:293`）已自带「避免刷屏」的节流设计。但两处都只是推测 |
| 另一处不一致 | `logComplRetry` 的两行日志是**中文**，而同为库内诊断的通道二（`taihu: aio `、`compaction: `）一律英文前缀。规则 8 说「面向用户的 CLI 文案用中文」，库内诊断不在其列 |
| 建议 | 二选一，任选都行但必须选：**(a)** spec 承认第四条通道，写明它存在的理由与适用边界（热路径/绕锁）；**(b)** 改 `device.go` 走通道二。倾向 (a) 若 (a) 的理由站得住 —— 但那需要作者给理由，我无法替他补 |

### F-04（**已核实为真**）—— 操作前缀有两套写法，且 spec 的标题与它自己的例子互相打脸

| 项 | 内容 |
|----|------|
| 位置 | `architecture/code-style.md:79` 规则 8「`fmt.Errorf("op: %w", err)`，op 前缀**小写英文**」 |
| 自相矛盾 | 同一条规则的**第二个例子**（`:84`）就是 `internal/rpcclient/dial.go:33` 的 `fmt.Errorf("DialPoolMulti: empty addrs")` —— **大写 CamelCase**。标题说小写，举的例是大写 |
| 代码实况 | 非测试代码 7 处 `fmt.Errorf("[A-Z]`，全是**标识符当 op 前缀**：`DialPoolMulti:`（`internal/rpcclient/dial.go:33`）、`StartCPUProfile:` / `DeviceCapacity:` / `NewStorage:` / `LoadCache:`（`cmd/taihu/cmd/bench_storage.go:100,108,119,126`）、`DeviceCapacity %s:` / `NewStorage:`（`cmd/taihu/cmd/server.go:122,148`）。其余约 30 处 `: %w"` 是小写词组（`tikv txnkv connect:`、`compact:`） |
| 这不是随手写错 | 7 处大写**集中且有规律**：op 前缀取的是**被调用的 API 名**（`NewStorage`、`DeviceCapacity`），而不是动作描述。库内步骤用小写词组、调用某个具名 API 用它的名字 —— 两套各自自洽 |
| 缺的是什么 | spec 只承认了小写那一套，却拿大写那套当例子。**读者照着标题写会对、照着例子写会错** |
| 附带 | `internal/rpcclient/dial.go:33` 那处是**静态字符串**（没有 `%w`，无 err 可包），按惯例应该是 `errors.New` 而非 `fmt.Errorf` —— 与 op 前缀之争无关的小瑕疵 |
| 建议 | 二选一：**(a)** 承认两套并写明各自的适用场合（库内步骤 / 调用具名 API）；**(b)** 统一到小写词组，7 处列成待办。倾向 (a) —— 它描述了代码里真实存在且自洽的规律 |

### F-05（**已核实为真**）—— `guides/` 整层是英文，且内容讲的是 Trellis 工具仓库自己

| 项 | 内容 |
|----|------|
| 事实 | 25 个 spec 文件里，**只有 `guides/` 下 3 个是 0 个中文字符**，其余 22 个全是中文（中文字符数 665~2715）。这与本仓库「spec 正文用中文」的刻意约定（`design.md` §4 亦重申）直接冲突 |
| 更严重的问题 | `guides/cross-layer-thinking-guide.md` 里的小节标题是 **`Cross-Platform Template Consistency`**、**`Generated Runtime Template Upgrade Consistency`**、**`Mode-Detection Probe Checklist`** —— 这些讲的是 **Trellis 模板生成器/运行时模板**，与 taihu 毫无关系。它们是 `trellis init` 从**工具仓库自己的开发文档**里带过来的 |
| 佐证 | 该文件里 `Cross-Platform Template Consistency`（`:126` 与 `:223`）、`Generated Runtime Template Upgrade Consistency`（`:141` 与 `:238`）、`Mode-Detection Probe Checklist`（`:197` 与 `:267`）**三组小节逐字重复** —— 重复本身就是「模板拼接产物」的指纹 |
| 性质 | 不是「英文 vs 中文」的偏好问题。**这一层的内容不是 taihu 的规则**，是工具自带的。留着它 = spec 里有一整层与代码无关的规则，违背本任务 `design.md` §3.2 的准入条件 |
| 建议 | 待裁决，选项见冲突清单 C-05 |

### F-06（**已核实为真**）—— `commits.md` 的两处硬编码数字与具名清单均可证伪

| 项 | 声明 | 实测 |
|----|------|------|
| `architecture/commits.md:9` | 「`git log` 现有 **143 条**提交里……」（写下的时点 `3bc5f91` 实际是 144 条，已差 1；今天 145 条） | **证伪** |
| `architecture/commits.md:75` | 「全仓库 143 条里**只有 2 条**带它（`git log` 逐条比对所得：`c50473a`、`522ae0c`）」 | **证伪**：行首为 `行为不变：` 的实际有 **3 条**（`c50473a`、`3bc5f91`、`876b75c`）。**且具名的 `522ae0c` 不在其中** —— 它正文里的「行为不变」是 bullet 中的一句（`- 行为不变：正常关机路径照旧…`），不是收尾的独立行 |
| 后果 | 下一个人照 `commits.md:75` 去 `git show 522ae0c` 想学这条约定怎么写，会发现那条并不是范例；而真正的范例 `3bc5f91` 是**纯新增文件**的提交，用它当「只搬不改」的范例本身就不对 |
| 结构性教训 | 这两条都是**把当时的计数快照写死进 spec**。它们不会「过时」，它们**从写下的那一刻就在腐烂**（`:9` 写的时候已经差 1）。同类断言一律应写成**可重跑的判据**而非硬编码数字 |
| 建议 | 改成判据式表述 + 重新裁定范例；见冲突清单 C-06 |

**反面参照 —— `testing/unit-tests.md` 那张计数表是「做对了」的样子**（值得一并写进规则，因为它说明了差别在哪）：

我一度怀疑它也在腐烂（第一遍只扫 `internal/ pkg/ cmd/ test/ examples/`，实测 6 个数字全对不上），差一点就报成第 10 条发现。**加上 `third_party/` 重测后，7 个数字里 6 个逐一对上**：

| 断言 | spec 声明 | 全仓实测 | |
|------|----------|---------|---|
| `t.Helper()` | 155 | **155** | ✓ |
| `t.TempDir()` | 97 | **97** | ✓ |
| `t.Cleanup(` | 64 | **64** | ✓ |
| `t.Setenv(` | 6 | **6** | ✓ |
| `t.Skipf(` | 22 | **22** | ✓ |
| `t.Skip(` | 7 | **7** | ✓ |
| `t.Run(` | 65 | 63 | 差 2（唯一一处，未追查） |

**差别在于口径是否写死**：`unit-tests.md` 的计数是「**全仓**（含 `third_party/`）」，谁都能用同一条命令复现；`commits.md:9` 的「143 条」随每次提交改变，没有任何口径能让它保持为真。**同一类「把数字写进 spec」的做法，一个可复现、一个必然腐烂** —— 规则该写的是「数字必须带可复现的口径」，不是「不许写数字」。

### F-07（**已核实为真**）—— `Makefile` 注释引用三个已不存在的路径

| 项 | 内容 |
|----|------|
| 位置 | `Makefile:39`、`:43-44` 的注释 |
| 事实 | 引用 `internal/transport/server_shm.go`（实际是 `server_shm_linux.go`）、`pkg/rpcclient`、`pkg/rpccluster`（已收进 `pkg/taihu-client`）。三个路径**均不存在**（已 `ls` 验证） |
| 性质 | 只是**注释**，不影响 `check-sdk-only` 的实际行为（该目标是按前缀 grep 实现的），门禁不红 |
| 与既有 spec 的关系 | 三个 spec 文件已把旧路径标注为历史路径，**但 Makefile 本身没改**。spec 说 X、注释说 Y |
| 建议 | 归入本轮「只落待办、不改代码/构建」，但**它连 spec 都不是，是构建脚本注释**。是否顺手改由裁决定；见冲突清单 C-07 |

### F-08（**已核实，但性质是「已符合」而非问题**）—— 测试 helper 的 `t.Helper()`

| 项 | 内容 |
|----|------|
| 上游 | `th-250`（测试辅助函数必须以 `t.Helper()` 开头） |
| 实测 | 接收 `*testing.T` 的**非 `Test` 前缀** helper 共 **93 个**，缺 `t.Helper()` 的只有 **2 个**，且都在同一文件：`test/e2e/f_soak_test.go:223`（`fCPUProfilePath`）、`:230`（`fStartCPUProfile`） |
| 实际影响 | **接近零**：`fCPUProfilePath` 函数体只有一行 `return filepath.Join(...)`，**根本不调用任何 `t.*`**；`fStartCPUProfile` 只用 `t.Logf`（在 goroutine 里）。二者都不会让失败行号指错 |
| 结论 | 这条应记为**「taihu 已基本遵守」**（91/93），不是冲突。两个残留是同一文件的顺手清理项，优先级最低 |
| 与 agent 报告的出入 | 盘点 agent 报的是「119 个 helper 中 2 个」——口径不同（它可能把 `Test*` 函数也算进 helper）。以本节实测的 93/2 为准 |

### F-09（**已核实为真**）—— spec 的 62 处裸 basename 引用，其中约 23 处真歧义

盘点 agent 只报出 1 处（`wire-protocol.md:342`），称「全仓唯一」。**实测是 62 处** —— 低估两个数量级。

仓库有 6 组同名文件（`client.go` 有 3 个：`cmd/taihu/cmd/`、`internal/cluster/`、`internal/transport/`；`server.go`、`storage.go`、`instance.go`、`reexport.go`、`version.go` 各 2 个）。逐文件实测「该 basename 在本文件内从未被目录限定过」的裸引用：

| spec 文件 | basename | 裸引用数 | 是否真歧义 |
|-----------|----------|---------|-----------|
| `sdk/public-api.md` | `storage.go` | **17** | **★ 是** |
| `sdk/index.md` | `storage.go` | 7 | **★ 是** |
| `sdk/index.md` | `reexport.go` | 3 | **★ 是** |
| `architecture/layering.md` | `storage.go` | 3 | **★ 是** |
| `cli/command-and-output.md` | `server.go` / `client.go` / `instance.go` | 8 / 5 / 3 | 否（整篇只讲 `cmd/taihu/cmd/`） |
| `transport/wire-protocol.md` | `server.go` / `client.go` | 7 / 5 | 否（整篇只讲 `internal/transport/`） |
| `cli/index.md` | `server.go` / `instance.go` / `client.go` / `version.go` | 4 / 3 / 2 / 2 | 否（同上） |

**为什么 sdk/ 与 layering 那 30 处是真歧义**：这几份文档**恰好同时讨论两侧** —— `pkg/taihu-client/storage.go` 与 `internal/storage/storage.go` 都是它们的主题，读者无法靠语境排除。

**最误导的一处**：`layering.md:109` 与 `sdk/index.md:32-34` 是**导入行清点**，排版成 `| internal/cluster | config.go:13、… storage.go:11 … |` —— 看起来像「`internal/cluster` 目录下的 storage.go」，实际指 **`pkg/taihu-client/storage.go:11`**。而 `internal/cluster/storage.go` **根本不存在**，读者只能靠试错发现。

**性质**：引用本身**全是对的**（562 条全量校验 0 缺失 / 0 越界），问题在**可核验性**。裁决见 `conflicts.md` C-09。

---

## 附：已确认符合上游规则的项（无需裁决）

| 上游编号 | 规则 | taihu 现状 |
|----------|------|-----------|
| `uber-035` | 生产代码必须避免 panic | 已符合：非测试代码 `panic(` 零命中 |
| `uber-008` | 不要内嵌 mutex，用命名字段 | 已符合：`internal/` 下无非测试结构体内嵌 `sync.Mutex`/`RWMutex` |
| `uber-053` | `init()` 不得启动 goroutine | 已符合：全部 12 处 `init()` 均无 goroutine |
| `ecc-015` | `context` 传超时 | 基本符合：8 个包 176 处使用，CLI 层 `--timeout` 可配；唯一例外见 C-03 |

> 注：以上为**抽查**结论，不是全量验证。

### ★ 勘误：`uber-042` 我先前判「已符合」，是**判错了**

初稿在这一行写的是：

> `uber-042` | `init()` 不得做 I/O | **已符合**：唯一的非 cmd `init()`（`internal/transport/frame.go:25`）只做赋值

**「只做赋值」是真的，但它回避了规则真正管的那件事。** 核到 `internal/transport/frame.go:25-29` 的原文：

```go
func init() {
	netpoll.SetAlignedAllocator(bufpool.Get, bufpool.Put)
	netpoll.SetInputAlignedAllocator(bufpool.GetExact, bufpool.PutExact)
	netpoll.SetInputNodeSize(protocol.InputNodeSize)
}
```

`uber-042` 禁止的不只是 I/O，原文还列了「**不访问或操纵全局状态**」—— 而这三行**恰恰是在操纵另一个包的全局状态**（改掉 netpoll 的分配行为）。我当初只核了「有没有 I/O」，没有核「有没有操纵全局」，于是把它读成了合规。

**这个错误的性质值得记下来**：它不是因为锚点假，而是因为**判据取窄了**。规则原文有四条要求，我只验了一条就下了「已符合」。**「已符合」这个结论比「违反」更需要逐条核 —— 因为违反会被后续检查抓出来，误判的「已符合」会让规则从此不再被看**。

正确归属：这是**与 `uber-042` 的冲突**，且**冲突的裁决已经存在** —— 见 `conflicts.md` C-04（维持 taihu，理由是 `init()` 在这里承担装配顺序保证：这三行必须在任何 netpoll 使用之前完成）。所以它该出现在 C-04 的冲突条目里，而不是这张「无需裁决」表里。

**顺带更正一处相关的**：`uber-053`（init 不得起 goroutine）我核过 `frame.go:25` 与其余 11 处 init，确实无 `go` 语句 —— 这条**站得住**，不受上面影响。
