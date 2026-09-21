# 跨平台构建验证

> 本机是 macOS，Linux 专有代码在这里「编得过」不构成证据 —— 平台相关的错误恰恰是 darwin 编得过、只在 linux 暴露，所以唯一的口径是 `make check-linux`，而 Linux 单测只能到 Linux 机器上跑。

---

## 规则 1：`make check-linux` 就是这四条命令，逐条对上

```make
check-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./...
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o /dev/null ./...
	@echo "check-linux: OK"
```
（`Makefile:65-70`）

| # | 命令 | 它在守什么 |
|---|------|-----------|
| 1 | `go vet ./...`（linux/amd64） | 静态检查在 **linux 视角**下重跑一遍：只有 linux 才会进编译的文件（`aio_uring_linux.go`、`client_shm_linux.go`、`server_shm_linux.go`、`shm_frame_linux.go`、`probe_linux.go`）里未被使用的变量、错误的 `printf` 格式串、`lostcancel` 等 |
| 2 | `go build ./...`（linux/amd64） | 生产架构必须真能编出二进制 |
| 3 | `go build ./...`（linux/arm64） | 交叉编译第二个架构 —— 这些 Linux 专有文件大量使用 `uintptr` / `unsafe` / `Syscall6`，字长敏感 |
| 4 | `go test -c -o /dev/null ./...` | **只编译不执行**测试二进制：把 `_test.go` 也放进 linux 视角编一遍，于是 `_test` 后缀顺序错误、测试里引用了不存在的 linux 符号等问题会在本机暴露 |

第 4 条是本仓库的关键设计：`test -c` 不跑测试，所以 `GOOS=linux` 的测试二进制能在 macOS 上编出来。

## 规则 2：为什么必须跑它 —— 平台错误在 darwin 上是隐形的

```make
# 跨平台编译核对：必须在 macOS/linux 上都能编过。
# 仓库有大量平台专有代码（unix.POLLIN 仅 linux 存在、Syscall6 参数个数、
# 结构体 size/offset 断言、_test 后缀必须排在 GOOS 之后），这些错误在 macOS
# 上编得过、只在 linux 上暴露，因此每次改动平台相关代码后都应跑一遍。
# test -c 只编译测试二进制不执行 —— 本机是 macOS，linux 测试跑不了，但能编过。
```
（`Makefile:60-64`）

第三行点名的三类错误在本仓库都有实体：`syscall.Syscall6` 出现在 `internal/aio/probe_linux.go:50-51`、`internal/aio/aio_uring_linux.go:528`、`:601`、`:615`、`internal/aio/aio_linux.go:203`；结构体 size/offset 断言在 `internal/aio/aio_uring_linux.go:139-147`（见 §3）。这些文件在 macOS 上**根本不参与编译**，所以本机全绿毫无信息量。

同一个理由还决定了 `check-layering` / `check-sdk-only` 的实现方式 —— 这两个门禁**用 grep 而不是 `go list`**：

