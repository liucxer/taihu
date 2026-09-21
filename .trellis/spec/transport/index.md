# 远程访问层（transport / rpcclient / cluster）

> 本层把 `internal/storage` 的能力通过网络暴露给客户端：`internal/transport` 是帧协议与服务端/客户端连接，`internal/rpcclient` 是直连客户端 SDK 内核，`internal/cluster` 是集群注册与索引的叶子抽象。

---

## 本层边界

| 包 | 职责 | 关键性质 |
|----|------|----------|
| `internal/transport/protocol` | 线路协议编解码纯函数 | 无 I/O、无全局状态（`internal/transport/protocol/protocol.go:1`） |
| `internal/transport` | netpoll TCP 与 shmipc 两条数据面的帧收发、RPC 服务端/客户端 | 两套帧头、同一套 op；平台差异靠 `_linux.go`/`_other.go` 分文件 |
| `internal/rpcclient` | 直连客户端：连接池 + `ObjectStore` 实现 + 对 `pkg/` 的 re-export 第一层 | 不感知集群；`ObjectStore` 在此声明（会成环的反方向） |
| `internal/cluster` | 注册中心 KV 抽象、实例/客户端注册与心跳、key 前缀约定 | 叶子包：不依赖仓库内任何其它包 |

**不归本层管的**：错误 sentinel 的定义与全局体系在 `internal/ierr`，见 `.trellis/spec/architecture/error-model.md`；本层只写"错误码怎么过线"（`wire-protocol.md` §5）与"错误怎么 re-export"（`interfaces-and-reexport.md` §4）。存储引擎、设备层、元数据层的规则见对应层目录。测试分层的完整约定在 `.trellis/spec/testing/`。

## Pre-Development Checklist

动手前逐条确认：

- [ ] 改动是否落在 `protocol` 包？若是，确认新函数仍是**纯函数**：不引入连接、全局状态、缓冲池、日志句柄（`wire-protocol.md` §1）。
- [ ] 是否改了任一尺寸/派生常量（`ChunkSize`/`FrameHeaderLen`/`MaxFrameTotal`/`InputNodeSize`/`ShmSliceSize`/`SegItemLen`/`OpGetDataFinal`）？必须同步 `internal/transport/protocol/protocol_test.go` 的 `TestWireConstantsAreConsistent`（`wire-protocol.md` §2）。
- [ ] 新增 `Parse*`：只收 `protocol.ByteReader`，长度字段先校验再分配，并加入 `protocol_test.go` 的 `parsers()` 表（截断/短一字节/超长 key 三个测试都会自动覆盖）。
- [ ] 新增或改动错误码：走 `MapStorageErr` / `MapCode` / `EncCode` 三个函数，不要让错误字符串上线；注意 `CodeInternal` 在 `MapCode` 里走 default 生成新 error（客户端无法 `errors.Is` 对齐）（`wire-protocol.md` §5）。
- [ ] 新增 op：同时给 TCP 与 shm 两个帧头长度下的处理路径；流的首帧类型要登记进 `Server.dispatch` 的 switch，否则会被判协议错误关连接（`wire-protocol.md` §9）。
- [ ] 改 `handlePut`：任何"PutHeader 之后提前返回"的分支都要先 `c.drainPutTail(st, size)`（`wire-protocol.md` §10）。
- [ ] 加平台相关能力：`_linux.go` 真实现 + `_other.go` 返回 sentinel，且服务端有 `ShmSupported()` 可降级（`wire-protocol.md` §15）。
- [ ] 新增内部接口：小（2~6 方法）、未导出、声明在使用方文件里；跨模块才导出，且签名只用标准库类型（`interfaces-and-reexport.md` §1、§2）。
- [ ] 新增实现：补一行 `var _ 接口 = (*实现)(nil)` 断言（`interfaces-and-reexport.md` §3）。
- [ ] 改动导出签名：把出现的 internal 类型（连同调用方解读它所需的常量、sentinel）补进 `internal/rpcclient/reexport.go`，必要时再补 `pkg/taihu-client/reexport.go`（`interfaces-and-reexport.md` §4、§5）。
- [ ] 补完 re-export：在 `internal/rpcclient/api_test.go` 里把新类型/常量/sentinel 命名一遍，否则漏补不会报错（`interfaces-and-reexport.md` §6）。
- [ ] 往 `internal/cluster` 加 import 前确认没有引入仓库内依赖（否则 `make check-sdk-only` 会红）（`interfaces-and-reexport.md` §7）。

## Quality Check

```bash
# 本层快速自检（本机 macOS 可跑）
go vet ./internal/transport/... ./internal/rpcclient/... ./internal/cluster/...
go test ./internal/transport/... ./internal/rpcclient/... ./internal/cluster/...

# 仓库级门禁：gofmt（排除 third_party）+ 分层不变量 + go vet
make check

# 平台相关代码改动后必跑：跨平台编译核对（linux/amd64 + linux/arm64，含测试二进制编译）
make check-linux
```

`make check-linux` 对本层**不是可选项**：`internal/transport` 的 shm 路径、`internal/rpcclient` 的 `_linux.go`/`_other.go` 全部靠 build tag 分流，macOS 上编得过不代表 Linux 上编得过（`Makefile:60-70`）。同理，改完 fork 相关代码后本机绿不代表真机绿：shmipc 段布局与上游 ABI 不兼容，两端必须同 fork，并做一次真实 Put/Get 往返（`third_party/README.md:162-164`）。

## 本层文件

| 文件 | 用途 |
|------|------|
| [wire-protocol.md](./wire-protocol.md) | 线路协议：帧格式与不变量、`protocol` 纯函数性质、错误码映射、TCP 与 shm 两条数据面的差异、fork 与跨平台 stub |
| [interfaces-and-reexport.md](./interfaces-and-reexport.md) | 接口设计约定（小接口、消费者侧声明、跨模块才导出）、编译期断言、re-export alias 链与外置测试包兜底、`internal/cluster` 叶子包约束 |
