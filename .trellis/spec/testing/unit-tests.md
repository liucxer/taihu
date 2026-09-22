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

---

## 规则 8：表测试保持"输入 → 期望"的单层形态

上游 `uber-119` / `120` / `122` / `123` / `124` 是一组：表测试只应承载**行为仅随输入变化**的用例；一旦表里需要条件断言、多条分支路径，就该拆成多个测试或独立 `Test...` 函数。

taihu 现状是该组的**既成正例** —— 实测自有测试里：

```bash
# 表字段用 shouldXxx 式分支开关的 —— 无命中
grep -rnE 'should[A-Z][a-zA-Z]*\s+(bool|error|string)' --include='*_test.go' internal pkg cmd test
# 表里放函数值的（setupMocks func(*X) 式）—— 无命中
```

表一律是「输入 → 期望输出」。规则 4 的两个例子（`internal/transport/protocol/protocol_test.go:81-103` 的 `name/key/size`、`internal/layout/layout_test.go:49-63` 的 `in/want`）都是这个形态。

**字段名的省略规则**（上游 `uber-107`）：表中字段数 **≤3 时可以省略字段名**，直接写匿名结构体。本仓库两种写法都在用，各自对应当下规模：

| 写法 | 条件 | 实例 |
|------|------|------|
| 带字段名 + `name string` | 需要用例名，或字段 >3 | `internal/transport/protocol/protocol_test.go:81-103` |
| 匿名字段 `struct{ in, want int64 }` | ≤3 字段的纯映射 | `internal/layout/layout_test.go:49-63` |

## 规则 9：断言失败用 `t.Fatal*` / `t.Error*`，不用 `panic`

上游 `uber-037`：即使在测试里也应优先用 `t.Fatal` / `t.FailNow` 而不是 panic，确保失败被标记为**测试失败**而非整个测试二进制崩溃。

实测自有 `_test.go` 里只有 2 处 `panic`，**都不是断言替代品**：

| 位置 | 内容 | 为什么可以留 |
|------|------|--------------|
| `internal/storage/storage_test.go:24-27` | `alignedPayload(n int)` 里 `panic("alignedPayload requires 4K multiple")` | 函数**签名里没有 `t *testing.T`**，调不了 `t.Fatalf`；传入非 4K 倍数是测试作者写错，不是被测行为 |
| `internal/transport/protocol/protocol_test.go:390` | 解析器分发表 `switch` 的 `panic("unknown parser " + name)` | 同上，该 helper 返回 `error` 给调用方，未知 name 是测试作者写错 |

**规则**：断言与"被测行为不符合预期"一律 `t.Fatalf` / `t.Errorf`。只有「签名里没有 `t`、且输入由测试作者写死」的纯 helper 才允许 panic —— 且新增这类 helper 时**优先改成返回 `error` 或补上 `t` 参数**，不要照抄上面两处。理由：helper 里的 panic 炸掉的是整个包的所有用例，失败定位成本远高于一次 `t.Fatal`。

## 规则 10：先写测试（硬规则）

上游 `ecc-034` 的 TDD 流程：**先写测试（RED）→ 跑到失败 → 最小实现（GREEN）→ 跑到通过 → 重构 → 复查覆盖率**。

**这条在本仓库先按"从今往后"执行，历史提交记为待改进** —— 用 git 历史做锚点，两条代表性的：

| 提交 | 实测 | 说明 |
|------|------|------|
| `fed67b4` | 改 84 个文件，其中 **81** 个 `_test.go` | 一次成批补测试，是"先实现后补测"的典型形态（注：该提交**信息**自称「82 个单测文件」，与 `--stat` 不符，引用时用 81） |
| `9bcc797` | 改 6 个文件（`internal/device/{device,device_linux,options}.go` + `third_party/shmipc-go/` 3 个），**0** 个 `_test.go` | 改了设备重试与 O_EXCL 独占打开，无任何测试随改动进入 |

这两条**不作为反例被追溯责难** —— 它们记录的是本规范生效前的惯例。**新改动照流程来**：先落一个能失败的测试，再写实现。

**落地判据**（可机械检查）：一次提交里若出现"新增了行为分支却没有新增/修改对应的 `_test.go`"，就要在提交正文里说明为什么不适用（例：纯文档、纯重命名、`_linux` 专有代码且已在 `make check-linux` 覆盖）。

## 规则 11：测试体按 Arrange-Act-Assert 三段组织

上游 `ecc-036`。现状是既成正例 —— 表驱动用例天然分成「准备用例数据（Arrange）→ 调用被测函数（Act）→ 断言（Assert）」三段。`internal/layout/layout_test.go:45-64` 是完整形态：

