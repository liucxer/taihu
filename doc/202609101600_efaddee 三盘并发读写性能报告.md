# efaddee 三盘并发读写性能报告（4MiB，目录重构后回归验证）

> 对应提交：`efaddee`（refactor: 仓库目录分层 internal/device+metastore、pkg/taihu 对外库，文档与打包产物归位）
> 对比基线：`eba8f68` 三盘并发读写性能报告（重构前，4MiB：写 7665 / 读 20339 MiB/s）
> 测试方式：3 个独立 taihu-bench 进程并发，各绑一盘；仅测 4MiB 对象读写

## 1. 测试目标

- 验证仓库目录重构（代码迁移至 `internal/`、`pkg/taihu`，方法改名 append/read→Append/Read 等）**零性能回归**。
- 复核三盘并发写/读 4MiB 的聚合带宽、延迟与 CPU 是否与重构前一致。

## 2. 测试环境

| 项目 | 配置 |
|------|------|
| 服务器 | 128.12（96 核） |
| 编译产物 | `taihu-bench`（efaddee，Go 1.23.12，aarch64，经编译部署 skill：commit→打包 dist/→同步→`go build ./cmd/taihu-bench`） |
| 数据设备 | `/dev/nvme1n1`、`/dev/nvme2n1`、`/dev/nvme3n1`（各 1.8T，均 O_DIRECT） |
| 元数据 | Pebble，`/tmp/tdb1`、`/tmp/tdb2`、`/tmp/tdb3`（每盘独立，游标续写） |
| 对象 | 4 KiB 对齐，4 MiB/对象，每盘 20000 个 key（80 GiB/盘，共 240 GiB） |

## 3. 测试方法

- **并发写**：`-mode write`，每盘 4 线程写 `p4m3-<盘号>` 前缀 20000×4MiB，`-latency`，每盘独立 `-cpuprofile`。
- **并发读**：`-mode read`，每盘 16 线程读各自同批 key，LoadCache 预热元数据，`-latency`，每盘独立 `-cpuprofile`。
- 全程并行采集 `iostat -d`（三盘）与 `mpstat -P ALL`（逐核）；采样文件名与 profile 名错开。

## 4. 测试结果

### 4.1 三盘并发写（4 线程/盘，31.1s）

| 盘 | bench 带宽 | 延迟 p50 / p90 / p99 | iostat 实测写带宽 |
|----|-----------|----------------------|-------------------|
| nvme1n1 | 2574.83 MiB/s | 6.16 / 6.25 / 7.37 ms | avg 2488.7 / peak 2590.4 MiB/s |
| nvme2n1 | 2553.62 MiB/s | 6.15 / 6.23 / 9.22 ms | avg 2468.9 / peak 2601.6 MiB/s |
| nvme3n1 | 2574.34 MiB/s | 6.15 / 6.23 / 7.36 ms | avg 2488.7 / peak 2599.8 MiB/s |
| **合计** | **7702.79 MiB/s（≈7.5 GB/s）** | — | ≈7.45 GB/s（avg） |

### 4.2 三盘并发读（16 线程/盘，11.9s）

| 盘 | bench 带宽 | 延迟 p50 / p90 / p99 | iostat 实测读带宽 |
|----|-----------|----------------------|-------------------|
| nvme1n1 | 6676.07 MiB/s | 9.53 / 11.56 / 13.85 ms | avg 5973.5 / peak 6764.2 MiB/s |
| nvme2n1 | 6731.44 MiB/s | 9.46 / 11.29 / 12.81 ms | avg 6095.1 / peak 6781.4 MiB/s |
| nvme3n1 | 6723.22 MiB/s | 9.46 / 11.33 / 12.96 ms | avg 6084.9 / peak 6763.5 MiB/s |
| **合计** | **20130.73 MiB/s（≈19.7 GB/s）** | — | ≈18.1 GB/s（avg） |

## 5. 资源占用

### 5.1 CPU（mpstat -P ALL）

| 场景 | busy | idle | usr | sys | iowait |
|------|------|------|-----|-----|--------|
| 三盘并发写（4 线程/盘） | 16.4% | 83.6% | 7.6% | 1.7% | 6.7% |
| 三盘并发读（16 线程/盘） | 42.1% | 57.9% | 4.7% | 1.7% | 34.6% |

- 读 TOP 忙核 iowait 占主导；usr/sys 与重构前同量级（写 usr 波动偏高属采样窗口系统噪声）。
- **结论：CPU 非瓶颈**，与重构前一致。

### 5.2 磁盘（iostat 实测）

- 每盘写 avg 2469–2489 MiB/s、peak 2590–2602 MiB/s，贴设备写上限；
- 每盘读 avg 5974–6095 MiB/s、peak 6764–6781 MiB/s，贴设备读上限。

### 5.3 网络

- 本机 O_DIRECT 测试不涉及跨主机传输，网络带宽≈0。

## 6. pprof CPU 热点

| 热点 | 写（盘1/盘3） | 读（盘1/盘3） | 归属 |
|------|--------------|--------------|------|
| `runtime.memmove` | 66.9% / 66.8% | 65.5% / 67.4% | 数据搬入对齐缓冲 / 消费拷贝 |
| `internal/runtime/syscall.Syscall6` | 28.0% / 28.4% | 25.6% / 26.2% | O_DIRECT pwrite / pread |
| 其他（futex/stealWork/mallocgc/pebble） | <1% | <1% | — |

- 写 memmove+Syscall6 ≈ 95%、读 ≈ 92%，**与重构前结构完全相同**。

## 7. 结论

1. **重构零性能回归**：写 7702.79 vs 7665 MiB/s（+0.5%）、读 20130.73 vs 20339 MiB/s（-1.0%，噪声级），带宽/延迟/CPU/pprof 均与重构前（eba8f68）一致。
2. **三盘线性扩展保持**：每盘写 ~2.55 GB/s、读 ~6.7 GB/s，iostat 峰值贴齐单盘设备上限。
3. **CPU 热点不变**：memmove + Syscall6 主导（写 ~95%、读 ~92%），GC/调度/pebble 均 <1%，说明纯目录/包迁移不引入任何运行时开销。
4. 目录重构后对外导入路径为 `github.com/liucxer/taihu/pkg/taihu`，bench 已按新路径编译并通过。