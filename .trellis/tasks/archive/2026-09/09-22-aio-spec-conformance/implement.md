# 执行清单：修正 `internal/aio` 审计发现的规范偏差

> 需求在 `prd.md`，技术设计与决策在 `design.md`。本文件是**有序执行清单**。
> 顺序不可交换，理由见 `design.md` §4。
> **每一大步做完即跑验证，不攒到最后。**

## 第 0 步 · 取基线（动手前，只做一次）

改动任何文件**之前**必须先留下可比对的基线，否则验收项「失败集合与改动前一致」无从判定。

```bash
cd /Users/liucx/gopath/src/github.com/liucxer/taihu

# 0.1 既有失败集合（本机 macOS 有 7 个平台性失败，工作区干净时就如此）
go test ./... 2>&1 | grep -E '^(FAIL|--- FAIL)' | sort > /tmp/aio_base_failures.txt
wc -l /tmp/aio_base_failures.txt        # 记下条数

# 0.2 覆盖率基线
go test -coverprofile=/tmp/aio_base.cover ./internal/aio/
go test -cover ./internal/aio/ 2>&1 | tail -1     # 期望 48.2%

# 0.3 逐文件覆盖率（供第 3 大步比对）
#     解析口径：loc,st,c = l.rsplit(' ',2)；命中时累加 **st**（语句数），不是 c
#     基期结果：aio.go 6/36、aio_internal.go 2/7、aio_internal.go 0/8、aio_fallback_other.go 55/81、probe_other.go 3/5

# 0.4 spec 引用基线
#     注意：这个脚本是上一个任务（09-21-spec-upstream-alignment）的 research 产物，
#     **不在** .trellis/scripts/ 下。R13 禁止把辅助工具引入本仓库，所以别把它搬去
#     scripts/ 或 .trellis/scripts/（后者还是 Trellis 模板跟踪目录）。
SPECREF=".trellis/tasks/archive/2026-09/09-21-spec-upstream-alignment/research/verify_spec_refs.py"
python3 "$SPECREF" .     # 基期：有效 860 条；文件不存在 0 条；行号越界 0 条

# 0.5 工作区状态
git status --porcelain
```

**⚠️ 基线陷阱（本轮踩过）**：0.2 的 `go test -cover` 对 `internal/aio` 报 48.2%，按 profile 逐文件求和**必须也等于 66/137 = 48.2%**。若算出的不是 66/137，先怀疑解析脚本 —— 常见错误是 `cov[f] += c`（加了命中次数）而不是 `cov[f] += st`（语句数）。

---

## 第 1 大步 · R2 修上界（行为变更，必须先做）

**文件**：`internal/aio/aio_fallback_other.go`

1.1 把 `:32` 的
```go
if maxEvents <= 0 {
```
改为
```go
if maxEvents <= 0 || maxEvents > 1<<16 {
```

1.2 在 `newLibAIORing` 的文档注释里补一句，说明上界与 Linux 侧一致（对齐 `aio_libaio_linux.go:57` / `aio_uring_linux.go:201` 的判据），避免下一个人以为非 Linux 侧可以放宽。

**验证**：
```bash
go vet ./internal/aio/
make check-linux
# 三处判据必须字面一致：
grep -n 'maxEvents <= 0 || maxEvents > 1<<16' internal/aio/aio_libaio_linux.go \
  internal/aio/aio_uring_linux.go internal/aio/aio_fallback_other.go    # 期望 3 行
```

**回滚点**：1 行改动，`git checkout -- internal/aio/aio_fallback_other.go` 即回到原状。

---

## 第 2 大步 · R1 纠正平台门控（依赖第 1 大步）

**文件**：`internal/aio/aio_linux_test.go`（改）、新增一个不带 tag 的测试文件

2.1 **先做 `TestParseMode`**（独立，不依赖第 1 大步）：
- 把 `TestParseMode`（`:11-44`）整体移到一个**不带 build tag** 的新文件（建议 `internal/aio/mode_parse_test.go`，沿用 `<subject>_test.go` 惯例）。
- 移之前先按 `design.md` §8 的手法验证：单独摘到无 tag 文件跑 `go test -run '^TestParseMode$'`，**确认 PASS**（PRD F16 已实测过，重做一遍以防改动后失效）。

2.2 **再做 `TestNewWithOptionsInvalidMaxEvents`**：
- 它只调 `NewWithOptions` / `r.Close()`，**不引用 linux-only 类型**，所以可以移出。
- 但**必须先完成第 1 大步**，否则 macOS 上会 FAIL（PRD F17）。
- 移到同一个无 tag 文件（或另建 `<subject>_test.go`，与 2.1 保持一致）。