```go
func TestAlign4k(t *testing.T) {
	if BlockSize != 4096 {                    // :46-48 前置守卫（见下）
		t.Fatalf("BlockSize=%d want 4096", BlockSize)
	}
	cases := []struct{ in, want int64 }{      // :49-58  Arrange
		{-1, 0},
		...
	}
	for _, c := range cases {                 // :59
		if got := Align4k(c.in); got != c.want {   // :60  Act + Assert 同一行
			t.Fatalf("Align4k(%d)=%d want %d", c.in, got, c.want)
		}
	}
}
```

注意 `:46-48` 是**前置守卫断言**：它先确认 `BlockSize` 这个表所依赖的常量没被改动，再进表。表里多处直接写 `BlockSize`（`:52-55`）而非字面量 4096，守卫就是为这个写法兜底 —— 常量一旦被改，这里立刻失败，而不是让整张表悄悄改变语义。**表里引用了全局常量时照这个形态加一行守卫。**

**不需要写注释标出三段**，靠空行分段即可。**规则**：不要在断言之间夹杂新的 Act —— 若一个用例需要"调用 → 断言 → 再调用 → 再断言"，说明它在测多个行为，拆成两个 `t.Run`。这条与规则 8（表测试保持单层）是同一诉求的两个侧面。

## 规则 12：按包统计覆盖率，目标 ≥80%，越高越好

上游 `ecc-032`（最低测试覆盖率 80%）与 `ecc-018`（用 `go test -cover ./...` 统计）。

**判据是「按包」，不是全仓总百分比。** 一个 48.2% 的包摊进 16 个能测出数字的包里，总均值仍有 90.4% —— 异常会被大包直接抹平。只有按包统计，例外才会显形。

实测（2026-09-22，口径见下）：

| 包 | 覆盖率 | | 包 | 覆盖率 |
|---|---|---|---|---|
| `internal/aio` | **48.2%** ← 唯一低于 80% | | `internal/cluster` | 92.1% |
| `cmd/taihu/cmd` | 82.7% ⚠️ | | `internal/metastore` | 94.1% |
| `cmd/taihu` | 85.7% | | `internal/rpcclient` | 97.2% |
| `internal/device` | 85.9% | | `pkg/taihu-client` | 97.6% |
| `internal/bufpool` | 88.4% | | `internal/benchkit` | 100.0% |
| `internal/transport` | 90.7% | | `internal/layout` | 100.0% |
| `internal/storage` | 91.4% | | `internal/transport/protocol` | 100.0% |
| `examples/taihu-client` | 91.7% | | `internal/version` | 100.0% |
| `internal/ierr` | `[no statements]` | | | |

统计集里 **17 个包**：16 个有覆盖率数字，算术平均 **90.4%**；`internal/ierr` 报 `[no statements]`（该包只有 sentinel 定义与 `errors.Is` 转发，没有可计数的语句），**本条不适用于它**，不要为了让它「有数字」去加测试。

**规则**：新增或改动一个包时，它的覆盖率不得低于 80%；已在 80% 以上的，只许升不许降。**越高越好** —— 80% 是下限不是目标值，上表 15 个包已在此之上，不要拿它当「够了」的挡箭牌。

⚠️ **`cmd/taihu/cmd` 那 82.7% 是在 2 个测试失败的情况下报出来的** —— `TestBenchStorageCmdErrorPaths` 与 `TestBenchSingleCmdShmRoundTrip` 在 macOS 上已知失败（shm 仅 Linux / 非块设备回退路径），**非改动引入**，覆盖率数字照常计入。这不是新发现，仓库早有记录：成因与完整失败清单见 `.trellis/spec/platform/build-verification.md:103-108`，这两个测试的断言行号见 `.trellis/spec/cli/index.md:144-149`。**不要为了「让本机全绿」去改它们。**

统计口径固定为：

```bash
go test -cover $(go list ./... | grep -v third_party | grep -v test/e2e)
```

两个排除项都是必须的，不是图省事：`third_party/` 是 fork 的上游代码，它的测试套件在本机 panic —— `third_party/shmipc-go` 的 `Test_EventDispatcher` 以空指针解引用告终，运行时把位置报在 `event_dispatcher_test.go:126`（`clientConn := d.newConnection(fd)` 那一行）；`test/e2e/` 要真集群（不设 `E2E_PD` 时全部 Skip，见 [index.md](./index.md)）。**这两个包的覆盖率不适用本条规则。**

**这条命令在 macOS 上退出码是 1，这是预期的**，原因就是本条开头 ⚠️ 里那 2 个平台性失败。`go test` 仍会逐包打印结果（`ok` / `FAIL` 行带 `coverage:`），**读输出里的数字，不要只看退出码**。还有个解析陷阱：`cmd/taihu/cmd` 的 `coverage:` 行**不附在** `FAIL\t<pkg>` 那一行末尾，而是被测试二进制的裸 `FAIL` 隔开、单独占一行 —— 按「`ok`/`FAIL` 行」抓取的工具会整个漏掉这个包（本表的第一次统计就是这么漏的）。判定改动是否破坏测试以 Linux 为准，口径见 `.trellis/spec/platform/build-verification.md:108`。

