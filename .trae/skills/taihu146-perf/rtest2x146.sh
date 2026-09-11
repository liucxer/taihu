#!/bin/bash
# taihu 146 集群 单节点双客户端跨节点读测试（2 bench 并行，分别读两个远端节点的前缀）：
# 采集 物机CPU + 进程CPU + 网络差分 + 2 客户端 pprof + 3 服务端 pprof（本机被远端读）。
# 用法: bash rtest2x146.sh <HOSTNAME> <RPRE_A> <RPRE_B> <TAG> [COUNT_PER_CLIENT] [THREADS] [CONNS]
set -e
HOST=$1
RA=$2
RB=$3
TAG=$4
COUNT=${5:-10000}
THREADS=${6:-32}
CONNS=${7:-1}
BENCH=/tmp/taihu-rpc-bench.b137e7b
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

rm -f /tmp/${TAG}_rx.*
echo "== start samplers (TAG=$TAG dual-client cross-node read) =="
setsid nohup mpstat -P ALL 1 > /tmp/${TAG}_rx.mpstat 2>&1 &
MP=$!
setsid nohup sh -c 'for t in $(seq 1 300); do ps -eo pid,comm,%cpu | grep -E "taihu-rpc-bench|taihu-server"; sleep 1; done' > /tmp/${TAG}_rx.pidstat 2>/dev/null &
PS=$!
grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372|lo)\b' /proc/net/dev > /tmp/${TAG}_rx.net.base

# 本节点 3 server pprof（本机数据被远端读）
PP=$(ss -ltnp 2>/dev/null | grep taihu-server | grep -oE '\*:[0-9]+' | grep -oE '[0-9]+$' | sort -u | head -3)
i=0
for p in $PP; do
  i=$((i+1))
  curl -s --max-time 40 "http://127.0.0.1:${p}/debug/pprof/profile?seconds=28" -o /tmp/${TAG}_rx.s${i}.cpu &
  eval SPID_${i}=$!
done

echo "== run bench A+B cross-node read (parallel) =="
START=$(date +%s)
$BENCH -client-name "$HOST-A" -tikv-pd "$PD" \
  -mode read -size 4194304 -count "$COUNT" -threads "$THREADS" \
  -conns "$CONNS" -keys-prefix "$RA" -report-interval 10s -latency -preload \
  -cpuprofile /tmp/${TAG}_rx.ca.cpu > /tmp/${TAG}_rx.ca.log 2>&1 &
CA=$!
$BENCH -client-name "$HOST-B" -tikv-pd "$PD" \
  -mode read -size 4194304 -count "$COUNT" -threads "$THREADS" \
  -conns "$CONNS" -keys-prefix "$RB" -report-interval 10s -latency -preload \
  -cpuprofile /tmp/${TAG}_rx.cb.cpu > /tmp/${TAG}_rx.cb.log 2>&1 &
CB=$!
wait $CA $CB
RC=$?
END=$(date +%s)

for i in 1 2 3; do eval wait \$SPID_${i} 2>/dev/null || true; done
sleep 1
grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372|lo)\b' /proc/net/dev > /tmp/${TAG}_rx.net.end
kill $MP $PS 2>/dev/null || true

echo "== bench A log =="
grep -E 'objects=|throughput:|latency:|preloaded' /tmp/${TAG}_rx.ca.log | head -4
echo "== bench B log =="
grep -E 'objects=|throughput:|latency:|preloaded' /tmp/${TAG}_rx.cb.log | head -4
echo "window=$((END-START))s rc=$RC"
echo "READ2X_DONE TAG=$TAG"
ls /tmp/${TAG}_rx.* 2>/dev/null | tr '\n' ' '