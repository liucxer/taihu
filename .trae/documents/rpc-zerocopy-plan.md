# RPC 读写路径零拷贝改造计划

## Context（为什么改）

当前 rpcclient↔server 的大块数据路径拷贝现状（基于工作树现状，上游 netpoll v0.7.5）：

- **出站（外发）已是零拷贝**：`internal/transport/conn.go#writeFrame` 对 >4K 负载 `WriteBinary` 引用原缓冲、`Flush` 用 writev 散射出栈。服务端 Get 下发、客户端 Put 上传均为 0 次用户态 memcopy。
- **入站（收流）仍有拷贝**：`conn.go#Get` 与 `server.go#handlePut` 都是 `p,_:=Next(rem); copy(buf[pos:],p)`，把 netpoll 收缓冲搬到目标缓冲。帧跨节点时 `Next` 还会先做一次分配拷贝。

目标是“尽力让两端都零拷贝”。经核查存在一个**物理硬约束**：

> 服务端 Put 落盘走 `Device.Append`（O_DIRECT），强制 **地址+偏移+长度** 全部 4K 对齐。而 `readLoop`（conn.go#118-149）`Slice` 整帧后先 `Skip(4)+Next(4)+ReadByte()`（共 9 字节内联帧头）再取 payload，故 **payload 切片起始地址 = netpoll 节点基址 + 9，必然 4K 不对齐**。因此服务端无法对 netpoll 收缓冲直接 O_DIRECT 直写，**必须先拷入 4K 对齐的 bufpool 缓冲（≥1 次拷贝）**。这是零拷贝的物理极限，除非改协议让载荷按 4K 对齐（本文不回退到该高风险方案，可作后续试探）。

**客户端 Get 不受此限制**：客户端把数据交还调用方，无磁盘、无对齐要求，可达成真零拷贝。

### 本计划目标（“尽力”的最终形态）
- 出站：保持 0 拷贝（不改）。
- **客户端 Get：真零拷贝（0 次）**——单节点数据帧直接移交 netpoll 缓冲给调用方。
- **服务端 Put：收敛到恰 1 次**——消除帧跨节点时的第 2 次分配拷贝，只保留 O_DIRECT 必需的 1 次对齐汇并。

## 方案（分两阶段）

### P0：引入极简 netpoll fork
把上游 netpoll v0.7.5 原样拷入 `third_party/netpoll/`，在 `go.mod` 加 `replace github.com/cloudwego/netpoll => ./third_party/netpoll`。fork 的 go.mod 仍为 `module github.com/cloudwego/netpoll`，**不 import 任何 taihu 包，避免模块环**。

新增（最小侵入，不影响原路径）：
1. **可插拔对齐节点分配器**
   - 新导出 `SetAlignedAllocator(get, put func([]byte))`；未设置时行为与上游一致（mcache）。
   - `newLinkBufferNode(size)` 在设置后改用 `get` 分配 4K 对齐缓冲（期望由 bufpool 提供）；`linkBufferNode.Release()` 中 `reusable()` 归还时改用 `put`。
2. **单节点所有权移交 `TakeBytes`**
   - 在 Reader/LinkBuffer 上新增：若当前未读数据位于**单个节点内且自当前 offset 起**，则将该节点从本 LinkBuffer 的复用链中**分离**（标记为 unmanaged、不再 Free），返回该片缓冲给调用方；调用方持有后用 `put`（`bufpool.Put`）归还。
   - 多节点/跨节点时返回 `ok=false`，调用方走既有拷贝路径。保证正确性不退化。

### P1：transport 接入
- `internal/transport`（`server.go#NewServer` 或包 init / `dial.go`）设置 `netpoll.SetAlignedAllocator(bufpool.Get, bufpool.Put)`，使收节点为 bufpool 对齐缓冲。
- **客户端 Get（conn.go#Get 收流分支）**：
  - `opGetData` 帧先尝试 `TakeBytes(rem)` → 成功则**直接作为返回缓冲**返回（0 拷贝），跳过 `copy(out[pos:],p)`；
  - 失败（多节点/非单节点）回退到现有 `Next`+`copy`（保持 1~2 拷贝，正确性不变）。
  - 返回缓冲仍满足“bufpool 池化缓冲”契约，调用方（taihu-client/rpc-bench 等）照旧 `bufpool.Put`，**无需改调用方**。
- **服务端 Put（server.go#handlePut）**：保留“汇入对齐 `buf`”→“`Storage.Put`→`Device.Append` 直写”的现有 1 次对齐拷贝路径；借助 P0 对齐单节点让 `Next` 变零拷贝，从而总次数稳定为 1（消除可能的第 2 次）。

## 涉及文件
- `go.mod`：新增 replace。
- `third_party/netpoll/`：netpoll v0.7.5 fork 全量（新增，含 2 处小改：allocator 钩子 + `TakeBytes`）。
- `internal/transport/conn.go`：`Get` 收流接入 `TakeBytes` 零拷贝分支。
- `internal/transport/server.go`（或 `pkg/rpcclient/dial.go`）：注入 `netpoll.SetAlignedAllocator(bufpool.Get, bufpool.Put)`。
- 若 fork 内需要复用 taihu 的 bufpool 常量，仅取字面量 4096/桶逻辑，不产生包依赖。

## 验证
1. `go build ./...`、`go vet ./...`。
2. `go test ./...`（含 `-race`），重点 `pkg/rpcclient/pool_test.go` 的 bufconn e2e round-trip；多帧对象（>4MiB）、单帧对象（≤4MiB）、`bufpool` 归还不漏。
3. 本机端到端：`taihu server`(本地 TCP) + `taihu-client/rpc-bench` 单帧/多帧 put/get，SHA256 对比；`-race` 跑一遍确保 zero-copy 归还无双重释放。
4. （可选）性能采样：客户端 Get 路径 memmove 占比应显著下降（历史 43%-62%）。

## 风险与回退
- **缓冲生命周期（核心风险，此前 gRPC 教训）**：`TakeBytes` 必须让 netpoll 彻底放弃该节点（不可复用、不 Free），且返回缓冲唯一所有权归调用方；配合“节点即 bufpool 缓冲”，归还路径唯一（`bufpool.Put`），杜绝双 Free。若验证中发现任何生命周期错乱，立即回退该分支到既有拷贝路径。
- 多节点 `TakeBytes` 失败即回退拷贝，正确性不退化，属 best-effort。
- netpoll 为本地 replace fork，模块无外部更新压力；不引入 taihu 依赖避免模块环。

## 明确不做（避免 over-engineering）
- 不重写协议把帧头/载荷改为 4K 对齐去强行实现服务端真零拷（脆弱且收益有限，仅此文档记录为后续方向）。
- 不改 `Device.Append` / `Storage` 的整对象语义，不引入 scatter-gather。