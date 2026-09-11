#!/bin/bash
# 6 节点并发清理编排：上传 clean146.sh 到全部节点，并发执行，收集结果。
# 依赖 nodef: 只要本机能运行 python 并连到代理（nefs-proxy），PROXY_PY 指向 proxy_client.py。
# 用法:
#   export PROXY_PY="/path/to/nefs-proxy/proxy_client.py"
#   bash clean_all146.sh [--full] [--force] [--dry-run]
set -u
NODES=(146 147 148 149 150 152)
declare -A BASE=( [146]=TAIHU-0 [147]=TAIHU-3 [148]=TAIHU-6 [149]=TAIHU-9 [150]=TAIHU-12 [152]=TAIHU-15 )
ARGS=""
for a in "$@"; do ARGS="$ARGS $a"; done
PROXY_PY="${PROXY_PY:-}"
[ -n "$PROXY_PY" ] || PROXY_PY="$(command -v proxy_client.py 2>/dev/null || true)"
if [ -z "$PROXY_PY" ] || [ ! -f "$PROXY_PY" ]; then
  echo "ERR: set PROXY_PY to proxy_client.py absolute path"; exit 1
fi
PY=$(command -v python3 || command -v python || echo python)

job() {
  local n=$1
  "$PY" "$PROXY_PY" --node "$n" \
    upload --local "$(dirname "$0")/clean146.sh" --remote /tmp/clean146.sh >/dev/null 2>&1 || { echo "node$n UPLOAD_FAIL"; return; }
  "$PY" "$PROXY_PY" --node "$n" exec --cmd "bash /tmp/clean146.sh ${BASE[$n]}${ARGS}" 2>&1 \
    | grep -E "CLEAN_DONE|procs left|no taihu-server|REFUSE|ABORT|free"
  echo "== node $n done =="
}

for n in "${NODES[@]}"; do job "$n" & done
wait
echo "CLEAN_ALL_DONE"