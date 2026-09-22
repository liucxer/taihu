# 错误体系

> 库错误的唯一事实源是 `internal/ierr/ierr.go:9-22`；跨进程边界传 code 不传字符串；对外的名字经 `internal/rpcclient` → `pkg/taihu-client` 两层 re-export。

---

## 1. `internal/ierr` 是唯一事实源

`internal/ierr/ierr.go:1-5` 的文件头把规则写死了：

```go
// Package ierr 定义 taihu 存储的公共错误 —— **唯一事实源**。
// 内部各层（device/metastore/storage/transport/protocol）直接使用本包错误；
// 面向客户端的 re-export 只有一处：internal/rpcclient/reexport.go（pkg/taihu-client/reexport.go
// 再转指一层）。internal/ 下的包不得自建错误别名层。
package ierr
```

6 个 sentinel 集中声明（`internal/ierr/ierr.go:9-22`），**消息前缀统一 `taihu: `**，除 `ErrConflict` 外都是一句话的下拉英文：

| 符号 | 消息 | 语义 |
|------|------|------|
| `ErrNotFound` | `taihu: key not found` | key 不存在 |
| `ErrInvalidRange` | `taihu: invalid range` | `off<0` 或 `size<0` |
| `ErrTooLarge` | `taihu: object too large, exceeds segment size` | 超单 segment 上限（不许跨段） |
| `ErrNoSpace` | `taihu: no free segment` | 无空闲 segment 可写 |
| `ErrShortWrite` | `taihu: short write` | 设备实际写入少于期望 |
| `ErrConflict` | `taihu: mapping conflict` | CAS 失败，**compaction 内部使用** |

**`ErrConflict` 的注释说明了它是控制信号而非错误**（`ierr.go:20-21`）：`// ErrConflict 表示条件写（CAS）失败：当前映射与期望不符（并发 Put/Delete 竞态），// 调用方应跳过本次操作并重试。compaction 搬移使用。` —— 它不对外。

**`internal/` 下不得再建别名层**：本层各包直接 import `ierr` 使用。实测 `internal/transport/client.go:13`、`internal/storage/storage.go:9`、`internal/device/device.go:27`、`internal/metastore/kv_pebble.go:12`、`internal/transport/protocol/protocol.go:26` 都是直连 `internal/ierr`。

---

## 2. 自定义 error 类型只有一处，且不导出

实测 `grep -rn 'type.*Error struct' --include='*.go' . | grep -v third_party/` 全仓库只有一个命中：`internal/aio/aio_uring_linux.go:399` 的 `type uringParamError struct`。

它是 io_uring 启动自检的诊断载体 —— 目的是**把"哪个字段对不上"带出来**，而不是给调用方做类型断言：

```go
// uringParamError 内核回填的 ring 参数不合理（通常意味着结构体布局与 UAPI 不符）。
type uringParamError struct {
	field string
	got   uint32
}
```

**命名**：自定义错误类型一律以 `Error` 结尾（`uringParamError`，上游 `uber-031`）。本仓库的唯一实例已合规，新增时照此 —— 不要用 `Err` 前缀命名**类型**（`ErrXxx` 是**值**的命名法，见第 3 节的 sentinel）。

**结论**：不要新增导出的自定义 error 类型。需要区分错误类别时，用 `internal/ierr` 的 sentinel + `errors.Is`；需要携带诊断细节时，用未导出类型（像 `uringParamError`）或 `fmt.Errorf` 带上下文。

---

## 3. 包内控制信号用未导出 sentinel

不跨包、不跨进程的控制信号一律小写未导出，避免污染包的对外面。三个实例：

| 位置 | 声明 | 用途 |
|------|------|------|
| `internal/aio/aio_internal.go:18` | `var errInvalidMaxEvents = errors.New("aio: maxEvents must be in [1, 65536]")` | 参数校验失败 |
| `internal/transport/frame.go:34` | `var errConnClosed = errors.New("taihu: connection closed")` | 读循环发现连接已关 |
| `internal/device/device.go:48` | `var errDeviceClosed = errors.New("taihu: device closed")` | 关闭后仍提交 IO |

