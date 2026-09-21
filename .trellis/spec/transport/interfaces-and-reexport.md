# 远程访问层的接口设计与 re-export

> 接口一律小、声明在使用方一侧；只有跨模块边界才导出；`internal/` 类型出现在导出签名里时必须逐层补 type alias，并由外置测试包兜住"漏补不报错"。

---

## 1. 内部接缝：未导出的小接口，声明在使用它的文件里

**`rpcConn`** —— `internal/rpcclient/storage_rpc.go:7-16`，5 个方法，未导出，存在的唯一目的是让 TCP `transport.Conn` 与 shm `transport.ShmConn` 互换：

```go
// rpcConn 一条底层传输连接的统一接口：TCP（transport.Conn，netpoll 帧协议）或
// 共享内存（shmConn，shmipc 流帧协议）均可满足，Storage 按 round-robin 分发到
// 各连接，两方案对上层调用方透明。
type rpcConn interface {
	Put(ctx context.Context, key string, size int64, in []byte) error
	Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error)
	Delete(ctx context.Context, key string) error
	Stat(ctx context.Context, key string) (int64, error)
	Close() error
}
```

它是 `ObjectStore` 的对外镜像（`objectstore.go:20` 明说），但**不导出**——只有 `Storage` 需要它。

**`putStream`** —— `internal/rpcclient/putwriter.go:13-17`，只有两个方法，把「零拷贝写流」收窄到 `Reserve`/`Commit`；Linux 下由 `transport.ShmPutWriter` 满足（`putwriter_linux.go:14-22` 断言连接具体类型后取 `PutBegin`），非 Linux 直接返回 `ErrShmOnly`（`putwriter_other.go:8-10`）。可见的 `PutWriter` 只是薄包装（`putwriter.go:21-29`）。

**`tikv*` 接口组** —— `internal/cluster/kv_tikv.go:32-65`，四个未导出接口（`tikvIterator` / `tikvSnapshot` / `tikvTxn` / `tikvClient`）逐一对齐 client-go 的具体类型，理由写在注释里：

```go
// 测试缝隙：TiKV 客户端/快照/事务/迭代器抽为最小接口（能力集与 client-go 具体类型
// 逐一对齐，无额外开销），生产实现为 txnkvClient/txnkvSnapshot，单元测试注入假实现
// 覆盖错误与边界路径——单测绝不连真实 TiKV/PD。迭代器接口之所以单独声明：
// client-go 的 unionstore.Iterator 位于其 internal 包，本包无法引用该类型。
```

适配器只有两个：`txnkvSnapshot`（`kv_tikv.go:67-81`）与 `txnkvClient`（`:83-94`），把 client-go 的 `unionstore.Iterator` 归一为 `tikvIterator`。**新增 client-go 调用时先在接口能力集里加一个方法、再在适配器里转发**，不要在 handler 里直接抓具体类型——否则 `kv_tikv_test.go` 的假实现就失效了。

## 2. 只在跨模块边界导出接口：`ObjectStore` 的存在理由

`internal/rpcclient/objectstore.go:29-35` 声明、理由在注释里（`:13-19`、`:20-21`）：

```go
// 为什么声明在本包而不是 taihuclient：taihuclient 依赖本包，反向声明会成环。
// Go 是结构化类型 —— taihuclient.Storage 天然满足这里声明的接口，无需 import 本
// 接口，只需一条 `var _ ObjectStore = (*Storage)(nil)` 断言（见 pkg/taihu-client/storage.go），
// 那条断言同时充当两个客户端「方法集不漂移」的编译期检查。
//
// 签名里只有 context / []byte / error 等标准库类型，因此**外部实现也能满足本接口**
// （内部类型出现在导出接口的方法签名里会让它对外不可实现）。
```

**规则**：导出接口的方法签名里只允许标准库类型与其它导出类型。这不是风格问题——一旦签名里出现 `internal/...` 类型，外部模块就**永远无法**实现该接口（也无法命名它）。`pkg/taihu-client/reexport.go:25-27` 再把它 alias 一层给外部调用方。

## 3. 编译期满足性断言是常态

