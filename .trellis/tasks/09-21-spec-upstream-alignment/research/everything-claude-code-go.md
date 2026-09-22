# Everything Claude Code (ECC) — Go 相关规则与技能全量提取

## 1. 元信息

| 项 | 值 |
|---|---|
| 仓库名（按提示词搜索到的名字） | `affaan-m/everything-claude-code` |
| **仓库全名（canonical，GitHub 重定向后的真实名）** | **`affaan-m/ECC`** |
| URL | https://github.com/affaan-m/ECC |
| 重定向证据 | `GET /repos/affaan-m/everything-claude-code` 返回 `301 Moved Permanently` → `https://api.github.com/repositories/1136590548` → `full_name: affaan-m/ECC` |
| 描述 | "The agent harness performance optimization system. Skills, instincts, memory, security, and research-first development for Claude Code, Codex, Opencode, Cursor and beyond." |
| default_branch | `main` |
| stars | 264229（抓取时） |
| homepage | https://ecc.tools |
| 抓取日期 | 2026-09-21 |
| 抓取方式 | GitHub REST `git/trees/main?recursive=1`（`truncated: false`，共 5031 条）+ `raw.githubusercontent.com/affaan-m/ECC/main/<path>` |
| 仓库内 Go 相关文件总数（顶层，去重后） | 13 个：5 个 `rules/golang/*`、2 个 `skills/golang-*`、2 个 `agents/go-*`、3 个 `commands/go-*`、1 个 `examples/go-microservice-CLAUDE.md` |

### 逐个文件的抓取结果

全部 **成功（HTTP 200）**，无失败项。

| 路径 | HTTP | 字节 | 说明 |
|---|---|---|---|
| `rules/README.md` | 200 | 5694 | 规则体系权威说明（层级/优先级/安装） |
| `rules/common/coding-style.md` | 200 | 3026 | 通用层 |
| `rules/common/testing.md` | 200 | 1400 | 通用层 |
| `rules/common/security.md` | 200 | 862 | 通用层 |
| `rules/common/patterns.md` | 200 | 1022 | 通用层 |
| `rules/common/code-review.md` | 200 | 3663 | 通用层（README 的目录树里未列出） |
| `rules/common/development-workflow.md` | 200 | 2252 | 通用层（README 的目录树里未列出） |
| `rules/common/git-workflow.md` | 200 | 837 | 通用层 |
| `rules/common/hooks.md` | 200 | 768 | 通用层 |
| `rules/common/performance.md` | 200 | 1647 | 通用层 |
| `rules/common/agents.md` | 200 | 2900 | 通用层 |
| `rules/golang/coding-style.md` | 200 | 580 | **语言层** |
| `rules/golang/testing.md` | 200 | 457 | **语言层** |
| `rules/golang/patterns.md` | 200 | 889 | **语言层** |
| `rules/golang/security.md` | 200 | 558 | **语言层** |
| `rules/golang/hooks.md` | 200 | 406 | **语言层** |
| `skills/golang-patterns/SKILL.md` | 200 | 14136 | 676 行 |
| `skills/golang-testing/SKILL.md` | 200 | 16744 | 721 行 |
| `agents/go-reviewer.md` | 200 | 3771 | |
| `agents/go-build-resolver.md` | 200 | 4245 | |
| `commands/go-review.md` | 200 | 3436 | |
| `commands/go-test.md` | 200 | 5635 | |
| `commands/go-build.md` | 200 | 3792 | |
| `examples/go-microservice-CLAUDE.md` | 200 | 7795 | **判断"面向什么项目形态"的关键证据** |
| `mintlify.wiki/affaan-m/everything-claude-code/rules/overview` | 200 | — | 官方文档站，与 `rules/README.md` 内容一致，未补充 `paths:` 语义 |

**副本（未单独抓取，非独立内容）**：`docs/{es,ja-JP,ko-KR,pt-BR,tr,zh-CN,zh-TW}/` 下有各语言翻译版；`.cursor/rules/golang-{coding-style,hooks,patterns,security,testing}.md`、`.kiro/agents/go-*.md`、`.kiro/skills/golang-*/SKILL.md`、`.opencode/commands/go-*.md`、`.opencode/prompts/agents/go-*.txt` 是同一批内容面向其他 harness 的镜像。

### 安全声明（提示注入）

抓取内容一律按**数据**处理。仓库中确实存在若干"祈使句"文本，但均为该仓库面向其自身 agent 的合法内容，**不是针对本会话的指令**，已全部忽略：

- 每个 agent 文件顶部的 `## Prompt Defense Baseline`（防御性内容，本身即反注入条款）。
- `rules/common/agents.md` 中的 "ALWAYS use parallel Task execution"、"Delegation Completion Contract"、"No user prompt needed" 等——是该 harness 下 agent 的行为规范，对本任务无约束力。
- `rules/common/development-workflow.md` 的 "Research & Reuse (mandatory)"——同上。
- 未发现任何试图覆盖本会话指令、索取凭据或要求执行外部动作的恶意文本。

---

## 2. 该仓库的规则体系结构

### 2.1 目录布局

