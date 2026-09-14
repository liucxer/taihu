#!/bin/bash
# 单节点部署 3 个 taihu-server 实例，每实例 -listen <BOND1>,<BOND2>（同端口双地址监听）。
# 用法: bash deploy_dualip146.sh <BOND1_IP> <BOND2_IP> <BASE_TAIHU_N> [BIN]
#   例: bash deploy_dualip146.sh 10.153.28.202 10.155.17.202 TAIHU-0 /tmp/taihu-server.e536f07
#   BIN 缺省 /tmp/taihu-server（取 /tmp 下最新 taihu-server.* 亦可自行指定）
# 使用 146 集群自带的 TiKV PD（TLS 加密），证书路径固定：
#   /nefsdata/meta/tikv-deploy/pd-12379/tls/{ca.crt,pd.crt,pd.pem}
set -e
B1=$1
B2=$2
BASE=$3
BIN=${4:-/tmp/taihu-server}
PD="10.153.28.202:12379,10.153.28.203:12379,10.153.28.204:12379"
TLS_DIR="/nefsdata/meta/tikv-deploy/pd-12379/tls"
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

echo "== stop old taihu-server (dual-ip $B1,$B2 base=$BASE) =="
ps -eo pid,comm | awk '$2 ~ /^taihu-server/{print $1}' | xargs -r kill 2>/dev/null || true
for i in $(seq 1 20); do
  N=$(ps -eo comm | grep -c '^taihu-server' || true)
  [ "$N" = "0" ] && break
  sleep 1
done
STILL=$(ps -eo pid,comm | awk '$2 ~ /^taihu-server/{print $1}')
[ -n "$STILL" ] && echo "$STILL" | xargs -r kill -9 2>/dev/null || true
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
    -tikv-ca   "$TLS_DIR/ca.crt" \
    -tikv-cert "$TLS_DIR/pd.crt" \
    -tikv-key  "$TLS_DIR/pd.pem" \
    >"/tmp/taihu-server-${name}-dual.log" 2>&1 &
done

sleep 6
echo "== verify procs =="
ps -eo pid,comm,args | grep "$(basename $BIN)" | grep -v grep
echo "== verify ports (both bond IPs) =="
ss -ltnp 2>/dev/null | grep taihu-server | awk '{print $4}' | sort -u | grep -E '10\.(153\.28|155\.17)' | head
echo "DEPLOY_DUAL_DONE"