# NEFS taihu 新读路径 A/B 复测报告（服务端 per-stream 异步读流水线 + 客户端 GetBatch + --tcp-inflight）

- 日期：2026-09-30
- 环境：100.71.128.11/12/13（各 96 核），三节点 9 盘（每节点 nvme1/2/3），bond1+bond2 = 每节点 100Gbps（4×25G）全双工数据面
- 被测路径：**taihu bench cluster 直压 SDK**（避开 FUSE 客户端，排除 EFS_nefs 拷贝与 syscall），`--write-routing round-robin`（均衡）+ `--transport auto`（同机 shm / 跨节点 TCP）
- 对象规模：4 MiB / 对象，`--count` 40000/客户端（每客户端 ~160 GiB/prefix），12 客户端同数据复用
- 被测栈：
  - **新栈**：未提交新代码（服务端 per-stream 异步读流水线 + 客户端 GetBatch 调用），二进制 sha **873c9081**，server 带 `--tcp-inflight 64`；客户端 GetBatch 走 `--pipeline 8`
  - **基线栈**：git HEAD `d45e0db`（2026-09-29 17:48，即 9/29 报告同源、旧串行读路径），二进制 sha **1eb17144**，server 无 `--tcp-inflight`；基线二进制在 node 12 构建（glibc 2.28，全节点可跑）

## 1. 结果总览（三维：带宽 / 延迟 / CPU）

| 栈 | 集群读带宽 | p50 | p90 | p99 | 物理机 busy%（11/12/13） | 服务端 SER 核（11/12/13） | 客户端 CLI 核/节点 |
|---|---|---|---|---|---|---|---|
| 9/29 报告峰值 | **31554 MiB/s** | — | — | — | — | — | — |
| 基线栈 p1（HEAD，无 tcp-inflight） | **25698 MiB/s** | 12–14ms | 135–353ms | 3.6–6.7s | 27.9 / 33.7 / 21.0 | 3.2 / 4.1 / 4.7 | 10–11 |
| 新栈 p1（今日复测，--tcp-inflight 64） | **25788 MiB/s** | 12.5–12.7ms | 82–209ms | 4.7–7.8s | 24.7 / 31.9 / 32.8 | 4.0 / 3.9 / 4.7 | 6.3–7.7 |
| 新栈 p8（GetBatch） | **25633 MiB/s** | ×30 恶化 | — | — | — | — | — |

> 三栈带宽均在 ~25.6–25.8 GiB/s 区间（±0.6%）；9/29 的 31.5 GiB/s 峰值未能在同数据上复现。取/copy 恒为 1:2（见 §4）。

## 2. 带宽 A/B（12 客户端 × threads=128 × conns=4 × pipeline=1，MiB/s）

| 栈 | node11 | node12 | node13 | 合计 | 相对 9/29 |
|---|---|---|---|---|---|
| 9/29 报告峰值 | — | — | — | 31554 | 基准 |
| 基线栈 p1（HEAD d45e0db） | 9021.18 | 8399.32 | 8277.08 | **25698**（25.70 GiB/s） | -18.6% |
| 新栈 p1 今日复测（--tcp-inflight 64） | 8546.64 | 8740.06 | 8500.83 | **25788**（25.79 GiB/s） | -18.3% |
| 新栈 p1 首轮/复跑（前日会话） | — | — | — | 27413 / 25360 | — |
| 新栈 p8（GetBatch，今日复测） | — | — | — | **25633** | -18.8% |

- 基线栈与新栈 **同数据、同条件均 ~25.7 GiB/s，差异 +0.35%** → 9/29 峰值不可复现属数据布局/环境因素，**新读路径无回归**。
- 背景：9/29 前后 128 集群曾出现段满死锁（seg_full=221 / active=0），clean 后段水位恢复（active=1）；9/29 峰值测于不同数据布局。今次 A/B 全部跑在相同 216GB 数据上。

## 3. 延迟 A/B

| 分位 | 基线栈 p1 | 新栈 p1 今日（11/12/13） | 新栈 p8 GetBatch |
|---|---|---|---|
| p50 | 12–14ms | 12.63 / 12.50 / 12.65ms | ×30 恶化 |
| p90 | 135–353ms | 208.66 / 81.99 / 105.51ms | — |
| p99 | 3.6–6.7s | 4.73 / 7.77 / 5.47s | — |

- 新栈 p1 与基线栈延迟同量级；GetBatch（pipeline=8 服务端合并批量）带宽持平但 **p50 恶化约 30 倍 → 无收益，不建议启用**。

