# layout —— 顶层目录布局（golang-standards/project-layout）

> 来源：`golang-standards/project-layout`（`master`，核对于 `a9d6fae`）。
> 这一层不来自 gbp——它是本 spec 的第二个来源。
>
> **它不是官方标准。** 该仓库 README 自己声明它是**社区约定**、非 Go 官方规范，且 `pkg/` 这类模式「不被所有人接受」。所以本层的用法是：**project-layout 提议、本仓库逐条裁决**，而不是照搬。

## 本仓库的实际顶层

```
cmd/  configs/  doc/  examples/  internal/  pkg/  scripts/  test/  third_party/
Makefile  README.md  LICENSE  go.mod  go.sum  AGENTS.md
```

对照 project-layout 的 19 个 Go/服务/通用目录，**命中 8 个、缺 11 个**：

| project-layout 目录 | 本仓库 | 裁决 |
|---|---|---|
| `/cmd` | ✅ `cmd/taihu/` | 适用，见下 |
| `/internal` | ✅ `internal/` | 适用，见下 |
| `/pkg` | ✅ `pkg/taihu-client/` | 适用，见下 |
| `/configs` | ✅ `configs/` | 适用，见下 |
| `/scripts` | ✅ `scripts/` | 适用，见下 |
| `/test` | ✅ `test/e2e/` | 适用，见下 |
| `/examples` | ✅ `examples/` | 适用 |
| `/third_party` | ✅ `third_party/shmipc-go/` | 适用 |
| `/docs` | ⚠️ `doc/`（单数） | **既有例外**，见下 |
| `/api` | ❌ 无 | 不适用（无 OpenAPI/JSON schema/协议定义文件） |
| `/web` | ❌ 无 | 不适用（无 Web 前端、无模板、无 SPA） |
| `/init` | ❌ 无 | 不适用（无 systemd/supervisord 配置入库） |
| `/build` | ❌ 无 | 不适用（无容器/发行包配置、无 CI 配置） |
| `/deployments` | ❌ 无 | 不适用（部署配置不入库，见下） |
| `/tools` | ❌ 无 | 不适用（无 Go 支持工具；Python 运维脚本归 `/scripts`，见下） |
| `/githooks` | ❌ 无 | 不适用（无 git hooks） |
| `/assets` | ❌ 无 | 不适用（无图片/logo 等静态资产） |
| `/website` | ❌ 无 | 不适用（无独立站点） |
| `/vendor` | ❌ 无 | 不适用（用 module proxy，见下） |
| **`/src`** | ❌ 无 | ✅ **本条是禁令，本仓库遵守**，见下 |

---

## `/src`：project-layout 明确说「不该有」

这一条是本层里唯一的**硬禁令**，且本仓库遵守了。

project-layout 把 `/src` 归在 "Directories You Shouldn't Have" 下，原文：

> Some Go projects do have a `src` folder, but it usually happens when the devs came from the Java world... If you can help yourself try not to adopt this Java pattern. You really don't want your Go code or Go projects to look like Java :-)
>
> Don't confuse the project level `/src` directory with the `/src` directory Go uses for its workspaces.

**对本仓库：无 `/src`，符合。新增目录时不要引入它。**

**这里有一条真实的混淆风险**：本仓库的物理路径是 `$GOPATH/src/github.com/liucxer/taihu`——**这个 `src/` 来自 `$GOPATH`，不是项目内的 `/src`**。project-layout 那段话点名的正是这个混淆。**不要把 `$GOPATH/src` 当成"项目里有 src 目录"来看**，也不要在项目内建 `src/`。

---

## 逐条裁决

### `/cmd` — 适用，主入口符合

**规则**：`/cmd` 放主应用；**每个应用一个目录，目录名必须与可执行文件名一致**；**不要把大量代码放进应用目录**；小 `main` 只负责 import 并调用 `/internal`、`/pkg` 的代码。

**对 taihu：适用，主入口严格符合。**

