---
name: nefs-taihu-deploy
description: 部署 EFS_nefs 对接 taihu 集群后端 — TiKV 注册中心 + taihu-server 数据面 + EFS_nefs "taihu" 存储后端：构建（-tags taihu）/配置/mgmt 建 pool+target+vol/挂载读写验证全流程。TRIGGER: 用户要求部署/重建/验证 EFS_nefs 对接 taihu、建 taihu 存储资源、挂载 taihu 卷，或排查 taihu 后端不可用时使用。
---

# EFS_nefs 对接 taihu 集群后端部署

EFS_nefs 以对象存储后端（注册名 `"taihu"`）对接 taihu 纯 Go 分布式集群：
客户端经 TiKV 发现 taihu-server 实例（注册 + 1s 心跳），本地优先写入、索引锚定、读回退索引。

| 角色 | 说明 |
|------|------|
| TiKV 集群 | 注册中心（`/taihu/instances/`、`/taihu/capacity/`）+ 索引（`/taihu/index/{key}`），3 节点 PD+TiKV |
| taihu-server | 数据面实例（netpoll RPC + shm），注册到 TiKV，裸盘存储 |
| EFS_nefs | `pkg/object/taihu`（`//go:build linux && taihu`），消费侧进程 |

## 环境约定

| 项目 | 值 |
|------|-----|
| EFS_nefs 仓库 | `d:\workspace\EFS_nefs`（改动只允许在这一侧） |
| taihu 仓库 | `d:\workspace\taihu`（**只读参考，一行不改**） |
| TiKV 集群 | `100.71.128.11/12/13`（PD 2379/2380，TiKV 20160/20180，host 网络，镜像 `registry.paas/nefs/ukv_pd_arm:v7.5.6` / `ukv_server_arm:v7.5.6`） |
| 测试节点 | 仅 `128.11 / 128.12 / 128.13`（aarch64 BCLinux） |
| 文件分发 | nefs-proxy 端口 `9527`，token `95279527`（curl + `X-Token` 直连） |
| 构建标签 | EFS_nefs 侧必带 `-tags taihu`（且仅 Linux 可编译）；无 tag 时 Windows 构建零影响（stub.go） |
| go 版本 | ≥ 1.25（taihu go.mod 要求；构建机 `GOTOOLCHAIN=auto` 或安装） |

## 部署流程

### 1. TiKV 集群（注册中心 + 索引存储）

已有 3 节点集群则跳过，直接验证。重建/首次部署见 taihu 仓库
`doc/20260911_TiKV三节点docker部署记录_128.11-12-13.md`，要点：

- 每节点 1 个 PD + 1 个 TiKV 容器，`--network host`、`--restart unless-stopped`，PD 用 `--initial-cluster` 三成员一次性初始化
- 内网 registry 不可达：`docker save + gzip` 导出 → 节点间 9527 `curl -H "X-Token: 95279527" http://<node>:9527/download?path=...` → `docker load` 分发
- 数据目录落在已有挂载盘（128.11/12: `/var/lib/jenkins/tikv3`，128.13: `/mnt/nvme0n1/tikv3`），不碰裸盘
- 128.12 根分区 99% 满，镜像/日志避免写 `/` 分区

验证：

```bash
curl -s http://100.71.128.11:2379/pd/api/v1/health      # 3 成员全 true
curl -s http://100.71.128.11:2379/pd/api/v1/stores      # 3 store 全 Up
```

### 2. 编译并部署 taihu 服务端（数据面实例）

> **新版命令框架（2026-09-13 起）**：原独立二进制 `taihu-server` / `taihu-cli` / `taihu-bench` 已合并为**统一 `taihu` 单二进制**（cobra 子命令树，`cmd/taihu`）。启动服务端子命令 `taihu server`，运维子命令 `taihu cluster`。**所有 flag 均为双横线（`--listen`，非 `-listen`）**，PD 为根命令全局参数 `--pd`。

#### 2a. 编译分发（taihu 侧流程，参考 taihu 仓库 `taihu-compile-deploy` skill）

