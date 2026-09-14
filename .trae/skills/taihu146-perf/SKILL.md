---
name: "taihu146-perf"
description: "在 10.151.26.146-150,152 六节点部署 6节点×3盘(18实例) taihu 集群(每节点 2实例监听 bond1.2175 + 1实例监听 bond2.2372)，做端到端写/跨节点读性能测试，采集物机CPU/进程CPU/网络带宽/pprof/客户端带宽延迟。TRIGGER: 用户要求在 146 集群做 taihu 读写性能测试/部署双客户端测试/跨节点读时使用。"
---

# taihu 146 集群性能测试 skill

在 **10.151.26.146-150,152（六节点 × 3 盘，18 实例）** 上部署/测试 taihu 集群，做端到端读写压测与指标采集（物机 CPU、进程 CPU、网络带宽、客户端/服务端 pprof、客户端带宽/延迟），并支持**双客户端**与**跨节点读（避开同机读）**场景。

## 节点与网络拓扑

| 节点 | hostname | bond1.2175 IP | bond2.2372 IP | TAIHU 实例 |
|------|----------|---------------|---------------|------------|
| 10.151.26.146 | ...-ebs-10 | 10.153.28.202 | 10.155.17.202 | TAIHU-0..2 |
| 10.151.26.147 | ...-ebs-11 | 10.153.28.203 | 10.155.17.203 | TAIHU-3..5 |
| 10.151.26.148 | ...-ebs-12 | 10.153.28.204 | 10.155.17.204 | TAIHU-6..8 |
| 10.151.26.149 | ...-ebs-13 | 10.153.28.205 | 10.155.17.205 | TAIHU-9..11 |
| 10.151.26.150 | ...-ebs-14 | 10.153.28.206 | 10.155.17.206 | TAIHU-12..14 |
| 10.151.26.152 | ...-ebs-16 | 10.153.28.208 | 10.155.17.208 | TAIHU-15..17 |

- **bond1.2175 / bond2.2372**：802.3ad(enp1s0f0/f1 与 enp133s0f0/f1) 2×25Gbps、MTU 9000 —— 跨节点数据面主链路
- **bond0.1326**（10.151.26.x/24）：1Gbps，管理面/跨网段控制面，**非数据面**
- **每节点 3 实例**：TAIHU-N/N+1 `-listen <bond1 IP>`、TAIHU-N+2 `-listen <bond2 IP>`（SDK 按 hostname 判定：同机 shm、跨节点 TCP）
- 本机 shm socket 固定 `/dev/<server-name>`；数据设备 nvme3n1/4n1/5n1；pebble 在 /mnt/nvme2/taihu2-*/db；TiKV PD 100.71.128.11/12/13:2379

## 前置：二进制与脚本准备

1. **本地交叉编译 arm64**（节点无公网、依赖缓存不全，必须在本地编译）：

```bash
cd d:\workspace\taihu
$env:GOOS="linux"; $env:GOARCH="arm64"; $env:CGO_ENABLED="0"
$SHA="<git short sha>"; $TS="<YYYYMMDDHHMM>"
go build -trimpath -ldflags "-X github.com/liucxer/taihu/internal/version.Commit=$SHA -X github.com/liucxer/taihu/internal/version.BuildTime=$TS" -o dist/build/taihu-server.50ebbe4 ./cmd/taihu-server
go build -trimpath -ldflags "<同上>" -o dist/build/taihu-rpc-bench.50ebbe4 ./cmd/taihu-rpc-bench
```

2. **上传到 6 节点并 chmod**（用 nefs-proxy，python 命令用 `.trae/skills/nefs-proxy/proxy_client.py`；Windows 下用 `py -0p` 找到真实 python 路径后调用）：

```bash
PY="C:\Users\USER484887\AppData\Roaming\uv\python\cpython-3.12.14-windows-x86_64-none\python.exe"
foreach($n in 146,147,148,149,150,152){
  & $PY .trae/skills/nefs-proxy/proxy_client.py --node $n upload --local dist\build\taihu-server.50ebbe4 --remote /tmp/taihu-server.50ebbe4
  & $PY .trae/skills/nefs-proxy/proxy_client.py --node $n upload --local dist\build\taihu-rpc-bench.50ebbe4 --remote /tmp/taihu-rpc-bench.50ebbe4
}
```

3. 上传本 skill 目录内的测试脚本到各节点 `/tmp/`：`redeploy_bond146.sh`、`wtest146.sh`、`wtest2x146.sh`、`rtest_all146.sh`、`rtest2x146.sh`、`report146.sh`、`report2x146.sh`、`xbond146.sh`、`probe_146.sh`。

## 步骤 1：部署/重启实例（18 实例）

每节点上传 `redeploy_bond146.sh` 后执行（参数：bond1 IP、bond2 IP、起始 TAIHU 名）：