2.3 **给 `internal/aio/aio_linux_test.go` 加文件头注释**，写明它为何仍需门控：
```
//go:build linux
//
// 本文件仍需 linux 门控的原因：剩余两个测试（TestNewWithOptionsEnvOverride、
// TestNewWithOptionsAuto）引用 *uringRing，该类型定义在 aio_uring_linux.go，
// 非 Linux 平台上不存在（摘掉 tag 会编译报錯 undefined: uringRing）。
// 平台无关的测试已移到 mode_parse_test.go。
```
- 注释里**不要**写错别字；上面的「报錯」是示意，实际写「报错」。

**验证**：
```bash
# 移出的测试在 macOS 上必须过
go test -run '^(TestParseMode|TestNewWithOptionsInvalidMaxEvents)$' -count=1 -v ./internal/aio/

# internal/aio/aio_linux_test.go 剩余部分在 linux 视角下仍能编译
make check-linux

# 全仓测试的失败集合不许变
go test ./... 2>&1 | grep -E '^(FAIL|--- FAIL)' | sort > /tmp/aio_step2.txt
diff /tmp/aio_base_failures.txt /tmp/aio_step2.txt && echo "失败集合未变 ✓"

# aio.go 覆盖率必须 ≥ 11/36
go test -coverprofile=/tmp/aio_step2.cover ./internal/aio/
# 用 0.3 的口径解析 aio.go 那一行
```

**回滚点**：`git checkout -- internal/aio/aio_linux_test.go` 并删除新文件。

---

## 第 3 大步 · R3 补 `aio_internal.go` 的测试

**文件**：新增一个不带 tag 的测试文件（建议 `internal/aio/probe_cache_test.go`）；**`aio_internal.go` 本身不许改**。

3.1 按 `design.md` §3 的三个用例写：热缓存命中（哨兵值）、冷缓存穿透（断言第二次**不**穿透）、并发安全（全部返回同一 `Info`）。

3.2 每个用例都要在 `probeMu` 保护下**保存并还原** `probeInfo`/`probeDone`（照 `internal/aio/aio_linux_test.go:128-135` 的手法），避免污染同包其他用例。

3.3 **断言必须平台无关** —— 不许断言 `Probe().Supported` 的具体取值。

**验证**：
```bash
go test -run '^TestProbe' -count=1 -v ./internal/aio/
go test -coverprofile=/tmp/aio_step3.cover ./internal/aio/
# aio_internal.go 必须由 0/8 变为非零

# 该文件确实没被改动
git diff --stat -- internal/aio/aio_internal.go    # 必须无输出

# 两个平台都要过
make check && make check-linux
```

**回滚点**：删除新增的测试文件即可，零影响。

---

## 第 4 大步 · 取新覆盖率数字（回填 spec 用）

```bash
go test -coverprofile=/tmp/aio_final.cover ./internal/aio/
go test -cover ./internal/aio/ 2>&1 | tail -1
# 按 0.3 的口径逐文件解析，记录：
#   - 全包新百分比
#   - aio.go 新值（应 ≥ 11/36）
#   - aio_internal.go 新值（应 > 0/8）
#   - 平台无关三文件合计（基期 8/51 = 15.7%）
```

**这一步的数字要原样抄进第 6 大步的 spec 修正里，不许估算。**

---

## 第 5 大步 · R12 五处行为不变的重构

**逐步做、逐步验**，不要一次改完再验。

5.1 **`_ =` 加理由注释** —— `aio_uring_linux.go:215`（本文件**第一处**）、`probe_linux.go:29`
- 在第一处上方加一行通用理由（清理路径的 `Close`/`Munmap` 失败无补救动作，错误已由主返回值表达），同文件后续同类可省 —— 这正是规则 14 的要求。
- `probe_linux.go:29` 是 `defer func() { _ = unix.Close(fd) }()`，单独加一行。

5.2 **位置式结构体字面量改具名** —— `aio_uring_linux.go:329-342`（13 条 × 4 字段）、`:385-389`（4 条 × 3 字段）
- 逐条补字段名。**先确认 `uringRingField` 的字段定义与顺序**，别按猜的写。
- 改完 `go vet ./internal/aio/` 必须无输出。

