#!/bin/bash
# 跨节点 bond 读验证：146 读 147 写的前缀（b1m-c147），抓 bond1.2175/bond2.2372/bond0.1326 差分。
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
echo "== net base =="
grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372):' /proc/net/dev > /tmp/xb.net.base
/tmp/taihu-rpc-bench.50ebbe4 -client-name RD146B -tikv-pd "$PD" \
  -mode read -size 4194304 -count 2000 -threads 16 -keys-prefix b1m-c147 -preload \
  -cpuprofile /tmp/xb.client.cpu 2>&1 | grep -E '====|throughput|objects|error|preloaded' | head -8
echo "== net end =="
grep -E '^(bond0\.1326|bond1\.2175|bond2\.2372):' /proc/net/dev > /tmp/xb.net.end
python3 - <<'PYEOF'
import re
def parse(f):
    d={}
    for ln in open(f):
        m=re.match(r'\s*(\S+):\s*(\d+)\s+(\d+)',ln)
        if m: d[m.group(1)]=(int(m.group(2)),int(m.group(3)))
    return d
b=parse("/tmp/xb.net.base"); e=parse("/tmp/xb.net.end")
for k in sorted(e):
    if k in b:
        print("%s: rx %+.1fMB tx %+.1fMB"%(k,(e[k][0]-b[k][0])/1e6,(e[k][1]-b[k][1])/1e6))
PYEOF
echo "XBOND_DONE"