# CLI 层（命令行）

> `cmd/taihu` 与 `internal/benchkit` 的实现约定：cobra 子命令树、包级测试缝隙、共享压测骨架，以及本层改动必须过的质量门槛。

---

## 本层覆盖范围

`cmd/taihu`（入口 `main.go` + 子命令包 `cmd/taihu/cmd`）与 `internal/benchkit`。

| 文件 | 用途 |
|---|---|
| [command-and-output.md](./command-and-output.md) | 退出码与错误出口、输出纯净性（压第三方日志）、`[stat]` 周期统计、`--json` 与人读输出、压测报告格式 |
| `cmd/taihu/main.go` | 进程入口：`run(args []string) int` 负责参数装配与退出码，`main` 只做 `os.Exit(run(os.Args[1:]))` |
| `cmd/taihu/cmd/root.go` | 根命令、全局 persistent flags、`Execute` 错误出口、pingcap 日志降噪 |
| `cmd/taihu/cmd/helpers.go` | 共享 helper：KV 连接缝隙、实例枚举/寻址、数据面拨号、`printJSON`/`humanBytes` |
| `cmd/taihu/cmd/bench_*.go` | 三个压测子命令：`storage`（裸盘直压）/ `cluster`（走 TiKV）/ `single`（直连 server） |
| `internal/benchkit` | 压测公共骨架：`Store` 数据面抽象、`Config`、区间切分、执行循环与汇总报告 |

---

## Pre-Development Checklist

- [ ] 新子命令挂到正确的父命令下，并在该文件 `init()` 里 `AddCommand`（`root.go:99-107`、`key.go:384`、`instance.go:118`）
- [ ] 全局参数只加在 `rootCmd.PersistentFlags()`；命令私有参数加在自己的 `Flags()`（`root.go:85-97`、`server.go:286-296`）
- [ ] 集群类命令（依赖 `--pd`）入口第一件事是 `requirePD()`（`helpers.go:19-24`、`cluster.go:36-38`）
- [ ] 参数校验放在 `RunE` 开头并 **返回 error**，不要中途 `os.Exit`（`bench_storage.go:204-221`、`bench_cluster.go:166-183`）
- [ ] 新增依赖外部环境的路径 → 按既有做法抽成包级变量留测试缝隙（`helpers.go:29-32`、`helpers.go:59-65`、`bench_storage.go:44-47`）
- [ ] 新输出先写 `--json` 分支，再写人类可读分支（`cluster.go:81-95`、`key.go:160-164`）
- [ ] 命令行里长参数一律双横线 `--x`（`bench_storage.go:63`），并同步更新 `configs/taihu-server.example.sh`（`:8-9`）
- [ ] 压测工具优先复用 `benchkit.Run`（`internal/benchkit/run.go:84-112`），不要另抄一份执行循环
- [ ] `make check` 与 `go build ./cmd/taihu` 干净（见下方 Quality Check）

---

## 命令树：实际存在的子命令

规则 1：**命令树以 `root.go` 的 `AddCommand` 清单为权威，新增顶层命令必须同时改这里和 `main.go` 的包注释。** 现有顶层命令共 7 个（`root.go:99-107`）：

| 顶层 | 定义处 | 子命令 | 子命令定义处 |
|---|---|---|---|
| `version` | `version.go:12-20` | — | — |
| `server` | `server.go:39` | — | — |
| `bench` | `bench.go:8` | `storage` / `cluster` / `single` | `bench.go:26` → `bench_storage.go:50`、`bench_cluster.go:23`、`bench_single.go:22` |
| `cluster` | `cluster.go:16` | `list` / `status` / `index` / `purge` | `cluster.go:301`（`list`/`status`/`index`）、`cluster.go:303`（`purge`） |
| `key` | `key.go:17` | `put` / `get` / `delete` / `stat` / `meta` / `list` | `key.go:384` |
| `instance` | `instance.go:15` | `segments` | `instance.go:118` |
| `client` | `client.go:16` | `list` / `info` | `client.go:218` |

