#!/bin/bash
# taihu 146 集群 单节点双客户端写性能测试（2 bench 并行，独立前缀）：
# 采集 物机CPU(1×mpstat) + 进程CPU(2 client + 3 server) + 网络差分 + 2 客户端 pprof + 3 服务端 pprof。
# 用法: bash wtest2x146.sh <HOSTNAME> <PREFIX_A> <PREFIX_B> <TAG> [COUNT_PER_CLIENT] [THREADS]
set -e
HOST=$1
PA=$2
PB=$3
TAG=$4
COUNT=${5:-20000}
THREADS=${6:-32}
BENCH=/tmp/taihu-rpc-bench.b137e7b
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

rm -f /tmp/${TAG}_wx.*
echo "== start samplers (TAG=$TAG dual-client write) =="
setsid nohup mpstat -P ALL 1 > /tmp/${TAG}_wx.mpstat 2>&1 &
MP=$!
setsid nohup sh -c 'for t in $(seq 1 240); do ps -eo pid,comm,%cpu | grep -E "taihu-rpc-bench|taihu-server"; sleep 1; done' > /tmp/${TAG}_wx.pidstat 2>/dev/null &
PS=$!
grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372|lo)\b' /proc/net/dev > /tmp/${TAG}_wx.net.base

# 本节点 3 server pprof
PP=$(ss -ltnp 2>/dev/null | grep taihu-server | grep -oE '\*:[0-9]+' | grep -oE '[0-9]+$' | sort -u | head -3)
i=0
for p in $PP; do
  i=$((i+1))
  curl -s --max-time 40 "http://127.0.0.1:${p}/debug/pprof/profile?seconds=28" -o /tmp/${TAG}_wx.s${i}.cpu &
  eval PPID_${i}=$!
done

echo "== run bench A+B write (parallel) =="
START=$(date +%s)
$BENCH -client-name "$HOST-A" -tikv-pd "$PD" \
  -mode write -size 4194304 -count "$COUNT" -threads "$THREADS" \
  -keys-prefix "$PA" -report-interval 5s -latency \
  -cpuprofile /tmp/${TAG}_wx.ca.cpu > /tmp/${TAG}_wx.ca.log 2>&1 &
CA=$!
$BENCH -client-name "$HOST-B" -tikv-pd "$PD" \
  -mode write -size 4194304 -count "$COUNT" -threads "$THREADS" \
  -keys-prefix "$PB" -report-interval 5s -latency \
  -cpuprofile /tmp/${TAG}_wx.cb.cpu > /tmp/${TAG}_wx.cb.log 2>&1 &
CB=$!
wait $CA $CB
RC=$?
END=$(date +%s)

for i in 1 2 3; do eval wait \$PPID_${i} 2>/dev/null || true; done
sleep 1
grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372|lo)\b' /proc/net/dev > /tmp/${TAG}_wx.net.end
kill $MP $PS 2>/dev/null || true

echo "== bench A log =="
grep -E 'objects=|throughput:|latency:' /tmp/${TAG}_wx.ca.log | head -3
echo "== bench B log =="
grep -E 'objects=|throughput:|latency:' /tmp/${TAG}_wx.cb.log | head -3
echo "window=$((END-START))s rc=$RC"
echo "WRITE2X_DONE TAG=$TAG"
ls /tmp/${TAG}_wx.* 2>/dev/null | tr '\n' ' '