命名一律 `err` 前缀（无 `Err` 大写）—— Go 里未导出标识符本就小写，用 `errXxx` 与导出的 `ErrXxx` 一眼区分。**落点**也一并受约束：未导出 sentinel 不进包的对外面文件，`errInvalidMaxEvents` 因此在包内私有的 `aio_internal.go` 而非 `aio.go`（见 [api-surface.md](./api-surface.md) 规则 4）。

注意 `internal/transport/frame.go:34` 这三个里唯一**没有**文档注释的 —— 不强制每条 sentinel 都配注释，但当名字不足以自解释时必须写（`errInvalidMaxEvents` 的注释就写了为什么：`// errInvalidMaxEvents 表示 NewWithOptions 的 maxEvents 超出内核允许范围。`）。

包内私有的不止"错误"，还包括跨包重复的 sentinel：`internal/transport/server_shm_other.go:15` 与 `internal/rpcclient/dial_shm_other.go:12` 各自声明了一个 `errShmUnsupported`，消息都是 `taihu: shmipc only supported on linux` —— **两个包各持一份是刻意的**，因为它们是不同层的平台桩，行为独立。

---

## 4. 跨进程边界传 code 不传字符串

错误过 wire 时只传 4 字节大端 code，不传 error 字符串 —— 字符串会随措辞变化破坏兼容，code 是稳定契约。三个函数都在 `internal/transport/protocol/protocol.go`：

**`ErrCode` 枚举**（`protocol.go:99-110`）：`CodeOK=0`、`CodeNotFound=1`、`CodeInvalidRange=2`、`CodeTooLarge=3`、`CodeNoSpace=4`、`CodeInternal=5`、`CodeInvalidArgument=6`。

**`MapStorageErr(err) ErrCode`**（`protocol.go:112-126`）—— **服务端出口**，用 `errors.Is` 逐条映射：

```go
// MapStorageErr 将库错误映射为错误码（对应旧 gRPC status 映射）。
func MapStorageErr(err error) ErrCode {
	switch {
	case errors.Is(err, ierr.ErrNotFound):
		return CodeNotFound
	...
	default:
		return CodeInternal
	}
}
```

注意映射表里**没有 `ErrShortWrite` 和 `ErrConflict`** —— 前者落到 `default: CodeInternal`，后者本就不出引擎（见第 1 节）。客户端因此收不到 `ErrShortWrite` 这个具体标识。

**`MapCode(c) error`**（`protocol.go:128-146`）—— **客户端入口**，把 code 还原成 sentinel。`CodeInvalidArgument` / 未知 code 没有对应 sentinel，就地 `errors.New`（`protocol.go:142,144`）。

**`EncCode(c) []byte`**（`protocol.go:148-153`）—— 4 字节大端编码。

用法边界在 grep 里非常干净：`MapStorageErr` + `EncCode` 只出现在服务端（`internal/transport/server.go:226,237,276,280,316,357,384,406`、`server_admin.go`、`server_shm_linux.go`），`MapCode` 只出现在客户端（`internal/transport/client.go:106,225,253,280`、`client_admin.go:50,100,135`、`client_shm_linux.go`）。**不要越过这条线**：服务端不该 `MapCode`，客户端不该 `MapStorageErr`。

---

## 5. 对外 re-export 只走两层，且**故意少一个**

链路是 `internal/ierr` → `internal/rpcclient/reexport.go` → `pkg/taihu-client/reexport.go`。

`internal/rpcclient/reexport.go:27-42` 逐个列名，并解释了为什么**不含** `ErrConflict`：

```go
// 客户端可见的库错误（唯一定义在 internal/ierr；此处 re-export 以便外部
// errors.Is(err, rpcclient.ErrNotFound) 判断）。
//
// 不含 ierr.ErrConflict：那是 compaction 内部的 CAS 控制信号，不是客户端会收到的错误。
var (
	// ErrNotFound 表示 key 不存在。
	ErrNotFound = ierr.ErrNotFound
	...
```

第二层 `pkg/taihu-client/reexport.go:29-31` 只做转发、不新增，理由写在注释里：`// 定义在 internal/ierr，internal/rpcclient 已 re-export，此处再指一层以保持单一来源。`

