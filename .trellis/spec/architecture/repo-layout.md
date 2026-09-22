# 仓库布局

> 本文件管**顶层目录的清单与归属** —— 哪个目录放什么、新增一个目录时该往哪放。
>
> 与 [layering.md](./layering.md) 的分工：那边管 `internal/` 与 `pkg/` 之间的**依赖方向**（谁不许 import 谁，有 `make` 门禁强制）；这边管**目录本身**。两者不重叠，也不互相复述。

**准入**：每条都要能指到真实路径（本文件的目录与文件均已 `ls` / `sed -n` 核对）。指不到的不写。

---

## 规则 1：顶层目录对照 golang-standards/project-layout

上游取自 [golang-standards/project-layout](https://github.com/golang-standards/project-layout)。逐项核对后：**采用 9 项、不适用 10 项、1 项既有例外**。

**先说清这份布局的分量。** 它的 README 自己写着：

> "NOT an official standard defined by the core Go dev team"
> "This is a set of common historical and emerging project layout patterns in the Go ecosystem."

所以本条的形态是**参考并逐项说明采用或偏离**，不是「Go 官方要求这么放」。它真正有价值的是**命名共识** —— `cmd/` / `internal/` / `pkg/` 这几个名字在 Go 生态里有稳定含义，读者看到就知道去哪儿找什么。这也是本仓库采用它的理由：**不是因为它权威，是因为它被广泛认得。**

### 采用（9 项）

| 目录 | 上游定义 | taihu 现状 |
|---|---|---|
| `cmd/` | 主应用；**每个应用的目录名应与可执行文件名一致** | `cmd/taihu/`，与 `Makefile:1` 的 `BINARY  := taihu` 一致。`main.go` 只有 `run(args) int` 与退出码映射，业务逻辑在子包 `cmd/taihu/cmd/` |
| `internal/` | 私有应用与库代码（编译器强制） | 12 个包：`aio` `benchkit` `bufpool` `cluster` `device` `ierr` `layout` `metastore` `rpcclient` `storage` `transport` `version` |
| `pkg/` | 可被外部应用引用的库代码 | 只有 `pkg/taihu-client` —— 客户端 SDK，外部调用方的唯一入口。**依赖方向见 [layering.md](./layering.md)，本条不重复** |
| `test/` | 额外的外部测试应用与测试数据 | `test/e2e/`，按字母分组的端到端套件（形态见 [testing/index.md](../testing/index.md)） |
| `third_party/` | fork 的外部代码与第三方工具 | `netpoll` / `shmipc-go` 两个 fork。`Makefile:24` 写明「保持与上游一致的格式，不纳入本地 gofmt 校验」，对应 `check-fmt` 里的 `grep -v '^third_party/'` |
| `configs/` | 配置模板与默认配置 | `configs/taihu-server.example.sh` |
| `examples/` | 应用与公开库的示例 | `examples/taihu-client/` |
| `scripts/` | 构建、安装、分析等操作脚本 | `scripts/proxy.py`（NEFS tools 统一脚本，单文件单命令的 HTTP server）与 `scripts/proxy_client.py`（其 agent 客户端，封装 exec / 文件传输 / proxy 管理） |
| 根级 `Makefile` | — | 门禁汇总入口：`make check` / `make check-linux`（见 [layering.md](./layering.md) 规则 3） |

### 不适用（10 项）

一行一条，每条说明为什么不适用 —— 都基于本仓库的实际形态，不是「暂时没有」。

| 目录 | 为什么不适用 |
|---|---|
| `api/` | 无 OpenAPI / Swagger / JSON schema。对外协议是自研二进制帧（`internal/transport/protocol/`），不是 HTTP API |
| `web/` | 无 Web 前端、无服务端模板、无 SPA |
| `build/` | 无容器打包与 CI 配置。构建就是 `make build`，产物进 `bin/` |
| `deployments/` | 无 k8s / helm / terraform / compose。部署形态记在 `doc/部署记录/` |
| `init/` | 无 systemd / supervisord 配置，未提供 `make install` |
| `tools/` | 无随仓库分发的工具程序。辅助脚本一律在 `scripts/` |
| `githooks/` | 未使用 git hooks。门禁靠 `make check` 手动跑，不挂在 commit 上 |
| `assets/` | 无图片 / logo 类仓库资产 |
| `website/` | 无项目站点 |
| `vendor/` | 依赖走 go modules，不 vendor。上游也说这条是可选的 |

### 不该有

`src/` —— 上游明确列为**不应存在**（它与 Go workspace 的 `$GOPATH/src` 容易混淆）。本仓库没有。

### ⚠️ 既有例外：`doc/` 而非 `docs/`

**上游主张**：设计文档与用户文档放 `/docs`（上游目录树里没有 `/doc`）。

**taihu 的做法**：用 `doc/`（单数）—— `doc/设计文档/`、`doc/性能测试报告/`、`doc/部署记录/` 与 `doc/README.md`。

**为什么偏离**：

1. **引用面广。** `README.md`、`doc/README.md` 与既有 spec 都按 `doc/` 指路；改名要同步改一批文档，收益却只是让目录名多一个字母。
2. **已有范围决定。** 上一轮 spec 补全任务的范围里明确写了「`doc/设计文档/`、`doc/性能测试报告/`、`.trae/documents/` 原样保留」—— 改名与那条决定冲突。

**新增文档仍放 `doc/`。** 不要因为「规范说 `docs`」而新起一个 `docs/` 目录 —— 两个名字并存比名字不合规范更糟，读者会不知道该看哪个。

---

## 规则 2：新增顶层目录前，先判断它属于哪一类

上游那份布局是**通用清单**，不是本仓库的待办列表。加目录前按下面的顺序过一遍，避免长出一堆只放一个文件的顶层目录：

| 问 | 若「是」 |
|---|---|
| 它是**可执行程序**吗？ | 放 `cmd/<可执行名>/`，目录名必须与产物名一致 |
| 它**只给本仓库用**吗？ | 放 `internal/`。这是默认去向 —— 拿不准时选它 |
| 外部调用方**必须**能 import 它吗？ | 才考虑 `pkg/`。**这是唯一进 `pkg/` 的理由**，不要因为「可能有人用」就放进去 |
| 它是**测试数据或外部测试程序**吗？ | 放 `test/` |
| 它是 **fork 的上游代码**吗？ | 放 `third_party/`，并保持与上游一致的格式 |
| 它是**运维/构建脚本**吗？ | 放 `scripts/` |
| 都不是，且是**文档**？ | 放 `doc/`（见规则 1 的既有例外） |

**仍然要新建顶层目录时**，先确认上表 10 个「不适用」项里没有能装下它的 —— 那些目录不是不能有，是**在本仓库当前形态下装不下东西**；真有了对应的东西（比如引入了容器化部署），`deployments/` 就该启用，并回来把这一条从不适用改为采用。

**不要在 `internal/` 下另起分层包袱。** 上游允许 `internal/app/` 与 `internal/pkg/` 的细分，本仓库不采用 —— 现在 12 个包是平铺的，包名即职责，没有中间层。要改这个形态属于架构调整，不是「新增目录」。