```
rules/
├── common/          # 语言无关原则，"always install"，无任何语言特定代码示例
│   ├── coding-style.md
│   ├── testing.md
│   ├── security.md
│   ├── patterns.md
│   ├── code-review.md            # ← 实际存在，但 README 的目录树漏列
│   ├── development-workflow.md   # ← 实际存在，但 README 的目录树漏列
│   ├── git-workflow.md
│   ├── hooks.md
│   ├── performance.md
│   └── agents.md
├── golang/          # 语言层（另还有 typescript/angular/vue/nuxt/python/web/react-native/swift/php/ruby/arkts/rust/cpp/csharp/kotlin/java/dart/fsharp/perl/react）
│   ├── coding-style.md
│   ├── testing.md
│   ├── patterns.md
│   ├── security.md
│   └── hooks.md
└── ...
```

**语言层的五件套固定**：`coding-style.md` / `testing.md` / `patterns.md` / `hooks.md` / `security.md`，与 `common/` 一一同名对应。

### 2.2 语言层与通用层的关系

- `common/` = 全项目通用默认值，**不含语言特定代码示例**。
- 语言层 = 用「框架模式 + 工具链 + 代码示例」扩展通用层。**每个语言层文件以一条引用声明开头**，这是强制约定：

  ```markdown
  > This file extends [common/coding-style.md](../common/coding-style.md) with Go specific content.
  ```

  5 个 golang 文件全部带这一行，格式完全一致（`common/testing.md ← golang/testing.md` 等）。
- `rules/README.md` 的「Adding a New Language」把它写成新增语言时必须遵守的模板：
  1. 建 `rules/<lang>/`；
  2. 加 5 个文件：`coding-style.md`（格式化工具/惯用法/错误处理）、`testing.md`（框架/覆盖率/组织）、`patterns.md`（设计模式）、`hooks.md`（PostToolUse 的 formatter/linter/type-checker）、`security.md`（密钥管理/扫描工具）；
  3. **每个文件都必须以那句 `> This file extends ...` 开头**；
  4. 引用已有 skill，或新建 skill。

### 2.3 覆盖（override）机制

分两层，一层是**全局声明**，一层是**逐条标记**：

**(a) 全局优先级声明**（`rules/README.md` §Rule Priority，原文）：

> When language-specific rules and common rules conflict, **language-specific rules take precedence** (specific overrides general). This follows the standard layered configuration pattern (similar to CSS specificity or `.gitignore` precedence).

即 `rules/golang/` 整体压过 `rules/common/`。README 举的例子恰好就是 Go：

> `common/coding-style.md` recommends immutability as a default principle. A language-specific `golang/coding-style.md` can override this:
> > Idiomatic Go uses pointer receivers for struct mutation — see common/coding-style.md for the general principle, but Go-idiomatic mutation is preferred here.

**(b) 逐条标记**（README §Common rules with override notes）：

> Rules in `rules/common/` that may be overridden by language-specific files are marked with:
> **Language note**: This rule may be overridden by language-specific rules for languages where this pattern is not idiomatic.

**实测：整个 `rules/common/` 里只有 1 处该标记**——`common/coding-style.md:62`（Naming Conventions 一节）。5 个 `rules/golang/*.md` 中**没有任何一处**携带该标记，也**没有任何一句**显式的 override 文字（包括 README 举例的那个 immutability 覆盖，在实际的 `rules/golang/coding-style.md` 里**并不存在**）。→ 见 §7 差异提示。

### 2.4 规则如何被加载

- **不做插件自动分发**。README 明确：Claude Code plugin 无法自动分发 rules，必须手动安装。
- 两条安装路径：
  - `./install.sh golang`（或 `./install.sh typescript python golang` 多语言）；
  - 手动 `cp -r rules/common .claude/rules/ecc/ && cp -r rules/golang .claude/rules/ecc/`。
- 命名空间：用户级 `~/.claude/rules/ecc/`，项目级 `.claude/rules/ecc/`（"ECC-owned namespace"，避免与其他规则包冲突）。
- **强警告**：必须整目录复制，**绝不能**用 `/*` 打平——common 与语言层同名文件会互相覆盖，且 `../common/` 相对引用会断。
- **激活机制 = `paths:` frontmatter**。5 个 `rules/golang/*.md` 全部以 YAML frontmatter 开头，内容一致：

  ```yaml
  ---
  paths:
    - "**/*.go"
    - "**/go.mod"
    - "**/go.sum"
  ---
  ```

  即：命中 `.go` / `go.mod` / `go.sum` 时才挂载该规则文件。注意 `rules/common/*.md` **没有** frontmatter（全局常驻），这也是层级差别的体现。官方文档站（mintlify.wiki rules/overview）**未提及** `paths:` 的语义，此机制仅能从语言层源文件本身观察到。

### 2.5 Rules vs Skills 的分工

> **Rules** define standards, conventions, and checklists that apply broadly. **Skills** provide deep, actionable reference material for specific tasks.
> Rules tell you _what_ to do; skills tell you _how_ to do it.

实操上：语言层规则文件**主动、显式地**把细节下沉到 skill——`golang/coding-style.md`、`golang/patterns.md` 结尾都有 `See skill: golang-patterns`，`golang/testing.md` 结尾有 `See skill: golang-testing`。所以**Go 规则层的绝对信息量很小（5 个文件合计 2890 字节），真正的 Go 知识在 2 个 skill 里（合计 30880 字节）**。

