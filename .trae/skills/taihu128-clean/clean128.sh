#!/bin/bash
# 清理 128 集群单个节点上的 taihu 部署。
# 用法: bash clean128.sh <NODE_IP> [--full] [--force] [--dry-run]
#   NODE_IP  本节点 IP（100.71.128.11/12/13），实例为 n{node}-nvme{1,2,3}
#   --full        额外删除 db 目录与 /tmp 测试二进制/日志/pprof（默认仅停进程）
#   --force       跳过二次确认（FULL+删除前）
#   --dry-run     只打印将停/将删项，不实际执行
# 安全护栏：只动 taihu-server 进程与白名单 db 目录，绝不触碰 TiKV/网络/磁盘分区表。
set -u
NODE=$1
MODE=STOP; FORCE=0; DRY=0
for a in "$@"; do
  case "$a" in --full) MODE=FULL;; --force) FORCE=1;; --dry-run) DRY=1;; esac
done
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH

# 校验节点 IP 在白名单内
case "$NODE" in
  100.71.128.11|100.71.128.12|100.71.128.13) : ;;
  *) echo "REFUSE_NODE: $NODE"; exit 3 ;;
esac

db_dirs=""
for k in 1 2 3; do
  db="/tmp/taihu-db/nvme$k"
  case "$db" in
    /tmp/taihu-db/nvme[0-9]) : ;;
    *) echo "REFUSE_NON_WHITELIST: $db"; exit 3 ;;
  esac
  db_dirs="$db_dirs $db"
done

echo "== clean node=$NODE mode=$MODE force=$FORCE dry_run=$DRY =="

# 1) 停进程：优雅停机（taihu-server 收到 TERM 会向 TiKV 注销），超时强杀兜底。
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
# 给优雅退出留时间完成 TiKV 注销。
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
  # 注意：不匹配 clean*.sh / check*.sh / stop*.sh，避免误删本脚本自身及检查脚本。
  for pat in "taihu-server.*" "taihu-rpc-bench.*" "*.cpu" "*.mem" "taihu-db*"; do
    echo "remove /tmp/${pat}"
    # db 目录已在上面单独白名单删除，这里只清理台面文件
    [ "$DRY" = "1" ] || rm -f /tmp/$pat 2>/dev/null || true
  done
  # 历史源码/目录残留（不删 check*/stop*，需保留供检查）
  for d in /tmp/taihu-src /tmp/taihu-go-sdk; do
    echo "remove dir ${d}"
    [ "$DRY" = "1" ] || rm -rf "$d" 2>/dev/null || true
  done
else
  echo "-- mode=STOP: db dirs & /tmp kept --"
fi

echo "-- final check --"
echo "taihu-server procs left: $(ps -eo comm | grep -c '^taihu-server' || true)"
ls -d /tmp/taihu-db/* 2>/dev/null || echo "no taihu-db dirs"
echo "FREE_KB $(df -k /tmp | tail -1 | awk '{print $4}')"
echo "CLEAN_DONE mode=$MODE dry=$DRY force=$FORCE"