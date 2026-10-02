# taihu SDK API 接口文档

> 唯一对外 SDK 包：`github.com/liucxer/taihu/pkg/taihu-client`（包名 `taihuclient`）。
> 外部业务**不能**直连某个 taihu server 实例，必须经注册中心（TiKV）路由访问集群。

本文档是 taihu SDK 的权威接口说明，覆盖：构造方式、配置项、数据面 API、运维/观测 API、
内部路由机制、对外类型与错误面。源码权威源为 [`pkg/taihu-client/`](../../pkg/taihu-client/)。

---

## 1. 概览与定位

taihu 是一个**集群对象存储**，SDK 提供「首写本地 + 索引锚定 + 回源兜底」的缓存场景能力：

| 维度 | 说明 |
|---|---|
| 写路由 | 本地优先（同机 Hostname 一致 → 共享内存）→ 远端兜底；可选 round-robin |
| 读定位 | 路由缓存 → TiKV 索引定位实例 →（全 miss）回源回调拉取并回写 |
| 传输 | 同机共享内存（shmipc 零拷贝） / 跨节点 TCP（netpoll 帧协议），自动选择 |
| 定位层 | Registry / Picker / Index / routeCache **不进入数据面热路径**，数据面复用 `internal/rpcclient` |
| 降级 | TiKV 不可用时退化为「本地实例 + 回源」，功能不中断 |
| 注册/索引后端 | 经 `cluster.KV` 接口注入（内存 KV / TiKV TxnKV） |

**核心约束（设计文档）**：
- `key` 不可变（无覆盖写）；更新语义 = `Delete` 后重建。
- 缓存可丢——索引尽力而为，读 miss 由回源兜底。
- `ClientID` 为必填：SDK 必须带唯一注册 ID 以参与心跳与 web「客户端清单」识别。

---

## 2. 快速开始

```go
import (
    "context"
    "errors"
    "fmt"
    "os"
    "strconv"
    "strings"

    taihuclient "github.com/liucxer/taihu/pkg/taihu-client"
)

func main() {
    ctx := context.Background()
    host, _ := os.Hostname()

    // 1. 构建集群客户端（推荐 NewFromTiKV）
    cli, err := taihuclient.NewFromTiKV(ctx, taihuclient.TiKVOptions{
        PDAddrs:    strings.Split(os.Getenv("TAIHU_PD"), ","), // 如 "10.0.0.11:2379,10.0.0.12:2379"
        ClientName: "billing-app",                              // 仅标注/注册用
        ClientID:   "billing-app+" + host + "+" + strconv.Itoa(os.Getpid()), // 必填，全局唯一
    })
    if err != nil {
        panic(err)
    }
    defer cli.Close() // 注销 SDK 客户端 + 停止后台心跳

    // 2. 尽早暴露配置错误
    if err := cli.CheckPoolIsValid(); err != nil {
        panic("集群无在线实例: " + err.Error())
    }

    // 3. 写对象（key 不可变，无覆盖写）
    key := "orders/2026-09-14/001"
    data := []byte(`{"order_id":1,"amount":99.9}`)
    if err := cli.Put(ctx, key, int64(len(data)), data); err != nil {
        panic(err)
    }

    // 4. 读对象：返回 (data, release, err)，用毕必须 release()
    out, release, err := cli.Get(ctx, key, 0, -1) // size=-1 读至结尾
    if err != nil {
        panic(err)
    }
    defer release()
    fmt.Println(string(out))

    // 5. 错误判定用 errors.Is
    if err := cli.Delete(ctx, key); err != nil {
        panic(err)
    }
    _, _, err = cli.Get(ctx, key, 0, -1)
    if errors.Is(err, taihuclient.ErrNotFound) {
        fmt.Println("已删除")
    }
}
```

完整示例见 [`examples/taihu-client/main.go`](../../examples/taihu-client/main.go)。

---

## 3. 术语表