```bash
cd d:/workspace/taihu
git add <交付源码> && git commit -m "<msg>"            # 只 commit 不 push
SHA=$(git rev-parse --short HEAD)
PKG=dist/taihu.$SHA.tar.gz
git ls-files -z | tar --null -czf "$PKG" -T -           # 仅打包受版本控制源码

# 上传 + 远端解压编译（128.11/12/13 任一；产物名带 commitid，如 /tmp/taihu.1d40665）
curl -s --max-time 120 -X PUT -T "$PKG" -H "X-Token: 95279527" \
  "http://100.71.128.13:9527/upload?path=/tmp/$(basename "$PKG")"
curl -s -G -H "X-Token: 95279527" "http://100.71.128.13:9527/exec" \
  --data-urlencode "cmd=rm -rf /tmp/taihu-src && mkdir -p /tmp/taihu-src && tar -xzf /tmp/$(basename "$PKG") -C /tmp/taihu-src && cd /tmp/taihu-src && export PATH=/usr/local/go/bin:\$PATH && go build -o /tmp/taihu.$SHA ./cmd/taihu && echo BUILD_ALL_DONE" \
  --data-urlencode "timeout=280"
```

产物命名习惯：`/tmp/taihu.<commitid>`（如 `/tmp/taihu.1d40665`），与历史 tar.gz 包同名区分。

#### 2b. 启动参数（以当前 `cmd/taihu/cmd/server.go` 为准）

**注意**：`dist/restart_taihu.sh` 为历史遗留（`-addr/-name/-node/-reg-addr/-shm`）；`cmd/taihu-server` 目录已废弃。当前版本以下列 flag 为准：

| Flag | 位置 | 说明 |
|------|------|------|
| `--listen` | server 子命令 | 逗号分隔监听 IP（必填，如 `100.71.128.13`；**RPC 端口自动分配**在 [50000,51000]，通告地址 = 首个 IP:端口） |
| `--db` | server 子命令 | pebble 元数据目录（必填） |
| `--dev` | server 子命令 | 裸设备路径（必填，如 `/dev/nvme1n1`） |
| `--server-name` | server 子命令 | 实例唯一名（必填，TiKV 注册/容量 key，如 `n13-nvme1`；注册到 `/taihu/instances/{name}`） |
| `--batch` | server 子命令 | shm 批读批量：>0 启用"多 stream 多 worker"聚合批读（一次 io_submit 提交多个任务）；0 关闭（默认关闭） |
| `--batch-workers` | server 子命令 | shm 批读 worker 池大小（默认 8，并行批提交，K×batch = 整机在途批读数） |
| `--pd` | 根命令全局 | 逗号分隔 PD 地址（必填，任选其一即可自动发现 leader） |
| `--tikv-ca/--tikv-cert/--tikv-key` | 根命令全局 | TiKV TLS 证书（默认明文，无需传） |

启动示例（128.13，1 个实例）：

```bash
export PATH=/usr/local/go/bin:$PATH
setsid nohup /tmp/taihu.1d40665 server \
  --listen 100.71.128.13 \
  --db /tmp/taihu-db/nvme1 \
  --dev /dev/nvme1n1 \
  --server-name n13-nvme1 \
  --pd 100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379 \
  >/tmp/taihu-server-n13-nvme1.log 2>&1 &
```

三节点 9 实例全量部署脚本（nvme1/2/3 × 128.11/12/13）见 `dist/deploy3x3_128_v2.sh`（`--` 双横线 flag）。

启动内置校验（失败即退出）：

- **容量一致性**：读 `--dev` 裸盘容量与 TiKV 中该 `--server-name` 记录比对，不一致拒绝启动 → **换盘必须以新实例名部署**
- 段大小固定 8GiB（不可配置）；注册 + 1s 心跳 + SIGTERM 自动注销
- shm unix socket 固定 `/dev/<server-name>`（同机客户端走 shm，无需额外配置）
- 日志含 `cluster registered name=... addr=...` 即注册成功

### 3. 构建 EFS_nefs 客户端（taihu 后端）

go.work 已存在（`go 1.25.0` + `use .` + `use D:/workspace/taihu`），Windows 开发不受影响：

```bash
# windows 默认构建验证（零影响，必须有 stub.go 兜底）
go build ./pkg/object/...
```

