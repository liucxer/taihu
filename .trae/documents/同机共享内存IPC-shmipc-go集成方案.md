# 同机共享内存 IPC（shmipc-go）集成方案

## Context

当前 taihu 客户端（`pkg/rpcclient`）与服务端（`internal/transport`）只有 TCP socket 通信（netpoll 帧协议）。当客户端和服务端部署在**同一节点**时，走 loopback TCP 存在用户态/内核态拷贝与 syscall 开销，且经实测 loopback 单流上限（~40-70 Gbps）远低于三盘并发读带宽（19.71 GiB/s ≈ 158 Gbps），TCP 会成为同机端到端的瓶颈。

用户要求：使用 `github.com/cloudwego/shmipc-go` 增加**共享内存 IPC** 方案（数据面共享内存零拷贝，控制面走 unix socket 同步，内存不足自动 fallback），同时**保留现有 RPC（netpoll/TCP）方案**。目标：同机部署时客户端经 shmipc 直连服务端，消除 TCP 内核拷贝与 syscall 开销。

**约束**：
- shmipc-go 的 `NewListener` 仅支持 Linux（issue #45 明确）。本地开发为 Windows，需 build tag 隔离（项目已有先例：`third_party/netpoll/netpoll_windows.go`）。
- 保留现有 netpoll/TCP 路径零改动，shmipc 为纯增量。
- 部署验证在 128.12（Linux aarch64）。

## 已确认的 shmipc-go API（官方 example/best_practice）

服务端：`net.ListenUnix("unix", ...)` → `Accept()` → `shmipc.Server(conn, DefaultConfig())` → `server.AcceptStream()` 逐流处理；流上用 `stream.BufferReader()` 读请求、`stream.BufferWriter()` 写响应、`stream.Flush(false)` 提交。

客户端：`shmipc.DefaultSessionManagerConfig()` 设 `Address/Network="unix"/MemMapType=MemMapTypeMemFd/SessionNum` → `shmipc.NewSessionManager(conf)` → `smgr.GetStream()` 取流、`smgr.PutBack(stream)` 归还复用。`BufferReader.ReadBytes()` 与 `BufferWriter.Reserve()` 支持零拷贝读写共享内存。共享内存缓冲容量经 `Config.ShareMemoryBufferCap` 配置（默认 1MB，对象 4MiB 需调大）。

**关键差异**：shmipc stream 天然按流隔离（请求-响应双向流），无 streamID；现有 transport 是"连接内多路复用流（streamID 分帧）"。shmipc 路径无需 streamID，帧格式退化为 `[4B len][1B op][payload]`。

## 实现方案（纯增量，TCP 零改动）

### 1. go.mod
直接 `require github.com/cloudwego/shmipc-go`（最新版本）。shmipc 相关文件均带 `//go:build linux`，Windows 构建不编译该包（stub 兜底，见下）。

### 2. `internal/transport/protocol.go`：编解码纯函数解耦（签名级改动）
现有 `readU32/readU64/parsePutHeader/parseGetReq/parseKeyReq` 参数为 `netpoll.Reader`。改为接口参数：
```go
type byteReader interface {
    Next(int) ([]byte, error)
    ReadString(int) (string, error)
}
```
`netpoll.Reader` 天然满足该接口（TCP 行为不变）；新增 `sliceReader{b []byte}` 适配共享内存切片。仅签名变化，TCP 路径零行为改动。

### 3. `internal/transport/shmipc_server.go`（//go:build linux）：服务端
```go
// ServeShm 在 unix socket 上提供 shmipc 服务，返回 io.Closer 用于关闭。
func ServeShm(storage *taihu.Storage, uds string) (io.Closer, error)
```
- `net.ListenUnix` + accept 循环，每连接 `shmipc.Server(conn, DefaultConfig())`，`AcceptStream()` 每流起 goroutine。
- `handleShmStream`：`readShmFrame`（`ReadBytes(4)` 取 len → `ReadBytes(1+len)`，校验 `maxFrameTotal`）→ 按 OpCode 分发，**逻辑镜像** `handlePut/handleGet/handleDelete/handleStat`（复用 `parse*` 纯函数与 `storage.Put/ReadAt/Delete/Stat`；Get 按 chunkSize 分块、末帧置 final 位）。响应 `writeShmFrame`（`BufferWriter.Reserve(1+len)` + `Flush(false)`，每 Flush 一帧）。
- 同步累加现有 `statTx*/statRx*` 统计，便于与 TCP 路径对比。
- 不新建 `internal/shmipc` 包：避免导出 parse/统计且 rpcclient 已依赖 transport，防循环依赖。