5.3 **手写 `Unlock` 改 `defer`** —— `aio_libaio_linux.go:121,160,168,174`
- ⚠️ **这是本任务唯一有争议的一处**：`submitBatch` 在 `errno != 0` 分支里是「先 `Unlock` 再判 errno 分派」。若复查认为「先解锁再处理 errno」是刻意意图，**则不改 `defer`，改为补注释说明该意图** —— 两条路选一条，不能两者都不做。
- 若改 `defer`：`defer r.mu.Unlock()` 紧跟在 `Lock()` 之后。`submitBatch` 末尾的 `r.seq = base + uint64(submitted)` 必须在锁内，`defer` 的语义正好满足。
- 改完必须跑 `make check-linux`。

5.4 **`if/else` 两分支赋同一变量** —— `aio_fallback_other.go:107-113`
- 让 `if` 只选 syscall，`res = result(n, err)` 提到分支外。
- 注意：两条分支的 `n, err` 变量作用域要调整，别让 `n`/`err` 泄漏到分支外产生新的编译错误（`go vet` 会抓）。

5.5 **`mu` 加保护范围注释** —— `aio_fallback_other.go:17`
- `mu sync.Mutex // 保护 seq / inflight / closed`

**验证（每小步都做）**：
```bash
gofmt -l internal/aio/     # 必须无输出
go vet ./internal/aio/     # 必须无输出
make check && make check-linux
go test -count=1 ./internal/aio/
```

---

## 第 6 大步 · R4–R11 七条纯 spec 文本修正

**这一大步不碰任何 `.go` 文件。**

| 步 | 需求 | 文件 | 要点 |
|---|---|---|---|
| 6.1 | R4 | `testing/unit-tests.md` | 改写 `:321-327` 的 aio 例外小节：拆成「Linux 专属 1017 行（量不到）」+「平台无关 51 条语句（可测，受 ≥80% 约束）」，删除「测不到，不是不达标」「不要把 48.2% 当成待补的缺口去追」。用第 4 大步的实测数字回填包表 |
| 6.2 | R5 | `architecture/code-style.md` | `:318` 的 AppendBatch 改 **126 行（`:439-564`）**；正文写明跨度的测量口径（花括号配平、`end - start + 1`、不含下一函数的文档注释）。把全仓 21 个超 50 行函数**逐条登记**并写可检验理由（含 aio 三处：`mapUringRing:222-298`、`submit:483-559`、`Wait:589-644`），写明「什么样的理由算合格」 |
| 6.3 | R6 | `architecture/code-style.md` | `_ =` 分布表把「编译期断言」与「error 丢弃」分开（aio 的 11 处里 6 处是断言、5 处真丢弃）；正文写明 `^[[:space:]]*_ =` 口径的盲区（漏 `defer func() { _ = ... }()`，`probe_linux.go:29`） |
| 6.4 | R7 | `engine/buffer-and-concurrency.md` | 按 `design.md` D1 走**归类**：在 §6 追加一类，写明「存活期由一次同步 syscall 限死 + join 点是公开 API `Wait()`（`:155` 阻塞于 `<-o.done`，`:117` 关闭它）」。同时点名 `client.go:28`、`registry.go:74`、`storage.go:94`；在正文写明 §6.1 表锚在 `go` 行、WaitGroup 段锚在声明行这一锚法差异 |
| 6.5 | R8 | `platform/build-verification.md`、`testing/unit-tests.md` | 两处 off-by-N：`:63` 的 `aio_libaio_linux.go:10-13` → `:11-14`；`:324` 的 `aio_backends_linux_test.go:9-13` → `:9-11` |
| 6.6 | R9 | `platform/file-splitting.md` | `:140` 规则 7 补 `checkIOPoll`（`aio_uring_linux.go:20`）与 `ringQueueDepth`（`:411`），写明对手在 `probe_other.go:17`/`:25` 而非 `aio_fallback_other.go` |
| 6.7 | R10 | `testing/unit-tests.md` | `:57-70` 的表格口径统一（按声明范围重算为 0/0/0/0/55/132/81/57，或保留整仓数并改范围句 + 说明两个口径都给了）；写清 `func Benchmark` 的 0 与整仓 16 的关系；**补可复现命令** |
| 6.8 | R11 | `testing/unit-tests.md` | 按 `design.md` D2 **收窄** `:176` 的措辞为「helper 里借出并返回给调用方的资源用 `t.Cleanup`」，并引用 `aio_test.go:31`（`newTestFile` 的正例）与 `transport_shm_test.go:71-79`（原范例）两处锚点 |
| 6.9 | — | `architecture/api-surface.md` | `:232` 的「待裁定的代码缺陷」（maxEvents 上界）同步改为「已修，见本任务」，保持与第 1 大步一致 |

**6.10 连带一致性（改完上面九项后必查，否则两处数字会打架）**：

