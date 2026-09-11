# segment 级 GC 设计文档（当前实现梳理）

> 本文档基于当前代码实现梳理 segment 级垃圾回收（对象删除驱动的段回收与复用）的完整设计。
> 对应代码：
> - `internal/metastore/segments.go` — segmentManager（GC 核心）
> - `internal/metastore/kv_pebble.go` — pebbleStore / allocator（持久化、游标、启动恢复）
> - `internal/metastore/store.go` — Store 接口（GC 相关语义约定）
> - `internal/metastore/meta.go` — SegmentMeta / SegmentState / ObjectMeta / WriteCursor 数据模型
> - `pkg/taihu/storage.go` — Storage 读路径 Ref/Unref 接入点
> - `internal/device/device.go` — Device.Delete 占位
> - 测试：`internal/metastore/segments_test.go`、`pkg/taihu/storage_test.go`

---

## 1. 背景与目标

存储采用 **append-only** 写模式：对象写入裸设备的固定 segment 布局，删除对象只移除 key → 位置映射，**物理数据不做就地擦除**。若不回收，长期运行会产生大量"孤儿段"——段内对象全部被删除、物理空间却永久占用，导致磁盘写满后无法写入新对象。

本设计的 GC 以 **segment（8GB 大段）为回收粒度**，核心目标：

| 目标 | 说明 |
|------|------|
| 段回收 | 段内对象计数（AliveCount）归零的段可整体回收 |
| 段复用 | 回收的段进入空闲池，供分配器在游标到顶时取用，保持"段内顺序写"的磁盘访问模式 |
| 并发安全 | 回收与在途读不产生竞态（读引用计数）；计数与 mapping 原子保持一致（WriteBatch） |
| 崩溃一致 | 任何时刻崩溃，重启后计数、段状态、空闲池可由持久化状态自愈 |

设计约束与硬性要求：
- **锁自由化**：GC 只持 segmentManager.mu（与 allocator.mu 保持 `allocator.mu → segmentManager.mu` 单向锁序，无反向），不涉及设备 IO；
- **GC 为后台低频操作**（1s 周期），不得进入稳态读写热路径；
- **不立即物理擦除**：`Device.Delete` 为占位实现（见 §9），物理空间仅在段整体回收复用后由新对象覆写。

---

## 2. 物理布局与命名空间

### 2.1 物理布局（internal/layout/layout.go）

```
整盘 ≈ 16TB = 2048 段 × 8GB
SegmentSizeBytes = 8GB
SegmentCount     = 2048
BlockSize        = 4KB（对齐粒度）
```

对象落点 = `(segmentID, 段内偏移, 大小)`，段内偏移与物理读都做 4K 对齐。

### 2.2 元数据命名空间（pebble，单 keyspace 前缀隔离）

pebble 为单 keyspace，用 key 前缀隔离两个逻辑命名空间：

| 命名空间 | 前缀 | 内容 | key 示例 |
|----------|------|------|----------|
| mapping | `m\x00` | 用户 key → ObjectMeta | `m\x00<key>` |
| state | `s\x00` | 写游标 + 段状态 | `s\x00cursor`、`s\x00seg/<8B segmentID>` |

mapping 切分为缓存与持久化两层：
- **持久层**：pebble（真实源）；
- **加速层**：`metaCache`（有界分片 LRU，256 shard，预算 1GB，read-through），由 pebble 重建，失败不丢持久性。

---

## 3. 数据模型（internal/metastore/meta.go）

### 3.1 ObjectMeta（mapping 的 value）

```go
type ObjectMeta struct {
    SegmentID int64 // 所在段
    Offset    int64 // 段内起始偏移（恒 4K 对齐）
    Size      int64 // 逻辑大小（不含 4K 填充）
}
```

### 3.2 SegmentMeta（state 的 value，key = seg/<id>）

```go
type SegmentMeta struct {
    State      SegmentState // 生命周期状态
    AliveCount int64        // 现存对象数（内存为准，持久化为参考）
    ReclaimSeq int64        // 回收世代号（每回收一轮 +1）
}
```

### 3.3 SegmentState 状态机

```go
const (
    SegmentStateFree        // 空闲，可分配
    SegmentStateActive      // 正在顺序写入
    SegmentStateFull        // 已写满，仅读
    SegmentStateReclaiming  // 待回收（计数归零）
)
```

另有隐式状态 **None**（`segs` 中无记录 = 从未写入）。

