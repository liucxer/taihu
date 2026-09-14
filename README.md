# taihu — 高性能对象存储引擎

`taihu` 是一个面向裸盘（NVMe）的高性能对象存储系统：服务端（`taihu server`）把本机裸盘组织为对象存储，业务通过唯一 SDK **taihu-client**（`pkg/taihu-client`）访问——**必须经 TiKV 集群路由**定位实例，不支持绕过集群直连。数据以对象（key → 任意大小字节序列）组织，单对象上限等于段大小（8 GiB）。

核心设计取向：**O_DIRECT 裸盘直写直读 + 异步 IO + 全程零拷贝**，配合纯 Go 元数据层（Pebble）与共享内存（shmipc）同机加速，追求极致的端到端读写带宽。

> 设计迭代详见 **[doc/README.md](doc/README.md)**（57 篇文档的索引，逐篇标注【现行】/【历史存档】）：
> `设计文档_v1/v2/v3` 与 segment 级 GC、Compaction、集群支持等专项设计，以及 46 篇读写性能测试报告。

---

## 核心特性

- **裸盘直写（O_DIRECT）**：Linux 上以 O_DIRECT 打开设备，数据不经页缓存，写等效同步刷盘；读写缓冲/偏移/长度全部 4K 对齐。
- **异步磁盘 IO（libaio）**：`io_setup/io_submit/io_getevents` 原生 AIO（兼容老内核，无需 io_uring），单完成泵串行分发；支持一次 `io_submit` 批量提交多条直读。
- **分段顺序写**：设备按 8 GiB 固定段划分（容量由启动时读取真实设备计算），对象落在段内 4K 对齐偏移，天然贴近顺序写。
- **零拷贝读路径**：设备读直接进 bufpool 对齐池化缓冲；服务端经共享内存直读（O_DIRECT DMA 进共享内存切片，免 memcpy）；客户端 Get 单帧 `TakeTry` 零拷贝移交接收缓冲。
- **纯 Go 元数据层**：CockroachDB Pebble（LSM）持久化 key→映射 与 段状态；1 GiB 有界 LRU 分片缓存（256 分片）read-through 加速。
- **空间回收三件套**：对象删除（映射删除）→ segment 级 GC（Reclaiming → Free 回收入池）→ 后台 Compaction（高空洞段数据搬移，CAS 防并发写冲突）。
- **自研 RPC 传输**：netpoll + LinkBuffer 自定义帧协议（替代 gRPC），4 MiB 分帧、流式多路复用、错误码与库错误一一映射。
- **同机共享内存 IPC**：shmipc unix socket（`/dev/<实例名>`），数据帧 4K 对齐，服务端读路径免 memcpy。
- **TiKV 集群支持**：PD 注册实例/心跳/容量记录，key→实例索引，本地优先路由 + 回源重建，SDK 客户端保活注册。
- **统一 CLI（cobra）**：服务端、性能压测（本地/直通/集群）、对象读写与运维均收敛于单一 `taihu` 命令。

---

## 架构总览

```
┌───────────────────────────── 应用/SDK 层 ─────────────────────────────┐
│   taihu CLI (cobra)                  pkg/taihu-client (唯一 SDK)      │
│   bench storage|single|cluster       集群模式经 TiKV 路由            │
└───────────────┬──────────────────────────────┬──────────────────────┘
                │ 本机: shmipc 共享内存           │ 跨节点: netpoll 自定义帧协议
                ▼                              ▼
        ┌─────────────────────  taihu server (internal/transport)
        │  Put / Get / Delete / Stat (4 个 RPC) + admin RPC
        └──────────────────────────────┬──────────────────────┐
                                       ▼                      ▼
        ┌────────────────────────────────────────────────────────────────┐
        │   internal/storage.Storage（对象存储库）                        │
        │   ├─ metastore（Pebble：mapping / state 两命名空间）            │
        │   │    ├─ 1GiB LRU 分片元数据缓存                               │
        │   │    ├─ segment 生命周期：Free→Active→Full→(Compacting)→      │
        │   │    │                    Reclaiming→Free                     │
        │   │    └─ 游标分配 AllocateSegment + 读引用计数 Ref/Unref       │
        │   ├─ device（裸设备 IO：O_DIRECT + libaio 异步）                 │
        │   │    └─ bufpool（4K 对齐分桶缓冲池，消除 GC 压力）            │
        │   └─ compactor（后台段压缩，60s 周期，预留段搬移）               │
        └────────────────────────────────────────────────────────────────┘
                                          │ O_DIRECT
                                          ▼
                                    NVMe 裸盘（8GiB/段）
```

