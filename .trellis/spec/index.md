# taihu 工程规范（spec）

本目录是 taihu 的**改动时规则库**：写代码前读，写完对照检查。

## 三个来源

本 spec 的**每一条**规则都来自下面三个来源之一，并在所在层标明是哪一类。

| 来源 | 版本 | 内容 | 本机位置 |
|---|---|---|---|
| [`cexll/golang-base-practices-skills`](https://github.com/cexll/golang-base-practices-skills) | `26426d2`（2026-01-23） | **53 条** Go 规则，分 9 类，每类一个层 | `/tmp/gbp`（若已清理：`git clone https://github.com/cexll/golang-base-practices-skills`） |
| [`golang-standards/project-layout`](https://github.com/golang-standards/project-layout) | `master` | **20 个**顶层目录约定 + 1 个「不该有」 | `/tmp/pl` |
| **本仓库自定** | — | 当前 **1 条**：包内布局（对外面集中在一个文件），见 [layout/](./layout/index.md) | 无上游 |

**上游规则与本仓库自定规则的边界**：读代码得出的结论**默认只用来回答「这条上游规则在 taihu 适用吗」**——那是在**裁决**，不是**立法**。

**允许自拟的判据（三条全中才收）**：

1. **两个上游都不覆盖这个话题**。上游写了而本仓库不认同的，走「裁决 + 标注偏离」，不算自拟；
2. **本仓库已经有稳定做法**，规则能从既有代码里抽象出来，而不是「我觉得应该这样」；
3. **规则能指名具体文件**，并说清与既有做法的差距在哪。

门槛写在这里，是为了防止这一节以后变成「把个人偏好写进规范」的口子。

**project-layout 自称非官方标准**，原文（README 第 29 行）：

> This is **`NOT an official standard defined by the core Go dev team`**. This is a set of common historical and emerging project layout patterns in the Go ecosystem.

所以「参考 project-layout」里的"参考"就是字面意思：它是生态惯例的集合，不是权威裁决。本仓库与它冲突时不必然是本仓库错——见 [layout/index.md](./layout/index.md) 的逐项目裁决。

## 十个层

前九层一一对应 gbp 的九个类别，第十层对应 project-layout（并承载目前唯一的自拟规则）。**53 条上游规则一条不漏地有归属**，20 个目录一条不漏地有裁决。

| 层 | 对应 gbp 类别 | 条数 | 对 taihu 的适用度 |
|---|---|---|---|
| [framework/](./framework/index.md) | Framework Selection | 4 | **4 条全不适用**（无 HTTP 服务端） |
| [database/](./database/index.md) | Database & ORM | 5 | **5 条全不适用**（本仓库是存储引擎本身） |
| [ddd/](./ddd/index.md) | DDD Project Structure | 6 | **6 条全不适用**（分层与门禁冲突） |
| [error/](./error/index.md) | Error Handling | 6 | 5 条适用 / 1 条部分 |
| [concurrency/](./concurrency/index.md) | Concurrency Patterns | 7 | 6 条适用 / 1 条偏离 |
| [idiomatic/](./idiomatic/index.md) | Idiomatic Go | 11 | 11 条全部适用 |
| [testing/](./testing/index.md) | Testing Practices | 7 | 4 条适用 / 3 条偏离 |
| [performance/](./performance/index.md) | Performance Optimization | 2 | 2 条全部适用 |
| [lint/](./lint/index.md) | Lint & Toolchain | 5 | **2 条已是门禁** / 3 条未引入 |
| [layout/](./layout/index.md) | （来源为 project-layout + **自定 1 条**） | 20 目录 + 1 条 | 9 采用 / 10 不适用 / 1 不该有 |

**前三个层整层不适用，为什么还要留着？** 因为它们记录的是**否决理由**，不是空模板。下一轮谁再提「要不要上 Gin」「要不要按 DDD 分层」，理由在这儿，不必重新论证一遍。删掉这三层等于把已经做过的工作丢掉。

## 适用性的判据

每条规则的适用性判断都基于**本仓库当前的代码事实**，不是印象。全文共用到这几个事实，先在这里给出可复现的核实命令：

| 事实 | 核实命令（仓库根执行） | 结果 |
|---|---|---|
| 无 gRPC | `grep -rl 'grpc' --include='*.go' internal cmd pkg examples` | 0 |
| 无 ORM / SQL | `grep -rlE 'gorm\|database/sql\|sqlx' --include='*.go' internal cmd pkg examples` | 0 |
| 无 Web 框架 | `grep -rlE 'gin-gonic\|go-kratos\|labstack/echo' --include='*.go' internal cmd pkg examples` | 0 |
| 无 wire | `grep -rl 'google/wire' --include='*.go' internal cmd pkg examples` | 0 |
| 无 goose | `grep -rl 'goose' --include='*.go' internal cmd pkg examples` | 0 |
| 无 errgroup | `grep -rl 'errgroup' --include='*.go' internal cmd pkg examples` | 0 |
| `net/http` 仅用于 pprof | `grep -rn 'net/http' --include='*.go' internal cmd pkg examples` | 1 处：`cmd/taihu/cmd/server.go:13-14` |
| testify 只在 fork 里 | `grep -rl 'stretchr/testify' --include='*.go' .` | 10 个文件，**全在 `third_party/shmipc-go/`** |
| 自有代码无 benchmark | `grep -rn 'func Benchmark' --include='*_test.go' internal cmd pkg` | 0 |

## 怎么用

1. **动手前**：找到你要改的代码属于哪一层，读那一层的 `index.md`。
2. **不确定某条规则适不适用**：每条规则都标了「对 taihu：适用 / 部分 / 偏离 / 不适用」，并给了理由或锚点。**理由写不出来的，按缺陷处理**——那是本 spec 没写清，不是你可以自行决定的。
3. **发现规则与本仓库实际不符**：不要就地迁就。要么改代码，要么在对应层里把「不适用」的理由补上（附可检验的锚点），然后才算数。

## 引用格式

规则里的 `file:line` 引用一律用**反引号包裹的仓库根相对路径**，例如 `internal/aio/aio.go:63-64`。不要用 Markdown 链接做跨层引用（链接改不动行号，反引号路径可以被脚本校验）。

**但脚本只验「存在与范围」，不验「内容」。** `.trellis/tasks/archive/2026-09/09-21-spec-upstream-alignment/research/verify_spec_refs.py` 查得出引用到不存在的文件、或行号超出文件长度，**查不出行号指错了行**——重排代码后 `internal/aio/aio_linux.go:49` 从 `mu` 变成了 `ctx`，脚本照样报「有效」，因为第 49 行确实存在。

**所以改完被引用的文件，必须逐条核对引用落在的内容**，不能只看脚本的绿字。这不是理论风险：2026-09-22 把两个 sentinel 挪进 `internal/ierr` 后（`internal/aio/aio.go` 少了 7 行），脚本报「有效 71 条、越界 0 条」，而实际有 **10 处**指向了错误的行。核对办法：把引用与它指向的那一行内容并排打印出来看（见 [layout/](./layout/index.md) 的核查脚本，同一套 AST 思路）。

**另外两条格式约束，直接决定引用能不能被校验：**

1. **一律写仓库根相对路径，不要只写文件名。** 校验脚本的 `is_ref` 要求 token 含 `/`（这是为了不把泛指的文件名当引用），所以 `aio_linux.go:49` 这种写法**会被整个跳过**——等于没有人校验它。在同一个表格里已经给过全路径、后续只写文件名「省地方」也不行，一样逃掉。
2. **行号只写 `:N` 或 `:N-M` 两种形式。** 脚本的 token 正则不认逗号形式：`:269,310,313,316` 连范围都匹配不上，同样会被跳过。多个行号就写多条引用。
