# 平台专有代码的分文件约定

> 后缀定平台：`x_linux.go` 是真实现，`x_other.go` 是「非 Linux」的降级实现 —— 两个文件都带 build tag 或后缀门控，成对提供给同一组导出符号。

---

## 规则 1：命名只用 `_linux` / `_other` 成对，不用 `_darwin.go` / `_windows.go` / `_unix.go`

本仓库自有代码（`cmd/`、`internal/`、`pkg/`）里**没有**任何 `_darwin*.go` / `_windows*.go` / `_unix*.go` / `_bsd*.go` 文件，`grep -rn "^//go:build\|^// +build" --include="*.go" internal/ pkg/ cmd/` 只有 `linux` 与 `!linux` 两种取值。非 Linux 侧统一收敛到**一个** `_other.go`，而不是每个平台给一份。

非测试文件的完整清单（7 对 + 3 个单侧，共 10 个 `_linux.go`、7 个 `_other.go`）：

| 包 | `_linux.go` | `_other.go` | 配对 |
|----|-------------|-------------|------|
| `internal/aio` | `aio_linux.go` | `aio_other.go` | ✅ |
| `internal/aio` | `probe_linux.go` | `probe_other.go` | ✅ |
| `internal/aio` | `aio_uring_linux.go` | —— | 单侧 |
| `internal/device` | `device_linux.go` | `device_other.go` | ✅ |
| `internal/device` | `info_linux.go` | `info_other.go` | ✅ |
| `internal/rpcclient` | `dial_shm_linux.go` | `dial_shm_other.go` | ✅ |
| `internal/rpcclient` | `putwriter_linux.go` | `putwriter_other.go` | ✅ |
| `internal/transport` | `server_shm_linux.go` | `server_shm_other.go` | ✅ |
| `internal/transport` | `client_shm_linux.go` | —— | 单侧 |
| `internal/transport` | `shm_frame_linux.go` | —— | 单侧 |

单侧的那三个见 §7。这个约定是**演进结果**：早期 `aio`/`device` 用 `_linux` 后缀、`transport`/`pkg/rpcclient` 用 build tag，评审文档点名「同一个仓库里混用会让『哪些文件是平台专有』需要靠 grep 而非文件名判断」，并建议统一为 `_linux` 后缀（`doc/设计文档/20260914_结构评审与优化建议.md:80`）—— 现在后缀方案已经统一，该文档 §2.3 里的 `shmipc_server.go` / `pkg/rpcclient/` 路径是当时的旧名，已不存在。

## 规则 2：`_other.go` 必须显式写 `//go:build !linux`，`_linux.go` 多数只靠后缀

`other` **不是** Go 认识的 GOOS，所以 `x_other.go` 不会被文件名自动门控 —— 真门控它的是那一行 build tag。实测（`go1.25.0`，`GOOS=linux`）：把 `a_other.go` 与 `a_linux.go` 放同一个包且都定义 `A()`，`go build` 报 `A redeclared in this block`。所以 `_other.go` 侧漏写 tag 不是「少一层保险」，而是直接编译失败。

本仓库现状（7/7 的 `_other.go` 首行都是 tag，`_linux.go` 侧则混用）：

```go
//go:build !linux

package aio

// ring 非 Linux 平台（macOS 开发/自测）兜底实现：每个提交起一个 goroutine
// 执行同步 pread/pwrite，完成后唤醒 Wait。接口语义与 Linux 版一致。
type ring struct {
```
（`internal/aio/aio_other.go:1-2`、`:14-16`）

反过来，`_linux.go` 侧 10 个里的 9 个不带 tag（`aio_linux.go:1`、`aio_uring_linux.go:1`、`probe_linux.go:1`、`device_linux.go:1`、`info_linux.go:1`、`dial_shm_linux.go:3`、`client_shm_linux.go:8`、`server_shm_linux.go:12`、`shm_frame_linux.go:9`），只有 `internal/rpcclient/putwriter_linux.go:1` 冗余地带了 `//go:build linux`。**新文件只需后缀**，不必模仿那一处冗余。

