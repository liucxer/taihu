# 元数据与压缩（metadata-and-compaction）

> pebble 持久化的映射与段状态、段状态机与后台 GC、分配器、以及基于 CAS 的段级搬移 —— 段生命周期所有不变量的唯一事实源在 `internal/metastore`，搬移的调用方在 `internal/storage/compact.go`。

---

## 一、命名空间与编码

包文档两行讲清全部 —— `internal/metastore/store.go:1-2`：

```go
// Package metastore 抽象元数据持久化（本实现基于 CockroachDB pebble，见 kv_pebble.go）。
// 两个逻辑命名空间：mapping（key → ObjectMeta）、state（cursor / seg/<id>）。
```

pebble 是单 keyspace、无列族，因此用 key 前缀隔离两个逻辑命名空间 —— `internal/metastore/kv_pebble.go:16-29`：

- `mapping = "m\x00"`（用户 key → `ObjectMeta`），`state = "s\x00"`（`cursor` 与 `seg/<id>`）—— `internal/metastore/kv_pebble.go:26-29`。
- 用户 key 会被**整体映射到 mapping 前缀之下**，因此即便用户 key 恰好以 state 前缀开头也不会与内部键冲突 —— `internal/metastore/kv_pebble.go:24-25`。
- state 内部键字面量：`"cursor"` 与 `"seg/"` + little-endian(int64) —— `internal/metastore/meta.go:143-147`、`internal/metastore/meta.go:149-154`。
- 迭代区间一律用「前缀末字节 +1」求独占上界，`internal/metastore/kv_pebble.go:86-91` 的 `appendUpper` 是这做法的复用点。

**编码规则**（固定字段 little-endian，每个 value 头部 1 字节 version 便于演进）—— `internal/metastore/meta.go:13-18`：

```go
const (
	metaVersion    = byte(0)
	objectMetaLen  = 1 + 8*3     // version + SegmentID + Offset + Size
	writeCursorLen = 1 + 8*2     // version + SegmentID + Offset
	segmentMetaLen = 1 + 1 + 8*2 // version + State + AliveCount + ReclaimSeq
)
```

- 三个 `decodeXxx` 都以「长度不等于常量则报错」作为第一道校验 —— `internal/metastore/meta.go:37-40`、`internal/metastore/meta.go:63-66`、`internal/metastore/meta.go:101-104`。
- **写入默认 Sync**，以保证「先写设备数据 → 写 mapping → 更新 cursor」的持久化顺序，崩溃后游标不回退 —— `internal/metastore/kv_pebble.go:22`；具体由 `syncWO = &pebble.WriteOptions{Sync: true}` 承载 —— `internal/metastore/kv_pebble.go:65`。

### 领域态只有一份，wire 态刻意分开

`SegmentEntry` / `SegmentSummary` 的领域态定义在 `internal/metastore/meta.go:112-141`，注释解释了为什么**刻意不让 `protocol` import 本包**：本包依赖 pebble，而 `protocol` 是只依赖 `encoding/binary` 的纯 codec，把持久化模型拖进编解码层不划算；两份结构的字段平齐性由 `protocol_test.go` 的 `TestSegmentWireParity` 守着 —— `internal/metastore/meta.go:117-122`。

## 二、`Store` 接口的明确契约

`Store` 是生产者侧抽象，也是本仓库最大的一个接口，每个方法都有中文语义注释 —— `internal/metastore/store.go:20-86`。`pebbleStore` 满足它的编译期断言在 `internal/metastore/kv_pebble.go:31`。必须记住的几条：

- `GetMapping`：key 不存在返回 `ierr.ErrNotFound` —— `internal/metastore/store.go:21`。
- `BatchGetMapping`：**结果按入参 key 顺序返回；任一 key 缺失整体返回 `ierr.ErrNotFound`** —— `internal/metastore/store.go:26-29`：

```go
	// BatchGetMapping 一次读取多个 key 的对象映射（等价于多次 GetMapping，
	// 但未命中走单快照 + 单迭代有序 Seek，摊薄 N 次独立 pebble Get）。
	// 结果按入参 key 顺序返回；任一 key 缺失整体返回 ierr.ErrNotFound。
	BatchGetMapping(ctx context.Context, keys []string) ([]ObjectMeta, error)
```

  实现佐证：命中缓存的 key 先填位；未命中 key **排序去重**后走单个迭代器有序 `SeekGE`，最后按原 `keys` 顺序回填 —— `internal/metastore/kv_pebble.go:167-215`；缺失时的出口是 `return nil, ierr.ErrNotFound` —— `internal/metastore/kv_pebble.go:198`。「顺序返回」的语义在调用方被真实依赖：`Storage.BatchRead` 直接用 `metas[i]` 对应 `blocks[i].Key` —— `internal/storage/storage.go:372-381`。

