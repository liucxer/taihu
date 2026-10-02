# framework —— Web 框架选型（gbp 类 1）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/framework-*.md`，4 条。
> 2026-10-02 重裁：本层曾整层判「不适用」，前提是「taihu 无 HTTP 服务端」。该前提已被
> commit `6ae80b5`（`feat(web): 新增集群管理 Web 界面`）推翻 —— `internal/web/` 是一个
> 正经的 HTTP 服务端。**但数据面结论不变**，见下。

## 两个面要分开

| 面 | 形态 | 与 gbp 本类的关系 |
|---|---|---|
| **数据面**（对象读写） | 自实现二进制帧协议（`internal/transport/protocol`），客户端走 shm 或 TCP | 无 HTTP，gbp 这一类管不到 |
| **管理面**（`taihu web` 子命令） | `internal/web/`：`net/http` + `go:embed` 前端，17 个 `/api/*` JSON 端点 + 静态页 | **gbp 这一类的载体在这里** |

管理面的事实（均可复核）：

- HTTP 服务端：`internal/web/web.go:229` 的 `http.NewServeMux()` + Go 1.22 方法路由（`GET /api/...`），16 个 API handler 注册在 `internal/web/web.go:233-247`。
- 进程装配：`cmd/taihu/cmd/web.go:84` 的 `http.Server{Handler: srv.Handler()}`，默认监听 `0.0.0.0:18080`。
- 前端：`internal/web/static/`（`index.html` / `app.js` / `style.css`）经 `//go:embed` 内嵌，单二进制分发，`internal/web/web.go:22-23`。
- handler 不碰实现：全部依赖本包 `Service` 接口（`internal/web/web.go:38-53`，14 个方法），实现 `webService` 在 `cmd/taihu/cmd/web.go`，复用 CLI helper。
- 无第三方 Web 框架：`grep -rlE 'gin-gonic|go-kratos|labstack/echo' --include='*.go' internal cmd pkg examples` → 0。

## 逐条裁决

### gbp-001 · Use Gin for Simple Projects（CRITICAL）— **不适用（载体存在，选型刻意不同）**

**规则**：中小项目、简单 REST API、快速原型选 Gin；反例是用 Go-Kratos 全家桶搭简单 CRUD。

**对 taihu：不适用 —— 不是「没有 HTTP」，而是「有 HTTP 但标准库够用」。** `internal/web` 的需求清单：

1. 路由：17 条静态路径 + 方法区分，Go 1.22 `ServeMux` 的 `"GET /api/key/get"` 模式原生覆盖（`internal/web/web.go:233-246`），无路径参数（取参走 query）；
2. 出参：全部 JSON，统一 `writeJSON`（`internal/web/web.go:254`），不需要绑定/校验/序列化生态；
3. 静态文件：`http.FileServer(http.FS(static))` 一行（`internal/web/web.go:232`）；
4. 可测性：handler 只依赖 `Service` 接口，`internal/web/web_test.go:122-130` 用 `httptest.NewRequest` + `httptest.NewRecorder` 直接测 handler，不需要 Gin 的测试工具。

Gin 提供而这里用不上的部分（路由组、中间件链、`ShouldBind`、渲染器）正是它的重量所在。**新增 HTTP 端点的默认做法仍是往 `Handler()` 的 mux 上加一行，不要引入框架。**

### gbp-002 · Use Go-Kratos for Complex Microservices（CRITICAL）— **不适用**

**规则**：复杂微服务（gRPC + HTTP 双协议、服务发现、配置中心）选 Go-Kratos。

**对 taihu：不适用，结论与本层重裁前相同。** 多出来的管理面没有改变 taihu 的进程形态：

- 无 gRPC（`grep -rl 'grpc'` → 0）；
- `taihu web` 是**单进程单二进制的运维子命令**，它自己反而是 TiKV 注册区的**客户端**（`webService.kv`，`cmd/taihu/cmd/web.go:109`），不是被注册、被发现的服务；
- 配置走 cobra flag（`--listen` / `-pd`），无配置中心。

