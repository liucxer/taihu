# testing —— 测试实践（gbp 类 7）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/testing-*.md`，7 条。
> 4 条适用 / 3 条**刻意偏离**。

## 本仓库的测试规模

| 项 | 数量 | 核实命令 |
|---|---|---|
| 测试代码 | **15013 行** | `find internal cmd pkg -name '*_test.go' \| xargs wc -l \| tail -1` |
| 非测试代码 | **13122 行** | `find internal cmd pkg -name '*.go' ! -name '*_test.go' \| xargs wc -l \| tail -1` |
| `t.Run` 子测试 | 41 | `grep -rn 't\.Run(' --include='*_test.go' internal cmd pkg` |
| `t.Helper()` | 90 | 同上模式 |
| `t.Cleanup(` | 51 | 同上模式 |
| `t.Parallel()` | **0** | 同上模式 |
| `func Benchmark` | **0** | 同上模式 |
| `go:generate` | **0** | `grep -rn 'go:generate' --include='*.go' internal cmd pkg` |

**测试代码比非测试代码还多**（15013 vs 13122）——但这不等于 gbp-040 的目标达成，原因见该条。

## 逐条裁决

### gbp-040 · 99% Test Coverage Target（CRITICAL）— **不适用（百分比硬指标）**

**规则**：生产代码目标 99% 覆盖率；用 `go test -coverprofile` + `go tool cover -func/-html`；CI 中低于 99% 就 `exit 1`；按分层设目标（domain 100% / application 99% / infra 95% / interface 90%）。

**对 taihu：不适用。** 理由有三条，都可检验：

1. **99% 是硬指标，本仓库不按百分比管理测试**。当前实测覆盖率（命令见下）从 48.2% 到 100% 分布，没有任何一条门禁按百分比卡。
2. **它的分层目标表建立在 DDD 分层上**（domain / application / infra / interface），而那一层对 taihu 不适用——见 [ddd/](../ddd/index.md) 的 gbp-010。
3. **对存储引擎来说，覆盖率数字与测试价值的相关性更弱**。大量关键是「在真机上跑真实 IO 是否出问题」，不是「有多少行被执行过」——这也是本仓库自建 `internal/benchkit` 与 `test/e2e/` 的原因。

**但有两个具体要求被采纳**（它们是手段，不是那条 99% 指标）：

- **按包统计覆盖率**，而不是全仓总百分比——全仓总数会被大包稀释，一个 48.2% 的包摊进 16 个包里总均值仍有 90.4%，异常会被抹平。
- **每处覆盖率数字必须附可复现命令**，见下。

**当前实测**（2026-09-22，`go test -cover $(go list ./... | grep -v third_party | grep -v 'test/e2e')`）：

| 包 | 覆盖率 | | 包 | 覆盖率 |
|---|---|---|---|---|
| `internal/aio` | **48.2%** ← 最低 | | `internal/metastore` | 94.1% |
| `cmd/taihu/cmd` | 82.7% | | `internal/rpcclient` | 97.2% |
| `cmd/taihu` | 85.7% | | `pkg/taihu-client` | 97.6% |
| `internal/device` | 85.9% | | `internal/benchkit` / `internal/layout` / `internal/transport/protocol` / `internal/version` | 100.0% |
| `pkg/bufpool` | 88.4% | | | |
| `internal/transport` | 90.9% | | | |
| `internal/storage` | 91.4% | | | |
| `examples/taihu-client` | 91.7% | | | |
| `internal/cluster` | 92.1% | | `internal/ierr` | `[no statements]` |

`internal/ierr` 无语句可计数（只有 sentinel 定义），**不适用任何覆盖率要求**，不要为了让它「有数字」去加测试。

**解析陷阱**：`cmd/taihu/cmd` 的 `coverage:` 行**不附在** `FAIL\t<pkg>` 那一行末尾，而是被测试二进制的裸 `FAIL` 隔开、单独占一行。按「`ok`/`FAIL` 行」抓取的工具会整个漏掉这个包。

**这条命令在本机退出码是 1**，因为 `cmd/taihu/cmd` 有 2 个已知平台性失败（见本层末节）。**读输出里的数字，不要只看退出码。**

**`internal/aio` 的 48.2% 是怎么来的**：它的分母只有 137 条语句，而该包非测试代码 1529 行中 **1017 行（66%）是 Linux 专属**（`aio_libaio_linux.go` / `aio_uring_linux.go` / `probe_linux.go`）。在 macOS 上：

