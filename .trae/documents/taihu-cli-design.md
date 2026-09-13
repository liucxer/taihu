# taihu CLI 工具设计文档

- 状态：设计稿 v2（已确认：①SDK 客户端需向 TiKV 注册+心跳保活；②segment/key 枚举走服务端新增 RPC；③CLI 使用 cobra）
- 目标版本：v3 传输栈（netpoll + LinkBuffer 帧协议）之上

## 1. 背景与目标

现有命令行工具只有两个：
- `taihu-client`：仅支持对**单个实例**（`-addr`）做 put/get/delete/stat，无集群视角，无管理能力。
- `taihu-cluster-check`：临时诊断程序（注释标明"不提交"），功能单一。

需要一款正式、可长期维护的 `taihu` CLI，覆盖运维与排查场景：

1. 查询集群信息（实例列表、状态、容量水位、连通性）；
2. 客户端信息——**所有 SDK 客户端也需像服务端一样向 TiKV 注册 + 周期心跳续约**，CLI 可查询全部注册客户端的存活/分布；
3. key 操作（读写删，兼 stat/meta/list）；
4. 查询每个 taihu 实例的 segment 信息（汇总 + 明细，走服务端新增 RPC）。

### 非目标（首版不做）
- 数据面性能压测（已有 taihu bench storage / taihu bench cluster / taihu-loop-bench）。
- 在线扩缩容、实例下架等变更类操作（仅只读观测 + key 读写删 + 客户端注册）。
- 图形界面、Web 面板。

## 2. 总体设计

### 2.1 二进制与代码落点
- 新命令：`cmd/taihu/main.go` + `cmd/taihu/cmd/`（cobra 经典布局）。
- CLI 框架：**github.com/spf13/cobra**（新增直接依赖，随核心 imported 拉入 pflag；启用内置 shell completion 子命令）。
- 集群/数据面复用：`internal/cluster`（KV 注册/心跳/枚举）、`pkg/rpcclient`（直连）、`pkg/rpccluster`（集群路由）。

### 2.2 命令树（cobra）

```
taihu
├── version                        版本号（commit_日期）
├── cluster
│   ├── list                       实例列表（注册区全量，含 stale 标注）
│   ├── status                     实例健康/水位/连通性(RTT)体检
│   └── index                      key→实例 索引概览（数量/分布/前缀过滤）
├── key
│   ├── put                       上传（缺省 stdin / -file）
│   ├── get                       下载（-off/-size 区间，缺省 stdout / -file）
│   ├── delete                    删除
│   ├── stat                      逻辑大小
│   ├── meta                      对象落盘元数据（segID/offset/size）
│   └── list                      按前缀枚举 key（新 RPC）
├── instance
│   └── segments                  每个实例的 segment 汇总（-detail 出明细）
└── client
    ├── list                      注册的 SDK 客户端清单（心跳存活状态）
    └── info                      CLI 自身信息/环境/可达性
```

### 2.3 全局参数（root persistent flags）