**同样是 5 个，同样不含 `ErrConflict`**。加错误时必须两层一起加，否则外部调用方拿到一个无法命名的错误值。

## 6. 内部错误不 re-export 是有意为之

`internal/rpcclient/putwriter.go:11` 的 `ErrShmOnly`（`taihu: zero-copy write requires shm connection`）**不在 re-export 名单里** —— 因为零拷贝写的 `NewPut` / `PutWriter` 本身也没暴露给 SDK：实测 `grep -rn 'PutWriter\|NewPut\|ErrShmOnly' pkg/taihu-client/*.go | grep -v '_test.go'` 无输出。

**规则**：re-export 名单跟着导出签名走。`internal/rpcclient/reexport.go:21-22` 的原话是「凡是本包导出函数签名或导出结构体字段里出现 internal 类型，就到这里补一条 alias」—— 对错误同理：SDK 调得到、能收到的错误才需要名字。

对照 `pkg/taihu-client/storage.go:16-21` 里 SDK **自己**定义的 `ErrNoInstances`（导出，集群无在线实例可写）与 `errSourceUnset`（未导出，内部选路控制信号）—— 这层自己的错误自己定义，不往上借。

---

## 7. 补 alias 的编译期兜底

`internal/rpcclient/reexport.go:22-25` 记录了这条约定为什么需要一个测试兜着：`// 签名里出现新 internal 类型而这里忘了补时**编译不会报错**，只是包对外悄悄不可用，故 internal/rpcclient/api_test.go 用外部测试包逐个命名这些类型，作为编译期的兜底。`

实测兜底文件是 `internal/rpcclient/api_test.go:1,8`：声明 `package rpcclient_test`（外部测试包）并 import 本包，逐个命名 re-export 出来的类型与常量。**改 re-export 时要同步改它**，否则兜底就失效了。

---

## 8. 同一个错误只处理一次：不要既 log 又 return

上游 `uber-032`：错误通常只应被处理一次 —— 既 log 又 return 会让上游调用方重复处理，日志里同一错误出现多遍。

实测**全仓库只有一处**把 error 交给 `log`（非测试代码）：

```bash
grep -rnE 'log\.Printf?n?\(.*\berr\b' --include='*.go' internal pkg | grep -v '_test.go'
# 仅 internal/storage/compact.go:72
```

`internal/storage/compact.go:63-78` 的 `run()`：

```go
moved, err := c.compactOnce(context.Background())
if moved > 0 || err != nil {
    log.Printf("compaction: moved=%d err=%v", moved, err)
}
```

**这一处不是违规**：`run()` 是后台 goroutine 的 `for/select` 主循环，**没有调用方可返回** —— 它属于第 9 节的「可恢复则 log 并优雅降级」分支。判据是**有没有调用方**：有调用方就 `return err` 让上层处理；没有（后台 goroutine / 收尾路径）才 log。

`cmd/taihu/cmd/server.go` 的若干 `log.Printf` 同理 —— 进程入口日志是运维通道，不是"把错误交给上层"的替代品。

## 9. 四种处置方式，按契约选

上游 `uber-033` 给了一张处置表，落到 taihu 是：

| 处境的判据 | 处置 | taihu 的写法 |
|------------|------|--------------|
| 函数契约**定义了**特定错误 | `errors.Is` / `errors.As` 匹配后分支 | `internal/transport/protocol/protocol.go:112-126` 的 `MapStorageErr`（`errors.Is` 逐条映射 sentinel）；`internal/aio/aio_other.go:133` 的 `errors.As(err, &errno)` 取回 `syscall.Errno` |
| 可恢复，无调用方可报 | log 并优雅降级 | `internal/storage/compact.go:72`（见第 8 节） |
| 属于本领域定义好的失败条件 | 返回 `internal/ierr` 的 sentinel | `internal/ierr/ierr.go:9-22` 的 6 个，见第 1 节 |
| 以上都不是 | 包装（`%w`）或原样返回 | `internal/cluster/kv_tikv.go` 等 37 处（见第 10 节） |

