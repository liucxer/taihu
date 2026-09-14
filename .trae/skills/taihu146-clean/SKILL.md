---
name: "taihu146-clean"
description: "清理 146 集群（10.151.26.146-150,152 共 6 节点）的 taihu 部署：优雅停机全部 taihu-server、可选删除 db 数据目录与 /tmp 测试产物。当用户要求清理/移除/回收 146 集群 taihu 测试部署、停掉实例、删除测试数据时使用；与部署 skill taihu146-perf 配套。安全护栏保证不触碰 TiKV PD、bond 网络、磁盘分区表。"
---

# 清理 146 集群 taihu 部署

一键清理 10.151.26.146-150,152（6 节点）上的 taihu 测试部署。每一个节点包含 3 个实例
（`TAIHU-N`，N 从各节点基准号起逐个+1：146→TAIHU-0、147→TAIHU-3、148→TAIHU-6、
149→TAIHU-9、150→TAIHU-12、152→TAIHU-15），每实例 `-listen <bond1>,<bond2>` 双地址监听，
数据目录 `/mnt/nvme2/taihu2-<name>-dual/db`、裸盘 `nvme{3,4,5}n1`、注册到 TiKV PD
`100.71.128.11-13:2379`。

## 触发条件

- 用户要求清理 / 移除 / 回收 146 集群的 taihu 部署
- 用户要求停掉全部实例、删除测试数据、清理之前测试遗留
- 新一轮测试前需要干净环境

## 使用流程

### 1. 准备节点脚本

脚本 `clean146.sh` 与 `clean_all146.sh` 位于本 skill 目录。用 nefs-proxy 上传 `clean146.sh` 到全部 6 节点：

```bash
proxy_client.py --node 146 upload --local clean146.sh --remote /tmp/clean146.sh
# 重复 147,148,149,150,152
```

### 2. 并发执行（每节点独立清理，互不依赖）

对 6 节点并发执行，节点→基准实例号映射见上。`--full` 才删除 db 数据目录与 /tmp 产物；
不带 `--full` 仅停进程（安全默认）。`--dry-run` 只打印要删/要停项不实删。

示例（PowerShell，由本机代理并发 6 节点）：

```powershell
foreach($n in 146,147,148,149,150,152){ $base = @{146='TAIHU-0';147='TAIHU-3';148='TAIHU-6';149='TAIHU-9';150='TAIHU-12';152='TAIHU-15'}[$n]; proxy_client.py --node $n exec --cmd "bash /tmp/clean146.sh $base --full --force" }
```

也可用 `clean_all146.sh` 编排（bash，自动分发+并发，变量 PROXY_PY 指定 proxy_client.py 绝对路径）。

### 3. 安全护栏（务必遵守）

- **只动** `taihu-server` 进程与 `/mnt/nvme2/taihu2-TAIHU-*-dual/db`、`/tmp` 测试产物。
- **绝不** 触碰 TiKV PD、bond/bridge 网络配置、`nvme*` 磁盘分区表（只读数据上的 db 目录文件，不做磁盘级操作）。
- 删除 db 目录前脚本会做白名单路径校验（必须匹配 `/mnt/nvme2/taihu2-TAIHU-*-dual/db`），不匹配即拒绝并退出码 3。
- 删除前脚本打印将删清单；不确定时先跑 `--dry-run`；`--force` 跳过二次确认。

### 4. 校验

执行后每节点会输出 `taihu-server procs left: N`（应为 0）、残留 `taihu2-*` 目录、磁盘剩余。
核对全部 6 节点 N=0；TiKV 注册项会因进程优雅退出时注销 + 心跳过期而自动 stale。

## 常见问题 / 教训复用

- proxy 的 exec 环境 PATH 极简，脚本需显式 `export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH`。
- 远程命令含 `$`（如 `$((N0+k))`）时，脚本必须上传为文件执行，禁止在命令行内联，避免 PowerShell 反斜杠/转义误展开。
- 上传的脚本无执行权限，用 `bash script.sh` 调用即可（不必 chmod）。
- 进程匹配按 `comm == taihu-server`（bin 名）为准；强杀兜底用 `kill -9`。

## 相关 Skill

- `taihu146-perf`：在 146 集群部署 + 做端到端读写性能测试（清理前请确认测试已结束、数据已回收）。