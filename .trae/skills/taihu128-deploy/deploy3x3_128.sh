#!/bin/bash
# 128 集群单节点部署：3 实例（nvme{1,2,3}），单地址监听本机 IP，注册到 128 TiKV（明文）。
# 用法: bash deploy3x3_128.sh <NODE_IP> <BASE_NAME> [BIN]
#   NODE_IP   本节点 IP（100.71.128.11/12/13）与 -listen 地址
#   BASE_NAME 实例名前缀，如 n11（实例为 n11-nvme1/2/3）
#   BIN       二进制路径，默认 /tmp/taihu-server.<SHA>
set -e
NODE=$1
BASE=$2
BIN=${3:-/tmp/taihu-server.ae325c9}
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

echo "== stop old taihu-server (node=$NODE base=$BASE) =="
ps -eo pid,comm | awk '$2 ~ /^taihu-server/{print $1}' | xargs -r kill 2>/dev/null || true
for i in $(seq 1 20); do
  [ "$(ps -eo comm | grep -c '^taihu-server' || true)" = "0" ] && break
  sleep 1
done
echo "remaining: $(ps -eo comm | grep -c '^taihu-server' || true)"

echo "== make exec =="
chmod +x "$BIN"

# 3 实例：nvme1/2/3，dev 裸盘，db 落持久盘 /var/taihu-db/nvme{1,2,3}（不用 tmp）
for k in 1 2 3; do
  name="${BASE}-nvme${k}"
  dev="/dev/nvme${k}n1"
  db="/var/taihu-db/nvme${k}"
  listen_ip="$NODE"
  [ -e "$dev" ] || { echo "DEV_MISSING: $dev"; exit 1; }
  mkdir -p "$db"
  echo "== start $name -> listen=$listen_ip dev=$dev =="
  setsid nohup "$BIN" \
    -listen "$listen_ip" \
    -db "$db" \
    -dev "$dev" \
    -server-name "$name" \
    -tikv-pd "$PD" \
    >"/tmp/taihu-server-${name}.log" 2>&1 &
done

sleep 6
echo "== verify procs =="
ps -eo pid,comm,args | grep "$BIN" | grep -v grep
echo "== verify ports =="
ss -ltnp 2>/dev/null | grep taihu-server | awk '{print $4}' | sort -u | head
echo "DEPLOY3X3_DONE"