## 4. 瓶颈归因（frame stats）

```
新栈 p1 今日复测（bch11s0）：transport-rx dataFrames=40000 dataBytes=167772160000
   take=13335（零拷贝）  copy=26665（CPU 拷贝）   → take/copy = 1/3 : 2/3
```

- 与 9/29 结论一致：**跨节点 TCP 读回 2/3 仍需内存拷贝**，copy 路径 + 每-op 固定开销（p50 ~12.5ms）是读带宽钳制在 ~25–31 GiB/s 的主因，与流水线深度/GetBatch 无关。

## 5. CPU 三维 A/B（运行期采样，USER_HZ=100，jiffies/100 = 核·秒）

| 项（11/12/13） | 基线栈 | 新栈（今日） |
|---|---|---|
| 物理机 busy% | 27.9 / 33.7 / 21.0 | 24.7 / 31.9 / 32.8 |
| usr% | 5.0 / 5.5 / 4.6 | 3.5 / 3.7 / 3.8 |
| sys% | 12.6 / 14.3 / 17.4 | 11.7 / 14.4 / 14.9 |
| 服务端 SER 核 | 3.2 / 4.1 / 4.7 | 4.0 / 3.9 / 4.7 |
| 客户端 CLI 核/节点 | 10–11 | 6.3–7.7 |

- 服务端 SER 基本持平（~4 核）；**新栈客户端等效核数明显低于基线（~7 vs ~11/节点）**，新客户端 syscall 更少 → 新读路径在带宽持平的同时降低了客户端 CPU 消耗，符合降 CPU 设计目标（预留更多 CPU 给外部程序）。

## 6. 结论

1. **无回归**：同数据同条件，基线栈（旧串行读路径）与今日新栈均为 ~25.7 GiB/s，差异 +0.35%；9/29 峰值 31554（+18%）系数据布局/环境因素（含段满死锁清理前后布局差异），非新代码引入。
2. **瓶颈未变**：take/copy 恒为 1:2，2/3 数据仍走拷贝路径 + 每-op 固定开销（p50 ~12.5ms）；pipeline=1 与 GetBatch 带宽一致，**GetBatch 反而 p50 ×30 恶化 → 不启用**。
3. **CPU 维度**：服务端 SER 持平（~4 核/节点），客户端核数 ~7（基线 ~11）→ 新读路径顺带降低客户端 CPU。后续再压带宽需针对 copy 路径（2/3 数据）动手（如 TCP 读回零拷贝/免拷贝接口）。

## 7. 原始数据

- 三节点 `/tmp/perf/rerun.p1.bch{11,12,13}s{0..3}.log`（今日新栈 p1，throughput / latency / frame stats）
- 三节点 `/tmp/perf/base.p1.bch*.log`（基线栈 p1）
- 三节点 `/tmp/perf/rerun.cpu.<N>.txt`（每秒采样 /proc/stat + SER/CLI jiffies，由 `/tmp/perf/.cpustop.<N>` 停止）
- 二进制：`/tmp/taihu.b93df7f`（新栈，sha 873c9081）、`/tmp/taihu.baseline`（基线，sha 1eb17144）

## 8. 附录：测试命令模板

```bash
# 基线栈：node 12 构建 HEAD d45e0db
export PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=auto GOPROXY="https://mirrors.aliyun.com/goproxy,https://goproxy.cn,direct"
export HOME=/root GOPATH=/home/huping/go GOMODCACHE=$GOPATH/pkg/mod GOCACHE=$HOME/.cache/go-build
cd /tmp/taihu-base && go build -ldflags "-X github.com/liucxer/taihu/internal/version.Commit=d45e0db" -o /tmp/taihu.baseline ./cmd/taihu

# 均衡读（12 客户端执行为例，每节点 4；BIN 分别为 b93df7f/baseline）
$BIN --pd 100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379 \
  --client-name bch11s0 bench cluster --mode read --size 4194304 --count 40000 \
  --threads 128 --pipeline 1 --transport auto --conns 4 --write-routing round-robin \
  --keys-prefix bch11s0 --report-interval 2s --latency --preload

# 服务端：新栈带 --tcp-inflight 64；基线栈不带
/tmp/taihu.XXXX server --listen ${B1},${B2} --db /var/taihu-db/nvme${k} --dev /dev/nvme${k}n1p1 \
  --server-name n${N}-nvme${k} --pd 100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379 \
  --batch 32 --batch-workers 8 --write-batch 32 --write-workers 8 [--tcp-inflight 64]
```
