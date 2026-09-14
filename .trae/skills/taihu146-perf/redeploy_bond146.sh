#!/bin/bash
# 用 50ebbe4 新二进制重启本节点 3 个 taihu-server 实例：
#   - 前 2 个实例 -listen <BOND1_IP>（通告 bond1.2175，跨节点走 25G）
#   - 第 3 个实例 -listen <BOND2_IP>（通告 bond2.2372，跨节点走 25G）
# 本机 shm（/dev/<server-name>）不受影响，同机读走 shm。
# 用法: bash redeploy_bond146.sh <BOND1_IP> <BOND2_IP> <BASE_TAIHU_N>
#   例: bash redeploy_bond146.sh 10.153.28.202 10.155.17.202 TAIHU-0
set -e
B1=$1
B2=$2
BASE=$3
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
BIN=/tmp/taihu-server.50ebbe4
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

echo "== stop old taihu-server (b1=$B1 b2=$B2 base=$BASE) =="
ps -eo pid,comm | awk '$2 ~ /^taihu-server/{print $1}' | xargs -r kill 2>/dev/null || true
for i in $(seq 1 20); do
  N=$(ps -eo comm | grep -c '^taihu-server' || true)
  [ "$N" = "0" ] && break
  sleep 1
done
echo "remaining: $(ps -eo comm | grep -c '^taihu-server' || true)"

echo "== make exec =="
chmod +x "$BIN"

N0=$(echo "$BASE" | grep -oE '[0-9]+$')

# 实例0: nvme3 bond1 | 实例1: nvme4 bond1 | 实例2: nvme5 bond2
for k in 0 1 2; do
  name="TAIHU-$((N0+k))"
  off=$((k+3))              # nvme3, nvme4, nvme5
  dev="/dev/nvme${off}n1"
  db="/mnt/nvme2/taihu2-${name}/db"
  listen_ip="$B1"
  [ "$k" = "2" ] && listen_ip="$B2"
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
echo "REDEPLOY_BOND_DONE"