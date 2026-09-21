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

**结论**：不要新增导出的自定义 error 类型。需要区分错误类别时，用 `internal/ierr` 的 sentinel + `errors.Is`；需要携带诊断细节时，用未导出类型（像 `uringParamError`）或 `fmt.Errorf` 带上下文。

---

## 3. 包内控制信号用未导出 sentinel

不跨包、不跨进程的控制信号一律小写未导出，避免污染包的对外面。三个实例：

| 位置 | 声明 | 用途 |
|------|------|------|
| `internal/aio/aio.go:45` | `var errInvalidMaxEvents = errors.New("aio: maxEvents must be in [1, 65536]")` | 参数校验失败 |
| `internal/transport/frame.go:34` | `var errConnClosed = errors.New("taihu: connection closed")` | 读循环发现连接已关 |
| `internal/device/device.go:48` | `var errDeviceClosed = errors.New("taihu: device closed")` | 关闭后仍提交 IO |

命名一律 `err` 前缀（无 `Err` 大写）—— Go 里未导出标识符本就小写，用 `errXxx` 与导出的 `ErrXxx` 一眼区分。

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
