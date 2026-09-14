---
name: "nefs-fio-perf"
description: "在 128 集群(100.71.128.11/12/13)对已挂载的 NEFS 卷做 fio 顺序读写性能测试并用 sar/pidstat/iostat/pprof 采集全套指标。适用于用户要求对 NEFS FUSE 挂载卷做读写带宽/延迟/CPU/网络/磁盘/pprof 性能测试时使用。"
---

# NEFS FUSE 挂载卷 fio 性能测试（三节点并发 + 全套采集）

在 128 集群每节点一个已挂载的 NEFS 卷上，做 fio 4M/numjobs16/iodepth16 顺序读写，并**同时**采集物理机 / 客户端进程 / 服务端进程 CPU、网络带宽、NVMe 磁盘带宽、以及客户端 + 服务端 pprof（heap/goroutine/CpuProfile）。

> 适用：100.71.128.11/12/13，挂载点 `/mnt/taihu`，数据面走 taihu（本机 3 个 taihu-server 实例，pprof = 数据面端口+1）。通过 nefs-proxy（9527, X-Token: `95279527`）下发命令。

---

## 0. 关键认知（避免踩坑）

- **NEFS 是 FUSE 挂载，不支持 `O_DIRECT`**。`libaio --direct=1` 会空转（报 `destination does not support O_DIRECT`），io_uring 在 kernel 4.19 不可用。
  → **必须用 `--ioengine=psync --direct=0`（buffered）**。psync 下 `iodepth` 不生效，实际并发数 = `numjobs`。
- `--size` 用整数（如每 job 3.75G 写 `--size=3932160K`）；`--size=3.75G` 会被 fio 3.7 误解析（报 `size too small ... 3`）。
- fio 多 job 默认可能只建 1 个文件：用 `--filename_format='fiotest.$jobnum'` 让每 job 独立文件。
- `nefs.client` pprof 在 `127.0.0.1:6061/nefs/v1/debug/pprof/` **带前缀**，根路径 `/debug/pprof` 是 404。
- tailhu-server pprof 路径 `/debug/pprof`，端口 = 数据面端口 + 1（见下）。
- **PowerShell 会把 cmd 内嵌的引号/`\$`/`$()` 解析掉**，还会把 `curl` 误判为 Invoke-WebRequest。→ 一律把要跑的命令写成 `.sh` 脚本上传到节点再执行，避免内嵌转义。

## 1. 前置检查

### 1.1 目标卷与挂载确认
```powershell
$r = curl.exe -s --max-time 30 -G -H "X-Token: 95279527" "http://<node>:9527/exec" --data-urlencode "cmd=df -h /mnt/taihu | grep nefs; mount | grep nefs"
($r | ConvertFrom-Json).stdout
```

### 1.2 fio 可用性（fio 依赖 ceph 库）
- 12/13 有 `fio-3.7`（ceph 节点自带 `librbd`）；**128.11 无 `librbd.so.1`**，需要分发。
- 分发 fio + 依赖库（从 12 打 tar 摊到缺库节点）：
  ```bash
  # 在 12 上
  cd / && tar czf /tmp/fiotest.tar.gz usr/bin/fio \
    usr/lib64/librbd.so.1 usr/lib64/librados.so.2 usr/lib64/ceph/libceph-common.so.0 \
    usr/lib64/libnuma.so.1 usr/lib64/librdmacm.so.1 usr/lib64/libibverbs.so.1 \
    usr/lib64/libssl3.so usr/lib64/libsmime3.so usr/lib64/libnss3.so usr/lib64/libnssutil3.so \
    usr/lib64/libplds4.so usr/lib64/libplc4.so usr/lib64/libnspr4.so \
    usr/lib64/libnl-3.so.200 usr/lib64/libnl-route-3.so.200
  RB=$(readlink -f /usr/lib64/librbd.so.1); RA=$(readlink -f /usr/lib64/librados.so.2)
  cd / && tar czf /tmp/rblib.tar.gz "$RB" "$RA"   # 真文件（非符号链接）
  ```
  目标节点：`tar xzf` 两个包，再
  ```bash
  ln -sf /usr/lib64/ceph/libceph-common.so.0 /usr/lib64/libceph-common.so.0
  ldconfig && fio --version
  ```
  经 proxy 下载/上传传输这三个 tar.gz。

