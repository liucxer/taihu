#!/bin/bash
# 双网卡均分版跨节点读带宽上限探针：146 上 N_CLI 个客户端并行读 147 的前缀（w-c147-A/B），
# 每客户端 CONNS 条连接/每地址 × THREADS 并发；抓 bond1.2175/bond2.2372 差分计算实际总带宽。
# 用法: bash probe_max146_dual.sh <N_CLI> <CONNS> <THREADS> <COUNT> <TAG>
set -e
NCLI=$1
CONNS=$2
THREADS=$3
COUNT=$4
TAG=$5
BENCH=/tmp/taihu-rpc-bench.b137e7b
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

echo "== probe_max_dual NCLI=$NCLI CONNS=$CONNS THREADS=$THREADS COUNT=$COUNT TAG=$TAG =="
grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372|lo)\b' /proc/net/dev > /tmp/${TAG}_pm.net.base
START=$(date +%s)

PIDS=()
for ((i=0; i<NCLI; i++)); do
  pre="w-c147-A"
  [ $((i % 2)) -eq 1 ] && pre="w-c147-B"
  $BENCH -client-name "RD146-$i" -tikv-pd "$PD" \
    -mode read -size 4194304 -count "$COUNT" -threads "$THREADS" \
    -conns "$CONNS" -keys-prefix "$pre" -report-interval 10s -latency -preload \
    > /tmp/${TAG}_pm.c$i.log 2>&1 &
  PIDS+=($!)
done
for p in "${PIDS[@]}"; do wait "$p"; done
RC=$?
END=$(date +%s)
W=$((END-START))

grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372|lo)\b' /proc/net/dev > /tmp/${TAG}_pm.net.end

echo "== per-client results =="
TOT_MB=0
for ((i=0; i<NCLI; i++)); do
  ops=$(grep -E '^throughput:' /tmp/${TAG}_pm.c$i.log | awk '{print $2}')
  mb=$(grep -E '^throughput:' /tmp/${TAG}_pm.c$i.log | awk '{print $4}')
  take=$(tail -12 /tmp/${TAG}_pm.c$i.log | grep -oE 'take=[0-9]+' | head -1)
  echo "client$i: ops=$ops MiB/s=$mb take=$take"
  TOT_MB=$(echo "$TOT_MB+$mb" | bc 2>/dev/null || python3 -c "print($TOT_MB+$mb)")
done
echo "CLIENT_AGG_MiBs=$(echo "scale=1; $TOT_MB" | bc 2>/dev/null || echo $TOT_MB)"
echo "== total network delta =="
python3 - <<PYEOF
import re
def parse(f):
    d={}
    for ln in open(f):
        m=re.match(r'\s*(\S+):\s*(\d+)\s+(\d+)', ln)
        if m: d[m.group(1)]=(int(m.group(2)),int(m.group(3)))
    return d
b=parse("/tmp/${TAG}_pm.net.base"); e=parse("/tmp/${TAG}_pm.net.end")
tot=0
for k in ("bond0.1326","bond1.2175","bond2.2372","lo"):
    if k in b and k in e:
        rx=(e[k][0]-b[k][0])/1e6; tx=(e[k][1]-b[k][1])/1e6
        if k!="lo": tot+=rx
        print("%s: rx %+.1fMB tx %+.1fMB"%(k,rx,tx))
print("NET_TOTAL_RX_GB=%.1f WINDOW=%ds AGG_MiBs=%.1f"%(tot/1000,${W},tot/${W}/1.048576))
PYEOF
echo "window=${W}s rc=$RC"
echo "PROBE_MAX_DONE TAG=$TAG th=$THREADS conns=$CONNS ncli=$NCLI"