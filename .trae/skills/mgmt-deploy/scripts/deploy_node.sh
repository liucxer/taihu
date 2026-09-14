#!/usr/bin/env bash
#===============================================================================
# mgmt 单节点部署脚本（在目标节点上运行）
# 生成配置 -> 准备目录 -> 启动容器。配合 mgmt-deploy skill 使用。
#
# 用法:
#   bash deploy_node.sh <NODE_ID> <NODE_IP> <PEERS> <CLUSTER_NAME> \
#       [DATA_DIR] [LOG_DIR] [CONF_SRC] [IMAGE] [LISTEN]
#
# 参数:
#   NODE_ID      节点 id，三节点为 1/2/3
#   NODE_IP      本节点管理地址（写入 conf 的 ip）
#   PEERS        Raft 成员串，如 "1:100.71.128.11:12379,2:100.71.128.12:12379,3:100.71.128.13:12379"
#   CLUSTER_NAME 集群名
#   DATA_DIR     数据/配置根目录，默认 /export/Data/mgmt-deploy
#   LOG_DIR      日志目录，默认 /export/Logs/mgmt-deploy
#   CONF_SRC     配置模板源文件；为空则用脚本同目录 ../conf/mgmt.conf.tpl
#   IMAGE        镜像名:tag，默认 nefs-mgmt:1.6.0-deploy
#   LISTEN       管理端口，默认 12379
#
# 功能: 只做配置生成+目录准备+容器启停，不负责镜像分发/编译。
# 环境变量可用预置默认值，也可用参数覆盖。
#===============================================================================
set -euo pipefail

NODE_ID="${1:?缺少 NODE_ID}"
NODE_IP="${2:?缺少 NODE_IP}"
PEERS="${3:?缺少 PEERS}"
CLUSTER_NAME="${4:?缺少 CLUSTER_NAME}"
DATA_DIR="${5:-/export/Data/mgmt-deploy}"
LOG_DIR="${6:-/export/Logs/mgmt-deploy}"
CONF_SRC="${7:-$(dirname "$0")/../conf/mgmt.conf.tpl}"
IMAGE="${8:-nefs-mgmt:1.6.0-deploy}"
LISTEN="${9:-12379}"
CONTAINER=nefs-mgmt
CONF_DST="$DATA_DIR/mgmt.conf"

echo "[1/4] 准备目录 $DATA_DIR / $LOG_DIR"
mkdir -p "$DATA_DIR" "$LOG_DIR"

echo "[2/4] 生成配置 $CONF_DST"
sed -e "s|__NODE_IP__|$NODE_IP|g" \
    -e "s|__NODE_ID__|$NODE_ID|g" \
    -e "s|__PEERS__|$PEERS|g" \
    -e "s|__LOG_DIR__|$LOG_DIR|g" \
    -e "s|__DATA_DIR__|$DATA_DIR|g" \
    -e "s|__CLUSTER_NAME__|$CLUSTER_NAME|g" \
    -e "s|__LISTEN__|$LISTEN|g" \
    "$CONF_SRC" > "$CONF_DST"
echo "--- 生成配置 ---"
cat "$CONF_DST"

echo "[3/4] 检查端口 $LISTEN / heartbeat 5901 / replica 5902"
for p in "$LISTEN" 5901 5902; do
  if ss -ltn 2>/dev/null | grep -q ":$p "; then
    echo "WARN: 端口 $p 已被占用，可能与本节点其他 mgmt 冲突，请确认是否继续"
  fi
done

echo "[4/4] 启动容器 $CONTAINER"
docker rm -f "$CONTAINER" 2>/dev/null || true
docker run -d \
  --name "$CONTAINER" \
  --network host \
  --restart unless-stopped \
  -v "$DATA_DIR:$DATA_DIR" \
  -v "$LOG_DIR:$LOG_DIR" \
  -v "$CONF_DST:/etc/mgmt.conf" \
  "$IMAGE" master

echo "部署完成。验证: curl -s http://127.0.0.1:$LISTEN/mgmt/v1/mgmt-cluster-info"