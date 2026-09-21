# taihu(Go) 与 nefs/taihu core(C++)、SDK(C++) 架构对比

> 日期：2026-09-15
> 背景：当前在做**分布式缓存**，定位为**不需要高可用、不需要数据可靠性**。本文梳理当前 Go 工程架构，并与 nefs/taihu 的 C++ core 与 C++ SDK 两个参考工程做横向对比，给出针对该场景的架构定位结论与可借鉴/可裁剪点。

## 0. 参考工程

| 工程 | 路径 | 语言/构建 | 定位 |
|---|---|---|---|
| 当前工程 | `github.com/liucxer/taihu` | Go / Makefile | 分布式对象存储 Go 一体化实现（server + client + bench 同二进制） |
| core | `gitlab.cmss.com/SDS/nefs/taihu/core` | C++11 / CMake | ESSD 分布式块存储核心：blockfs（块语义）+ megrez（强一致多副本 append 底座） |
| SDK | `gitlab.cmss.com/SDS/nefs/taihu/SDK` | C++11 / CMake（SPDK/DPDK/brpc） | taihu 分布式客户端库：文件语义（追加写+读取），krpc 走网络 RPC |

---

## 1. 当前工程（Go）架构梳理

### 1.1 模块结构

```
taihu/ (Go)
├── cmd/taihu/                唯一入口二进制，cobra 子命令树（server/bench/cluster/key/instance/client/version）
├── internal/
│   ├── aio/                  异步盘 IO：libaio / io_uring 双后端
│   ├── bufpool/              4K 对齐缓冲池（2^n 分桶 4KB~8GB），消除热路径大块分配
│   ├── cluster/              集群注册/索引 KV（内存/TiKV 两实现）+ 实例注册心跳
│   ├── device/               O_DIRECT 裸设备：段 append/read、批 IO、ReadAtInto
│   ├── ierr/                 错误定义（ErrNotFound 等）
│   ├── layout/               物理布局：段 8GiB、4K 对齐工具
│   ├── metastore/            元数据（Pebble 持久化 + 256 分片 LRU 缓存）+ 段管理/分配器
│   ├── storage/              对象存储层 + Compactor（段级回收/搬移）
│   ├── transport/            netpoll 帧协议传输层（TCP + shmipc 双数据面）
│   ├── rpcclient/            直连客户端（内部，满足 ObjectStore 接口）
│   ├── benchkit/             压测工具
│   └── version/
├── pkg/taihu-client/         唯一对外 SDK：集群客户端（TiKV 路由）
├── third_party/              netpoll、shmipc-go 本地 fork
└── examples/ doc/ configs/ scripts/ dist/
```

- `taihu-server / taihu-bench / taihu-shm-bench` 并非独立二进制，均为 `taihu` 单 binary 的子命令（`cmd/taihu/cmd/root.go`）。

### 1.2 对外 SDK 形态（pkg/taihu-client）

入口 `NewCluster(cfg)` / `NewFromTiKV(opts)`，接口 `ObjectStore`（声明于 `internal/rpcclient/objectstore.go`）：

```go
Put(ctx, key, size, in) / Get(ctx, key, off, size) ([]byte, release, error) / Delete(ctx, key) / Stat(ctx, key) / Close()
```

选项包含：回源 SourceGetter、写路由（本地优先/轮询）、传输方式（auto/rpc/shm 零拷贝）。

### 1.3 IO 链路架构（写流程 / 读流程）

传输层双数据面（TCP netpoll + shmipc 共享内存）共用同一帧协议 `[4B len][4B streamID][1B op]`，客户端按同机/跨节点自动切换（cfg.transport: auto/rpc/shm）。

#### 1.3.1 写（Put）完整调用链