**实际发布在 Linux 节点编译**（EFS_nefs tools 带 CGO 依赖，Windows 交叉编译 `CGO_ENABLED=0` 会因 tbos 依赖失败）。三个产物一次构建（128.12，`/tmp/code/EFS_nefs` 为源码目录）：

```bash
export GOTOOLCHAIN=auto
cd /tmp/code/EFS_nefs

# nefs.tools（管理端，必带 tools tag）
go build -tags taihu,tools -o /tmp/nefs.tools.taihu .
# nefs.client（挂载端，main 在仓库根目录 —— 勿用 ./cmd/client，那里没有 main）
go build -tags taihu -o /tmp/nefs.client.taihu .
# taihu_probe（端到端探针）
go build -tags taihu -o /tmp/taihu_probe ./cmd/taihu_probe
```

构建前需在节点源码上打两处补丁：

- **tbos 依赖**：`cmd/tools/destroy.go` 删除 `pkg/object/tbos` import 和 `cleanupTbosFiles` 函数；新增 `cmd/tools/destroy_notbos.go`（`//go:build !tbos` 空实现 `func cleanupTbosFiles(_ meta.Meta, _ *meta.FormatVol) {}`）。否则 `-tags taihu`（无 tbos）构建报 `build constraints exclude all Go files`
- **TiKV client-go 混合集群解码**：taihu 用 RawKV（原始 key）、EFS_nefs meta 用 TxnKV（memcomparable），同一 TiKV 集群下 region 边界解码报 `invalid marker byte`。patch `github.com/tikv/client-go/v2@v2.0.2/internal/locate/pd_codec.go`：`codec.DecodeBytes(r.Meta.StartKey)` 失败时 fallback 保留原始 key，不返回 error

验证产物：`file /tmp/nefs.client.taihu` 必须是 `ELF 64-bit ... executable`。若显示 `current ar archive` → 构建目标错了（编成了库，不是 main 包）。

发布期（脱离 go.work）：go.mod 增加

```
require github.com/liucxer/taihu v0.0.0
replace github.com/liucxer/taihu => <本地路径或 git 远端>
```

### 4. 对接配置（Storage "taihu"）

kvcache 同款 `StorageCfg.Extra` 注入（无需 meta，TiKV 直连自建 rawkv）：

```go
cfg := &object.StorageCfg{
    Storage: "taihu",
    Extra: map[string]string{
        "tikvPD": "100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379", // 必填
        // 可选键及默认值：
        // "node":              os.Hostname()  本机节点标识（仅标注/日志）
        // "conns":             "4"            每实例数据面连接数
        // "usageThreshold":    "0.9"          选实例水位阈值 used/capacity
        // "refreshInterval":   "5s"           实例发现刷新周期
        // "heartbeatTimeout":  "10s"          实例离线判定（≥3×服务端心跳 1s）
    },
}
blob, err := object.CreateNefsStorage(cfg, nil)
```

### 5. 端到端验证

前置：taihu-server 已注册（TiKV `/taihu/instances/` 下有活实例）。实例列表可用 taihu 仓库工具确认：

```bash
/tmp/taihu.1d40665 cluster list --pd 100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379
# 输出 9 行（3 节点 × 3 实例），STATUS 全为 online 即注册成功（HEARTBEAT_AGE 为负数/接近 0 表示活跃）
# 也可用 client 子命令：/tmp/taihu.<commitid> client list --pd ...（查看 SDK 客户端注册）
```

#### 5a. taihu_probe 全流程

```bash
# 分发 probe 到 128.11 后执行（无参默认 tikvPD=127.0.0.1:2379，务必传真实 PD）
curl -s --max-time 120 -X PUT -T /tmp/nefs-taihu-probe -H "X-Token: 95279527" \
  "http://100.71.128.11:9527/upload?path=/tmp/nefs-taihu-probe"
curl -s -G -H "X-Token: 95279527" "http://100.71.128.11:9527/exec" \
  --data-urlencode "cmd=chmod +x /tmp/nefs-taihu-probe && /tmp/nefs-taihu-probe 100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
```