| 术语 | 含义 |
|---|---|
| **实例 (Instance)** | 一个 `taihu server` 进程，注册到 TiKV 注册区 |
| **同机 (local)** | SDK 进程 `os.Hostname()` 与实例注册的 `Hostname` 一致 → 走共享内存 |
| **跨节点 (remote)** | Hostname 不一致 → 走 TCP |
| **索引 (Index)** | key→实例名 的 KV 映射（存于 TiKV `IndexKeyPrefix`），异步批量写 |
| **路由缓存 (routeCache)** | 本地内存 key→实例名 LRU 缓存，避免每读打 TiKV |
| **回源 (Source)** | 集群全 miss 时，调用方提供的 `SourceGetter` 拉取整对象并回写缓存 |
| **水位 (Usage)** | 实例 `Used / Capacity`，超过阈值则写路径跳过该实例 |

---

## 4. 构造方式

### 4.1 `NewFromTiKV`（推荐）

连接 TiKV TxnKV 并构建集群客户端。返回的 `*Storage` 已启动实例发现/索引后台任务，
用毕**必须**调用 `Close()`。

```go
func NewFromTiKV(ctx context.Context, opts TiKVOptions) (*Storage, error)
```

| 返回错误 | 触发条件 |
|---|---|
| `taihuclient: TiKVOptions.PDAddrs is required` | `opts.PDAddrs` 为空 |
| TiKV 拨号错误 | PD 不可达 / TLS 握手失败 |
| `ErrClientIDRequired` | `opts.ClientID` 为空 |

### 4.2 `NewCluster`（自定义 KV 后端）

注入已构造好的 `cluster.KV`（如内存 KV 用于测试、或共享已有的 TiKV 客户端）。

```go
func NewCluster(cfg ClusterConfig) (*Storage, error)
```

| 返回错误 | 触发条件 |
|---|---|
| `ErrKVRequired` | `cfg.KV == nil` |
| `ErrClientIDRequired` | `cfg.ClientID == ""` |

---

## 5. 配置项

### 5.1 `TiKVOptions`

`NewFromTiKV` 的入参。字段与 `ClusterConfig` 一一对应，外加 TiKV 连接参数。

| 字段 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `PDAddrs` | `[]string` | ✅ | — | TiKV PD 地址，如 `["host:2379"]` |
| `CA` / `Cert` / `Key` | `string` | ❌ | 空 | TLS 三件套路径；全空=明文 |
| `ClientID` | `string` | ✅ | — | SDK 客户端唯一注册 ID |
| `ClientName` | `string` | ❌ | — | 客户端标识（仅标注/注册用，不参与路由） |
| `ClientAddr` | `string` | ❌ | — | SDK 数据面地址（随心跳上报） |
| `ClientLabels` | `map[string]string` | ❌ | — | 自定义标签（随心跳上报） |
| `RefreshInterval` | `time.Duration` | ❌ | `1s` | 实例发现刷新周期 |
| `HeartbeatTimeout` | `time.Duration` | ❌ | `5s` | 实例离线判定超时 |
| `UsageThreshold` | `float64` | ❌ | `80` | 选实例水位阈值百分比（0-100），超过则跳过 |
| `WriteRouting` | `string` | ❌ | `"local"` | 写路由算法，见 §5.3 |
| `Conns` | `int` | ❌ | `1` | 每地址数据面连接数 |
| `Transport` | `string` | ❌ | `"auto"` | 传输方式，见 §5.4 |
| `Source` | `SourceGetter` | ❌ | `nil` | 回源回调 |

### 5.2 `ClusterConfig`

`NewCluster` 的入参。与 `TiKVOptions` 等价（后者内部转为此结构），但 `KV` 字段为已构造的 KV 客户端。

```go
type ClusterConfig struct {
    KV               cluster.KV        // 必填
    ClientID         string            // 必填
    ClientName       string
    ClientAddr       string
    ClientLabels     map[string]string
    RefreshInterval  time.Duration
    HeartbeatTimeout time.Duration
    UsageThreshold   float64
    WriteRouting     string
    Conns            int
    Transport        string
    Source           SourceGetter
}
```

### 5.3 写路由算法常量

