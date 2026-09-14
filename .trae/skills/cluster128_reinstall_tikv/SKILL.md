---
name: "cluster128_reinstall_tikv"
description: "重装 128 集群（100.71.128.11/12/13）三节点 TiKV+PD 容器：1 停止 tikv/pd 容器 → 2 删除数据目录 → 3 启动 tikv/pd 容器。当用户要求在 128 集群重建/重装/重置 TiKV、清除已损坏或过期的 PD/TiKV 数据、恢复到干净集群时使用。"
---

# 128 集群 TiKV 重装（三节点）

在三节点 100.71.128.11/12/13 上，通过「停止容器 → 删除数据目录 → 启动容器」三步把整集群 PD+TiKV 重装成全新空集群。

> 本 skill 覆盖**整集群**（pd1/tikv1, pd2/tikv2, pd3/tikv3 全部重建）。若只重装单个节点，可对单节点重复三步（但建议重装整集群以保证 region/元数据一致）。

## 拓扑与部署基线（实测确认，2026-09-13）

| 节点 | PD 容器 | TiKV 容器 | PD 监听 | TiKV 数据面/状态口 | 数据目录（宿主） |
|------|---------|-----------|---------|-------------------|------------------|
| 128.11 | `pd1` | `tikv1` | 2379/2380 | 20160/20180 | `/var/lib/jenkins/tikv3` |
| 128.12 | `pd2` | `tikv2` | 2379/2380 | 20160/20180 | `/var/lib/jenkins/tikv3` |
| 128.13 | `pd3` | `tikv3` | 2379/2380 | 20160/20180 | `/mnt/nvme0n1/tikv3` |

- 镜像：`registry.paas/nefs/ukv_pd_arm:v7.5.6`（PD）、`registry.paas/nefs/ukv_server_arm:v7.5.6`（TiKV），arm64，host 网络。
- 数据目录挂载：容器内 `/nefsdata/tikv` ← bind 宿主 `/var/lib/jenkins/tikv3`（11/12）或 `/mnt/nvme0n1/tikv3`（13）。
- 访问通道：nefs-proxy（节点 9527，token `95279527`）；三节点间 PD `--initial-cluster=pd1=http://100.71.128.11:2380,pd2=...,pd3=...`。
- 内网 registry 不可达时，需先 `docker save` 现有镜像 → 9527 分发 → `docker load`（本 skill 依赖镜像已在节点上存在）。

## 三个步骤

### 步骤 1：停止 tikv 和 pd 容器

每节点串行：先停 TiKV（`tikvN`），再停 PD（`pdN`），避免节副的同时停止顺序影响。

对每个节点 N ∈ {11,12,13}，容器名 = `pd${N 末位}`/`tikv${N 末位}`（即 11→pd1/tikv1，12→pd2/tikv2，13→pd3/tikv3）：

```bash
# 任选节点（PowerShell 经 proxy /exec）
docker stop tikv2 && docker stop pd2

# 或按末位动态（bash 内循环）
node=12; n=${node#*12}; docker stop tikv$n; docker stop pd$n
```

> 也可保留容器用 `docker start` 复用（不 rm）。本 skill 的"重装"用 stop 而非 rm，是为了在步骤 3 用相同 run 参数 `docker start` 拉起；若曾 `--rm` 或需改参数，则改用 `docker rm -f` + 重新 `docker run`（见步骤 3 备用）。

验证已停：
```bash
docker ps --format '{{.Names}} {{.Status}}' | grep -E 'tikv|pd'   # 应无 tikv/pd 行
```

重复此步骤到 11、13 节点。

### 步骤 2：删除 tikv 和 pd 的数据目录

删除各节点宿主数据目录 **PD 数据（`<data>/pd`）与 TiKV 数据（`<data>/tikv`）**：

```bash
# 128.11 与 128.12
rm -rf /var/lib/jenkins/tikv3/pd /var/lib/jenkins/tikv3/tikv
# 128.13
rm -rf /mnt/nvme0n1/tikv3/pd /mnt/nvme0n1/tikv3/tikv
```

> 只删 PD/TiKV 的数据子目录，**保留挂载盘结构**（不卸载、不重建文件系统）。删除的正是容器 `--data-dir=/nefsdata/tikv/pd` 与 `/nefsdata/tikv/tikv` 在宿主侧的映射。

验证目录已清空：
```bash
ls -la /var/lib/jenkins/tikv3/   # 应只有空挂载点或零星文件，无 pd/ tikv/ 子目录（128.13 同理查 /mnt/nvme0n1/tikv3/）
```

### 步骤 3：启动 tikv 和 pd 容器

先启 PD（三节点都要, 需用 `--initial-cluster` 一次性组集群），再启 TiKV。最稳妥顺序：**三个 PD 全部启动**，再依次启动各 TiKV。

若容器仍在（只 stop 未 rm），**必须逐节点在各自节点启动本机容器**（容器只存在于各自节点，跨节点一次 start 会报 No such container）：

```bash
# 128.11
docker start pd1; docker start tikv1
# 128.12
docker start pd2; docker start tikv2
# 128.13
docker start pd3; docker start tikv3
```