```
Storage.Put (pkg/taihu-client/storage.go:181)
 ├─ picker.pick() 选实例 (picker.go:34)
 │    RouteLocal(默认): 同机健康→远端健康→同机随机→全局随机（按 usagePercent 水位）
 │    RouteRoundRobin: 原子游标轮询
 ├─ clientFor(inst) 懒建/复用连接 (storage.go:117)
 │    auto: 同机→DialShmPool(shm)；跨节点→DialPoolMulti(TCP 多地址 rr)
 ├─ rpcclient.Storage.Put (internal/rpcclient/storage_rpc.go:48)
 │    ├─ TCP: transport.Conn.Put (client.go:71)
 │    │   newStream(streamID 复用) → 首帧 OpPutHeader(key+size)
 │    │   → 按 ChunkSize=4MiB 切 OpPutData（netpoll 分帧零拷贝引用 in）→ OpPutEnd → await OpResp
 │    └─ shm: ShmConn.Put (client_shm_linux.go:77)
 │        PutBegin → ShmPutWriter.Reserve 返回共享内存 4K 对齐可写区（零拷贝直写）
 │        → 写满 4MiB 自动切帧(帧头[4B len][1B op]) → Commit flush 末帧 + OpPutEnd → 等 OpResp
 → server.dispatch → handlePut (internal/transport/server.go:169)
     首帧 OpPutHeader 开流 → 校验大小
     → 立即 PutBegin（收数据前串行分配段游标）
     → 循环 OpPutData：TCP → bufpool.Get(size) 4K 对齐池缓冲汇入(1 次拷贝)；
                      shm → 直引共享内存攒 WriteJob(0 拷贝，提交后统一 Release)
     → OpPutEnd：写流水线 submit(batch.go:98：AppendBatch+BatchPutCommit) 或直调 storage.Put
     → 写 OpResp
 → storage.Put (internal/storage/storage.go:83) = PutBegin + PutAppend + PutCommit
     → PutBegin → db.AllocateSegment (kv_pebble.go:450)
         allocator 持独立 a.mu (kv_pebble.go:432)：加锁只覆盖廉价游标分配(align4k、段滚动/popFree)
         a.persist 用 syncWO(Sync:true) 持久化游标；设备写在其锁外执行 → 多 Put 并发写不同偏移
     → PutAppend → dev.Append (device.go:269)
     → PutCommit → db.PutMapping → segs.putObject (segments.go:207)
         mapping + 段存活计数同一 pebble.Batch.Apply(syncWO) 原子落盘
         顺序保证：先写设备数据、再写元数据 (storage.go:118-119)
 → Device.Append (device.go:269)，fd=O_RDWR|O_DIRECT 无 O_SYNC (device_linux.go:12)
     4K 对齐快路径(bufAligned)：主体 bulkEnd 零拷贝直写调用方缓冲，仅 <4K 尾块 bufpool 4K 补零
     非对齐兜底：copy 进 bufpool.Get(aligned) 两段写
     异步：aio.Ring.io_submit 批提交 (aio_linux.go:136) → 单 pump goroutine 串行 Wait(io_getevents) 分发 (device.go:142)
     → submitOp 阻塞至完成事件并校验 res = 内核已提交（无 O_SYNC，需显式 Sync 才持久化）
```

**写同步点**：设备写回 + Pebble 映射 Sync 都完成后服务端才写 OpResp；客户端 await OpResp 才返回。TiKV key→实例注册是客户端异步尽力而为（storage.go:193），索引失败不阻塞 Put 返回。

**写路径逐跳：缓冲 / 拷贝数 / 对齐**

| 环节 | 缓冲来源 | 拷贝 | 对齐 | 归属 |
|---|---|---|---|---|
| 客户端 in | 调用方 | 0（TCP/shm 均零拷贝分帧） | — | 调用方 |
| TCP 服务端汇入 | bufpool.Get(size) 池缓冲 | 1 | 4K | 服务端 |
| shm 服务端 | 直引共享内存 WriteJob | 0 | 4K | 共享内存 |
| 设备写 | 对齐主体直写调用方缓冲；尾块/非对齐走 bufpool | 对齐 0；非对齐 1 | 4K 起址+off、Align4k 补零 | 服务端 |
| 完成回执 | — | — | — | 阻塞等 io_getevents |

