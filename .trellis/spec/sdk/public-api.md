# SDK 对外可见面与调用契约

> `taihuclient` 包导出什么、re-export 到哪一层为止、外部调用方必须遵守哪些约定（全部以源码为准，不以 README 为准）。

---

## 入口：两个构造函数

`NewFromTiKV` 是生产入口，`NewCluster` 是注入后端入口；两者返回同一个 `*Storage`。

```go
// NewFromTiKV 连接 TiKV TxnKV 并构建集群客户端（Storage）。
// 返回的 Storage 已启动实例发现/索引后台任务，用毕须 Close()。
func NewFromTiKV(ctx context.Context, opts TiKVOptions) (*Storage, error) {
	if len(opts.PDAddrs) == 0 {
		return nil, fmt.Errorf("taihuclient: TiKVOptions.PDAddrs is required")
	}
```
（`pkg/taihu-client/tikv.go:39-44`）

- `NewFromTiKV` 内部构造 `cluster.NewTiKVKV`（TxnKV，memcomparable 编码），再逐字段搬进 `ClusterConfig`（`tikv.go:45-64`）；构造失败时关掉已开的 KV（`tikv.go:65-68`）。
- `NewCluster(cfg ClusterConfig)` 要求 `cfg.KV != nil`，否则返回 `"taihuclient: KV is required"`（`pkg/taihu-client/storage.go:52-55`）；成功后 `reg.start()` / `index.start()` / `startClientKeepalive` 三件事都已做完，**不需要调用方再启动任何东西**（`storage.go:67-69`）。
- 两个构造函数都返回必须 `Close()` 的对象：`Close` 停 index + registry + 客户端心跳，再关闭全部数据面连接（`storage.go:339-360`）。

## `Storage` 的能力面

`Storage` 是**唯一**的公开客户端类型（`storage.go:29-45`），能力分五块：

| 能力 | 方法 | 定义处 |
|---|---|---|
| 数据面 | `Put` / `Get` / `Delete` / `Stat` / `Close` | `storage.go:181`、`201`、`298`、`320`、`339` |
| 预热 | `PreloadRoute` | `storage.go:216` |
| 健康检查 | `HasLive` / `CheckPoolIsValid` | `ops.go:11` / `ops.go:16` |
| 用量上报 | `UsageGet` | `ops.go:24` |
| 索引枚举 | `ListIndexKeys` | `ops.go:46` |

数据面五方法正好是 `rpcclient.ObjectStore`（`internal/rpcclient/objectstore.go:29-35`），并有编译期断言钉住：

```go
// 编译期断言：集群客户端与直连客户端的方法集不得漂移（ObjectStore 声明在
// rpcclient，两个实现都必须满足）。接口声明处无法反向断言，故放在这里。
var _ rpcclient.ObjectStore = (*Storage)(nil)
```
（`pkg/taihu-client/storage.go:47-49`）

### 读路径的语义（调用方最需要知道的一条）

`Get` 的定位顺序是「路由缓存 → TiKV 索引 → 回源重建」，**不逐个本地试读**：

```go
// Get 读取对象 [off, off+size) 子区间（size=-1 读至结尾）。
// 顺序：路由缓存 → TiKV 索引定位实例 → 回源重建（不再本地逐个试读）。
// 返回 (data, release, err)，用毕必须 release()（幂等，归还池化缓冲）。
func (s *Storage) Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error) {
```
（`pkg/taihu-client/storage.go:198-201`）

- `release` **必须**调用。索引命中路径返回的是池化缓冲；回源路径返回的 `release` 是 noop（`storage.go:276` 的「返回的 data 无池化归属，release 为 noop」，实现为 `storage.go:294`）。调用方统一 `defer release()` 即可，两条路径都安全。
- `key` 不可变、无覆盖写，更新语义 = `Delete` 后重建（`storage.go:27`，示例侧同口径 `examples/taihu-client/main.go:74-75`）。
- 索引是尽力而为：异步批量写、队列满即丢（`index.go:78-84`），读 miss 由回源兜底 —— 所以**没有配 `Source` 时，索引丢过的 key 读不到**，此时 `Get` 返回包内私有的 `errSourceUnset`（`storage.go:20`、`storage.go:278-280`，见下文「错误」）。