### ⚠️ 已知例外：`internal/aio` 本机测不出真实覆盖率

`internal/aio` 测得 48.2%，但**这个数字不能读成「测试写得少」** —— 它的分母只有 137 条语句，而该包非测试代码有 **1529 行，其中 1017 行（66%）是 Linux 专属**（`aio_linux.go` / `aio_uring_linux.go` / `probe_linux.go`）。三件事叠加，导致本机根本量不到这个包：

1. **Linux 专属文件在 macOS 上不编译，也就不进统计** —— 占该包 66% 的实现连分母都没进。
2. **平台测试带 `_linux_test` 后缀，本机不运行** —— `probe_linux_test.go:1` 就是 `//go:build linux`；后端的 `t.Skipf` 机制见规则 5、规则 6 与 `aio_backends_linux_test.go:9-13` 的说明。
3. **`make check-linux` 只做 `go test -c`**（`Makefile:69`：`go test -c -o /dev/null ./...`），编译测试二进制而**不执行** —— 所以 Linux 侧的真实覆盖率**在本机没有任何一条命令能测出来**。

因此 `aio` 的这个数是**测不到**，不是**不达标**。要拿它的真实覆盖率，得在 Linux 机器上跑 `go test -cover ./internal/aio` 再把结果回填到上表。**在那之前，不要把 48.2% 当成待补的缺口去追。**

---

## 刻意偏离上游规则

本节登记「**明确知道上游怎么说、但 taihu 有意不照做**」的条目。三要素缺一不可：上游主张 / taihu 的做法（带锚点）/ 为什么偏离（具体到可检验）。

**不在这节里的「不遵守」不是偏离，是遗漏** —— 写不出可检验理由的，按缺陷处理。

### 1. 性能测量不在单测层，不写 `go test -bench`

- **用 `go test -bench` 量化性能，配合 `benchstat` 做前后对比。** —— 上游见 `gbp-044`。
  **taihu 的做法**是**不写 benchmark**：自有代码里 `func Benchmark` **零命中**（`grep -rn 'func Benchmark' --include='*.go' cmd internal pkg test examples` 的结果是 0）；仓库里仅有的 16 个全在 fork 里（`third_party/netpoll/` 11 个、`third_party/shmipc-go/` 5 个，见本文规则 3 的表）。性能测量走 `internal/benchkit/` 的自研 harness 与 `test/e2e/` 的 F/G 组 —— 这条约定本身写在本文 `:75`。
  **为什么偏离**：`go test -bench` 的三条硬限制正好卡住本仓库要测的东西 —— ① 它起不了真实的 O_DIRECT / io_uring 设备负载，跑出来的是缓存命中路径；② 它只覆盖单进程，而本仓库的性能问题（在途队列深度、`Ring.ErrFull` 背压、批量提交粒度）要跨进程才复现；③ 它只跑 TCP 或什么都不跑，覆盖不了 shm 与 netpoll 两条数据面。`internal/benchkit` 的 `Config.Pipeline`（`internal/benchkit/run.go:33`）、`-transport` 三选一与 e2e 的 `E2E_PD` 门控，就是为这三条限制准备的。**代价也认**：`benchstat` 那套统计对比本仓库没有对应物，跨提交的性能回归靠人读 `internal/benchkit` 的输出，不靠门禁。

### 2. 不用 testify，断言手写

- **用 testify 让断言更清晰，并区分 `assert` 与 `require`。** —— 上游见 `gbp-046`。
  **taihu 的做法**是手写断言，形态与失败消息惯例见本文规则 2。自有代码对 testify **零引用**（`grep -rl 'testify' --include='*.go' cmd internal pkg test examples` = 0）。
  **为什么偏离**：`go.mod:12` 里的 `github.com/stretchr/testify v1.9.0` 只为 `third_party/shmipc-go/` 的 13 个上游 fork 测试而存在（它们随 `go test ./...` 一起跑）。**这条偏离的理由是可检验的**：手写断言让测试代码的标准库依赖只有 `testing` 一手到底，读测试的人不需要在「这是断言库的行为还是 Go 的行为」之间切换；代价是失败消息要自己写，这条成本由规则 2 的消息惯例（`"<主体>(<入参>)=<got> want <want>"`）抵掉。
  **别被 `go.mod` 骗到**：依赖清单里有 testify ≠ 仓库在用 —— 这是本文规则 2 特意提醒过的判据，也是 `.trellis/spec/guides/index.md` §二记的第 3 种假阳性。
