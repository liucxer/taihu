# 1a9fea5 服务端读路径整帧零拷贝发送优化验证报告

> 对应提交：`2f87501`（server 整帧零拷贝 writev 快速路径）+ `1a9fea5`（修复快速路径空 itl 时 peek() panic）
> 测试时间：2026-09-10
> 前置：`04ff444`+`39c31df`（fork gRPC 调大 HTTP/2 帧 1MiB）、`de625b8`+`2f96c69`（client RawFrame 延迟物化 + GetRaw 帧式 API，详见对应报告）

## 1. 背景与目标

2f96c69 后 client 读路径拷贝已基本消除（GetRaw 帧式交付），读带宽达 ~6.1 GiB/s（盘极限），但**服务端** gRPC 发送路径仍有大量用户态拷贝：

- `mem.(*sliceReader).Read`：`loopyWriter.processData` 把 1MiB mem.Buffer 拷入临时物化缓冲（1 次整块 memmove）。
- `bufWriter.Write`：发送缓冲再次 memmove + socket write syscall。
- 服务端 pprof：`runtime.memmove` 28.66% + `Syscall6` 54.84%（2f96c69 实测），属 gRPC loopyWriter/bufWriter 固有路径（lessons learned 提及的 2 次不可避免用户态 memmove）。

目标：消除服务端发送路径的整快 memmove，把「2 次 1MiB memmove + 1 次 syscall」降为「1 次 writev syscall」。

## 2. 优化内容

| 文件 | 变更 | 目的 |
|------|------|------|
| `http_util.go` `frameWriter.writev` | bufWriter 增 writev：先 flush 待发缓冲，再 `net.Buffers.WriteTo(conn)` 一次性写出多段 | 底层 `*net.TCPConn` 走 writev gather syscall，零用户态拷贝 |
| `http_util.go` `framer.writeDataDirect` | 组装 [9B HTTP/2 帧头 + 5B gRPC 消息头 + 消息数据] 为 net.Buffers，一次 writev | 帧头字段直接手写（长度/type/flags/streamID），不重材质化 |
| `controlbuf.go` `dataFrame.buffers` | dataFrame 增原始 BufferSlice 字段（与 reader 同引用，不额外 Ref） | 快路径可直接引用数据，跳过物化和 Read |
| `controlbuf.go` `processData` | server 侧 + 整帧可发（`hSize==len(h)`、`dSize==reader.Remaining()`、`remainingBytes==0`、`size>=batchSize`）时走 `writeDataDirect` | 大帧整帧一次 writev |
| `http2_server.go` `write` | dataFrame 填充 `buffers` | 提供快路径数据 |

**快路径触发条件**：server 侧、携带原始 data、消息头整发、数据整读、单帧发完、帧 ≥ bufWriter batchSize（大帧才整帧 flush；小帧保留 bufferWriter 聚合，避免 syscall 数上升）。

**1a9fea5 修复**：`itemList.peek()` 无 nil 保护（直接 `return il.head.it`），dequeue 后 itl 可能为空，原实现无条件 peek() 触发 SIGSEGV。改为先判 `isEmpty()`。

## 3. 测试环境

- 100.71.128.12（128.12）单机，client/server 同机走 lo 回环，96 核 aarch64。
- `/dev/nvme1n1`（O_DIRECT），`-db /tmp/tdb-rpc`。
- 单进程 `taihu-rpc-bench`：`-mode rawread -size 4194304 -count 20000 -threads 32 -conns 16 -keys-prefix r6f-w1`。
- server：`taihu-server.new -addr :50051 -db /tmp/tdb-rpc -dev /dev/nvme1n1 -pprof :6060`。
- 先 `-mode write` 写入 20000 个 4MiB 对象（642.65 ops/s, 2570.59 MiB/s）作为读数据。
- 读测期间双端 pprof（client 用 -cpuprofile，server 抓 15s 窗口）+ iostat/mpstat 采集。

## 4. 效果对比

### 4.1 带宽（rawread，单客户端 32×16）

| 版本 | 带宽 |
|------|------|
| 2f96c69（server 拷贝路径） | 6238 MiB/s（6.09 GiB/s） |
| **1a9fea5（writev 零拷贝）** | **6298.78 MiB/s（6.15 GiB/s）** |