「web 里有 HTTP 服务」不等于「taihu 是微服务」——它是存储引擎外挂的一块管理面板。

### gbp-003 · Middleware Design Patterns（HIGH）— **意图适用，当前刻意不用 middleware 形态**

**规则**：日志、鉴权、限流等跨切面关注点抽成 middleware，别在每个 handler 里重复。

**对 taihu：意图适用，形态未采用 —— 这是本层唯一需要盯住的一条。** 现状的跨切面对策：

| 跨切面 | 现状 | 位置 |
|---|---|---|
| 单请求超时/取消 | **每个 handler 手写** `ctx, cancel := s.withTimeout(r); defer cancel()`，17 处重复 | `internal/web/handlers.go:16-18` + 各 handler |
| 错误 → 状态码 | 统一函数 `apiStatus` + `writeErr`，**不是** middleware | `internal/web/web.go:261-282` |
| 鉴权 / 限流 / 审计 | **没有** | — |

没有鉴权等三件套是刻意的：管理页面向**内网运维**场景，无用户体系，危险操作（delete / purge）在业务层强制 `confirm=true` 二次确认（`internal/web/key_handlers.go:146-154`），不靠 HTTP 层拦。

**17 处超时样板就是规则点名的那类重复，但当前规模下抽 middleware 不省多少**（每个 handler 仍要拿 ctx）。**触发改造的条件写在这里**：一旦要加鉴权、审计日志或限流 —— 第一刀是在 `Handler()` 包一层 mux middleware（`func(http.Handler) http.Handler` 包住 `mux` 或按 `/api/` 子树挂），**不要**逐 handler 加参数。

### gbp-004 · Graceful Server Shutdown（HIGH）— **部分适用，且当前有真实偏离**

**规则**：服务必须支持优雅停机，等在途请求处理完；载体是 `http.Server` + `signal.Notify` + `srv.Shutdown(ctx)`。

**对 taihu：形态对上了一半，语义是反的。** `cmd/taihu/cmd/web.go:87-92`：

```go
go func() {
    sig := make(chan os.Signal, 1)
    signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
    <-sig
    _ = httpSrv.Close() // ← 是 Close()，不是 Shutdown(ctx)
}()
```

`Close()`（`cmd/taihu/cmd/web.go:92`）立即关 listener **并强断所有活连接**；规则要求的 `Shutdown(ctx)` 是停接新请求、等在途请求收尾。两者对「在途请求怎么办」给出的答案相反。

**偏离的影响面（说清，不夸大）**：

- 在途请求可能很长：`Server.timeout = 60s`（`internal/web/web.go:34`），cluster status 要逐实例探测；
- web 进程不持有数据：handler 经 `rpcclient` 对存储实例发 RPC。响应被截断**不会损坏数据**，但 `key put` 可能出现「服务端已落盘、客户端收到断连」的不确定结局 —— 调用方需重新 stat/get 确认（运维操作可接受）；
- 数据面另有一套独立停机语义：`cmd/taihu/cmd/server.go` 收信号后走 `internal/transport` 的 `GracefulStop()`，与本条无关，不套 HTTP 模型。

**所以当前状态是「已知偏离、风险可接受」，不是「已优雅停机」。** 若以后写端点变重（长事务、服务端副作用不可忽略），修法是把 `Close()` 换成带超时的 `Shutdown(ctx)`（如 10s，长于单实例探测、短于运维等待耐心），并让 `Serve` 回到主 goroutine 等待。

> 旁证：`internal/web/web.go:227` 的 `panic(fmt.Sprintf("embed static: %v", err))` 符合 [error/](../error/index.md) gbp-021 —— `//go:embed` 目录缺失只可能发生在编译/装配期，属不可恢复的 init 不变量，不是运行时错误。
