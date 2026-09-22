# 执行计划

## 阶段总览

| 阶段 | 产出 | 完成判据 | 评审门 |
|------|------|----------|--------|
| P1 取料 | `research/` 下四份清单 | 四份齐全；失败项已标注 | — |
| P2 对撞 | `research/triage.md` + `research/conflicts.md` | 每条上游编号都有归属；冲突条目带 `file:line` | **★ 人逐条裁决冲突** |
| P2.3 去重 | `research/dedup-{uber,ecc-gbp}.md` | 118 条新增对 297 条既有规则不重复 | — |
| P3 并入 | 更新后的 `.trellis/spec/**` | 每条并入规则有真实锚点 | — |
| P4 收尾 | 溯源表 / 刻意偏离 / 不适用附录 | 三项齐全 | — |
| P5 验证 | 引用检查 + 门禁回归 | 0 越界 / `make check` 与改前一致 | — |

---

## P1 取料（并行，四路）

四份清单各由一个 research 子代理产出，写到 `research/`：

- `uber-go-style.md` —— Uber Go Style Guide 全量规则，编号 `uber-`
- `everything-claude-code-go.md` —— ECC 的 `rules/golang/` + Go skills/agents/commands，编号 `ecc-`
- `golang-base-practices.md` —— cexll 技能包 53 条，编号 `gbp-`
- `taihu-spec-inventory.md` —— 现有 8 层规则 + 落地程度 + 覆盖空白，编号 `th-`

**验收**：

```bash
ls .trellis/tasks/09-21-spec-upstream-alignment/research/
# 四份都在；每份头部有来源 URL 与抓取日期
grep -c '^| uber-' .../uber-go-style.md    # 条数非零
grep -n '取不到\|失败\|未取到' .../everything-claude-code-go.md   # 失败项显式标注
```

**回滚点**：某份取不到时，不阻塞其余三路；缺口记入溯源表「未参与对撞」。

---

## P2 对撞（串行，我主做）

### P2.1 三分类

以四份清单为输入，产出 `research/triage.md`，三张表：

- **适用**：`| 上游编号 | 规则 | taihu 锚点 file:line | 拟落 spec 文件:节 |`
- **冲突**：`| 冲突编号 | 上游编号 | taihu 现状 file:line | 双方理由 | 建议选项 |`
- **不适用**：`| 上游编号 | 规则 | 为什么不适用 |`

**自检**：每条上游编号必须出现在且仅出现在一张表里。

```bash
# 三张表里出现过的编号去重后，条数应等于上游清单总条数
```

### P2.2 冲突清单（评审门）

从 P2.1 的冲突表扩写成 `research/conflicts.md`，按「是否影响真实行为」排序：

1. **影响真实行为** —— 遵守上游就得改代码语义（如 `maxEvents` 上界那类）
2. **影响 spec 措辞** —— 只是说法不同，不改变实际做法
3. **影响工具链** —— 上游要求某工具，taihu 没装

每条给「建议选项」但不替用户决定。**用户裁决前不进入 P3。**

**裁决记录**：裁决结果回填进 `conflicts.md` 每条末尾（`裁决：维持 taihu / 改 taihu`），作为验收证据。

---

## P3 并入（串行）

按 `design.md` §3.1 的承载体表逐条归位。每条落 spec 前跑锚点验证：

```bash
# design.md §3.2 的准入检查
sed -n "<line>p" <file>   # 该行内容必须与规则陈述对得上
```

### 已裁决的两条并入约定（P2 评审门产出）

**（a）新开 `architecture/go-style.md`** —— 裁决「全开，76 条都收」。Uber 语言层规则集中一处，理由：现有 297 条是围绕 aio/bufpool/protocol 等**机制**长的，对 `time.Time`、`strconv`、原始字符串、`make(chan` 缓冲、内嵌类型等**语言层主题零覆盖**（逐词 grep 实测均为 0 命中）。注意这**不是**新建层 —— `architecture/` 层已存在，只是层内多一个文件。

**（b）有反例的规则照写，并显式标注既有例外** —— 裁决「写规则 + 在 spec 里显式标注既有例外」。照 `architecture/api-surface.md:87` 已立的先例（「新增包照这条来；已有包不要求追溯改造」）：**写应然规则，同时写明仓库现状与判据**，spec 不留矛盾、也不假装已合规。全文统计共 41 条属此类。

**顺序**（先低风险的，避免中途发现结构问题要回退）：

1. `architecture/go-style.md` —— **新建**，量最大、最独立，也是唯一没有存量约束的文件
2. `architecture/code-style.md` —— 存量规则的措辞订正
3. `architecture/error-model.md` —— 错误处理
4. `testing/*.md` —— 测试形态（含 TDD 硬规则）
5. `engine/*.md` / `platform/*.md` —— 引擎与平台
6. `architecture/layering.md` / `api-surface.md` —— 依赖方向与对外面（改动风险最高，放后面）

**每步之后**：

```bash
make check && make check-linux   # 本轮不碰代码，结果应与改前完全一致
python3 .trellis/tasks/09-21-spec-upstream-alignment/research/verify_spec_refs.py  # 引用检查
```

**回滚点**：每完成一个 spec 文件算一个回滚点；发现并入的规则与既有规则冲突时，回退该文件重做，不留半成品。

---

## P4 收尾

1. **溯源表** —— `guides/index.md` 新增章节，反向索引「上游编号 → spec 文件:节」
2. **刻意偏离清单** —— 挂到各主题 spec 文件末尾的 `## 刻意偏离上游规则` 一节
3. **不适用附录** —— 简表，一行一条
4. **既有 spec 自身矛盾点** —— 若 P2 阶段发现，一并记录并修正

---

## P5 验证