- `BatchDeleteMapping`：返回 **per-key 错误**（缺失 key 为 `ierr.ErrNotFound`，其余 nil），整体存储错误经第二个返回值暴露 —— `internal/metastore/store.go:36-38`。
- `AllocateSegment`：返回的偏移**恒 4K 对齐、单调不重叠，可并发调用**；段写满自动滚动 —— `internal/metastore/store.go:43-45`。
- `AllocateSegmentReserve`：可动用预留缓冲段（用户路径不可用），保证 compaction 在用户写入占满时仍有落点，**避免搬移死锁** —— `internal/metastore/store.go:50-52`。
- `MarkCompacting`：段不存在或非 `Full` 时为空操作（返回 nil）—— `internal/metastore/store.go:59-61`。
- `MoveMapping`：条件写（CAS）—— **仅当当前 mapping 与 `old` 完全一致**时原子改为 `new` 并转移段存活计数（new 段 +1、old 段 −1，同一 WriteBatch）；不一致返回 `ierr.ErrConflict` 且**不修改任何数据** —— `internal/metastore/store.go:63-66`。
- `ListSegments`：枚举**内存引用表**（不是 pebble 全扫），admin / 诊断用 —— `internal/metastore/store.go:68-69`。
- `Cursor`：返回当前顺序写游标；**尚无写入时返回 `(0, 0)`** —— `internal/metastore/store.go:70-71`。
- `RefSegment` / `UnrefSegment`：读开始前 / 读结束后调用；配合 GC，**仅在引用归零时才允许 Reclaiming 段回收复用**，防止迟到读读错数据 —— `internal/metastore/store.go:73-76`。
- `UsedBytes`：Full/Reclaiming 段计整段、Active 段计已写偏移，供容量上报 `Available = Capacity − Used` —— `internal/metastore/store.go:81-83`。

## 三、段状态机与后台 GC

状态定义 —— `internal/metastore/meta.go:76-82`：

```go
const (
	SegmentStateFree       SegmentState = iota // 空闲，可分配
	SegmentStateActive                         // 正在顺序写入
	SegmentStateFull                           // 已写满，仅读
	SegmentStateReclaiming                     // 待回收（计数归零）
	SegmentStateCompacting                     // 搬移中（高空洞段存活对象搬迁，禁止分配/回收）
)
```

完整迁移链 —— `internal/metastore/segments.go:19-20`：

```go
//   - 状态机：Free → Active（分配写入）→ Full（游标写出段尾）→ Reclaiming（对象删光）
//     → Free（GC 确认无在途读者后回收入池）。
```

五条核心机制（都在 `segmentManager` 的类型注释里）—— `internal/metastore/segments.go:13-26`：

1. **存活计数 `AliveCount`**：内存维护（mapping 为真实源），每次 Put/Delete 与 mapping 写入**同一 pebble WriteBatch 原子持久化**；启动时全量扫描 mapping 重建（权威）—— `internal/metastore/segments.go:15-17`。
2. **读引用计数 `refCount`**：仅在 `refCount==0` 时允许 `Reclaiming → Free` —— `internal/metastore/segments.go:21-22`。
3. **空闲池 `free`**：FIFO，供 allocator 在游标到顶时取段复用（游标回跳，段内仍顺序写）—— `internal/metastore/segments.go:23`。
4. **后台 GC goroutine**：周期 `gcInterval = 1s`，把「无引用」的 Reclaiming 段转 Free 入池 —— `internal/metastore/segments.go:43`、`internal/metastore/segments.go:135-151`。
5. **锁序**：`allocator.mu` → `segmentManager.mu` —— `internal/metastore/segments.go:26`。

其余实现细节：