`internal/transport/shmipc_server_other.go`（//go:build !linux）：同签名 stub，返回"shmipc 仅支持 linux"错误 → cmd 主文件无需 build tag。

### 4. `pkg/rpcclient`：客户端
- `storage_rpc.go`：引入 `type rpcConn interface{ Put(...); Get(...)([]byte, func(), error); Delete(...); Stat(...); Close() error }`；`Storage.conns` 从 `[]*transport.Conn` 改为 `[]rpcConn`（`transport.Conn` 已满足，`dial.go`/`pick()` 仅类型变更，round-robin 不变）。
- `shmipc.go`（//go:build linux）：
```go
func DialShmPool(ctx context.Context, uds string, n int) (*Storage, error)
func DialShm(ctx context.Context, uds string) (*Storage, error) // = DialShmPool(…,1)
```
  - `SessionManagerConfig{Address: uds, Network: "unix", MemMapType: MemMapTypeMemFd, SessionNum: n, MaxStreamNum: 256}`。
  - 每 RPC：`GetStream` → 写帧（`BufferWriter`+`Flush`）→ 读帧（`BufferReader`）→ `PutBack`。
  - **Get 零拷贝**：整响应恰一帧（==size）时直接返回 `ReadBytes` 切片并 pin 住 stream，release 幂等 `PutBack`（数据在归还前有效，契约同 TCP Get）；多帧回退 bufpool 汇入后即 `PutBack`。
- `shmipc_other.go`（//go:build !linux）：同签名 stub 报错。

### 5. 命令入口
- `cmd/taihu-server/main.go`：加 `-shm`（uds 路径，默认空=禁用）。非空时 `go transport.ServeShm(storage, *shm)`，返回的 `io.Closer` 纳入关闭流程。TCP 监听（现有 `gs.Serve(lis)`）与 shm 服务并行，互不干扰。
- `cmd/taihu-rpc-bench/main.go`：加 `-shm`，validate 要求 `-addr`/`-shm` 二选一；`if c.shm != "" { s = rpcclient.DialShmPool(...) } else { s = rpcclient.DialPool(...) }`。`runWorker` 零改动复用。

### 6. 容量配置
- `ShareMemoryBufferCap = chunkSize + 4KiB`（按帧而非整对象 SegmentSizeBytes=8GB——buffer 按流分配，8GB×SessionNum 会耗尽内存；且分帧与现有 4MiB 帧断言/统计 1:1 对齐）。
- `SessionNum` 映射 `-conns`（默认 1）；`MaxStreamNum=256` 防 `GetStream` 阻塞；`QueueCap` 保持默认。
- Put 数据 >4MiB 时逐帧 `Reserve`+`Flush`，共享内存环满自动背压（语义同 TCP 流控）。

## 涉及文件
- `go.mod`（+shmipc-go 依赖）
- `internal/transport/protocol.go`（byteReader 接口 + sliceReader）
- `internal/transport/shmipc_server.go`、`internal/transport/shmipc_server_other.go`（新增）
- `pkg/rpcclient/storage_rpc.go`、`pkg/rpcclient/dial.go`（rpcConn 接口）
- `pkg/rpcclient/shmipc.go`、`pkg/rpcclient/shmipc_other.go`（新增）
- `cmd/taihu-server/main.go`、`cmd/taihu-rpc-bench/main.go`（-shm 参数）

## 验证
1. **Windows 本地**：`go build ./...`、`go vet ./...` 必须通过（shmipc 文件被 build tag 排除，stub 兜底）。
2. **Linux（128.12）**：
   - `taihu-server -addr :50051 -shm /tmp/taihu.shm -db ... -dev ...` 起服，TCP 与 shm 双监听。
   - `taihu-rpc-bench -shm /tmp/taihu.shm -mode write -size 4194304 ...` → read，与 `-addr 127.0.0.1:50051` 全量对比：吞吐/延迟/帧统计（`transport.StatsString()`）。
   - 重点验证：Get 零拷贝 release 幂等、`PutBack` 后数据不被后续请求覆盖、跨帧（多帧 Get）正确性、容量配置生效（无 shared memory exhausted fallback 报错）。
3. 同机端到端对比报告：shmipc vs loopback TCP 的带宽与 CPU（预期 shmipc 消除内核拷贝，CPU busy 显著下降，带宽不受 loopback 单流上限约束）。
