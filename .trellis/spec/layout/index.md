# layout —— 目录与文件布局

本层有两半，来源不同，**分开读**：

| 半 | 来源 | 内容 |
|---|---|---|
| **顶层目录**（本文档前半） | `golang-standards/project-layout` | 20 个目录的逐项裁决 |
| **包内文件**（本文档末尾） | **本仓库自定**（本 spec 唯一的自拟规则） | 一个包的对外面集中在一个文件 |

**顶层目录这半不是官方标准。** project-layout 的 README 自己声明它是**社区约定**、非 Go 官方规范，且 `pkg/` 这类模式「不被所有人接受」（`master`，核对于 `a9d6fae`）。所以用法是：**project-layout 提议、本仓库逐条裁决**，而不是照搬。这半不来自 gbp——它是本 spec 的第二个来源。

## 本仓库的实际顶层

```
cmd/  configs/  docs/  examples/  internal/  pkg/  scripts/  test/  third_party/
Makefile  README.md  LICENSE  go.mod  go.sum  AGENTS.md
```

对照 project-layout 的 19 个 Go/服务/通用目录，**命中 8 个、缺 11 个**：

| project-layout 目录 | 本仓库 | 裁决 |
|---|---|---|
| `/cmd` | ✅ `cmd/taihu/` | 适用，见下 |
| `/internal` | ✅ `internal/` | 适用，见下 |
| `/pkg` | ✅ `pkg/ierr/` + `pkg/taihu-client/` | 适用（语义收窄），见下 |
| `/configs` | ✅ `configs/` | 适用，见下 |
| `/scripts` | ✅ `scripts/` | 适用，见下 |
| `/test` | ✅ `test/e2e/` | 适用，见下 |
| `/examples` | ✅ `examples/` | 适用 |
| `/third_party` | ✅ `third_party/shmipc-go/` | 适用 |
| `/docs` | ✅ `docs/` | 适用（统一文档目录），见下 |
| `/web` | ⚠️ 前端在 `internal/web/static/` | **不设顶层 `/web`**，见下 |
| `/api` | ❌ 无 | 不适用（无 OpenAPI/JSON schema/协议定义文件） |
| `/init` | ❌ 无 | 不适用（无 systemd/supervisord 配置入库） |
| `/build` | ❌ 无 | 不适用（无容器/发行包配置、无 CI 配置） |
| `/deployments` | ❌ 无 | 不适用（部署配置不入库，见下） |
| `/tools` | ❌ 无 | 不适用（无 Go 支持工具；Python 运维脚本归 `/scripts`，见下） |
| `/githooks` | ❌ 无 | 不适用（无 git hooks） |
| `/assets` | ❌ 无 | 不适用（无图片/logo 等静态资产；唯一的前端资产内嵌于 `internal/web/static/`） |
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

**⚠️ 一处需要说明的张力：`cmd/taihu/cmd/` 有 3036 行非测试代码（2026-10-02 数；含 `web.go` 管理面装配）。**

严格按「不要把大量代码放进应用目录」读，这 3036 行是超标的。**本 spec 的裁决是保留现状**，理由：

1. `/cmd` 那条规则的**判据**是它自己给的——「代码能否被别的项目 import？能 → `/pkg`；不能且不想被别人复用 → `/internal`」。`cmd/taihu/cmd/` 里的内容是 **CLI 命令定义**（`key.go` / `cluster.go` / `bench_*.go` 等），**没有外部调用方**，属于「不想被复用」。
2. 按第 1 条的判据，它确实**可以**移到 `/internal`。但它是 **CLI 的表示层**——`root.go` 定义 cobra 命令树、`server.go` 定义启动参数、`helpers.go` 定义输出格式。放进 `internal/` 会让「引擎内部机制」与「命令行界面」混在同一个目录下，反而削弱 `internal/` 按机制分层的清晰度（见 [ddd/](../ddd/index.md) 对分层的那段）。
3. 迁移是一次**无行为收益的大范围改名**（含 5 个测试文件、`kvConnect` / `newTiKVKV` 两个测试缝隙的引用面）。

**所以：新增 CLI 子命令时按现状放 `cmd/taihu/cmd/`。** 但如果某段逻辑**能被 `internal/` 或 `pkg/` 复用**（例如一个可复用的格式化函数），按规则它应当**下沉**，不要留在 `cmd/` 下。

### `/internal` — 适用

**规则**：私有代码，Go 编译器强制不可外部导入；可按需加 `internal/app`（应用）与 `internal/pkg`（共享私有库）的细分，**但这是可选的、不是必须的**。