- 回收时 `ReclaimSeq++` 记一代 —— `internal/metastore/segments.go:354`。
- `Free` / `Reclaiming` 段被写入新对象 → **重新激活**（Reclaiming 从 GC 候选退出）；`Free` 还需先移出空闲池 —— `internal/metastore/segments.go:184-190`。
- `markCompacting` 仅 `Full` 段可进搬移 —— `internal/metastore/segments.go:298-310`。
- **重启自愈**（`rebuild` 第 3 步）：搬移中断 / 未收尾的 `Compacting` 段以重建后的计数收口 —— `AliveCount>0` 回退 `Full`（下轮由搬移任务重新选中，幂等续搬），`AliveCount==0` 直接转 `Reclaiming` —— `internal/metastore/segments.go:89-101`。
- **不一致自愈**：段标记 `Free` 但 mapping 仍引用它 → 以 mapping 为准回退 `Active` 并移出空闲池 —— `internal/metastore/segments.go:116-120`。
- 重建存活计数时**丢弃持久化的 `AliveCount`**（置 0），避免与全量扫描双重累加 —— `internal/metastore/segments.go:71-72`。
- `popFree` 在持久化失败时**回滚**：段状态改回 `Free` 并放回池头 —— `internal/metastore/segments.go:324-329`。
- `reclaimOnce` 在持久化失败时返回 0，段留在内存态 `Free`，下轮 GC 重试写入 —— `internal/metastore/segments.go:365-368`。
- `Ref`/`Unref` 对**未知 segmentID 静默忽略**（`e == nil` 直接返回），`Unref` 还额外要求 `refCount > 0` 才减 —— `internal/metastore/segments.go:246-261`。

## 四、分配器（allocator）

存在理由写在类型注释上 —— `internal/metastore/kv_pebble.go:336-337`：

```go
// allocator 原子管理顺序写游标（curSeg/curOff）与游标持久化。持有独立锁，
// 使「申请偏移」成为廉价原子操作，真正的设备写由调用方在锁外执行，从而支持并发写不同偏移。
```

- 游标**懒加载**：首次 `AllocateSegment` 时才从持久化游标恢复，无需在启动时读取 —— `internal/metastore/kv_pebble.go:41-42`、`internal/metastore/kv_pebble.go:370-382`。
- 段滚动策略 —— `internal/metastore/kv_pebble.go:423-427`：

```go
// 段滚动策略（保持磁盘顺序写）：
//   - 下一段未使用或处于 Free：顺序滚动（正常路径，行为与 v1 一致）；
//     用户路径滚动上限为 SegmentCount−reserveSegs（预留缓冲段不参与）；
//   - 下一段已被占用（Active/Full/Reclaiming）或游标到顶：从空闲池取 Free 段复用（游标回跳）；
//   - 空闲池为空：ErrNoSpace。
```

- **预留段 `reserveSegs = 2`**：最后 2 个段不参与用户写路径的顺序滚动，仅 `AllocateSegmentReserve` 可动用；保证用户写满时搬移仍有落点，避免死锁。占 2048 段中 2 段 ≈ 0.1% 容量 —— `internal/metastore/kv_pebble.go:67-70`。
- 切换段时**旧段标记 Full，新段激活** —— `internal/metastore/kv_pebble.go:429`、`internal/metastore/kv_pebble.go:389-411`。
- `AllocateSegmentBatch` 一次锁定分配器、逐项 `allocOneLocked` 推进游标，**全程只做一次游标持久化**（摊薄 N 次 sync 写）—— `internal/metastore/kv_pebble.go:454-456`、`internal/metastore/kv_pebble.go:465-478`。
- 普通路径每次分配都单独持久化游标 —— `internal/metastore/kv_pebble.go:443-445`。

## 五、CAS 搬移：`ierr.ErrConflict` 的正确用法

**唯一正确姿势：把 `ierr.ErrConflict` 当控制信号 — 跳过该 key，下轮重扫；不要原地重试同一次 CAS。**

接口层的措辞 —— `internal/metastore/store.go:63-66`：

```go
	// MoveMapping 条件写（CAS）搬移对象映射：仅当当前 mapping 与 old 完全一致时，
	// 原子地改为 new 并转移段存活计数（new 段 +1、old 段 −1，同一 WriteBatch）。
	// 不一致（key 不存在/已被并发 Put/Delete 改变）返回 ierr.ErrConflict，不修改任何数据。
	MoveMapping(ctx context.Context, key string, old, new ObjectMeta) error
```