---

## 4. 段状态机与生命周期

### 4.1 状态转移图

```
  首次分配                           游标写出段尾
 None ─────► Active ────────────────────────► Full
             │  ▲                            │
  被写新对象  │  │ 被写新对象                   │ 对象全部删除（AliveCount==0）
  (Free→Active│  │ (Reclaiming→Active,        ▼
   移出池)     │  │  退出 GC 候选)          Reclaiming ──── GC 确认 refCount==0 ────► Free（入池）
             └──┴───────────── 分配器 popFree 复用 ─────────────▲────────────────────┘
```

### 4.2 状态定义与转换条件

| 状态 | 含义 | 进入条件 | 离开条件 |
|------|------|----------|----------|
| None | 从未写入 | 启动后无记录 | 首次分配 → Active（`activate`/`ensureLocked`） |
| Active | 正在顺序写入 | 分配器选中 / 被写新对象 | 游标写出段尾 → Full（`markFull`）；对象删光 → Reclaiming |
| Full | 已写满，仅读 | 游标滚动离开该段 | 对象删光 → Reclaiming |
| Reclaiming | 待回收（计数 0） | `AliveCount==0` 时由 Active/Full 转入 | ① GC 确认 `refCount==0` → Free 入池；② 被写新对象 → 退回 Active（从 GC 候选退出） |
| Free | 空闲可复用 | GC 回收（`reclaimOnce`） | 分配器取池（`popFree`）或新对象直接写入 → Active（移出池） |

关键规则：
- **Reclaiming 可被复活**：Free/Reclaiming 段若被新对象写入，立即转 Active——Free 移出池、Reclaiming 退出 GC 候选，防止 GC 回收"正在被写入"的段。
- **GC 只回收 `Reclaiming && refCount==0`** 的段，这是读安全的最后屏障。

---

## 5. 核心机制

### 5.1 存活计数 AliveCount（回收判据）

- **维护方式**：段内现存对象数，内存态维护于 `segEntry.meta.AliveCount`；
- **原子性**：每次 `PutMapping` / `DeleteMapping` 时，mapping 写入与段计数更新位于**同一个 pebble WriteBatch** 原子提交（`putObject` / `delObject`），保证"计数与映射要么同时生效，要么同时不生效"；
- **覆盖写**（同 key 重复 Put）：新段计数 +1、旧段计数 −1；新旧同段则计数保持不变，不泄漏空间计数；
- **归零**：`AliveCount` 递减到 ≤0 时钳位为 0，段立即转 Reclaiming；
- **权威来源**：启动时全量扫描 mapping 重建计数（§7.2），持久化的 AliveCount 仅作参考、重启动时被丢弃。

### 5.2 读引用计数 refCount（GC 与在途读的竞态防护）

**竞态场景**：GC 把 Reclaiming 段转 Free 并投入空闲池 → 分配器立即复用该段并覆写数据 → 仍在途的读读到错乱数据。

**解法**：段级读引用计数（内存态，在 `segEntry.refCount`）：

- `Ref(segmentID)`：读开始前调用，计数 +1；
- `Unref(segmentID)`：读结束后调用，计数 −1；
- `reclaimOnce` 仅回收 `State==Reclaiming && refCount==0` 的段；
- 对未知段（从未写入，无记录）Ref/Unref 为空操作，不产生段记录。

**接入点**：唯一在 `pkg/taihu/storage.go` 的 `Storage.ReadAt`——`device.ReadAt` 前后对称调用 `RefSegment` / `UnrefSegment`（`defer` 保证配对）。所有服务端读（含 RPC 读）最终都走 `Storage.ReadAt`，因此一个接入点覆盖全部读路径。

### 5.3 空闲段池 free（复用供给）

- 结构：`free []int64`，**FIFO**（先进先出）；
- 内存态，状态变更随 pebble 持久化（段状态落盘即池的持久化依据）；
- 入池：GC 回收（`reclaimOnce`）时入队尾；
- 出池：分配器取用（`popFree`）时取队头并同步转 Active 持久化；
- 若被写新对象直接命中（`putObject` 写 Free 段），从池中移除（`removeFree`，池小，顺序扫描可接受）。

### 5.4 回收世代号 ReclaimSeq

段每被回收一轮 `ReclaimSeq++`，用于区分"同一段的不同世代"（调试、世代一致性验证、后续物理重写时避免读到旧世代垃圾数据）。当前主要用于观测与测试断言。

