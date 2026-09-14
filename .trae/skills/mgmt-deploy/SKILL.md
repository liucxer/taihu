---
name: "mgmt-deploy"
description: "在 aarch64 节点用容器部署 NEFS mgmt 三节点 Raft 集群：源码编译→打包镜像→分发→启动→验证。当要在 100.71.128.11/12/13 等节点部署或重建 mgmt 集群时使用。"
---

# mgmt 三节点容器集群部署

基于 2026-09-11 在 100.71.128.11/12/13 的实操提炼。目标：把当前源码 `d:\workspace\mgmt`（dev_nefs_1.6.0_pc）编译成 mgmt 二进制，打包容器镜像，分发到三节点以 host 网络各起一个容器，组成 1 Leader + 2 Follower 的 Raft 集群。

## 依赖与背景

- 节点：`100.71.128.11/12/13`，均为 **aarch64/arm64**，Docker **18.09**（无 compose v2，需 `docker run`）。
- 远程操作通道：**nefs-proxy**（节点 9527 端口）。见 `.trae/skills/nefs-proxy/SKILL.md`。
- 本机为 Windows，无跨平台 cgo，**必须上传源码到节点在 Linux/arm64 原生编译**（mgmt 依赖 librocksdb cgo）。
- 首次调用先确认 9527 连通、节点 Docker 正常。

## 关键经验（易踩坑）

1. **cgo 编译必须在 arm64 Linux 节点做**，本机 Windows 不可行。推荐用 128.12（有 go 1.24 + rocksdb-devel + base 镜像）。
2. **源码 tar 必须含 `docs/` 目录**（`main.go` 空 import 了 `gitlab.cmss.com/SDS/mgmt/docs`）。漏掉会 `go mod tidy` 报 `no matching versions for query "latest"`。
3. **base 镜像**：`registry.paas/nefs/nefs_base:v1.0.1`（arm64）已含全部动态库（rocksdb/snappy/gflags 等），直接 COPY 二进制即可运行，容器内 `ldd` 校验。
4. **host 网络 + 卷挂载**：程序硬编码读 `/etc/mgmt.conf`；用 `-v <data>/mgmt.conf:/etc/mgmt.conf` 挂载覆盖，数据/日志目录也用宿主卷。
5. **端口**：管理端口用 **12379**（避开 80/8000 等已占用的高权限端口；<1024 需特权）。心跳默认 5901、副本默认 5902（`listen` ≤1024 时才重置）。peers 第三段是 **HTTP 管理端口**（存入 AddrDatabase 作 Leader 代理地址），与心跳/副本端口相互独立。
6. **节点残留数据**：若节点已有旧 mgmt 数据（Raft/RocksDB），换独立数据目录（如 `/export/Data/mgmt-deploy`）避免污染；不要在旧目录上直接起新集群。
7. **128.12 曾有裸机 systemd mgmt（nefs-test）**：占用 5901/5902。必须 `systemctl disable mgmt && systemctl stop mgmt && pkill -f "nefs.mgmt master"` 释放端口，否则容器起不来。它由 systemd 拉起，单 kill 会被复活。
8. **`isOnline:false`** 是存储节点（DN/数据节点）心跳字段，刚部署无 DN 接入时为 false，属正常，不是 mgmt 控制面故障。
9. 长编译/传输用 `setsid nohup ... > log &` 后台 + 轮询日志，勿一次长 exec 硬等。

## skill 自带文件

| 文件 | 用途 |
|---|---|
| `conf/mgmt.conf.tpl` | mgmt.conf 模板，含 `__变量__` 占位（NODE_IP/NODE_ID/PEERS/DATA_DIR/LOG_DIR/CLUSTER_NAME等），由脚本 sed 填充 |
| `scripts/deploy_node.sh` | **单节点**部署脚本：生成配置→准备目录→启动容器。用于单个节点，参数化 id/ip/peers |
| `scripts/deploy_cluster.ps1` | **一键三节点**编排（本机 PowerShell 调 proxy）：自动上传脚本/模板到三节点并执行、等待选主、汇总 liveness/cluster-info |

