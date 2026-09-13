# fc70f49 taihu bench single（shm）4M IO 读写性能测试报告（128.13）

日期：2026-09-13
对应提交：`fc70f49`（refactor: 合并为统一 taihu 命令（cobra 子命令树），含 `bench single`）
测试节点：100.71.128.13（单节点，同机 shm）

## 1. 测试目标

1. 用 **taihu bench single**（单机直通压测，rpcclient 直连 taihu-server，不查 TiKV）做 4 MiB IO 读写测试，`-transport shm`（同机共享内存 IPC，shmipc）。
2. 对比 [c8336a2 taihu-storage-bench 4M IO 读写性能测试报告](./20260913_c8336a2%20taihu-storage-bench%204M%20IO%20读写性能测试报告.md) 的裸盘 Storage 层基线，量化**端到端链路（客户端 → shmipc → taihu-server → Storage → 裸盘）相对纯 Storage 层的带宽/延迟开销**。
3. 验证 shm 单帧传输的零拷贝命中（take/copy 帧统计）。

## 2. 测试环境

| 项 | 值 |
|----|----|
| 节点 | 100.71.128.13（96 核 aarch64，内存 754G） |
| 编译产物 | `/tmp/taihu`（commit `fc70f49`，GOARCH=arm64，含 server + bench single） |
| 服务端 | `taihu server` 单实例 `TAIHU-SINGLE`（--listen 100.71.128.13，rpcPort=50000，pprof=50001） |
| shm socket | `/dev/TAIHU-SINGLE`（unix socket，与 TCP 并行监听） |
| 数据设备 | /dev/nvme3n1（1.7T 空闲裸盘，无分区、无进程占用） |
| 元数据 | pebble（/tmp/tdbsingle，测试前清空重建） |
| 对象大小 | 4 MiB（-size 4194304） |
| 数据量 | count=20000（≈80 GiB） |
| 线程数 | 写 2（shm 2t）/ 读 32（shm 32t），对齐 storage 报告最佳参数 |
| TiKV | --pd 100.71.128.11:2379（server 容量记录/集群注册依赖） |
| 采样工具 | iostat -d nvme3n1（磁盘稳态带宽，按 iostat-bw 规则剔除首行累计与收尾残差样本） |

## 3. 测试方法

```bash
# 启动服务端（shm socket 固定 /dev/<server-name>）
/tmp/taihu server --listen 100.71.128.13 --db /tmp/tdbsingle --dev /dev/nvme3n1 \
  --server-name TAIHU-SINGLE --pd 100.71.128.11:2379

# 写（2 线程，80 GiB）
/tmp/taihu bench single --mode write --transport shm --shm /dev/TAIHU-SINGLE \
  --size 4194304 --count 20000 --threads 2 --report-interval 5s --latency

# 读（32 线程，同批 key）
/tmp/taihu bench single --mode read --transport shm --shm /dev/TAIHU-SINGLE \
  --size 4194304 --count 20000 --threads 32 --report-interval 5s --latency
```

写/读分别后台并行 iostat 逐秒采样；bench 自带帧统计（transport-tx/rx data4MiB、take/copy）。

## 4. 测试结果

### 4.1 应用带宽与延迟（bench single shm 实测）

| 模式 | 线程 | ops/s | 带宽 MiB/s | p50 | p90 | p99 | 耗时 |
|------|------|-------|-----------|-----|-----|-----|------|
| 写 | 2 | 635.28 | **2541.14** | 3.08ms | 3.17ms | 4.63ms | 31.48s |
| 读 | 32 | 1563.65 | **6254.58** | 19.98ms | 24.53ms | 29.03ms | 12.79s |

### 4.2 磁盘实测带宽（iostat 稳态样本，iostat-bw 规则）

| 模式 | 稳态均值 | 峰值 | 稳态样本数 |
|------|---------|------|--------|
| 写 | 2539.0 MiB/s | 2598.2 MiB/s | 31 |
| 读 | 6231.9 MiB/s | 6328.6 MiB/s | 12 |

- 应用/磁盘吻合：写 2541.1 vs 2539.0（差 0.08%），读 6254.6 vs 6231.9（差 0.36%）。

### 4.3 帧统计（零拷贝验证）

| 方向 | dataFrames | dataBytes | data4MiB | take | copy |
|------|-----------|-----------|----------|------|------|
| 写 tx | 20000 | 83886080000 | 20000 | - | - |
| 读 rx | 20000 | 83886080000 | 20000 | **20000** | **0** |

- 全部 4 MiB 整块帧；**读侧 20000 帧全部 TakeTry 命中（take=20000，copy=0）**——shm 单帧路径零拷贝移交缓冲，与设计文档一致。

## 5. 与 storage 裸盘基线对比（同机 128.13，4 MiB）

| 指标 | storage 裸盘基线 | bench single shm | 差异 |
|------|----------------|------------------|------|
| 写带宽（2t） | 2565.8 MiB/s | 2541.1 MiB/s | **-0.96%** |
| 写 p50 / p99 | 3.08 / 3.95 ms | 3.08 / 4.63 ms | p50 持平，p99 +0.7ms |
| 写磁盘稳态 | 2500.0 MiB/s | 2539.0 MiB/s | 磁盘侧吻合（+1.6%，应用压满） |
| 读带宽（32t） | 6748.5 MiB/s | 6254.6 MiB/s | **-7.32%** |
| 读 p50 / p99 | 18.76 / 26.24 ms | 19.98 / 29.03 ms | +1.2 / +2.8 ms |
| 读磁盘稳态 | 6666.7 MiB/s | 6231.9 MiB/s | -6.5%（与应用带宽同步回落） |

> storage 基线数据取自身报告 §6.1/§6.2（写 2t、读 32t 行）。

## 6. 结论

1. **shm 写几乎无开销（-0.96%）**：2 线程下 shmipc 传输开销可忽略，带宽仍打满磁盘写上限（磁盘 2539 MiB/s），p50 与裸盘完全一致（3.08ms）。
2. **shm 读 32 线程损耗 -7.3%**：带宽 6254.6 vs 6748.5 MiB/s，磁盘同样回落（6231.9 vs 6666.7），说明**损耗不在磁盘而在端到端链路**——32 线程高并发下共享内存单 socket 通道串行化成为次要瓶颈；延迟同步上升（p50 +1.2ms）。
3. **零拷贝验证通过**：读 20000 帧 TakeTry 全命中、copy=0，shm 单帧路径无拷贝；损耗来源是 shm 通道并发能力而非帧拷贝。
4. **瓶颈分层**：写=磁盘；读=shm 单通道并发（次要瓶颈）叠加磁盘接近上限。若追求更高读带宽，可改用 `-transport rpc`（多地址多连接并行）或 `bench cluster` 多实例汇聚，预期可追平裸盘基线。
