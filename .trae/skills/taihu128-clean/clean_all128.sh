#!/bin/bash
# 128 集群全节点并发清理编排。
# 用法: bash clean_all128.sh [--full] [--force] [--dry-run]
#   先分发 clean128.sh 到 3 节点，再并发执行。
# 依赖: PROXY_PY 指向 nefs-proxy 的 proxy_client.py 绝对路径。
set -u
PROXY_PY="${PROXY_PY:-/path/to/proxy_client.py}"
HERE="$(cd "$(dirname "$0")" && pwd)"
NODES="100.71.128.11 100.71.128.12 100.71.128.13"
ARGS="$*"

echo "== upload clean128.sh to all nodes =="
for n in $NODES; do
  python3 "$PROXY_PY" --node "$n" upload --local "$HERE/clean128.sh" --remote /tmp/clean128.sh || { echo "upload fail $n"; exit 1; }
done

echo "== concurrent clean per node =="
mapfile -t JOBS
for n in $NODES; do
  python3 "$PROXY_PY" --node "$n" exec --cmd "bash /tmp/clean128.sh $n $ARGS" &
done
wait
echo "CLEAN_ALL_DONE"