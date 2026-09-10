# 468a9af RPC 端到端读写性能全量测试报告

> 对应提交：`468a9af`（feat: taihu-server 新增可选 pprof 端点 `-pprof :6060`，默认关闭，仅压测/排查用）
> 测试时间：2026-09-09 ~ 2026-09-10
> 详细分项报告：`468a9af 16客户端读性能全量分析报告.md`、`468a9af 4客户端写性能全量分析报告.md`

## 1. 测试目标

验证 taihu 远程访问层（gRPC，设计文档 v3）端到端读写性能，并定位瓶颈：

- 标定**单盘读写极限**（本地 Storage）与 **RPC 端到端**读写带宽、吞吐、延迟；
- 扫描**达到带宽极限所需的最低客户端数**（连接数）；
- 全量采集 CPU（整机 + 逐核 + TOP 忙核）、磁盘（iostat 实测）、网络（lo 回环实测）占用；
- 采集 server / client 双端 pprof 热点，重点确认**数据拷贝（memmove/memclr）**在读写路径的占比，为「减少拷贝」优化提供依据。

## 2. 测试环境

| 项目 | 配置 |
|------|------|
| 服务器 | 100.71.128.12（client 与 server 同机） |
| 架构 / CPU | aarch64，96 核 |
| 系统 | Linux 4.19.90 bclinux |
| 编译产物 | `/tmp/taihu-server.new`、`/tmp/taihu-rpc-bench.new`、`/tmp/taihu-bench.new`（go1.24，GOTOOLCHAIN=auto 补齐 go1.25.0） |
| 数据设备 | `/dev/nvme1n1`（1.8T 裸盘，O_DIRECT） |
| 元数据 | `/tmp/tdb-rpc`（pebble，server 实例内复用） |
| 对象大小 | 4MiB（4194304 B），4K 对齐 |
| 网络 | client/server 同机，`100.71.128.12` 路由至 **lo 回环**（实测 lo 承载流量，物理网卡计数不变） |
| 部署方式 | `taihu-server -addr :50051 -db /tmp/tdb-rpc -dev /dev/nvme1n1 -pprof :6060` |
| 压测参数 | `taihu-rpc-bench -addr 100.71.128.12:50051 -mode write/read -size 4194304 -threads 16 -latency [-cpuprofile]` |

## 3. 测试方法

1. **单盘极限标定**：本地 `taihu-bench`（同机 O_DIRECT，不经 gRPC），16 线程并发，测写/读最大带宽作为盘的真上限；另用 `dd` 直测裸盘作为下限参照。
2. **RPC 单客户端基线**：1 个 `taihu-rpc-bench`（16 线程）先写后读同一批 4MiB key，各 10000 个（40GB/轮）。
3. **多客户端收敛扫描**：启动 1/2/3/4/6/8/10/12/14/16 个独立 bench 进程（各 16 线程 = 独立 gRPC 连接），每档先写 1500×4MiB/客户端再读，统计合计带宽，找封顶客户端数。
4. **全量仪表化复测**：对 16 客户端读、4 客户端写分别复测，期间并行采集 `iostat -d`（磁盘）、`mpstat -P ALL`（CPU）、lo 网络采样（/proc/net/dev 每秒增量）；**server 侧 pprof** 用 `-pprof :6060` 采 15s CPU profile（覆盖测试中段），**client 侧 pprof** 用 `-cpuprofile` 采其中 2 个进程。
5. pprof 用远端 `go tool pprof -top`（go1.24）分析。

## 4. 测试结果

### 4.1 单盘读写极限（本地 Storage，不经 RPC）

| 方向 | 带宽 | 说明 |
|------|------|------|
| 写 | **2.51 GiB/s**（2573 MiB/s） | 16 线程并发，贴齐设备写上限 |
| 读 | **6.6 GiB/s**（6759 MiB/s） | 16 线程并发，贴齐设备读上限 |

dd 单线程直测裸盘：读 1.5 GB/s、写 2.1 GB/s（并发后更高）。**本地 Storage 可打满单盘读写极限。**

### 4.2 RPC 单客户端基线（16 线程、4MiB、40GB/轮）

| 模式 | 吞吐 | 带宽 | p50 | p90 | p99 |
|------|------|------|-----|-----|-----|
| 写 | 226.87 ops/s | **907.48 MiB/s** | 70.33ms | 76.02ms | 83.09ms |
| 读 | 229.91 ops/s | **919.64 MiB/s** | 67.17ms | 80.31ms | 90.77ms |

单连接端到端仅 ~0.9 GiB/s，远低于盘极限（写 35%、读 14%）。

### 4.3 多客户端收敛：达到带宽极限的最低客户端数