期望输出逐行：`[OK] storage: taihu` → `[OK] Put 4MB` → `[OK] Get full 4MB` → `[OK] Get range 4KB` → `[OK] Head size 4194304` → `[OK] Delete` → `[OK] Head after delete not found` → 最后 `[ALL OK]`。

#### 5b. 集群语义验证

1. **路由/本地优先**：3 节点各起 taihu-server 注册同一 TiKV；128.11 上跑 probe，数据落在本地实例（shm 路径）
2. **实例容错**：kill 客户端路由缓存中的实例后，读走索引回退仍正确（RouteCache 失效 → TiKV `/taihu/index/` 定位新实例）
3. **冷启动**：客户端重启后 RouteCache 为空，读靠索引可正常命中

### 6. mgmt 创建 taihu 存储资源（pool → target → vol）

前置：mgmt 集群正常（`/endpoint/meta-rs` 能返回 rs）、nefs.tools.taihu 已部署到节点（128.12 实测）。顺序固定：**先 pool、再 target、最后 vol**。

```bash
cd /tmp
MASTER=100.71.128.12:12379   # mgmt API 地址（mgmt.conf 的 port；128.12 为 host 网络 0.0.0.0:12379）

# 6a. pool（--storage taihu，--extra 注入 tikvPD；--pool 为池名）
./nefs.tools.taihu pool add --pool taihu-test --storage taihu \
  --extra '{"tikvPD":"100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"}' \
  --desc "taihu pool" --master $MASTER

# 6b. target（--pool 绑定 pool 名）
./nefs.tools.taihu target add --name taihu-tgt --pool taihu-test \
  --desc taihu --master $MASTER

# 6c. vol（--capacity 单位 GiB，实测 100 → 100GiB；--target 指定 target 名）
./nefs.tools.taihu vol create --name taihu-vol --capacity 100 \
  --target taihu-tgt --master $MASTER
```

校验（全部 `-o json`）：`pool list`、`target list`、`vol list`。记录 vol 的 `UUID` 供挂载使用（实测：pool `571808fa-...`，target `15c410a9-...`，vol `c312033e-...`）。

### 6-HTTP. mgmt HTTP 接口直连创建 pool/target/vol（不依赖 nefs.tools）

> 来源：mgmt 源码 `mgmt/proto/admin_proto.go`（API 路径常量）+ `mgmt/mgrs/api_service.go`（handler）+ `mgmt/api_test/test_api.go`（请求示例，实测 2026-09-13 全通）。
> API 前缀直接挂根路由（**无 `/mgmt/v1/`**），Gin+Mux 双注册，128 mgmt 监听 `:12379`（host 网络）。
> **为什么用 HTTP 直连**：nefs.tools.taihu 的 `pool add` 用普通名会因服务端 `util.IsUUID` 校验失败被静默吞错（FormatOutput 掩盖、rc=0 假象）；HTTP 直连用 UUID 名数次成功，且响应直接返回 `code/msg/data`，易排错。

**请求格式总表**（curl 实测成功）：

```bash
MASTER=http://100.71.128.12:12379
TIKVPD='"100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"'
POOL_NAME=$(cat /proc/sys/kernel/random/uuid)   # pool/vol 名必须为 UUID（服务端 IsUUID 强校验）
VOL_NAME=$(cat /proc/sys/kernel/random/uuid)

# ① 创建 pool —— POST JSON body，字段即 StoragePoolConf：Name/Desc/Storage/Extra(map[string]string)
curl -s -X POST $MASTER/storage/addPool -H "Content-Type: application/json" \
  -d "{\"Name\":\"$POOL_NAME\",\"Desc\":\"taihu pool via http\",\"Storage\":\"taihu\",\"Extra\":{\"tikvPD\":$TIKVPD}}"
#   响应 data 返回新 pool（含 ID）→ 提取 ID/PoolName 供 target 用

# ② 创建 target —— GET + query 参数（poolID 与 poolName 二选一；整个 URL 注意 & 需引号）
curl -s "$MASTER/storage/setTarget?name=taihu-tgt&poolID=<POOL_ID>&poolName=<POOL_NAME>&desc=taihu%20target"

# ③ 创建 vol —— POST JSON body，字段即 proto.Vol；Name 必须是 UUID，Target 用 target 名
curl -s -X POST $MASTER/admin/createVol -H "Content-Type: application/json" \
  -d "{\"Name\":\"$VOL_NAME\",\"Capacity\":100,\"Target\":\"taihu-tgt\",\"BlockSize\":4194304,\"Inodes\":1000000,\"DirStats\":true,\"EnableBucketLink\":false,\"EnableDirLazyLoad\":false}"
```

