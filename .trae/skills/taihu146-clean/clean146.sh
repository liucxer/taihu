#!/bin/bash
# 清理单个节点上的 taihu 部署。
# 用法: bash clean146.sh <BASE_TAIHU_N> [--full] [--force] [--dry-run]
#   BASE_TAIHU_N  本节点 3 个实例的基准名，如 TAIHU-0（实例为 TAIHU-0/1/2）
#   --full        额外删除 db 目录与 /tmp 测试二进制/日志/pprof（默认仅停进程）
#   --force       跳过二次确认（FULL+删除前）
#   --dry-run     只打印将停/将删项，不实际执行
# 安全护栏：只动 taihu-server 进程与白名单 db 目录，绝不触碰 TiKV/bond 网络/磁盘分区表。
set -u
BASE=$1
MODE=STOP; FORCE=0; DRY=0
for a in "$@"; do
  case "$a" in --full) MODE=FULL;; --force) FORCE=1;; --dry-run) DRY=1;; esac
done
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

N0=$(echo "$BASE" | grep -oE '[0-9]+$')
[ -z "$N0" ] && { echo "BAD_BASE: $BASE"; exit 2; }

db_dirs=""
for k in 0 1 2; do
  name="TAIHU-$((N0+k))"
  db="/mnt/nvme2/taihu2-${name}-dual/db"
  case "$db" in
    /mnt/nvme2/taihu2-TAIHU-*-dual/db) : ;;
    *) echo "REFUSE_NON_WHITELIST: $db"; exit 3 ;;
  esac
  db_dirs="$db_dirs $db"
done

echo "== clean node base=$BASE mode=$MODE force=$FORCE dry_run=$DRY =="

# 1) 停进程：FIN<UNV 优雅停机（taihu-server 收到 TERM 会向 TiKV 注销），超时强杀兜底。
TPIDS=$(ps -eo pid,comm | awk '$2 ~ /^taihu-server/{print $1}')
if [ -z "$TPIDS" ]; then
  echo "no taihu-server running"
else
  echo "graceful stop: $TPIDS"
  if [ "$DRY" = "1" ]; then echo "[dry] would kill: $TPIDS"; else echo "$TPIDS" | xargs -r kill 2>/dev/null || true; fi
  for i in $(seq 1 20); do
    [ "$(ps -eo comm | grep -c '^taihu-server' || true)" = "0" ] && break
    sleep 1
  done
  STILL=$(ps -eo pid,comm | awk '$2 ~ /^taihu-server/{print $1}')
  if [ -n "$STILL" ]; then
    echo "force kill: $STILL"
    if [ "$DRY" = "1" ]; then echo "[dry] would kill -9"; else echo "$STILL" | xargs -r kill -9 2>/dev/null || true; fi
  fi
fi
# 给优雅退出留 time 完成 TiKV 注销；后等待心跳过期兜底（默认心跳周期~1s/过期窗口数秒）。
sleep 2

# 2) FULL：删除 db 目录与 /tmp 测试产物。
if [ "$MODE" = "FULL" ]; then
  if [ "$FORCE" != "1" ] && [ "$DRY" != "1" ]; then
    echo "will remove:$db_dirs"
    read -r -p "confirm remove db dirs? [y/N] " ans || ans=N
    case "$ans" in y|Y) ;; *) echo "ABORT"; exit 4 ;; esac
  fi
  for db in $db_dirs; do
    echo "remove: $db"
    [ "$DRY" = "1" ] || rm -rf "$db"
  done
  echo "-- remove /tmp artifacts --"
  # 注意：不匹配 clean*.sh，避免误删本脚本自身（节点上脚本需保留供重跑/检查）。
  for pat in "taihu-server.*" "taihu-rpc-bench.*" "*_dual.log" "*.cpu" "*.mem" "*p_rx.*" "*p_wt.*" "probe_*" "taihu-*.log" "taihu-srv-*.log" "taihu.*.tar.gz" "taihu-bench*" "taihu-cli*"; do
    echo "remove /tmp/${pat}"
    [ "$DRY" = "1" ] || rm -f /tmp/$pat 2>/dev/null || true
  done
  # 历史源码/目录残留
  for d in /tmp/taihu-src /tmp/taihu-go-sdk; do
    echo "remove dir ${d}"
    [ "$DRY" = "1" ] || rm -rf "$d" 2>/dev/null || true
  done
else
  echo "-- mode=STOP: db dirs & /tmp kept --"
fi

echo "-- final check --"
echo "taihu-server procs left: $(ps -eo comm | grep -c '^taihu-server' || true)"
ls -d /mnt/nvme2/taihu2-TAIHU-* 2>/dev/null || echo "no taihu2 dirs"
echo "FREE_KB $(df -k /mnt/nvme2 | tail -1 | awk '{print $4}')"
echo "CLEAN_DONE mode=$MODE dry=$DRY force=$FORCE"