---

## 6. 关键流程

### 6.1 写入路径 Put

```
Storage.Put(key, size, in)
 │ 1. db.AllocateSegment(size)          // 原子申请写位置（§6.4），返回 (seg, off)
 │ 2. dev.Append(seg, off, size, in)    // 写设备数据（锁外，并发 Put 可写不同偏移）
 │                                       // 顺序保证：先设备后元数据，避免「有映射无数据」
 └ 3. db.PutMapping(key, meta)          // §6.2，同一 WriteBatch 原子写 mapping + 段计数
```

### 6.2 PutMapping → putObject（写映射 + 计数更新，原子）

```
putObject(key, meta, old):
  batch: Set(mapping/key → meta)
  锁 segmentManager.mu
    e = ensureLocked(meta.SegmentID)          // 不存在则建（默认 Active）
    若 old 不存在或不同段: AliveCount++
    若 e.State ∈ {Free, Reclaiming}:
      Free → removeFree(段移出池)
      State = Active                          // Free 复活 / Reclaiming 退出 GC 候选
    batch: Set(state/seg/<新段> → e.meta)
    若 old 存在且不同段:
      旧段 AliveCount--; ≤0 → 钳 0 + State=Reclaiming
      batch: Set(state/seg/<旧段> → oe.meta)
  解锁
  Apply(batch, Sync:true)                    // 原子提交
```

### 6.3 删除路径 Delete → delObject（删映射 + 计数归零判定，原子）

```
DeleteMapping(key):
  old = GetMapping(key)                      // 不存在 → ErrNotFound
  delObject(key, old):
    batch: Delete(mapping/key)
    锁 segmentManager.mu
      段 e.AliveCount--
      ≤0 → 钳 0 + State=Reclaiming（等待 GC 确认无在途读者）
      batch: Set(state/seg/<段> → e.meta)
    解锁
    Apply(batch, Sync:true)
  缓存失效 cache.del(key)
```

### 6.4 段滚动与复用 AllocateSegment（游标 + 空闲池）

```
AllocateSegment(size):
  锁 allocator.mu
    懒加载持久化游标（首次）
    aligned = Align4k(size)
    if 游标段放不下 (curOff+aligned > 8GB):
      旧段 markFull（curOff>0 时）
      if 下一段 < 2048 且 canUse(下一段)（无记录或 Free）:
        ── 顺序滚动（正常路径，保持磁盘顺序写）──
        段++/off=0 → activate(下一段)(Free→Active 移出池或建记录) → 持久化游标
      else:
        ── 取空闲池复用（游标回跳）──
        seg, ok = popFree(ctx)               // 池空 → ErrNoSpace
        curSeg/curOff = seg, 0 → 持久化游标
    返回 (seg, off)；curOff += aligned；持久化游标
```

滚动策略总结：

| 场景 | 行为 |
|------|------|
| 段内放得下 | 原地推进游标 |
| 放不下、下一段未用/Free | 顺序滚动（正常路径） |
| 放不下、下一段被占用或游标到顶 | 取空闲池复用（游标回跳） |
| 空闲池为空 | `ErrNoSpace` |

切换段时旧段标记 Full、新段激活，每次换段持久化游标——崩溃后游标不回退。

### 6.5 后台 GC 循环 reclaimOnce

```
run():  // Open 时启动，gcInterval = 1s
  for ticker:
    锁 segmentManager.mu
      cands = {id, e | e.State==Reclaiming && e.refCount==0}
      if 无候选: 解锁 continue
      for each cand:
        State = Free; AliveCount = 0; ReclaimSeq++
        入池 free 队尾
    解锁
    batch: Set(state/seg/<每个候选> → new meta)
    Apply(batch, Sync:true)
    失败 → 内存态保持 Free（池内），下轮 GC 重试持久化（最终一致）
```

Close 时 `stopGC` 关闭 stop chan 并等待 goroutine 退出。

### 6.6 读路径保护 Ref/Unref

```
Storage.ReadAt(key, off, size):
  meta = GetMapping(key)
  校验边界（InvalidRange / EOF 截断）
  RefSegment(meta.SegmentID)
  defer UnrefSegment(meta.SegmentID)         // 覆盖整个设备读区间
  data = dev.ReadAt(seg, 对齐后 dstart, dlen) // 4K 对齐读，bufpool 池化缓冲
  ...
```

