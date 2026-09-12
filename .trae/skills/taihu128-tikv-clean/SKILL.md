---
name: "taihu128-tikv-clean"
description: "清空 128 集群（100.71.128.11/12/13）taihu 写入 TiKV 的全部 key（/taihu/ 命名空间：实例注册、key→实例索引、SDK客户端注册），用于重置 128 集群元数据/重跑测试前清理。TRIGGER: 用户要求清理/清空/重置 128 集群 taihu 在 TiKV 里的所有 key、清除 128 集群注册与索引时使用。"
---

# 清理 128 集群 taihu 写入 TiKV 的全部 key

128 集群的 taihu-server 实例把**全部元数据**写进 128 集群的 TiKV（无 TLS），落在同一个共享命名空间 `/taihu/` 下，共三类（见 `internal/cluster/kv.go`）：

| 前缀 | 内容 | 写入方 |
|------|------|--------|
| `/taihu/instances/{name}` | 实例注册信息（InstanceInfo JSON） | taihu-server 启动注册 / 1s 心跳 / 退出注销 |
| `/taihu/index/{key}` | key → 实例名 映射（索引锚定） | rpccluster 写路径异步索引 |
| `/taihu/clients/{id}` | SDK 客户端注册 + 心跳 | SDK（配置了 ClientID 时） |

本 skill 通过 `taihu-cli cluster purge` 一次性清空 128 集群的整个 `/taihu/` 命名空间，把集群恢复成「零元数据」状态，供重新部署/重跑压测前重置。

## 128 集群 TiKV 参数

| 项 | 值 |
|----|-----|
| PD | `100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379` |
| TLS | 无（明文连接） |

## 核心机制

- **范围删除**：`cluster.KV.DeleteRange(start, end)`（TiKV 端 rawkv `DeleteRange`，原子删除 `[/taihu/, /taihu0x8..)` 区间全部 key），`TaihuDataRange()` 返回该区间。
- **三态安全**：先统计各前缀数量 → 无 `-confirm` 时只预览退出 → 带上 `-confirm` 才真正删除。

## 前置

- 本地能交叉编译 taihu-cli（arm64，节点无公网/缓存不全，需本地编译后上传）。
- **先停掉 128 集群所有 taihu-server**（否则其 1s 心跳会立刻把实例重新注册回来，删了也白删）。
- 目标节点能访问 128 TiKV PD（100.71.128.11/12/13:2379）。

## 步骤

1. **本地交叉编译 taihu-cli（linux/arm64）**：

```bash
cd d:\workspace\taihu
$env:GOOS="linux"; $env:GOARCH="arm64"; $env:CGO_ENABLED="0"
$SHA="<git short sha>"; $TS="<YYYYMMDDHHMM>"
go build -trimpath -ldflags "-X github.com/liucxer/taihu/internal/version.Commit=$SHA -X github.com/liucxer/taihu/internal/version.BuildTime=$TS" -o dist/build/taihu-cli ./cmd/taihu-cli
```

2. **上传到 128.12**（用 nefs-proxy 或 curl）并 chmod：

```powershell
# 方式 1：nefs-proxy
$PY="C:\Users\USER484887\AppData\Roaming\uv\python\cpython-3.12.14-windows-x86_64-none\python.exe"
$P="d:\workspace\taihu\.trae\skills\nefs-proxy\proxy_client.py"
& $PY $P --node 128 upload --local "d:\workspace\taihu\dist\build\taihu-cli" --remote /tmp/taihu-cli
& $PY $P --node 128 exec --cmd "chmod +x /tmp/taihu-cli && echo OK"

# 方式 2：curl（Windows）
curl.exe -s --max-time 120 -X PUT -T dist/build/taihu-cli -H "X-Token: 95279527" "http://100.71.128.12:9527/upload?path=/tmp/taihu-cli"
curl.exe -s -G -H "X-Token: 95279527" "http://100.71.128.12:9527/exec" --data-urlencode "cmd=chmod +x /tmp/taihu-cli && echo OK"
```

3. **预览（不删）**：

```bash
PD="100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379"
/tmp/taihu-cli cluster purge --pd $PD
```

会打印三类数量与 total；无数据则显示「无元数据，无需清洗」。

4. **确认删除**：

```bash
/tmp/taihu-cli cluster purge --pd $PD --confirm
```

5. **验证**：再跑一次不带 `--confirm` 的 purge（应显示 0），或 `taihu-cli cluster list --pd $PD` 应无实例。

## 注意事项

- **必须先停 server 再删**：运行中的 taihu-server 心跳会重新写入 `/taihu/instances/`。
- 删除是针对 128 集群的 TiKV，会把 128.11/12/13 上注册的全部实例/索引一并清空。
- 只删 `/taihu/` 命名空间，不碰 TiKV 其他数据（其他前缀不受影响）。
- 清洗后重部署，实例会重新注册、索引由写路径重建，无需人工干预。
- **与 146 集群隔离**：128 TiKV 和 146 TiKV 是独立的两套，互不影响。

## 相关 Skill

- `编译部署skill`：编译部署 taihu 到 128.12
- `性能测试skill`：128.12 上读写性能测试
- `taihu146-tikv-clean`：清理 146 集群 taihu 在 TiKV 的元数据