---

## 3. Go 相关规则全量清单

层级说明：一个 Go 项目实际会同时挂载 `common/`（全 10 个文件，无 `paths:` 门控）+ `golang/`（命中 `**/*.go` 时挂载）。故下表分两组给出。

「可被语言层覆盖」列取值：
- `全局声明` = README §Rule Priority 的全局优先声明覆盖到，但该条**无**显式 `Language note` 标记；
- `显式标记` = 源文件里真的带 `Language note` 那句（全仓仅 1 条）。
- 语言层条目本身即"最高层"，标 `n/a`。

### 3a. 语言层（`rules/golang/`）

| 编号 | 规则一句话陈述 | 出自哪个文件 | 层级 | 是否被标注"语言层可覆盖通用层" |
|---|---|---|---|---|
| ecc-001 | 格式化：`gofmt` 与 `goimports` 是强制项，不讨论风格 | rules/golang/coding-style.md | golang | n/a（本身即语言层） |
| ecc-002 | 设计原则：Accept interfaces, return structs | rules/golang/coding-style.md | golang | n/a |
| ecc-003 | 接口保持小（1-3 个方法） | rules/golang/coding-style.md | golang | n/a |
| ecc-004 | 错误处理：一律用 `fmt.Errorf("...: %w", err)` 包裹上下文 | rules/golang/coding-style.md | golang | n/a |
| ecc-005 | 详细 Go 惯用法与模式以 skill `golang-patterns` 为准（下沉引用） | rules/golang/coding-style.md | golang | n/a |
| ecc-006 | PostToolUse hook：编辑 `.go` 后自动 `gofmt`/`goimports` | rules/golang/hooks.md | golang | n/a |
| ecc-007 | PostToolUse hook：编辑 `.go` 后跑 `go vet` | rules/golang/hooks.md | golang | n/a |
| ecc-008 | PostToolUse hook：对改动的包跑 `staticcheck` | rules/golang/hooks.md | golang | n/a |
| ecc-009 | 用 Functional Options 模式构造对象（`type Option func(*Server)` + `NewServer(opts ...Option)`） | rules/golang/patterns.md | golang | n/a |
| ecc-010 | 接口定义在使用方，而非实现方 | rules/golang/patterns.md | golang | n/a |
| ecc-011 | 依赖注入用构造函数（`NewUserService(repo, logger)`），不用全局 | rules/golang/patterns.md | golang | n/a |
| ecc-012 | 详细 Go 模式（并发、错误处理、包组织）以 skill `golang-patterns` 为准 | rules/golang/patterns.md | golang | n/a |
| ecc-013 | 密钥从环境变量读（`os.Getenv`），缺失即启动失败（`log.Fatal`） | rules/golang/security.md | golang | n/a |
| ecc-014 | 用 `gosec ./...` 做静态安全扫描 | rules/golang/security.md | golang | n/a |
| ecc-015 | 一律用 `context.Context` + `WithTimeout` 做超时控制，`defer cancel()` | rules/golang/security.md | golang | n/a |
| ecc-016 | 测试框架：标准 `go test` + 表驱动测试 | rules/golang/testing.md | golang | n/a |
| ecc-017 | 测试必须带 `-race`（`go test -race ./...`） | rules/golang/testing.md | golang | n/a |
| ecc-018 | 覆盖率用 `go test -cover ./...` | rules/golang/testing.md | golang | n/a |
| ecc-019 | 详细 Go 测试模式与 helper 以 skill `golang-testing` 为准 | rules/golang/testing.md | golang | n/a |

### 3b. 通用层（`rules/common/`，对 Go 项目同样生效）