定义于 [`config.go`](../../pkg/taihu-client/config.go#L21-L28)：

| 常量 | 值 | 语义 |
|---|---|---|
| `RouteLocal` | `"local"` | **默认**。本地优先：同机实例（Hostname 一致）→ 远端兜底；每档内只挑水位未超阈值的实例，全超时整档随机兜底 |
| `RouteRoundRobin` | `"round-robin"` | 所有在线实例轮询（含跨节点 TCP）；单实例超水位跳过该次，全满则兜底返回游标处实例 |

### 5.4 传输方式常量

定义于 [`config.go`](../../pkg/taihu-client/config.go#L31-L38)：

| 常量 | 值 | 语义 |
|---|---|---|
| `TransportAuto` | `"auto"` | **默认**。同机走共享内存 shm，跨节点走 TCP |
| `TransportRPC` | `"rpc"` | 强制 TCP，含同机实例（用于测同机 RPC 性能 / 网络路径正确性） |
| `TransportShm` | `"shm"` | 强制共享内存，仅同机实例可用（需实例注册 `ShmAddr`） |

> **同机判定**：SDK 比较本机 `os.Hostname()` 与服务端注册的 `InstanceInfo.Hostname`，一致则视为同机。

### 5.5 `SourceGetter`（回源回调）

```go
type SourceGetter func(ctx context.Context, key string) ([]byte, error)
```

集群内全部 miss 时调用，返回整对象数据。SDK 按 `[off, size)` 截取后返回调用方，
并**本地优先回写缓存**（`Put` 失败不阻塞读）。实现方负责源侧错误语义。

---

## 6. 数据面 API

所有方法挂在 `*Storage` 上。`Storage` 满足 `rpcclient.ObjectStore` 接口
（编译期断言见 [`storage.go:50`](../../pkg/taihu-client/storage.go#L50)）。

### 6.1 `Put` — 写入对象

```go
func (s *Storage) Put(ctx context.Context, key string, size int64, in []byte) error
```

- `size` 为逻辑大小，实际取 `in[:size]`。
- **key 不可变**（无覆盖写）；同一 key 重复 Put 行为未定义，更新请 `Delete` 后重建。
- 流程：Picker 选实例 → 数据面写入 → 异步写索引 + 路由缓存。

| 错误 | 说明 |
|---|---|
| `ErrNoInstances` | 集群无在线实例 |
| `ErrTooLarge` | 对象超过单 segment 上限 |
| `ErrNoSpace` | 无空闲 segment 可写 |
| `ErrShortWrite` | 实际写入字节数少于期望 |
| `ErrKeyTooLong` | key 超过协议允许长度 |

### 6.2 `Get` — 读取对象区间

```go
func (s *Storage) Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error)
```

- 读取 `[off, off+size)` 子区间；`size = -1` 读至结尾。
- **返回值语义**：`data` 来自内部池化缓冲，用毕**必须**调用 `release()`（幂等）归还。
  漏归还会耗尽缓冲池、走兜底分配路径。
- 定位顺序：路由缓存 → TiKV 索引定位实例 →（全 miss）回源重建。

| 错误 | 说明 |
|---|---|
| `ErrNotFound` | key 不存在且无回源 / 回源未配置 |
| `ErrInvalidRange` | `off < 0` 或 `size < 0` |
| `ErrSourceUnset` | 全 miss 且未配置 `Source` |

### 6.3 `GetFd` — 零拷贝读（splice 源）

```go
func (s *Storage) GetFd(ctx context.Context, key string, off, size int64) (fd int, foff uint64, data []byte, release func(), err error)
```

尽力交付共享内存 `(fd, foff)` 零拷贝源（供 FUSE 读路径 `splice`）。

- **`fd > 0`**：`splice(fd, foff, size)` 完成后调用 `release()`。
- **`fd == 0`**：实例连接不支持（TCP/多帧）时回退拷贝路径，`data` 为池化拷贝缓冲，用毕 `release()`。
- 回源重建仅走拷贝路径（`fd == 0`）。

### 6.4 `GetBatch` — 批量区间读

```go
func (s *Storage) GetBatch(ctx context.Context, keys []string, off, size int64) ([][]byte, func(), error)
```

- 语义同 `Get`，批量读多个 key 的 `[off, off+size)`。
- **仅经路由缓存定位**（不打 TiKV 索引）：缓存命中的 key 按实例分组后在各自实例连接上批量读；
  路由未命中的 key 逐 key 走原 `Get`（含 TiKV 索引定位 + 回源重建），并顺带预热路由缓存。
- `out[i]` 对应 `keys[i]`；返回的 `release` 归还**全部**缓冲（幂等）。
- SDK 不接受 `size < 0`（与单 key `Get` 不同），返回 `ErrInvalidRange`。

### 6.5 `GetFdBatch` — 批量零拷贝读

```go
func (s *Storage) GetFdBatch(ctx context.Context, keys []string, off, size int64) ([]*FdBuf, func(), error)
```

- 定位与分组语义同 `GetBatch`；路由 miss 与实例连接非批量均逐 key 原 `GetFd` 回退。
- 返回 `out[i]` 对应 `keys[i]`，每个 `*FdBuf` 各持独立引用，用毕**逐个 `Release()`**。
- 返回的 `release` 为整批兜底（幂等），调用后不得再使用/释放批内 `FdBuf`。

### 6.6 `PutBatch` — 批量写（同内容）

```go
func (s *Storage) PutBatch(ctx context.Context, keys []string, size int64, in []byte) error
```

- 全部 key 均写 `in` 前 `size` 字节（bench 语义）。
- Picker 一次定实例，成功后批量写索引与路由缓存。
- 实例连接不支持批量时逐 key `Put` 回退（同一实例）。

### 6.7 `PutBatchKeys` — 批量写（各 key 不同内容）

```go
func (s *Storage) PutBatchKeys(ctx context.Context, keys []string, size int64, datas [][]byte) error
```

- 每 key 写 `datas[i]` 前 `size` 字节（FUSE 数据面用）。
- `len(datas) != len(keys)` 返回 `ErrInvalidRange`。
- 其余语义同 `PutBatch`。

### 6.8 `Delete` — 删除对象

```go
func (s *Storage) Delete(ctx context.Context, key string) error
```

- 索引定位删；索引 miss 则遍历本地全部实例删。
- 同步删除索引与路由缓存。
- key 不存在不报错（`ErrNotFound` 被吞）。

### 6.9 `Stat` — 查询对象大小

```go
func (s *Storage) Stat(ctx context.Context, key string) (int64, error)
```

- 返回对象逻辑大小。
- 顺序：本地实例优先 → 索引定位远端。全 miss 返回 `ErrNotFound`。

### 6.10 `PreloadRoute` — 预热路由缓存

```go
func (s *Storage) PreloadRoute(ctx context.Context, keys []string)
```

- 逐个 key 查索引/路由并填充 `routeCache`（不读数据）。
- 压测/预热场景用：热缓存下 `Get` 首查命中 routeCache，消除「每 key 先查 TiKV」的冷启动开销。
- 内部用 64 并发 worker 并行填充（`lookup` 幂等且并发安全）。

### 6.11 `Close` — 释放资源

```go
func (s *Storage) Close() error
```

- 停止索引/发现后台任务。
- 停止 SDK 客户端心跳并尽力注销注册记录。
- 关闭全部数据面连接。
- **幂等**（索引与注册表的 stop 均幂等）。

---

## 7. 运维 / 观测 API

### 7.1 `HasLive`

```go
func (s *Storage) HasLive() bool
```

是否有在线实例可服务。

### 7.2 `CheckPoolIsValid`

```go
func (s *Storage) CheckPoolIsValid() error
```

确认集群有在线实例可服务；无实例返回 `ErrNoInstances`。挂载/卷创建时尽早暴露配置错误。

### 7.3 `UsageGet`

```go
func (s *Storage) UsageGet() (float64, error)
```

返回在线实例**最低**水位（0~1），无实例返回 `ErrNoInstances`。用于对象存储用量上报。

### 7.4 `ListIndexKeys`

```go
func (s *Storage) ListIndexKeys(ctx context.Context, prefix string) ([]string, error)
```

从 KV 索引区枚举全部 key，剔除索引前缀后返回。用于迁移 / destroy 等全量枚举场景。

---

## 8. 内部机制

### 8.1 写路由

```
Put/PutBatch/PutBatchKeys
  └─ picker.pick()
       ├─ RouteLocal：local(同机)健康 → remote 健康 → local 随机 → all 随机
       └─ RouteRoundRobin：所有在线实例轮询，超水位跳过
```

- 水位 = `Used / Capacity * 100`，超过 `UsageThreshold`（默认 80）的实例在写路径被跳过。
- 全部超水位时兜底随机/游标处实例，**不丢写入**（缓存场景宁可写满盘）。

### 8.2 读定位

```
Get/GetFd
  ├─ routeCache.get(key)        → 本地内存 LRU（16 分片，每片 4096）
  ├─ index.get(key)             → TiKV 索引（命中后回填 routeCache）
  └─ getFromSource(key)         → Source 回调（回写缓存，失败不阻塞读）
```

`GetBatch`/`GetFdBatch` **只查 routeCache**，不打 TiKV 索引；miss 的 key 逐 key 走 `Get`/`GetFd`
（含索引 + 回源），并顺带预热缓存。

### 8.3 传输方式

| Transport | 同机 | 跨节点 |
|---|---|---|
| `auto`（默认） | shm 优先，失败回退 TCP | TCP（多地址时每地址各建 Conns 条连接，round-robin 均分） |
| `rpc` | TCP | TCP |
| `shm` | shm（无 `ShmAddr` 返回 `ErrNoShmAddr`） | 不可用 |

连接缓存为 `atomic.Value` 只读快照（copy-on-write），读热路径无锁；懒建首连触碰锁。

### 8.4 索引（indexManager）

- `Put` 后异步投递 `key→实例名`，批量（128 条 / 100ms）写 TiKV。
- 队列满（4096）时丢弃（读 miss 回源兜底）。
- `Delete` 同步尽力删索引，保证删除语义。

### 8.5 客户端注册与心跳

- `NewCluster`/`NewFromTiKV` 时向 KV 注册 SDK 客户端（`ClientKeyPrefix/{id}`），
  字段含 ID / Node / Addr / Host / Pid / SDKVersion / Extra / StartTime。
- 心跳 goroutine 每秒续约；`Close` 时取消心跳并尽力注销。
- 注册失败不阻断（心跳每周期重试注册，自愈）。

---

## 9. 对外类型

以下类型通过 `type alias` 从 `internal/` re-export，外部可直接命名（见
[`reexport.go`](../../pkg/taihu-client/reexport.go) 与
[`kv_reexport.go`](../../pkg/taihu-client/kv_reexport.go)）。

### 9.1 `Storage`

集群对象存储客户端（`*Storage`），实现 `ObjectStore`。所有数据面/运维方法的接收者。

### 9.2 `KV`

```go
type KV = cluster.KV  // 接口
```

集群注册与索引的最小存储抽象。内置实现：`*cluster.TiKVKV`（TiKV TxnKV）、内存 KV。

```go
type KV interface {
    Put(ctx, key, value []byte) error
    Get(ctx, key []byte) ([]byte, error)
    Delete(ctx, key []byte) error
    DeleteRange(ctx, start, end []byte) error
    Scan(ctx, start, end []byte, limit int) ([][]byte, [][]byte, error)
    BatchPut(ctx, kvs map[string][]byte) error
    BatchGet(ctx, keys [][]byte) ([][]byte, error)
    Close() error
}
```

约定：`Get` 返回 `nil` 表示 key 不存在；`Scan` 返回 `[start, end)` 字典序区间，`limit<=0` 不限制。

### 9.3 `InstanceInfo`

实例注册信息（定义于 [`internal/cluster/instance.go`](../../internal/cluster/instance.go#L8-L20)）。

| 字段 | 类型 | 说明 |
|---|---|---|
| `Name` | `string` | 实例名 |
| `Node` | `string` | 节点标识 |
| `Hostname` | `string` | 主机名（同机判定依据） |
| `Addr` | `string` | 首个 TCP 地址（兼容旧客户端） |
| `Addrs` | `[]string` | 完整 TCP 地址列表（旧 server 无此字段） |
| `ShmAddr` | `string` | 同机 unix socket 路径（空=未开放 shm） |
| `Capacity` | `int64` | 容量（字节） |
| `Available` | `int64` | 可用（字节） |
| `Used` | `int64` | 已用（字节） |
| `StartTime` | `int64` | 注册时刻（unix 秒，固定不变） |
| `LastHeartbeat` | `int64` | 最后心跳（unix 秒，离线判定依据） |

方法：`Aliveness(now time.Time, timeout time.Duration) bool`。

### 9.4 `FdBuf`

`GetFdBatch` 单 key 的交付结果（定义于 [`internal/transport/client.go`](../../internal/transport/client.go#L28-L47)）。

```go
type FdBuf struct {
    Fd   int
    Foff uint64
    Data []byte
    // release 未导出
}
func (b *FdBuf) Release()  // 归还底层缓冲，幂等
```

构造：`NewFdBuf(fd int, foff uint64, data []byte, release func()) *FdBuf`。

### 9.5 `ObjectStore`

```go
type ObjectStore = rpcclient.ObjectStore  // 接口
```

taihu 客户端共同满足的对象存储接口。两个实现：
- `*rpcclient.Storage`：直连一个已知实例；
- `*taihuclient.Storage`（本包）：经 TiKV 定位实例后路由。

```go
type ObjectStore interface {
    Put(ctx, key string, size int64, in []byte) error
    Get(ctx, key string, off, size int64) ([]byte, func(), error)
    Delete(ctx, key string) error
    Stat(ctx, key string) (int64, error)
    Close() error
}
```

### 9.6 `TLSConfig`

```go
type TLSConfig = cluster.TLSConfig  // { CA, Cert, Key string }
```

TiKV TLS 配置（三件套齐全才启用；全空=明文）。

---

## 10. 错误面

所有错误唯一定义于 [`pkg/ierr`](../../pkg/ierr/ierr.go)，SDK 通过 re-export 透出。
调用方用 `errors.Is` 判定。

### 10.1 SDK 直接返回的错误

| 错误 | 来源 | 触发场景 |
|---|---|---|
| `ErrNotFound` | `ierr` | key 不存在（Get/Stat 全 miss；Delete 不返回） |
| `ErrInvalidRange` | `ierr` | `off < 0` 或 `size < 0`（GetBatch 还含 `len(datas)!=len(keys)`） |
| `ErrTooLarge` | `ierr` | 对象超过单 segment 上限 |
| `ErrNoSpace` | `ierr` | 无空闲 segment 可写 |
| `ErrShortWrite` | `ierr` | 实际写入少于期望 |
| `ErrNoInstances` | `ierr` | 集群无在线实例（Put/CheckPoolIsValid/UsageGet） |
| `ErrSourceUnset` | `ierr` | 全 miss 且未配置 `Source` |
| `ErrKVRequired` | `ierr` | `NewCluster` 时 `KV == nil` |
| `ErrNoShmAddr` | `ierr` | `Transport=shm` 但实例无 `ShmAddr` |
| `ErrClientIDRequired` | 本包 | `ClientID` 为空 |
| `ErrKeyTooLong` | `ierr` | key 超过协议允许长度 |

> 传输层错误（`ErrConnClosed` / `ErrShmBadFrame` / `ErrShmStreamBroken` /
> `ErrShmUnsupported` / `ErrInvalidArgument` / `ErrRPCError` 等）由底层连接透出，
> 同样可 `errors.Is` 判定。

### 10.2 错误使用示例

```go
_, _, err := cli.Get(ctx, key, 0, -1)
switch {
case errors.Is(err, taihuclient.ErrNotFound):
    // key 不存在
case errors.Is(err, taihuclient.ErrSourceUnset):
    // 集群全 miss 且未配置回源
default:
    // 其它错误
}
```

---

## 11. 版本与兼容

- **SDK 版本**：`internal/version.String()` 返回 `{commit}_{buildtime}`，如 `8abdf4d_202609111002`。
  随客户端心跳上报（`ClientInfo.SDKVersion`），可在 web 客户端清单中查看。
- **版本注入**：编译期 `-ldflags "-X ...version.Commit=<sha> -X ...version.BuildTime=<YYYYMMDDHHMM>"`；
  未注入时运行时回退查询 `git`。
- **传输兼容**：服务端通告 `Addrs`（多地址）时跨节点总连接数 = 地址数 × `Conns`；
  旧 server 无 `Addrs` 时回退单地址 `Addr`。
- **无覆盖写约束**：自 v1 起即固定，不提供 `Put` 覆盖语义。
- **错误兼容**：所有 sentinel error 定义于 `pkg/ierr`，仅增不删；内部不建别名层。

---

## 12. 与设计稿对齐情况

上游设计稿：`.trae/documents/taihu-cli-design.md`（taihu-cli 设计文档 §5 客户端注册）、
`docs/设计文档/20260911_taihu集群支持设计文档.md`。

| 设计项 | 状态 | 说明 |
|---|---|---|
| 集群客户端经 TiKV 路由 | ✅ 一致 | `NewFromTiKV` 连 PD，`KV` 接口可替换 |
| 本地优先写路由 | ✅ 一致 | `RouteLocal` 默认，同机 shm / 跨节点 TCP |
| key 不可变 | ✅ 一致 | 无覆盖写，更新 = Delete 后重建 |
| 索引尽力而为 | ✅ 一致 | 异步批量写，队列满丢弃，读 miss 回源兜底 |
| TiKV 不可用降级 | ✅ 一致 | 注册表 refresh 失败保留旧快照，退化为本地实例 |
| SDK 客户端注册 + 心跳 | ✅ 一致 | `ClientID` 必填，注册/心跳/注销全链路 |
| 回源回调 | ✅ 一致 | `SourceGetter`，全 miss 时拉取并回写 |
| 多地址连接均分 | ✅ 一致 | `Addrs` 每地址各建 `Conns` 条，round-robin |

> 无「设计未实现」或「语义不一致」项。如后续设计稿更新，以本文档「版本与兼容」节记录差异。

---

## 附录 A：API 速查表

| 分类 | 方法 | 简要 |
|---|---|---|
| 构造 | `NewFromTiKV` | 连 TiKV 构建客户端（推荐） |
| 构造 | `NewCluster` | 注入自定义 KV 构建客户端 |
| 数据面写 | `Put` | 写单对象 |
| 数据面写 | `PutBatch` | 批量写（同内容） |
| 数据面写 | `PutBatchKeys` | 批量写（各 key 不同内容） |
| 数据面读 | `Get` | 区间读（含回源） |
| 数据面读 | `GetFd` | 零拷贝读（splice 源） |
| 数据面读 | `GetBatch` | 批量区间读 |
| 数据面读 | `GetFdBatch` | 批量零拷贝读 |
| 数据面删 | `Delete` | 删除对象 |
| 数据面查 | `Stat` | 对象大小 |
| 数据面预热 | `PreloadRoute` | 预热路由缓存 |
| 运维 | `HasLive` | 有无在线实例 |
| 运维 | `CheckPoolIsValid` | 校验集群可用 |
| 运维 | `UsageGet` | 最低水位 |
| 运维 | `ListIndexKeys` | 枚举索引 key |
| 生命周期 | `Close` | 释放全部资源 |

## 附录 B：相关文档

- e2e 测试文档：[`docs/e2e/README.md`](../e2e/README.md)
- 设计文档：[`docs/设计文档/20260911_taihu集群支持设计文档.md`](../设计文档/20260911_taihu集群支持设计文档.md)
- 示例：[`examples/taihu-client/main.go`](../../examples/taihu-client/main.go)