#### 1.3.2 读（Get）完整调用链

```
Storage.Get (pkg/taihu-client/storage.go:201)
 ├─ lookup (storage.go:258)：route_cache(无锁 4096) hit? → miss → indexManager.get (index.go:87)
 │    查 TiKV /taihu/index/{key} (internal/cluster/kv.go:28) → registry.lookup(name) 得实例 → 回填 route_cache
 │    完全 miss → getFromSource (storage.go:277)：cfg.Source 回源拉整对象截取 [off,off+size) 并回写缓存
 ├─ getFrom → clientFor：同机 shm(DialShmPool) / 跨节点 TCP(DialPoolMulti)
 ├─ rpcclient.Storage.Get (storage_rpc.go:57) → transport.Conn.Get (client.go:121)
 │    发 OpGetReq(key/off/size)（size=-1 先 Stat 定长）
 │    服务端按 ChunkSize=4MiB 分块回 OpGetData + 末帧 OpGetDataFinal
 │    收帧路径：
 │      整响应恰一帧 → TakeTry() 零拷贝移交 netpoll 收缓冲 (client.go:187)；release→bufpool.PutExact
 │      多帧/回退 → bufpool.Get(size) 对齐缓冲逐帧 ReadCopy 恰 1 次拷贝；release→bufpool.Put
 → server.handleGet (internal/transport/server.go:260)
     循环按 chunk 调 storage.ReadAt → writeFrame 零拷贝引用返回缓冲 (frame.go:184)
     → WriteBinary(writev) + Flush 排空 → 发送完 bufpool.Put(data) 归还 (server.go:299)
 → storage.ReadAt (internal/storage/storage.go:219)
     → db.GetMapping：先 256-shard LRU cache.get (cache.go:49) hit? 否则 pebble Get + cache.put 回填（read-through）
     → RefSegment/UnrefSegment 防在途段被 GC 回收 (kv_pebble.go:290-296)
     → 物理读区间 4K 下/上对齐 (storage.go:237)，非对齐窗口池内原址左移 1 次
 → device.ReadAt (device.go:482)
     bufpool.Get 对齐缓冲 → submitRead → submitOp 阻塞至 pump goroutine 取回完成事件
     O_DIRECT (device_linux.go:12)，读方用毕必须 bufpool.Put (device.go:481)
   同机 shm 变体：device.ReadAtInto (device.go:517) 把磁盘 O_DIRECT 直读进共享内存切片
    （server_shm_linux.go:544 Reserve → :564 ReadAtInto 直写 buf[ShmDataPad:]，免 bufpool→shm 的 memcpy；
      非对齐 off 回退 ReadAt+拷贝）
```

**读同步点**：全部同步。客户端阻塞至最后一帧 `pos==size`（short read 校验，client.go:232）。跨节点 vs 同机差异：跨节点 = 磁盘读→bufpool→writev→TCP→客户端汇入/零拷贝移交；同机 shm = 磁盘 O_DIRECT 直读进共享内存→客户端直接引用共享内存返回（**双向免 memcpy**）。

**读路径逐跳：缓冲 / 拷贝数**

| 环节 | 拷贝 | 缓冲来源 / 归还 |
|---|---|---|
| TCP 客户端收帧（整响应恰一帧） | 0 | netpoll 收缓冲；release→bufpool.PutExact |
| TCP 客户端收帧（多帧/回退） | 1 | bufpool.Get；release→bufpool.Put |
| shm 客户端接收（单帧） | 0 | 直引共享内存；release 释放 pin+PutBack |
| 服务端磁盘读 ReadAt | 0~1（对齐时 0） | device 内 bufpool；handleGet 送完 bufpool.Put |
| 服务端 TCP 发送 | 0 | writev 引用 ReadAt 缓冲，Flush 后 Put |
| 服务端 shm 发送 | 0 | ReadAtInto DMA 直进共享内存 |

#### 1.3.3 IO 链路关键设计点小结