### 机器可验证

```bash
# 1. 引用完整性（本轮唯一的机器判据）
python3 .trellis/tasks/09-21-spec-upstream-alignment/research/verify_spec_refs.py
#    期望：文件不存在 0 条；行号越界 0 条
#    （脚本原在 /tmp，按 C-11 裁决随任务入库；/tmp 重启即失，别再用那个路径）

# 2. 门禁回归 —— 本轮不碰代码，必须与改动前逐字一致
make check
make check-linux

# 3. 编码/格式 —— spec 是 markdown，但要确认工作区没有意外改动 Go 文件
git status --short
git diff --stat -- '*.go'          # 期望：空
```

> **实测结果（2026-09-22 收尾）**：本任务对 `.go` 零改动，但执行期间工作区里
> **一直有 5 个与本任务无关的 `internal/aio/` 文件未提交**（`aio.go` / `probe_linux.go` /
> `probe_other.go` 已改，`aio_internal.go` / `probe_cache.go` 未跟踪，mtime 2026-09-21
> 17:37–17:39），所以上面这条在跑的时候并非空 —— 原因是既有的未提交重构，不是本任务。
> 该重构已按人裁决收成独立提交 `6a3b21c`（`refactor(aio)`），它同时补上了 `876b75c`
> 写的 `api-surface.md` 规则 4 所引用、却从未进过 git 的 `aio_internal.go`。
> 提交后 `git status --porcelain -- '*.go'` 才真正为空。

### 人可验证

- 抽查 3~5 条新并入的规则，确认 `file:line` 锚点真实且对得上（这是「规则来自真实代码」的唯一验收方式）
- 读一遍冲突清单的裁决记录，确认每条都有裁决
- 检查是否出现同一主题的两处规定

### 一致性自检

```bash
# 同一主题关键词是否在多个 spec 文件里各规定了一遍（应人工复核每个命中）
grep -rn 'defer\|panic\|recover' .trellis/spec/ | awk -F: '{print $1}' | sort -u
```

---

## 不做的事（防止执行中漂移）

- 不执行任何「改 taihu」的裁决 —— 只落待办
- 不装 `golangci-lint` / `staticcheck` / `revive` 等工具（`gbp-053`、`uber-115` 涉及工具链的条目一律只记不装）
- **不新建 spec 层**（`architecture/` 等 8 层不变）；本次只多一个层内**文件** `architecture/go-style.md`，这是 P2 评审门的明确裁决
- 不碰 `internal/` `pkg/` `cmd/` 下任何 Go 文件 —— P5 验收判据 `git diff --stat -- '*.go'` 必须为空
- 不碰 `third_party/`
- 不因「顺便」而修改与本任务无关的既有规则

---

## P2 评审门产出（已裁决，2026-09-22）

### 去重总账（194 条适用规则 vs 297 条既有 `th-` 规则）

| 来源 | 适用 | 已覆盖 | 部分覆盖 | 新增 | 措辞可改善 |
|---|---|---|---|---|---|
| Uber | 122 | 15 | 15 | **92** | 0 |
| ECC | 40 | 10 | 13 | **16** | 1 |
| GBP | 32 | 13 | 9 | **10** | 0 |
| **合计** | **194** | **38** | **37** | **118** | **1** |

新增 118 条按锚点性质再分：**有反例 41 条**（仓库当下违反，走约定 b）、**仅正例 77 条**（已做对只是没写，防改坏）。

### 四项裁决

| # | 议题 | 裁决 |
|---|---|---|
| 1 | `architecture/go-style.md` 开不开 | **全开，76 条都收**（Uber 新增 92 条中的落点分布：go-style 76 / unit-tests 9 / buffer-and-concurrency 4 / code-style 2 / error-model 1 / public-api 1） |
| 2 | 41 条有反例规则的既有违反怎么处理 | **写规则 + 在 spec 里显式标注既有例外** |
| 3 | F-03（`internal/device` 库代码写 `os.Stderr` 破坏三通道） | **本轮记为待办，另开任务改代码**；spec 只标注 device 是已知例外 |
| 4 | `ecc-034` TDD | **采纳为硬规则**；`fed67b4`、`9bcc797` 记为待改进的历史惯性 |

> `fed67b4` / `9bcc797` 两条实测（供 spec 引用时用真实数字）：`fed67b4` 一次改 84 个文件、其中 **81** 个 `_test.go`（注意：该提交**信息**自称「82 个单测文件」，与 `--stat` 对不上，引用时用 81）；`9bcc797` 改 6 个文件、改动了 `internal/device/{device,device_linux,options}.go` 与 `third_party/shmipc-go/` 3 个文件，**0 个 `_test.go`**。

### 另需在本轮修正的既有 spec 缺陷

- `architecture/commits.md:11` —— type 清单数字全错（写 `docs`(6) 实测 42），且漏列 `revert`(3)、`test`(1)、`init`、`bench`
- `architecture/code-style.md:84` —— th-049 的正例 `fmt.Errorf("DialPoolMulti: empty addrs")` 与同条「op 前缀小写英文」自相矛盾（与 F-04 同源）

### 不采纳（附理由，避免下一轮重复讨论）

- `gbp-036` —— 上游原文称「range 中删 map 是未定义行为」，**Go 语言规范允许**；taihu 三处写法均正确。采纳会把正确代码判成违规
- `uber-062` —— 行宽超 99 字符，上游原文自己写明「allowed to exceed」，**不构成违反**
- `uber-014` —— 枚举从 1 起，已被 `uber-015`（零值即默认）完全吸收，不单列
- `uber-038` —— `go.uber.org/atomic` 在 Go 1.25（本仓库 `go.mod` 版本）已由 stdlib 类型化原子量取代，规则过时