| 位置 | 为什么会被波及 | 处置 |
|---|---|---|
| `architecture/index.md:95` 与 `:99` | 两处硬编码「**96 处 `_ =`**」，并说「靠人守」；R6 把口径拆成断言/真丢弃后，这个单一数字不再成立 | 改成 R6 定下的新口径数字（或改成指向规则 14 的转指，不再复述数字） |
| `architecture/index.md:77` | 「丢弃必须 `_ =` + 理由 —— 见 code-style.md 规则 14」，是规则 14 的转指 | R6 若是**改规则标题或编号语义**，此处措辞要跟着改；仅改正文计数则不动 |
| `guides/index.md:49` | 溯源自省表第 5 条：「曾报『139 处 `_ =`』，严格按**行首**计数实际是 **96**」—— 这条教训引用的是 R6 正在改的口径 | 保留教训本身，但数字要跟 R6 同步；若 R6 发现行首口径本身有盲区（`defer func() { _ = … }()`），把盲区补进这条教训里 |
| `testing/index.md:42` | 写着「见 unit-tests.md 规则 12」 | R4 重写规则 12 的内容后，确认这个转指仍然指向"覆盖率下限"这件事（若 R4 把规则 12 拆成两节，转指要更新） |

```bash
# 6.10 的验证：全仓搜这几个数字，看还有什么地方复述了它们
command grep -rn "96 处\|139 处\|133 行\|48.2%\|0/8" .trellis/spec/
# 期望：命中项只剩 R4/R5/R6 定稿后的新数字，没有旧数字残留
```

**验证**：
```bash
python3 ".trellis/tasks/archive/2026-09/09-21-spec-upstream-alignment/research/verify_spec_refs.py" .   # 0 不存在 / 0 越界
grep -rn "To be filled by the team" .trellis/spec/ || echo "无残留占位"
# spec 引用必须用反引号包裹的仓库根相对路径，不得用 Markdown 链接做跨 spec 引用
```

---

## 第 7 大步 · 全量验证与提交

7.1 全量验证：
```bash
make check                      # 必须 exit 0
make check-linux                # 必须 exit 0
python3 ".trellis/tasks/archive/2026-09/09-21-spec-upstream-alignment/research/verify_spec_refs.py" .
go test ./... 2>&1 | grep -E '^(FAIL|--- FAIL)' | sort > /tmp/aio_after.txt
diff /tmp/aio_base_failures.txt /tmp/aio_after.txt && echo "失败集合未变 ✓"
git status --porcelain          # 只应有预期文件
git status --porcelain -- '*.go' | grep -v '^ M internal/aio/\|^?? internal/aio/' || echo "aio 之外无 .go 改动 ✓"
```

7.2 **逐条过 PRD 的 Acceptance Criteria**，勾选前必须亲手复现该条，不许凭印象勾。

7.3 提交（Phase 3.4）：按 `architecture/commits.md` 的格式 —— `type(scope): 中文标题` + 中文 bullet body + `行为不变：` 收尾段（本任务有 R2 一条行为变更，**必须在 body 里单独写明它**，不能整篇都说「行为不变」）。

7.4 提交后：归档任务（`task.py archive --skip-branch-validation`，本仓直推 main）→ 更新 journal → 推送由你决定。

---

## Review Gates（需要停下来给人看的位置）

| Gate | 时机 | 要看什么 |
|---|---|---|
| **G0** | 本清单执行前 | `prd.md` + `design.md` + 本文件（Phase 1.4 的激活门） |
| **G1** | 第 1 大步后 | R2 是**唯一的行为变更**，改动虽小但要单独过目（`prd.md` Notes 已标） |
| **G2** | 第 2 大步后 | 移测试的结果 + `aio.go` 覆盖率是否 ≥ 11/36 |
| **G3** | 第 5 大步的 5.3 | `defer` vs 「补注释保留手写 Unlock」二选一 —— 这是全任务唯一有争议的代码改动 |
| **G4** | 第 6 大步前 | 第 4 大步的实测覆盖率数字（要抄进 spec，抄错就污染规范） |
| **G5** | 提交前 | 全部 Acceptance Criteria 的逐条复现结果 |

## 回滚点汇总

| 步 | 回滚方式 | 代价 |
|---|---|---|
| 1 | `git checkout -- internal/aio/aio_fallback_other.go` | 零 |
| 2 | `git checkout -- internal/aio/aio_linux_test.go` + 删新文件 | 零 |
| 3 | 删新测试文件 | 零 |
| 5 | `git checkout -- internal/aio/` | 零（行为不变） |
| 6 | `git checkout -- .trellis/spec/` | 零（纯 markdown） |

**全任务无数据迁移、无兼容窗口**，所以可以整体 revert。
