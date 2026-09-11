#!/bin/bash
# 146 集群综合跨节点读测试（单节点）：本节点作为 读客户端（读远端前缀=TCP 跨节点）
# + 数据节点（被其它节点读本机前缀），采集：物机CPU mpstat、进程CPU pidstat、网络差分、
# 客户端 pprof、本机 3 server pprof。
# 用法: bash rtest_all146.sh <HOST> <READ_PREFIX> <TAG> [COUNT] [THREADS]
set -e
HOST=$1
RPRE=$2
TAG=$3
COUNT=${4:-4000}
THREADS=${5:-16}
BENCH=/tmp/taihu-rpc-bench.50ebbe4
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

rm -f /tmp/${TAG}_r.*
echo "== start samplers (TAG=$TAG reader=$HOST prefix=$RPRE) =="
setsid nohup mpstat -P ALL 1 > /tmp/${TAG}_r.mpstat 2>&1 &
MP=$!
setsid nohup sh -c 'for t in $(seq 1 400); do ps -eo pid,comm,%cpu | grep -E "taihu-rpc-bench|taihu-server"; sleep 1; done' > /tmp/${TAG}_r.pidstat 2>/dev/null &
PS=$!
grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372|lo)\b' /proc/net/dev > /tmp/${TAG}_r.net.base

# 本机 3 server pprof（本节点数据被远端读，抓读侧服务端处理）
PP=$(ss -ltnp 2>/dev/null | grep taihu-server | grep -oE '\*:[0-9]+' | grep -oE '[0-9]+$' | sort -u | head -3)
i=0
for p in $PP; do
  i=$((i+1))
  curl -s --max-time 40 "http://127.0.0.1:${p}/debug/pprof/profile?seconds=30" -o /tmp/${TAG}_r.s${i}.cpu &
  eval SPID_${i}=$!
done

echo "== run bench read (all keys -> remote instance, TCP) =="
START=$(date +%s)
$BENCH -client-name "$HOST" -tikv-pd "$PD" \
  -mode read -size 4194304 -count "$COUNT" -threads "$THREADS" \
  -keys-prefix "$RPRE" -report-interval 10s -latency -preload \
  -cpuprofile /tmp/${TAG}_r.client.cpu > /tmp/${TAG}_r.bench.log 2>&1
RC=$?
END=$(date +%s)
for i in 1 2 3; do eval wait \$SPID_${i} 2>/dev/null || true; done
sleep 1
grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372|lo)\b' /proc/net/dev > /tmp/${TAG}_r.net.end
kill $MP $PS 2>/dev/null || true

echo "== bench log =="
grep -E 'throughput|latency|====|preloaded|frame stats|ops/s' /tmp/${TAG}_r.bench.log | tail -10
echo "window=$((END-START))s rc=$RC"
echo "READ_ALL_DONE TAG=$TAG"
ls /tmp/${TAG}_r.* 2>/dev/null