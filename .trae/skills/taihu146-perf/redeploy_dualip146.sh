#!/bin/bash
# 双 IP 全实例部署：本节点 3 个 taihu-server 实例，每实例 -listen <BOND1>,<BOND2>（同端口双地址监听）。
# 用法: bash redeploy_dualip146.sh <BOND1_IP> <BOND2_IP> <BASE_TAIHU_N>
#   例: bash redeploy_dualip146.sh 10.153.28.202 10.155.17.202 TAIHU-0
set -e
B1=$1
B2=$2
BASE=$3
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
BIN=/tmp/taihu-server.b137e7b
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

echo "== stop old taihu-server (dual-ip $B1,$B2 base=$BASE) =="
ps -eo pid,comm | awk '$2 ~ /^taihu-server/{print $1}' | xargs -r kill 2>/dev/null || true
for i in $(seq 1 20); do
  N=$(ps -eo comm | grep -c '^taihu-server' || true)
  [ "$N" = "0" ] && break
  sleep 1
done
echo "remaining: $(ps -eo comm | grep -c '^taihu-server' || true)"
chmod +x "$BIN"

N0=$(echo "$BASE" | grep -oE '[0-9]+$')
for k in 0 1 2; do
  name="TAIHU-$((N0+k))"
  off=$((k+3))                # nvme3, nvme4, nvme5
  dev="/dev/nvme${off}n1"
  db="/mnt/nvme2/taihu2-${name}-dual/db"
  mkdir -p "$db"
  echo "== start $name listen=$B1,$B2 dev=$dev =="
  setsid nohup "$BIN" \
    -listen "$B1,$B2" \
    -db "$db" \
    -dev "$dev" \
    -server-name "$name" \
    -tikv-pd "$PD" \
    >"/tmp/taihu-server-${name}-dual.log" 2>&1 &
done

sleep 6
echo "== verify procs =="
ps -eo pid,comm,args | grep "$BIN" | grep -v grep
echo "== verify ports (both bond IPs) =="
ss -ltnp 2>/dev/null | grep taihu-server | awk '{print $4}' | sort -u | grep -E '10\.(153\.28|155\.17)' | head
echo "REDEPLOY_DUAL_DONE"