`main.go:1-3` 的包注释逐项列出了同一份清单（server / bench / cluster / key / instance / client / version）——改命令树时两处一起改，别只改一处。

规则 2：**父命令只是分组容器，`RunE`/`Run` 留空**；`bench`、`cluster`、`key`、`instance`、`client` 都只声明 `Use/Short/Long/Example` 并在 `init()` 里 `AddCommand`，没有任何执行体（`bench.go:8-27`、`cluster.go:16-24`）。给分组命令加轻量动作用例 `version` 那种 `Run`（`version.go:17-19`）。

规则 3：**全局参数在 `root.go` 一处定义、经包级 `global` 结构体读取。** root.go:85-97 注册 `--pd/--tikv-ca/--tikv-cert/--tikv-key/--client-name/--timeout/--json/--io-uring/--io-uring-iopoll`，默认值在 `root.go:37`（`timeout: 5s`、`ioUring: "auto"`）。子命令通过 `global.pd`、`global.clientName` 等直接读（`bench_cluster.go:63-67`）；超时统一用 `ctxWithTimeout`（`root.go:111-113`），AIO 后端统一用 `aioOptions()`（`root.go:117-126`，取值非法在此报错而非静默取默认）。

---

## 测试缝隙：外部依赖一律抽成包级变量（本仓库招牌做法）

规则 4：**凡是"需要外部环境才能跑"的依赖，都抽成包级函数变量，生产路径赋真实实现，单测替换为内存/临时实现。** 现有三处，新增时照抄这个形状：

```go
// kvConnect 是 connectKV 的实际实现。作为测试缝隙抽成包级变量：单测里替换为
// cluster.NewMemoryKV()，使 CLI 命令可在无 TiKV/PD 的环境下端到端跑通；
// 生产路径恒为 connectKVReal，行为不变。
var kvConnect = connectKVReal
```

（`helpers.go:29-32`；调用侧包一层 `connectKV` 见 `helpers.go:26-27`。）

另两处：`newTiKVKV`（`helpers.go:59-65`，供 `server` / `bench cluster` 的**启动直连**路径用，返回值放宽为 `cluster.KV`）、`deviceCapacity`（`bench_storage.go:44-47`，裸盘容量查询，单测换成按普通文件大小）。

规则 5：**为什么必须留缝隙：CLI 命令的单测要求"无 TiKV/PD、无真实块设备、固定端口为零"也能端到端跑通 RunE。** 依据见 `cli_core_test.go:3-6`（测试包头的说明：把 `kvConnect` 换成 `cluster.NewMemoryKV()`，真实数据面用"真实 Storage + transport server + `127.0.0.1:0` 临时端口"提供）与 `bench_cli_test.go:23-26`（三个 bench 子命令：随机端口、临时目录、极小 size/count）。打桩写法见 `bench_cli_test.go:54-61`（KV 缝隙）与 `bench_cli_test.go:63-76`（容量缝隙）。

规则 6：**注意 `server` 与 `bench cluster` 是故意不走 `connectKV` 的**：它们直连 TiKV 是为了保留原始错误前缀、且不加 `ctx` 兜底 goroutine（`server.go:110-114`、`bench_cluster.go:80-85`）。改这两条路径时替换的是 `newTiKVKV`，不是 `kvConnect`。

---

## 参数写法与校验

规则 7：**长参数一律双横线，单横线会被 pflag 当作 shorthand 簇拒绝。** 部署模板头部写明这条并指定权威源：

```
# 注意：长参数一律用双横线（--listen 而非 -listen；pflag 会把单横线长参数当短参数簇拒绝）。
# 参数定义权威源：cmd/taihu/cmd/server.go、cmd/taihu/cmd/root.go。
```

（`configs/taihu-server.example.sh:8-9`。）三个压测命令的 `Long` 里也各自重复了同一条（`bench_storage.go:63`、`bench_cluster.go:40`、`bench_single.go:34`）。改 `server`/根命令的参数集时，同步改 `configs/taihu-server.example.sh` 里的变量清单（`:11-22`）。