1. **双数据面共享同一帧协议**：同机走 shmipc（零拷贝），跨节点走 netpoll TCP，配置 auto 自动切换。
2. **对齐即性能**：O_DIRECT 要求 4K 对齐；bufpool 保证全链路缓冲 4K 对齐，对齐后主体可零拷贝直写/直读（shm 甚至 DMA 直进共享内存）。
3. **异步盘 IO + 单 pump**：libaio/io_uring 批提交（io_submit），单 goroutine 串行收割（io_getevents），语义同步、内核异步。
4. **写先数据后元数据**：设备写回成功后 mapping 才写入（pebble batch + Sync 原子落盘），保证崩溃一致性顺序。
5. **无 O_SYNC**：数据不经页缓存直接落设备（等效同步刷盘），强落盘由显式 Sync 控制。

### 1.4 核心设计点

- **段（segment）**：整盘 8GiB 分段，游标顺序写、滚动；段状态机 `Free→Active→Full→Reclaiming→Free`；删除仅失效映射，物理回收靠 Compaction/GC。
- **O_DIRECT**：4K 对齐读裸盘，首块对齐时零拷贝直写；aio 异步提交 + 单 pump goroutine 分发。
- **256 shards LRU 缓存**：有界 1GB，缓存 Pebble 映射，read-through，崩溃可重建。
- **Compaction**：低频搬移高空洞段存活对象（CAS MoveMapping 防并发），配合 GC 复用空间。
- **TiKV 元数据路由**：命名空间 `/taihu/instances/`、`/taihu/index/`、`/taihu/clients/`；心跳注册、读写路由、客户端清单。TiKV 不可用时降级为"本地实例+回源"。
- **shmipc 零拷贝**：共享内存数据面，数据帧内嵌 4K padding 使服务端可 O_DIRECT 直读共享内存。
- **帧协议**：`[4B len][4B streamID][1B op]`，多 stream 多路复用，主块 4MiB。

关键路径：
- SDK：`pkg/taihu-client/storage.go`
- 存储层：`internal/storage/storage.go`
- 裸设备：`internal/device/device.go`
- 元数据+段+缓存：`internal/metastore/{store,segments,cache}.go`
- 帧协议：`internal/transport/protocol/protocol.go`
- 集群路由：`internal/cluster/kv.go`

---

## 2. core（C++）架构梳理

### 2.1 分层

（CMake 工程名 ESSD，版本 ESSD 3.1.6 / TAIHU 3.1.1）

```
core/src
├── proto/              protobuf 服务定义（blockfs/megrez/volume/xmem/nameserver）
├── base/               dbapi/fileop/metric/misc/rpc 基础设施（krpc 封装）
├── blockfs/            块语义数据面
│   ├── blockserver/    索引转发层（读改写、plane 调度、rpc/task worker）
│   ├── blockmaster/    控制面（元数据/租约下发）
│   ├── blocklink/      SDK 侧挂载/读写入口
│   ├── compactionserver/ 回收/压缩
│   ├── migrationserver/  迁移
│   └── backupserver/   备份
├── megrez/             存储底座（append 语义、多副本/EC）
│   ├── streamsvr/      元数据（StreamServer，etcd/braft/RocksDB KV）
│   ├── extentsvr/      Extent 数据服务（SPDK/libaio 落盘）
│   ├── nameserver/     名字服务
│   └── metaportal/     元数据门户
├── volume/volumemanager/ 卷生命周期门面
├── ece/                xmem 内存加速（含分片 LRU 缓存、swapmgr）
└── tools/              essdcli/deployer 等
```

### 2.2 关键外部组件

| 组件 | 用途 |
|---|---|
| krpc | RPC 框架（TCP/RDMA，零拷贝 Attachment，EventWorker/PollingWorker） |
| RocksDB | extent 元数据索引落盘（`megrez/streamsvr/index/persist/kvdb`） |
| etcd | 集群注册/路由发现（`megrez/common/etcd`、`volumemanager/kvstorage/cluster_etcd`） |
| braft/brpc | raft 强一致 |
| SPDK+DPDK | NVMe 用户态零拷贝 IO |
| isa-l | EC 编解码 |
| 其他 | libaio/liburing、rdma、mongocxx、zstd/snappy/lz4 |