`cmd/taihu/main.go` **只有 28 行**，做的正是「import 并调用」：

```go
// cmd/taihu/main.go
func main() { os.Exit(run(os.Args[1:])) }
```

目录名 `cmd/taihu/` 与可执行文件名 `taihu` 一致 ✓。

**⚠️ 一处需要说明的张力：`cmd/taihu/cmd/` 有 2469 行非测试代码。**

严格按「不要把大量代码放进应用目录」读，这 2469 行是超标的。**本 spec 的裁决是保留现状**，理由：

1. `/cmd` 那条规则的**判据**是它自己给的——「代码能否被别的项目 import？能 → `/pkg`；不能且不想被别人复用 → `/internal`」。`cmd/taihu/cmd/` 里的内容是 **CLI 命令定义**（`key.go` / `cluster.go` / `bench_*.go` 等），**没有外部调用方**，属于「不想被复用」。
2. 按第 1 条的判据，它确实**可以**移到 `/internal`。但它是 **CLI 的表示层**——`root.go` 定义 cobra 命令树、`server.go` 定义启动参数、`helpers.go` 定义输出格式。放进 `internal/` 会让「引擎内部机制」与「命令行界面」混在同一个目录下，反而削弱 `internal/` 按机制分层的清晰度（见 [ddd/](../ddd/index.md) 对分层的那段）。
3. 迁移是一次**无行为收益的大范围改名**（含 5 个测试文件、`kvConnect` / `newTiKVKV` 两个测试缝隙的引用面）。

**所以：新增 CLI 子命令时按现状放 `cmd/taihu/cmd/`。** 但如果某段逻辑**能被 `internal/` 或 `pkg/` 复用**（例如一个可复用的格式化函数），按规则它应当**下沉**，不要留在 `cmd/` 下。

### `/internal` — 适用

**规则**：私有代码，Go 编译器强制不可外部导入；可按需加 `internal/app`（应用）与 `internal/pkg`（共享私有库）的细分，**但这是可选的、不是必须的**。

**对 taihu：适用。** `internal/{aio,benchkit,bufpool,cluster,device,ierr,layout,metastore,rpcclient,storage,transport,version}` 扁平一层，**不采用 `internal/app` + `internal/pkg` 的细分**——project-layout 原文写明「It's not required (especially for smaller projects)」，本仓库有 Makefile 门禁保证依赖方向（见 [ddd/](../ddd/index.md)），细分不带来额外保证。

**`internal/` 的语义在本仓库比 project-layout 说的更强**：它不只是「不想被外部导入」，还是 **Makefile 两条门禁的管辖范围**——`internal/` 不得依赖 `pkg/`。**新增包时要知道自己进了这个管辖范围。**

### `/pkg` — 适用，但本仓库的语义比规则更窄

**规则**：可被外部应用使用的库代码；**放之前要想两次**；`internal` 因为编译期强制，是更好的私有化手段；`pkg` 只是**显式声明「这里的东西外部可以安全使用」**的方式。

**对 taihu：适用，且本仓库把范围收得更窄。**

`pkg/` 下**只有一个包**：`pkg/taihu-client/`（包名 `taihuclient`，目录名带连字符、包名不能带）。

**本仓库对 `/pkg` 的约束严于 project-layout**：project-layout 只说「外部可用的库」，本仓库另有 `check-sdk-only` 门禁——**`pkg/` 不得直接依赖存储引擎**（`internal/{storage,device,aio,bufpool,layout}`，测试除外），只应依赖 `transport` / `cluster` / `metastore` / `ierr` / `version`。

**这条约束的方向是不可逆的**：一旦 `pkg/` 依赖了引擎内部，SDK 就被绑死在服务端实现上，外部调用方也就继承了那层依赖。

**所以新增 `pkg/` 下的包之前要问**：它是不是**面向外部调用方**的？不是 → 放 `internal/`。判据与门禁一致，不是风格偏好。

### `/configs` — 适用

**规则**：配置文件模板或默认配置。