数据读写路径：

- **写**：`Put` → ① `PutBegin` 从 Pebble 原子申请 4K 对齐段内偏移（段满自动滚动）；② `PutAppend` **在分配锁外** `device.Append` O_DIRECT 直写（数据 4K 对齐时零拷贝直写调用方缓冲）；③ `PutCommit` 写 key→(段,偏移,大小) 映射（先数据后元数据）。
- **读**：`GetMapping`（LRU 缓存命中即走）→ `RefSegment` 持读引用（防 GC 复用竞态）→ `device.ReadAt` 直读 bufpool 对齐缓冲（4K 对齐窗口零拷贝，非对齐窗内一次平移）→ `UnrefSegment`。
- **回收**：`Delete` 仅删映射；后台 GC 周期扫描把无在途读者的 Reclaiming 段转 Free 入池；Compaction 将高空洞 Full 段标记 Compacting 后逐个搬移存活对象，CAS（`MoveMapping`）原子切换映射并转移段存活计数。

---

## 目录结构

```
cmd/taihu/            统一 CLI（cobra 子命令树）
  └── cmd/            server / bench(storage|cluster|single) / cluster / key / instance / client / version
internal/
  ├── aio/            Linux libaio 异步 IO（io_setup/submit/getevents）；其他平台 goroutine 兜底
  ├── bufpool/        4K 对齐、2 的幂分桶缓冲池（4KB~8GB）
  ├── cluster/        TiKV 注册区：实例/客户端注册、心跳、容量记录、KV 抽象（memkv / tikv）
  ├── device/         裸设备 IO：O_DIRECT 读写、批读、IO 尺寸统计（linux / other 平台分实现）
  ├── ierr/           库错误原始定义
  ├── layout/         物理布局参数：段大小（8GiB）、4K 对齐工具
  ├── metastore/      Pebble 元数据层：mapping/state、1GiB LRU 缓存、段状态机与 GC、游标分配、CAS 搬移
  ├── rpcclient/      直连客户端：rpcConn 抽象（TCP netpoll / shmipc 共享内存，round-robin 分发）；SDK 数据面与 cmd 运维/压测的内部依赖，不对外
  ├── storage/        对象存储引擎（Storage：Put/ReadAt/BatchRead、Compactor、裸盘直写直读）
  ├── transport/      netpoll 帧协议（收发循环 / 流式多路复用）+ shmipc 服务端/客户端
  └── version/        版本信息
pkg/taihu-client/     对外唯一 SDK（集群模式：实例发现、TiKV 索引、本地优先选路、回源重建；数据面复用 internal/rpcclient）
third_party/          两份 fork 并入主模块（无嵌套 go.mod），改动清单与运维约束见 third_party/README.md
  ├── netpoll/        cloudwego/netpoll v0.7.5 fork（对齐节点分配器 + TakeTry 零拷贝移交）
  └── shmipc-go/      cloudwego/shmipc-go v0.2.0 fork（数据区 4K 对齐，O_DIRECT 直读共享内存）
scripts/              通用运维脚本：proxy agent（scripts/proxy.py / scripts/proxy_client.py）
configs/              部署参数模板（见 configs/taihu-server.example.sh）
doc/                  README.md 是全目录索引；下分 设计文档 / 性能测试报告 / 部署记录
```