| 编号 | 规则一句话陈述 | 出自哪个文件 | 层级 | 是否被标注"语言层可覆盖通用层" |
|---|---|---|---|---|
| ecc-020 | 不可变性（CRITICAL）：永远创建新对象，绝不原地修改既有对象 | rules/common/coding-style.md | common | 全局声明（README 点名此条可由 golang 覆盖，但 golang 文件实际未写） |
| ecc-021 | KISS：选能用的最简单方案，避免过早优化，清晰优先于机巧 | rules/common/coding-style.md | common | 全局声明 |
| ecc-022 | DRY：重复逻辑抽公共函数；抽象只在重复真实存在时引入 | rules/common/coding-style.md | common | 全局声明 |
| ecc-023 | YAGNI：不预先构建用不到的抽象与特性 | rules/common/coding-style.md | common | 全局声明 |
| ecc-024 | 多小文件优于少大文件；源文件 200-400 行典型，800 行为软上限（测试/生成/厂商文件可豁免） | rules/common/coding-style.md | common | 全局声明 |
| ecc-025 | 每一层都显式处理错误，绝不静默吞掉；UI 侧给友好信息，服务端记详细上下文 | rules/common/coding-style.md | common | 全局声明（**但 golang/testing.md、go-reviewer 与其一脉相承**） |
| ecc-026 | 只在系统边界做输入校验，schema 化校验，快速失败 | rules/common/coding-style.md | common | 全局声明 |
| ecc-027 | 命名：名字自解释无需注释；布尔名读起来是断言；常量/类型与普通值视觉可分（大小写与前缀交由语言规则定） | rules/common/coding-style.md | common | **显式标记**（`> **Language note**: This rule may be overridden...`，全仓唯一一条） |
| ecc-028 | 少深嵌套，尽早 return | rules/common/coding-style.md | common | 全局声明 |
| ecc-029 | 魔法数字改具名常量 | rules/common/coding-style.md | common | 全局声明 |
| ecc-030 | 长函数拆成职责单一的小块 | rules/common/coding-style.md | common | 全局声明 |
| ecc-031 | 收尾前质量检查单：函数 <50 行、文件 <800 行、嵌套 ≤4 层、有错误处理、无硬编码值、无 mutation | rules/common/coding-style.md | common | 全局声明 |
| ecc-032 | 最低测试覆盖率 80% | rules/common/testing.md | common | 全局声明 |
| ecc-033 | 三类测试全部必需：单元 / 集成 / E2E | rules/common/testing.md | common | 全局声明 |
| ecc-034 | TDD 强制流程：先写测试（RED）→ 跑到失败 → 最小实现（GREEN）→ 跑到通过 → 重构 → 复查 80%+ | rules/common/testing.md | common | 全局声明 |
| ecc-035 | 测试失败排查顺序：用 tdd-guide agent、检查测试隔离、核对 mock，改实现而非改测试（除非测试本身错） | rules/common/testing.md | common | 全局声明 |
| ecc-036 | 测试结构优先 Arrange-Act-Assert | rules/common/testing.md | common | 全局声明 |
| ecc-037 | 测试命名描述行为而非实现（"returns empty array when no markets match query"） | rules/common/testing.md | common | 全局声明 |
| ecc-038 | 提交前硬性检查：无硬编码密钥 | rules/common/security.md | common | 全局声明 |
| ecc-039 | 所有用户输入必须校验 | rules/common/security.md | common | 全局声明 |
| ecc-040 | 防 SQL 注入：参数化查询 | rules/common/security.md | common | 全局声明 |
| ecc-041 | 防 XSS：HTML 转义 | rules/common/security.md | common | 全局声明 |
| ecc-042 | 开启 CSRF 防护 | rules/common/security.md | common | 全局声明 |
| ecc-043 | 校验认证/授权 | rules/common/security.md | common | 全局声明 |
| ecc-044 | **所有端点**加限流 | rules/common/security.md | common | 全局声明 |
| ecc-045 | 错误信息不得泄露敏感数据 | rules/common/security.md | common | 全局声明 |
| ecc-046 | 密钥绝不硬编码，只用环境变量/密钥管理服务；启动时校验存在；泄露即轮换 | rules/common/security.md | common | 全局声明 |
| ecc-047 | 安全事件响应协议：立刻 STOP → 用 security-reviewer agent → 先修 CRITICAL → 轮换已泄露密钥 → 全库排查同类问题 | rules/common/security.md | common | 全局声明 |
| ecc-048 | 强制评审触发点：写完/改完代码后、提交共享分支前、安全敏感代码变更、架构变更、合并 PR 前 | rules/common/code-review.md | common | 全局声明 |
| ecc-049 | 请求评审前置条件：CI 全绿、冲突已解、分支与目标分支同步 | rules/common/code-review.md | common | 全局声明 |
| ecc-050 | 评审检查单：可读且命名好、函数 <50 行、文件内聚（≤800 行或说明例外）、无 >4 层嵌套、错误显式处理、无硬编码密钥、无 console.log/调试语句、新功能有测试、覆盖率 ≥80% | rules/common/code-review.md | common | 全局声明 |
| ecc-051 | 安全评审触发点：认证授权、用户输入处理、数据库查询、文件系统操作、外部 API 调用、密码学操作、支付/金融代码 | rules/common/code-review.md | common | 全局声明 |
| ecc-052 | 严重度分级：CRITICAL=阻断合并 / HIGH=警告 / MEDIUM=提示 / LOW=可选 | rules/common/code-review.md | common | 全局声明 |
| ecc-053 | 通过标准：无 CRITICAL 且无 HIGH 才 approve | rules/common/code-review.md | common | 全局声明 |
| ecc-054 | 重点排查项：硬编码凭据、SQL 注入、XSS、路径穿越、CSRF 缺失、认证绕过 | rules/common/code-review.md | common | 全局声明 |
| ecc-055 | 重点排查项（质量）：>50 行函数、>800 行文件、>4 层嵌套、缺错误处理、mutation、缺测试 | rules/common/code-review.md | common | 全局声明 |
| ecc-056 | 重点排查项（性能）：N+1 查询、缺分页、无界查询、缺缓存 | rules/common/code-review.md | common | 全局声明 |
| ecc-057 | Repository 模式：数据访问藏在统一接口后（findAll/findById/create/update/delete），业务逻辑依赖抽象 | rules/common/patterns.md | common | 全局声明 |
| ecc-058 | API 响应统一信封：success 标志 + data + error + 分页 metadata（total/page/limit） | rules/common/patterns.md | common | 全局声明 |
| ecc-059 | 新功能优先找久经考验的 skeleton 项目（并行 agent 评估安全性/可扩展性/相关性），克隆后在其结构内迭代 | rules/common/patterns.md | common | 全局声明 |
| ecc-060 | 提交信息格式 `<type>: <description>`，type ∈ feat/fix/refactor/docs/test/chore/perf/ci | rules/common/git-workflow.md | common | 全局声明 |
| ecc-061 | PR 流程：分析完整提交历史、`git diff <base>...HEAD`、写完整摘要、附含 TODO 的测试计划、新分支 `push -u` | rules/common/git-workflow.md | common | 全局声明 |
| ecc-062 | 不自动添加 `Co-Authored-By` 尾注（`includeCoAuthoredBy: false`），且不覆盖用户显式选择 | rules/common/git-workflow.md | common | 全局声明 |
| ecc-063 | 实现前强制「Research & Reuse」：先用 `gh search repos`/`gh search code`，再用 Context7/官方文档，再退到 Exa；写工具代码前先查 npm/PyPI/crates.io | rules/common/development-workflow.md | common | 全局声明 |
| ecc-064 | 先规划：用 planner agent 产出 PRD / architecture / system_design / tech_doc / task_list | rules/common/development-workflow.md | common | 全局声明 |
| ecc-065 | 走 TDD：用 tdd-guide agent，RED→GREEN→IMPROVE，验证 80%+ 覆盖 | rules/common/development-workflow.md | common | 全局声明 |
| ecc-066 | 写完代码立刻用 code-reviewer agent，先处理 CRITICAL/HIGH，再尽量处理 MEDIUM | rules/common/development-workflow.md | common | 全局声明 |
| ecc-067 | 提交前复查：CI 全绿、无冲突、分支同步，通过后才请求评审 | rules/common/development-workflow.md | common | 全局声明 |
| ecc-068 | 模型选择：Haiku 做轻量高频/worker，Sonnet 做主开发与编排，Opus 做架构决策与深度推理 | rules/common/performance.md | common | 全局声明 |
| ecc-069 | 上下文管理：不在最后 20% 上下文里做大重构/跨文件实现/复杂交互调试 | rules/common/performance.md | common | 全局声明 |
| ecc-070 | 复杂任务开 Extended Thinking + Plan Mode，多轮批判，按角色拆子 agent | rules/common/performance.md | common | 全局声明 |
| ecc-071 | 构建失败处理：用 build-error-resolver agent，逐条分析、增量修复、每步验证 | rules/common/performance.md | common | 全局声明 |
| ecc-072 | Hook 分三类：PreToolUse / PostToolUse / Stop | rules/common/hooks.md | common | 全局声明 |
| ecc-073 | 自动放行权限慎用；绝不用 `--dangerously-skip-permissions`，改用 `allowedTools` | rules/common/hooks.md | common | 全局声明 |
| ecc-074 | 多步任务用 TodoWrite 跟踪进度、暴露漏项/多余项/粒度错误 | rules/common/hooks.md | common | 全局声明 |
| ecc-075 | Agent 名册以 plugin-scoped `subagent_type` 调用（`ecc:planner`、`ecc:code-reviewer` 等 68 个） | rules/common/agents.md | common | 全局声明 |
| ecc-076 | 无需用户提示即应调用 agent 的四种情形（复杂特性→planner、刚写完码→code-reviewer、修 bug/新功能→tdd-guide、架构决策→architect） | rules/common/agents.md | common | 全局声明 |
| ecc-077 | 独立操作一律并行发起多个 agent | rules/common/agents.md | common | 全局声明 |
| ecc-078 | 委托完成契约：最终消息即交付物，禁止以"等待后台 agent"收尾；委托了就要自己收结果；不要把本可单 agent 完成的任务再拆分 | rules/common/agents.md | common | 全局声明 |
| ecc-079 | 复杂问题用分角色子 agent 做多视角分析（事实核查/资深工程/安全/一致性/冗余） | rules/common/agents.md | common | 全局声明 |

