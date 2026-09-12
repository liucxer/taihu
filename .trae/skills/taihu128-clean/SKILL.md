---
name: "taihu128-clean"
description: "清理 128 集群（100.71.128.11/12/13 共 3 节点）的 taihu 部署：优雅停机全部 taihu-server、可选删除 db 数据目录与 /tmp 测试产物。当用户要求清理/移除/回收 128 集群 taihu 测试部署、停掉实例、删除测试数据时使用。安全护栏保证不触碰 TiKV PD、网络、磁盘分区表。"
---

# 清理 128 集群 taihu 部署

一键清理 100.71.128.11/12/13（3 节点）上的 taihu 测试部署。每节点包含 3 个实例
（`n{node}-nvme{1,2,3}`，如 11→n11-nvme1/2/3），单地址 `-listen <本机IP>`，数据目录
`/tmp/taihu-db/nvme{1,2,3}`、裸盘 `nvme{1,2,3}n1`、注册到 TiKV PD
`100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379`（明文，无 TLS）。

## 拓扑约定

| 节点 | 实例名 | 监听地址 | db 目录 | 设备 |
|------|--------|---------|---------|------|
| 100.71.128.11 | n11-nvme1/2/3 | 100.71.128.11 | /tmp/taihu-db/nvme{1,2,3} | nvme{1,2,3}n1 |
| 100.71.128.12 | n12-nvme1/2/3 | 100.71.128.12 | /tmp/taihu-db/nvme{1,2,3} | nvme{1,2,3}n1 |
| 100.71.128.13 | n13-nvme1/2/3 | 100.71.128.13 | /tmp/taihu-db/nvme{1,2,3} | nvme{1,2,3}n1 |

## 触发条件

- 用户要求清理 / 移除 / 回收 128 集群的 taihu 部署
- 用户要求停掉全部实例、删除测试数据、清理之前测试遗留
- 新一轮测试前需要干净环境

## 使用流程

### 1. 准备节点脚本

脚本 `clean128.sh` 与 `clean_all128.sh` 位于本 skill 目录。用 nefs-proxy 上传 `clean128.sh` 到全部 3 节点：

```bash
proxy_client.py --node 100.71.128.11 upload --local clean128.sh --remote /tmp/clean128.sh
# 重复 100.71.128.12, 100.71.128.13
```

### 2. 并发执行（每节点独立清理，互不依赖）

对 3 节点并发执行。`--full` 才删除 db 数据目录与 /tmp 产物；不带 `--full` 仅停进程（安全默认）。`--dry-run` 只打印要删/要停项不实删。

示例（PowerShell，由本机代理并发 3 节点）：

```powershell
$PY="C:\Users\USER484887\AppData\Roaming\uv\python\cpython-3.12.14-windows-x86_64-none\python.exe"
$P="d:\workspace\taihu\.trae\skills\nefs-proxy\proxy_client.py"
foreach($n in "100.71.128.11","100.71.128.12","100.71.128.13"){
  & $PY $P --node $n exec --cmd "bash /tmp/clean128.sh $n --full --force"
}
```

也可用 `clean_all128.sh` 编排（bash，自动分发+并发，变量 PROXY_PY 指定 proxy_client.py 绝对路径）。

> **注意**：上传脚本请用 Write 工具写出文件后通过 proxy upload 推送到节点，再用 `bash script.sh` 执行。不要在远程命令里内联含 `$` 的脚本，避免 PowerShell 转义误展开。脚本若用 PowerShell `Set-Content -Encoding UTF8` 写入会带 BOM 导致首行 `PIDS=... command not found`，务必用 UTF-8 无 BOM 格式。

### 3. 安全护栏（务必遵守）

- **只动** `taihu-server` 进程与 `/tmp/taihu-db/nvme*`、`/tmp` 测试产物。
- **绝不** 触碰 TiKV PD、网络配置、`nvme*` 磁盘分区表（只读数据上的 db 目录文件，不做磁盘级操作）。
- 删除 db 目录前脚本做白名单路径校验（必须匹配 `/tmp/taihu-db/nvme[0-9]`），不匹配即拒绝并退出码 3。
- 默认仅停进程；删 db 需 `--full`，且 `--force` 跳过二次确认。不确定时先 `--dry-run`。

### 4. 校验

执行后每节点会输出 `taihu-server procs left: N`（应为 0）、残留 `taihu-db` 目录、磁盘剩余。
核对全部 3 节点 N=0；TiKV 注册项会因进程优雅退出时注销 + 心跳过期而自动 stale。

## 常见问题 / 教训复用

- proxy 的 exec 环境 PATH 极简，脚本需显式 `export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH`。
- 远程命令含 `$` 时，备份/清理脚本必须上传为文件执行，禁止在命令行内联，避免 PowerShell 转义误展开。
- 脚本文件传输用 Write + upload（UTF-8 无 BOM），禁止 `Set-Content` 写 BOM（BOM 会让 bash 首行命令报 `command not found`）。
- 进程匹配按 `comm == taihu-server`（bin 名）为准；强杀兜底用 `kill -9`。
- 128 集群 TiKV 是明文（无 TLS），PD 为 `100.71.128.{11,12,13}:2379`。

## 相关 Skill

- `taihu128-tikv-clean`：清空 128 集群 taihu 在 TiKV 的元数据（清理实例后如需重置元数据）
- `编译部署skill`：编译部署 taihu 到 128.12
- `性能测试skill`：128.12 上读写性能测试