cobra 约定：长选项用双横线，短选项单横线。本工具全局长选项：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--pd` | "" | TiKV PD 地址列表（逗号分隔）；集群类命令要求非空 |
| `--node` | "" | 本节点标识（status 标记 LOCAL、client list 高亮本机） |
| `--timeout` | 5s | 单次交互超时 |
| `--json` | false | 机器可读 JSON 输出 |

子命令局部参数：`--addr`（直连实例）、`--instance NAME`、`--key`、`--file`、`--size`、`--off`、`--prefix`、`--limit`、`--detail`。

连接模型复用现有库：
- 集群信息/索引/客户端清单：`cluster.NewTiKVKV(ctx, pdAddrs)` + `cluster.ListInstances / ListClients / kv.Scan(index 前缀)`。
- key 数据面：`--addr`/`--instance` 直连 → `rpcclient.DialPool(ctx, addr, 1)`；`--pd --node` 集群路由 → `rpccluster.NewCluster(...)`（本地优先 + 索引 + 回源，首版 Source 留空仅路由）。
- 健康/水位：新增服务端 RPC（§4），经 rpcclient 逐实例调用。
- 连接兜底：rawkv 对不可达 PD 的发现不服从 ctx，`connectKV` 包 goroutine + select 按 `--timeout` 兜底报错。

### 2.4 输出约定
- 文本模式：人类可读，字节自动转 GB/MB（2 位小数），不做 ANSI 上色（终端兼容性优先）。
- `-json`：单对象 JSON（字段见各子命令）。
- 多实例聚合命令输出带 `instance=` 前缀，便于 grep 归组。

## 3. 子命令规格

### 3.1 `taihu cluster list`
列出注册区全部实例（含心跳超时的僵尸，状态标注）。排序：Node 分组后按 name。

```
NAME      NODE     ADDR           SHM        STATUS   CAPACITY   USED      AVAILABLE  HEARTBEAT_AGE
taihu-a   node1    10.0.0.1:50051  /tmp/xxx   online   16.0 GB    3.2 GB    12.4 GB    1s
taihu-c   node2    10.0.0.2:50051             stale    16.0 GB    8.0 GB    6.9 GB     62s
```

- STATUS：online（心跳及时）/ stale（超时，按 `Aliveness` 判定，默认 5s）。
- 数据源：`cluster.ListInstances`。`-json` 输出 `[{name,node,addr,shm_addr,capacity,used,available,start_time,last_heartbeat,status}]`。

### 3.2 `taihu cluster status`
实例体检：逐实例 dial + `Ping`（新 RPC）+ `Segments` 汇总（新 RPC）：

```
instance   tcp_rtt   shm_ok  seg_free  seg_active  seg_full  seg_reclaiming  cursor(seg/off)  object_count
taihu-a    0.3ms     yes     128       2           1917      1               12/3.2GB         45123
taihu-c    ERR: dial timeout (10.0.0.2:50051)
```

- 任一步失败不中断整体，逐实例记错误；`shm_ok` 仅在 `ShmAddr` 非空且同 node 时尝试。

### 3.3 `taihu cluster index`
扫描 `/taihu/index/`，按实例归组计数；`-prefix` 过滤。

```
index entries: 1000  (taihu-a: 620, taihu-b: 380)
prefix "rk-" -> 200 entries (taihu-a: 120, taihu-b: 80)
```

- 语义等价 taihu-cluster-check index 模式，正式化后临时程序退役。

### 3.4 `taihu key put/get/delete/stat`
与 `taihu-client` 同语义（数据面复用 rpcclient/rpccluster）：

```
taihu key put    -key K [-size N] [-file F] [-addr A | -pd PDS -node N]
taihu key get    -key K [-off O] [-size N] [-file F] [...]
taihu key delete -key K [...]
taihu key stat   -key K [...]
```

- put `size` 缺省=输入长度；get `size=-1` 全量；Get 池化缓冲用毕 `release()`。
- 目标选择：`-addr`/`-instance` 直连优先；均空且给 `-pd` 走 rpccluster 路由。集群 delete 调 `rpccluster.Storage.Delete`（清索引/路由缓存）。
- `-json`：put 输出 `{key,size,instance}`；get 写文件/stdout 时禁用 -json 混用二进制。

### 3.5 `taihu key meta`（新 RPC opMetaReq）
返回对象落盘元数据：

```
key rk-0001 size=1048576 seg=12 off=4194304
```

- `-json`：`{key,size,segment_id,offset}`。

### 3.6 `taihu key list`（新 RPC opKeysReq）
按前缀枚举单实例 key（缺省 ""=全部）：

```
taihu key list -addr A [-prefix rk-] [-limit 10000]
```

- 流式下发聚合，不整读进内存；`-limit` 钳制返回条数。

### 3.7 `taihu instance segments`
默认遍历全部在线实例，每实例打汇总行；`-detail` 展开明细：

```
taihu instance segments [-pd PDS] [-instance NAME] [-detail]

taihu-a:
  total=2048 free=128 active=2 full=1917 reclaiming=1
  cursor=seg 12 off 3.2 GB  object_count=45123
  [detail] segID  state    alive  reclaim_seq
           0      active   234    0
           1      free     0      0
