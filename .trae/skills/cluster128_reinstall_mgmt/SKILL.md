---
name: "cluster128_reinstall_mgmt"
description: "重装 128 集群（100.71.128.11/12/13）三节点 mgmt（nefs-mgmt 容器）Raft 集群：1 停止 mgmt 容器 → 2 删除数据目录（raft/rocksdb/日志） → 3 启动 mgmt 容器。当用户要求在 128 集群重建/重装/重置 mgmt 控制面、清除损坏或过期的 mgmt 元数据、恢复到干净控制面时使用。"
---

# 128 集群 mgmt 重装（三节点）

在三节点 100.71.128.11/12/13 上，通过「停止 mgmt 容器 → 删除数据目录 → 启动 mgmt 容器」三步把整集群 mgmt 控制面（Raft+ RocksDB）重装成全新空集群。

> 本 skill 覆盖**整集群**（nefs-mgmt 三容器全重建），与单节点重装相比能保证 Raft 成员与 RocksDB 元数据一致。若只重装单个节点，需注意 Raft 多数派仍在线。

## 拓扑与部署基线（实测确认，2026-09-13）

| 节点 | mgmt 容器 | 监听端口 | raft/replica 端口 | 数据目录（宿主 `/export/Data/mgmt-deploy`） |
|------|-----------|-----------|------------------|---------------------------------------------|
| 128.11 | `nefs-mgmt` | 12379 | 5901/5902 | `/export/Data/mgmt-deploy` |
| 128.12 | `nefs-mgmt` | 12379 | 5901/5902 | `/export/Data/mgmt-deploy` |
| 128.13 | `nefs-mgmt` | 12379 | 5901/5902 | `/export/Data/mgmt-deploy` |

- 镜像：`nefs-mgmt:1.6.0-deploy`，host 网络，`--restart unless-stopped`。
- Entrypoint/Cmd：`/usr/sbin/nefs.mgmt master`。
- 挂载（bind，三节点一致）：
  - `/export/Data/mgmt-deploy/mgmt.conf` → `/etc/mgmt.conf`
  - `/export/Data/mgmt-deploy` → `/export/Data/mgmt-deploy`（含 raft WAL + rocksdbstore）
  - `/export/Logs/mgmt-deploy` → `/export/Logs/mgmt-deploy`（日志）
- Raft 集群：`peers=1:100.71.128.11:12379,2:100.71.128.12:12379,3:100.71.128.13:12379`；RocksDB `storeDir` + raft `walDir` 均在 `/export/Data/mgmt-deploy`。
- 访问通道：nefs-proxy（节点 9527，token `95279527`）。

## 三个步骤

> skill 用 `docker stop`/`docker start`（保留容器）即可完成重装；容器曾被删除或需保持一致才走 `docker rm -f` + `docker run`。数据目录删除前务必先停容器。

### 步骤 1：停止 mgmt 容器

三节点都可停（确认无读写依赖后）。每节点执行 `docker stop nefs-mgmt`：

```bash
docker stop nefs-mgmt
```

> 前置：三节点当前都跑着 `nefs.mgmt master`。若三节点全部停掉，Raft 集群会失去多数派；重装是整集群操作，先全部停止再统一清理/启动最稳。是否存在依赖 mgmt 的挂载客户端，重装前确认并先停（mgmt 存活时其 Raft 状态会重写 raft WAL）。

验证已停：
```bash
docker ps --format '{{.Names}} {{.Status}}' | grep nefs-mgmt   # 应无行
pgrep -af nefs.mgmt | grep -v grep                             # 应无进程
```

### 步骤 2：删除 mgmt 的数据目录

删除各节点 `/export/Data/mgmt-deploy` 下的 **raft WAL（`raft`）**、**RocksDB store（`rocksdbstore`）**与可能的节点数据子目录；日志目录按需清空：

```bash
# 每节点（128.11/12/13 一致）
rm -rf /export/Data/mgmt-deploy/raft 		# raft WAL
rm -rf /export/Data/mgmt-deploy/rocksdbstore # RocksDB 控制面元数据
# 可选：清空日志（保留目录结构）
rm -rf /export/Logs/mgmt-deploy/*
```

> 注意**不要删 `/export/Data/mgmt-deploy/mgmt.conf`**（容器 `/etc/mgmt.conf` 的配置源），否则启动后无配置。只删 raft 与 rocksdbstore 数据子目录即可清空控制面元数据。

验证目录已清空（应无 raft/、rocksdbstore/，mgmt.conf 仍在）：
```bash
ls -la /export/Data/mgmt-deploy/
```

### 步骤 3：启动 mgmt 容器

若容器仍存在（只 stop 未 rm），三节点分别 `docker start`：