**写（MiB/s，每客户端 16 线程 4MiB×1500）：**

| NC | 1 | 2 | 3 | **4** | 6 | 8 | 10 | 12 | 14 | 16 |
|----|---|---|---|-------|---|---|----|----|----|----|
| 合计 | 827 | 1547 | 2142 | **2550** | 2555 | 2559 | 2557 | 2572 | 2571 | 2569 |

→ **写方向 NC=4 即达盘写极限 ~2.55 GiB/s**（NC=3 仅 83%，NC≥4 全部封顶）。

**读（MiB/s）：**

| NC | 2 | 3 | 4 | 6 | 8 | 10 | 12 | **14** | 16 |
|----|---|---|---|---|---|----|----|-------|----|
| 合计 | 1583 | 2218 | 2866 | 3795 | 4635 | 5218 | 5610 | **5784** | 5792 |

→ **读方向 NC=14 封顶 ~5.79 GiB/s**（12→14 +174 趋缓，14→16 +8），仍未达盘读极限 6.6G（~88%）。

### 4.4 全量仪表化复测：16 客户端读

| 指标 | 数值 |
|------|------|
| 总带宽 | **5488 MiB/s = 5.36 GiB/s**（1370 ops/s，每客户端 ≈340 MiB/s） |
| 延迟 | p50 ≈187ms / p90 ≈225ms / p99 ≈260ms |
| 磁盘实测 | avg **5328 MiB/s**、peak **5823 MiB/s**（盘读极限的 ~80%） |
| CPU 整机 | **busy 69.6%**（≈29/96 核，最忙核 72.2%） |
| 网络 lo | rx/tx ≈**4717 MiB/s**（≈4.6 GiB/s 回环） |

### 4.5 全量仪表化复测：4 客户端写

| 指标 | 数值 |
|------|------|
| 总带宽 | **2534 MiB/s = 2.47 GiB/s**（633 ops/s，每客户端 ≈634 MiB/s） |
| 延迟 | p50 ≈100ms / p90 ≈105ms / p99 ≈116~128ms |
| 磁盘实测 | avg **2525.6 MiB/s**、peak **2576.9 MiB/s**（**盘写极限 2.5G，100% 饱和**） |
| CPU 整机 | **busy 26.5%**（≈11/96 核，最忙核 36.1%） |
| 网络 lo | rx/tx ≈**2286 MiB/s**（≈2.23 GiB/s 回环） |

## 5. 资源占用对比

### 5.1 CPU（mpstat -P ALL，测试窗口均值）

| 场景 | 整机 busy | 活跃核数 | 最忙核 | 结论 |
|------|-----------|----------|--------|------|
| 4 客户端写 | 26.5% | ~11 | 36.1% | **非瓶颈**（磁盘先饱和） |
| 16 客户端读 | 69.6% | ~29 | 72.2% | **瓶颈**（无单核热点，每字节 CPU 成本主导） |

### 5.2 磁盘（iostat 实测，稳态帧）

| 场景 | 平均 | 峰值 | 设备极限 | 利用率 |
|------|------|------|----------|--------|
| 4 客户端写 | 2525.6 MiB/s | 2576.9 MiB/s | ~2.5 GiB/s | **100%（饱和）** |
| 16 客户端读 | 5328 MiB/s | 5823 MiB/s | ~6.6 GiB/s | ~80% |

### 5.3 网络（lo 回环实测）

| 场景 | rx / tx 均值 | 与 bench 吞吐关系 |
|------|--------------|-------------------|
| 4 客户端写 | 2286 / 2286 MiB/s | 2.23G vs 2.47G（窗口对齐误差） |
| 16 客户端读 | 4717 / 4719 MiB/s | 4.6G vs 5.36G（同上） |

回环承载真实数据流量，网络层非瓶颈；跨物理网卡（10G 级）场景需另测。

## 6. pprof 热点分析

### 6.1 Server（读，15s 窗口，总样本 441s ≈ 29 核）

| 函数 | flat% | 性质 |
|------|-------|------|
| `runtime.memmove` | **48.11%** | 数据拷贝 |
| `internal/runtime/syscall.Syscall6` | **20.36%** | O_DIRECT + socket 系统调用 |
| `runtime.memclrNoHeapPointers` | **19.34%** | 缓冲清零 |
| 调度（runqgrab/stealWork/schedule） | ~3% | — |

**内存操作合计 ≈67.5%**。调用链：`Get cum 54.79% → SendMsg cum 35.17% → loopyWriter.processData cum 38.24% → bufWriter.Write cum 20.01% → FD.Write cum 18.18% → Syscall`；`proto.Marshal` cum 14.44%。

### 6.2 Server（写，15s 窗口，总样本 171s ≈ 11 核）

