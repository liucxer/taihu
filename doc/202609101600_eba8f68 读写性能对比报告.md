# eba8f68 读写性能对比报告（sync.Pool 对齐缓冲池化验证）

> 优化提交：`eba8f68`（perf: 对齐缓冲 sync.Pool 分桶池化，消除读写路径高频大块堆分配）
> 基线提交：`461d092`（bench: 增加 -cpuprofile/-memprofile(pprof) 与 -latency 写延迟统计）
> 对比测试：128.12，同一批 20000 × 4MiB key，写线程 4 / 读线程 16

## 1. 测试目标

- 验证 sync.Pool 分桶池化后读路径 GC 热点是否消除、CPU 是否下降。
- 确认读写带宽、延迟不被优化破坏，且读带宽是否能进一步贴齐设备上限。

## 2. 优化内容

新增 [bufpool.go](file:///Users/liucx/gopath/src/github.com/liucxer/taihu/bufpool.go)：按 2 的幂分桶（4KB~8GB）的 `sync.Pool` 对齐缓冲池。

- [device_linux.go](file:///Users/liucx/gopath/src/github.com/liucxer/taihu/device_linux.go#L21-L59)：`read` 从池取缓冲，返回 `io.ReadCloser`，`Close` 归还。
- [storage.go](file:///Users/liucx/gopath/src/github.com/liucxer/taihu/storage.go#L106-L125)：`Get` 透传 Close 闭环归还。
- [device.go](file:///Users/liucx/gopath/src/github.com/liucxer/taihu/device.go#L67-L73)：`append` 写缓冲同样池化，`defer` 归还。

原问题：读/写每次 O_DIRECT io 都 `make` 4MiB+ 堆缓冲（读 ≈6.5GB/s、写 ≈2.5GB/s 堆分配），读路径 GC 占 CPU 热点 ~64%（gcDrain）。

## 3. 测试环境

| 项目 | 配置 |
|------|------|
| 服务器 | 128.12（100.71.128.12） |
| 编译产物 | `taihu-bench`（Go 1.23.12，aarch64），对比 461d092 / eba8f68 |
| 数据设备 | `/dev/nvme1n1`（O_DIRECT） |
| 元数据 | Pebble，`/tmp/tdb1` |
| 数据规模 | 20000 × 4MiB 对象（写 `p4m-pool` 前缀新 key；读同批，每 key 全进程只读一次） |

## 4. 测试方法

- 写：`-mode write -size 4194304 -count 20000 -threads 4 -latency -cpuprofile ...`
- 读：`-mode read -size 4194304 -count 20000 -threads 16 -latency -cpuprofile ...`（读前 LoadCache 预热元数据）
- 期间并行采集 `iostat -d`（真实磁盘带宽）、`mpstat -P ALL`（完整逐核 CPU）；结束取 pprof。
- 注：461d092 写测试为旧版 bench（无 `-latency`），写延迟仅有历史样本 p50≈6.18ms，无 p90/p99；eba8f68 为完整分位。

## 5. 测试结果与对比

### 5.1 读（线程 16）

| 指标 | 461d092 | eba8f68 | 变化 |
|------|---------|---------|------|
| 吞吐 | 6529.44 MiB/s（1632.36 ops/s） | **6799.14 MiB/s（1699.79 ops/s）** | **+4.1%** |
| 延迟 p50 / p90 / p99 | 9.74 / 11.94 / 14.24 ms | **9.37 / 10.91 / 12.38 ms** | p99 **-13%** |
| CPU all 均值 busy / idle | 23.8% / 76.2% | **12.4% / 87.6%** | **busy -48%** |
| CPU usr / sys / iowait | 12.8% / 1.9% / 8.4% | **1.3% / 0.8% / 9.8%** | **usr -90%**（GC 消失） |
| 最忙核（修正口径） | core24 busy 38.5%（usr 27.6%） | core14 busy 73.1%（usr 3.8%、iowait 63.4%） | usr 大幅↓，转为 iowait |
| 磁盘读带宽（iostat 实测） | avg 5532.9 / peak 6570.2 MiB/s | avg 5507.8 / peak **6814.1 MiB/s** | 峰值 +3.7% |

### 5.2 写（线程 4）

| 指标 | 461d092 | eba8f68 | 变化 |
|------|---------|---------|------|
| 带宽 | 2560.51 MiB/s | 2556.72 MiB/s | 持平（设备瓶颈） |
| 延迟 p50 / p90 / p99 | p50≈6.18 ms（无 p90/p99） | **6.16 / 6.23 / 8.54 ms** | 补齐分位 |
| CPU all 均值 busy / idle | — / 95.09% | 4.1% / 95.9% | 持平（本就极低） |
| 最忙核（修正口径） | — | core41 busy 42.5%（iowait 35.7%） | — |
| 磁盘写带宽（iostat 实测） | peak 2600.8 MiB/s | avg 2409.9 / peak 2597.9 MiB/s | 持平 |

## 6. pprof 热点对比（读，用户态）

| 461d092（GC 主导） | flat% | eba8f68 | flat% |
|---------------------|-------|---------|-------|
| `runtime.gcDrain`（cum） | 64.29% | `runtime.memmove` | 55.13% |
| `runtime.(*gcWork).tryGetFast` | 16.84% | `internal/runtime/syscall.Syscall6` | 38.17% |
| `runtime.(*lfstack).pop` | 13.14% | `runtime.(*gcBits).bitp` | 0.61% |
| `runtime.markBits.setMarked` | 10.43% | `runtime.mallocgc` | 0.43% |
| `runtime.memclrNoHeapPointers` | 9.84% | — | — |

- **GC 类热点全部消失**；残余 `memmove 55%` 为 `io.Copy(io.Discard, rc)` 从读缓冲拷出（bench 侧丢弃，语义拷贝），`Syscall6 38%` 为 O_DIRECT 读系统调用。两者之和占 ~93%，已是「读 + 落库」本身成本。
- 写侧 `memclr`（缓冲清零）由 12.9% 归零，残余热点为 memmove（数据搬运，必需）+ Syscall6（写系统调用）。

## 7. 结论

1. **读 CPU 用户态计算下降 ~90%**（usr 12.8%→1.3%），整机 busy -48%，GC 热点（gcDrain 64%）完全消除——对齐缓冲池化命中要害。
2. **读带宽 +4.1% 至 6799.14 MiB/s**，磁盘读峰值 6814.1 MiB/s 贴齐设备上限；延迟 p99 -13%。
3. **写性能持平**（2556.72 MiB/s，设备 util/IO 队列为瓶颈），CPU 本就极低（idle 95.9%），池化消除 memclr 无带宽收益，符合预期。
4. 下一步可优化点：`io.Copy` 到 `io.Discard` 的语义拷贝（memmove 55%）——如提供直读缓冲的接口可再省这一份拷贝。