# 对照 Uber / ECC / golang-base-practices 三份上游规范完善 Go spec

## Goal

学习 Uber Go Style Guide、Everything Claude Code 的 golang 规则、cexll/golang-base-practices-skills 三份上游规范，逐条对照 taihu 真实代码做「适用 / 冲突 / 不适用」三分类，把适用项择优并入 `.trellis/spec/` 现有 8 层（不新建层）；冲突项列成清单交人拍板；不适用项在附录列「已看过但未采纳」简表。本轮只改 markdown，不动代码与 Makefile。

## 背景

`.trellis/spec/` 现有 8 层（guides / architecture / cli / engine / platform / sdk / testing / transport）是在没有上游参照的情况下，从 taihu 自己的代码里反推出来的。好处是每条规则都锚定真实文件，坏处是**覆盖面由「当时踩过哪些坑」决定**，存在系统性盲区 —— 没踩过坑的领域就没有规则（例如 `defer` / `panic` 边界、接口设计的归属、切片预分配、context 传播）。

三份上游是**外部视角**，能补上这类盲区；但它们同时是**外来输入**，其中相当一部分（框架选型、ORM、DDD 目录结构）假设你在写应用/微服务，与 taihu 是存储引擎这一事实不符。因此本轮的核心不是「搬运」，而是**三分类 + 归位 + 冲突裁决**。

taihu 规模：13,254 行非测试 Go / 20,139 行测试 Go / 12 个 `internal/` 包 / `third_party/` 112 个 fork 文件（免 gofmt）。

## Requirements

### R1 —— 三份上游来源取全且可追溯

每份上游都要落成 `.trellis/tasks/09-21-spec-upstream-alignment/research/` 下的一份清单，逐条编号（`uber-xxx` / `ecc-xxx` / `gbp-xxx`），并记录来源 URL、抓取日期、实际取到的文件与取不到的文件。取不到的部分必须显式标注，**不得编造内容**。

### R2 —— taihu 现有 spec 规则盘点

把 8 层里**所有规定性内容**（「必须 / 不得 / 应当 / 默认」）盘成一表，并且逐条去真实代码抽查落地程度，四态之一：`已机器化` / `仅靠人` / `有反例` / `无法判定`。

- 「有反例」必须给 `file:line` 证据，不许靠猜。
- 同时产出**覆盖空白清单**：Go 工程常见主题里，taihu 现有 spec 一条规则都没有的那些。

### R3 —— 三分类对撞

每条上游规则归入且仅归入下列一类：

| 类别 | 判据 |
|------|------|
| **适用** | 能在 taihu 真实代码里找到至少一个锚点，且与现有 spec 不矛盾 |
| **冲突** | 与现有 spec 的某条规则矛盾，**或**与真实代码的矛盾行为矛盾 |
| **不适用** | 面向应用/微服务（框架、ORM、DDD 结构等），taihu 是存储引擎 |

分类结果必须**无遗漏**：每条上游编号都要出现在三类之一的表里。

### R4 —— 适用项并入现有 8 层，不新建层

- 归位原则：**一个主题只有一处规定**。能归进 `code-style.md` / `error-model.md` / `unit-tests.md` 等既有文件的，就归进去；只有确认 8 层都没有承载体时才新开**文件**（不是新开层）。
- 并入的每条规则**必须能指到 taihu 真实 `file:line`**。指不到的，规则不写进 spec —— 宁可少写，不写空规则。
- 产出**溯源表**：上游编号 → 落在哪个 spec 文件的哪一节。

### R5 —— 冲突清单交人拍板（本轮的关键交付）

冲突**不由我单方面裁决**。每条冲突给出一行：

`| 冲突编号 | 上游主张（含编号） | taihu 现状（含 file:line） | 双方理由 | 建议选项 |`

用户逐条裁决后，按裁决结果落 spec：

- 裁决「维持 taihu」→ 写进对应 spec 的**「刻意偏离」**清单，附理由（参照 `bufpool` 否决 `sync.Pool` 那种「有实测依据的偏离」写法）。
- 裁决「改 taihu」→ **本轮不改代码**，记为 spec 里的「已知遗留 + 待办」，交下一轮独立验收。

未获裁决的冲突条目，不得单方面落 spec。

### R6 —— 不适用项附录

三份来源里不适用于 taihu 的规则，列成简表挂在相关 spec 的附录，说明**为什么**不适用（一句话：它假设的项目形态是什么、taihu 是什么）。目的是让下一个人不必重新判断一遍。

### R7 —— 一致性与引用完整性

- 新增规则**不得与既有规则矛盾**。发现既有 spec 自身互相矛盾的地方，本轮一并记录（上一轮 `api-surface.md` 规则 4 与规则 5 就互相抵触过，是写规则 4 时才发现并回改规则 5 的）。
- 本轮结束后，全 spec 的 `path:line` 引用必须**全部有效**（0 条文件不存在 / 0 条行号越界）。

## Non-Goals（明确不做）

- **不改任何 Go 代码** —— 即便裁决为「改 taihu」，也只落成 spec 里的待办
- **不动 `Makefile` / CI / 不引入新工具**（`golangci-lint`、`staticcheck`、`fieldalignment` 等一律不装）
- **不新建 spec 层**
- **不把上游原文抄进 spec** —— spec 里只留结论 + taihu 锚点，原文留在 `research/`
- **不追溯改造已有包的对外面**（那是上一轮 `api-surface.md` 的事）
- **不动 `third_party/`**（fork 回迁上游，免 gofmt）

## Constraints（本轮已拍板的四个决定）