即使读在途期间该段被删除、计数归零、GC 把段转 Free，`refCount > 0` 也会阻塞本轮回收；等读结束 `refCount` 归零后，下一轮 GC（最迟 1s 后）才真正回收。

---

## 7. 启动恢复（rebuild）

`Open` 时 `segmentManager.rebuild` 分两步，全量扫描成本一次性，换来自洽的计数基准：

### 7.1 段状态与空闲池恢复

- 区间扫描 `s\x00seg/` 前缀，解码每个 `SegmentMeta`；
- **丢弃持久化 AliveCount**（置 0），避免与下一步映射扫描双重累加；
- `State==Free` 的段放入空闲池。

### 7.2 存活计数重建（权威）

- 全量扫描 `m\x00` mapping，对每个对象所在段 `AliveCount++`；
- **不一致自愈**：若扫描中发现"段标记 Free 但 mapping 仍有对象"（崩溃于回收落盘前后），以 mapping 为准——状态回退 Active，并从空闲池移除该段，防止复用未删干净的空间。

### 7.3 游标恢复

写游标由分配器首次 `AllocateSegment` 时懒加载（`loadCursor`），非启动时读取。

---

## 8. 并发模型、锁序与崩溃一致性

### 8.1 锁与数据归属

| 锁 | 守卫 | 持有者 |
|----|------|--------|
| `allocator.mu` | 写游标 curSeg/curOff 及滚动决策 | emitter：`AllocateSegment`（每次 Put 的入口） |
| `segmentManager.mu` | segs map、refCount、free 池 | `putObject`/`delObject`/`reclaimOnce`/`Ref`/`Unref`/`activate`/`markFull`/`popFree`/`canUse` |
| `shard.mu`（256 分片） | metaCache 各分片 LRU | `GetMapping`/`PutMapping`/`DeleteMapping`/`LoadCache` |

**锁序约束**：`allocator.mu → segmentManager.mu`（`AllocateSegment` 持 allocator.mu 期间调用 `markFull`/`canUse`/`activate`/`popFree`）；GC / PutMapping / DeleteMapping 路径只持 `segmentManager.mu`，**无反向加锁**，无死锁环。

### 8.2 一致性保证

| 层面 | 机制 |
|------|------|
| 计数 ↔ mapping | 同一 WriteBatch 原子提交（`putObject` / `delObject`） |
| 段状态 ↔ mapping | 同上 batch 内一并落盘 |
| 设备数据 ↔ 元数据 | 写路径先设备后 mapping（§6.1） |
| 回收落盘失败 | 内存态已 Free/入池，下轮 GC 重写（最终一致，可容忍） |
| 崩溃后任意状态 | rebuild 以 mapping 为准重建计数 + 自愈 Free 不一致 |
| 持久化粒度 | mapping / cursor / seg 状态均 `Sync:true`（先数据后元数据，游标不回退） |

### 8.3 读安全窗口

GC 回收必需要素：段处于 Reclaiming（即已无映射引用）**且** `refCount==0`（无在途读）。两者都由 segmentManager.mu 串行化判定，杜绝"回收后立即复用 → 迟到读读错数据"。

---

## 9. 代码地图

| 文件 | 职责 | 关键函数 |
|------|------|----------|
| `internal/metastore/segments.go` | segmentManager：生命周期、GC 循环、池 | `rebuild` / `run` / `stopGC` / `putObject` / `delObject` / `Ref` / `Unref` / `canUse` / `activate` / `markFull` / `popFree` / `reclaimOnce` / `stats` |
| `internal/metastore/kv_pebble.go` | pebbleStore、allocator 游标、Store 实现 | `Open` / `PutMapping` / `DeleteMapping` / `AllocateSegment` / `SegmentStats` / `Close` |
| `internal/metastore/store.go` | Store 接口与 GC 语义约定 | `GetSegment` / `PutSegment` / `RefSegment` / `UnrefSegment` / `SegmentStats` |
| `internal/metastore/meta.go` | 数据模型与编解码 | `SegmentMeta` / `SegmentState` / 各类 `encode`/`decode` |
| `internal/metastore/cache.go` | mapping 加速缓存（有界分片 LRU） | `metaCache.get/put/del` |
| `internal/layout/layout.go` | 物理布局与 4K 对齐 | `SegmentSizeBytes` / `SegmentCount` / `Align4k` |
| `pkg/taihu/storage.go` | Storage 层；读路径 Ref/Unref 接入点 | `Put` / `ReadAt` / `Delete` / `SegmentStats` |
| `internal/device/device.go` | 设备 IO；`Delete` 占位 | `Device.Delete`（返回 nil，物理擦除延迟到段复用由新对象覆写） |
| `internal/metastore/segments_test.go` | GC 单元测试（12 例） | 见 §10 |
| `pkg/taihu/storage_test.go` | Storage 集成测试（含 GC 生命周期 2 例） | 见 §10 |

