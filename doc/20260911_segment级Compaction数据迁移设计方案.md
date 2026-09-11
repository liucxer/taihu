# segment 级 Compaction（数据迁移）设计方案

> 状态：**方案评审版**（仅设计，未实现）
> 解决场景：部分填充段（如删了 80% 的段）的空间空洞回收
> 前置文档：[20260911_segment级GC设计文档.md](./20260911_segment级GC设计文档.md)
> 涉及代码（实现时）：`internal/metastore/segments.go`、`internal/metastore/meta.go`、`internal/metastore/store.go`、`pkg/taihu/storage.go`，新增 `pkg/taihu/compact.go`

---

## 1. 问题与背景

现有 GC 只回收 **AliveCount==0** 的段（[segments.go 的 reclaimOnce](file:///Users/liucx/gopath/src/github.com/liucxer/taihu/internal/metastore/segments.go#L290-L325)）：

- 段内对象删光 → 转 Reclaiming → refCount==0 → Free 入池复用；
- **删了 80%、还剩 20% 存活对象的段**：AliveCount=20%>0，永不进 Reclaiming → 8GB 物理空间被 20% 对象永久占用 → 空洞随写入累积，最终 `ErrNoSpace`。

**目标**：把"高空洞段"的存活对象搬移到新位置，搬空后整段走现有 GC 回收链路，物理空间重新可写。

**非目标**：不做限定子块/对象级就地管理；不做物理擦除（覆写语义不变）。

---

## 2. 方案总览

后台任务扫描选段 → 逐对象搬移（读旧、写新、原子切映射）→ 搬空后段 AliveCount==0 → 自动进入现有 Reclaiming → 现有 GC 收尾。**回收与复用完全复用现有链路**，本方案只新增"搬移"动作。

```
                       空洞率 ≥ 阈值 → 标记 Compacting
 Full(高空洞) ──────────────────────────────► Compacting
                                                │ 逐对象搬移（新段+1/旧段-1 原子）
                                                ▼ AliveCount 归零
                                           Reclaiming ── 现有后台 GC（refCount==0）──► Free 入池
```

---

## 3. 状态机扩展

在现有 4 态（Free / Active / Full / Reclaiming）上新增：

```go
const (
    SegmentStateFree        SegmentState = iota // 空闲，可分配
    SegmentStateActive                          // 正在顺序写入
    SegmentStateFull                            // 已写满，仅读
    SegmentStateReclaiming                      // 待回收（计数归零）
    SegmentStateCompacting                      // 搬移中（新增）
)
```

| 进入条件 | 离开条件 |
|----------|----------|
| 后台选段：Full 且空洞率 ≥ 阈值 | 存活对象全部搬移 → AliveCount==0 → Reclaiming（由 delObject/putObject 旧段计数递减到 0 自动触发） |

**对现有逻辑零侵入**：
- allocator 只认 `canUse`（Free）/`popFree`（Free），Compacting 天然被排除，不会被分配；
- `markFull`/`activate` 只操作游标所在段，不碰 Compacting 段；
- Reclaiming 的进入判定（`AliveCount<=0` 置 Reclaiming）在 `putObject`（旧段计数递减分支）与 `delObject` 中已有，搬移最后一个对象后计数归零**自动转 Reclaiming**，无需新代码。

**崩溃自愈（rebuild 扩展，一行分支）**：
```
rebuild 扫描 seg/ 恢复状态后：
  若 State==Compacting：
    AliveCount 从 mapping 重建后
      > 0 → 视为中断的搬移 → 回退 Full（下轮重新选中，幂等续搬）
      ==0 → 置 Reclaiming（直接走回收）
```
Compacting 状态落盘（随 putObject 的 batch 或单独 Set），保证重启后能区分"搬了一半的段"。

---

## 4. 搬移协议

前置：**mapping 无反向索引**（key→meta 单向），枚举段内对象需 `IterMapping` 全扫过滤（`meta.SegmentID==候选段`）。后台低频，全扫可接受；极端数据量再引入增量 seg→keys 索引（见 §8 权衡）。

### 4.1 搬移单个对象（原子切映射）

对候选段内每个存活对象执行一次"搬移事务"，**完全复用现有 `putObject(ctx, key, newMeta, old)`**：

```
① 重读最新 mapping（key）
   ← 若 key 已不再指向本段（被用户覆盖写/删/已被其他任务搬走）→ 跳过（幂等）
② Storage.ReadAt(key)                // 自带 Ref/Unref，bufpool 池化缓冲，用毕归还
③ db.AllocateSegment(size) → (seg', off')   // 新段，走正常分配
④ dev.Append(seg', off', size, buf)         // 先写数据（先数据后元数据，沿用现有顺序）
⑤ db.PutMapping(key, {seg', off', size})    // putObject：新段 AliveCount+1、本段 AliveCount-1，
                                            // 同一 WriteBatch 原子；cache 自动回填
```

> putObject 的 `old` 参数（旧对象所在段计数 −1）**正是为跨段搬移设计的**，见 [kv_pebble.go PutMapping](file:///Users/liucx/gopath/src/github.com/liucxer/taihu/internal/metastore/kv_pebble.go#L109-L129)。

### 4.2 与用户并发写的冲突（关键风险）

搬移第①步"重读"与第⑤步"写 mapping"之间存在窗口：若用户并发 `Put` 同 key，用户新 mapping 可能被搬移的 putObject（带旧 old）覆盖 → 丢用户数据。

**解法（二选一，实现时定）**：
- **A. 乐观重试（推荐，零侵入）**：putObject 升级为条件写——`old` 与当前 mapping 不一致则搬移失败跳过，下轮重扫重试。需要给 putObject 增加并发校验语义。
- **B. 搬移期间 key 级短锁**：候选段搬移时对该段内对象集中加锁（侵入读路径，不推荐）。

选择 A：数据不丢、实现简单；代价是极端竞争下搬移可能反复失败（重试有退避）。

### 4.3 在途读的保护

- 新读：读到最新 mapping → 指向新位置，天然正确；
- 在途旧读：旧位置读者由 `refCount` 记录，旧段要等 refCount==0 才能转 Free → 现有 `reclaimOnce` 已保证，**本方案零改动**。

### 4.4 落点管理与乒乓防护（全空洞场景必备）

搬移必须落在**可写的目标位置**。全段高空洞时，若搬移目标裸露地走普通 `AllocateSegment`，可能把对象搬进"下一个即将被搬空的段"，形成来回倒腾（乒乓）。**落点管理**规则：

1. **收纳段集合**：compaction 维护一组"落点段"（可含 Free 段、低空洞 Active 段、或专用新区段），搬移对象**只写收纳段**；
2. **源段永不作为目标**：候选源段（Compacting / 高空洞 Full）明确排除出落点集合；
3. **目标优先级**：Free 段 > 低空洞段 > 新段；收纳段一旦变高空洞即移出落点、加入源候选；
4. **链式腾空**：源段搬空转 Free 后，**立即加入落点集合**，成为下一个源段的落点——"搬一腾一"多米诺式推进，只需 1 个初始落点即可压缩任意多个源段（前提：存活总量 < 盘容量）。

收敛性：存活总量 D、容量 C、段数 N；压缩后占用 `ceil(D/C)` 个段，其余 `N − ceil(D/C)` 个段全部转 Free。每个对象最多被搬一次（幂等跳过已搬对象），无乒乓。

### 4.5 无落点兜底：over-provisioning 与段内就地压缩

若盘**已写满且无任何可写段**（全 Full、无 Free、无游标剩余），搬哪一段都没有落点 → 死锁。两条出路：

- **运维约束（必选，首选）**：Reserve **over-provisioning**——分配器永远保留 ≥2 个 Free 段不入池（`SegmentCount` 减少 2），专作搬移缓冲。成本 2/2048 ≈ 0.1% 容量，SSD/数据库行业惯例。有此保障即永远退回情形 A 的链式压缩。**该决策在 `AllocateSegment` 的 `ErrNoSpace` 边界（池底防御）实现**。
- **代码兜底（可选演进）**：**段内就地压缩**——选一个牺牲段，把段内存活对象在**段内**收紧到段头，段尾腾出连续空闲区，段重新可写 → 制造出第一个落点 → 再走链式压缩。改动大（段内空闲区间列表、分配器段内复用、与游标语义冲突），仅当不允许牺牲任何容量时评估。

---

## 5. 触发与调度

### 5.1 空洞率定义

```
空洞率(段) = (段内已分配字节 − 段内存活对象字节) / 段容量
```

- `段容量`：Full 段 = SegmentSizeBytes；Active 段 = 当前写偏移（一般不选，见下）；
- `存活对象字节 = Σ 该段对象 Size`：与枚举（IterMapping 过滤）一趟算完，无额外扫描。

### 5.2 选段规则

1. **选段条件**：
   - 只选 `Full` 段（Active 正在写、且 allocator 会自然滚走，不搬）；
   - 空洞率 ≥ `CompactionThreshold`（默认 0.8，即删了 80%+，可配置下降）；
   - 排除 Reclaiming / Compacting / Free / 无记录段。
2. **触发水位（强制压缩）**：**已用段数 / 总段数 ≥ 80%**（即 80% 的 segment 都已占用时）→
   立即进入压缩模式，不再等待单段空洞率达标，提前释放空间避免触底 `ErrNoSpace`；
3. **搬移顺序**：候选段按空洞率**降序**——**空洞率最大的段先搬**，单位搬移量释放的可用空间最多，
   最快缓解段水位；空洞率相同则按段号升序，保证确定性。

### 5.3 后台调度器（新增 `compact.go`）

```
run():  // Open 后随 GC goroutine 一并启动，独立 goroutine
  周期 tick（默认 60s，可配置；磁盘水位低时可缩短/加大力度）
  全局视角：维护平均空洞率 Σ(段空洞) / N、已用段占比、Free 段水位
    ① 已用段占比 ≥ 80% 或平均空洞率 ≥ 阈值 → 强制压缩（不等待单段触发）
    ② 每轮：落点优先（§4.4）→ 选候选段（Full 中空洞率降序，空洞率最大者先搬）
       → 搬移限额内执行
    ③ 落点耗尽（无 Free 可写段）→ 进入 §4.5 兜底策略（预留 ≥2 缓冲段防御）
  每轮搬移限额（N 对象 / M 字节，默认防抖，限速避免冲击稳态带宽）
  与现有 GC 的关系：互不可见、互不冲突——
    GC 收 Reclaiming 段，compaction 产 Reclaiming 段，两条独立惰性链
```

---

## 6. 崩溃一致性与恢复（汇总）

| 场景 | 保证 |
|------|------|
| 单对象搬移中断 | 新段数据+mapping 同事务已提交/未提交，旧段数据仍在（mapping 权威），读永远一致 |
| 搬了一半崩溃 | rebuild：Compacting 段 AliveCount>0 → 回退 Full → 下轮重选续搬（幂等：已搬对象重读时已指向新段，跳过） |
| 搬完未转 Reclaiming 崩溃 | rebuild 计数重建：AliveCount==0 的 Compacting 段 → Reclaiming → GC 收尾 |
| 搬移期间用户同 key 并发写 | 条件写失败 → 跳过重试（不丢数据） |
| 回收落盘失败 | 沿用现有：内存态已 Free，下轮重写（最终一致） |

---

## 7. 实现落点（接口草案）

### 7.1 新增

```go
// pkg/taihu/compact.go
type Compactor struct {
    st       *Storage
    interval time.Duration   // 扫描周期
    threshold float64        // 空洞率阈值
    perRound int64           // 每轮搬移字节上限（限速）
    stop     chan struct{}
}

// metastore 接口新增：
//   ListMappingOf(segmentID) ([]{key, ObjectMeta}, error)   // 按段枚举（无索引时 = IterMapping+过滤）
//   SetSegmentStateToCompacting(ctx, id) error               // 标记 Compacting 并落盘
```

### 7.2 修改

| 位置 | 改动 |
|------|------|
| `internal/metastore/meta.go` | 新增 `SegmentStateCompacting` 枚举值 |
| `internal/metastore/kv_pebble.go` | `putObject` 增加条件写（old 校验）或由外层重读兜底；rebuild 增加 Compacting 自愈分支 |
| `pkg/taihu/storage.go` / `NewStorage` | 可选：Open 时启动 Compactor（默认开/关可配置） |

### 7.3 测试方案

| 层 | 用例 |
|----|------|
| L1 | 状态机：Full→Compacting→(搬完)→Reclaiming；Compacting 不被 allocator 选中；rebuild 对 Compacting 的两分支自愈 |
| L1 | 搬移正确性：搬后 key 指向新段、旧段计数递减、数据往返一致 |
| L2 | Storage 集成：删 80% 段 → 触发搬移 → 旧段回收 → 空间可复用；搬移中并发读/写（-race） |
| L3 | 真机：灌满多段 → 删 80% → 观察 compaction → SegmentStats 验证 Free 段回归 → 重写成功 |

---

## 8. 权衡与可选优化

| 项 | 说明 |
|----|------|
| 反向索引 | 无索引：每轮 O(N) 全扫 mapping。增量维护 seg→keys 索引（随 putObject/delObject batch 一致更新）可降至 O(段内对象数)，但增加写入路径复杂度和 metaCache 一致性问题。**首版不做，观测到扫描成为瓶颈再升级** |
| 全空洞场景 | 全 70% 空洞并非无解（存活总量 < 容量即可压缩到 ceil(0.3N) 段），但必须满足 §4.4 落点管理与 §4.5 预留空间约束；否则触底 `ErrNoSpace` |
| over-provisioning | **必选**：分配器保留 ≥2 个搬移缓冲段。预留容量约 0.1%（2/2048），是链式压缩正常工作的前提 |
| 段内就地压缩 | 可选演进兜底，抵消 over-provisioning 依赖，但需重构分配器（段内空闲区间列表），成本高 |
| 搬移进度 | 无进度游标，每轮从段头重枚举已搬对象（幂等跳过）。段极大（8GB 满载 ~2K 对象×4MiB）时预留给进度持久化 |
| 写入放大 | 搬移 = 每对象一次读+一次写，加上后续新数据覆写旧段，放大 2-3×。阈值越高越值得搬（删 80% 场景搬 2 写 1 放大率 ≈ 1.25× 摊薄后极低） |
| 与 metaCache | 搬移走 PutMapping → cache.put 自动回填，无额外失效问题 |
| 调度默认值 | 首版建议默认开启但低频（60s + 限速），可通过配置关停，避免无预期后台负载 |

---

## 9. 结论

- 删 80% 的段**现无法回收**，需要 compaction；
- 方案**不需要新机制**：搬移复用 `putObject(old)` 的跨段计数、`Storage.ReadAt` 的引用保护、以及"计数归零自动转 Reclaiming→GC 回收"的现有链路；
- 主要新增：`Compacting` 状态 + 后台搬移调度器 + rebuild 自愈分支；
- 唯一需要慎重处理的：与用户同 key 并发写的条件写语义（§4.2，建议乐观重试）。