| 决定项 | 选择 |
|--------|------|
| 落地方式 | **逐条择优并入现有 8 层**，不新建层 |
| 冲突裁决 | **逐条列出来一起拍板** —— 我出清单，用户裁决 |
| 取材范围 | **由我判断，只报结论** —— 不适用项只在附录列简表，不逐条论证 |
| 产出形态 | **纯 spec 文本** —— 只改 markdown，不动代码与 Makefile |

另有两项仓库既有约定必须遵守：

- `.trellis/spec/` 的正文**用中文**（刻意如此，不是模板默认）
- 提交时机由人决定（`.trellis/config.yaml` 里 `session_auto_commit: false`）

## Acceptance Criteria

- [x] `research/` 下四份清单齐全：`uber-go-style.md`、`everything-claude-code-go.md`、`golang-base-practices.md`、`taihu-spec-inventory.md` —— 实际 13 个文件（另含 `triage*.md`、`dedup-*.md`、`conflicts.md`、两份校验脚本）
- [x] 三份上游清单的**取不到的文件已显式标注**，无编造内容 —— `golang-base-practices.md:60` 记「53 条规则文件全部抓取成功，无 404、无空文件、无截断」；`triage.md:26` 记「快照锁定在 commit `26426d2…`，仓库默认分支是 `master` 不是 `main`，用 `main` 的 URL 会全部 404」
- [x] `taihu-spec-inventory.md` 里所有「有反例」条目都带 `file:line` 证据 —— 脚本复核 399 条 `th-` 行，标「有反例」却无 `file:line` 的：**0 条**
- [x] 三分类表覆盖**每一条**上游编号，无遗漏 —— 复核 uber 134/134、ecc 79/79、gbp 53/53，缺号均为 0
- [x] 冲突清单存在，每条含「上游主张 / taihu 现状 file:line / 双方理由 / 建议选项」—— `research/conflicts.md`，11 条（A 类 6 / B 类 3 / C 类 2）
- [x] 冲突清单**已获人裁决**，且有裁决记录（每条都能看出裁决结果）—— 11 条 `裁决：` 已全部回填 + 顶部「裁决汇总」表
- [x] 并入 spec 的每条规则都能指到 taihu 真实 `file:line`；指到的一律不含空规则 —— 见下一条的脚本口径；`.trellis/spec/` 内 `To be filled by the team` 占位为 0
- [x] 「刻意偏离」清单存在，每条附理由 —— 4 处 `## 刻意偏离上游规则`：`engine/buffer-and-concurrency.md`（2 条）、`testing/unit-tests.md`（2 条）、`architecture/error-model.md`（1 条）、`architecture/go-style.md`（2 条）
- [x] 不适用项附录存在，每条说明为什么不适用 —— `guides/index.md` §四，62 条（§4.1 uber 8 / §4.2 ecc 36 / §4.3 gbp 18）
- [x] 溯源表存在（上游编号 → spec 文件:节）—— `guides/index.md` §三，114 条已并入 + 78 条已符合
- [x] 现有 spec 自身矛盾点已记录（若有）—— `conflicts.md` 的 C-06（`code-style.md:84` 标题与自己的正例矛盾）、C-08（`commits.md` 两处硬编码计数已证伪、且具名了错误范例 `522ae0c`）
- [x] 全 spec `path:line` 引用检查：**0 不存在 / 0 越界** —— `research/verify_spec_refs.py`：**850 条有效 / 0 / 0**
- [x] `make check` 与 `make check-linux` 结果与改动前**完全一致** —— 两者均 exit 0（check-fmt / check-layering / check-sdk-only / go vet 全 OK；`check-linux: OK`）；**但 `git diff --stat -- '*.go'` 非空**，见下方说明

> ⚠️ **关于 `*.go` 非空（与本任务无关，需人确认）**：工作区有 5 个 `internal/aio/` 文件处于未提交状态 ——
> `aio.go` / `probe_linux.go` / `probe_other.go` 已修改（`git diff --stat`：3 files, +9/−49），
> `aio_internal.go` / `probe_cache.go` 未跟踪。
> 文件 mtime 为 **2026-09-21 17:37–17:39**，早于本轮 spec 工作；形态像一次把 `aio.go` 拆成
> `aio_internal.go` + `probe_cache.go` 的重构。`task.py list` 只有本任务、journal 为空 —— 这次改动
> **没有记录在任何任务里**，不是本任务产生的。
> 本任务对 `.go` 文件**零改动**（`git status --porcelain | grep '\.go$'` 只有上述 5 个）。
> 已据当前工作树复核过 spec 里所有指向 `internal/aio/` 的锚点（含引用这两个新文件的
> `aio_internal.go:18`、`probe_cache.go:12-16`），**全部对得上** —— 即 spec 是按重构后的树写的。
> 但若这两个新文件被回退，相关引用会失效。**建议提交前先决定这 5 个文件怎么处理。**

## Deliverables

| 产物 | 位置 | 性质 |
|------|------|------|
| 三份上游规则清单 | `research/*.md` | 过程产物，可追溯 |
| taihu spec 盘点 | `research/taihu-spec-inventory.md` | 过程产物，含反例证据 |
| 三分类表 | `research/triage.md` | 对撞结果 |
| **冲突清单** | `research/conflicts.md` | **评审门：需人逐条裁决** |
| 更新后的 spec | `.trellis/spec/**` | 最终交付 |
| 溯源表 + 刻意偏离 + 不适用附录 | 挂在相关 spec 文件里 | 最终交付 |

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