> 命名约定在 `.trellis/spec/architecture/code-style.md` 第 7 条，两边表述一致：`_linux` 靠后缀，`_other` 必须带 `//go:build !linux`。

## 规则 3：`_other` 侧不是 stub，而是同语义降级实现

包文档把这条写成了明确承诺（`internal/aio/aio.go:13-17`）：

```go
// 平台策略：
//   - Linux：真异步（libaio 或 io_uring），缓冲必须由调用方持有到 Wait 返回
//     （对应 O_DIRECT 对齐池）。
//   - 其余平台（macOS 开发/自测）：goroutine + 同步 pread/pwrite 兜底，
//     接口语义一致（Submit 立即返回序号，Wait 阻塞收集完成），便于本地开发测试。
```

落到代码：`newLibAIORing` 在两侧是**同一个签名、同一份语义**（`internal/aio/aio_linux.go:56` vs `internal/aio/aio_other.go:31`），`ModeLibAIO` 在 macOS 上照样能建出可用的 `Ring` —— 这就是本机 `go test ./internal/aio/...` 能全绿的原因。设备层同理：`openDevice` 在非 Linux 侧只是把 `O_DIRECT` 换成 `O_SYNC`，函数签名与职责不变（`internal/device/device_linux.go:15-17` / `internal/device/device_other.go:9-11`）。

## 规则 4：区分「同语义降级」与「明确不支持」两种情况

降级不是万能的。做不到同语义的能力，`_other` 侧就**报错**，而且报得能被上层判掉：

**（a）明确不支持 —— 直接返回错误**。同一份 `aio_other.go` 里两种写法并存，是这条分界的最好例子：

```go
// newLibAIORing 构建兜底队列。非 Linux 平台没有 libaio，落到这里。
func newLibAIORing(maxEvents int) (Ring, error) {
	if maxEvents <= 0 {
		return nil, errInvalidMaxEvents
	}
	...
}

// newIOUringRing 非 Linux 平台没有 io_uring。
func newIOUringRing(int, bool) (Ring, error) {
	return nil, errors.New("aio: io_uring 仅 Linux 支持")
}
```
（`internal/aio/aio_other.go:30-39`、`:41-44`）

**（b）降级为「确定不可用」的探测结论**。`probe_other.go` 让 auto 模式能自己回退，而不是让调用方撞错误：

```go
// probe 探测 io_uring 可用性。非 Linux 平台恒不支持，且结论是确定性的（可缓存）。
// 导出入口在 aio.go，结论缓存与转发在 probe_cache.go。
func probe() (Info, bool) {
	return Info{Reason: "io_uring 仅 Linux 支持", KernelRelease: kernelRelease()}, true
}
```
（`internal/aio/probe_other.go:7-11`；第二个返回值 `true` 表示结论可缓存，消费方是 `internal/aio/probe_cache.go:23-33` 的 `probeCached`，它在 `probe_other.go` / `probe_linux.go` 之外 —— 缓存是平台无关的。`Probe` 在 `internal/aio/aio.go:207-209` 只做一层转发。）

**（c）shm 能力：不支持但上层可运行期判断**（见 §6）。

## 规则 5：包内未导出 sentinel，两个包各持一份是刻意的

非 Linux 的 shm 桩在 `internal/transport` 与 `internal/rpcclient` 里各有一个**未导出**的 `errShmUnsupported`，消息相同、互不复用：

```go
// errShmUnsupported shmipc 仅支持 Linux。
var errShmUnsupported = errors.New("taihu: shmipc only supported on linux")

// ShmSupported 报告本平台是否支持 shmipc 共享内存 IPC：非 Linux 恒为 false。
// 调用方（server 启动）据此跳过 shm 服务而非启动失败 —— 本平台 TCP 数据面
// 仍然完整可用，只是少了同机零拷贝那条路径。
func ShmSupported() bool { return false }
```
（`internal/transport/server_shm_other.go:14-20`；另一份在 `internal/rpcclient/dial_shm_other.go:12`，并由 `DialShm` / `DialShmPool` 返回，`:15-22`）

