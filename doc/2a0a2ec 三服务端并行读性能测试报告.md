# 2a0a2ec 三服务端并行读性能测试报告

> 对应提交：`2a0a2ec`（perf: device Append 地址对齐零拷贝快路径、读写帧限制 4M、客户端 Get 整对象同步聚合）
> 测试范围：**3 服务端 × 3 客户端并行 RPC 读**（各 32T/16C/4M，loopback，客户端与服务端同机）

## 1. 测试目标

验证多实例部署的聚合读能力：3 个 taihu-server（各绑一块 NVMe）× 3 个 taihu-rpc-bench 客户端（各 32 线程 / 16 连接 / 4M 对象）并行读时，物理机 CPU、聚合应用层带宽、延迟、loopback 网络、三盘磁盘的真实表现，并采集 3 客户端 + 3 服务端 pprof 定位聚合上限的来源。

## 2. 测试环境

| 项目 | 值 |
| --- | --- |
| 服务器 | 100.71.128.12（客户端 + 服务端同机，loopback） |
| CPU | 96 核 ARM aarch64（BCLinux 4.19，mpstat -P ALL 逐秒采样） |
| 内存 | 约 756 GiB |
| 数据设备 | /dev/nvme1n1、nvme2n1、nvme3n1（各 1.92 TB NVMe SSD，O_DIRECT 直读） |
| 元数据 | 各服务端独立 pebble 目录：/tmp/tdb（srv1）、/tmp/tdb2（srv2）、/tmp/tdb3（srv3） |
| 服务端 | /tmp/taihu-server.new ×3：srv1 :50051/:6061，srv2 :50052/:6062，srv3 :50053/:6063 |
| 客户端 | /tmp/taihu-rpc-bench.new ×3，各自直连对应服务端 |
| 对象 | 4 MiB（4194304 B），各客户端读 20000 个（80GB），key 源：q1-4m（srv1）/3w-b（srv2）/3w-c（srv3），均此前写入 |
| 通信参数 | chunkSize=4MiB，http2MaxFrameLen=4MiB+16，recv/send 上限 8MiB |

## 3. 测试方法

taihu-rpc-bench 参数：`-mode read -size 4194304 -threads 32 -conns 16 -count 20000 -keys-prefix <p> -latency -report-interval 2s -cpuprofile <clientN.cpu>`。

- 三组服务端同时启动，三客户端**并行**读各自服务端，同步进入稳态。
- 采集：`mpstat -P ALL 1`、`iostat -d nvme1n1 nvme2n1 nvme3n1 1`、`/proc/net/dev` loopback 每秒差分。
- pprof：三客户端 `-cpuprofile`；三服务端经各自 `:6061/6062/6063` 抓 `/debug/pprof/profile?seconds=10`（读测试短，入稳态 2s 后并行抓取）。
- **loopback 上限实测定标**（附录 A）：自研 netlo 工具（Go TCP，无依赖）对 127.0.0.1 单向上行多流压测，测得 lo 聚合能力 13.5-25.2 GB/s，用于判定网络是否构成瓶颈。

## 4. 测试结果

### 4.1 应用层带宽（三客户端并行）

| 客户端 | 带宽 MiB/s | ops/s | 对象 |
| --- | --- | --- | --- |
| C1（srv1/nvme1n1） | 3069.43 | 767.36 | 20000 |
| C2（srv2/nvme2n1） | 3075.67 | 768.92 | 20000 |
| C3（srv3/nvme3n1） | 3078.98 | 769.74 | 20000 |
| **聚合** | **~9224 MiB/s（9.0 GB/s）** | ~2306 | 60000（240GB） |

### 4.2 延迟（-latency，各客户端）

| 客户端 | p50 | p90 | p99 |
| --- | --- | --- | --- |
| C1 | 41.43ms | 51.54ms | 67.25ms |
| C2 | 40.95ms | 51.99ms | 71.80ms |
| C3 | 40.86ms | 51.45ms | 76.40ms |

三实例延迟一致（p50≈41ms），较单实例 32T16C 读（19.82ms）显著上升——三实例共享整机 CPU/系统资源，单 op 排队加剧。

## 5. 资源占用

### 5.1 CPU（mpstat -P ALL，整机）

- busy **79.4%**，idle 20.6%；**实占 usr+sys = 53.8%**（usr 25.6 + sys 28.2），iowait 22.6%。
- 三实例叠加后整机系统负载高企（sys 28.2% 为 syscall 密集），接近整机处理上限但未完全打满（仍余 20.6% idle）。

### 5.2 磁盘（iostat 实测，三盘独立稳态）

| 盘 | 读均值 | 峰值 | tps |
| --- | --- | --- | --- |
| nvme1n1 | 2956.4 MiB/s | 3630.2 | 23660 |
| nvme2n1 | 2962.7 | 3401.2 | 23704 |
| nvme3n1 | 2963.0 | 3396.9 | 23712 |
| 聚合 | ~8.9 GB/s | ~10.4 GB/s | ~71000 |