**Go 相关规则合计：79 条**（语言层 19 条 ecc-001..019，通用层 60 条 ecc-020..079）。

---

## 4. 技能（skills）清单

### 4.1 `skills/golang-patterns/SKILL.md`（676 行 / 14136 B）

frontmatter：`name: golang-patterns`，`metadata.origin: ECC`。触发条件："写下/评审/重构 Go 代码，或质疑 Go 惯用结构与约定时"。

| 主题块 | 子主题 |
|---|---|
| When to Activate | 写新 Go 代码 / 评审 / 重构 / 设计包与模块 |
| Core Principles | 1. 简洁与清晰；2. Make the zero value useful；3. Accept interfaces, return structs |
| Error Handling | 带上下文包裹；自定义错误类型；`errors.Is`/`errors.As`；绝不忽略错误 |
| Concurrency | Worker Pool；Context 取消与超时；Graceful Shutdown；errgroup 协调；避免 goroutine 泄漏 |
| Interface Design | 小而聚焦的接口；接口定义在使用处；用类型断言做可选行为 |
| Package Organization | 标准项目布局（`cmd/ internal/{handler,service,repository,config} pkg/ api/v1 testdata/`）；包命名（短小写无下划线）；避免包级状态 |
| Struct Design | Functional Options 模式；用嵌入做组合 |
| Memory and Performance | 已知长度预分配 slice；`sync.Pool` 复用高频分配；循环内禁用字符串拼接（用 `strings.Builder`/`strings.Join`） |
| Go Tooling Integration | 必备命令（build/run/test/vet/staticcheck/golangci-lint/mod tidy/verify/gofmt/goimports）；推荐 `.golangci.yml`（启用 errcheck、gosimple、govet、ineffassign、staticcheck、unused、gofmt、goimports、misspell、unconvert、unparam；`errcheck.check-type-assertions`、`govet.enable: [shadow]`） |
| Quick Reference: Go Idioms | 8 条惯用法速查表（accept interfaces return structs / errors are values / don't communicate by sharing memory / make the zero value useful / a little copying better than a little dependency / clear is better than clever / gofmt is no one's favorite / return early） |
| Anti-Patterns | 长函数里的 naked return；用 panic 做控制流；把 context 放进 struct；混用值/指针接收者 |

