# framework —— Web 框架选型（gbp 类 1）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/framework-*.md`，4 条。
> **本层 4 条对 taihu 全部不适用。** 保留这一层是为了记住否决理由。

## 为什么整层不适用

gbp 这一类的前提是「你要写一个 HTTP 服务端」。taihu 不是：

- **数据面**是自实现的二进制帧协议（`internal/transport/protocol`），不是 HTTP。客户端走 shm 或 TCP，帧格式自己定。
- **控制面**走 TiKV（`github.com/tikv/client-go/v2`），不是 REST。
- 全仓库 `net/http` 只有 1 处引用：`cmd/taihu/cmd/server.go:13-14` 的 `_ "net/http/pprof"`，那是**调试端点**，不是业务服务端。

核实：`grep -rlE 'gin-gonic|go-kratos|labstack/echo' --include='*.go' internal cmd pkg examples` → 0。

## 逐条裁决

### gbp-001 · Use Gin for Simple Projects（CRITICAL）

**规则**：中小项目、简单 REST API、快速原型选 Gin；反例是用 Go-Kratos 全家桶搭简单 CRUD。

**对 taihu：不适用。** 本仓库无 HTTP 服务端、无路由、无 handler 栈。Gin 的一切用法（`gin.Default()` / `c.JSON` / `r.Group`）都没有安放处。

### gbp-002 · Use Go-Kratos for Complex Microservices（CRITICAL）

**规则**：复杂微服务（gRPC + HTTP 双协议、服务发现、配置中心）选 Go-Kratos。

**对 taihu：不适用，且是本条最常见的误用方向。** taihu 是**单进程存储引擎**，不是微服务：无 gRPC（`grep -rl 'grpc'` → 0）、无服务发现、无配置中心（配置走 cobra flag，见 `configs/taihu-server.example.sh`）。把 Kratos 引入会带来一套用不上的抽象。

### gbp-003 · Middleware Design Patterns（HIGH）

**规则**：日志、鉴权、限流等跨切面关注点抽成 middleware，别在每个 handler 里重复。

**对 taihu：不适用（载体不存在），但意图有对应物。** 规则的载体是 `gin.HandlerFunc` + `r.Use(...)`；本仓库无 HTTP handler 栈。跨层关注点在这里的形态是**传输层的帧读写**与**日志三通道**，属另一主题，不靠本条并入。

### gbp-004 · Graceful Server Shutdown（HIGH）

**规则**：服务必须支持优雅停机，等在途请求处理完；载体是 `http.Server` + `signal.Notify` + `srv.Shutdown(ctx)`。

**对 taihu：部分适用——意图适用，载体不适用。** 本仓库有明确对应物：`cmd/taihu/cmd/server.go` 收到 `SIGINT` / `SIGTERM` 后走 `internal/transport/server.go` 的 `GracefulStop()`。但本条的具体形态（`http.Server`、`srv.Shutdown(ctx)`、5s 超时）在此没有对应关系。**在途请求如何收尾由 transport 层自己的语义定**，不套 HTTP 的模型。
