# 单元测试约定

> `_test.go` 与实现同目录同包，标准库 `testing` 一手到底、断言手写，逻辑测试表驱动；平台差异用「无 tag 契约测试 + 平台提供后端切片」切分，而不是在测试里 `if runtime.GOOS`。

---

## 规则 1：测试与实现同目录同包

`internal/storage/` 下 `storage.go` 与 `storage_test.go` 并列，两边首行都是 `package storage`（`internal/storage/storage.go:1`、`internal/storage/storage_test.go:1`）；`internal/metastore/meta.go:1` 与 `internal/metastore/meta_test.go:1` 同为 `package metastore`；`internal/transport/transport_frame_test.go:3` 是 `package transport`，与同目录 `frame.go`、`stats.go` 同包。白盒断言直接摸未导出符号，不需要任何导出或 `export_test.go` 层。

**全仓库只有一个外置测试包**：`internal/rpcclient/api_test.go:1` 的 `package rpcclient_test`。文件头写明了破例理由（`internal/rpcclient/api_test.go:11-20`）：

```go
// 本文件以**外部测试包**（package rpcclient_test，不是 package rpcclient）身份，
// 把本包导出的、以及出现在导出签名里的类型与常量逐个命名一遍。
//
// 为什么需要：reexport.go 里的 alias 是人手维护的，**漏补一条编译器不会报错** ——
// 包照样编过，只是对外悄悄不可用（调用方写不出那个类型的变量、构造不了字面量）。
// 本文件从外部包视角把这些名字全用一遍，漏了就编译失败，把「静默不可用」变成编译期错误。
```

它同时用外部包身份在**调用方自己的包里**实现接口，验证方法签名里没有 internal 类型（`internal/rpcclient/api_test.go:22-25`、`:41-42`）：

```go
var (
	// 外部实现可满足接口，且具体类型也可赋值给接口变量。
	_ rpcclient.ObjectStore = extStore{}
	_ rpcclient.ObjectStore = (*rpcclient.Storage)(nil)
)
```

## 规则 2：不用 testify，断言手写

`go.mod:12` 里**有** `github.com/stretchr/testify v1.9.0`（直接 require，不是 indirect），但仓库自有代码**零引用**：

| grep 范围 | testify 命中数 |
|---|---|
| `cmd/`、`examples/`、`internal/`、`pkg/`、`test/`（即本仓库所有 `_test.go`） | **0** |
| `third_party/shmipc-go/` 的 13 个上游 fork 测试文件 | 13 |

13 处全部是 `third_party/shmipc-go/{buffer,buffer_slice,buffer_manager,block_io,config,listener,net_listener,protocol_manager,queue,session,session_manager,stream,util}_test.go:23-32` 的 `import "github.com/stretchr/testify/assert"`。fork 已并入主模块（`go.mod:18-31` 的注释说明了原因），其测试随 `go test ./...` 一起跑，这就是 testify 仍留在 `go.mod` 里的原因。

**结论：判断「本仓库怎么写测试」时不要看 `go.mod`，要看代码 —— 自有测试里 `testify` 命中数为 0。** 断言一律手写标准库形式（`internal/layout/layout_test.go:21-26`）：

```go
			got := ComputeLayout(c.capacity, segSize)
			if got.SegmentSizeBytes != segSize {
				t.Fatalf("SegmentSizeBytes=%d want %d", got.SegmentSizeBytes, segSize)
			}
			if got.SegmentCount != c.wantCnt {
				t.Fatalf("SegmentCount=%d want %d", got.SegmentCount, c.wantCnt)
			}
```

失败消息惯例是 `"<主体>(<入参>)=<got> want <want>"` 或 `<what>: %v, want %v`，带上实际值与期望值两侧。

## 规则 3：以下设施在自有代码里的实际命中数

对 `cmd/ examples/ internal/ pkg/ test/` 逐个 grep（排除 `third_party/`、`vendor/`）的字面命中数：