注意：core 无 TiKV 引用，路由依赖 etcd；TiKV 集成是上层 Go 工程（pkg/taihu-client）的职责。

### 2.3 数据路径（写）

驱动 → BlockLink → blockserver Write → Recv Worker（限速/校验）→ Task Worker → Plane 查索引分片 → krpc 转发 megrez StreamServer（分配 extent）→ ExtentServer 落盘（SPDK/AIO）。

### 2.4 缓存设计

两级：
- **xmem 内存加速**（`src/ece/xmem/cache/cache.{h,cc}`）：基于 LBA 的 LRUCachePolicy + Cache/CacheManager/CacheMemoryPool，分片 + LRU 换出，支持 **Mem+TAIHU / Mem+Disk** 两种落盘模式。
- 数据面另有 block 内存池（CacheManager，引用计数复用）与迁移缓存。

关键路径：`core/src/base/rpc/rpc_base.h`、`core/src/ece/xmem/cache/cache.h`、`core/src/megrez/streamsvr/index/persist/kvdb.h`

---

## 3. SDK（C++）架构梳理

> 注意：工程里的 `go.mod`/`main.go` 是 GoLand 脚手架残留，**实际是纯 C++11 工程**（CMake 构建，产出 taihu_sdk 库）。

### 3.1 结构

```
SDK/
├── include/taihu/sdk/   对外公共头（client/controller/status/types）
├── src/                 实现（client/controller/task/handle/node_manager/rpc_engine/blacklist/erasurecode/cache…）
├── mock/ test/          mock 后端、cli/demo/fio_plugin/iotest/unittest
└── common/              git 子模块（未 checkout）
```

### 3.2 对外 API（文件语义，非 KV）

`taihu::sdk::Client::CreateClient(ClientOption)`，异步回调 + Controller 上下文（超时/零拷贝/优先级）：
- 文件：CreateFile/OpenFile/CloseFile/DeleteFile/MoveFile/SealFile/StatFile
- **追加写**：AppendFile/AppendVFile（返回写后 offset）、ReadFile/ReadVFile、FlushFile/SyncFile；单次 IO ≤ 256K
- Inline 小文件、目录（ListDirectory 分页）、StatCluster

### 3.3 路由与容错

- krpc 连接三类后端：元数据→StreamServer，数据 IO→ExtentServer，集群信息→MetaPortal；etcd 发现集群。
- NodeManager 维护节点表（Healthy/Suspected/Unhealthy）定时刷新路由；blacklist 故障黑名单。
- 缓存：`src/cache.h` LRU 4K 页缓存，`enable_cache` 开启，用于 EC 降级读缓存。

关键路径：`SDK/include/taihu/sdk/client.h`、`SDK/src/node_manager.h`、`SDK/src/rpc_engine.h`、`SDK/src/cache.h`

---

## 4. 三工程横向对比

### 4.1 总体定位

| 维度 | 当前工程（Go） | core（C++） | SDK（C++） |
|---|---|---|---|
| 形态 | 一体化二进制（server+client+bench） | 服务端多组件 | 纯客户端库 |
| 语义 | ObjectStore KV（Put/Get/Delete/Stat/Close） | 块语义 + append 存储底座 | 文件语义（追加写） |
| 目标 | 单副本高性能对象存储 | 企业级强一致多副本块存储 | 客户端访问 |
| 是否含存储面 | 是（device/metastore/storage） | 是 | 否（纯 RPC 客户端） |

### 4.2 元数据与路由

| 维度 | 当前工程 | core | SDK |
|---|---|---|---|
| 本地元数据 | Pebble（LSM） | RocksDB | 无（仅 4K 页缓存） |
| 集群路由 | TiKV（/taihu/ 命名空间），可降级本地+回源 | etcd | etcd 发现 + NodeManager 本地路由表 |
| 路由粒度 | key→实例索引（对象级） | 卷→extent（块级） | 节点表（服务级） |
| 映射 | 单层 key→(segment, offset) | 索引（BlobIndex/PageIndex/LSM） | —— |