### 写路径的语义

`Put` 不做写前归属查询，直接「Picker 本地优先选实例 → 写 → 异步索引 + 路由缓存」（`storage.go:179-196`）；无在线实例时返回 SDK 自己定义的 `ErrNoInstances`（`storage.go:18`、`storage.go:182-185`）。

## 配置

`ClusterConfig`（`config.go:41-76`）与 `TiKVOptions`（`tikv.go:14-32`）字段一一对应，默认值规则**写在字段注释里，不在此重述**（如 `RefreshInterval <=0 默认 1s`、`UsageThreshold <=0 或 >100 默认 80`，`config.go:48-54`）。新增字段时两边都要改：`tikv.go:51-64` 是手写逐字段搬运，漏一个不会报错、只会静默丢配置。

对外可写字符串的枚举常量集中声明，调用方应引用常量而非字面量：

```go
	// RouteRoundRobin 轮询：写请求在所有在线实例间按 round-robin 均分（含跨节点 TCP），
	// 单实例超水位时跳过该次（全满则兜底）。
	RouteRoundRobin = "round-robin"
)
```
（写路由：`config.go:21-28`；传输方式：`config.go:31-38`）

回源回调类型是 SDK 自定义的，不是 internal 类型：`SourceGetter func(ctx, key) ([]byte, error)`，返回**整对象**，实现方负责源侧错误语义（`config.go:16-18`）。

## re-export 契约：外部能命名什么

`pkg/taihu-client/reexport.go` 只做一件事 —— 把出现在导出签名/调用方解读路径上的 `internal/` 类型与错误起一个外部可引用的名字（type alias，零运行时代价）。

| 对外名字 | 指向 | 为什么需要 |
|---|---|---|
| `KV` | `cluster.KV` | 唯一真正出现在**导出签名**里的 internal 类型（`ClusterConfig.KV`，`config.go:43`）；`reexport.go:20` |
| `InstanceInfo` | `cluster.InstanceInfo` | 实例注册记录类型。目前**不在**任何导出签名里（只出现在 `registry.go:114`、`picker.go:34` 等未导出函数与字段中），是留给调用方解读注册信息用的；`reexport.go:23` |
| `ObjectStore` | `rpcclient.ObjectStore` | 给外部泛化用（唯一实现是本包 `Storage`）；`reexport.go:27` |
| `ErrNotFound` / `ErrInvalidRange` / `ErrTooLarge` / `ErrNoSpace` / `ErrShortWrite` | `rpcclient` 同名变量（再指向 `internal/ierr`） | 本包把底层错误**原样透出、不做包装**，故只 import 本包的调用方也必须能命名它们才能写 `errors.Is`；`reexport.go:29-43` |

### 故意没有转出的名字

- **`ObjectMeta` / `SegmentEntry` / `SegmentSummary` / `SegmentState` 及段状态常量**：`internal/rpcclient/reexport.go:45-63` 转了它们，SDK **不转**。原因可核实：`Storage` 的导出方法集只用 `[]byte` / `int64` / `error`（`storage.go:181`、`201`、`298`、`320`、`339`），签名里根本不出现 metastore 类型，转了也只是死名字。
- **`ierr.ErrConflict`**：不是客户端会收到的错误，而是 compaction 内部的 CAS 控制信号 —— 排除理由写在 `internal/rpcclient/reexport.go:30`，SDK 侧没有理由翻案。

### 维护约定

「签名里出现 internal 类型就补一条」是跨模块约定，本层的原文在 `pkg/taihu-client/reexport.go:8-17`。注意 alias 漏补**编译不报错**，只是包对外悄悄不可用（`internal/rpcclient/reexport.go:21-25` 说明了这一点与兜底做法）。跨模块完整论证见 `.trellis/spec/transport/interfaces-and-reexport.md`。

## 错误处理约定

- **原样透出**：本包的 `Storage` 把底层错误原样透出不做包装，`errors.Is` / 相等判断直接可用（`reexport.go:29-31`）。示例里的正确写法：

  ```go
  	if _, _, afterDelErr := cli.Get(ctx, key, 0, -1); afterDelErr != nil {
  		if errors.Is(afterDelErr, taihuclient.ErrNotFound) {
  			fmt.Fprintln(w, "删除后 Get 预期返回 ErrNotFound")
  		} else {
  ```
  （`examples/taihu-client/main.go:109-113`）