| 形态 | 命中数 | 说明 |
|---|---|---|
| `t.Parallel()` | **0** | 全仓不用并发用例 |
| `testing.Short()` | **0** | 没有「短模式」分支，慢用例靠 build tag 隔离 |
| `func Example` | **0** | 无示例测试 |
| `func Benchmark` | **0** | 自有代码无 benchmark；仓库里 16 个全在 `third_party/netpoll/`（11 个）与 `third_party/shmipc-go/`（5 个） |
| `t.Run(` | 65 | 表驱动与子测试 |
| `t.Helper()` | 155 | |
| `t.TempDir()` | 97 | |
| `t.Cleanup(` | 64 | |
| `t.Setenv(` | 6 | |
| `t.Skipf(` | 22 | |
| `t.Skip(` | 7 | |

Benchmark 全部落在 fork 里（如 `third_party/netpoll/poll_test.go:116` 的 `BenchmarkPollMod`、`third_party/shmipc-go/queue_test.go:145` 的 `BenchmarkQueuePut`），**自有代码写性能测量不在单测层，在 `internal/benchkit/` 与 e2e 的 F/G 组**。

## 规则 4：表驱动是逻辑/纯函数测试的默认形态

`cases := []struct{...}` + `for _, tc := range cases` + `t.Run(`，用例名是中文短句。`internal/transport/protocol/protocol_test.go:81-103`：

```go
	cases := []struct {
		name string
		key  string
		size int64
	}{
		{"空 key 零长度", "", 0},
		{"单字节 key", "k", 1},
		{"常规", "some/object/key", 4 << 20},
		{"size 取 MaxInt64", "k", math.MaxInt64},
		{"size 为 -1（读至结尾的哨兵值经 uint64 往返）", "k", -1},
		{"key 恰为 MaxKeyLen", strings.Repeat("x", MaxKeyLen), 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
```

`internal/layout/layout_test.go:8-20` 同形（边界值专门列成用例：`{"小于一段", segSize - 1, 0}`、`{"恰好一段", segSize, 1}`）。纯映射类函数可以省掉 `name` 字段，直接内联匿名结构体（`internal/layout/layout_test.go:49-63`）：

```go
	cases := []struct{ in, want int64 }{
		{-1, 0},
		{0, 0},
		{1, BlockSize},
		{BlockSize - 1, BlockSize},
		{BlockSize, BlockSize},
		{BlockSize + 1, 2 * BlockSize},
	}
	for _, c := range cases {
		if got := Align4k(c.in); got != c.want {
			t.Fatalf("Align4k(%d)=%d want %d", c.in, got, c.want)
		}
	}
```

## 规则 5：招牌模式 —— 无 tag 契约测试 + 平台提供后端切片

这是本仓库最好的测试实践，`doc/设计文档/20260914_结构评审与优化建议.md:302` 点名要求推广到 transport 与 device。

**`internal/aio/aio_test.go` 不带任何 build tag**（第 1 行直接是 `package aio`），里面只写后端无关的契约断言（round-trip / 多 in-flight / 读越界 / Wait 超时 / fd 存活），以及一个后端描述结构（`internal/aio/aio_test.go:14-19`）：

```go
// backend 一个待测的后端实现。new 负责建好队列；后端在当前机器不可用时应 t.Skipf
// （带上原因，避免「静默跳过」在日志里看起来和「通过」一样）。
type backend struct {
	name string
	new  func(t *testing.T, maxEvents int) Ring
}
```

每条契约测试都是「同一断言体 × 后端切片」的两层结构（`internal/aio/aio_test.go:45-49`）：

```go
func TestRoundTrip(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) { assertRoundTrip(t, b.new(t, 8)) })
	}
}
```

`testBackends` 由平台文件提供：`internal/aio/aio_backends_linux_test.go:1` 是 `//go:build linux`（给 libaio + io_uring），`internal/aio/aio_backends_other_test.go:1` 是 `//go:build !linux`（只给 fallback）。

**关键规则：不可用的后端不在列表里悄悄过滤掉，而是让 `new` 回调 `t.Skipf` 带原因**（`internal/aio/aio_backends_linux_test.go:7-11`）：

