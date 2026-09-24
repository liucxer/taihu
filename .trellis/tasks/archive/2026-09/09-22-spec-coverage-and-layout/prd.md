# 补两条 spec 规则：测试覆盖率下限与工程结构布局

## Goal

在 `.trellis/spec/` 补两条规则：

1. **测试覆盖率下限 ≥80%**，落进 `testing/unit-tests.md`，并把 `internal/aio` 标注为已知例外
2. **工程结构参考 golang-standards/project-layout**，落进新的 `architecture/repo-layout.md`，逐项列出采用 / 不适用，并把唯一实际偏差 `doc/` vs `/docs` 标注为既有例外

只改 markdown。**不动 `Makefile`，不碰任何 `.go` 文件。**

## 来源与现状（已实测，2026-09-22）

### 覆盖率

`go test -cover` 跑遍非 `third_party`、非 `test/e2e` 的包，统计集 **17 个包**：

| 包 | 覆盖率 |
|---|---|
| `internal/aio` | **48.2%** ← 唯一低于 80% |
| `cmd/taihu/cmd` | 82.7% ⚠️（在 2 个已知平台性失败下报出） |
| `cmd/taihu` | 85.7% |
| `internal/device` | 85.9% |
| `internal/bufpool` | 88.4% |
| `internal/transport` | 90.7% |
| `internal/storage` | 91.4% |
| `examples/taihu-client` | 91.7% |
| `internal/cluster` | 92.1% |
| `internal/metastore` | 94.1% |
| `internal/rpcclient` | 97.2% |
| `pkg/taihu-client` | 97.6% |
| `internal/benchkit` / `internal/layout` / `internal/transport/protocol` / `internal/version` | 100.0% |
| `internal/ierr` | `[no statements]` ← 无语句可测 |

16 个有数字，算术平均 **90.4%**；其中 ≥80% 的有 15 个。

**这张表的第一版是错的，2026-09-22 复核时才发现并订正**：原版漏了 `cmd/taihu/cmd` 与 `internal/ierr`，于是包数写成 15、均值写成 90.9%、「已在此之上」写成 11。根因是 `cmd/taihu/cmd` 的 `coverage:` 行被测试二进制的裸 `FAIL` 隔开、**单独占一行**，按「`ok`/`FAIL` 行」抓取会整个漏掉。该陷阱已写进 spec 规则本体，防止下一个人重蹈。

**`internal/aio` 的例外性质（必须写清，不能简化成「平台问题」）**：本机测得的 48.2% 分母只有 137 条语句 —— Linux 专属的 1017 行（占该包 66%）在 macOS 上**不编译、不进统计**，`internal/aio/aio_linux_test.go` 带 `_linux_test` 后缀在本机也不运行，而 `make check-linux` 只做 `go test -c`（编译不执行，见 `Makefile:69`）。所以这是**本机测不到真实覆盖率**，不是「测试写得少」。

### 工程结构

对照 repo 与 `golang-standards/project-layout` 逐项核对：

- **采用**：`cmd/`（目录名 `taihu` 与 `Makefile:1` 的 `BINARY  := taihu` 一致）、`internal/`、`pkg/`、`test/`、`third_party/`、`configs/`、`examples/`、`scripts/`、根级 `Makefile`
- **不适用**：`api/` `web/` `build/` `deployments/` `init/` `tools/` `githooks/` `assets/` `website/` `vendor/`
- **不该有**：`src/` —— 本仓库没有
- **唯一实际偏差**：`doc/` —— project-layout 规定的是 `/docs`

## Requirements

### R1 —— 覆盖率规则

- 写进 `testing/unit-tests.md`，作为**规则 12**（该文件现有规则 1–11 + `## 刻意偏离上游规则`）
- 判据是**按包**统计，不是全仓总百分比
- 目标值 **≥80%，越高越好**
- 必须给出**实测锚点**（上表），让规则可被复核，而不是一句口号
- `internal/aio` 作为**已知例外**显式标注，理由必须是上节那段可检验的机制说明
- 形态为**纯 spec 规则**：不加 `make cover`、不改 `Makefile`

### R2 —— 工程结构规则

- 新建 `architecture/repo-layout.md`，H1 与 topic 命名对齐同层文件风格
- 从 `architecture/index.md` 的「本层文件」表加一行索引
- 逐项列出**采用 / 不适用**，每项说明为什么，不写空表
- `doc/` 作为**既有例外**显式标注，附不改的理由
- **必须写明 project-layout 自称非官方标准**（其 README 原文："NOT an official standard defined by the core Go dev team"），让「参考」这个词在其真实分量上被理解

### R3 —— 溯源表回填

- `guides/index.md` §三 溯源表：新增两条对应行
- `guides/index.md` §四 不适用附录：**`ecc-032`（最低覆盖率 80%）与 `ecc-018`（覆盖率用 `go test -cover`）当前被登记为「不适用」**（见 `:349` / `:348`），本任务推翻该裁决，须从 §4.2 移出并改记
- `guides/index.md` §四 的条目计数（当前 62 / §4.2 为 36）随移动更新

### R4 —— 范围约束

