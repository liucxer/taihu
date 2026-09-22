# error —— 错误处理（gbp 类 4）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/error-*.md`，6 条。
> 5 条适用 / 1 条部分适用。

## 本仓库的错误模型（先读这段）

taihu 的错误只有一个事实源：**`internal/ierr`**。该包的文件头原文写着：

> Package ierr 定义 taihu 存储的公共错误 —— **唯一事实源**。
> 内部各层（aio/device/metastore/storage/transport/protocol）直接使用本包错误；
> 面向客户端的 re-export 只有一处：`internal/rpcclient/reexport.go`（`pkg/taihu-client/reexport.go` 再转指一层）。**`internal/` 下的包不得自建错误别名层。**

当前共 8 个 sentinel，全部在 `internal/ierr/ierr.go:10-27`：`ErrNotFound`、`ErrInvalidRange`、`ErrTooLarge`、`ErrNoSpace`、`ErrShortWrite`、`ErrConflict`、`ErrFull`、`ErrTimeout`。

**改动错误时第一条要问的**：这个错误该不该进 `ierr`？如果它要跨包被 `errors.Is` 判断，就进；如果只是本包内部的失败信号，就地 `fmt.Errorf` 包装。

### 错误字符串的形态

sentinel 的文案是**小写英文、带包名前缀、无结尾句号**：

```go
ErrNotFound = errors.New("taihu: key not found")
ErrTooLarge = errors.New("taihu: object too large, exceeds segment size")
```

包装前缀同理，形如 `fmt.Errorf("op: %w", err)`——小写、无句号。**这是 gbp-030 与本仓库一致的约定**，见 [idiomatic/](../idiomatic/index.md) 的 gbp-030。

核实（**注意口径：这两个数字差在测试文件上，引用时不要混**）：

| 项 | 非测试 | 含测试 | 命令（非测试口径） |
|---|---|---|---|
| `fmt.Errorf`（`internal/`） | **56** | 65 | `grep -rn 'fmt.Errorf(' --include='*.go' internal \| grep -v _test.go` |
| `errors.Is` / `errors.As` | **6** | **98** | `grep -rnE 'errors\.(Is\|As)\(' --include='*.go' internal cmd pkg \| grep -v _test.go` |

`errors.Is/As` 的 6 与 98 差得悬殊，因为**绝大多数 `errors.Is` 断言写在测试里**（测试要验证错误确实可判定）——这恰恰是 [testing/](../testing/index.md) 该有的样子。**不要引用 98 来说明「生产代码大量使用 errors.Is」**，那是含测试的数。

## 逐条裁决

### gbp-016 · Error Wrapping and Context（HIGH）— **适用**

**规则**：用 `fmt.Errorf` + `%w` 包装错误保留完整调用链；包含操作名与关键参数；调用方用 `errors.Is()` 判类型、`errors.As()` 取值。

**对 taihu：适用，且是本仓库的主流做法**（非测试代码 56 处 `fmt.Errorf`；`errors.Is/As` 的口径与数字见本层开头）。

**要点**：`%w` 而不是 `%v`。用 `%v` 会把错误链打断，调用方的 `errors.Is` 就失效了——本仓库大量依赖跨层 `errors.Is`（`device` → `metastore` → `storage` → `transport`），打断一次就有一层判不出根因。

### gbp-017 · Sentinel Error Definition（MEDIUM）— **适用（本仓库更强）**

**规则**：为调用方可判断的错误条件定义包级 sentinel。

**对 taihu：适用，但强于规则本身。** gbp 只说「定义包级 sentinel」，本仓库进一步规定**sentinel 只有一处**：`internal/ierr`。这是比「每个包自己定义」更严的约束——它换来的是**跨层 `errors.Is` 的可判定性**：任何一个包拿到 `error` 都能用同一组 sentinel 判断，不必知道对方内部定义了什么。

**因此新增 sentinel 的判据是**：它需要跨包/跨层被判断吗？需要 → 进 `ierr`。不需要 → 不要新建 sentinel，用 `fmt.Errorf` 包装即可。

**「进 `ierr`」与「re-export 给客户端」是两件事，别只做一半。** 进 `ierr` 的判据是**本仓库内部**有人跨包判断它；能不能被 SDK 调用方命名，由 `internal/rpcclient/reexport.go` 那份**显式白名单**另外决定。当前 8 个 sentinel 只 re-export 了 5 个：

| sentinel | 进 `ierr` | re-export | 差额的理由 |
|---|---|---|---|
| `ErrNotFound` / `ErrInvalidRange` / `ErrTooLarge` / `ErrNoSpace` / `ErrShortWrite` | ✅ | ✅ | 会作为 `Storage` 方法返回值到达调用方 |
| `ErrConflict` | ✅ | ❌ | compaction 内部的 CAS 控制信号，客户端收不到（`internal/rpcclient/reexport.go:30` 已注明） |
| `ErrFull` / `ErrTimeout` | ✅ | ❌ | `device` ↔ `aio` 的流控信号：只在重试判断里被 `==` 比较（`internal/device/device.go:221`、`:521`），从不出现在任何导出签名里 |