`var _ 接口 = (*实现)(nil)` 在本层及相邻层共 7 条（`grep -rn 'var _ ' --include='*.go' internal/ pkg/ | grep -v _test.go`）：

| 位置 | 断言 |
|------|------|
| `internal/transport/protocol/protocol.go:191` | `var _ ByteReader = netpoll.Reader(nil)` |
| `internal/transport/protocol/protocol.go:192` | `var _ ByteReader = (*SliceReader)(nil)` |
| `internal/cluster/kv_mem.go:16` | `var _ KV = (*MemoryKV)(nil)` |
| `internal/cluster/kv_tikv.go:30` | `var _ KV = (*TiKVKV)(nil)` |
| `internal/rpcclient/objectstore.go:37` | `var _ ObjectStore = (*Storage)(nil)` |
| `pkg/taihu-client/storage.go:49` | `var _ rpcclient.ObjectStore = (*Storage)(nil)` |
| `internal/metastore/kv_pebble.go:31` | `var _ Store = (*pebbleStore)(nil)`（邻层，列出仅为完整性） |

`pkg/taihu-client/storage.go:47-48` 解释了为什么这条断言必须放在**实现侧**而不是接口声明侧：

```go
// 编译期断言：集群客户端与直连客户端的方法集不得漂移（ObjectStore 声明在
// rpcclient，两个实现都必须满足）。接口声明处无法反向断言，故放在这里。
```

**新增一个传输/存储实现（第三个客户端形态、fake KV）时，第一件事是补这条断言。**

## 4. re-export 用 type alias（`=`），不用新类型

`internal/rpcclient/reexport.go:8-19` 是这一整套做法的论证原文：

```go
// 本文件把出现在本包**导出签名**里的 internal 类型 re-export 出去。
//
// 为什么必须补：internal/ 下的类型，外部模块既不能 import 也不能命名。于是
//
//	func (s *Storage) Meta(ctx, key) (metastore.ObjectMeta, error)
//	func (s *Storage) Segments(ctx) (metastore.SegmentSummary, []metastore.SegmentEntry, error)
//
// 这类签名虽然能被调用，但调用方无法声明该类型的变量、写不了字面量、也放不进自己的
// 结构体字段 —— 等于这些 API 对外不可用（曾实测外部模块报 `undefined: taihu.Layout`）。
//
// 用 type alias（=）而不是自定义新类型：不新建类型、零运行时代价、无需任何转换，
// 只是给同一个类型在本包内起一个外部可引用的名字，**签名与行为完全不变**。
```

用新类型会坏在：签名里的 `metastore.ObjectMeta` 与新类型不是同一类型，方法签名与行为都要改，且调用方拿到的仍是不可命名的类型——alias 是零成本地把"同一个类型"换个外部可见的名字。

维护约定（同文件 `:21-25`）：**类型和常量要一起补**，因为"只给类型不给常量，调用方仍然用不了 —— 例如 `map[SegmentState]int` 没法写 key"（`:22-23`）。所以 `SegmentState`（`:54`）与 5 个状态常量（`:57-63`）成对出现。

错误 sentinel 的 re-export 是**白名单**，且故意排除 `ierr.ErrConflict`（`reexport.go:29-30`）：

```go
// 不含 ierr.ErrConflict：那是 compaction 内部的 CAS 控制信号，不是客户端会收到的错误。
```

**新增 sentinel 时先问"客户端会不会收到"**：不会收到就不要挂到 re-export 列表里。

## 5. 第二层 re-export：`pkg/taihu-client/reexport.go`

`pkg/taihu-client/reexport.go:8-17` 复述规则并指向第一层：

```go
// 本文件把出现在本包**导出签名**里的 internal 类型 re-export 出去。
//
// internal/ 下的类型外部模块无法命名，会让下面这些签名对外不可用：
//
//	func newInstanceRegistry(kv cluster.KV, ...) *instanceRegistry
//	func (r *instanceRegistry) lookup(name string) (cluster.InstanceInfo, bool)
//	type ClusterConfig struct { KV cluster.KV }
//
// type alias（=）不新建类型、零运行时代价，只是让同一类型有一个外部可引用的名字。
// 详细说明见 internal/rpcclient/reexport.go。维护约定同：签名里出现 internal 类型就补一条。
```