### 1.3 pprof 端口探测
```bash
# 客户端（带前缀）
curl -s -m3 http://127.0.0.1:6061/nefs/v1/debug/pprof/heap -o /dev/null -w '%{http_code}'
# 服务端：列出所有 taihu-server 端口并试 /debug/pprof
ss -ltnp | grep taihu-server | awk '{print $4}' | sed 's/.*://' | sort -u | while read p; do
  echo "$p -> $(curl -s -m2 -o /dev/null -w '%{http_code}' http://127.0.0.1:$p/debug/pprof/heap)"
done
```
实测映射：11 → {50001,50003,50005}，12 → {50002,50003,50005}，13 → {50001,50003,50005}。节点变更时先探测。

## 2. 测试脚本（见配套文件）

- **`runphase.sh`**：单节点跑一个 phase(write/read)，fio + 后台采集 + pprof（本文末尾给出）。
- **`reparse.sh` / `analyze.sh`**：汇总 /tmp/perf 下采集日志为可读摘要。

使用方法：
```bash
# 上传到三节点
bash /tmp/runphase.sh <node> write    # node ∈ 11|12|13；read 同理
# 后台并发：
setsid bash /tmp/runphase.sh $tag write >/tmp/run_write_$tag.log 2>&1 </dev/null &
# 完成后：
bash /tmp/reparse.sh write   # 输出该节点 write phase 汇总
```

方式：
- 通过 nefs-proxy `/upload` 上传 .sh，`/exec` 执行。
- 三节点各开一个后台任务，轮询 `/tmp/perf_res/{write,read}.done` 确认完成。

## 3. 采集与解析要点

### 3.1 采集命令（后台）
```bash
mpstat 1 > ${PH}.cpu.mpstat &            # 物理机每核/汇总 CPU
pidstat -u -p <clientpid> 1 > ${PH}.client.pidstat &   # 客户端 nefs 进程 CPU
pidstat -u -p <srvpid1,srvpid2,srvpid3> 1 > ${PH}.server.pidstat & # 3 个 taihu-server
sar -n DEV 1 > ${PH}.net.sar &           # 网络带宽
iostat -x 1 > ${PH}.disk.iostat &        # 磁盘带宽（nvme1/2/3n1 数据盘 + nvme0n1 系统盘）
# 客户端 CPU 火焰图（fio 结束后再 kill，否则不落盘！）
curl -s -m900 "http://127.0.0.1:6061/nefs/v1/debug/pprof/profile?seconds=700" -o ${PH}.client.pprof.pb.gz &
# 服务端同理（每个 pprof 端口一个）
curl -s -m900 "http://127.0.0.1:50001/debug/pprof/profile?seconds=700" -o ${PH}.server50001.pprof.pb.gz &
```
**坑**：`profile?seconds=N` 的 curl 必须在 fio 跑完后 `kill`，否则它持续 700s 且被 kill 时**不落盘**（只有 heap/goroutine 能稳定抓到）。若想要 CPU 火焰图，等 profile curl 自然结束或延后 kill。

### 3.2 列偏移解析（PM 后缀陷阱）
这些采集输出带 `02:14:16 PM` 时间戳（**AM/PM**），导致列比无 PM 的格式右移 1 列。解析时用下表：

| 工具 | 说明 | 关键列 |
|------|------|--------|
| `pidstat` | `$1=time $2=PM $3=UID $4=PID ...` | **%CPU = $9**，PID = $4 |
| `sar -n DEV` | `$2=PM $3=IFACE ...` | **rxkB/s = $6，txkB/s = $7** |
| `mpstat` | `$3=="all"`, `%idle = $NF` | busy = 100 - 末列；只取 `$3=="all"` 行 |
| `iostat -x` | 无 AM/PM | rkB/s = $3，wkB/s = $9，%util = $21 |
| 客户端 pidstat CPU | 多核求和，可 >100% | 用 sum/n 与 max |
| taihu 磁盘 | nvme1n1/2n1/3n1 为数据盘 | nvme0n1 是系统盘，忽略 |

`reparse.sh` 已按此写死。

### 3.3 fio 结果解读
从 `fio.txt` 取：
```
write: IOPS=704, BW=2816MiB/s (2953MB/s)(60.0GiB/21817msec)
clat percentiles (msec): 50.00th=[..], 99.00th=[..], 99.90th=[..]
```