---

## 10. 测试覆盖

### L1 单元（internal/metastore，12 例）

| # | 用例 | 验证点 |
|---|------|--------|
| 1 | TestAliveCountAndReclaim | 计数维护；删光→Reclaiming；GC→Free 入池 |
| 2 | TestRefBlocksReclaim | 在途读引用阻塞回收，Unref 后可回收 |
| 3 | TestOverwriteSameKey | 覆盖写扣减旧段计数（新旧不同段），不泄漏 |
| 4 | TestAllocatorReuseFreeSegment | 游标到顶取池复用，继续顺序写 |
| 5 | TestRebuildFromMapping | 重启后计数从 mapping 重建，AliveCount 正确 |
| 6 | TestFreeSegmentReactivateOnPut | Free 段被写新对象→Active、移出池 |
| 7 | TestReclaimSeqIncrements | 多轮回收世代号递增 |
| 8 | TestBackgroundGCPeriodic | 后台 GC 周期自动回收（Open 即启动） |
| 9 | TestRefOnMissingSegment | 未知段 Ref/Unref 空操作 |
| 10 | TestConcurrentPutDeleteGC | 并发写/删/引用/回收下计数与 mapping 对账一致（-race） |
| 11 | TestFullLifecycleReuse | 写→删→GC→复用全链路，分配偏移正确 |
| 12 | 原有用例回归 | 既有 Store 用例全部保留 |

### L2 集成（pkg/taihu 新增 2 例）

| # | 用例 | 验证点 |
|---|------|--------|
| 13 | TestStorageDeleteReuseLifecycle | 真实 Storage：写→删→后台 GC 自动回收→复用，数据往返正确 |
| 14 | TestStorageConcurrentPutDeleteRead | 并发 Put/Delete/ReadAt 无数据错乱，无段卡在 Reclaiming（-race） |

**既有结论**：128.11（linux/arm64，gcc 10.3.1）`-race` 全绿（26 例 PASS，无 DATA RACE）；128.12/128.13 gcc 7.3.0 缺 aarch64 原子内建符号无法链接 TSan。

### L3 端到端（真机裸盘）

写 N×4MiB → 全量删（观察段→Reclaiming）→ 等后台 GC（观察转 Free 入池、SegmentStats）→ 重写验证复用与数据正确 → 重启验证 rebuild 一致。

---

## 11. 已知限制与演进方向

| 限制 | 说明 | 演进方向 |
|------|------|----------|
| 部分填充段不压缩 | 仅整段对象删光才回收，Full 但计数>0 的段存在空间空洞 | 段内 compaction：存活对象搬移到新段后整段回收 |
| 回收粒度粗 | 段 8GB，回收复用为整段级别 | 定长子块/对象级分配与回收（提升空间利用率，代价是元数据与碎片管理复杂化） |
| 空闲池仅内存态 | 极端崩溃时由 rebuild 自愈 | 可将池显式持久化（当前以段状态落盘代替） |
| 物理擦除延迟 | `Device.Delete` 为占位，物理空间仅在复用后被新数据覆写 | 需要安全擦除/审计时实现惰性物理擦除（配合 ReclaimSeq 防跨世代误读） |
| GC 与并发写竞争 | GC 判定与写入由同一把 segmentManager.mu 串行化 | 若 GC 成为热点可改无锁/细粒度状态机 |
| 单调递增游标语义弱化 | 上游到顶后退回空闲池（游标回跳），不再严格单调 | 文档/观测层明确"段内顺序写、段间可回跳"的语义 |

---

## 12. 相关文档

- `doc/20260911_6c5297e segment级GC设计与测试方案.md` — GC 引入时的设计与测试方案（commit 6c5297e）
- `doc/202609091720_设计文档_v3.md`（及 v2/v1）— 仓储整体演进设计
- 性能基线与 GC 无关性参考：读写性能测试报告系列