### 4.2 `skills/golang-testing/SKILL.md`（721 行 / 16744 B）

frontmatter：`name: golang-testing`，`metadata.origin: ECC`。触发条件："写 Go 测试——表驱动、子测试、benchmark、fuzz、覆盖率时"。

| 主题块 | 子主题 |
|---|---|
| When to Activate | 写新函数/方法、补覆盖率、给性能关键代码写 benchmark、给输入校验写 fuzz、在 Go 项目里走 TDD |
| TDD Workflow for Go | RED-GREEN-REFACTOR 循环；Go 里的分步 TDD |
| Table-Driven Tests | 基本形态；带错误用例的表驱动 |
| Subtests and Sub-benchmarks | 组织相关测试；并行子测试（`t.Parallel()` + `tt := tt`） |
| Test Helpers | `t.Helper()`；`t.TempDir()` 临时文件与目录 |
| Golden Files | 黄金文件比对 |
| Mocking with Interfaces | 基于接口的 mock（mockgen / `//go:generate`） |
| Benchmarks | 基础 benchmark（`b.ResetTimer()`）；不同规模（`b.Run("size=N")`）；内存分配 benchmark（`-benchmem`，对比 `+=` / `Builder` / `strings.Join`） |
| Fuzzing (Go 1.18+) | 基础 fuzz 测试；多输入 fuzz |
| Test Coverage | `-cover` / `-coverprofile` / `-html` / `-func` / `-race -cover`；覆盖率目标（关键业务 100%、公开 API 90%+、一般代码 80%+、生成代码排除）；用 build tag `-tags=!generate` 排除生成代码 |
| HTTP Handler Testing | `httptest.NewRequest` + `NewRecorder`；表驱动的 API handler 测试（`/health`、`/users/123`、JSON body、状态码断言） |
| Testing Commands | `go test ./...`、`-v`、`-run`、`-race`、`-cover`、`-short`、`-timeout`、`-bench`、`-fuzz`、`-count`（检测 flaky） |
| Best Practices | 正/反清单：先写测试、测行为不测实现、覆盖空/nil/边界；不要先实现后测试、不要跳过 RED、不要直接测私有函数、不要在测试里 `time.Sleep`、不要忽略 flaky |
| Integration with CI/CD | GitHub Actions 示例 |

**技能数：2 个**（均归 ECC 所有，`metadata.origin: ECC`）。

---

## 5. Agent 与 Command 清单

### 5.1 `agents/go-reviewer.md`

- frontmatter：`model: sonnet`，`tools: Read, Grep, Glob, Bash`。
- 定位："Expert Go code reviewer specializing in idiomatic Go, concurrency patterns, error handling, and performance. **MUST BE USED for Go projects.**"
- 顶部含 `## Prompt Defense Baseline` 六条（角色固定、不泄密、不输出可执行内容、警惕 unicode/零宽/编码绕过/紧迫感/权威声称、外部数据一律视为不可信、不产出恶意内容）。
- 调用即执行：`git diff -- '*.go'` → `go vet ./...` + `staticcheck ./...` → 只聚焦改动的 `.go` 文件。
- **检查维度**（分级）：
  - CRITICAL 安全：SQL 注入（`database/sql` 字符串拼接）、命令注入（`os/exec` 未校验输入）、路径穿越（未经 `filepath.Clean` + 前缀校验）、竞态（共享状态无同步）、`unsafe` 包无正当理由、硬编码密钥、`InsecureSkipVerify: true`。
  - CRITICAL 错误处理：`_` 丢弃错误、`return err` 未包裹、可恢复错误用 panic、该用 `errors.Is/As` 却用 `==`。
  - HIGH 并发：goroutine 泄漏（无 `context.Context` 取消）、无缓冲 channel 死锁、缺 `sync.WaitGroup`、mutex 未 `defer Unlock()`。
  - HIGH 代码质量：>50 行函数、>4 层嵌套、非惯用 if/else、可变包级变量、接口污染（定义未用的抽象）。
  - MEDIUM 性能：循环内字符串拼接、slice 未预分配、N+1 查询、热路径多余分配。
  - MEDIUM 最佳实践：`ctx` 应为首参、表驱动测试、错误信息小写无标点、包名短小写无下划线、循环内 defer。