**taihu 相对上游的一处结构性偏离**：上游默认错误在**同进程**内流动，所以 `errors.Is` 是主要判据。taihu 的错误要过进程边界，跨边界传的是 **code 不是 error 值**（第 4 节）。因此：

- **进程内**（`internal/` 各层之间）照上表用 `errors.Is`
- **跨进程**（server ↔ client）走 `MapStorageErr` / `MapCode` 的 code 映射，客户端拿到的是 `MapCode` 还原出的 sentinel

这是刻意偏离，不是漏做；理由是可验证的（wire 格式见第 4 节）。

## 10. `%w` 是默认选择，且被包装的错误是契约的一部分

上游 `uber-026`（原文自我声明为 "a good default for most wrapped errors"，不是唯一选择）与 `uber-027`。

实测非测试代码 37 处 `: %w"` 包装，分布在 14 个文件；**库侧只有 8 处**（`internal/cluster/kv_tikv.go` 2、`internal/metastore/kv_pebble.go` 2、`internal/device/device.go` 2、`internal/aio/aio.go` 1、`internal/aio/aio_uring_linux.go` 1），其余 **29 处在 `cmd/taihu/cmd/`**。

**为什么库侧少**：库代码的错误大多是本领域定义好的 sentinel，直接返回即可（第 9 节第三行），不需要包。包装集中在 CLI —— 那里是把库错误转成给操作人看的上下文。

规则的后半句更要紧：**凡是用 `%w` 包过的错误，就成为函数契约的一部分，要文档化并测试**。taihu 在这一条上是**既成正例**，证据是测试里大量直接断言 sentinel：

```bash
grep -rnE 'errors\.Is|ierr\.Err[A-Z][a-z]+' --include='*_test.go' internal pkg cmd | wc -l
# 149
```

`internal/transport/transport_shm_test.go:210,223` 是典型形态 —— 断言客户端收到的具体 sentinel：

```go
if _, _, err := c.Get(ctx, "missing-key", 0, -1); !errors.Is(err, ierr.ErrNotFound) {
if err := c.Put(ctx, "short", 10, []byte{1, 2}); !errors.Is(err, ierr.ErrShortWrite) {
```

**新增可包装错误时**：在 `internal/ierr` 定义 sentinel（带文档注释，照 `internal/ierr/ierr.go:9-22` 的形态）→ 在 `MapStorageErr` 补映射（否则客户端收不到具体标识，第 4 节）→ 在两层 re-export 补 alias（第 5 节）→ 加一条 `errors.Is` 断言的测试。

---

## 刻意偏离上游规则

本节登记「**明确知道上游怎么说、但 taihu 有意不照做**」的条目。三要素缺一不可：上游主张 / taihu 的做法（带锚点）/ 为什么偏离（具体到可检验）。

**不在这节里的「不遵守」不是偏离，是遗漏** —— 写不出可检验理由的，按缺陷处理。

### 1. 错误判据的主入口是 code，不是 `errors.Is`

- **用 `errors.Is` / `errors.As` 作为错误的判定与分支手段。** —— 上游见 `uber-033`（本文第 9 节的处置表即由它落成）。
  **taihu 的做法**把判据**分成两半**：同一进程内照上游用 `errors.Is`（`internal/transport/protocol/protocol.go:112-126` 的 `MapStorageErr` 就是逐条 `errors.Is` 映射）；一旦过进程边界，传的是 **4 字节 code 而不是 error 值**，客户端侧由 `MapCode` 还原成 sentinel —— 映射的两端见第 4 节，`reexport` 的两层见第 5 节。
  **为什么偏离**：上游规则默认错误在**同进程内**流动 —— 在那里 `errors.Is` 是完备的判据。本仓库的 `server` 与 `client` 是两个进程，`error` 值本身过不去，只能传可枚举的 code。这不是「不想用 `errors.Is`」，而是**跨进程时它无法作为判据**；同进程内它仍是主入口。展开见本文第 9 节末段。
  **新增错误时的代价**（必须一起做，漏了不报错）：在 `internal/ierr` 定义 sentinel → 在 `MapStorageErr` 补映射 → 在两层 re-export 补 alias → 加 `errors.Is` 断言的测试。清单见第 10 节末。