1. Linux 专属文件**不编译，也就不进统计**——占该包 66% 的实现连分母都没进。
2. 平台测试带 `_linux_test` 后缀，本机**不运行**。
3. `make check-linux` 只做 `go test -c`（`Makefile` 的 `check-linux` 目标），**编译测试二进制而不执行**。

所以这是**本机测不到**，不是**不达标**。要拿真实覆盖率得在 Linux 机器上跑。**不要把 48.2% 当成待补的缺口去追。**

### gbp-041 · Table-Driven Tests（HIGH）— **适用**

**规则**：用表驱动测试提升可维护性——匿名 struct 切片 + `t.Run(tt.name, ...)` 子测试；表里可放 `setup func(*testing.T)` 构造依赖；按 `wantErr` 断言。

**对 taihu：适用，本仓库主流做法**（41 处 `t.Run`）。

**要点一**：表项必须有 `name`，`t.Run` 用它做子测试名——失败时才能定位到是哪一行数据。
**要点二**：表**保持单层**，不要嵌套表测试。若一个用例需要「调用 → 断言 → 再调用 → 再断言」，它测的是多个行为，拆成两个子测试。
**要点三**：循环变量在 `t.Parallel()` 下要重新捕获（`c := c`）。本仓库当前 `t.Parallel()` 零使用，所以这个坑暂时不触发——但**引入 `t.Parallel()` 时**必须同时处理。

### gbp-042 · Mock and Interface Abstraction（HIGH）— **不适用（不用 mockgen）**

**规则**：通过接口抽象 + mockgen 做依赖注入与 mock 测试；用 `//go:generate mockgen -source=...` 生成 mock，`gomock.NewController` + `EXPECT()` 驱动。

**对 taihu：不适用（未引入代码生成的 mock）。** `grep -rn 'go:generate' --include='*.go' internal cmd pkg` → **0**。

**本仓库的替身策略是手写 fake + 包级变量接缝**：

- **手写 fake**：共享的 fake 放 `testutil_test.go`，测试辅助函数带主体前缀（如 `newTestFile` / `shmDial`）。
- **包级变量接缝**：如 `cmd/taihu/cmd/helpers.go` 的 `kvConnect` —— 生产路径恒为真实实现，测试替换它。**替换时必须写明「生产路径行为不变」**。

**与 gbp-031 的关系**：接口仍然按「消费方定义、尽量小」来做（`internal/rpcclient/objectstore.go:15` 有明确论证：接口只需一条 `var _ ObjectStore = (*Storage)(nil)` 断言即可，外部实现也能满足）。**不引入 mockgen 不等于不抽象接口**——本仓库抽象了接口，只是不生成 mock。

### gbp-043 · Test Helper Function Guidelines（MEDIUM）— **适用**

**规则**：测试辅助函数必须调 `t.Helper()`（让报错行号指向调用处）；**校验逻辑留在测试里**，helper 只取数；不要在 goroutine 里调 `t.Fatal`（要用 channel 回传错误）。

**对 taihu：适用，当前符合**（90 处 `t.Helper()`、51 处 `t.Cleanup(`）。

**要点一**：`t.Helper()` 是**必须**的。没有它，helper 里的失败会指向 helper 内部的行号，排查时要多跳一层——本仓库 90 处已是主流，新增的 helper 不要漏。

**要点二**：helper **只取数不断言**。`getUserCount(...)` 对，`assertUserCount(...)` 里塞断言逻辑就错——断言散进 helper 后，失败信息不再指向具体用例。

**要点三（`t.Cleanup` 还是 `defer`）**：判据是**资源是否被借出给调用方**。

- **helper 里借出并 `return` 给调用方的资源 → 用 `t.Cleanup`**。因为 helper 自己 `defer` 会在 helper 返回时就释放掉，调用方拿到的就是已关闭的资源。正例：`transport_shm_test.go` 的 `shmDial`（拨号后 `t.Cleanup(func() { _ = c.Close() })` 再 `return c`）、`internal/aio/aio_test.go` 的 `newTestFile`（借出 `*os.File`，在 helper 内注册 `t.Cleanup`）。
- **其余情况用 `defer`** 即可——`defer` 的作用域是当前函数，够用且更直白。

**不要教条地把 `defer` 全改成 `t.Cleanup`**：本仓库 `defer X.Close()` 与 `t.Cleanup(` 并存是有意的，各自匹配上面两种形态。

### gbp-044 · Performance Benchmark Testing（MEDIUM）— **刻意偏离**