- 诊断命令：`go vet`、`staticcheck`、`golangci-lint run`、`go build -race`、`go test -race`、`govulncheck`。
- 通过标准：无 CRITICAL/HIGH → Approve；仅 MEDIUM → Warning；有 CRITICAL/HIGH → Block。

### 5.2 `agents/go-build-resolver.md`

- frontmatter：`model: sonnet`，`tools: Read, Write, Edit, Bash, Grep, Glob`（**唯一有写权限的 Go agent**）。
- 定位：Go 构建/vet/编译错误修复专家，要求**最小、外科手术式**改动。
- 职责：诊断编译错误、修 `go vet`、清 `staticcheck`/`golangci-lint`、处理模块依赖问题、修类型错误与接口不匹配。
- 工作流：`go build ./...` → 读受影响文件 → 最小修复 → 重新 build → `go vet` → `go test`。
- **常见错误→修法映射表（10 条）**：`undefined: X`、`cannot use X as type Y`、`X does not implement Y`、`import cycle not allowed`、`cannot find package`、`missing return`、`declared but not used`、`multiple-value in single-value context`、`cannot assign to struct field in map`、`invalid type assertion`。
- 模块排障：`grep "replace" go.mod`、`go mod why -m`、`go get pkg@v1.2.3`、`go clean -modcache && go mod download`。
- 原则：只做外科手术式修复不重构；**未经明确许可绝不加 `//nolint`**；非必要不改函数签名；增删 import 后必须 `go mod tidy`；修根因不压症状。
- 停止条件：同一错误 3 次未修好 / 修完引入更多错误 / 需超出范围的架构改动。
- 输出格式：`[FIXED] file:line` + Error + Fix + Remaining errors，末尾 `Build Status: SUCCESS/FAILED | Errors Fixed: N | Files Modified: list`。

### 5.3 Commands（3 个）

| Command | 作用 | 关键内容 |
|---|---|---|
| `/go-review` | 调用 **go-reviewer** agent 做 Go 专项评审 | 6 步：定位 `.go` 改动 → 静态分析（vet/staticcheck/golangci-lint）→ 安全扫描 → 并发评审 → 惯用法检查 → 分严重度出报告。CRITICAL/HIGH/MEDIUM 三级清单与 agent 一致。审批表：PASS / WARNING / FAIL。 |
| `/go-test` | 强制 Go TDD 工作流 | 6 步：先定类型与接口签名 → 写表驱动测试（RED）→ 跑到失败且失败原因正确 → 最小实现（GREEN）→ 重构 → 查覆盖率 ≥80%。含完整 Email Validator 示例会话。覆盖率目标表：关键业务 100% / 公开 API 90%+ / 一般 80%+ / 生成代码排除。DO/DON'T 清单（同 skill）。 |
| `/go-build` | 调用 **go-build-resolver** agent 增量修构建错误 | 诊断命令集、按文件分组按严重度排序、一次修一个、每步验证。修复策略优先级：build 错误 → vet 警告 → lint 警告。停止条件与 agent 一致。 |

三者互相引用形成闭环：`/go-test` 先跑 → `/go-build` 修构建 → `/go-review` 评审后再提交。

---

## 6. 该仓库假设你写的是什么类型的 Go 项目（重点）

### 结论

**它假设的是「云原生 API 后端 / 微服务」**：HTTP/gRPC 服务进程 + 外部 SQL 数据库（PostgreSQL 为主）+ Web 框架与 ORM/查询生成器 + 经典分层架构（handler → service → repository）/ 轻量 DDD + 容器与 K8s 部署。**规则与技能里几乎完全不涉及存储引擎、系统编程、无服务端的库或 CLI 工具**。

### 证据（逐条）

1. **`examples/go-microservice-CLAUDE.md`（最直接、最权威）**，第 3 行自述：
   > "Real-world example for a Go microservice with PostgreSQL, gRPC, and Docker."
   > Stack: "Go 1.22+, PostgreSQL, gRPC + REST (grpc-gateway), Docker, **sqlc** (type-safe SQL), **Wire** (dependency injection)"
   > Architecture: "**Clean architecture** with domain, repository, service, and handler layers. gRPC as primary transport with REST gateway for external clients."

2. **同一文件的目录结构**：`cmd/server/main.go`、`internal/{domain,service,repository/postgres,handler/{grpc,rest},config}`、`proto/user/v1/`、`queries/*.sql`、`migrations/00X_create_users.{up,down}.sql`。部署："Docker image built in CI, deployed to **Kubernetes**"。环境变量有 `DATABASE_URL`、`GRPC_PORT`、`REST_PORT`、`JWT_SECRET`、`OTEL_ENDPOINT`。

3. **`skills/golang-patterns` 的「Standard Project Layout」**明确写出：
   ```
   internal/handler/   # HTTP handlers
   internal/service/   # Business logic
   internal/repository/# Data access
   pkg/client/         # Public API client
   api/v1/             # API definitions (proto, OpenAPI)
   ```
   —— 这就是 REST 服务模板，不是系统软件布局。