本层相关的三条 alias：`KV = cluster.KV`（`:20`）、`InstanceInfo = cluster.InstanceInfo`（`:23`）、`ObjectStore = rpcclient.ObjectStore`（`:27`）。错误值是**再指一层**而不是重新定义（`:29-32` 注释："定义在 internal/ierr，internal/rpcclient 已 re-export，此处再指一层以保持单一来源"）。

## 6. alias 漏补不会编译报错 —— 用外置测试包把它变成编译错误

`internal/rpcclient/api_test.go:11-21` 说明了这个文件为什么以 `package rpcclient_test` 存在：

```go
// 本文件以**外部测试包**（package rpcclient_test，不是 package rpcclient）身份，
// 把本包导出的、以及出现在导出签名里的类型与常量逐个命名一遍。
//
// 为什么需要：reexport.go 里的 alias 是人手维护的，**漏补一条编译器不会报错** ——
// 包照样编过，只是对外悄悄不可用（调用方写不出那个类型的变量、构造不了字面量）。
// 本文件从外部包视角把这些名字全用一遍，漏了就编译失败，把「静默不可用」变成编译期错误。
```

它做三件事，**任何一条新增对外类型/常量都要同步补进来**：

1. 在调用方自己的类型上实现接口（`api_test.go:25-43`）——验证接口签名里没有不可命名的 internal 类型；
2. 把类型放进带 json tag 的结构体（`:55-60`）——验证字段类型真的可写；
3. 常量与类型配套 + 错误身份互异（`:63-94`）：`map[rpcclient.SegmentState]int` 用到全部 5 个常量并要求"恰好 5 个"（`:70-72`），5 个 sentinel 逐个 `errors.Is(err, err)` 且两两不同一（`:82-94`）。

诚实边界（`api_test.go:18-20`）：外置测试包与库同属一个 module，**不能**完全复现"internal 类型对外不可命名"，真正的门外验证要用模块外消费者探针（/tmp 下另建 module + replace）。这个文件只是仓库内的廉价兜底。更完整的测试层约定见 `.trellis/spec/testing/`，这里只保留与 re-export 契约耦合的部分。

## 7. `internal/cluster` 是叶子包：不依赖仓库内任何其它包

包注释（`internal/cluster/kv.go:1-4`）：

```go
// Package cluster 提供 taihu 集群支持的基础组件：注册中心 KV 接口、
// 实例注册/心跳/注销、实例信息模型。注册/索引后端可替换（内存 / TiKV TxnKV），
// 调用方通过 KV 接口注入，数据面（internal/transport、internal/storage）不感知集群细节。
```

`grep -rn 'github.com/liucxer/taihu' --include='*.go' internal/cluster/` 无输出——本包只依赖标准库与 `github.com/tikv/client-go/v2`。**往这个包加 import 前先想清楚**：它一旦依赖 transport/storage，`pkg/taihu-client` 的依赖面就会被拖进引擎（`make check-sdk-only` 会红，见 `Makefile:36`）。

两个实现，一真一假：

- `MemoryKV`（`kv_mem.go:9-14`）："内存 KV 实现：单机开发/测试、无 TiKV 环境的降级运行。数据不持久化，进程退出即丢失（与缓存语义一致：可丢）"，所有方法都返回深拷贝（`kv_mem.go:23-28`、`:30-38`），`Scan` 按字典序排序并支持 `limit`（`:59-80`）。
- `TiKVKV`（`kv_tikv.go:26-30`）：走 TxnKV（注释解释为何不用 rawkv：memcomparable 编码共存），读走 `RC` 隔离的一致性快照（`kv_tikv.go:111-121`，"RC 隔离避免读被残留锁阻塞"），`Get` 对 `tikverr.IsErrNotFound` 归一为 `(nil, nil)` 以对齐 KV 接口约定（`:134-148`）。

**降级路径是设计目标而非权宜**：`MemoryKV` 让无 TiKV 环境（本机开发、单测）也能跑完整集群逻辑；测试用假 TiKV 接口注入错误路径（见 §1），不要为了测试去连真实 PD。