- **SDK 自有的错误**：只有 `ErrNoInstances`（`storage.go:18`），「集群无在线实例可写」；`CheckPoolIsValid` / `UsageGet` 在无实例时也返回它（`ops.go:17-20`、`ops.go:37-39`）。
- **已知缺口，调用方需知晓**：`errSourceUnset` 是**未导出**的（`storage.go:20`），全 miss 且未配置 `Source` 时 `Get` 返回的就是它（`storage.go:278-280`）。外部无法用 `errors.Is` 命名它 —— 包内测试是同一包所以能写（`storage_paths_test.go:183`），包外调用方不能。外部只能比较错误文本，或在使用时**始终配置 `Source`** 来规避。

## 示例即契约

`examples/taihu-client/` 不是文档，是编译期约束：

- 示例包注释直接声明边界：「taihu 的对外接口只有一个：pkg/taihu-client（唯一对外 SDK，集群模式）。外部业务不能直连某个 taihu server 实例，必须经 TiKV 路由访问集群。」（`examples/taihu-client/main.go:1-5`）
- 示例把用到的能力抽成接口并做编译期断言，SDK 方法集漂移会直接编译失败：

  ```go
  // 编译期断言：真实 SDK 客户端必须满足本示例使用的能力集。
  var _ clusterClient = (*taihuclient.Storage)(nil)
  ```
  （接口定义 `examples/taihu-client/main.go:28-37`，断言 `main.go:40`）

- 示例的 `newClusterClient` 是个变量而非直接调用（`main.go:43-54`），用作测试缝：无集群环境下的单测替换它注入假实现（`examples/taihu-client/main_test.go:58`）。
- 典型调用序列（示例即最短上手路径）：`NewFromTiKV` → `defer Close()`（`main.go:63-67`）→ `CheckPoolIsValid()` 尽早暴露配置错误（`main.go:70-72`）→ `Put`（`main.go:78`）→ `Get` + `defer release()`（`main.go:85-89`）→ `Stat` / 区间读（`main.go:93-103`）→ `Delete` + `errors.Is` 判定（`main.go:106-114`）。

## SDK 特有的测试约定

（更完整的测试约定见 `.trellis/spec/testing/`，这里只写 SDK 特有的缝。）

- **共享 fake 用嵌入而非实现**：`errKV` 嵌入 `cluster.KV`，只覆写需要注入错误的五个方法，注释写明用途与边界：

  ```go
  // errKV 包装 cluster.KV，按需让指定操作返回错误（错误/降级路径注入）。
  // 只出现在测试里：生产代码不感知。
  type errKV struct {
  	cluster.KV
  	scanErr  error
  ```
  （`pkg/taihu-client/testutil_test.go:13-22`；覆写方法 `testutil_test.go:24-57`）

  「生产代码不感知」是可核实的：`errKV` 只在 `_test.go` 里定义与使用 —— `ops_test.go:95` 注入 `scanErr`、`registry_picker_test.go:49` 注入 `scanErr` 让实例发现降级、`registry_picker_test.go:155` 注入 `batchErr` 让索引批量写失败。

- **构造 + 清理粘在 helper 里**：`newClusterWithKV` 内 `t.Cleanup(func() { _ = s.Close() })`（`testutil_test.go:59-72`），用例不必自己收尾。
- **实例注册与快照刷新成对**：`registerInstances` 写完 KV 后立即调 `s.registry.refresh()`，否则快照不含新实例（`testutil_test.go:74-88`）；`newClusterForInst` 把 `HeartbeatTimeout` 放宽到 1h，避免用例执行期间实例被判离线（`testutil_test.go:98-110`）。
- **外部依赖用间接层而不是真连**：`tikv.go:34-37` 的 `newTiKVKV` 变量是「cluster.NewTiKVKV 的间接层：单测注入假实现，避免连接真实 TiKV/PD」，测试用 save/restore 模式替换它（`tikv_test.go:22-23`）。新增外部后端时照此加一条缝。
