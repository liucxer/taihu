# 20260913 四模式 4M IO 读对比测试报告（storage / shm / rpc / rpc·多连接）

日期：2026-09-13
对应提交：`d57edf9`（共享内存池扩容 2GiB + shmMaxParallel=8）
测试节点：100.71.128.13（单节点，96 核 aarch64）

## 1. 测试目标

在**单个客户端 + 单个服务端**、**同一块盘**的场景下，对比 4M IO 读的**四种**测法上限（其中 rpc 细分单连接与多连接两档）：

| 场景 | 工具 | 链路 |
|------|------|------|
| storage | `taihu bench storage -mode read` | 纯 Storage 层直测（Pebble meta + O_DIRECT/libaio），无网络 |
| shm | `taihu bench single -mode read -transport shm` | 客户端 → shmipc 共享内存 → taihu-server → Storage → 盘 |
| rpc（单连接） | `taihu bench single -mode read -transport rpc -conns 1` | 客户端 → 单 TCP(netpoll) → taihu-server → Storage → 盘 |
| rpc（多连接） | `taihu bench single -mode read -transport rpc -conns 2/4/8/16` | 客户端 → 多 TCP(netpoll) 连接并行 → taihu-server → Storage → 盘 |

指标：读带宽上限、延迟（p50/p90/p99）、应用进程 CPU、物理机整体 CPU、磁盘稳态带宽。

## 2. 测试环境

| 项 | 值 |
|----|----|
| 代码版本 | d57edf9（2GiB 池 + shmMaxParallel=8） |
| 编译产物 | `/tmp/taihu.p8`（GOARCH=arm64，本地交叉编译后经 9527 上传） |
| 服务端 | `TAIHU-SINGLE`（--listen 100.71.128.13，rpcPort=50000，shm socket /dev/TAIHU-SINGLE） |
| 数据设备 | /dev/nvme3n1（**storage 与 server 共用同一块盘**，串行测试） |
| 元数据 | storage: /tmp/tdb-st；server: /tmp/tdb-single（测试前清空） |
| 对象大小 | 4 MiB（-size 4194304） |
| 数据量 | count=20000（≈80 GiB），三种场景分别自行灌写同规格数据 |
| 线程数 | 64 / 128 两档 |
| TiKV | --pd 100.71.128.11:2379（server 注册/容量依赖） |
| 采样 | iostat -d nvme3n1（磁盘）；sar -u（整机 CPU）；pidstat -u -p bench,server（进程 CPU） |

## 3. 测试方法

```bash
# storage 直测（自行写数据 → 读）
/tmp/taihu.p8 bench storage -mode write -db /tmp/tdb-st -dev /dev/nvme3n1 -size 4194304 -count 20000 -threads 2
/tmp/taihu.p8 bench storage -mode read --db /tmp/tdb-st --dev /dev/nvme3n1 -size 4194304 -count 20000 -threads 64|128 --latency

# 服务端（单实例，与 storage 同盘 nvme3n1）
/tmp/taihu.p8 server --listen 100.71.128.13 --db /tmp/tdb-single --dev /dev/nvme3n1 \
  --server-name TAIHU-SINGLE --pd 100.71.128.11:2379

# shm 读
/tmp/taihu.p8 bench single -mode read -transport shm -shm /dev/TAIHU-SINGLE -size 4194304 -count 20000 -threads 64|128 --latency

# rpc 读（单 TCP 连接，-conns 默认 1）
/tmp/taihu.p8 bench single -mode read -transport rpc -addr 100.71.128.13:50000 -size 4194304 -count 20000 -threads 64|128 --latency

# rpc 多连接扫描（-conns 2/4/8/16，128 线程固定）
/tmp/taihu.p8 bench single -mode read -transport rpc -addr 100.71.128.13:50000 -conns 4 -size 4194304 -count 20000 -threads 128 --latency
```

每次读测试期间并行采集：iostat（磁盘）、sar -u（整机 CPU）、pidstat -u（bench + server 进程 CPU）。

## 4. 测试结果

### 4.1 应用带宽与延迟

| 场景 | 线程 | 读带宽 MiB/s | ops/s | p50 | p90 | p99 | fallback |
|------|------|-------------|-------|-----|-----|-----|---------|
| **storage** | **64** | **6640.70** | 1660.17 | 38.17 | 41.70 | 48.00 | - |
| storage | 128 | 6351.22 | 1587.80 | 76.72 | 81.93 | 211.57 | - |
| **shm** | **128** | **5972.15** | 1493.04 | 76.53 | 81.78 | 256.29 | 0 |
| shm | 64 | 5872.58 | 1468.14 | 38.20 | 46.30 | 135.94 | 0 |
| **rpc** | **64** | **2344.92** | 586.23 | 106.21 | 123.24 | 141.50 | 0 |
| rpc | 128 | 2260.75 | 565.19 | 227.54 | 236.54 | 259.23 | 0 |