```bash
# 146
bash /tmp/redeploy_bond146.sh 10.153.28.202 10.155.17.202 TAIHU-0
# 147
bash /tmp/redeploy_bond146.sh 10.153.28.203 10.155.17.203 TAIHU-3
# 148
bash /tmp/redeploy_bond146.sh 10.153.28.204 10.155.17.204 TAIHU-6
# 149
bash /tmp/redeploy_bond146.sh 10.153.28.205 10.155.17.205 TAIHU-9
# 150
bash /tmp/redeploy_bond146.sh 10.153.28.206 10.155.17.206 TAIHU-12
# 152
bash /tmp/redeploy_bond146.sh 10.153.28.208 10.155.17.208 TAIHU-15
```

脚本自动：杀旧 taihu-server → 按 `TAIHU-{N+N0}, {N+N0+1}, {N+N0+2}` 启 3 实例（nvme3/4/5）→ 输出监听确认。验证：`ps -eo pid,comm,args | grep taihu-server`、`ss -ltnp | grep taihu-server`。

## 步骤 2：验证注册与路由

写 3 个 1MiB probe key 确认可写：

```bash
/tmp/taihu-rpc-bench.50ebbe4 -client-name PROBE -tikv-pd "100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379" -mode write -size 1048576 -count 3 -threads 3 -keys-prefix probe
```

**跨节点 bond 读验证**（可选，146 读 147 数据抓 bond 差分，确认走 25G）：

```bash
# 147 写 2000 key（4MiB）
/tmp/taihu-rpc-bench.50ebbe4 -client-name WR -tikv-pd "$PD" -mode write -size 4194304 -count 2000 -threads 16 -keys-prefix b1m-c147
# 146 读（xbond146.sh 内含网络差分）
bash /tmp/xbond146.sh
```

预期：bandwidth **2.5+ GiB/s**，bond1.2175 + bond2.2372 差分 = 数据量（2:1 比例），bond0.1326 仅 10-20MB。

## 步骤 3：写性能测试

**单客户端**（wtest146.sh：HOSTNAME、PREFIX、TAG，[COUNT 40000] [THREADS 32]）：

```bash
bash /tmp/wtest146.sh <hostname> p4m-c146 w146 40000 32
```

**双客户端**（wtest2x146.sh：HOSTNAME、PREFIX_A、PREFIX_B、TAG，[COUNT 20000] [THREADS 32]）——每节点 2 bench 并行、各写独立前缀：

```bash
bash /tmp/wtest2x146.sh <hostname> w2-c146A w2-c146B w146 20000 32
```

脚本自动采集：物理机 mpstat、进程 pidstat（client+3 server）、bond/lo 网络差分、2 客户端 pprof、3 服务端 pprof（pprof 端口从 ss 提取）。产物 `/tmp/${TAG}_wx.*`。

## 步骤 4：跨节点读测试（避开同机读）

**核心：读前缀必须来自其它节点**（hostname 不同 → SDK 走 TCP；数据面落 bond1/bond2 25G）。

**单客户端**（rtest_all146.sh：HOSTNAME、远端 PREFIX、TAG，[COUNT] [THREADS]）——环形错位：

```bash
146: bash /tmp/rtest_all146.sh <ebs-10> p4m-c147 r146 10000 32   # 读147数据
147: bash /tmp/rtest_all146.sh <ebs-11> p4m-c148 r147 10000 32
148: bash /tmp/rtest_all146.sh <ebs-12> p4m-c149 r148 10000 32
149: bash /tmp/rtest_all146.sh <ebs-13> p4m-c150 r149 10000 32
150: bash /tmp/rtest_all146.sh <ebs-14> p4m-c152 r150 10000 32
152: bash /tmp/rtest_all146.sh <ebs-16> p4m-c146 r152 10000 32
```

**双客户端**（rtest2x146.sh：HOSTNAME、远端前缀 A、远端前缀 B、TAG，[COUNT] [THREADS]）——每节点 2 reader 分别读两个不同远端：

```bash
146: bash /tmp/rtest2x146.sh <ebs-10> w2-c147A w2-c148A r146 10000 32
147: bash /tmp/rtest2x146.sh <ebs-11> w2-c148B w2-c149A r147 10000 32
148: bash /tmp/rtest2x146.sh <ebs-12> w2-c149B w2-c150A r148 10000 32
149: bash /tmp/rtest2x146.sh <ebs-13> w2-c150B w2-c152A r149 10000 32
150: bash /tmp/rtest2x146.sh <ebs-14> w2-c152B w2-c146A r150 10000 32
152: bash /tmp/rtest2x146.sh <ebs-16> w2-c146B w2-c147B r152 10000 32
```

读模式带 `-preload`（预热 RouteCache）；每客户端 10000×4MiB=40GB。

## 步骤 5：分析汇总

