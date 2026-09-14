#!/bin/bash
# taihu 146 集群 单节点写性能测试：采集 物机CPU + 进程CPU + 网络差分 + 客户端/服务端 pprof，前台跑 bench。
# 用法: bash wtest_146.sh <HOSTNAME> <KEY_PREFIX> <TAG> [COUNT] [THREADS]
set -e
HOST=$1
PREFIX=$2
TAG=$3
COUNT=${4:-40000}
THREADS=${5:-32}
PY=/tmp
BENCH=/tmp/taihu-rpc-bench.50ebbe4
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

rm -f /tmp/${TAG}_w.*
echo "== start samplers (TAG=$TAG) =="
setsid nohup mpstat -P ALL 1 > /tmp/${TAG}_w.mpstat 2>&1 &
MP=$!
# 进程 CPU：client + 3 server
setsid nohup sh -c 'for t in $(seq 1 240); do ps -eo pid,comm,%cpu | grep -E "taihu-rpc-bench|taihu-server"; sleep 1; done' > /tmp/${TAG}_w.pidstat 2>/dev/null &
PS=$!
grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372|lo|bond0|bond1|bond2)\b' /proc/net/dev > /tmp/${TAG}_w.net.base

# 本节点 3 个 server pprof（pprof 端口自动分配，从 ss 提取 *:port）
PP=$(ss -ltnp 2>/dev/null | grep taihu-server | grep -oE '\*:[0-9]+' | grep -oE '[0-9]+$' | sort -u | head -3)
i=0
for p in $PP; do
  i=$((i+1))
  curl -s --max-time 40 "http://127.0.0.1:${p}/debug/pprof/profile?seconds=30" -o /tmp/${TAG}_w.s${i}.cpu &
  eval PPID_${i}=$!
done

echo "== run bench write (HOST=$HOST prefix=$PREFIX) =="
START=$(date +%s)
$BENCH -client-name "$HOST" -tikv-pd "$PD" \
  -mode write -size 4194304 -count "$COUNT" -threads "$THREADS" \
  -keys-prefix "$PREFIX" -report-interval 5s -latency \
  -cpuprofile /tmp/${TAG}_w.client.cpu > /tmp/${TAG}_w.bench.log 2>&1
RC=$?
END=$(date +%s)

for i in 1 2 3; do eval wait \$PPID_${i} 2>/dev/null || true; done
sleep 1
grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372|lo|bond0|bond1|bond2)\b' /proc/net/dev > /tmp/${TAG}_w.net.end
kill $MP $PS 2>/dev/null || true

echo "== bench log =="
grep -E 'throughput|latency|====' /tmp/${TAG}_w.bench.log | tail -8
echo "window=$((END-START))s rc=$RC pprof_ports=$PP"
echo "WRITE_DONE TAG=$TAG"
ls /tmp/${TAG}_w.* 2>/dev/null