不要为「消重」把它们合并到 `internal/ierr`：它们是**不同层的平台桩**，行为与生存期独立（`.trellis/spec/architecture/error-model.md:68` 有同源结论）。客户端侧另有一个**导出**的哨兵 `ErrShmOnly`，它定义在跨平台的 `internal/rpcclient/putwriter.go:10-11`，因为调用方需要 `errors.Is` 它 —— 平台桩只负责返回，不负责定义。

## 规则 6：平台分文件不等于调用方要加 build tag

桩的设计目标之一是让上层主文件保持无 tag。`server_shm_other.go:3-4` 把这一点写进了文件头注释：

```go
// 共享内存 IPC 服务端占位实现：shmipc-go 仅支持 Linux（官方约束），
// 非 Linux 平台返回"仅支持 linux"错误，保证 cmd 主文件无需 build tag。
package transport
```

调用点因此是一个普通运行期分支（`cmd/taihu/cmd/server.go:210-228`）：

```go
		// 非 Linux 平台 shmipc 不可用（且 /dev 不可写），跳过该数据面而非启动失败 ——
		// TCP 服务照常，使服务端能在 macOS 上跑起来做本机联调。
		batchTarget, _ := cmd.Flags().GetInt("batch")
		batchWorkers, _ := cmd.Flags().GetInt("batch-workers")
		if transport.ShmSupported() {
```
（同形的客户端侧写法见 `internal/rpcclient/putwriter.go:31-33` 的 `NewPut`：TCP/非 Linux 返回 `ErrShmOnly`）

**新增平台能力时**：`_linux.go` 真实现 + `_other.go` 让 `ShmSupported()`（或等价的布尔/哨兵）能表达出来，别逼上层加 tag。

## 规则 7：确实无对应面的能力，允许只有 `_linux.go`

`aio_uring_linux.go`、`client_shm_linux.go`、`shm_frame_linux.go` 没有 `_other` 对手，因为它们要么被包内的桩挡在上游（io_uring 由 `newIOUringRing` / `probe` 挡住，见 §4），要么只被同样 linux-only 的文件引用（`shm_frame_linux.go:3-4` 说明它由 `client_shm_linux.go` 与 `server_shm_linux.go` 共用）。判断标准是「非 Linux 上有没有必要存在同名符号」：**有调用方**就必须有成对文件，**没有**才允许单侧。

## 规则 8：`_test.go` 后缀必须排在 GOOS 之后

这是个静默坑，`Makefile:60-64` 把它和历史踩过的坑并列写进了注释：

```make
# 跨平台编译核对：必须在 macOS/linux 上都能编过。
# 仓库有大量平台专有代码（unix.POLLIN 仅 linux 存在、Syscall6 参数个数、
# 结构体 size/offset 断言、_test 后缀必须排在 GOOS 之后），这些错误在 macOS
# 上编得过、只在 linux 上暴露，因此每次改动平台相关代码后都应跑一遍。
# test -c 只编译测试二进制不执行 —— 本机是 macOS，linux 测试跑不了，但能编过。
```

实测（`go1.25.0`）错序的后果：包 `p` 里放 `p_wrongname_test_linux.go`，`go test -v -run TestAdd ./...` 输出 `?   tmp/gotestorder  [no test files]` —— 文件不匹配 `_test.go` 结尾，被当成**普通源码**编进包，`TestAdd` 永远不会被执行，也不报错。

两条可操作的规则：