**验证/查询接口**：

```bash
curl -s $MASTER/storage/listPool    # [{"ID":1,"Name":"<uuid>","Storage":"taihu","Extra":{"tikvPD":...}}]
curl -s $MASTER/storage/listTarget  # [{"Name":"taihu-tgt","PoolID":1,"PoolName":"<uuid>",...}]
curl -s $MASTER/vol/list            # [{"ID":1,"Name":"<vol-uuid>","Capacity":100,"Target":"taihu-tgt",...}]
```

**关键字段语义**（对齐 2026-09-13 实测）：

| 接口 | 方法 | 关键参数 | 说明 |
|------|------|---------|------|
| `/storage/addPool` | POST | `Name`(UUID)、`Storage`("taihu")、`Extra`(map:`{"tikvPD":"<PD逗号列表>"}`) | Extra 为 `map[string]string`，JSON tag 大写；taihu 后端仅消费 `tikvPD`（EFS_nefs/pkg/object/taihu/config.go `extra["tikvPD"]`） |
| `/storage/setTarget` | GET | `name`、`poolID` 或 `poolName`、`desc` | target 名为普通字符串即可（无需 UUID） |
| `/admin/createVol` | POST | `Name`(UUID)、`Capacity`(GiB)、`Target`(target 名)、`BlockSize`(字节)、`Inodes` | 校验失败返回 `Create volume failed. Volume name must be uuid / Invalid target name[...]` |
| `/storage/listPool` | GET | - | 返回全部 pool 配置（含 ID） |
| `/storage/listTarget` | GET | - | 返回 target→pool 绑定 + PoolName |
| `/vol/list` | GET | - | 返回全部 vol（含 Name/Capacity/Target） |

**注意事项**：
- **pool/vol 名必须是 UUID 格式**（`util.IsUUID` 强校验），否则 HTTP 返回 code=1 但 nefs.tools 会静默吞错。可用 `cat /proc/sys/kernel/random/uuid` 生成。
- `addPool` 的 `Extra` 传 `tikvPD`（小写 key）没错——SDK 端读 `extra["tikvPD"]`；JSON body 键名是 `Extra`（首字母大写，map 值）。
- path 无 `/mgmt/v1/` 前缀：`/mgmt/v1/storage/listPool` 会 404。
- mgmt 源码对应：`proto/admin_proto.go`（路径常量）、`proto/pool.go`/`proto/target.go`/`proto/vol.go`（结构体 JSON tag）、`mgrs/api_service.go`（handler）、`api_test/test_api.go`（TC-2/3/4 完整用例含 configPool/targetAppendPool/vol update 等）。

### 7. 挂载 nefs.client + 读写验证

```bash
mkdir -p /mnt/taihu
setsid nohup ./nefs.client.taihu mount <vol-uuid> /mnt/taihu \
  --master 100.71.128.12:12379 --enable-xattr \
  --log /var/log/nefs.client.taihu.log > /tmp/mount_taihu.log 2>&1 &

# 等 5s 后看日志/进程：出现 "is ready at /mnt/taihu" 即成功
ps aux | grep nefs.client.taihu | grep -v grep
tail -30 /var/log/nefs.client.taihu.log
```

挂载日志关键链路：`S6-创建对象存储 CreateStorage(TBOS引擎初始化)` OK（taihu 后端注册成功）→ `S7-meta Init` → `S9-NewSession` → `S11-FUSE挂载` → `OK, <uuid> is ready at /mnt/taihu`。`Allocating client ID from mgmt` 说明 mgmt 连接正常。

读写验证（全部通过为合格）：