```bash
# 三节点各自执行本机容器
docker start nefs-mgmt
```

若容器被删除或需重建，按原始参数 `docker run`（host 网络 + bind `/export`）：

```bash
# 128.12 示例；三节点挂载路径一致，仅 mgmt.conf 内 IP/id/peers 不同
docker run -d --name nefs-mgmt --network host --restart unless-stopped \
  -v /export/Data/mgmt-deploy:/export/Data/mgmt-deploy \
  -v /export/Logs/mgmt-deploy:/export/Logs/mgmt-deploy \
  -v /export/Data/mgmt-deploy/mgmt.conf:/etc/mgmt.conf \
  nefs-mgmt:1.6.0-deploy master
```

mgmt 会用自带的 mgmt.conf（peers 指向三节点 12379）自动重新组成 Raft 集群。

## 验证（整集群健康）

```bash
# 进程存活（每节点）
pgrep -af nefs.mgmt
# 端口监听（每节点，host 网络）
ss -ltnp | grep -E ':12379|:5901|:5902'
# Raft 集群 / 卷可用：经任意节点用 nefs.tools 或 HTTP 查 volume
ps aux | grep -E 'nefs-mgmt' | grep -v grep
```

预期：三节点 nefs-mgmt 进程运行，12379 端口监听，Raft 集群重新组齐三成员后，可正常建 pool/target/vol。

## 经验教训（实测 2026-09-13）

本次重装顺利，坑主要集中在**用 nefs.tools 验证集群健康时的参数**，记录如下复用：

1. **`--host` 不是合法选项**：`nefs.tools vol list --host <addr>` 直接 FATAL `unknown option: --host`。正确用 `--master <mgmt 地址列表>` 指定 mgmt 集群。
2. **`vol list` 无数据显示 0 退出但零输出**：刚重装完元数据空，`vol list --master ...` 会正常退出（rc=0）但无任何打印，容易误判为失败/超时。**改用 `nefs.tools pool list` / `nefs.tools target list` 验证**——同样空列表，但退出码 + 无报错即证明 Raft 集群已组齐并能正常响应控制面请求。
3. **mgmt HTTP 端点不是 `/mgmt/v1/volume/list`**：`curl -X POST http://<node>:12379/mgmt/v1/volume/list` 返回 `404 page not found`。不要用猜的 URL，直接以容器内现成工具 `/usr/sbin/nefs.tools pool list --master 100.71.128.11:12379,100.71.128.12:12379,100.71.128.13:12379` 为准。
4. **每个节点只能 docker 操作本机容器**：`docker start pd1 pd2 pd3` 这类跨节点一次拉起的命令会报 `No such container`（容器只存在于各自节点）。mgmt 的 `docker start nefs-mgmt` 也必须三节点分别执行。
5. **验证要分层判断**：进程在（`pgrep nefs.mgmt`）+ 端口 LISTEN（`ss -ltnp | grep -E ':12379|:5901|:5902'`）是"进程起来了"；pool/target list 空且 rc=0 才是"Raft 集群组齐可用"。两者都要查才算重装成功。
6. **日志目录在 `/export/Logs/mgmt-deploy/master/`**：mgmt 运行日志在 `master/` 子目录（`master_info.log` 等），不在 `/export/Logs/mgmt-deploy/` 根下。验证 admin 请求路径时可 `tail` 该文件确认元数据被读写。

## 注意

- **整集群重装即清空 mgmt 控制面全部元数据**（卷/存储池/target/QoS/配额/快照/鉴权等），保留与否需先备份 `/export/Data/mgmt-deploy/rocksdbstore`。
- **`metaPeers` 指向 128 TiKV（2379）**：mgmt 通过 TiKV 作为 meta 后端。若 TiKV 也刚重装（`cluster128_reinstall_tikv`），两者都已是空状态，重装 mgmt 后从零建卷即可。
- **先停依赖客户端/服务再重装**：运行中的 nefs.client 挂载会向 mgmt 写入/轮询，见相关 bottom skill。
- 数据目录删除前务必确认路径（都在 `/export`，非根分区）；`mgmt.conf` 是配置源不可删。
- 日志清理只针对 `/export/Logs/mgmt-deploy/*`，不影响数据目录。

## 相关 Skill

- `cluster128_reinstall_tikv`：重装 128 集群 TiKV（mgmt 的 `metaPeers` 后端）
- `mgmt-deploy`：mgmt 三节点 Raft 部署（编译→镜像→分发→启动→验证）全流程
- `nefs-taihu-deploy`：mgmt 就绪后建 taihu 资源（pool/target/vol/挂载）
- `taihu128-tikv-clean`：只清 taihu `/taihu/` 前缀元数据（轻量重置，与 mgmt 独立）