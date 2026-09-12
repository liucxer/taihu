---
name: "taihu128-deploy"
description: "在 128 集群（100.71.128.11/12/13 共 3 节点）部署 taihu：每节点 3 实例（n{node}-nvme{1,2,3}），单地址监听本机 IP，注册 128 TiKV（明文无 TLS）。当用户要求在 128 集群部署/拉起/重建 taihu 实例、使用裸盘 nvme 与持久 db 路径时使用；与 taihu128-clean（清理）、taihu128-tikv-clean（TiKV 元数据重置）配套。"
---

# 128 集群 taihu 部署

在 100.71.128.11/12/13（3 节点）部署 9 实例 taihu 集群（每节点 3 实例 × 3 盘）。

## 拓扑约定（固定）

| 节点 | 实例名 | 监听地址（-listen） | 数据盘（-dev） | db 目录（-db，持久盘） |
|------|--------|--------------------|---------------|------------------------|
| 100.71.128.11 | n11-nvme1/2/3 | 100.71.128.11 | /dev/nvme{1,2,3}n1 | /var/taihu-db/nvme{1,2,3} |
| 100.71.128.12 | n12-nvme1/2/3 | 100.71.128.12 | /dev/nvme{1,2,3}n1 | /var/taihu-db/nvme{1,2,3} |
| 100.71.128.13 | n13-nvme1/2/3 | 100.71.128.13 | /dev/nvme{1,2,3}n1 | /var/taihu-db/nvme{1,2,3} |

- TiKV PD（128 集群自带，**明文无 TLS**）：`100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379`
- 端口自动分配（50000-51000），以 `taihu-cli cluster list` 实际为准
- **db 目录必须落持久盘**（用户明确要求不用 `/tmp` tmpfs，重启不丢元数据）
- 数据盘（-dev）是整块裸盘，由 taihu 以 O_DIRECT 直写，跨实例不共用

## 触发条件

- 用户要求在 128 集群部署/拉起/重建 taihu 实例
- 不使用 tmp 空间、db 要落持久盘
- 新一轮测试前需要干净的持久化部署

## 部署流程

### 1. 本地编译（当前 HEAD，linux/arm64）

```bash
cd d:\workspace\taihu
$env:GOOS="linux"; $env:GOARCH="arm64"; $env:CGO_ENABLED="0"
$TS=Get-Date -Format "yyyyMMddHHmm"
go build -trimpath -ldflags "-X github.com/liucxer/taihu/internal/version.Commit=<SHA> -X github.com/liucxer/taihu/internal/version.BuildTime=$TS" -o dist/build/taihu-server.<SHA> ./cmd/taihu-server
go build -trimpath -ldflags "...同..." -o dist/build/taihu-cli.<SHA> ./cmd/taihu-cli
```

`<SHA>` = `git rev-parse --short HEAD`。

### 2. 探测节点磁盘（可选但建议）

确认 nvme{1,2,3}n1 裸盘存在且未作为 db 目录冲突。注意 128.12/13 的 nvme{1,2,3}n1 是裸盘，128.11 的已挂载——但本 skill 一律以裸盘作 -dev，db 单独落 /var。

```bash
for d in 1 2 3; do ls -la /dev/nvme${d}n1 2>&1; done
```

### 3. 上传部署脚本 + 执行

脚本 `deploy3x3_128.sh` 位于本 skill 目录。用 nefs-proxy 上传二进制与脚本到 3 节点：

```powershell
$PY="C:\Users\USER484887\AppData\Roaming\uv\python\cpython-3.12.14-windows-x86_64-none\python.exe"
$P="d:\workspace\taihu\.trae\skills\nefs-proxy\proxy_client.py"
foreach($n in "100.71.128.11","100.71.128.12","100.71.128.13"){
  & $PY $P --node $n upload --local "d:\workspace\taihu\dist\build\taihu-server.<SHA>" --remote /tmp/taihu-server.<SHA>
  & $PY $P --node $n upload --local "d:\workspace\taihu\.trae\skills\taihu128-deploy\deploy3x3_128.sh" --remote /tmp/deploy3x3_128.sh
}
# 逐节点执行：bash /tmp/deploy3x3_128.sh <NODE_IP> <BASE_NAME>
foreach($n in @{"100.71.128.11"="n11";"100.71.128.12"="n12";"100.71.128.13"="n13"}.GetEnumerator()){
  & $PY $P --node $n.Key exec --cmd "bash /tmp/deploy3x3_128.sh $($n.Key) $($n.Value) /tmp/taihu-server.<SHA>"
}
```

脚本语义：停旧进程（kill→轮询至 0）→ chmod → 起 3 实例（setsid nohup，`-listen <IP>` 单地址）→ 校验进程与端口。

### 4. 验证

**注册检查**（明文 PD，无 TLS 参数）：

```bash
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
/tmp/taihu-cli cluster list --pd $PD
# 期望 n11-nvme1..n13-nvme3 共 9 个 online，心跳 ~0-1s
```

**探针读写**（跨节点 TCP 验证数据面；get 必须 --file 输出再 md5sum，stdout 会混 JSON 状态行）：

```bash
head -c 4194304 /dev/urandom > /tmp/probe128.obj
/tmp/taihu-cli key put --pd $PD --instance n11-nvme1 --key probe/cluster-ae325c9 --file /tmp/probe128.obj
/tmp/taihu-cli key get --pd $PD --instance n11-nvme1 --key probe/cluster-ae325c9 --file /tmp/probe128.out
md5sum /tmp/probe128.out /tmp/probe128.obj   # 必须一致
```

## 复用旧部署（nvme + rocksdb）

- **数据盘（-dev）**：整块裸盘直接复用，盘上旧数据会随写入逐步覆盖；盘无 superblock，内容完全由 db 元数据解释。
- **db 目录（-db）**：若 `/var/taihu-db/nvme{n}` 存在则复用其中 rocksdb；重建后会新建空库、写游标从头开始，旧 key 不可恢复。
- 注意：历史部署 db 曾落在 `/tmp/taihu-db/`（tmpfs），**本次迁移到 `/var/taihu-db/`**——如有历史数据需先确认是否保留。

## 教训复用（务必遵守）

- proxy 的 exec 环境 PATH 极简：脚本内必须 `export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH`。
- 远程命令含 `$` 时**必须上传脚本文件执行**，禁止命令行内联（PowerShell 转义会吃掉 `$`）。
- 上传脚本用 `bash script.sh` 调用，无需 chmod；二进制需 `chmod +x`。
- 脚本文件传输用 Write + upload（UTF-8 无 BOM），禁止 PowerShell `Set-Content -Encoding UTF8` 写 BOM（BOM 会让 bash 首行报 `command not found`）。
- 端口自动分配在 50000-51000 区间，每 listener 独立；多实例同机各占一端口。

## 相关 Skill

- `taihu128-clean`：清理本部署（停进程/删 db/清产物）
- `taihu128-tikv-clean`：清空 128 集群 taihu 在 TiKV 的元数据
- `性能测试skill`：部署后做读写性能测试