**对 taihu：适用。** `configs/taihu-server.example.sh` —— 名字里的 `.example.` 正是「模板」语义。

**注意本仓库的配置载体是 shell 脚本而不是 YAML/TOML**（服务端参数走环境变量），这是刻意的。`configs/taihu-server.example.sh:8` 里已写明一条必须遵守的约束：

> 长参数一律用双横线（`--listen` 而非 `-listen`；pflag 会把单横线长参数当短参数簇拒绝）。

**不要因为 project-layout 的例子里是 `confd` / `consul-template` 就改成本仓库没有的格式**；同理，改这个模板时不要顺手把双横线改成单横线——那是能跑与不能跑的区别，不是风格。

### `/scripts` — 适用

**规则**：构建、安装、分析等各类操作的脚本；**存在的意义是让根目录 Makefile 保持小而简单**。

**对 taihu：适用。** `scripts/proxy.py` + `scripts/proxy_client.py` —— NEFS 节点的远程执行 / 文件传输 agent 及其客户端。

**说明一处不完全对齐**：这两个脚本是**运维工具**（部署与节点管理），不是 project-layout 举例的 build / install / analysis。`/scripts` 的原文范围是「build, install, analysis, **etc operations**」——运维脚本落在 `etc operations` 里，**本条不判为偏离**。

**为什么不放 `/tools`**：project-layout 对 `/tools` 的定义包含「**these tools can import code from the `/pkg` and `/internal` directories**」——它指的是 **Go 工具**（要能 import 本项目的包）。`proxy.py` 是 Python，不满足这个前提，**放 `/tools` 反而是错的**。

**与 Makefile 的关系**：本仓库的 Makefile 只有 8 个目标、70 行（`all` / `build` / `clean` / `check` / `check-fmt` / `check-layering` / `check-sdk-only` / `check-linux`），已经是「小而简单」。**新增构建步骤时优先加进 `scripts/`，不要让 Makefile 膨胀**——这是本条规则的实际约束力所在。

### `/test` — 适用

**规则**：**额外的、外部的**测试应用与测试数据；目录内部结构随意；`/test/data` 或 `/test/testdata` 会被 Go 忽略（`.` 或 `_` 开头的目录同样被忽略）。

**对 taihu：适用。** `test/e2e/` 是端到端套件，与单测分离：

```
test/e2e/{a_data,b_transport,c_registry,d_lifecycle,e_concurrency,f_soak,g_soak_long}_test.go
test/e2e/harness_test.go   doc.go
```

**要点一**：`/test` 是**外部**测试目录——它测的是**已构建的二进制或一个真实运行的集群**，不是包内单测。包内单测与被测代码同目录，一律 `_test.go`。**不要把包内单测挪进 `test/`。**

**要点二**：整个目录带 `//go:build e2e`，**不设 `E2E_PD` 环境变量时全部 Skip**，默认 `go test ./...` 不会跑它。

**要点三（数据目录命名）**：若将来需要测试数据目录，用 `test/testdata/`（Go 会自动忽略该名字，避免被当包扫描）。`E2E_WORKDIR` 指向的运行目录**不要用 `/tmp`** —— tmpfs 打不开 `O_DIRECT`。详见 [testing/](../testing/index.md) 的 gbp-045。

### `/examples` — 适用

**规则**：应用与公开库的示例。

**对 taihu：适用。** `examples/taihu-client/`。**它是 `pkg/taihu-client` 的对外用法的活文档**——改 `pkg/` 的公开 API 时，`examples/` 是最先该跟着改的地方，改不动就说明这次 API 变更破坏了对外兼容性。

### `/third_party` — 适用

**规则**：外部辅助工具、fork 的代码与其它第三方工具。

**对 taihu：适用。** `third_party/shmipc-go/` 是 fork。

**两条本仓库特有的约束**（都已在 Makefile 与 spec 里落实）：