**对 taihu：适用。** `internal/{aio,benchkit,bufpool,cluster,device,layout,metastore,rpcclient,storage,transport,version,web}` 扁平一层，**不采用 `internal/app` + `internal/pkg` 的细分**——project-layout 原文写明「It's not required (especially for smaller projects)」，本仓库有 Makefile 门禁保证依赖方向（见 [ddd/](../ddd/index.md)），细分不带来额外保证。

**`internal/` 的语义在本仓库比 project-layout 说的更强**：它不只是「不想被外部导入」，还是 **Makefile 两条门禁的管辖范围**——`internal/` 不得依赖 `pkg/`（唯一例外 `pkg/ierr`，理由见下节）。**新增包时要知道自己进了这个管辖范围。**

### `/pkg` — 适用，但本仓库的语义比规则更窄

**规则**：可被外部应用使用的库代码；**放之前要想两次**；`internal` 因为编译期强制，是更好的私有化手段；`pkg` 只是**显式声明「这里的东西外部可以安全使用」**的方式。

**对 taihu：适用，且本仓库把范围收得更窄。**

`pkg/` 下**只有两个包**，分两类：

| 包 | 类别 | 为什么在 pkg/ |
|---|---|---|
| `pkg/taihu-client/`（包名 `taihuclient`） | 对外 SDK | 外部调用方唯一入口 |
| `pkg/ierr/` | 引擎与 SDK **共享的错误事实源** | SDK 库代码直接 import（`pkg/taihu-client/storage.go:14`），外部调用方用同一批 sentinel `errors.Is`；它是 `check-layering` 允许 `internal/ → pkg/` 的**唯一例外** |

**本仓库对 `/pkg` 的约束严于 project-layout**：project-layout 只说「外部可用的库」，本仓库另有 `check-sdk-only` 门禁——`pkg/` 不得直接依赖存储引擎（`internal/{storage,device,aio,bufpool,layout}`，测试除外），SDK 库代码只应依赖 `internal/{cluster,rpcclient,version}` 与 `pkg/ierr`。

**这条约束的方向是不可逆的**：一旦 `pkg/` 依赖了引擎内部，SDK 就被绑死在服务端实现上，外部调用方也就继承了那层依赖。

**一个来回过的先例（新增 pkg 包前必读）**：`bufpool` 2026-09-24 曾以「SDK 可复用同一对齐缓冲」为由从 `internal/bufpool` 迁至 `pkg/bufpool`，但该复用**始终没有发生**——12 个引用方全是引擎内部与 CLI，SDK 零引用。2026-10-02 迁回 `internal/bufpool`，门禁恢复单例外（`pkg/ierr`）。**「外部将来可能用」不足以进 `pkg/`；要有真实的外部 import。**

**所以新增 `pkg/` 下的包之前要问**：它是不是**已有外部调用方**（含 SDK 库代码直接 import）的？不是 → 放 `internal/`。判据与门禁一致，不是风格偏好。

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
2. **`testify` 只在这个 fork 里用**，自有代码零引用（2026-10-02：全仓 13 个引用文件全在 `third_party/`，见 [testing/](../testing/index.md) 的 gbp-046）。

### `/web` 不设顶层 — 前端是服务端包的内嵌资产

**规则**：`/web` 放专门面向 Web 的应用（SPA、模板、静态前端工程）。

**对 taihu：不设顶层 `/web`，前端资产放在 `internal/web/static/`。** 这是 2026-10-02 新有的裁决（commit `6ae80b5` 引入管理 Web 前，本仓库无任何前端）。

判据是**前端是不是独立工程**：

- project-layout 的 `/web` 设想的是独立构建/独立部署的前端（node 工程、打包产物、自己的依赖清单），顶层目录是它的工程根；
- 本仓库的前端只有三个手写静态文件（`internal/web/static/{index.html,app.js,style.css}`），无构建步骤、无 npm 依赖，经 `//go:embed` 编进服务端二进制（`internal/web/web.go:22-23`），与 HTTP handler **同包同生命周期**演进——路由改了端点，前端同一天改，放在一起才能一起被 review。

**若哪天前端长成独立工程**（引入框架/构建链/独立发版），再按 project-layout 建顶层 `/web`，并让 embed 指向构建产物。在此之前不要新建 `/web`。

### `/docs` — **统一文档目录**

**规则**：`/docs` 放设计与用户文档（godoc 之外的）。

**对 taihu：适用——全部文档统一收敛到 `docs/`。**

```
docs/{README.md, 设计文档/, 性能测试报告/, 部署记录/, e2e/, sdk/}
```