- 文件名写成 `x_linux_test.go`（GOOS 紧邻 `_test`），后缀本身即可门控，如 `internal/aio/aio_linux_test.go:1`、`internal/device/info_linux_test.go:1`、`internal/rpcclient/shm_linux_test.go:1`。
- GOOS **不紧邻** `_test` 的文件必须补显式 tag：`internal/aio/aio_linux_more_test.go:1`（`..._linux_more_test.go`）、`internal/aio/aio_uring_more_test.go:1`、`internal/aio/mode_test.go:1`、`internal/transport/transport_shm_test.go:1`、`internal/transport/transport_shmframe_test.go:1`、`cmd/taihu/cmd/server_cli_test.go:1` 全部带 `//go:build linux`。

`_other` 侧的测试同理：`internal/aio/aio_backends_other_test.go:1` 是 `//go:build !linux`，它与 `aio_backends_linux_test.go:1` 成对提供同一个 `testBackends`（契约测试的写法见 `.trellis/spec/testing/unit-tests.md` 规则 5）。

## 规则 9：`third_party/` 免 gofmt，因为它要跟上游对齐

```make
# third_party/ 是上游 fork，保持与上游一致的格式，不纳入本地 gofmt 校验。
check-fmt:
	@bad=$$(gofmt -l . | grep -v '^third_party/' || true); \
	if [ -n "$$bad" ]; then echo "以下文件未通过 gofmt："; echo "$$bad"; exit 1; fi; \
	echo "check-fmt: OK"
```
（`Makefile:24-28`）

这条排除是**承重**的，不是图省事：实测 `gofmt -l third_party/ | wc -l` = 45，而 `gofmt -l . | grep -v '^third_party/'` **无输出**。也就是说仓库其余部分是 100% gofmt 干净的，一旦去掉那行 `grep -v`，门禁立刻变红；而按 gofmt 重排 fork 会让「与上游 diff」失去意义（fork 的 patch 是承重的，见 `third_party/README.md:197-198`）。**改 `third_party/` 下任何文件都不要顺手 reformat。**

## 规则 10：`third_party/{netpoll,shmipc-go}` 并进主模块，不用嵌套 `go.mod` + `replace`

目录下**没有** `go.mod`（`find third_party -name go.mod` 无输出），`go.mod` 里也**没有** `replace` 指令；import 路径直接是本仓库路径，如 `internal/transport/client.go:10` 的 `github.com/liucxer/taihu/third_party/netpoll`。理由记在两处，互为印证（`go.mod:18-27`）：

```go
// 两份 fork（third_party/netpoll、third_party/shmipc-go）曾经是独立的嵌套子模块，
// 由上面的 replace 以相对路径挂进来。该做法有个对外致命的性质：Go 的 replace
// **只在主模块生效、不会传递给消费者**，于是任何外部模块 import 本仓库的 pkg/
// 都会拿到上游 netpoll/shmipc-go，因缺少 fork 新增的 API 而构建失败
// （实测：undefined: netpoll.SetAlignedAllocator）。
```

消费者视角的完整后果在 `third_party/README.md:6-24`：编译期表现为 `undefined: netpoll.SetAlignedAllocator / SetInputAlignedAllocator / SetInputNodeSize`（`third_party/README.md:19-21`），更隐蔽的是**匿名接口断言会静默退化成拷贝路径**（`third_party/README.md:42`）—— 所以「消费者自己也加一条 replace」不构成绕过方案，并模块是唯一解。

## 附录：上游带来的同名后缀，语义正好相反

`third_party/netpoll/sys_epoll_linux_other.go:15` 也是 `_other` 结尾，但它的含义是「linux 上除 amd64/arm64/riscv64 之外的架构」，tag 是 `//go:build linux && !arm64 && !amd64 && !riscv64` —— 与我们的 `_other`（= 非 Linux）**正好相反**。这个文件名是上游 v0.7.5 带进来的（`third_party/README.md:45-49` 的基线表），fork 不改上游命名。在 `grep _other` 时要注意区分这两类；本层 §1 的清单只统计仓库自有代码。
