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

测试期间在 128.12 后台并行起采样任务，写入文件，结束后汇总。

> 磁盘带宽**必须真实采集**，禁止用写吞吐推算。用 `iostat`（首选）优先；若不存在再回退 `/proc/diskstats`。CPU 信息**必须完整**：既要有系统整体均值，也要有逐核（`mpstat -P ALL`）的完整分解与 TOP 核。

```bash
# 写测试也要开 -latency，否则无写延迟
# 1) 磁盘：iostat -d 逐秒，列 Device,tps,kB_read/s,kB_wrtn/s,...（kB_wrtn/s 即真实写带宽）
setsid nohup iostat -d $DEV 1 > /tmp/${TAG}.disk 2>&1 &
# 2) CPU 完整信息：mpstat -P ALL 逐秒，输出每个核的 us/sy/wa/id 分解 + 系统 all 均值
setsid nohup mpstat -P ALL 1 > /tmp/${TAG}.cpu  2>&1 &
# 3) 网络带宽：/proc/net/dev 逐秒差，或 ifstat（若有）
# 4) 磁盘回退方案 /proc/diskstats：按 dev sector 读写差 ×512 / 间隔
```

关键坑（均已踩过）：
1. **不要**在本命令里用 `pkill -f taihu-bench.new`——它会匹配执行该命令的 shell 自身命令行并 SIGTERM 自己，导致整条命令 45ms 即被杀死。清进程改用精确 PID，或跳过 pkill。
2. TOP 忙核解析：**不要**用 `sort -t= -k2` 排形如 `core=14 busy=73.1%` 的行——`-t= -k2` 的排序键是 `14 busy=...`（核号做数字前缀），会按**核号**而非 **busy** 排序，输出最大核号而非最忙核。应让行首直接是 busy 数值再 `sort -rn`。
3. **`pkill -x taihu-server.new` 杀不掉进程**：Linux comm 截断为 15 字符（实际是 `taihu-server.ne`），精确匹配失败 → 旧 server 未杀、新 server bind 端口失败但仍 daemonize，脚本继续跑 → **测试连的是旧二进制，整轮数据作废**。正确清理：
   `ps -eo pid,comm | awk '$2 ~ /^taihu-server/{print $1}' | xargs -r kill; sleep 3`
   并在跑测试前用 `ss -ltnp` 确认监听 PID 是刚启动的新进程。
4. **正在运行的二进制无法覆盖上传**：curl PUT 返回 `{"code":1,"error":"[Errno 26] Text file busy"}`。必须先杀进程再上传。
5. **curl -T 上传失败但命令链继续**：HTTP 错误时 curl 退出码仍为 0（未加 `-f`），配合 `>/dev/null` 吞响应 → 误判上传成功、跑旧二进制。上传后必须检查响应 JSON `code==0`，部署后 `md5sum` 校验版本，测试前 `ss -ltnp` 核对 PID。
6. **mpstat -P ALL 列布局**（每 1s 一行）：`$1=时间 $2=PM $3=core|all $4=%usr $5=%nice $6=%sys $7=%iowait $8=%irq $9=%soft $10=%steal $11=%guest $12=%gnice $13=%idle`。awk 逐核统计**必须累加**（`u+=$4`）再除以样本数；用赋值（`u=$4`）只保留末帧 → 输出近似 0 的假值。
7. **128.12 是 aarch64**：交叉编译必须 `GOOS=linux GOARCH=arm64`（amd64 二进制报 `cannot execute binary file`）；本地若 GOROOT 环境变量指向不存在的 toolchain，构建用 `env -u GOROOT GOTOOLCHAIN=auto go build ...`。
8. **临时诊断代码不要直接 commit**（会被拒/污染历史）：交叉编译上传 `/tmp` 覆盖验证后，必须清理工作区诊断代码再提交。诊断期间如遇 `Text file busy`，先杀占用进程再上传。
9. **bufpool 冷分配异常排查**：若 server pprof 中 memclr/makeslice 占比高（读路径曾达 ~18-20%），用 `go tool pprof -peek 'runtime.memclrNoHeapPointers'` / `-peek 'runtime.makeslice'` 看 callers。曾因 alignedBuffer 返回 `backing[start:start+n]`（cap = (n+4096)-start > n），Put 按 cap 归一化后落入高一档桶、Get 永远取不到 → 复用率 0、每 op 冷分配（`get=put=alloc`、reuse_rate=0）。修复：三索引切片 `backing[start:start+n:start+n]` 使 cap==n 归桶一致。可在 bufpool 临时加 get/put/alloc 计数验证 reuse_rate。
10. **跨机压测 128.12 链路仅 ~2-4MB/s**（Mac→128.12 TCP），客户端必须放 128.12 本机走 loopback；跨机客户端会淹没在链路带宽上限，测不出 server 真实能力。

测试结束停止采样并解析：

