#!/bin/bash
# 跨节点读带宽上限探针：146 上 N_CLI 个客户端并行读 147 的前缀（w2-c147A/B），
# 每客户端 CONNS 条 TCP 连接 × THREADS 并发；抓 bond1.2175/bond2.2372 差分计算实际总带宽。
# 用法: bash probe_max146.sh <N_CLI> <CONNS> <THREADS> <COUNT> <TAG>
set -e
NCLI=$1
CONNS=$2
THREADS=$3
COUNT=$4
TAG=$5
BENCH=/tmp/taihu-rpc-bench.50ebbe4
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

echo "== probe_max NCLI=$NCLI CONNS=$CONNS THREADS=$THREADS COUNT=$COUNT TAG=$TAG =="
grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372|lo)\b' /proc/net/dev > /tmp/${TAG}_pm.net.base
START=$(date +%s)

# NCLI 个客户端，交替读 147 的两个前缀（数据都在节点147的3个实例上）
PIDS=()
for ((i=0; i<NCLI; i++)); do
  pre="w2-c147A"
  [ $((i % 2)) -eq 1 ] && pre="w2-c147B"
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
TOT_OPS=0; TOT_MB=0
for ((i=0; i<NCLI; i++)); do
  ops=$(grep -E '^throughput:' /tmp/${TAG}_pm.c$i.log | awk '{print $2}')
  mb=$(grep -E '^throughput:' /tmp/${TAG}_pm.c$i.log | awk '{print $4}')
  take=$(tail -12 /tmp/${TAG}_pm.c$i.log | grep -oE 'take=[0-9]+' | head -1)
  echo "client$i: ops=$ops MiB/s=$mb take=$take"
done
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
print("NET_TOTAL_RX_MB=%.1f WINDOW=%ds AGG_MiBs=%.1f"%(tot,${W},tot/${W}/1.048576))
PYEOF
echo "window=${W}s rc=$RC"
echo "PROBE_MAX_DONE TAG=$TAG th=$THREADS conns=$CONNS ncli=$NCLI"
ls /tmp/${TAG}_pm.c0.log >/dev/null 2>&1 && echo "logs ok"