```bash
df -h /mnt/taihu                        # NEFS:<uuid> 容量可见
touch /mnt/taihu/hello.txt && ls -l /mnt/taihu/
dd if=/dev/urandom of=/mnt/taihu/test.bin bs=1M count=64   # 实测 187MB/s
cat /mnt/taihu/test.bin | md5sum; ls -l /mnt/taihu/test.bin  # 读回字节数一致
echo hello >> /mnt/taihu/hello.txt && cat /mnt/taihu/hello.txt
rm /mnt/taihu/test.bin && df -h /mnt/taihu                  # 删除后空间回收
```

### 8. mgmt 配置参考（对照 146 生产）

配置位置：128.12 为 docker 容器 `nefs-mgmt`（host 网络，`/export/Data/mgmt-deploy/mgmt.conf` bind 到容器 `/etc/mgmt.conf`，mgr 进程 `/usr/sbin/nefs.mgmt master`）。146 为裸进程，配置在宿主机 `/etc/mgmt.conf` + `/etc/mgmt_mount.conf`。

**对照原则：参考生产关键字段，但不用完全一样** —— 测试环境保留自己的 peers/ip/port/metaPeers，不引入生产专属组件：

| 项 | 146 生产 | 128.12 测试 | 处理 |
|----|----------|-------------|------|
| `peers` | 1:..202:12580,2:..203:12580,3:..204:12580 | 1:..11:12379,2:..12:12379,3:..13:12379 | 保留 128.12 |
| `metaPeers` | PD 12379（TLS） | PD 2379（http） | 保留 128.12 |
| `metaTLS`+证书路径 | true+crt/pem | 无 | 128.12 PD 为 http，不加 |
| consul/etcd/exporter 等 | 有 | 无 | 生产组件，不加 |

**mgmt_mount.conf（挂载配置，参考 146 对齐关键项）**：缺失时客户端报 `Failed to get client config for vol:...`，且 mgmt 日志报 `LoadConfigFile err: open /etc/mgmt_mount.conf: no such file or directory`。修复：上传文件后 `docker cp /tmp/mgmt_mount.conf nefs-mgmt:/etc/mgmt_mount.conf`（mgr 每次读取，无需重启）。

参考 146 对齐：`heartbeat` "2"（128.12 原 "12"）、补 `clientIDPoolSize` 2000、`tbos-workercnt` 6、`no-agent` false；**`protocol-scheme` 按环境**（146 https / 128.12 http）。生效验证：`docker exec nefs-mgmt curl -s http://127.0.0.1:12379/config/mountConfig`，data 中出现 `"heartbeat":"2"`、`"clientIDPoolSize":2000`。

## 注意事项

- **只改 EFS_nefs 侧**：taihu 仓库一行不动（`internal/cluster` 的 `InstanceInfo`/索引 schema 为唯一事实，只读参考）
- **无覆盖写语义**：key 基本只写一次，重复 key 走"删旧写新"，不依赖覆盖写
- **换盘/容量变化**：taihu-server 启动容量校验失败 → 用新 `-server-name` 部署
- **旧脚本陷阱**：`dist/restart_taihu.sh` 参数（`-addr` 等）已被 `main.go` 新参数取代，以本 skill 表为准
- **registry.paas 不可达**：新增节点镜像走 save/load + 9527 分发
- **测试受限**：仅 128.11/12/13；128.12 根分区 99% 满，避免写 `/`
- **Windows 构建**：不带 `-tags taihu` 时整包排除（stub.go），`go build ./pkg/object/...` 必须通过
- **Get 资源纪律**：probe/消费端 Get 的 ReadCloser 必须 Close（幂等归还池缓冲），避免连接缓冲泄漏
- **nefs.client 构建目标**：main 在仓库根目录。误用 `./cmd/client` 会编成 ar archive（`file` 显示 `current ar archive`，`nohup` 报 Permission denied）。确认产物是 ELF executable 再挂载
- **节点远程操作**：proxy_client.py 与 128.12 存在 HTTP 兼容问题，用 `curl.exe -H "X-Token: 95279527" http://<node>:9527/exec` 直调；Windows PowerShell 不支持 `&&`，复杂命令 base64 编码后 `echo <b64> | base64 -d | sh` 在节点执行
- **nefs.client 挂载需后台**：mount 为前台阻塞命令，必须 `setsid nohup ... &`，再轮询日志等 "is ready"