```bash
kill $IOPID $CPUPID 2>/dev/null
# 磁盘真实写带宽（iostat 列4=kB_wrtn/s，steady 样本求均值/峰值，剔除首行累计均值与末尾收尾样本）
awk '/^'"$DEV"'/{k=$4/1024; if($4>0){sum+=k; n++; p=pk>k?pk:k}} END{printf "DISK_AVG=%.1fMiB DISK_PEAK=%.1fMiB SAMPLES=%d\n", sum/n, p, n}' /tmp/${TAG}.disk
# CPU 整机均值：mpstat 的 all 行（$3=="all"，$NF=%idle）；usr=$4 sys=$6 iowait=$7
awk '$3=="all"{b+=100-$NF; usr+=$4; sy+=$6; iow+=$7; n++}
     END{printf "CPU_AVG_busy=%.1f%% idle=%.1f%% usr=%.1f%% sys=%.1f%% iowait=%.1f%%\n", b/n,100-b/n,usr/n,sy/n,iow/n}' /tmp/${TAG}.cpu
# TOP 忙核：逐核行 $3 为核号、$NF=%idle；每核多帧求均值，行首即 busy 值再 sort -rn 取前 N
awk '$3 ~ /^[0-9]+$/{nb[$3]+=100-$NF; nu[$3]+=$4; ni[$3]+=$7; c[$3]++}
     END{for(k in nb) printf "%.1f core=%s usr=%.1f iowait=%.1f\n", nb[k]/c[k], k, nu[k]/c[k], ni[k]/c[k]}' /tmp/${TAG}.cpu | sort -rn | head -5
```

## 步骤 4：跑写测、读测

```bash
# 写
/tmp/taihu-bench.new -mode write -dev $DEV -db /tmp/tdb -keys-prefix p4m-w1 \
  -size 4194304 -count 20000 -threads 16 -report-interval 5s
# 读（同批 key，LoadCache 预热元数据剔除 pebble 干扰）
/tmp/taihu-bench.new -mode read -dev $DEV -db /tmp/tdb -keys-prefix p4m-w1 \
  -size 4194304 -count 20000 -threads 16 -report-interval 5s
```

bench 输出含：吞吐 ops/s 与 MiB/s，读含延迟 p50/p90/p99；**写测试务必开 `-latency`** 以得到写延迟。编排：先 `setsid nohup` 起 bench 与采样器，轮询 bench PID 结束（**禁止 `pkill -f`**），结束停采样并解析。

## 步骤 5：pprof（测试程序）

bench 接入 `runtime/pprof` 并支持 `-cpuprofile out.cpu -memprofile out.mem` 时：

```bash
cmd="/tmp/taihu-bench.new -mode read ... -cpuprofile /tmp/bench.cpu -memprofile /tmp/bench.mem; go tool pprof --text /tmp/taihu-bench.new /tmp/bench.cpu"
```
- 测试后拉取 profile 到本地：`curl -G ... "cmd=cat /tmp/bench.cpu"`（base64 落地），`go tool pprof -http=` 分析。
- 关注的用户态热点：`Device.append`/`read`、`AllocateSegment` 分配、pebble Get/PutMeta。

## 步骤 6：生成测试报告

模板参考工程内 `doc/461d092 写性能测试报告.md` 的结构与字段，以 Markdown 保存到 `doc/<commit> <读写>性能测试报告.md`。必含章节：

```markdown
# <commit> 写性能测试报告
> 对应提交：<commit>（<提交说明>）
## 1. 测试目标
## 2. 测试环境（服务器 / 编译产物 / 数据设备 / 元数据 / 对象大小 / 部署方式）
## 3. 测试方法（taihu-bench 参数：-mode -size -threads -count -keys-prefix -db -dev -latency）
## 4. 测试结果
### 4.1 带宽与最低线程数（写 xx MiB/s 对应线程；达到 .5GB/s 阈值的最低线程数）
### 4.2 延迟（线程N：p50/p90/p99）
## 5. 资源占用
### 5.1 CPU（完整：系统整体均值 + mpstat -P ALL 逐核 + 忙核排序；us/sy/wa/id 分解；idle%；结论是否瓶颈）
### 5.2 磁盘（iostat 实测：稳态写带宽/峰值/均值/读带宽/tps；结论是否瓶颈）
### 5.3 网络（按网卡 avg/峰值）
## 6. 瓶颈分析与 pprof 热点（前5）
## 7. 结论与建议
```

硬性要求：
- **磁盘带宽**必须是 iostat 实测值（禁止用写吞吐推算）；给出稳态区间、峰值、均值、tps。
- **CPU 信息必须完整**：整机 idle%、all 均值、逐核分解、TOP 忙核（含 iowait 占比），并给出"是否 CPU 瓶颈"判断。
- 写测试需含 `-latency` 的写延迟 p50/p90/p99。

## 注意事项

- 写测试在裸设备上做破坏性写入，覆盖区域先确认边界，避免破坏生产/存量数据。
- 数据量按设备剩余空间控制；`count` 不足会导致读提前耗尽 key。
- 采网络/磁盘时明确设备名（`-s` 指定 dev）避免误判。
- token/IP 属内网测试环境，勿外传。