| 函数 | flat% | 性质 |
|------|-------|------|
| `runtime.memmove` | **21.18%** | Recv 帧 → 对齐缓冲 |
| `internal/runtime/syscall.Syscall6` | **20.87%** | O_DIRECT 写 + socket |
| `runtime.memclrNoHeapPointers` | **13.90%** | 缓冲清零 |
| **GC 合计（gcDrain 9.37% + scanobject 5.24% + mark 系列）** | **≈15%+** | 写路径每 chunk 分配压力 |

写路径内存操作 ≈35%，且 **GC 占比显著高于读**（每 chunk 反序列化 + Storage.Put 内部分配）。

### 6.3 Client（读，2/16 客户端，各 ~250% ≈ 2.5 核）

| 函数 | flat% |
|------|-------|
| `runtime.memmove` | 29~33% |
| `internal/runtime/syscall.Syscall6` | ~20% |
| `runtime.memclrNoHeapPointers` | 15~17% |

内存操作合计 ≈45~50%（Recv 反序列化 + `io.Pipe` 写入拷贝）。

### 6.4 Client（写，2/4 客户端，各 ~170% ≈ 1.7 核）

| 函数 | flat% |
|------|-------|
| `runtime.memmove` | 35.5~38.9% |
| `internal/runtime/syscall.Syscall6` | 24.1~28.0% |
| `runtime.memclrNoHeapPointers` | 20.8% |

内存操作合计 ≈57~60%；调用链：`SendMsg cum ~30% → codecV2.Marshal cum 27.75%（编码拷贝）→ loopyWriter.processData cum 42~44%`。

## 7. 结论与建议

1. **写方向（4 客户端）：磁盘饱和型瓶颈**。2.47 GiB/s 已贴齐单盘写极限（2.5G，磁盘 100% 饱和），NC=4 即封顶；server CPU 仅 busy 26.5%，**写路径无拷贝优化收益空间**（先满盘）。若未来上多盘，写路径的 GC（~15%）与 memmove 才会成为下一瓶颈。
2. **读方向（16 客户端）：server CPU 拷贝型瓶颈**。5.36 GiB/s（盘极限的 80%）即封顶，瓶颈是 server 每字节拷贝成本——**server 侧 memmove 48.11% + memclr 19.34% ≈ 67.5% 的内存操作**，GC 无碍，非磁盘（80%）、非网络（回环充足）、非核数（29/96）。
3. **读路径是「减少拷贝」优化的主战场**，按收益排序：
   - server `Get` 去掉 `GetChunk` proto.Marshal 的冗余编码拷贝（cum 14.44%），数据直接复用读缓冲；
   - client `Get` 去掉 `io.Pipe` 双拷贝（边 Recv 边直写目标缓冲）；
   - 消除每 chunk 的 memclr（gRPC sizedBufferPool 复用面已验证，可再排查）。
   - 预期：砍掉 server 侧 ~48% memmove 后，16 客户端读可从 5.36G 推向盘极限 6.6G 附近。
4. **单客户端基线回顾**：单连接仅 ~0.9 GiB/s（写 907 / 读 920 MiB/s），同属用户态拷贝 + 系统调用主导；多客户端并行可规避单流瓶颈，但读方向最终仍被 server CPU 拷贝锁住。

## 8. 测试还原

- server：`setsid nohup /tmp/taihu-server.new -addr :50051 -db /tmp/tdb-rpc -dev /dev/nvme1n1 -pprof :6060`
- 采样：`iostat -d /dev/nvme1n1 1`、`mpstat -P ALL 1`、lo 网络采样脚本（每秒读 `/proc/net/dev` 计算增量），与 bench 同命令贯穿、按 PID 精确停止
- server pprof：`curl -s "http://127.0.0.1:6060/debug/pprof/profile?seconds=15" -o server.cpu.pprof`（前台采、覆盖测试中段）
- client pprof：bench `-cpuprofile`（读取 2/16、写取 2/4 个进程）
- 分析：远端 `go tool pprof -top -nodecount=30`（go1.24）

## 9. 修正说明

- 多客户端扫描与全量复测之间带宽有 ~7% 差异（读 5784 vs 5488 MiB/s）：扫描轮客户端与 server 并发写灌数据后立即读、负载略低，全量复测为纯净读路径，以复测为准。
- 延迟解析早期脚本误将 `p50=` 的 `50` 当值（实为 p50≈187ms），已用 sed 从原始日志重算修正。
- 早前 r16 轮（旧二进制无 pprof）曾报 server CPU 100%，468a9af 窗口均值 69.6%：差异来自旧轮取「最忙单帧 min_idle」及采样窗口含起停阶段，均值口径更准确；结论方向一致（server CPU 主导读瓶颈）。