**所以新增 sentinel 要问两步**：内部有没有人跨包判断它（决定进不进 `ierr`）、客户端会不会收到它（决定进不进白名单）。只做第一步，SDK 调用方就 `errors.Is` 不了；只做第二步而不进 `ierr`，内部又立了第二个事实源。

**判断「客户端会不会收到」的方法**：看它落在哪个函数里。落在**未导出**函数（如 `Device.pump`）或**导出函数的重试分支**（如 `AppendBatch` 命中 `ErrFull` 后 `continue` 而不 `return`）里的，客户端收不到。2026-09-22 把 `ErrFull` / `ErrTimeout` 从 `internal/aio` 收进 `ierr` 时，正是按这条判据确认**不需要**动 re-export 白名单的。

**例外**：`pkg/taihu-client/storage.go:18` 的 `ErrNoInstances` 在 SDK 包内，不在 `internal/` 的管辖范围（`ierr` 的禁令写的是「`internal/` 下的包不得自建错误别名层」）——它表达的是**客户端侧的部署状态**（没有在线实例），不是存储语义，不该进 `ierr`。

### gbp-018 · Custom Error Types（MEDIUM）— **部分适用**

**规则**：需要携带额外信息时定义自定义错误类型，用 `errors.As` 提取。

**对 taihu：部分适用——机制适用，形态少见。** 本仓库全仓只有**一个**自定义 error 类型：`internal/aio/aio_uring_linux.go:406-409` 的 `uringParamError`（`:411` 是它的 `Error()` 方法）。

这不是「还没做」。本仓库的绝大多数错误信息走**包装链**（`fmt.Errorf("op: %w", err)` 层层加前缀）而不是**携带字段的错误类型**，因为：

- 跨进程边界的错误**不传字符串**，传的是 code（`internal/transport/protocol` 的 `ErrCode` 枚举 + `MapStorageErr` / `MapCode`）。需要携带结构化信息的场景已经被这套机制覆盖了。
- 进程内的错误要么是需要 `errors.Is` 判断的 sentinel（进 `ierr`），要么是需要人读的上下文（`fmt.Errorf` 包装）。

**所以新增自定义错误类型的判据**：只有当错误需要携带**调用方要按字段读取的结构化数据**、且这些数据**不跨进程边界**时才用。能进 `ierr` 或能包装的，不要新造类型。

### gbp-019 · Always Check Error Returns（CRITICAL）— **适用**

**规则**：永远不要忽略 error 返回值；真正不需要时用 `_` 并**加注释说明**。反例是 `data, _ := fetchData()`；正例给出 `_ = conn.Close() // 已日志` 这个例外形态。

**对 taihu：适用。** 判据同 gbp-019 的原文：**丢弃是被允许的，缺的是那行理由**。

**特别提醒本仓库的一类高频场景**：清理路径（`Close` / `Munmap` / `Unmap`）的错误丢弃。这类丢弃**仍需理由**，通用理由可以是「清理失败的处置动作不存在，且错误已由主返回值表达」——但**同一个文件里第一次出现时必须写出来**，后续同类可省。

### gbp-020 · API Error Response Standards（HIGH）— **不适用（载体不对应）**

**规则**：定义统一的 API 错误响应结构体与错误码；正例是 `ErrorResponse{Code,Message,Details}` + Gin 中间件按 `errors.As/Is` 分派。

**对 taihu：不适用（载体是 HTTP/JSON）。** 本仓库无 REST 接口、无 JSON 错误响应体。

**但本仓库有更强的对应物，且不要把它当成「已经有了就不用管」**：跨进程边界**传 code 不传字符串**——`ErrCode` 枚举 + `MapStorageErr` / `MapCode` 在 `internal/transport/protocol/protocol.go`。它比 HTTP 状态码映射更严，因为：

- 字符串跨进程会因版本、语言环境、拼写差异而无法判定；code 是编译期常量。
- 服务端不 `MapCode`、客户端不 `MapStorageErr`——两侧各管自己的方向，不越界。

**不适用的是它的具体形态**（HTTP 状态码、JSON body、中间件），**不是它的意图**。改传输层错误时按 `protocol` 的机制来，不要引入 `ErrorResponse` 结构体。

### gbp-021 · Panic and Recover Usage Guidelines（HIGH）— **适用**

**规则**：`panic` 仅用于不可恢复错误（init 校验、`MustCompile`、`unreachable()`）；`recover` 只在 `defer` 中有效；不要在包边界外暴露 panic。

**对 taihu：适用。** 非测试代码不得 `panic()` ——解析失败、设备故障、格式不符这些都是**可预期**的运行时状况，必须走 error 返回，不能 panic。

**注意与规则 028（race detection）的边界**：`recover` 不是用来兜 bug 的。若 `recover` 捕获的是本可返回 error 的情况，那是把错误处理当成了异常处理。