```go
// testBackends 返回 Linux 上要跑契约测试的后端：libaio 与 io_uring。
//
// io_uring 不可用时不在这里过滤掉，而是让 new 回调 t.Skipf —— 这样 go test -v
// 会打出 `--- SKIP: TestRoundTrip/io_uring` 并带上 errno 原因。若在这里悄悄
// 剔除，老节点上的输出与「全部通过」完全一样，没人会发现 io_uring 根本没跑。
```

具体实现 `internal/aio/aio_backends_linux_test.go:30-32` 是 `if info := Probe(); !info.Supported { t.Skipf("io_uring 不可用: %s (kernel=%s)", info.Reason, info.KernelRelease) }`。**给自己写新后端切片时照抄这个形态：切片只列「这台机器上应该能跑的后端」，能不能跑由 `new` 里的探测决定，跳过必须带 errno/原因。**

## 规则 6：平台门控用 build tag，且断言体不带 tag

带 build tag 的测试文件全清单（`//go:build <tag>` 必须是文件第一行）：

- `//go:build linux`（10 个）：`cmd/taihu/cmd/server_cli_test.go`、`internal/aio/{aio_backends_linux,aio_linux,aio_linux_more,aio_uring_more,probe_linux,mode}_test.go`、`internal/device/{device_ioerr_linux,info_linux}_test.go`、`internal/rpcclient/shm_linux_test.go`、`internal/transport/{transport_shm,transport_shmframe}_test.go`
- `//go:build !linux`（1 个）：`internal/aio/aio_backends_other_test.go`
- `//go:build e2e`（8 个）：`test/e2e/` 下除 `doc.go` 外的全部文件

`_test.go` 后缀必须排在 GOOS 之后（`Makefile:62` 把这条列为经典坑之一）。本机是 darwin，linux 专有文件在本地根本进不了编译，所以每次改平台相关代码后必须跑 `make check-linux`（`Makefile:65-70`）。

## 规则 7：测试辅助函数带主体前缀，共享 fake 放 `testutil_test.go`

辅助函数用「被测主体 + 动作」命名，避免不同包之间同名混乱：

- `newTestServer` / `newTestServerMultiAddr` —— `internal/rpcclient/pool_test.go:20`、`:141`
- `tcpNewTestStorage` —— `internal/transport/transport_tcp_test.go:29`
- `shmDial` / `shmFillPattern` —— `internal/transport/transport_shm_test.go:71`、`:81`
- `assertRoundTrip` / `newTestFile` —— `internal/aio/aio_test.go:52`、`:22`
- `assertRoundTripODirect` —— `internal/aio/aio_linux_test.go:35`
- `eRunPool` —— `test/e2e/e_concurrency_test.go:26`

辅助函数体内第一句恒为 `t.Helper()`，资源用 `t.Cleanup` 释放而不是 `defer`（`internal/transport/transport_shm_test.go:71-79`）：

```go
func shmDial(t *testing.T, uds string) *ShmConn {
	t.Helper()
	c, err := DialShm(uds, 1)
	if err != nil {
		t.Fatalf("DialShm: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
```

需要落盘但不需要真块设备的用例用 `t.TempDir()`（`internal/device/device_test.go:18`、`internal/storage/storage_test.go:35`、`internal/metastore/store_test.go:313`）；`t.Cleanup` 的典型用法见 `internal/transport/transport_tcp_test.go:41`；`t.Setenv` 集中在环境变量驱动的 `internal/aio/mode_test.go:63,74,90,98,112`（AIO 模式选择）与 `internal/version/version_test.go:29`（改 `PATH` 做版本探测）。

跨用例复用的 fake 单独放 `testutil_test.go`，且只有这一个文件承担该职责（`pkg/taihu-client/testutil_test.go`）。里面的 fake 用**嵌入接口 + 按需注入错误**的写法，只覆盖要打断的那几条路径（`pkg/taihu-client/testutil_test.go:14-30`）：

```go
// errKV 包装 cluster.KV，按需让指定操作返回错误（错误/降级路径注入）。
// 只出现在测试里：生产代码不感知。
type errKV struct {
	cluster.KV
	scanErr  error
	getErr   error
	putErr   error
	delErr   error
	batchErr error
}
```