**规则**：用 benchmark 量化性能，配合 `benchstat` 对比；`for i := 0; i < b.N; i++`；`-benchmem` / `-count`；`b.Run` 子基准；`b.ResetTimer()`。

**对 taihu：刻意偏离（不是遗漏）。** `grep -rn 'func Benchmark' --include='*_test.go' internal cmd pkg` → **0**。

**为什么不做**：

- 本仓库的性能测量走 `internal/benchkit`（自研 harness）与 `test/e2e/` 的 F / G 分组——它们跑**真实设备 IO**。
- `go test -bench` 压不满真实 IO：它的循环里没有真实块设备的队列深度、没有 `O_DIRECT` 的对齐开销、没有 io_uring 的提交/完成批处理。
- 本仓库有 **shm 与 TCP 两条数据面**，`go test -bench` 覆盖不了。

**所以「加个 benchmark」在这里不是补测试，是换个测不准的工具。** 要量化性能，用 `internal/benchkit`，报告格式参照 `doc/性能测试报告/`。

### gbp-045 · Integration Testing Guidelines（HIGH）— **部分适用**

**规则**：用 build tag + testcontainers 写集成测试；`//go:build integration` 隔离，默认 `go test ./...` 不跑；起真实 MySQL 容器 + `httptest.NewServer`。

**对 taihu：部分适用——build tag 那半适用且做得更细，testcontainers 那半不适用。**

**适用的一半（build tag）**：本仓库用两组构建约束隔离测试：

- `//go:build e2e` —— `test/e2e/` 的端到端套件。不设 `E2E_PD` 环境变量时**全部 Skip**。
- `//go:build linux` —— 平台专有测试（io_uring / libaio 路径）。

**注意 `_test` 后缀必须排在 GOOS 之后**（`internal/aio/aio_linux_test.go`，不是 `aio_test_linux.go`）。

**不适用的一半（testcontainers）**：正例是起真实 MySQL 容器 + `httptest.NewServer`；本仓库无 MySQL、无 HTTP 服务端（见 [framework/](../framework/index.md)）。本仓库的集成测试起的是**真实块设备与真实集群**，靠环境变量（`E2E_PD` / `E2E_WORKDIR`）而不是容器编排。

**一个必须知道的坑**：`E2E_WORKDIR` **不要用 `/tmp`** —— tmpfs 不支持 `O_DIRECT`，测试会以难懂的方式失败。

### gbp-046 · testify Assertion Library Usage（MEDIUM）— **刻意偏离**

**规则**：用 testify 让断言更清晰，区分 `assert`（继续）与 `require`（立即终止）；`assert.ErrorIs` / `Len` / `Contains` / `Empty`；`suite.Suite` + `SetupSuite` / `TearDownSuite`。

**对 taihu：刻意偏离（不是遗漏）。** 自有代码零引用。

`github.com/stretchr/testify v1.9.0` 确实在 `go.mod` 的直接依赖里，但它**只为 `third_party/shmipc-go/` 的 fork 测试而存在**——`grep -rl 'stretchr/testify' --include='*.go' .` 命中的 10 个文件**全部**在 `third_party/` 下。

**为什么自有代码不用**：

- 标准库 `t.Errorf` / `t.Fatalf` 对纯 Go 测试足够，本仓库的表驱动测试（`t.Run` + `wantErr`）不需要更丰富的断言 DSL。
- 引入 testify 会让测试代码多一层依赖——而 `third_party/` 的 fork 已经把它拖进 `go.mod` 了，自有代码再引入等于把这个依赖变成"我们的"。

**注意**：不要因为 `go.mod` 里有 testify 就以为可以用。**自有代码用 testify 会被本 spec 判为偏离。**

---

## 已知的平台性失败（本机固有，不是回归）

本机（macOS）跑 `go test ./...` **不是全绿**，工作区干净时就如此：

| 包 | 失败测试 | 原因 |
|---|---|---|
| `cmd/taihu/cmd` | `TestBenchStorageCmdErrorPaths`、`TestBenchSingleCmdShmRoundTrip` | shm 仅 Linux / 非块设备回退路径 |
| `third_party/shmipc-go` | `Test_EventDispatcher` 等 | fork 的上游代码，测试套件在本机 panic |

**判定改动是否破坏测试，以 `make check-linux` 为准**（它做双架构编译核对），不以本机 `go test ./...` 的红绿为准。**不要为了「让本机全绿」去改这些测试。**
