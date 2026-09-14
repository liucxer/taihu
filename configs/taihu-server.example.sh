#!/usr/bin/env bash
# taihu server 部署参数模板 —— 复制为 server.env 按环境填值后：
#   set -a; source server.env; set +a
#   taihu server --listen "$LISTEN" --db "$DB" --dev "$DEV" --server-name "$NAME" --pd "$PD" \
#     --io-uring "$IO_URING" ${IO_URING_IOPOLL:+--io-uring-iopoll} \
#     --batch "$BATCH" --batch-workers "$BATCH_WORKERS"
#
# 注意：长参数一律用双横线（--listen 而非 -listen；pflag 会把单横线长参数当短参数簇拒绝）。
# 参数定义权威源：cmd/taihu/cmd/server.go、cmd/taihu/cmd/root.go。

# ── 必填 ──────────────────────────────────────────────────────────────────
LISTEN=""          # 逗号分隔监听 IP（多网卡；RPC 端口在 [50000,51000] 自动分配，通告地址=首个 IP:端口）
DB=""              # Pebble 元数据目录（必填）
DEV=""             # 裸设备路径（必填；容量启动时读取并与 TiKV 记录比对，换盘/容量变化拒绝启动）
NAME=""            # 实例唯一标识（必填；= shmipc socket /dev/<NAME>、TiKV 注册与容量记录 key）
PD=""              # TiKV PD 地址列表（逗号分隔，必填）

# ── 可选 ──────────────────────────────────────────────────────────────────
IO_URING=auto      # 磁盘异步 IO 后端：auto=内核支持 io_uring 则用（其余回退 libaio）| on=强制 | off=libaio
IO_URING_IOPOLL=0  # 1=io_uring IOPOLL（仅 io_uring 生效；需 /sys/class/block/<dev>/queue/io_poll=1）
BATCH=0            # shm 批读批量：>0 启用"多 stream 多 worker"聚合批读（一次 io_submit 提交多个任务）
BATCH_WORKERS=8    # shm 批读 worker 池大小

# ── 真机参考（146 集群 6 节点 × 3 盘 18 实例：TAIHU-0..17）────────────────
# 每节点 2 实例听 bond1.2175、1 实例听 bond2.2372：
#   LISTEN=10.153.28.202                                # 本节点 bond1 地址
#   DB=/mnt/nvme1/taihu0-<name>-single/db
#   DEV=/dev/nvme1n1
#   NAME=TAIHU-0
#   PD=10.151.26.161:2379,10.151.26.162:2379,10.151.26.163:2379
#
# 128 集群（100.71.128.11/12/13）：
#   LISTEN=100.71.128.12
#   DB=/mnt/nvme2/taihu-<name>/db
#   DEV=/dev/nvme0n1
#   NAME=TAIHU-X
#   PD=100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379