目录骨架对齐 [golang-standards/project-layout](https://github.com/golang-standards/project-layout)
（`cmd/`、`internal/`、`pkg/`、`third_party/`、`scripts/`、`configs/`、`Makefile`）。
文档目录采用 `doc/` 而非标准的 `docs/`，属有意取舍：57 篇文档互相引用相对链接，改名会造成全量断链。

对外公共接口（[pkg/taihu-client/reexport.go](pkg/taihu-client/reexport.go)）：`ObjectStore` 类型（type alias，定义见 [internal/rpcclient/objectstore.go](internal/rpcclient/objectstore.go)）声明 `Put / Get / Delete / Stat / Close`，唯一实现是本包 `Storage`（集群路由）；调用方持该接口即可泛化访问 taihu 集群。

**分层约定**：`pkg/` 只放对外唯一 SDK 包 `taihu-client`；引擎（`internal/storage`）、传输（`internal/transport`）、元数据（`internal/metastore`）与直连客户端（`internal/rpcclient`）均在 `internal/`，对外不可见。业务访问**必须**走 `taihu-client`（TiKV 集群路由）；直连客户端仅供服务端与命令行内部使用。`internal/**` 不得 import `pkg/**`、SDK 不得直接依赖存储引擎，由 `make check` 的 `check-layering` / `check-sdk-only` 两条门禁守着。

---

## 快速开始

### 构建

```bash
go build -o taihu ./cmd/taihu
```

Go 1.25+。第三方依赖（netpoll、shmipc-go）使用本仓库 `third_party/` 下的 fork。两份 fork **并入了主模块**
（目录下没有 `go.mod`，import 路径为 `github.com/liucxer/taihu/third_party/...`），因此外部模块 import 本仓库
`pkg/` 时无需为 fork 另加任何 `replace` —— 这是刻意的：`replace` 只在主模块生效、不会传递给消费者，
早期用 `replace` 挂 fork 的写法会让外部消费者拿到上游版本而构建失败或静默降级。基线版本、逐文件改动清单、
许可证义务与运维约束见 [third_party/README.md](third_party/README.md)。

### 启动服务端

```bash
taihu server \
  --listen 10.0.0.1,10.0.0.2 \   # 监听 IP（逗号分隔，多网卡；RPC 端口在 [50000,51000] 自动分配）
  --db /mnt/db \                 # Pebble 元数据目录（必填）
  --dev /dev/nvme0n1 \           # 裸设备路径（必填）
  --server-name TAIHU-0 \        # 实例唯一标识（必填）
  --pd 10.0.0.10:2379            # TiKV PD 地址（集群注册/容量记录，必填）
```

启动后自动完成：设备容量读取并写入 TiKV 容量记录（换盘/容量变化拒绝启动）→ 集群注册 + 1s 心跳 → 端口自动分配（RPC / pprof / shmipc unix socket `/dev/<server-name>`）→ 后台 Compaction 启动。

### 对象读写

```bash
# 集群路由模式（--pd 定位实例）：写/读/删
echo hello | taihu --pd 10.0.0.10:2379 key put --key hello
taihu --pd 10.0.0.10:2379 key get --key hello
taihu --pd 10.0.0.10:2379 key delete --key hello

# （内部命令行的直连模式——业务请走上方集群模式；示例 TCP / 区间读 / 落盘元数据）
echo hi | taihu key put --key k1 --addr 10.0.0.1:50051
taihu key get --key k1 --off 0 --size 2 --addr 10.0.0.1:50051
taihu key meta --key k1 --addr 10.0.0.1:50051
```

### 集群健康

```bash
taihu --pd 10.0.0.10:2379 cluster list       # 实例列表（含心跳超时的僵尸）
taihu --pd 10.0.0.10:2379 cluster status     # 逐实例连通性 RTT / 段汇总 / 水位
taihu --pd 10.0.0.10:2379 instance segments --instance TAIHU-0 --detail   # 段明细
```

---

## CLI 命令一览

| 命令 | 说明 |
|------|------|
| `taihu server` | 启动对象服务端（daemon），暴露 Put/Get/Delete/Stat RPC + shmipc + pprof |
| `taihu bench storage` | 本地裸盘 Storage 层压测（不走网络，`-db/-dev` 直连） |
| `taihu bench cluster` | 集群端到端压测（走 TiKV 定位实例，`taihuclient`） |
| `taihu bench single` | 单机直通压测（不走 TiKV，`-transport rpc\|shm` 直连 server） |
| `taihu cluster list` | 列出注册区全部实例 |
| `taihu cluster status` | 逐实例连通性/段汇总/水位体检 |
| `taihu cluster index` | key→实例索引按实例归组计数 |
| `taihu cluster purge` | 预览并清空 `/taihu/` TiKV 命名空间（需 `--confirm`） |
| `taihu key put/get/delete/stat/meta/list` | 对象读写删、大小查询、落盘元数据、按前缀枚举 |
| `taihu instance segments` | 实例 segment 汇总与明细 |
| `taihu client list/info` | SDK 客户端清单与工具环境信息 |
| `taihu version` | 版本信息 |

全局参数：`--pd`（TiKV PD）、`--timeout`（默认 5s）、`--json`（机器可读输出）、`--client-name`、`--tikv-*`（TLS）。

---

## 关键设计点

### 传输协议（internal/transport）

- 自定义帧流式多路复用，替代 gRPC：`[4B len][4B streamID][1B op][payload]`，单帧负载 4 MiB，连接内 streamID 多路复用。
- 读：读循环 Peek 帧头 → Slice 整帧零拷贝子 Reader → 按 streamID 分发；写：>4K 负载零拷贝引用原缓冲，writev 散射写出。
- 错误码（`errCode`）与库错误双向映射：`ErrNotFound / ErrInvalidRange / ErrTooLarge / ErrNoSpace`。

### 共享内存 IPC（shmipc）

- unix socket 固定在 `/dev/<server-name>`，与 TCP 监听并行，默认开启。
- 数据帧布局 `[5B 帧头][4091B pad][4K 对齐数据区]`，数据区 4K 对齐供服务端 O_DIRECT **直接读入共享内存**（读路径免 bufpool→shm memcpy）。
- 客户端 `PutWriter/NewPut`（Linux）经 `Reserve` 在共享内存数据区内直接生成 payload，`-zero-copy-write` 全程零拷贝写。

### 集群（internal/cluster + pkg/taihu-client）

- 注册区（TiKV）：实例注册/心跳（1s，超时判定离线）、容量记录（换盘拒绝启动）、SDK 客户端注册保活。
- 路由：写=本地优先选实例（Picker）；读=路由缓存 → TiKV key→实例索引 → 回源重建；同机（Hostname 一致）自动走 shm，跨节点走 TCP（多地址均分建连）。
- 兼容：旧客户端忽略多地址字段，仍按 `Addr`（首 IP）直连。

### 缓存与对齐（internal/bufpool、internal/metastore/cache.go）

- bufpool：2 的幂分桶（4KB~8GB）自管理 freelist（避开 `sync.Pool` 的 GC 清空行为），保证热路径零分配、零清零。
- 元数据缓存：256 分片、全局预算 1 GiB（每片 4MB）的 LRU read-through 缓存，Pebble 为真实源，任何时刻可重建。

---

## 平台支持

| 能力 | Linux | 其他（macOS 开发/自测） |
|------|-------|-------------------------|
| 异步 IO | libaio（真异步） | goroutine + 同步 pread/pwrite 兜底 |
| 文件打开 | O_DIRECT（4K 对齐约束） | 普通打开 |
| shmipc 零拷贝写 | 支持（PutWriter/Reserve） | 回退普通 Put（ErrShmOnly） |

---

## 相关文档

- **[doc/README.md](doc/README.md) —— 全部 57 篇文档的索引与状态标注**：10 篇设计文档 +
  46 篇性能测试报告（按主题分了系列，每系列标明「现行结论看哪一篇」）+ 1 篇部署记录。
  逐篇标注【现行】/【部分被取代】/【历史存档】，并注明标注所依据的原文。

先看这几篇就够：

- [taihu 项目架构设计文档](doc/设计文档/20260914_taihu项目架构设计文档.md) —— 【现行·总纲】当前实现的总体架构
- [设计文档 v2](doc/设计文档/202609091204_设计文档_v2.md) —— 【现行·存储核心】存储层设计（v3 及后续均沿用；权威版本）
- [结构评审与优化建议](doc/设计文档/20260914_结构评审与优化建议.md) —— 结构评审 8 项及落实记录（§6）

历史存档：`设计文档 v1`（35 行原始需求草稿）、`设计文档 v3`（远程访问层已由 netpoll 取代 gRPC，见架构文档）、
`20260911_6c5297e_segment级GC设计与测试方案`（GC 引入时的方案，现行版见 `segment级GC设计文档`）。