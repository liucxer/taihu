---
name: nefs-proxy
version: 1.0.0
description: "通过 proxy.py agent（端口 9527）在已部署节点上执行命令、传输文件、管理端口转发规则。TRIGGER: 在 100.71.7.194 / 100.71.128.12 / 10.151.26.146-150,152 节点上执行命令、上传下载文件、配置端口转发、查看 proxy 规则。"
allowed-tools: Bash, Read, Write
keywords: proxy,nefs,exec,命令执行,文件传输,upload,download,端口转发,9527,100.71.7.194,100.71.128.12,10.151.26
---

# NEFS proxy skill

在已部署 `proxy.py server` 的节点上，通过其 HTTP 接口（默认端口 9527，默认 token `95279527`）执行命令、传输文件、管理端口转发。客户端脚本纯标准库，无需安装依赖。

> 本 skill 已收录到 taihu 工程 `.trae/skills/nefs-proxy/`，是 `taihu-compile-deploy` / `taihu-perf` 的底层远程操作依赖（128.12 经 9527 端口操作）。

## 已部署节点清单

| 节点 | 主机名 | 访问方式 |
|------|--------|----------|
| 100.71.7.194 | SZYFQ-PM-OS01-BCNFS-GFS41 | Mac 直连 |
| 100.71.128.12 | SZYFQ-PM-OS01-BCNFS-XCKP05（跳板） | Mac 直连 |
| 10.151.26.146/147/148/149/150/152 | fhcsy-...-pm-os01-ebs-10/11/12/13/14/16 | Mac 直连 |

> 所有节点统一从本地 Mac 直接访问 `http://<节点>:9527`。某个节点不可达时，单独排查网络/部署问题即可。

## 用法

```bash
PY=scripts/proxy_client.py   # 相对 taihu 工程根目录

# 执行命令
python3 $PY --node 194 exec --cmd "ceph -s" --timeout 60
python3 $PY --node 100.71.128.12 exec --cmd "df -h" --cwd /tmp

# 上传 / 下载 / 列目录 / 建目录 / 删除
python3 $PY --node 146 upload   --local ./a.tar.gz --remote /tmp/a.tar.gz
python3 $PY --node 146 download --remote /tmp/a.tar.gz --local ./a.tar.gz
python3 $PY --node 146 ls --path /tmp
python3 $PY --node 146 mkdir --path /tmp/x
python3 $PY --node 146 delete --path /tmp/x

# 健康检查
python3 $PY --node 194 ping

# proxy 规则管理（在目标节点本身上增/查/删）
python3 $PY --node 100.71.128.12 proxy list
python3 $PY --node 100.71.128.12 proxy add --listen-port 19520 --target-ip 127.0.0.1 --target-port 9527 --name test-fwd
python3 $PY --node 100.71.128.12 proxy delete --name test-fwd
```

节点参数 `--node` 支持别名：`194`/`128`/`12`/`jump`/`146`..`152`，也支持完整 IP。可用 `--token` 覆盖默认 token。

## 底层接口（proxy.py agent，同一端口 9527，除 /ping 外需 X-Token）

- 命令执行：`POST /exec`（body `{"cmd","timeout","cwd"}`），返回 `stdout/stderr/exit_code`；`GET /exec?cmd=...` 同
- 文件传输：`PUT/POST /upload?path=...`、`GET /download?path=...`、`GET /ls?path=...`、`POST /mkdir`、`POST /delete`
- proxy 管理：`GET /proxy`、`POST /proxy/add`、`POST /proxy/delete` / `DELETE /proxy?name=...`
- 健康检查：`GET /ping`

## Agent (proxy.py) 部署与已知修复

节点端 agent 代码已归档在工程 `scripts/proxy.py`（**修复版，2026-09-01**）。部署：
```bash
# 上传 proxy.py 到节点 /tmp/ 后后台启动（root=/tmp）
cd /tmp && setsid nohup python3 /tmp/proxy.py server > /tmp/proxy_server.log 2>&1 &
```

**已修复 bug（v2，来自 128.13 / 194 实测）**：`proxy add` 端口转发的上游连接此前用
`socket.create_connection((...), timeout=30)`，**30s 超时残留**在 upstream socket 上。做长
命令转发（如 /exec 跑几分钟）时，目标 30s 内不发数据 → `pipe()` 里 `recv()` 抛
`socket.timeout` → 连接被拆 → 客户端收 "Remote end closed connection without response"。
修复：`proxy_handle` 建立连接后加 `upstream.settimeout(None)` + `client.settimeout(None)`
+ `set_keepalive()`（TCP keepalive idle=20/interval=10/count=3，防中间设备回收空闲连接）。
验证：修复前 65s 命令必断，修复后 65s 正常返回（elapsed 65005ms）。

**经端口转发做长命令的正确姿势**：转发对长连接仍有风险时，把长任务在目标节点
`setsid nohup bash xxx.sh > /tmp/xxx.log 2>&1 &` **后台跑**（exec 只发启动命令，立即返回），
再轮询日志（短命令），等 `ALL DONE` 标记——不要用一次长 exec 直接等结果。

## 与 taihu 工程的关联（本地实测经验）

- **proxy_client.py 与 128.12 存在 HTTP 兼容 bug**：实测经 `proxy_client.py` 操作 128.12 异常；
  `taihu-compile-deploy` / `taihu-perf` 均改用 **curl + X-Token** 直调（`curl -s -G -H "X-Token: $TOKEN" ...`）。二者择一，若客户端通不过就换 curl。
- 上传正在运行的二进制会报 `{"code":1,"error":"[Errno 26] Text file busy"}`：先杀进程再上传。
- 跨机 Mac→128.12 链路仅 ~2-4MB/s，压测客户端须放 128.12 本机走 loopback。

## 注意事项

- 节点上进程为 `python3 ./proxy.py server`，文件 `/tmp/proxy.py`，root=/tmp。
- 命令支持 shell 管道/重定向；`timeout` 默认 3600s，超时会整组 kill（返回 `timed_out=true`）。
- 文件传输支持大文件（分块/流式）。
- 涉及敏感信息（token、IP）仅限内网测试环境，勿外传。