若容器被 `docker rm` 删掉或需重建，按以下原始参数 `docker run`（host 网络 + bind 数据目录）重新创建：

```bash
# PD（每节点 IP 不同；128.12 示例）
docker run -d --name pd2 --network host --restart unless-stopped \
  -v /var/lib/jenkins/tikv3:/nefsdata/tikv -v /etc/localtime:/etc/localtime:ro \
  registry.paas/nefs/ukv_pd_arm:v7.5.6 \
  --name=pd2 --data-dir=/nefsdata/tikv/pd \
  --client-urls=http://100.71.128.12:2379 --advertise-client-urls=http://100.71.128.12:2379 \
  --peer-urls=http://100.71.128.12:2380 --advertise-peer-urls=http://100.71.128.12:2380 \
  --initial-cluster=pd1=http://100.71.128.11:2380,pd2=http://100.71.128.12:2380,pd3=http://100.71.128.13:2380

# TiKV（128.12 示例；三节点的 --pd-endpoints 均指向全部 3 个 PD）
docker run -d --name tikv2 --network host --restart unless-stopped \
  -v /var/lib/jenkins/tikv3:/nefsdata/tikv -v /etc/localtime:/etc/localtime:ro \
  registry.paas/nefs/ukv_server_arm:v7.5.6 \
  --data-dir=/nefsdata/tikv/tikv \
  --addr=100.71.128.12:20160 --status-addr=100.71.128.12:20180 \
  --advertise-addr=100.71.128.12:20160 \
  --pd-endpoints=http://100.71.128.11:2379,http://100.71.128.12:2379,http://100.71.128.13:2379
```

## 验证（整集群健康）

选一可达节点执行：

```bash
# PD 成员健康（应 3 个 true）
curl -s http://100.71.128.11:2379/pd/api/v1/health
# TiKV store 状态（应 3 个 Up）
curl -s http://100.71.128.11:2379/pd/api/v1/stores
# PD leader
curl -s http://100.71.128.11:2379/pd/api/v1/leader
```

预期：所有 PD 成员 `health=true`、3 个 TiKV store 全部 `Up`、leader 存在。此时集群为全新空状态。

## 经验教训（实测 2026-09-13）

本次重装顺利，坑主要在**跨节点 docker 操作**与**验证脚本**，记录如下：

1. **跨节点一次 `docker start` 不可行**：容器（pd1-3/tikv1-3）只存在于各自节点，在一台节点上执行 `docker start pd1 pd2 pd3` 会报 `No such container`。**必须逐节点、在各自节点上启动本机容器**（128.11 启 pd1/tikv1，128.12 启 pd2/tikv2，128.13 启 pd3/tikv3）。下面的启动命令已按此修正。
2. **先停依赖 nefs.client/taihu-server 再重装**：本次 128.12 有 `nefs.client.taihu.zc mount /mnt/v12-2` 挂载（对接 taihu，经 TiKV 做索引），不先停会在重装清空后持续向 TiKV 写入/报错。执行重装前先 `pkill` 客户端并 `umount -l` 挂载点，验证 `pgrep nefs.client` 为空。
3. **验证 parse 别依赖 python3**：`curl /pd/api/v1/stores | python3 -c ...` 在节点上可能无输出（python3 环境/依赖），改用 `curl -s ... | grep -oE '"state_name": "[A-Za-z]+"'` 直接怼正则过滤即可在线确认 store Up。
4. **PD leader 不用强求在某节点**：leader 由 Raft 选出，`/pd/api/v1/leader` 返回的 `name`（如 pd2）应为三成员之一即可，不必固定。
5. **数据目录验证**：删除前先 `ls -la <data>/` 确认确实存在 `pd/`、`tikv/` 子目录；`pd` 目录含 `dashboard.sqlite.db` + `hot-region`，`tikv` 目录含 `db/` + `import/`。删除后目录只剩挂载点空壳。

## 注意

- **整集群重装即清空全部数据**：会丢失 PD/TiKV 存储的所有数据（含 taihu 元数据、EFS meta）。如需保留某层，先备份或先停 taihu-server/客户端，或改用 `taihu128-tikv-clean`（只清 `/taihu/` 前缀）。
- **先停 taihu-server/客户端再重装**：否则运行中的实例/客户端会在重装期间向 TiKV 写入/报错（见 `taihu128-tikv-clean` 前置）。
- 步骤 1 用 `docker stop`（保容器），步骤 3 直接 `docker start` 最简单；仅当容器被删除或需改参数才走 `docker run`。
- 数据目录删除前务必确认路径正确（11/12 是 `/var/lib/jenkins/tikv3`，13 是 `/mnt/nvme0n1/tikv3`），误删会落系统盘。
- 128.12 根分区 99% 满：镜像/日志避免写 `/` 分区；数据盘在已挂载的 jenkins/nvme0n1 挂载下。

## 相关 Skill

- `taihu128-tikv-clean`：只清 taihu `/taihu/` 前缀元数据（轻量重置，无需重建容器）
- `nefs-taihu-deploy`：重装后对新集群建 taihu 资源（pool/target/vol/挂载）
- `mgmt-deploy`：mgmt 控制面（与 TiKV 独立，不受本次重装影响，但若 meta 存 TiKV 需确认）