```

- `-json`：`{"instances":[{name,summary{...},segments:[{id,state,alive_count,reclaim_seq}]}]}`。
- 数据量 = 段数 × 25B（2048 段 ≈ 50KB），落多帧流式协议（§4）保持一致。

### 3.8 `taihu client list`（SDK 注册客户端清单）
读取 `/taihu/clients/` 注册区（§5），列出全部注册 SDK 客户端：

```
ID             NODE    HOST      PID   SDK_VER          STATUS   HEARTBEAT_AGE
cache-svc-01   node1   host-1    1234  8abdf4d_...     online   0.8s
etl-job-x      node2   host-2    5678  8abdf4d_...     stale    63s
2 registered, 1 online, 1 stale
```

- STATUS 判定与实例一致（LastHeartbeat + 5s 超时）。
- `-json`：`[{id,node,host,pid,sdk_version,start_time,last_heartbeat,status}]`。

### 3.9 `taihu client info`（CLI 自身/环境信息）
工具自身版本、生效配置、KV 后端状态、可达实例（RTT 复用 Ping）：

```
taihu 8abdf4d_202609111002 (go1.25)
config: pd=10.0.0.10:2379 node=node1 timeout=5s
kv backend: tikv rawkv (ok, instances: 3, clients: 2, index entries: 1000)
connectivity: taihu-a 0.3ms (shm ok); taihu-b 0.4ms (no shm); taihu-c err: dial timeout
```

## 4. 服务端能力扩展（新 RPC，已确认）

当前 `internal/transport` 仅暴露 Put/Get/Delete/Stat 四个 op；segment 明细、key 枚举、对象元数据、ping 均无 RPC 通道（`SegmentStats()` 仅计数且只在服务端日志）。需扩展帧协议：

### 4.1 新增 OpCode（protocol.go）
沿用帧格式 `[len(4)][sid(4)][op(1)][payload]`，payload 字段一律大端：

```
opPing      0x0C  请求空；响应: [code(4)][server_time_unix_nano(8)]（客户端掐表测 RTT）
opMetaReq   0x0D  请求: [keyLen(4)] key（编码同 opStatReq）
                  响应: code==0 → [segID(8)][off(8)][size(8)]；否则仅 [code(4)]
opSegReq    0x0E  请求空；多帧流式响应:
                  opSegSum  : [code(4)][total(8)][free(8)][active(8)][full(8)]
                              [reclaiming(8)][cursorSeg(8)][cursorOff(8)][segSize(8)][objectCount(8)]
                  opSegData : [count(4)] + count × ([segID(8)][state(1)][alive(8)][reclaimSeq(8)])
                  opSegEnd  : 空（正常结束）
opKeysReq   0x0F  请求: [prefixLen(4)] prefix（可为空）
                  响应: 0..N 帧 opKeysData（[count(4)] + count × ([keyLen(4)] key)）+ 收尾 opResp[code(4)]