### 4.2 磁盘实测带宽（iostat 稳态样本）

| 场景 | 线程 | 磁盘 avg MiB/s | 峰值 MiB/s | tps |
|------|------|---------------|-----------|-----|
| storage | 64 | 6677.3 | 6723.0 | 53419 |
| storage | 128 | 6157.9 | 6725.4 | 49266 |
| shm | 64 | 6008.5 | 6724.8 | 48068 |
| shm | 128 | 5998.1 | 6723.1 | 47985 |
| rpc | 64 | 2345.9 | 2539.0 | 18767 |
| rpc | 128 | 2268.6 | 2481.9 | 18149 |

### 4.3 CPU 使用（整机与进程）

| 场景 | 线程 | 整机 user/sys/iowait | bench+server 进程 %usr/%sys |
|------|------|---------------------|-----------------------------|
| storage | 64 | 0.46 / 1.00 / 0.24 | 6.3 / 35.8 |
| storage | 128 | 0.32 / 1.12 / 0.04 | 8.1 / 51.7 |
| shm | 64 | 0.88 / 1.44 / 0.10 | 12.0 / 30.1 |
| shm | 128 | 0.46 / 1.21 / 0.15 | 12.2 / 29.8 |
| rpc | 64 | 0.42 / 2.54 / 0.00 | 7.8 / 92.3 |
| rpc | 128 | 0.55 / 2.64 / 0.00 | 7.6 / 93.6 |

### 4.4 rpc 多连接（-conns）扩展扫描：读带宽上限

单还是多 TCP 连接决定 rpc 上限，固定 128 线程逐档扫描（数据 80 GiB，同 server 同盘）：

| conns | 读带宽 MiB/s | ops/s | p50 | p90 | p99 | 磁盘 avg MiB/s | 磁盘峰值 MiB/s | tps | bench+server 进程 %usr/%sys |
|-------|-------------|-------|-----|-----|-----|---------------|---------------|-----|------------------------------|
| 1 | 2260.75 | 565.19 | 227.54 | 236.54 | 259.23 | 2268.6 | 2481.9 | 18149 | 7.6 / 93.6 |
| 2 | 4121.10 | 1030.28 | 117.71 | 193.98 | 210.55 | 4855.1 | 6708.5 | 38841 | 12.0 / 165.6 |
| **4** | **6640.38** | 1660.10 | **76.44** | 80.40 | 86.05 | **6657.2** | 6708.9 | 53258 | 12.2 / 153.0 |
| 8 | 6631.49 | 1657.87 | 76.46 | 80.98 | 88.50 | 6390.8 | 6736.1 | 51126 | 17.7 / 229.4 |
| 16 | 6652.97 | 1663.24 | 76.27 | 80.19 | 85.64 | 6660.3 | 6729.8 | 53283 | 14.5 / 184.4 |

> conns=1 行沿用 §4.1/4.2/4.3 rpc 128t 同次测量（数字与此处来源一致）；conns≥4 磁盘 avg≈峰值（6657/6709），盘已打满。

### 4.5 rpc 复测（128 线程 / 4 连接）：整机与进程 CPU 明细

对 §4.4 conns=4 档做独立复测（80 GiB 数据，同 server 同盘），确认带宽稳定并细粒度统计 CPU（sar 整机 + pidstat 分进程，均裁剪到测试窗口 03:51:00~03:51:17 PM 排除前后空闲）：

| 指标 | 值 |
|------|-----|
| 读带宽 MiB/s / ops/s | **6643.40** / 1660.85 |
| p50 / p90 / p99 | 76.30 / 80.91 / 88.52 ms |
| 磁盘 avg / 峰值 / tps | 6653.2 / 6722.1 / 53225 |
| 整机 user / sys / iowait / idle | 0.71% / 5.37% / 0.07% / **88.29%** |
| server 进程 usr / sys（%核） | 12.7 / **221.4**（≈2.34 核，sys 95%） |
| bench 客户端 usr / sys（%核） | 38.1 / **310.4**（≈3.49 核，sys 89%） |