**历史**：原 `doc/`（设计文档/性能测试报告/部署记录 + 索引）已于 2026-10-02 合并进 `docs/`，与既有的 `docs/e2e/`、`docs/sdk/` 统一为单一文档入口。

**新增文档时的规则**：**放进 `docs/` 下对应子目录**，不要新建 `doc/` 或其它文档目录。

**`docs/` 的分工**（与 spec 的区别，不要混）：

| 目录 | 内容 |
|---|---|
| `docs/设计文档/` | **当时为什么这么定**——决策记录 |
| `docs/性能测试报告/` | 性能数据与结论 |
| `docs/部署记录/` | 部署过程记录 |
| `.trellis/spec/` | **改动时必须遵守的规则** |

**spec ≠ 设计文档**：spec 是规则，设计文档是理由。两者不合并、不互相搬运。

---

## 11 个缺失目录：逐条不适用理由

**缺失不等于违规**——project-layout 是「按需取用」的目录集合，不是必填清单。下列每一条都给出本仓库不适用的**具体原因**，避免以后有人看到「缺了 11 个」就以为要补齐：

| 目录 | 不适用原因 |
|---|---|
| `/api` | 规则是「OpenAPI/Swagger 规格、JSON schema、协议定义文件」。本仓库的协议定义**在 Go 代码里**（`internal/transport/protocol/protocol.go` 的 `ErrCode` 与二进制帧格式），不存在独立的规格文件 |
| `/web` | 前端存在但**内嵌在服务端包**（`internal/web/static/` 三个手写静态文件 + `go:embed`），不是独立前端工程——裁决见上文专节 |
| `/assets` | 无图片、logo 等静态资产；唯一前端资产已随 `internal/web/static/` 内嵌 |
| `/init` | 无 systemd / upstart / sysv / supervisord 配置入库——本仓库的部署配置**刻意不入库**（见 `/deployments` 行） |
| `/build` | 规则管的是「打包与 CI」：容器配置放 `/build/package`、CI 配置放 `/build/ci`。本仓库**没有容器化配置、没有 CI 配置** |
| `/deployments` | 无 docker-compose / k8s / helm / terraform。本仓库的部署走 `docs/部署记录/`（**过程记录**，不是可执行的编排配置） |
| `/tools` | 定义明确要求是**能 import 本项目 `/pkg`、`/internal` 的 Go 工具**。本仓库无此类工具；Python 运维脚本归 `/scripts` |
| `/githooks` | 无 git hooks |
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

---

# 包内布局：一个包的对外面集中在一个文件

> **来源：本仓库自定** —— 这是本 spec 里唯一的自拟规则（收编门槛见[根 index](../index.md) 的「三个来源」）。
> project-layout 只覆盖顶层目录，gbp 的九个类别里没有「包内文件组织」这一类，**两个上游都不管这件事**。
> 它管的是「文件摆在哪」，与上面的顶层目录同题，所以放在本层。
>
> **规则身世的变迁（2026-10-02 订正，务必知道）**：本规则 2026-09 从 `internal/aio` 的
> `aio_internal.go` 注释升格而来。该文件此后在 commit `5dc2294`（aio 文件布局收敛）中被
> 删除——aio 刻意改成「单入口文件 + 文末『包内私有』节」，即**规则的起源包自己选择了
> 单一性、放弃了纯粹性**（其包注释 `internal/aio/aio.go:23-27` 明确记录这一选择）。
> 规则保留：仍有 4 个包严格符合（见现状表），且对**新增包**有效；但不要再把 aio 当正例引用。

## 规则

**一个包应当有一个「对外面文件」**，同时满足两条**独立**判据：

| 判据 | 内容 |
|---|---|
| **① 单一性** | 该包**全部包级导出**（`func` / `type` / `var` / `const`）都定义在**同一个**文件里 |
| **② 纯粹性** | 那个文件里**不得有任何未导出的顶层声明**——未导出的类型 / 常量 / 变量 / 函数一律在别的文件 |

**文件命名：与包同名。** 2026-10-02 实测：包级导出只落在一个文件的 10 个包里，9 个用 `<包名>.go`——`aio.go` / `bufpool.go` / `device.go` / `ierr.go` / `layout.go` / `metastore.go` / `protocol.go` / `version.go` / `web.go`。唯一例外是 `cmd/taihu/cmd`（包名 `cmd`，主文件叫 `root.go`，那是 cobra 的惯例）。**新增包时用 `<包名>.go`，不要另起 `api.go` / `public.go` / `export.go`。**