```

- 新 op 加入 `dispatch` 首帧白名单并注册 handler；keyLen/prefixLen 沿用 `maxKeyLen` 校验，count 钳制防放大。

### 4.2 服务端落点
| 层 | 新增 |
|---|---|
| `internal/metastore.Store` | `ListSegments(ctx, fn(id, SegmentMeta))`；`Cursor() (segID, off)`；objectCount 复用 `IterMapping` 计数 |
| `pkg/taihu.Storage` | 透传 `ListSegments/Cursor/ListKeys(prefix)`；`GetMapping` 已有（供 meta）；`Ping` |
| `internal/transport` | 新 op 编码/解析/处理器 + dispatch 白名单 |
| `pkg/rpcclient` | `Ping() (rtt, serverTime, err)`、`Meta(key) (ObjectMeta, err)`、`Segments() (Summary, []Entry, err)`、`ListKeys(prefix) ([]string, err)` |
| shm 路径 | 首版 admin op 仅走 TCP（同机亦有 TCP 地址），已知取舍，文档标注 |

### 4.3 兼容性
仓库内服务端与 CLI 同步发布；旧服务端对未知 op 首帧按协议错误关连接 → 客户端报 rpc error 提示版本不匹配。帧头暂不加 version 字段。

## 5. 客户端（SDK）续约保活机制（新，已确认）

要求：**所有 SDK 客户端定期向 TiKV 续约保活，与当前服务端一致**；`taihu client list` 据此查询。

### 5.1 注册区与数据模型
- 新 KV 前缀：`ClientKeyPrefix = "/taihu/clients/"`，key = `{ID}`（全局唯一，调用方指定，如 `host:pid:随机种子` 或业务名）。
- `ClientInfo`（JSON 序列化，字段与 InstanceInfo 同风格）：

```
ID, Node, Addr(可选，SDK 数据面地址), Host, Pid, SDKVersion, Extra(调用方标签 map),
StartTime(注册时刻固定), LastHeartbeat(每周期刷新)
```

### 5.2 internal/cluster 改造
与实例注册**平行新增**（不进泛型，避免过度设计）：`client.go` 提供
`ClientKey / ClientScanRange / RegisterClient / UnregisterClient / RunClientHeartbeat / ListClients / GetClient`，
心跳语义（interval 1s、超时判定 5s、写失败下周期重试、ctx 取消退出）与 `register.go` 完全一致。

### 5.3 SDK 接入点
- `pkg/rpccluster.ClusterConfig` 新增可选字段：`ClientID`、`ClientAddr`、`ClientLabels`；`NewCluster` 在 KV 非空且 ClientID 非空时自动注册 + 启动心跳，`Close` 时注销（unregister 尽力而为）。
- 纯 `rpcclient`（直连）用户不自动注册；提供导出函数 `rpccluster.RunClientKeepalive(ctx, kv, info, interval)`（内部复用 5.2 实现），供任意 SDK 显式开启。
- 保活失败语义：与实例心跳一致（不致命，下周期重试）；离线由 CLI 按 LastHeartbeat 判定。

## 6. 附加能力建议（已纳入命令树）

1. 实例健康体检（`cluster status`：RTT + 水位 + 段统计)；
2. 索引诊断（`cluster index`，`taihu-cluster-check` 退役）；
3. key 定位排查（`key meta` 段/偏移、`key list` 前缀枚举）；
4. SDK 客户端可观测（`client list`，配合 §5 保活机制）；
5. 机器可读输出（全局 `-json`）与 shell completion（cobra 内建）。

将来可扩展：实例级"接入客户端连接数"运行时观测 RPC、`--watch` 连续观测、段水位可视化。

## 7. 实施计划（实现状态 2026-09-11 全部完成）

1. ✅ **协议与服务端扩展**：protocol.go 新 op（opPing/opMetaReq/opSegReq/opKeysReq + 应答帧）+ dispatch；metastore/taihu/transport/rpcclient 能力补齐；rpcclient admin 往返单测（Ping/Meta/Segments/ListKeys）。
2. ✅ **CLI 骨架**：引入 cobra；`cmd/taihu` + `cmd/taihu/cmd` 布局、root 全局参数、version/completion（内建）。
3. ✅ **集群命令**：`cluster list/status/index`、`client info`。
4. ✅ **key 命令**：put/get/delete/stat（直连 + 集群路由）+ `key meta/list`（meta/list 仅直连）。
5. ✅ **segment 命令**：`instance segments`（汇总 + detail + json）。
6. ✅ **SDK 保活**：internal/cluster `client.go` + `rpccluster` 接入（ClusterConfig.ClientID/ClientAddr/ClientLabels、自动注册/心跳/Close 注销）+ keepalive 单测；`client list`。
7. ✅ **收尾**：`-json` 全命令覆盖、connectKV 超时兜底、TiKV 日志抑制、文档。

> 注：cobra 长选项须双横线（`--addr`）；`key get --json` 需配合 `--file` 防二进制混写 stdout。
> 已知取舍：admin RPC 首版仅 TCP 路径（shm 不实现）；macOS 上 `TestDialPoolMultiFrame` 为预存失败（device_other 无 O_DIRECT），与本次改动无关。

## 8. 未决问题（待确认）

1. `ClientInfo` 字段集与 SDK 接入范围：是否所有 pkg/rpcclient 直连用户也要注册（默认否，显式开启）；Extra 标签首版是否放开自由 map。
2. 心跳/超时参数是否需要与实例可区分（默认同 1s/5s）。
3. `client list` 条数上限：SDK 客户端数量可能远超实例，`-limit` 默认 10000，是否需要分页。