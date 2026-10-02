# error —— 错误处理（gbp 类 4）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/error-*.md`，6 条。
> 5 条适用 / 1 条部分适用。

## 本仓库的错误模型（先读这段）

taihu 的错误只有一个事实源：**`pkg/ierr`**（2026-09-24 自 `internal/ierr` 迁入 `pkg/ierr`，全仓错误统一收敛于此）。该包的文件头原文写着：

> Package ierr 定义 taihu 存储的公共错误 —— **唯一事实源**。
> 内部各层（aio/device/metastore/storage/transport/protocol）与对外 SDK（pkg/taihu-client）直接使用本包错误；包位于 pkg/ 下，外部调用方可用 errors.Is 判定同一批哨兵。**`internal/` 下的包不得自建错误别名层。**

当前共 26 个 sentinel，全部在 `pkg/ierr/ierr.go`：`ErrNotFound`、`ErrInvalidRange`、`ErrTooLarge`、`ErrNoSpace`、`ErrShortWrite`、`ErrConflict`、`ErrFull`、`ErrTimeout`、`ErrInvalidMaxEvents`、`ErrUringLinuxOnly`、`ErrIOPOLLLinuxOnly`、`ErrInvalidArgument`、`ErrRPCError`、`ErrKeyTooLong`、`ErrConnClosed`、`ErrShmBadFrame`、`ErrShmStreamBroken`、`ErrShmUnsupported`、`ErrShmOnly`、`ErrAdminShmOnly`、`ErrStorageClosed`、`ErrDeviceClosed`、`ErrNoInstances`、`ErrSourceUnset`、`ErrKVRequired`、`ErrNoShmAddr`。