### 3.4 产物下载
经 proxy `/download?path=/tmp/perf/<file>` 拉回本地存档：
`client.heap.pb.gz / client.goroutine.txt / server5xxxx.heap.pb.gz / fio.txt / cpu.mpstat / net.sar / disk.iostat`（甚至下载为空文件也存在，体积 0 说明没采到）。

## 4. 收尾
- `rm -rf /mnt/taihu/perf` 清理测试文件（每节点 60GiB，三节点约 180GiB）。
- 卷保持挂载；如需测试期间 pprof 火焰图补抓，单独跑一次长 profile 即可。

## 5. 实测基准（2026-09-12，60GiB，三节点同时）
| 指标 | 写 | 读 |
|------|-----|-----|
| 12/13 吞吐 | ~2.7-2.8 GiB/s | ~4.2-4.4 GiB/s |
| 11 吞吐 | 2.888 GiB/s | 11.2 GiB/s |
| 客户端 nefs CPU | ~22-25 核(峰值) | 7-18 核 |
| taihu-server ×3 | ~67-88% 总(avg) | ~63-143% 总 |
| 物理机整体 | ~31% | ~23-32% |
| 数据盘写/读 | 3×~800-845 MB/s | 3×~1.1-2.3 GB/s |
| 网络 | 很小(总 57-317MB) | 很小(本地读) |

线索：写/读基本**本地化**（网络 tx << 数据量），客户端进程（非整机）是主要 CPU 消耗方。

## 6. 配套脚本

### runphase.sh
```bash
#!/bin/bash
NODE=$1; PH=$2
OUT=/tmp/perf; RES=/tmp/perf_res; mkdir -p "$OUT" "$RES"
DIR=/mnt/taihu/perf; mkdir -p "$DIR"
case $NODE in
  11) SPORTS="50001 50003 50005";;
  12) SPORTS="50002 50003 50005";;
  13) SPORTS="50001 50003 50005";;
esac
CLIENT=$(pgrep -f 'nefs.client.taihu mount'|head -1)
SPIDS=$(pgrep -f 'taihu-server'|paste -sd, -)
RVW=write; [ "$PH" = read ] && RVW=read
( mpstat 1 > $OUT/$PH.cpu.mpstat )& C1=$!
( sar -n DEV 1 > $OUT/$PH.net.sar )& C2=$!
( iostat -x 1 > $OUT/$PH.disk.iostat )& C3=$!
( pidstat -u -p $CLIENT 1 > $OUT/$PH.client.pidstat )& C4=$!
( pidstat -u -p $SPIDS 1 > $OUT/$PH.server.pidstat )& C5=$!
( curl -s -m900 "http://127.0.0.1:6061/nefs/v1/debug/pprof/profile?seconds=700" -o $OUT/$PH.client.pprof.pb.gz )& PCLI=$!
for sp in $SPORTS; do ( curl -s -m900 "http://127.0.0.1:$sp/debug/pprof/profile?seconds=700" -o $OUT/$PH.server$sp.pprof.pb.gz )& PSRV+=" $!"; done
sleep 2
fio --name=$PH --directory="$DIR" --ioengine=psync --direct=0 --bs=4M --rw=$RVW \
    --numjobs=16 --iodepth=16 --size=3932160K --group_reporting=1 \
    --filename_format='fiotest.$jobnum' >> $OUT/$PH.fio.txt 2>&1
kill $C1 $C2 $C3 $C4 $C5 $PCLI $PSRV 2>/dev/null; sleep 2
curl -s -m5 "http://127.0.0.1:6061/nefs/v1/debug/pprof/heap" -o $OUT/$PH.client.heap.pb.gz
curl -s -m5 "http://127.0.0.1:6061/nefs/v1/debug/pprof/goroutine?debug=1" -o $OUT/$PH.client.goroutine.txt
for sp in $SPORTS; do curl -s -m5 "http://127.0.0.1:$sp/debug/pprof/heap" -o $OUT/$PH.server$sp.heap.pb.gz; done
echo "$PH done" > $RES/$PH.done
```

### reparse.sh（解析，列已按 §3.2 修正）
上传到节点后 `bash /tmp/reparse.sh write|read`，依序打印 fio / 客户端 CPU / 服务端 CPU / 物理机 CPU / 网络 / NVMe 磁盘摘要。