实现层以 **pebble 为权威**校验（不信任内存缓存），再单 batch 切换 —— `internal/metastore/kv_pebble.go:504-533`。两个冲突出口：key 不存在 `internal/metastore/kv_pebble.go:513`，当前值与 `old` 不等 `internal/metastore/kv_pebble.go:520`。

调用方（Compactor）的处理 —— `internal/storage/compact.go:150-157`：

```go
		for _, key := range cd.keys {
			n, err := c.moveObject(ctx, key)
			if err == ierr.ErrConflict {
				continue // 并发 Put/Delete 改动该 key，跳过（下轮重扫）
			}
			if err != nil {
				return moved, err
			}
```

- `moveObject` 对 `ierr.ErrNotFound` 也返回「跳过」（对象已被并发删除）—— `internal/storage/compact.go:175-180`。
- CAS 冲突时**数据未丢**：新位置的数据已落盘，只是映射没切；这些孤儿块由 segment GC 兜底回收 —— `internal/storage/compact.go:202-204`。
- `ErrConflict` **不暴露给客户端** —— re-export 里被明确排除，因为它是 compaction 内部的 CAS 控制信号 —— `internal/rpcclient/reexport.go:30`。错误体系全貌见 `.trellis/spec/architecture/error-model.md`。

## 六、Compaction 的实际行为

配置默认值 —— `internal/storage/compact.go:26-34`：60s 低频扫描、单段空洞率 ≥ 0.8 或段水位 ≥ 0.8 触发、每轮搬移 ≤ 512 对象。字段语义见 `internal/storage/compact.go:19-24`。该配置对应一份仓库外文档《segment 级 Compaction（数据迁移）设计方案》—— `internal/storage/compact.go:17-18`。

Compactor 的定位 —— `internal/storage/compact.go:36-38`：

```go
// Compactor 后台段压缩器：把高空洞 Full 段（含中断未完成的 Compacting 段）的存活对象
// 搬移到新位置（可动用预留缓冲段），搬空后旧段计数归零自动转 Reclaiming，
// 由现有后台 GC 回收入池复用。与 GC 为两条独立惰性链，互不冲突。
```

一轮 `compactOnce` 的流程 —— `internal/storage/compact.go:82-168`：

1. **一趟全扫 mapping** 同时得到各段存活字节与对象 key 清单 —— `internal/storage/compact.go:83-100`。
2. 判全局水位 `force` —— `internal/storage/compact.go:102`。
3. 候选条件是 `Full` **或上次中断的 `Compacting`**，且（空洞率达标 **或** force）—— `internal/storage/compact.go:104-123`；空洞率 `hole = 1 − aliveBytes/segSize` —— `internal/storage/compact.go:119`。
4. 候选**空洞率降序**（空洞最大者先搬，单位搬移释放空间最多），同率按段号升序保证确定性 —— `internal/storage/compact.go:124-130`。
5. 搬移前若段仍为 `Full` 则先 `MarkCompacting` —— `internal/storage/compact.go:145-149`。
6. 限额 `MaxMovePerRound` **跨段合并记账**（不是每段各 512）—— `internal/storage/compact.go:132-136`、`internal/storage/compact.go:158-161`。

单个对象的搬移顺序 —— `internal/storage/compact.go:170-206`：

- 顺序是：读旧位置（`ReadAt` 自带 Ref/Unref，防回收竞态）→ 分配新位置 → **写设备（先数据）** → **CAS 切映射（后元数据）** —— `internal/storage/compact.go:170-172`。
- 新落点用 `AllocateSegmentReserve`；源段为 Full/Compacting，分配器不会选中它 —— `internal/storage/compact.go:194-198`。
- 读到的长度不足 `meta.Size` 时报错返回（不能搬一个残缺对象）—— `internal/storage/compact.go:190-192`。
- 读出的池化缓冲 `defer bufpool.Put(buf)` 归还 —— `internal/storage/compact.go:189`。

`compactOnce` 的可重入性：注释声明「可并发调用（同一 Go 进程内唯一定时器驱动）」—— `internal/storage/compact.go:81`；`Compactor.Stop` 通过 `close(c.stop)` + `wg.Wait()` 保证 `run` 退出后才返回 —— `internal/storage/compact.go:57-61`。

## 相关文件

- 缓冲所有权（`ReadAt` 返回值必须 `bufpool.Put`）、锁纪律与 `ErrConflict` 的并发背景：见 [buffer-and-concurrency.md](./buffer-and-concurrency.md)。