- 复测带宽 6643.40 与首测 6640.38 差 0.05%，磁盘 avg 6653≈峰值 6722 仍顶满，结果可复现。
- **rpc 读峰值期整机 idle 88.3%、约 12% 核活跃，CPU 富余**；两进程合计约 5.8 核，其中 sys 占 91%+（server 2.3 核 / bench 3.5 核，均为内核 TCP 收发与拷贝开销），usr 仅 0.5 核。

## 5. 分析与结论

### 5.1 读带宽上限三个层级

**storage（6640）= rpc·多连接（6640）> shm（5972）> rpc·单连接（2345）MiB/s**

1. **storage 64t 最优 6640 MiB/s**：磁盘稳态 avg 6677 ≈ 峰值 6723，直测链路把盘顶满；128t 过饱和（磁盘 avg 降到 6158）。
2. **shm 128t 最优 5972 MiB/s**：磁盘峰值与 storage 相同（6723~6725，盘硬件能力一致），但**稳态 avg 只维持到 ~6000**（< storage 的 6677）。根因仍是服务端读路径"批量 Reserve → 并行 DMA → 整批 Flush"的批同步节奏无法像直测那样持续维持满磁盘队列深度，同盘对比 storage 丢 **~10%**。
3. **rpc 单连接（conns=1）64t 仅 2345 MiB/s**：**单 TCP 连接成为链路瓶颈**——磁盘 avg 2346 ≈ 应用 2345，连盘峰值 2539 都没跑起来；每帧 4MiB 的 TCP 收发包 + 拷贝开销把上限钉在 ~2.3 GiB/s。128t 无提升（2261），p50 排队延迟翻倍（227ms）。
4. **rpc 多连接（-conns）打破单连接瓶颈，conns=4 即追平 storage**：见 §4.4，conns=2 → 4121 MiB/s（磁盘 avg 仅 4855，未打满），**conns=4 → 6640.38 MiB/s，磁盘 avg 6657≈峰值 6709，盘已顶满，与 storage 直测 6640.70 持平（差 0.005%）**；conns=8/16 无提升（6631/6653，±0.2% 噪声），说明 **rpc 读上限 = 磁盘能力 ≈ 6.64 GiB/s，由盘决定而非链路**。

### 5.2 CPU

- 各场景整机 CPU 均很低（<3% 总核占比，96 核空闲充裕），瓶颈均不在 CPU。
- **系统态（sys）占比高**是显著特征：rpc 进程 sys 达 **90%+**（4MiB 大帧 TCP 收发 + 内核拷贝开销最大，多连接时总消耗随带宽上升按比例增长、占比不变）、storage 次之（libaio 系统调用 36~52%）、shm 最低（28~30%，零拷贝传输内核介入最少）。
- **rpc 多连接（4conns）CPU 明细（§4.5）**：读峰值期两进程合计约 **5.8 核**（sys 91%+：server 2.3 核全 sys、bench 3.5 核内 sys 89%），整机 96 核仅 ~12% 活跃（idle 88.3%）。rpc 的 CPU 消耗是 end-to-end 铁律开销，多连接只是把它分散到多个 socket 上，总量仍由带宽规模决定。

### 5.3 结论

- 单客户端单服务端场景下，读上限：**storage 直测 = rpc 多连接 = 6.64 GiB/s（盘顶满）> shm 链路 = 5.97 GiB/s > rpc 单连接 = 2.34 GiB/s**。
- **rpc 若要追平 storage，只需 `-conns≥4` 多连接并行**：单连接 2.3 GiB/s 是单 TCP 连接的固有上限，多连接把该瓶颈打散后可完全跑满盘（conns=4 起追平直测，conns=2 与 4 之间接近线性爬升，4 之后按盘上限平台）。
- **shm 与 storage 的 ~10% 差距是当前唯一显著链路损耗**，根因在服务端写回共享内存的批同步节奏（参见本报告 §8/§9 多块并行 DMA 与池扩容的已有优化方向），且在 4M IO 场景下实现的端到端带宽反而不如简单堆 rpc 连接的方案。
- 三种模式下进程 sys 占比均高（rpc 单/多连接均 90%+，storage 36~52%，shm 28~30%），但整机 CPU 均远未打满，CPU 均非瓶颈。

## 6. 附：环境状态

测试结束后 128.13 保留 `TAIHU-SINGLE` 单实例与灌好的 80GiB 数据（/tmp/tdb-single + nvme3n1），可随时复测 shm/rpc（含 `-conns` 多连接）；storage 直测数据目录 /tmp/tdb-st 已清理，复测需重新灌写。