---
name: "taihu146-deploy"
description: "在 146 集群（10.151.26.146-150,152 共 6 节点）部署 taihu：每节点 3 实例（TAIHU-0..17），每实例同时监听 bond1+bond2，注册 TiKV。当用户要求在 146 集群部署/拉起/重建 taihu 实例、复用已有 rocksdb 路径与 nvme 设备时使用；与 taihu146-clean（清理）、taihu146-perf（性能测试）配套。"
---

# 146 集群 taihu 部署

在 10.151.26.146-150,152（6 节点）部署 18 实例 taihu 集群（每节点 3 实例）。

## 拓扑约定（固定）

| 节点 | 实例 | bond1.2175 | bond2.2372 | 设备 |
|------|------|-----------|-----------|------|
| 146 | TAIHU-0/1/2 | 10.153.28.202 | 10.155.17.202 | nvme3/4/5n1 |
| 147 | TAIHU-3/4/5 | 10.153.28.203 | 10.155.17.203 | nvme3/4/5n1 |
| 148 | TAIHU-6/7/8 | 10.153.28.204 | 10.155.17.204 | nvme3/4/5n1 |
| 149 | TAIHU-9/10/11 | 10.153.28.205 | 10.155.17.205 | nvme3/4/5n1 |
| 150 | TAIHU-12/13/14 | 10.153.28.206 | 10.155.17.206 | nvme3/4/5n1 |
| 152 | TAIHU-15/16/17 | 10.153.28.208 | 10.155.17.208 | nvme3/4/5n1 |

- 每实例 `-listen <bond1>,<bond2>`（同端口双地址监听，客户端多 IP round-robin 均分）
- db 路径 `/mnt/nvme2/taihu2-<name>-dual/db`
- TiKV PD（146 集群自带，TLS）：`10.153.28.202:12379,10.153.28.203:12379,10.153.28.204:12379`
- TLS 证书（6 节点统一路径）：`/nefsdata/meta/tikv-deploy/pd-12379/tls/{ca.crt,pd.crt,pd.pem}`
- 端口自动分配（50000/50002/50004 等），以 `taihu-cli cluster list` 为准

## 部署流程

### 1. 本地编译（当前 HEAD，linux/arm64）

```powershell
$env:GOOS="linux"; $env:GOARCH="arm64"; $env:CGO_ENABLED="0"
go build -o dist/build/taihu-server.<commit> ./cmd/taihu-server
go build -o dist/build/taihu-cli.<commit> ./cmd/taihu-cli
```

`<commit>` = `git rev-parse --short HEAD`。

### 2. 逐节点查询 bond IP（防止 IP 漂移）

上传 `get_bond_ips.sh` 到各节点执行，拿到 `BOND1=.. BOND2=..`。

### 3. 上传 + 部署

上传 `taihu-server.<commit>`、`taihu-cli.<commit>`（重命名 /tmp/taihu-cli）、`deploy_dualip146.sh` 到各节点，然后每节点执行：

```bash
bash /tmp/deploy_dualip146.sh <BOND1_IP> <BOND2_IP> <BASE_TAIHU_N> [BIN]
# 例: bash /tmp/deploy_dualip146.sh 10.153.28.202 10.155.17.202 TAIHU-0 /tmp/taihu-server.e536f07
```

脚本语义：停旧进程（优雅→强杀，轮询至 0）→ chmod → 起 3 实例（setsid nohup）→ 校验进程与双 IP 端口。

### 4. 验证

```bash
TLS="--tikv-ca /nefsdata/meta/tikv-deploy/pd-12379/tls/ca.crt --tikv-cert /nefsdata/meta/tikv-deploy/pd-12379/tls/pd.crt --tikv-key /nefsdata/meta/tikv-deploy/pd-12379/tls/pd.pem"
PD="10.153.28.202:12379,10.153.28.203:12379,10.153.28.204:12379"
/tmp/taihu-cli cluster list --pd $PD $TLS
# 期望 TAIHU-0..17 全部 online，心跳 ~1s
```

探针读写（**get 必须 --file 输出再 md5sum**，stdout 模式会混 JSON 状态行导致校验假失败）：

```bash
head -c 4194304 /dev/urandom > /tmp/probe.obj
/tmp/taihu-cli key put --pd $PD $TLS --key probe/deploy-check --file /tmp/probe.obj
/tmp/taihu-cli key get --pd $PD $TLS --key probe/deploy-check --file /tmp/probe.out
md5sum /tmp/probe.out /tmp/probe.obj   # 必须一致
```

## 复用旧部署（rocksdb + nvme）

- 同路径同设备直接重启即可：`deploy_dualip146.sh` 复用 `/mnt/nvme2/taihu2-<name>-dual/db` 与 nvme3/4/5n1。
- 若 db 目录被删（如 taihu146-clean --full 后），metastore 会新建空库，**写游标从头开始**，盘内旧数据随写入逐步覆盖——设备可安全复用，但旧 key 不可恢复。
- 盘上无 superblock，设备内容完全由 rocksdb 元数据解释；元数据丢失 = 数据丢失。

## 教训复用（务必遵守）

- proxy 的 exec 环境 PATH 极简：脚本内必须 `export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH`。
- 远程命令含 `$` 时**必须上传脚本文件执行**，禁止命令行内联（PowerShell 转义会吃掉 `$`）。
- 上传的脚本用 `bash script.sh` 调用，无需 chmod；二进制需 `chmod +x`。
- 节点时钟必须同步（曾出现 14 分钟偏差导致心跳过期、实例被剔除）。

## 相关 Skill

- `taihu146-clean`：清理本部署（停进程/删 db/清 /tmp）
- `taihu146-perf`：部署后做端到端读写性能测试
- `clean-taihu-tikv`：需要重置 TiKV 元数据时使用