`deploy_cluster.ps1` 用法：
```powershell
powershell -ExecutionPolicy Bypass -File .trae\skills\mgmt-deploy\scripts\deploy_cluster.ps1 -Image nefs-mgmt:1.6.0-deploy
# -Listen 12379 -ClusterName nefs-mgmt-deploy -Force(跳过确认)
```
前提与 skill 主流程一致：镜像已 save/load 到三节点、复合 mgmt.config 相关端口空闲。

## 远程执行方式（PowerShell）

proxy 通道实测用 `Invoke-RestMethod + X-Token` 最稳；`curl.exe` 引号会被 PowerShell 干扰，`proxy_client.py` 与 128.12 有兼容 bug，均不推荐。

```powershell
$ip = "100.71.128.12"
$body = '{"cmd":"<shell 命令>"}'
(Invoke-RestMethod -Uri "http://${ip}:9527/exec" -Method POST -Headers @{"X-Token"="95279527"} -ContentType "application/json" -Body $body).stdout
```

上传文件：
```powershell
curl.exe -s -m 300 -X PUT -H "X-Token: 95279527" --data-binary "@<local>" "http://${ip}:9527/upload?path=<remote>"
```

## 部署步骤

### 0. 前置确认
```powershell
# 三节点 docker 就绪 + 端口空闲
foreach($k in 11,12,13){ $ip=@{11="100.71.128.11";12="100.71.128.12";13="100.71.128.13"}[$k];
  $b='{"cmd":"docker info | grep -E \"Server Version|Storage\"; uname -m"}'
  "$k -> " + (Invoke-RestMethod -Uri "http://${ip}:9527/exec" -Method POST -Headers @{"X-Token"="95279527"} -ContentType "application/json" -Body $b).stdout }
```
确认各节点 12379/5901/5902 空闲（若 12 有裸机 mgmt 先按步骤 4 停掉）。

### 1. 打包源码上传编译

```powershell
# 本地打包（含 docs，排除大目录）
cd d:\workspace\mgmt
tar --exclude=.git --exclude=client --exclude=docs --exclude=build/bin --exclude=api_test --exclude=.trae -czf <tmp>/mgmt_src.tar.gz .
tar --exclude=.git -czf <tmp>/mgmt_docs.tar.gz docs   # docs 单独补传
```
上传两个 tar 到 128.12 的 `/tmp/`，解压到 `/tmp/mgmt_build`（docs 解压到 `/tmp/mgmt_build` 根下）。

```powershell
$b='{"cmd":"cd /tmp/mgmt_build && : > build.log && go mod tidy >> build.log 2>&1 && make bin >> build.log 2>&1 && echo BUILD_DONE >> build.log","timeout":60}'
# exec 后台启动后轮询：tail build.log 直到出现 BUILD_DONE
```
产出：`/tmp/mgmt_build/build/bin/nefs.mgmt`（约 26MB）。校验：
```bash
file build/bin/nefs.mgmt        # 应为 ARM aarch64 ELF
./build/bin/nefs.mgmt version    # 应为 1.6.0
```

### 2. 打包容器镜像（在 128.12）

Dockerfile 放置于 `/tmp/imgtmp/`，`ADD nefs.mgmt /usr/sbin/nefs.mgmt`：
```
FROM registry.paas/nefs/nefs_base:v1.0.1
ADD nefs.mgmt /usr/sbin/nefs.mgmt
RUN chmod +x /usr/sbin/nefs.mgmt
WORKDIR /app
EXPOSE 12379 5901 5902
ENTRYPOINT ["/usr/sbin/nefs.mgmt"]
CMD ["master"]
```
```powershell
$b='{"cmd":"cd /tmp/imgtmp && docker build -t nefs-mgmt:1.6.0-deploy . >> /tmp/img_build.log 2>&1 && echo IMAGE_BUILD_DONE >> /tmp/img_build.log","timeout":60}'
```
注意：`cp nefs.mgmt` 到 `/tmp/imgtmp/` 再 build（build context）。产镜像约 583MB。