```bash
# 单客户端产物
bash /tmp/report146.sh <TAG> w   # 或 r
# 双客户端产物
bash /tmp/report2x146.sh <TAG> w # 或 r
```

输出：bench 吞吐/延迟、frame stats（`take=` 零拷贝命中）、物理机 CPU 均值、进程 CPU 均值、网络差分（bond1.2175/bond2.2372/bond0.1326）。

pprof 下载到本地用 `go tool pprof -top`（Windows 下若 GOTOOLCHAIN=auto 触发下载，设 `$env:GOTOOLCHAIN="local"`）。

## 步骤 6：生成报告

参照 `doc/202609112230_50ebbe4 六节点集群双客户端bond双网络跨节点读写性能测试报告.md` 模板，含：测试目标/环境/写结果/读结果（错位配对表）/网络差分/物机CPU/进程CPU/pprof热点/结论。保存到 `doc/<时间>_<commit> ...md`。

## 参考基线（2026-09-11，commit 50ebbe4）

| 测试 | 结果 |
|------|------|
| 写（单客户端 40000×4MiB，32T） | 聚合 ~86.9 GiB/s，每节点 14.2 GiB/s |
| 写（双客户端 2×20000，32T） | 聚合 ~90.6 GiB/s（+5%） |
| 读（单客户端跨节点，bond 25G） | 每节点 ~2.5 GiB/s，聚合 ~15.0 GiB/s |
| 读（双客户端跨节点，bond 25G） | 每节点 ~4.2-4.8 GiB/s，聚合 ~26.3 GiB/s |
| 读（上限加压，conns≥4，双客户端） | 每节点 ~8.0-8.6 GiB/s，聚合 ~47.6 GiB/s |
| 读（旧 1Gbps bond0.1326 单客户端对比） | ~110 MiB/s（瓶颈网络） |

## 跨节点读带宽上限结论（探明，2026-09-11）

- **连接数 -conns 是第一瓶颈**：conns 1→4，聚合带宽 +60%（5.0→8.0 GiB/s）；conns=4 已够，8 无增益。
- 线程 32→64 无提升 → 非 CPU 瓶颈（物理机忙碌 22-29%）。
- 单扇入节点上限 **8.0-8.6 GiB/s**：受 bond1.2175（2 实例/2/3 数据）**~5.6 GiB/s 线速饱和**限制；bond2 只吃 1/3（~2.8 GiB/s，余量足）。
- 分流固定 **2:1**（2 实例 bond1 + 1 实例 bond2）→ 总带宽 ≈ bond1 上限×1.5。6 节点全环 conns=4 双客户端聚合 **~47.6 GiB/s**。
- 提升建议：让数据均匀铺到两 bond（1.5:1.5 分流理论 ~9.4 GiB/s/节点）、25G 成员扩容（4×25 或 100G）、或 RDMA。
- 探针脚本：`probe_max146.sh <NCLI> <CONNS> <THREADS> <COUNT> <TAG>`（单数据源参数扫描 + bond 精确差分）；全环加压用 `rtest2x146.sh ... [COUNT] [THREADS] [CONNS]`（已支持 -conns）。

## 注意事项（踩坑记录）

1. **PowerShell 下复杂 shell 命令常被 `$()`/`$(...)` 转义破坏**——尽量用「上传 .sh 脚本后在节点执行」方式，避免一条 exec 内嵌复杂管道/数学。
2. Windows 本机 `python`/`py` 指向 WindowsApps 或 astral launcher，可能无法直接跑脚本；用 `py -0p` 找真实 python（uv 安装路径），存变量 `$PY` 复用。
3. **网络差分要 `grep '^bond0\.1326\|^bond1\.2175\|^bond2\.2372'`**，不要用 `awk '/(bond|eth|enp|lo):/'`——该模式会漏无前导空格的 bond0.1326 这类接口名。
4. 上传后必须 `chmod +x` 二进制（proxy 上传不保留可执行位）。
5. 跨节点读时**时钟漂移**会导致实例被标记 stale（Aliveness 5s 心跳）→ 报寻址错误；先 `date` 核对各节点，必要时 `ntpdate`/`chronyc` 同步。
6. 每节点 3 盘位固定 nvme3n1/4n1/5n1（不要动 nvme0/1/2 系统盘）。
7. 服务端 pprof 端口自动分配（50001/3/5 之类，脚本已从 ss 自动提取），跨节点抓取用 `curl http://<IP>:<port>/debug/pprof/profile?seconds=N`。
8. 写测试数据面走 shm，bond 差分应≈0（仅控制面 MB 级）；若 bond 有数十 GB 网络流量说明配置错误。
9. 测试前先确认集群无其它测试残留进程：`ps -eo comm | grep -cE 'taihu-server|taihu-rpc-bench'`。
10. token/IP 属内网测试环境，勿外传。