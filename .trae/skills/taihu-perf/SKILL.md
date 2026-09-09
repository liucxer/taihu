---
name: "性能测试skill"
description: "在 128.12 上对已编译部署的 taihu-bench 做读写性能测试：测读写带宽、延迟，并采集物理机 CPU/网络/磁盘带宽与测试程序 pprof。TRIGGER: 用户要求性能测试/测读写带宽/采集 CPU或pprof 时使用，前提是先执行 编译部署skill。"
---

# 性能测试 skill

对 **已由 `编译部署skill` 部署在 128.12 的 `taihu-bench`** 执行读写性能压测，并采集主机（CPU/网络/磁盘)与程序（pprof）指标。

> 前置依赖：必须先跑过 `编译部署skill`，得到产物 `/tmp/taihu-bench.new`（128.12 上）。

## 环境

```bash
NODE=100.71.128.12
TOKEN=95279527
DEV=/dev/nvme1n1
BENCH=/tmp/taihu-bench.new
```
执行/上传用 curl（X-Token）：`curl -s -G -H "X-Token:$TOKEN" "http://$NODE:9527/exec" --data-urlencode "cmd=..." --data-urlencode "timeout=..."`。

## 步骤 1：确认 bench 就绪

```bash
cmd=/tmp/taihu-bench.new -h 2>&1
```
读 key 参数：`-mode write|read`、`-size`、`-count`、`-threads`、`-db`、`-dev`、`-keys-prefix`、`-report-interval`。若无 `-pps-/cpuprofile` 参数，pprof 需先给 bench 加 `-cpuprofile/-memprofile`（runtime/pprof）再走编译部署。

## 步骤 2：选参数并规划数据量

- `-size`：对象字节数（读不重复 key，`count` 需 ≥ 读满时长所需 key 数，避免读提前耗尽）。
- 写不留坑：先 `-mode write` 写入 `count` 个 key，再用 `-mode read` 读同一批（key 不重复）。
- `-keys-prefix` 用唯一前缀隔离批次（如 `p4m-w1`），`-db` 复用同一 pebble 目录以便游标续写。
- `-threads` 常用 1/4/16/32 对比；写优先验证并发扩展性。

## 步骤 3：采集背景（并行后台，贯穿全程）

测试期间在 128.12 后台起采样任务，结束后汇总。统计尽量用 `/proc` 采样（不依赖 iftop/iostat 是否存在），采样间隔约 1s：

```bash
# CPU%（/proc/stat 采样）：两次读差 / 间隔
# 网络带宽（/proc/net/dev）：按 dev 累加 receive+transmit 字节差 / 间隔
# 磁盘吞吐（/proc/diskstats）：按 dev sector 读写差 ×512 / 间隔
```
采集器写文件增量为内存即可（测试结束算均值/峰值）。若 `mpstat`/`iostat`/`ifstat` 存在可直接用，更直观。

## 步骤 4：跑写测、读测

```bash
# 写
/tmp/taihu-bench.new -mode write -dev $DEV -db /tmp/tdb -keys-prefix p4m-w1 \
  -size 4194304 -count 20000 -threads 16 -report-interval 5s
# 读（同批 key，LoadCache 预热元数据剔除 pebble 干扰）
/tmp/taihu-bench.new -mode read -dev $DEV -db /tmp/tdb -keys-prefix p4m-w1 \
  -size 4194304 -count 20000 -threads 16 -report-interval 5s
```

bench 输出含：吞吐 ops/s 与 MiB/s，读含延迟 p50/p90/p99；写延迟若有 `-latency` 参数一并开。长测试用 `setsid nohup ... &` 后台 + 轮询（见 编译部署skill）。

## 步骤 5：pprof（测试程序）

bench 接入 `runtime/pprof` 并支持 `-cpuprofile out.cpu -memprofile out.mem` 时：

```bash
cmd="/tmp/taihu-bench.new -mode read ... -cpuprofile /tmp/bench.cpu -memprofile /tmp/bench.mem; go tool pprof --text /tmp/taihu-bench.new /tmp/bench.cpu"
```
- 测试后拉取 profile 到本地：`curl -G ... "cmd=cat /tmp/bench.cpu"`（base64 落地），`go tool pprof -http=` 分析。
- 关注的用户态热点：`Device.append`/`read`、`AllocateSegment` 分配、pebble Get/PutMeta。

## 步骤 6：汇总输出

表格给出：读写 MiB/s 与 ops/s、读延迟 p50/90/99、同时段 CPU%（avg/峰值）、网络带宽（avg/峰值，按网卡）、磁盘吞吐（读/写、通过率 ％util 若有）、pprof 热点前 5。

## 注意事项

- 写测试在裸设备上做破坏性写入，覆盖区域先确认边界，避免破坏生产/存量数据。
- 数据量按设备剩余空间控制；`count` 不足会导致读提前耗尽 key。
- 采网络/磁盘时明确设备名（`-s` 指定 dev）避免误判。
- token/IP 属内网测试环境，勿外传。