## 为什么值得

1. **读一个包只要读一个文件**就知道它的全部对外契约，不必在实现细节里挑出可导出的部分。
2. **「这个符号能不能改」变成一次查找**：在对外面文件里 → 破坏性变更；不在 → 内部实现。Go 没有工具能回答「有没有包外调用方」，靠读代码猜不可靠，靠一个固定的文件位置才可靠。
3. **纯粹性让规则可机械核查**：对外面文件里出现小写开头的顶层声明就是错——**不需要判断「这个未导出符号重不重要」**。少了这条判据，「哪些未导出符号该挪走」每次都要吵一遍。

## 三条例外（不算违规）

### 例外一 · 实现导出接口的方法（硬性，无法避免）

Go 不允许实现导出接口的方法私有。`internal/aio` 的 `Ring` 接口（`internal/aio/aio.go:67` 起）声明了 `SubmitRead` / `SubmitWrite` / `Wait` / `Close`，所以三个后端类型上的同名方法**必须**导出：

| 文件 | 导出方法（receiver 方法） | **包级导出** |
|---|---|---|
| `internal/aio/ring_libaio_linux.go` | 6 个 | **0** |
| `internal/aio/ring_fallback_other.go` | 6 个 | **0** |
| `internal/aio/ring_uring_linux.go` | 7 个（含 `uringParamError.Error`，类型定义在 `:230`） | **0** |

**关键在最后一列**：这三个文件**一个包级导出都没有**——它们是「未导出类型 + 导出方法」。判据是**包级导出**，方法不计；按「文件里有几个大写开头的名字」数会误判成「导出面分散在 3 个文件」。

### 例外二 · `_` 声明

`var _ ByteReader = netpoll.Reader(nil)` 这类**没有名字**的顶层声明不计入「未导出声明」：它必须**紧挨着被断言的接口**才有意义，为了「纯粹性」把它挪走是更糟的选择。实例：`internal/transport/protocol/protocol.go:191-192` 紧跟在同一文件 `:184` 的 `ByteReader` 之后。（`_` 的三种合法用法见 [idiomatic/](../idiomatic/index.md) 的 gbp-039。）

### 例外三 · `reexport.go` 与零逻辑转调

reexport 文件**逐层转指上游类型与错误**，当前两个，全部 0 个未导出顶层声明，符合纯粹性：

- `internal/rpcclient/reexport.go`（16 个导出）；
- `pkg/taihu-client/reexport.go`（10 个导出）。

**允许的内容**：type alias、`= upstream.Err` 形式的变量 / 常量，以及**零逻辑转调构造函数**——函数体只有一条 `return upstream.Xxx(args...)`，参数原样透传。正例两处：

- `internal/rpcclient/reexport.go:72` 的 `NewFdBuf`（`FdBuf.release` 未导出，包外无法用字面量构造，只能提供转调入口）。

**禁止的内容**：分支、转换、聚合等任何真实逻辑。一旦 reexport 文件里出现「第二步动作」，它就从「投影」变成了「第二个契约来源」，那时应当拆成一个正经的包。

## 当前状态（2026-10-02 实测，AST 判定）

| 状态 | 包（E = 包级导出数，U = 未导出顶层声明数） |
|---|---|
| ✅ **两条都符合** | `internal/layout`(5E)、`internal/metastore`(14E)、`internal/transport/protocol`(65E)、`pkg/ierr`(26E) |
| ⚠️ **单一但不纯** | `internal/aio`(11E/8U，**起源包刻意偏离**，包注释有「包内私有」节)、`internal/device`(9E/12U)、`internal/bufpool`(4E/11U)、`internal/version`(3E/1U)、`internal/web`(21E/5U，**见下：新包欠债**)、`cmd/taihu/cmd`(1E/6U) |
| ⚠️ **按主题拆成多个纯导出文件** | `internal/cluster`(7 文件：6 纯 + `kv_tikv.go` 3E/7U)、`pkg/taihu-client`(5 文件) |
| ❌ **导出散在实现文件里** | `internal/transport`(8)、`internal/rpcclient`(7)、`internal/benchkit`(2)、`internal/storage`(2：主文件 `storage.go` 11E 已纯，仅 `read.go` 漏 1E/1U) |

**这条规则不追溯既有代码。** 大范围移动换文件摆放不划算。它对**新增代码**生效：

- **新增导出名** → 放进该包**已有**的对外面文件；**不要新开**一个只放导出名的文件。
- **新增未导出名** → **不要**放进对外面文件，哪怕只有 3 行。
- **新增一个包** → 一开始就按本规则建：`<包名>.go` 只放导出名。