规则 8：**每个有必填项的命令都有一个集中的 `validate()`，`RunE` 拿到 flag 后先调它再干活。** `bench_storage.go:204-221`（mode 必须 write/read，size/threads/count/db/dev 校验）、`bench_single.go:126-143`（transport 与 addr/shm 的配对，再委托 `Config.Validate`）、`bench_cluster.go:166-183`（transport 枚举 + `-client-name` / `-pd` 必填）。`RunE` 侧只在 `bench_storage.go:85-87`、`bench_single.go:57-59`、`bench_cluster.go:69-71` 调用一次；调完立刻 `return err`，不夹带打印。

规则 9：**超时必须可配，或者写明为什么是这个值 —— 不许静默写死。**（上游 `ecc-015` 的意图：`context` 传超时，而不是各处自己定）

优先级从高到低，新写任何带超时的路径时按这个顺序选：**① 暴露成参数 → ② 从父 ctx 派生 → ③ 写死但注释说明为什么是这个值**。三条都不满足的就是缺陷。

**本仓库的默认形态是 ① + ②**：

- 超时是**参数**，不是常量。全局开关在 `root.go:91`（`pf.DurationVar(&global.timeout, "timeout", 5*time.Second, ...)`），一处收敛在 `ctxWithTimeout`（`root.go:111-113`），子命令一律经它派生；子超时再从父 ctx 派生 —— `cluster.go:279`、`:295` 的 `context.WithTimeout(ctx, 3*time.Second)`。
- 库侧的 `5 * time.Second` 是 `if timeout <= 0` 的**兜底默认值**，正常路径由调用方传入 —— `internal/cluster/client.go:38-40`、`pkg/taihu-client/registry.go:58-60`。

**唯一一处反例**（记为已知例外）：`internal/transport/server.go:83`

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
```

`GracefulStop()` 三个问题叠加：方法**不接参数**（调用方无从控制）、用 `context.Background()`（与任何父 ctx 断开，`--timeout` 影响不到它）、**无注释**说明 5s 的来路。⇒ 违反 ①、②、③ 全部三条。

**但它不是纯粹的疏漏**：关停路径**确实需要一个兜底期限**，否则 `GracefulStop` 可能永久挂住 ——「有界」是刻意的，问题只在「这个界不可配且来路不明」。所以规则允许 ③ 作为退路，不允许「既不可配也不写」。**本轮只记录，不改代码**（该待办见任务 `09-21-spec-upstream-alignment`）。

---

## 格式化

规则 9：**本层所有 `.go` 文件必须 gofmt 干净；`third_party/` 是例外，改它也别顺手 reformat。** 门槛实现在 Makefile:24-28：

```make
# third_party/ 是上游 fork，保持与上游一致的格式，不纳入本地 gofmt 校验。
check-fmt:
	@bad=$$(gofmt -l . | grep -v '^third_party/' || true); \
```

`make check` 把 `check-fmt` 与 `check-layering`、`check-sdk-only`、`go vet ./...` 串在一起（`Makefile:21-22`）。

---

## Quality Check

```bash
# 1. 格式 + 分层不变量 + vet（本层改动的第一道门）
make check

# 2. CLI 必须能编
go build ./cmd/taihu

# 3. CLI 单测与压测骨架单测
go test ./cmd/... ./internal/benchkit/...

# 4. 平台专有代码（shm / io_uring / Syscall6）在 linux 上编得过
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./cmd/...
make check-linux
```

第 3 条在 macOS 本机有 **2 个已知平台性失败**（工作区干净时即如此，非改动引入）：

- `TestBenchStorageCmdErrorPaths`：`bench_cli_test.go:249` 断言"普通文件上 `BLKGETSIZE64` 必失败"，macOS 走非块设备回退路径故不报错。
- `TestBenchSingleCmdShmRoundTrip`：`bench_cli_test.go:378`，shmipc 仅 Linux（`server.go:227-229` 有同源的平台分支说明）。

判定本层改动是否破坏测试，以 Linux 结果为准；macOS 上至少要求 `go build ./cmd/taihu` 与 `go vet ./cmd/...` 干净。