```make
# 两条都用 grep 源码，**不用 go list**：go list 在本机（darwin）看不见
# `//go:build linux` 的文件，而 internal/transport/server_shm.go 恰恰是最容易漏改的那个 ——
# 用 go list 会本机通过、Linux 上炸。grep 对 build tag 无感。
```
（`Makefile:38-41`）

门禁不是摆设，上线即抓到过真问题：残留的 rpcserver 引用、以及 vet 的 `lostcancel` 告警（`doc/设计文档/20260914_结构评审与优化建议.md:323`）。该处写的 `pkg/rpcclient/pool_test.go` 是评审当时的路径，`pkg/rpcclient` 现已迁到 `internal/rpcclient`。

## 规则 3：`CGO_ENABLED=0` 在本项目里的确切含义

不是「顺手关掉 cgo」，而是对一个具体承诺的守门：**`internal/aio` 是纯 Go 实现，不链接 `-laio`、不依赖 libc**。包文档开头就写着：

```go
// Package aio 提供异步磁盘 IO 的纯 Go 实现，不依赖任何外部库。
//
// Linux 上有两个后端，都只封装系统调用、结构体按 UAPI 手工声明（无需 cgo）：
```
（`internal/aio/aio.go:1-3`）

「手工声明」指的是照 UAPI 头文件抄结构体：`internal/aio/aio_linux.go:10-13` 注明「事件与 iocb 布局必须与 linux/aio_abi.h 一致（64 位平台）」并抄了 `struct io_event` / `struct iocb` 的字节数（32 / 64），`internal/aio/aio_uring_linux.go:125-137` 抄了 120 字节的 `io_uring_params`。既然布局是手抄的，就必须有编译期护栏：

```go
// 编译期布局断言：与内核 UAPI 不一致时索引越界，直接编译失败（零运行时代价）。
var (
	_ = [1]byte{}[unsafe.Sizeof(ioUringParams{})-120]
	_ = [1]byte{}[unsafe.Sizeof(ioUringSQE{})-ioUringSQESize]
	_ = [1]byte{}[unsafe.Sizeof(ioUringCQE{})-ioUringCQESize]
	_ = [1]byte{}[unsafe.Offsetof(ioUringSQE{}.UserData)-32]
	_ = [1]byte{}[unsafe.Offsetof(ioUringSQE{}.BufIndex)-40]
	_ = [1]byte{}[unsafe.Offsetof(ioUringCQE{}.Res)-8]
)
```
（`internal/aio/aio_uring_linux.go:139-147`：`[1]byte{}[n]` 的索引只有 `n == 0` 才合法，所以断言失败等于常量越界 → 编译错误）

核实方式与结论：在本仓库内（含 `third_party/`）`grep -rn 'import "C"' --include="*.go" .` **零命中**，`grep -rln '^//go:build cgo' --include="*.go" .` 同样零命中 —— `internal/aio` 那句「无需 cgo」在本仓库范围内是字面成立的（cgo 只从模块依赖的传递路径上进来，见下）。

但 `CGO_ENABLED=0` 在本机是**编不过的**，实测（darwin / `go1.25.0`）：

```console
$ CGO_ENABLED=0 go build ./...   # 退出码 1
../../../../pkg/mod/github.com/elastic/gosigar@v0.14.2/sigar_darwin.go:13:12: undefined: sysctlbyname
$ CGO_ENABLED=1 go build ./...   # 退出码 0
```

原因是传递依赖：`internal/cluster` → `tikv/client-go/v2/txnkv` → `.../tikv` → `tikv/pd/client/resource_group/controller` → `github.com/elastic/gosigar`（`go mod why -m github.com/elastic/gosigar` 的输出）。gosigar 只在 darwin 上需要 cgo —— `sigar_common_darwin.go` 是 `import "C"` 的实现，`sysctlbyname` 由它提供；`sigar_linux.go` 没有 `import "C"`，是纯 Go。

所以 **`GOOS=linux` 与 `CGO_ENABLED=0` 必须成对出现**：`Makefile:66-69` 的四条命令每条都同时写两者，单独在本机加 `CGO_ENABLED=0` 只会得到上面那条编译错误。这也说明门禁真正依赖的是 **linux 侧依赖树的纯 Go 路径**，而不是本机 cgo 的可用性。

## 规则 4：本机能做什么、什么必须上 Linux

| 验证 | 本机（darwin） | 说明 |
|------|----------------|------|
| gofmt / 分层不变量 / `go vet ./...`（darwin 视角） | ✅ `make check` | 见 `.trellis/spec/architecture/layering.md` |
| linux/amd64 + linux/arm64 的 vet / build / `test -c` | ✅ `make check-linux` | 本机实测通过（`check-linux: OK`） |
| `CGO_ENABLED=0 go build ./...`（**不带** `GOOS=linux`） | ❌ 编不过 | gosigar 在 darwin 上是 cgo 实现，报 `undefined: sysctlbyname`；`CGO_ENABLED=0` 必须与 `GOOS=linux` 配对（§3） |
| 非平台专有包的单测 | ✅ `go test ./...` | `internal/aio`、`internal/device`、`internal/rpcclient`、`internal/transport`、`internal/storage` 等本机全绿 |
| Linux 专有逻辑的单测 | ❌ 只能编过 | 带 `//go:build linux` 的测试（`transport_shm_test.go`、`aio_uring_more_test.go`、`shm_linux_test.go` …）在本机**根本不进编译**，`go test -c` 只证明它编得过 |
| 真异步 IO / io_uring / shmipc 往返 | ❌ 必须上 Linux 机 | 本机 `Probe()` 恒返回 `Supported=false`（`internal/aio/probe_other.go:9-11`），跑的是 goroutine 兜底 |

