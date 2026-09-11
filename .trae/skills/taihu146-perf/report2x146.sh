#!/bin/bash
# 分析双客户端测试产物（TAG_wx / TAG_rx）。
# 用法: bash report2x146.sh <TAG> <phase>   (phase=w|r)
export TAG=$1
export PH=$2
echo "===== $(hostname) report TAG=$TAG phase=$PH (2x) ====="
echo "--- bench A ---"
grep -E 'objects=|throughput:|latency:' /tmp/${TAG}_${PH}x.ca.log 2>/dev/null | head -3
echo "--- bench B ---"
grep -E 'objects=|throughput:|latency:' /tmp/${TAG}_${PH}x.cb.log 2>/dev/null | head -3
echo "--- frame A/B (take/copy) ---"
tail -10 /tmp/${TAG}_${PH}x.ca.log 2>/dev/null | grep -E 'take=|data4MiB' | head -2
tail -10 /tmp/${TAG}_${PH}x.cb.log 2>/dev/null | grep -E 'take=|data4MiB' | head -2
echo "--- physical CPU (mpstat all-avg) ---"
awk '$3=="all"{b+=100-$NF; usr+=$4; sy+=$6; iow+=$7; n++}
     END{if(n>0) printf "busy=%.1f%% usr=%.1f%% sys=%.1f%% iowait=%.1f%% samples=%d\n", b/n,usr/n,sy/n,iow/n,n}' /tmp/${TAG}_${PH}x.mpstat 2>/dev/null
echo "--- proc CPU (pidstat avg%) ---"
python3 - <<'PYEOF'
import os,re
from collections import defaultdict
t=os.environ.get("TAG",""); p=os.environ.get("PH","")
d=defaultdict(lambda:[0.0,0])
try:
    for ln in open("/tmp/%s_%sx.pidstat"%(t,p)):
        m=re.match(r'\s*(\d+)\s+(\S+)\s+([0-9.]+)', ln)
        if m: d[m.group(2)][0]+=float(m.group(3)); d[m.group(2)][1]+=1
except Exception as e: print("pidstat err",e)
for k,v in sorted(d.items()):
    print("%s: avg=%.1f%% (%d samples)"%(k,v[0]/max(v[1],1),v[1]))
PYEOF
echo "--- network delta MB ---"
python3 - <<'PYEOF'
import os,re
t=os.environ.get("TAG",""); p=os.environ.get("PH","")
def parse(f):
    d={}
    if not os.path.exists(f): return d
    for ln in open(f):
        m=re.match(r'\s*(\S+):\s*(\d+)\s+(\d+)', ln)
        if m: d[m.group(1)]=(int(m.group(2)),int(m.group(3)))
    return d
b=parse("/tmp/%s_%sx.net.base"%(t,p)); e=parse("/tmp/%s_%sx.net.end"%(t,p))
for k in e:
    if k in b:
        print("%s: rx %+.1fMB tx %+.1fMB"%(k,(e[k][0]-b[k][0])/1e6,(e[k][1]-b[k][1])/1e6))
PYEOF
echo "REPORT2X_DONE $TAG"