### 4.3 一致性 / 高可用 / 可靠性

| 维度 | 当前工程 | core |
|---|---|---|
| 副本 | 单副本 | braft 多副本 + EC（isa-l） |
| 一致性 | 无强一致（写直落盘即返回） | raft 强一致 |
| 租约/心跳 | 实例心跳注册（仅路由用途） | blockmaster 租约下发 |
| 故障转移 | TiKV 降级 + 回源 | StreamServer 迁移 / 多副本切换 |
| 可靠性代价 | 极低 | 极高（raft/EC/迁移/备份全栈） |

### 4.4 传输与零拷贝

| 维度 | 当前工程 | core | SDK |
|---|---|---|---|
| RPC | netpoll 帧协议 `[len][sid][op]`（TCP） | krpc（TCP/RDMA） | krpc |
| 零拷贝 | shmipc 共享内存 + O_DIRECT 直读、bufpool、多级 Take/Reserve | krpc Attachment 零拷贝、SPDK | Controller 零拷贝缓冲 |
| 限制 | 帧 4MiB / 消息 8MiB | RPC 分片 256K（SDK）/krpc | kRpcMaxSize 256K |

### 4.5 缓存设计（重点）

| 维度 | 当前工程 | core xmem | SDK |
|---|---|---|---|
| 缓存对象 | 元数据映射（key→段位置） | LBA 数据块 | EC 降级读数据页 |
| 结构 | 256 shards / LRU / 1GB 有界 / read-through | 分片 Cache + LRUCachePolicy + CacheMemoryPool | LRU 4K 页 |
| 落盘模式 | 无（崩溃重建映射） | Mem+TAIHU / Mem+Disk | 无 |
| 位置 | 服务端内 | 服务端 | 客户端 |

### 4.6 落盘与 IO

| 维度 | 当前工程 | core |
|---|---|---|
| 直写 | O_DIRECT（写 fd 无 O_SYNC，等效同步刷盘） | SPDK 用户态 / libaio |
| 异步 | libaio / io_uring（Go 封装） | libaio、rdma |
| 对齐 | 4K（bufpool 保障） | 4K/SPDK |

### 4.7 可观测与运维

| 维度 | 当前工程 | core |
|---|---|---|
| 控制面 | cobra 子命令 + pprof（自动端口） | essdcli / deployer / volumemanager |
| 指标 | 测试报告体系（doc/性能测试报告） | metrics/meter + 文档体系 |

### 4.8 IO 流程逐跳对比（Go vs core）

| 阶段 | 当前工程（Go） | core（C++） |
|---|---|---|
| 客户端入口 | ObjectStore.Put/Get（KV 语义） | 文件/块语义：driver → BlockLink |
| 头跳 | 客户端选实例后**直连目标实例**（单跳 RPC） | BlockServer 入口 → Recv Worker（限速/校验）→ 多级转发 |
| IO 编排 | 无队列依赖（可选写流水线 batch） | Recv/Task/RPC 三类 worker + pending 队列 + RotateWriteTarget 状态机 + op_ref 在途计数 |
| 元数据分配 | 服务端本地 allocator 分配段游标（a.mu 廉价加锁） | StreamServer 独立服务管理 extent 拓扑（RPC 查询/分配） |
| 数据发送 | 4MiB chunk 帧（TCP 零拷贝分帧 / shm 直写共享内存） | krpc Attachment 零拷贝（SDK 单次 IO ≤ 256K） |
| 落盘 | libaio/io_uring + O_DIRECT 4K，单 pump goroutine | SPDK 用户态 DMA 直写 / libaio，io_worker_colony_ 多 worker |
| 一致性代价 | 无强一致：先盘后映射（单副本） | braft 多副本/EC + CRC32c + Hedged Read + 副本切换 |
| 结论 | 单跳直连 + 本地分配 + 异步直落，**链短、编排轻** | 多级转发 + 分片调度 + 存储底座，重在高可用编排 |

