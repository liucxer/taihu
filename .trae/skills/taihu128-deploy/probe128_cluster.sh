#!/bin/bash
# 128 集群探针读写验证（跨节点 TCP 数据面）。
# 用法: bash probe128_cluster.sh [TARGET_INSTANCE]
#   TARGET_INSTANCE 默认 n11-nvme1
set -e
INSTANCE=${1:-n11-nvme1}
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
BIN=/tmp/taihu-cli
KB=/tmp/probe128
echo "== gen 4MiB probe =="
head -c 4194304 /dev/urandom > $KB.obj
SRC=$(md5sum $KB.obj | awk '{print $1}')
echo "src md5=$SRC"
echo "== put via $INSTANCE (cross-node TCP) =="
$BIN key put --pd $PD --instance $INSTANCE --key probe/cluster-check --file $KB.obj || { echo PUT_FAIL; exit 1; }
echo "put ok"
echo "== get back =="
$BIN key get --pd $PD --instance $INSTANCE --key probe/cluster-check --file $KB.out || { echo GET_FAIL; exit 1; }
DST=$(md5sum $KB.out | awk '{print $1}')
echo "dst md5=$DST"
if [ "$SRC" = "$DST" ]; then echo "MD5_MATCH"; else echo "MD5_MISMATCH"; exit 1; fi
rm -f $KB.obj $KB.out
echo "PROBE128_DONE"