1. **`check-fmt` 排除 `third_party/`** —— 保持与上游一致的格式，不纳入本地 gofmt 校验。
2. **`testify` 只在这个 fork 里用**，自有代码零引用（见 [testing/](../testing/index.md) 的 gbp-046）。

### `/docs` vs 本仓库的 `doc/` — **既有例外，不改**

**规则**：`/docs` 放设计与用户文档（godoc 之外的）。

**对 taihu：偏离——本仓库用单数 `doc/`。**

```
doc/{README.md, 设计文档/, 性能测试报告/, 部署记录/}
```

**裁决：保留 `doc/`，不改为 `docs/`。** 理由是这不是空白目录而是**已有 4 个子目录的既成事实**，改名要动大量交叉引用的路径（README、设计文档之间、`.trae/` 等处），**收益是把一个目录名从单数变复数**。

**新增文档时的规则**：**放进 `doc/` 下，不要新建 `docs/`**。出现了两个相近目录比单数/复数的差异更糟。

**`doc/` 的分工**（与 spec 的区别，不要混）：

| 目录 | 内容 |
|---|---|
| `doc/设计文档/` | **当时为什么这么定**——决策记录 |
| `doc/性能测试报告/` | 性能数据与结论 |
| `doc/部署记录/` | 部署过程记录 |
| `.trellis/spec/` | **改动时必须遵守的规则** |

**spec ≠ 设计文档**：spec 是规则，设计文档是理由。两者不合并、不互相搬运。

---

## 11 个缺失目录：逐条不适用理由

**缺失不等于违规**——project-layout 是「按需取用」的目录集合，不是必填清单。下列每一条都给出本仓库不适用的**具体原因**，避免以后有人看到「缺了 11 个」就以为要补齐：

| 目录 | 不适用原因 |
|---|---|
| `/api` | 规则是「OpenAPI/Swagger 规格、JSON schema、协议定义文件」。本仓库的协议定义**在 Go 代码里**（`internal/transport/protocol/protocol.go` 的 `ErrCode` 与二进制帧格式），不存在独立的规格文件 |
| `/web` | 无 Web 前端、无模板、无 SPA |
| `/init` | 无 systemd / upstart / sysv / supervisord 配置入库——本仓库的部署配置**刻意不入库**（见 `/deployments` 行） |
| `/build` | 规则管的是「打包与 CI」：容器配置放 `/build/package`、CI 配置放 `/build/ci`。本仓库**没有容器化配置、没有 CI 配置** |
| `/deployments` | 无 docker-compose / k8s / helm / terraform。本仓库的部署走 `doc/部署记录/`（**过程记录**，不是可执行的编排配置） |
| `/tools` | 定义明确要求是**能 import 本项目 `/pkg`、`/internal` 的 Go 工具**。本仓库无此类工具；Python 运维脚本归 `/scripts` |
| `/githooks` | 无 git hooks |
| `/assets` | 无图片、logo 等静态资产 |
| `/website` | 无独立站点（文档就在仓库里，用 GitHub 渲染） |
| `/vendor` | **刻意的**：规则原文即「若 module proxy 满足你的需求，你根本不需要 `vendor` 目录」。本仓库用 module proxy，`go.sum` 保证可复现性 |
| — | （第 11 个是上一节已单独裁决的 `/src`——那是**禁令**，不是缺的目录） |

**这 11 条里，`/vendor` 与 `/build` 是唯一两条将来最可能被翻出来的**：若哪天引入容器化发布，`/build/package` 与 `/deployments` 就是 project-layout 指定的位置——**届时应按规则建目录，而不是新发明一个名字。**

---

## 一句话规则

**新增任何顶层目录之前，先在这张表里找 project-layout 的对应项：**

- **找到** → 用它的名字与语义（不是自创一个近义名）。
- **没找到** → 才自创；并在提交信息里写明为什么 project-layout 没有对应项。
- **默认不新建**：本仓库现有 9 个顶层目录已经覆盖了全部需要。**顶层多一个名字，阅读成本就多一分。**

**唯一硬禁令：项目内不得出现 `/src`。**