---

## 5. 针对"分布式缓存（无高可用 / 无数据可靠性）"的定位结论

### 5.1 当前工程与目标场景的匹配度：高度契合

- 当前工程**天然就是"无高可用、无可靠性"的最简形态**：单副本、无 raft、无 EC、无租约、无迁移，写路径 O_DIRECT 直落盘。
- 对比 core 的 braft/EC/租约/迁移/备份等强一致全栈设施，当前工程已全部省略，只保留"路由 + 本地元数据 + 裸设备写入"，恰好是分布式缓存所需的骨架。

### 5.2 可进一步裁剪点（缓存场景不需要的）

| 能力 | 现状 | 缓存场景结论 |
|---|---|---|
| Compaction / 段级 GC | 现有（60s 周期、按空洞率搬移） | 若缓存允许"删了就不管"，可关闭或延长周期；若需回收空间仍需保留 |
| 实例心跳注册 | 现有（供路由） | 可保留，但间隔可放宽（缓存场景容忍路由陈旧） |
| TiKV 路由 | 现有 | 保持为可降级能力：单机部署时退化为本地（集群模式已如此设计） |
| 回源 SourceGetter | 现有 | 缓存场景下默认可关闭 |
| 元数据持久化（Pebble） | 现有 | 缓存场景可考虑纯内存 metastore（映射可重建），省掉落盘；但需权衡重建代价 |

### 5.3 可借鉴项

1. **core xmem 的两级缓存架构**（Mem+TAIHU/Mem+Disk 落盘模式）：当前工程只有元数据缓存，没有数据缓存。做分布式缓存时可借鉴其"分片 LRU + 内存/盘两级换出"设计，把当前 256 shards LRU 从"元数据缓存"推广为"数据对象缓存"。
2. **SDK 的客户端路由容错**（NodeManager 状态机 + 黑名单）：当前客户端写路由（本地优先/轮询）可叠加节点健康态与降级策略，缓存场景对路由陈旧容忍度高，实现成本低。
3. **core 的批 IO / 分片调度**：当前 device 已用 aio 异步 + 单 pump goroutine；若缓存读放大显著，可参考 xmem 的按 LBA 分片调度。
4. **对齐与零拷贝链**：当前工程（bufpool/shmipc/O_DIRECT/帧协议）与该目标场景完全匹配，无需回退。

5. **IO 链路（本章重点）**：写（客户端直连单跳 → 本地段游标分配 → O_DIRECT 异步直落 → 先数据后映射）与读（路由缓存 → TiKV 定位 → 目标实例 O_DIRECT 直读 + writev/shm 零拷贝交付）当前已是最简可用形态，链路短、编排轻，完全匹配"无高可用、无可靠性"的缓存场景——单副本直接写盘返回，无可复制、无强一致协议停留；同机 shm 路径的 DMA 直进共享内存对缓存读热点尤其有利。需要简化的仍是数据面之外的元数据（Pebble 落盘可改纯内存映射）与 TiKV 定位（单机可退化本地），IO 链本身无冗余可减。

### 5.4 结论

以"分布式缓存、无高可用、无可靠性"为定位，**当前 Go 工程已是最优起点**：
- 分层干净（SDK → transport → storage → metastore/device），语义已是 KV（Put/Get/Delete/Stat），无需转换为块/文件语义；
- 与 core（企业级块存储）相比，所有高可用/可靠性设施均未引入，架构复杂度低一个量级；
- 唯一显著的"不利项"是元数据（Pebble 落盘 + TiKV 路由）仍带持久化/发现负担，缓存场景下可做"纯内存映射 + 可降级路由"的轻量化改造；
- 需要真正新增的，是**数据面缓存**（借鉴 xmem 分片 LRU + 两级换出），这是当前工程与"缓存"定位的主要差距。