**磁盘未用满**：每盘稳态仅 2.96 GB/s，而单盘单独压测可达 5.9-6.4 GB/s（峰值 6.6），本次 PEAK 也仅 3.4-3.6——盘被上游资源压住，非盘瓶颈。

### 5.3 网络（loopback /proc/net/dev 差分实测）

- **RX_AVG=9340.2 / TX_AVG=9340.2 MiB/s**（PEAK 10345.1），双向对称，数值与应用层聚合一致。
- **附录 A 实测定标**：lo 单向上行聚合能力 13.5（8 流）/ 15.2（16 流）/ 25.2（32 流）/ 23.5（48 流）GB/s —— **lo 硬上限远超本次 9.34GB/s 用量**。

## 6. pprof 热点（3 客户端 + 3 服务端）

### 客户端（同构 ×3，各 ~1045% CPU ≈ 10.4 核）

| 热点 | flat | 说明 |
| --- | --- | --- |
| runtime.memmove | 44.92% | Get 收流聚合 `f.CopyTo(out[pos:])` 单帧搬运 |
| syscall.Syscall6 | 36.17% | socket 读（数据收帧） |
| memclrNoHeapPointers | 10.34% | 返回缓冲清零 |

### 服务端（同构 ×3，各 ~403% CPU ≈ 4 核）

| 热点 | flat | 说明 |
| --- | --- | --- |
| syscall.Syscall6 | 73.21% | O_DIRECT 设备读 + wire writev 推流 |
| memclrNoHeapPointers | 18.05% | bufpool 对齐缓冲清零 |
| 无 memmove 热点 | — | 服务端读零拷贝 sendpath 有效 |

## 7. 瓶颈分析（重要修正）

聚合吞吐三方吻合（应用层 9.22 ≈ 磁盘 8.9 ≈ lo 9.34），初看像"lo 撞顶"，但**附录 A 实证推翻了 lo 瓶颈论**：

- lo 单向上行实测可达 **13.5-25.2 GB/s**（远超 9.34 用量），网络路径不是硬上限；
- 磁盘每盘仅 2.96 GB/s（单盘能力 6.4），盘也远未打满；
- 真正的约束是**整机 CPU 对读路径的处理能力**：3 实例合计客户端 ~10.4 核 ×3 + 服务端 ~4 核 ×3 ≈ 43 核的 gRPC 读处理（memmove/syscall/memclr 三级开销），叠加磁盘等待（iowait 22.6%）与 sys 28.2%，系统在 ~9.3 GB/s 处达到处理平衡点——此时每 op 排队（延迟 41ms），盘侧 iowait 显示队列在等 CPU 持续推进；
- 单实例读 6.39 GB/s 时 CPU 实占仅 20.4%，盘为瓶颈；三实例把 CPU 处理推到 ~54% 实占 + 22.6% iowait，系统级处理率先触顶 9.3GB/s。**9.34 = lo 计数与应用层一致的巧合**（lo 只是搬运了多少流量），不能凭 lo 数值判定瓶颈，须靠能力实测定标。

## 8. 结论与建议

1. **聚合 9.0 GB/s 读**（各客户端 ~3.07GB/s），三实例并行未能线性叠加（3×6.39=19.2 → 实际 9.0，仅 47%）；
2. **瓶颈是整机 CPU 读路径处理**（gRPC 收/推流 memmove+syscall+memclr），磁盘（余量 ~50%）与 loopback（实测能力 25GB/s）均未用满；
3. 单客户端带宽 6.39→3.07、延迟 19.8→41ms，均为三实例共享 CPU 所致；
4. **优化方向**（复用单实例结论、多实例下直接转化为聚合收益）：
   - 服务端读路径 memclr 18%（bufpool 对齐缓冲清零）→ 改首末块补零；
   - 客户端收流 memmove 45% + memclr 10% → 池化 + 减少返回缓冲清零；
   - 服务端 sendpath 已无 memmove（零拷贝写 wire 生效），继续压 syscall 次数。
   - 若需跨实例线性扩展，考虑跨机部署（物理网卡）以绕开同机 CPU/lo 共享。

## 附录 A：loopback 能力实测定标（netlo）

自研 Go TCP 工具（client 单进程多流 4M 块上行写，server 读丢弃）：

| 并发流 | 聚合上行 MiB/s |
| --- | --- |
| 8 | 13528.7 |
| 16 | 15195.7 |
| 32 | 25213.7 |
| 48 | 23537.3 |

lo（CPU 直连内存拷贝）能力 13-25 GB/s，本次三实例读聚合 9.34GB/s 远低于该值，确认网络非瓶颈。