- **不改任何 `.go` 文件、不碰 `Makefile`、不碰 `third_party/`、不碰 `doc/`**（不重命名、不移动）
- **不新建 spec 层**；只新增层内文件 `architecture/repo-layout.md`
- 不因「顺便」修改与本任务无关的既有规则

## Acceptance Criteria

- [x] `testing/unit-tests.md` 有覆盖率规则，含 ≥80% 目标、按包统计的判据、上表实测锚点 —— 规则 12
- [x] 该规则显式标注 `internal/aio` 为例外，理由含「1017 行 Linux 专属本机不编译 / `_linux_test` 不运行 / `check-linux` 只编译」三条机制 —— `unit-tests.md` 的「已知例外」小节
- [x] `architecture/repo-layout.md` 存在，逐项列出采用与不适用，每项有理由 —— 采用 9 / 不适用 10 / 不该有 1
- [x] 该文件显式标注 `doc/` 为既有例外并附理由，且写明 project-layout 自称非官方标准 —— 引了 README 原文 "NOT an official standard defined by the core Go dev team"
- [x] `architecture/index.md` 的「本层文件」表已收录新文件（markdown 链接，非反引号路径）
- [x] `guides/index.md` 溯源表已加两条；`ecc-032` / `ecc-018` 已从「不适用」移出，§4.2 计数同步更新 —— 溯源表 114→116，§4.2 36→34，§4.6 62→60
- [x] `verify_spec_refs.py`：**0 不存在 / 0 越界** —— 有效 860 条（订正引用形态后由 857 增至 860，新增 3 条均被机器校验）
- [x] `To be filled by the team` 占位残留 0
- [x] `make check` 与 `make check-linux` 均 exit 0，且 `git status --porcelain -- '*.go'` 为空
- [x] 覆盖率数字被人复核过一次（抽查 2~3 个包重跑 `go test -cover`，确认与锚点一致）—— **复核推翻了初版数字**：初版漏 2 个包、包数/均值/「已在此之上」三个数全错，已按复核结果订正（见上节）。这正是本条存在的意义

### 复核后的订正（2026-09-22）

独立复核（`trellis-check` 子代理）报了 6 项，我逐条独立复现后确认全部成立并已修：

| # | 问题 | 修法 |
|---|---|---|
| 1 | 「固定口径」命令 exit 1 未说明；`cmd/taihu/cmd` 的 `coverage:` 行被裸 `FAIL` 隔开、按行抓取会漏，该包因此整个缺失 | 表内补 82.7% + ⚠️；补一段说明「退出码 1 是预期的」+ 解析陷阱 |
| 2 | 「15 个包」错，实为 17（漏 `cmd/taihu/cmd` 与 `internal/ierr`） | 改为 17 个包 / 16 个有数字，并写入 `[no statements]` 行 |
| 3 | 「上表 11 个包已在此之上」错，实为 15（原按 ≥90% 数的） | 改为 15 |
| 4 | `guides/index.md` 的 `ecc-065` 行仍写「后者见 `ecc-032`」，而 `ecc-032` 已被移入已并入表 | 改为直指规则 10 / 规则 12，并写明「不适用的只剩 `tdd-guide` agent 这一半」 |
| 5 | `Makefile:1` 引成 `BINARY := taihu`，原文是双空格 | 改双空格（spec 与 prd 两处） |
| 6 | 「失败会打断整个统计」言过其实 —— `go test` 仍逐包打印结果，只是退出码非 0 | 改为准确表述 |

**复核判为正确的**：`internal/aio` 三条机制（1529 行非测试代码 / 1017 行 Linux 专属 = 66.5% / 137 条语句分母 / 48.2%）；「本机没有任何命令能量出该包 Linux 路径的真实覆盖率」不可被证伪；`repo-layout.md` 全部目录断言；`guides/index.md` 的全部计数算术；引用完整性；无主题重复。

## Notes

**已知的既有状况，不在本任务范围内**：「全仓 `go test ./...` 全绿」在本机不成立，这是**工作区干净时就如此**的平台性状态，仓库早有完整记录（`.trellis/spec/platform/build-verification.md:103-108` 列了全部 7 个失败）。与本任务相关的有两个包：

- `third_party/shmipc-go`：测试套件 panic + FAIL（`Test_EventDispatcher`，`event_dispatcher_test.go:126` 空指针）。**这是覆盖率统计范围把 `third_party/` 排除掉的原因。**
- `cmd/taihu/cmd`：2 个失败（`TestBenchStorageCmdErrorPaths` / `TestBenchSingleCmdShmRoundTrip`，行号见 `.trellis/spec/cli/index.md:144-149`）。**这个包不能排除** —— 它是自有代码，所以它的 `coverage:` 数字照常计入规则，代价是**这条统计命令的退出码在 macOS 上恒为 1**。这一点必须写进 spec，否则下一个人会以为命令「跑失败了」。

修那两个失败是另一件事，本任务只记录、只交叉引用，不重复解释（归位原则）。

**为什么判据是按包而不是全仓总百分比**：全仓总数会被大包稀释 —— 一个 48.2% 的 `aio` 摊进 16 个能测出数字的包后总均值仍有 90.4%，异常被抹平。按包统计才能让例外显形。