带宽在盘极限附近（读极限 6.3~6.6G，iowait 高），优化不改带宽上限。

### 4.2 服务端 pprof（rawread，15.16s，总样本 55.91s）

**Top（flat）：**

| 函数 | flat | flat% | 说明 |
|------|------|-------|------|
| `internal/runtime/syscall.Syscall6` | 46.97s | **84.01%** | socket 写（writev），不可避免 |
| `framer.writeDataDirect` | cum 40.74s | 72.87% | 快路径主体，全部归 syscall |
| `runtime.memmove` | **0** | **0%** | **彻底消除（2f96c69 为 28.66%）** |
| `runtime.memclrNoHeapPointers` | — | — | 无 |
| `loopyWriter.processData` | cum 40.99s | 73.31% | gRPC 发送主循环 |

**快路径实际生效的证据**：`writeDataDirect` cum 72.87%、`bufWriter.writev` cum 72.60%、`net.(*Buffers).WriteTo` cum 71.72%、`(*FD).Writev` cum 71.63% —— 整帧走 writev 路径命中，且 **memmove 从 pprof 完全消失**。

**结论**：服务端发送路径的两次用户态 memmove（sliceReader.Read + bufWriter.Write）全部消除，CPU 现集中为 writev socket syscall（84.01%，不可再减，是传输 6.15 GiB/s 的真实数据移动成本）。磁盘 `os.(*File).ReadAt`（10.46% cum）为数据来源，占比合理。

### 4.3 客户端 pprof（rawread，12.81s，总样本 72.45s）

服务端优化不改变客户端行为，客户端仍为 gRPC 接收侧固有成本：

| 函数 | flat | flat% | 说明 |
|------|------|-------|------|
| `syscall.Syscall6` | 39.03s | 53.87% | socket 读 |
| `runtime.memmove` | 20.69s | 28.56% | `mem.Copy`（http2Client.handleData 向缓冲池拷贝），接收路径固有 |

客户端 memmove 来自 `golang.org/x/net/http2` 接收侧把手写缓冲拷入 gRPC 缓冲池（`mem.Copy` cum 28.31%），因 framer 复用同一 read buffer，属不可避免，与本服务端优化无耦合。

### 4.4 物理机资源（读测窗口）

- **磁盘（iostat 实测）**：读带宽稳定 ~6.2 GiB/s（6523776 kB/s 稳态样本），峰值 ~6612 kB × 256 = 6.46 GB/s，基本打满盘读。
- **CPU（mpstat all 均值）**：busy ~26.7%（idle 73.3%），iowait 16.7%。
- **忙核 TOP**：core=42/44/46/20 等，busy 61~64%，**iowait 43~49%**（磁盘瓶颈确认）。

## 5. 结论

1. **服务端零拷贝达标**：`runtime.memmove` 28.66% → 0%，CPU 转为纯 writev syscall（84%），彻底消除 gRPC loopyWriter/bufWriter 固有的 2 次用户态 memmove。
2. **带宽不变（~6.15 GiB/s）**：写读路径已达磁盘访问模式极限，iowait 高（忙核 ~45%），优化属于 CPU 减负而非带宽提升。
3. **后续方向**：服务端 CPU 已收敛到「磁盘读 syscall + socket 写 syscall」，用户态零拷贝到顶；进一步降服务端 CPU 只能走 io_uring/MSG_ZEROCOPY 或多缓冲 DMA，或改磁盘访问模式提升带宽上限（与 RPC 层无关）。

## 6. 测试还原

- 读数据：`/tmp/taihu-rpc-bench.new -mode write -addr 100.71.128.12:50051 -size 4194304 -count 20000 -threads 16 -conns 16 -keys-prefix r6f-w1 -latency`
- 读压测：`/tmp/taihu-rpc-bench.new -mode rawread -addr 100.71.128.12:50051 -size 4194304 -count 20000 -threads 32 -conns 16 -keys-prefix r6f-w1 -cpuprofile /tmp/raw6.client.cpu`
- server pprof：`curl -s localhost:6060/debug/pprof/profile?seconds=15 -o /tmp/raw6.server.cpu`
- 后台：`iostat -d /dev/nvme1n1 1` + `mpstat -P ALL 1`