4. **`skills/golang-testing` 有整节「HTTP Handler Testing」**：`httptest.NewRequest` / `NewRecorder`，用 `/health`、`/users/123`、POST `/users` 作示例断言 JSON body 与状态码。

5. **`rules/golang/patterns.md` 的示例对象是 `Server`**：`WithPort(port int)`、`NewServer(opts ...Option)`、`s := &Server{port: 8080}` —— 一个监听端口的网络服务。

6. **`rules/golang/security.md` 的密钥示例是 `OPENAI_API_KEY`**（AI/Web 服务语境），而非任何系统级 secret。

7. **`rules/common/patterns.md`** 规定 Repository 模式与「API Response Format 统一信封（success/data/error + 分页 metadata total/page/limit）」—— 典型 Web API 关注点。

8. **`rules/common/testing.md`** 把集成测试定义为 "API endpoints, database operations"，E2E 定义为 "critical user flows"。

9. **`rules/common/code-review.md` / `agents/go-reviewer.md`** 的检查项全是 Web 安全与 ORM 性能：SQL 注入、XSS、CSRF、限流、路径穿越、**N+1 查询**、缺分页、无界查询。

10. **`rules/common/security.md`** 要求 "Rate limiting on all endpoints" —— 假设存在 HTTP 端点。

### 对 taihu（存储引擎 / 系统编程）的直接含义：覆盖缺口

该仓库的 Go 规则集**没有任何一条**涉及下列 taihu 核心议题（已全量核对 79 条规则与 2 个 skill 的所有标题块）：

- 无 `mmap` / `msync` / `madvise` / 大页 / NUMA
- 无 `fsync` / `fdatasync` / `O_DIRECT` / 写屏障 / 崩溃一致性 / WAL / 日志重放
- 无块设备、裸盘、`io_uring` / `libaio` / AIO、IOPOLL
- 无 on-disk 布局、页格式、校验和、数据格式版本与升级兼容
- 无 LSM / B-tree / 压缩（compaction）/ 快照 / MVCC
- 无 cgo / CGO 边界 / unsafe 的正当系统级使用（`unsafe` 在 go-reviewer 里被当作 CRITICAL 一律质疑）
- 无 syscall 层编程、`syscall` / `golang.org/x/sys/unix` 用法
- 无长时运行守护进程的资源治理、信号处理细节（只有基础的 graceful shutdown）
- 无模糊测试之外的故障注入、掉电测试、磁盘满/坏块等异常路径测试

反倒有两条**与存储引擎语境可能冲突**的规则需要留意：
- **ecc-020（不可变性 CRITICAL）**：要求"绝不原地修改"，而零拷贝/复用缓冲/原地编码是存储引擎的常规手法。README 声称 golang 层可覆盖此条，但**实际 `rules/golang/coding-style.md` 并没有写这个覆盖声明**（见 §7）。
- **ecc-015（一律 `context.Context` + 5s 超时）** 与 **ecc-027 命名**等偏应用层约定，在磁盘 I/O 路径上需要按场景放宽。

可迁移性最高的部分集中在：**错误包裹与 `errors.Is/As`（ecc-004/ecc-025）、goroutine 泄漏与 `context` 取消（agent 的 HIGH 并发维度）、`-race` 测试（ecc-017）、benchmark/`-benchmem`/fuzz 方法论（golang-testing）、预分配 slice 与 `sync.Pool`（golang-patterns Memory 节）、`go vet`/`staticcheck`/`golangci-lint`/`gosec`/`govulncheck` 工具链（ecc-007/ecc-008/ecc-014）**。

---

## 7. 差异与存疑提示

1. **README 举例的覆盖并不存在**：`rules/README.md` 明确用「golang 覆盖 common 的 immutability」作为 override 示例，但实际的 `rules/golang/coding-style.md` 只有 29 行、5 个要点，**没有任何一句 override 声明**。集成时不要把 README 的例子当成 golang 规则文件的内容。
2. **`Language note` 标记全仓仅 1 处**（`rules/common/coding-style.md:62` Naming Conventions）。README 描述的"会被覆盖的通用规则都带此标记"在实践中几乎未落实，所以**不能**用该标记来判定某条通用规则是否适用于 Go —— 必须走 README §Rule Priority 的全局声明。
3. **README 的目录树漏列 2 个 common 文件**：实际 `rules/common/` 有 10 个文件，README 只列了 8 个（`code-review.md` 与 `development-workflow.md` 未列）。抓取时应以目录实际内容为准。
4. **仓库改名**：`affaan-m/everything-claude-code` → `affaan-m/ECC`。用旧名走 `raw.githubusercontent.com` 仍可用（本次全部 200），但 API 会 301。写死 URL 的地方建议用 `affaan-m/ECC` 或接受重定向。
5. **语言层文件极小**：5 个 golang 规则文件合计 2890 字节，是"指针层"；实质 Go 知识（约 31 KB）在 2 个 skill。任何"提取 Go 规则"的工作若只抓 `rules/golang/` 会严重失真。
6. `paths:` frontmatter 的作用仅在源文件中可观察，**官方文档站无任何说明**，属未文档化机制。