**`internal/web` 是一笔明确的新包欠债**（2026-10-02 记录）：它是规则成文后第一个新增的包，但没有起步即合规——`web.go` 混着 5 个未导出声明（`staticFS` 嵌裁变量 + `writeJSON` / `writeErr` / `errStringf` / `apiStatus`，见 `internal/web/web.go:23`、`:254-282`）。收敛方向无需决策、仅欠移动：四个输出/错误辅助挪进 `handlers.go`，`staticFS` 挪进一个小文件（如 `static.go`）。**下次改 `internal/web` 时顺手完成，不要在 `web.go` 里继续加未导出声明。**

**各类偏差的处置方向不同，不要一律返工：**

- **`internal/aio`：刻意的单入口，不要替它「纠正」。** 包注释已声明私有节的位置，这是与本规则平行的另一种成文选择。新增导出/私有名都进 `aio.go` 时跟随它文末的「包内私有」分节。
- **`internal/cluster`：有意设计，跟着它自己的分法走。** 6 个纯导出文件按主题拆（`client.go` 8E / `kv.go` 6E / `capacity.go` 5E / `register.go` 5E / `instance.go` 4E / `kv_mem.go` 2E），实现文件 `kv_tikv.go`(3E/7U) 是唯一混合点。新增导出名**跟着主题走**，不要混进实现文件。
- **`internal/storage`：差一步走完。** 主文件 `storage.go`(11E) 已纯，只有 `read.go` 漏了 1 个导出。下次碰 `read.go` 时把那个导出名挪进 `storage.go` 即可。
- **`internal/device` / `internal/bufpool` / `internal/version` / `cmd/taihu/cmd`：差的只是纯粹性。** 导出已集中在一个文件，新增未导出名时**不要往那个文件里加**。

## 核查脚本（正则在这里不可靠，必须用 AST）

**`grep` 数不出这件事。** 块形式的声明里，名字前面**没有关键字**：`pkg/ierr/ierr.go:9-64` 的 26 个 sentinel 全在 `var ( ... )` 块里，换行后是 `\tErrNotFound = errors.New(...)`——任何 `^var [A-Z]` 模式的 grep 都**一条也匹配不到**，会得出「该包 0 个导出」的错误结论。**这个坑必须记住**：同类命令在别的层（如 [performance/](../performance/index.md)）能凑合用，是因为那些规则数的东西不藏在块里。

正确的数法用 `go/ast`。存成 `/tmp/apidump/main.go`：

```go
// apidump 列出每个包里「承载包级导出」的文件，并标出该包对外面文件的纯粹性。
// 接口方法（receiver 非空）不计：Go 语言强制它们导出。`_` 声明也不计。
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	for _, dir := range os.Args[1:] {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		fset := token.NewFileSet()
		var carriers []string
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			af, err := parser.ParseFile(fset, f, nil, 0)
			if err != nil {
				continue
			}
			ne, nu := 0, 0
			for _, d := range af.Decls {
				switch dd := d.(type) {
				case *ast.FuncDecl:
					if dd.Recv != nil {
						continue
					}
					if ast.IsExported(dd.Name.Name) { ne++ } else { nu++ }
				case *ast.GenDecl:
					if dd.Tok == token.IMPORT {
						continue
					}
					for _, s := range dd.Specs {
						var names []*ast.Ident
						switch sp := s.(type) {
						case *ast.TypeSpec:
							names = append(names, sp.Name)
						case *ast.ValueSpec:
							names = append(names, sp.Names...)
						}
						for _, n := range names {
							if n.Name == "_" {
								continue
							}
							if ast.IsExported(n.Name) { ne++ } else { nu++ }
						}
					}
				}
			}
			if ne > 0 {
				tag := fmt.Sprintf("%s(%dE", filepath.Base(f), ne)
				if nu > 0 {
					tag += fmt.Sprintf("/%dU", nu)
				}
				carriers = append(carriers, tag+")")
			}
		}
		if len(carriers) > 0 {
			fmt.Printf("%-28s %d 个文件  %s\n", dir, len(carriers), strings.Join(carriers, " "))
		}
	}
}
```

跑法（`internal/*` 不会递归到 `internal/transport/protocol`，必须用 `find` 展开）：

```bash
find internal pkg cmd -name '*.go' ! -name '*_test.go' | xargs -n1 dirname | sort -u \
  | command grep -v third_party > /tmp/dirs.txt
xargs go run /tmp/apidump/main.go < /tmp/dirs.txt
```