### 3. 分发镜像到 11/13
```powershell
# 128.12 save 到 tar
$b='{"cmd":"docker save nefs-mgmt:1.6.0-deploy -o /tmp/mgmt.tar && echo SAVE_DONE"}'
# download 到本地，再 upload 到 11/13（经 Mac 中转，约几百 MB/节点）
foreach($k in 11,13){ $ip=@{11="100.71.128.11";13="100.71.128.13"}[$k]
  curl.exe -s -m 590 -X PUT -H "X-Token: 95279527" --data-binary "@<local_mgmt.tar>" "http://${ip}:9527/upload?path=/tmp/mgmt.tar" }
# 各节点 docker load
$b='{"cmd":"docker load -i /tmp/mgmt.tar >> /tmp/load.log 2>&1 && echo LOAD_DONE >> /tmp/load.log"}'
```

### 4.（如 12 有裸机 mgmt）停掉裸机实例
```powershell
$b='{"cmd":"systemctl disable mgmt; systemctl stop mgmt; pkill -f \"nefs.mgmt master\"; systemctl is-active mgmt; ss -ltn | grep -E \":5901 |:5902 \" || echo RAFT_PORTS_FREE"}'
```

### 5. 配置三节点
每个节点 `/export/Data/mgmt-deploy/mgmt.conf`（挂载进容器覆盖 `/etc/mgmt.conf`）。注意 `ip/listen/peers/id` 各不相同，其余相同：
```json
{
  "role": "master",
  "ip": "100.71.128.11",
  "listen": "12379",
  "id": "1",
  "peers": "1:100.71.128.11:12379,2:100.71.128.12:12379,3:100.71.128.13:12379",
  "heartbeatPort": "5901",
  "replicaPort": "5902",
  "logDir": "/export/Logs/mgmt-deploy",
  "logLevel": "info",
  "walDir": "/export/Data/mgmt-deploy/raft",
  "storeDir": "/export/Data/mgmt-deploy/rocksdbstore",
  "clusterName": "nefs-mgmt-deploy"
}
```
节点对照：11→id=1，12→id=2，13→id=3。

### 6. 启动容器（host 网络 + 数据卷）
```powershell
$b='{"cmd":"mkdir -p /export/Data/mgmt-deploy /export/Logs/mgmt-deploy && cp /etc/mgmt.conf /export/Data/mgmt-deploy/mgmt.conf && docker rm -f nefs-mgmt 2>/dev/null; docker run -d --name nefs-mgmt --network host --restart unless-stopped -v /export/Data/mgmt-deploy:/export/Data/mgmt-deploy -v /export/Logs/mgmt-deploy:/export/Logs/mgmt-deploy -v /export/Data/mgmt-deploy/mgmt.conf:/etc/mgmt.conf nefs-mgmt:1.6.0-deploy master"}'
```
> 若 12 的裸机实例已被停，可直接三节点同时起；若 12 暂时无法停，可先起 11/13 两节点形成 Leader，验证后再起 12 并入。

### 7. 验证
```powershell
foreach($k in 11,12,13){ $ip=... ;
  $b='{"cmd":"curl -s http://127.0.0.1:12379/mgmt/v1/liveness; echo; curl -s http://127.0.0.1:12379/mgmt/v1/mgmt-cluster-info"}'
  "$k -> " + (Invoke-RestMethod ...).stdout }
```
期望（Leader 在 11）：
- 三节点 liveness 均 `{"mgmt":"success"}`
- Leader（11）：`"isRaftLeader":true,"leaderInfo":{"metaReady":true,"address":"100.71.128.11:12379"}`
- 12/13：`"isRaftLeader":false`，`leaderInfo.address` 指向 Leader

写路径验证（经 follower 代理到 Leader）：
```bash
curl -s -d '{"name":"probe","owner":"t","capacity":1048576}' http://<follower>:12379/admin/createVol
# 返回业务校验错误（Invalid target name）即证明 Raft 写链路已打通
```

### 8. 收尾 / 回滚
- 容器 `--restart unless-stopped`，机器重启自动拉起。
- 停止：`docker stop nefs-mgmt`（不会删除数据）；删除：`docker rm -f nefs-mgmt`（保留 `/export/Data` 数据）。
- 换端口/重建：改 conf 反挂载目录，重新 `docker run`。

## 参考
- 配置键解析：`mgrs/config.go`（listen/peers/heartbeatPort/replicaPort/AddrDatabase）
- 集群信息：`routes/route.go` 的 `mgmt-cluster-info` / `liveness` 端点