**为什么在 pkg/ 而不是 internal/**：SDK 库代码直接 import 它（`pkg/taihu-client/storage.go:14`），外部调用方用同一批哨兵做 `errors.Is`；它也是 `check-layering` 门禁允许 `internal/` 依赖 `pkg/` 的**唯一例外**（见 [lint/](../lint/index.md) 与 Makefile 注释）。

**改动错误时第一条要问的**：这个错误该不该进 `ierr`？如果它要跨包被 `errors.Is` 判断，就进；如果只是本包内部的失败信号，就地 `fmt.Errorf` 包装。

### 错误字符串的形态

sentinel 的文案是**小写英文、带包名前缀、无结尾句号**：

```go
ErrNotFound = errors.New("taihu: key not found")
ErrTooLarge = errors.New("taihu: object too large, exceeds segment size")
```

包装前缀同理，形如 `fmt.Errorf("op: %w", err)`——小写、无句号。**这是 gbp-030 与本仓库一致的约定**，见 [idiomatic/](../idiomatic/index.md) 的 gbp-030。

核实（**注意口径：这两个数字差在测试文件上，引用时不要混**；2026-10-02 重测）：

| 项 | 非测试 | 含测试 | 命令（非测试口径） |
|---|---|---|---|
| `fmt.Errorf`（`internal/`） | **81** | 91 | `grep -rn 'fmt.Errorf(' --include='*.go' internal \| grep -v _test.go` |
| `errors.Is` / `errors.As`（`internal` `cmd` `pkg`） | **25** | **130** | `grep -rnE 'errors\.(Is\|As)\(' --include='*.go' internal cmd pkg \| grep -v _test.go` |

`errors.Is/As` 的 25 与 130 差得悬殊，因为**大多数 `errors.Is` 断言写在测试里**（测试要验证错误确实可判定）——这恰恰是 [testing/](../testing/index.md) 该有的样子。**不要引用 130 来说明「生产代码大量使用 errors.Is」**，那是含测试的数。非测试的 25 处里，管理面 HTTP 的 `apiStatus`（`internal/web/web.go:271-282`）与设备重试路径（`internal/device/device.go:146`、`:249`、`:584`）是新增的两类使用方。

## 逐条裁决

### gbp-016 · Error Wrapping and Context（HIGH）— **适用**

**规则**：用 `fmt.Errorf` + `%w` 包装错误保留完整调用链；包含操作名与关键参数；调用方用 `errors.Is()` 判类型、`errors.As()` 取值。

**对 taihu：适用，且是本仓库的主流做法**（非测试代码 81 处 `fmt.Errorf`；`errors.Is/As` 的口径与数字见本层开头）。

**要点**：`%w` 而不是 `%v`。用 `%v` 会把错误链打断，调用方的 `errors.Is` 就失效了——本仓库大量依赖跨层 `errors.Is`（`device` → `metastore` → `storage` → `transport`），打断一次就有一层判不出根因。

### gbp-017 · Sentinel Error Definition（MEDIUM）— **适用（本仓库更强）**

**规则**：为调用方可判断的错误条件定义包级 sentinel。

**对 taihu：适用，但强于规则本身。** gbp 只说「定义包级 sentinel」，本仓库进一步规定**sentinel 只有一处**：`pkg/ierr`。这是比「每个包自己定义」更严的约束——它换来的是**跨层 `errors.Is` 的可判定性**：任何一个包拿到 `error` 都能用同一组 sentinel 判断，不必知道对方内部定义了什么。

**因此新增 sentinel 的判据是**：它需要跨包/跨层被判断吗？需要 → 进 `ierr`。不需要 → 不要新建 sentinel，用 `fmt.Errorf` 包装即可。

**「进 `ierr`」与「re-export 给客户端」是两件事，别只做一半。** 进 `ierr` 的判据是**本仓库内部**有人跨包判断它；能不能被 SDK 调用方命名，由 `internal/rpcclient/reexport.go` 那份**显式白名单**另外决定。当前 26 个 sentinel 只 re-export 了 5 个（`internal/rpcclient/reexport.go:32-43`，经 `pkg/taihu-client/reexport.go:41-52` 再转一层）：

| sentinel | 进 `ierr` | re-export | 差额的理由 |
|---|---|---|---|
| `ErrNotFound` / `ErrInvalidRange` / `ErrTooLarge` / `ErrNoSpace` / `ErrShortWrite` | ✅ | ✅ | 会作为 `Storage` 方法返回值到达调用方 |
| `ErrConflict` | ✅ | ❌ | compaction 内部的 CAS 控制信号：`internal/metastore/kv_pebble.go:448` 产生、`internal/storage/compact.go:122` 用 `errors.Is` 捕获后跳过该对象，客户端收不到（`internal/rpcclient/reexport.go:31` 已注明） |
| `ErrFull` / `ErrTimeout` | ✅ | ❌ | `aio` ↔ `device` 的流控信号：只在重试判断里被 `errors.Is` 比较（`internal/device/device.go:146`、`:249`、`:584`），从不出现在任何导出签名、re-export 白名单或管理面响应中 |

**所以新增 sentinel 要问两步**：内部有没有人跨包判断它（决定进不进 `ierr`）、客户端会不会收到它（决定进不进白名单）。只做第一步，SDK 调用方就 `errors.Is` 不了；只做第二步而不进 `ierr`，内部又立了第二个事实源。

**判断「客户端会不会收到」的方法**：看它落在哪个函数里。落在**未导出**函数（如 `Device.pump`）或**导出函数的重试分支**（如 `AppendBatch` 命中 `ErrFull` 后 `continue` 而不 `return`）里的，客户端收不到。2026-09-22 把 `ErrFull` / `ErrTimeout` 从 `internal/aio` 收进 `ierr` 时，正是按这条判据确认**不需要**动 re-export 白名单的。

**管理面 web 不构成第三个事实源。** `internal/web` 直接 import `pkg/ierr`，用 `apiStatus`（`internal/web/web.go:271-282`）把同一批 sentinel 映射成 HTTP 状态码——它是这些错误的又一个**呈现层**，不定义新错误。详见 gbp-020。

**例外**：`pkg/taihu-client/storage.go` 的 `ErrNoInstances` 在 SDK 包内，不在 `internal/` 的管辖范围（`ierr` 的禁令写的是「`internal/` 下的包不得自建错误别名层」）——它表达的是**客户端侧的部署状态**（没有在线实例），不是存储语义，不该进 `ierr`。

### gbp-018 · Custom Error Types（MEDIUM）— **部分适用**

**规则**：需要携带额外信息时定义自定义错误类型，用 `errors.As` 提取。

**对 taihu：部分适用——机制适用，形态少见。** 本仓库全仓只有**一个**自定义 error 类型：`internal/aio/ring_uring_linux.go:230` 的 `uringParamError`（`:235` 是它的 `Error()` 方法）。

这不是「还没做」。本仓库的绝大多数错误信息走**包装链**（`fmt.Errorf("op: %w", err)` 层层加前缀）而不是**携带字段的错误类型**，因为：

- 跨进程边界的错误**不传字符串**，传的是 code（`internal/transport/protocol` 的 `ErrCode` 枚举 + `MapStorageErr` / `MapCode`）。需要携带结构化信息的场景已经被这套机制覆盖了。
- 进程内的错误要么是需要 `errors.Is` 判断的 sentinel（进 `ierr`），要么是需要人读的上下文（`fmt.Errorf` 包装）。

**所以新增自定义错误类型的判据**：只有当错误需要携带**调用方要按字段读取的结构化数据**、且这些数据**不跨进程边界**时才用。能进 `ierr` 或能包装的，不要新造类型。

### gbp-019 · Always Check Error Returns（CRITICAL）— **适用**

**规则**：永远不要忽略 error 返回值；真正不需要时用 `_` 并**加注释说明**。反例是 `data, _ := fetchData()`；正例给出 `_ = conn.Close() // 已日志` 这个例外形态。

**对 taihu：适用。** 判据同 gbp-019 的原文：**丢弃是被允许的，缺的是那行理由**。

**特别提醒本仓库的一类高频场景**：清理路径（`Close` / `Munmap` / `Unmap`）的错误丢弃。这类丢弃**仍需理由**，通用理由可以是「清理失败的处置动作不存在，且错误已由主返回值表达」——但**同一个文件里第一次出现时必须写出来**，后续同类可省。

### gbp-020 · API Error Response Standards（HIGH）— **适用（形态刻意简化）**

**规则**：定义统一的 API 错误响应结构体与错误码；正例是 `ErrorResponse{Code,Message,Details}` + Gin 中间件按 `errors.As/Is` 分派。

**对 taihu：适用 —— 管理面 HTTP 服务端（`internal/web`）是这条规则的真实载体，但只实现了简化形态。** 2026-10-02 前本条判「不适用（载体是 HTTP/JSON）」，随着 commit `6ae80b5` 引入 `internal/web`，该裁决失效。

现状（`internal/web/web.go:254-282`）：

- 成功/失败统一走 `writeJSON` / `writeErr`，错误体只有 **Message**：`{"error":"..."}`；
- 「错误码」由 **HTTP 状态码**承担，不另设 Code 字段：`apiStatus` 用 `errors.Is` 分派——`ErrNotFound→404`、`ErrInvalidRange→400`、`ErrNoSpace/ErrTooLarge/ErrShortWrite→422`、其余→`502`；
- 没有 `Details` 结构、没有错误中间件（分派是每个 handler 显式调 `writeErr(w, apiStatus(err), err)`，见 `internal/web/handlers.go:31-40`）。

**为什么不照搬 `ErrorResponse{Code,Message,Details}`**：

1. 管理面只有两类消费者——运维人员读 `error` 字符串、内嵌前端按 HTTP 状态码分支；没有跨语言客户端需要稳定的业务错误码枚举；
2. 机器可判定的错误身份在**进程内**仍是 `pkg/ierr` 哨兵（`apiStatus` 就是查它们），在**数据面跨进程**仍是 `internal/transport/protocol` 的 `ErrCode` 枚举 + `MapStorageErr` / `MapCode`。管理面 HTTP 不承担跨进程错误契约，不必再发明一层 code；
3. `Details` 的需求（字段级校验信息等）尚未出现；真出现时再扩响应体，比预先背一个三字段结构便宜。

**新增管理面端点时两条必须守**：错误一律经 `writeErr` + `apiStatus`，不要在 handler 里手写 `w.WriteHeader`；新增可判定错误时同步在 `apiStatus` 登记状态码——漏登记的错误一律落到 502，前端无法区分「客户端参数错」与「后端失败」。

### gbp-021 · Panic and Recover Usage Guidelines（HIGH）— **适用**

**规则**：`panic` 仅用于不可恢复错误（init 校验、`MustCompile`、`unreachable()`）；`recover` 只在 `defer` 中有效；不要在包边界外暴露 panic。

**对 taihu：适用。** 非测试代码不得 `panic()` ——解析失败、设备故障、格式不符这些都是**可预期**的运行时状况，必须走 error 返回，不能 panic。

**允许的形态有正例**：`internal/web/web.go:227` 的 `panic(fmt.Sprintf("embed static: %v", err))` —— `//go:embed static` 的目标在编译期固定，`fs.Sub` 失败只可能是装配写错（不可恢复的 init 不变量），与 `MustCompile` 同类。

**注意与规则 028（race detection）的边界**：`recover` 不是用来兜 bug 的。若 `recover` 捕获的是本可返回 error 的情况，那是把错误处理当成了异常处理。