**本机 `go test ./...` 不是全绿，这是已知的平台性状态**（工作区干净时即如此，非改动引入）。实测 7 个失败，全部落在 Linux 专有路径上：

- `cmd/taihu/cmd` 2 个：`TestBenchStorageCmdErrorPaths`（断言「普通文件上 `BLKGETSIZE64` 必失败」，macOS 走 `info_other.go` 的 `Stat` 回退路径故不报错）、`TestBenchSingleCmdShmRoundTrip`（shmipc 仅 Linux）。
- `third_party/shmipc-go` 5 个：`Test_VerifyConfig` 与 `Test_CreateCSWithoutConfig` 的错误文本就是 `shmipc just support linux OS now`；`TestBlockReadFullAndBlockWriteFull` 死在 unix socket `bind: invalid argument`；`Test_EventDispatcher` 在 epoll 事件循环里 panic。另 1 个 `TestGlobalBufferManagerErrors` 只观察到断言失败，未逐条归因。

**判定口径**：改平台相关代码后，本机以 `make check` + `make check-linux` 绿为准；**不要**把上述 7 个失败当成自己的破坏，也不要为了「让本机全绿」去改它们。

## 规则 5：Linux 测不了的替代 —— 把断言抽到无 tag 的契约测试里

平台门控只用在**提供后端清单**的文件上，断言体本身不带 tag。`internal/aio/aio_linux_test.go` 与 `aio_backends_linux_test.go` 提供 libaio + io_uring，`internal/aio/aio_backends_other_test.go` 提供兜底后端：

```go
// testBackends 返回非 Linux 平台可跑的后端：只有 goroutine 兜底实现
// （aio_other.go）。io_uring 与 libaio 都是 Linux 专有，此处无需出现。
func testBackends(t *testing.T) []backend {
	t.Helper()
	return []backend{
		{
			name: "fallback",
			new: func(t *testing.T, maxEvents int) Ring {
```
（`internal/aio/aio_backends_other_test.go:7-14`）

于是本机能跑 `internal/aio` 的全部契约断言（实测 `ok github.com/liucxer/taihu/internal/aio`），只是跑在兜底后端上；Linux 上同一组断言跑三倍的后端，且不可用的后端**用 `t.Skipf` 带 errno 报出来**而不是静默剔除（`internal/aio/aio_backends_linux_test.go:7-11` 的理由说明）。这套模式的价值判断见 `.trellis/spec/testing/unit-tests.md` 规则 5。

## 规则 6：`check-linux` 的覆盖面包含 `third_party/`

`third_party/` 是主模块的一部分（没有嵌套 `go.mod`，见 `file-splitting.md` §10），所以 `./...` 会扫到它：`go vet ./...` 会对 fork 代码报 vet 告警，`go test -c ./...` 会编译 fork 的测试（netpoll 18 个 `_test.go`、shmipc-go 15 个，`third_party/README.md:103-115`、`:154-158` 有清单），`go test ./...` 也会跑它们（`third_party/README.md:115`「均随主模块 `go test ./...` 一起跑」）。

这带来一个具体后果：**并模块把两份 fork 的有效语言版本从 `go1.15` / `go1.20` 升到主模块的 `go1.25`，vet 因此新报出 3 处问题**（`third_party/README.md:173-175`），修法记在 `third_party/README.md:139-153` 的 **[门禁]** 段（`reflect.SliceHeader` misuse、非常量格式串、`unreachable code`）。改 fork 或升 Go 版本时，`check-linux` 是这几类问题的第一发现点。

## 规则 7：动过 fork 后的验收口径是三条命令，且不能只靠门禁

`third_party/README.md:182-194` 把 fork 更新流程写成可执行清单，最后一步是：

```bash
make check && make check-linux && go test ./...
```

再加两条运维约束（`third_party/README.md:162-166`）：shm 的段布局与上游 ABI 不兼容，**两端必须同时用本 fork**，升级后要做一次实际 Put/Get 往返；shm 与 netpoll 两条路径的往返验证互相不能替代。也就是说 `make check-linux` 绿只证明「编得过」，**不证明 shm 协议还通** —— 后者必须上 Linux 机器。同一段还提醒：门禁绿只覆盖迁移后仍然存在的测试，对 fork 做非平凡改动仍建议在临时副本里并入上游测试跑一遍。
