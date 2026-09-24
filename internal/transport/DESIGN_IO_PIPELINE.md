# IO 流水线设计：读/写/删对称批处理（Batched Read/Write/Delete Pipeline）

> 状态：设计稿（待评审）
> 范围：`internal/transport`（请求分发层），涉及 `internal/storage`、`internal/metastore`、`internal/device`、`internal/aio` 配套
> 目标：把"逐请求串行"的服务端 IO 改造为"队列 + worker 池 + 批量提交 + 批量回写"的对称流水线

## 1. 背景与动机

当前服务端（shm + rpc）每条 Get/Put/Delete 请求**独立处理**：
- 读：逐请求 `GetMapping`（单条 pebble Get）→ `ReadAtIntoBatch` 只在 `shmBatchReader` 攒批时合并，但映射查询仍是逐条同步，且攒批→批提交之间排队不彻底。
- 写：`PutData` 逐帧 `PutAppend`（单条 append）→ `PutEnd` 时单条 `PutCommit`；**无任何批量能力**。
- 删：`Delete` 逐 key `GetMapping` + `DeleteMapping`（单条 pebble 事务）；**无任何批量能力**。

瓶颈：pebble Get/Put/Delete 单条调用是同步 syscall（用户态往返），磁盘 IO 单条 `io_submit` 摊薄不了 syscall 开销；三者都吃 CPU 且限制吞吐。

改造后：**读/写/删各一条对称流水线**，请求先进队列，worker 池批量取当前队列内的全部任务（不足 N 个就全拿走，不做攒批等待；达到 N 是批量上限而非下限），批量查映射 / 批量提交内核 IO / 批量提交元数据，结果批量写回响应队列，由各流 goroutine 批量写帧。

## 2. 总体架构

```
                    ┌───────────────────────────── 服务端 ─────────────────────────────┐
 客户端 shm/rpc 帧    │                                                                 │
 ──────────┐         │   ┌─读请求队列──┐  ┌─读 worker 池(2-4)──┐  ┌─读响应队列(per-stream)┐│
   Get 请求 └────────────▶│ readTasks   │──▶│ drain 取批         │──▶│  结果回投             │└─▶ stream goroutine
   Put 请求 └────────────▶│ writeTasks  │   │ 1) BatchGetMapping │   │  按流分组            │    批量写帧 Flush
   Del 请求 └────────────▶│ deleteTasks │   │ 2) BatchRead(io)   │   └──────────────────────┘
                          └─────────────┘  └─┴─┴─┴─┴─┴─┴─┴─┴─┘──┴─┐
                                           ┌─写 worker 池(2-4)──▼──┐ ┌─写返回队列(per-stream)┐
                                           │ drain 取批            │ │  结果回投             └└─▶ stream goroutine
                                           │ 1) AppendBatch(io)    │─▶│  按流分组              批量写帧 Flush
                                           │ 2) BatchPutCommit     │ │                        ↑
                                           ├───────────────────────┤ ├────────────────────────┘
                                           │ 删 worker 池(2-4)     │ │ 删返回队列(per-stream)
                                           │ drain 取批            │─▶│  结果回投 → 批量写帧 Flush
                                           │ 1) BatchGetMapping     │ │
                                           │ 2) BatchDeleteMapping  │ │
                                           └───────────────────────┘ └────────────────────────┘
```

**六条队列**（与你的模型一一对应）：

| 队列 | 方向 | 生产者 → 消费者 | 内容 |
|------|------|----------------|------|
| 读请求队列 | 入 | 各流 goroutine → 读 worker 池 | ReadTask{key, pos, want, 响应缓冲} |
| 读响应队列 | 出 | 读 worker 池 → 各流 goroutine | 按流分组的批结果 |
| 写请求队列 | 入 | 各流 goroutine → 写 worker 池 | WriteTask{key, data, off, size} |
| 写返回队列 | 出 | 写 worker 池 → 各流 goroutine | 按流分组的批结果 |
| 删除请求队列 | 入 | 各流 goroutine → 删 worker 池 | DeleteTask{key} |
| 删除返回队列 | 出 | 删 worker 池 → 各流 goroutine | 按流分组的批结果 |

读/写/删 worker 池独立（数量、批量 N 均各自可配置）。

## 3. 读流水线

### 3.1 请求入队（各流 goroutine，[handleShmGet](file:///d:/workspace/nefs/taihu/internal/transport/server_shm_linux.go#L369) 所在层改造）

```go
type ReadTask struct {
    Stream *shmipc.Stream
    Key    string
    Pos    int64
    Want   int64
    Buf    []byte // 共享内存数据区（Reserve 已由本流 goroutine 完成，对齐可用）
    done   chan ReadTaskResult
}
```

- 流 goroutine `ParseGetReq` 后：`Reserve` 共享内存切片 → 写占位帧头 → 构造 ReadTask **投递到读请求队列** → **阻塞等 `done`**。
- `Reserve` 必须留在流 goroutine 侧（BufferWriter 是 per-stream 的，worker 池不可触碰）——**共享内存写序仍由流 goroutine 独占**，与现状一致。

### 3.2 worker 池批量处理（每轮取当前队列全部任务）

```go
func (w *readWorker) run() {
    for {
        batch := w.takeN(reqQueue, w.batchSize)      // drain：清空队列，最多取 N 个；空则阻塞
        metas, err := st.BatchGetMapping(ctx, keys(batch))    // ① 批量查 rocksdb
        jobs := toReadJobs(batch, metas)                      // ② 拼 device.ReadJob
        ns, err := st.BatchRead(ctx, jobs)                    // ③ 一次 io_submit
        for i := range batch { batch[i].done <- result(ns[i]) } // ④ 逐任务回投响应队列
    }
}
```

三步对应你的要求：
1. **一次性查 rocksdb**：新增 `metastore.BatchGetMapping`（见 §7.2），N 个 key 一次批量读。
2. **一次性提交内核队列**：`storage.BatchRead` → `device.ReadAtIntoBatch` → 一次 `io_submit`（已有）；若 `ReadTask` 全为对齐整块，则与 `shmBatchReader` 语义合并（该协调器可退役或保留为 L1 专用）。
3. **一次性写读响应队列**：同一批的结果**按流分组回投**（batch[i].done），流 goroutine 收齐后批量写帧（见 §3.3）。

### 3.3 响应批量写帧（各流 goroutine）

- 流 goroutine 同时抽查**响应队列**（或沿用 per-task `done`）取回结果。
- 取回的批结果按请求顺序补帧头（OpGetData/Final）、一次 `Flush`——**保序由本流串行写帧保证**（Reserve→Flush 同线程）。
- 与现有 [shmWriteDataFrameBatch:471](file:///d:/workspace/nefs/taihu/internal/transport/server_shm_linux.go#L471-L511) 的"占位帧头→批读→更新帧头→Flush"完全同构，只是把"磁盘读"从阻塞内联改为队列+worker。

### 3.4 回退路径

非对齐窗口/对象末尾（现 [server_shm_linux.go:437](file:///d:/workspace/nefs/taihu/internal/transport/server_shm_linux.go#L437) 的 `ReadAt` 慢路径）**不进批量队列**：直接走原回退（经 bufpool + `shmWriteFrame` 拷贝），避免把非对齐任务混入批量（批量项要求对齐直读）。rpc 路径同理：TCP `handleGet` 全部走非批量 `ReadAt`（该路径无共享内存直读，天然逐条）。

## 4. 写流水线

### 4.1 请求入队

```go
type WriteTask struct {
    Stream *shmipc.Stream
    Key    string
    Off    int64 // 段内偏移（PutBegin 已分配）
    Data   []byte
    done   chan WriteTaskResult
}
```

写分两段：
- **数据段**：`OpPutData` 帧 → 构造 WriteTask 投递写请求队列（数据来自共享内存直读，`shmReadFrame` 已 4K 对齐）；**不做逐帧 Append**。
- **提交段**：`OpPutEnd` 帧 → 投递"commit 任务"（同一 key 的数据段批完成后触发 PutCommit）。

### 4.2 worker 池批量处理（每轮取当前队列全部任务）

```go
func (w *writeWorker) run() {
    for {
        batch := w.takeN(writeQueue, w.batchSize)           // drain：清空队列，最多取 N 个；空则阻塞
        st.BatchPutBegin(batch)                             // ① 批量申请写位置（若入队时未分配）
        st.AppendBatch(batch)                               // ② 一次 io_submit 排空数据段
        st.BatchPutCommit(batch)                            // ③ 内核返回后批量处理 rocksdb（映射批）
        for i := range batch { batch[i].done <- ok }        // ④ 写返回答：w 楺队列
    }
}
```

对应你的要求：
1. **一次性提交内核队列**：新增 `device.AppendBatch` 依赖 aio `SubmitWriteBatch`（§7.1，来自 device 重构文档）。
2. **内核返回后一次性处理 rocksdb**：新增 `metastore.BatchPutMapping`（Pebble Batch 事务一次性提交 N 条映射 + 段计数更新一次性落盘）。
3. **一次性写写返回**：批结果按流分组回投 `done`，流 goroutine 收齐后批量写 OpResp 帧 + Flush。

### 4.3 一致性约束

- **先数据后元数据**不变：`AppendBatch` 全部完成后才 `BatchPutCommit`（[storage.go:120](file:///d:/workspace/nefs/taihu/internal/storage/storage.go#L120) 现有顺序保证保留）。
- **PutBegin 前置**：写位置分配（AllocateSegment）仍需串行（metastore 锁只覆盖游标分配），要在 worker 批内一次性完成（`BatchPutBegin`），避免并发 Put 争抢同一段（现状 `PutBegin` 边收数据边分配，改造后改为**先整对象批入队、再整批分配+写+commit**）。

## 5. 删除流水线

与读/写完全对称的第三条流水线（shm [handleShmDelete:665](file:///d:/workspace/nefs/taihu/internal/transport/server_shm_linux.go#L665) / rpc [handleDelete:296](file:///d:/workspace/nefs/taihu/internal/transport/server.go#L296) 改造）。

### 5.1 请求入队（各流 goroutine）

```go
type DeleteTask struct {
    Stream *shmipc.Stream // 或 rpc Conn
    Key    string
    done   chan error
}
```

流 goroutine `ParseKeyReq` 后投递删除请求队列并阻塞等 `done`。删除是**一元请求**，无数据帧、无 Reserve（不占共享内存数据区），入队成本最低。

### 5.2 worker 池批量处理（每轮取当前队列全部任务）

```go
func (w *deleteWorker) run() {
    for {
        batch := w.takeN(deleteQueue, w.batchSize)         // drain：清空队列，最多取 N 个；空则阻塞
        res, err := st.BatchDelete(ctx, keys(batch))       // 批量查 + 批量删（一次 Pebble Batch）
        for i := range batch { batch[i].done <- res[i] }   // 结果回投删除返回队列
    }
}
```

对应批量语义：
1. **批量查**：`BatchGetMapping` 确认各 key 存在（不存在回 `ierr.ErrNotFound`，[storage.go:334](file:///d:/workspace/nefs/taihu/internal/storage/storage.go#L334) 现状语义保留）。
2. **批量删元数据**：`BatchDeleteMapping` 用单个 `*pebble.Batch` 一次性删除 N 条映射，segment 的 AliveCount 减量一次性完成（`delObject` 现有逻辑扩展为批量版，[segments.go:220](file:///d:/workspace/nefs/taihu/internal/metastore/segments.go#L220)）。
3. **批量回写返回队列**：批结果按流分组回投 `done`，流 goroutine 收齐后批量写 OpResp 帧 + Flush（status 逐 key 独立：OK / ErrNotFound 可按 key 返回，不用整批失败即全败）。

### 5.3 语义要点

- **删读/删写并发安全**：删除与 GC 竞态由现有段引用计数与状态机保护（`DeleteMapping` 路径删除映射即触发 AliveCount 归零 → Reclaiming，语义不变）；删除不触碰磁盘 IO，只改元数据。
- **回退路径**：删除无对齐/缓冲约束，**全部任务都可进批量队列**（不像读有"非对齐回退"），实现最干净。
- **批量不合并错误**：每 key 独立记录结果（命中/未命中），不因单个 ErrNotFound 失败整批；`BatchDelete` 返回 per-key error 切片，与 `BatchedReadResult` 同构。

## 6. 队列与线程池组件（公共骨架）

```go
type TaskQueue[T any] struct {
    ch        chan T  // bounded channel，容量 = batchSize × 池大小
    batchSize int     // 每轮最多取出 N 个（不是攒批目标）
}

func (q *TaskQueue[T]) Enqueue(t T)                  // 满则阻塞（背压）
func (q *TaskQueue[T]) takeN() []T {                 // drain：清空当前队列，最多 N 个
    // 仅当队列空时阻塞等首个任务；一旦有任务立刻把当前可取的
    // 全部取走（至多 batchSize 个），不做攒批等待
}
```

- worker 池：`NewWorkerPool(workers int, q *TaskQueue[T], fn func([]T)) `，每 worker 一个 goroutine 循环 `takeN` → `fn(batch)`。
- worker 取批拼批大小 N 无等待超时；CPU 空闲时天然退化为 batch(1) 单条处理，避免尾延迟。
- **可配置项**：读 worker 数(2-4)、读 batchSize、写 worker 数、写 batchSize、删 worker 数、删 batchSize。

- 队列容量背压：`Enqueue` 满时阻塞流 goroutine，天然限流（对应共享内存池上限约束，[server_shm_linux.go:592](file:///d:/workspace/nefs/taihu/internal/transport/server_shm_linux.go#L592) 注释"池约束"由 Reserve 失败截断兜底，本设计不改该契约）。

## 7. 基础设施配套改动

### 7.1 aio + device（写批量；读批量已具备）

| 层 | 改动 |
|----|------|
| `internal/aio` | 新增 `SubmitWriteBatch(specs) (firstSeq, submitted, err)`（fd 绑定在 `Options.FD`），复用 `submitBatch`（传 `opcodePwrite`）；`aio_fallback_other.go` goroutine+pwrite 兜底 |
| `internal/device` | 新增 `AppendBatch(ctx, []AppendJob) error`（对齐直写 + 尾块补零逻辑逐项保留，批量排空）；待 device 重构落地后成为两个批量接口之一 |
| `internal/storage` | 新增 `BatchPutBegin(ctx, items)` / `BatchPutCommit(ctx, items)`（对齐 `PutBegin`/`PutCommit` 语义的批量版） |

### 7.2 metastore（批量映射）

| 新增接口 | 说明 |
|---------|------|
| `BatchGetMapping(ctx, keys []string) ([]ObjectMeta, error)` | N 个 key 一次读；实现：metaCache 命中走缓存，未命中走 pebble `Snapshot` + 单迭代 `SeekGE` 顺序取（key 排序去重），避免 N 次独立 Get 往返 |
| `BatchPutMapping(ctx, items []{key, meta}) error` | N 条映射用单个 `*pebble.Batch` 一次性提交 + segment 计数一次性更新（[segments.go:176](file:///d:/workspace/nefs/taihu/internal/metastore/segments.go#L176) `putObjectLocked` 已支持 batch 结构，扩展为多 key 版） |
| `BatchDeleteMapping(ctx, keys []string) ([]error, error)` | N 条映射用单个 `*pebble.Batch` 一次性删除 + segment AliveCount 一次性减量（[segments.go:220](file:///d:/workspace/nefs/taihu/internal/metastore/segments.go#L220) `delObject` 扩展为批量版），返回 per-key 结果 |

> 说明：pebble 无 MultiGet 原生语义，`BatchGetMapping` 的"一次性"指**单快照 + 单迭代**（Seek 定位 N 次顺序扫描），避免 N 次独立事务往返——这是能把 rocksdb 查询摊薄的实现方式。

## 8. 保序与并发正确性

1. **请求序（关键）**：同一流（同一 `shmipc.Stream`）的请求在客户端按帧序发送，处理顺序不得打乱。**drain 语义下多 worker 并发取批会破坏同流保序**（A 被 worker1 取、B 被 worker2 取，完成后乱序回投）。方案：**同流任务哈希路由到固定 worker**——`TaskQueue` 按 `streamID % workers` 分发（复用现有 [shmBatchReader worker 数组+轮转](file:///d:/workspace/nefs/taihu/internal/transport/server_shm_linux.go#L87-L112) 的设计，改为同流哈希而非 rr 轮转）。同流任务只进同一 worker 的 channel，该 worker 内串行 drain，天然保序；跨流间并行。删除任务同样保序（同一流连续 Del 按序处理）。
2. **共享内存写序**：`Reserve`/`Flush` 始终在流 goroutine 单线程执行（[server_shm_linux.go:364](file:///d:/workspace/nefs/taihu/internal/transport/server_shm_linux.go#L364) 注释"链序==块序"），worker 只产出结果不进写路径。
3. **读删互不干扰**：读 worker 池与删 worker 池独立；同一 key 并发读删（GC 在途）由 `db.RefSegment/UnrefSegment` + 段状态机保护（现有语义不变，删除只改映射不指磁盘）。
4. **关闭安全**：`Close()` 排空：停接收 → 等队列排空 → 等池退出 → 等流 goroutine 收尾（沿用 `shmServer.Close` [201](file:///d:/workspace/nefs/taihu/internal/transport/server_shm_linux.go#L201) 的 wg 编排）。

## 9. 收益预估

| 项 | 现状 | 改造后 |
|----|------|--------|
| 映射查询 | 每请求 1 次 pebble Get（同步 syscall） | 每批 N 个 key 1 快照 + 顺序迭代 |
| 磁盘读 | shm L1 批读（已聚合） | 全线批量（含未走批的 rpc） |
| 磁盘写 | 逐帧 Append 单条 | AppendBatch 一次 io_submit |
| 元数据写 | 逐对象 PutCommit 单条 | Pebble Batch 一次提交 |
| 元数据删 | 逐 key DeleteMapping 单条 | Pebble Batch 一次提交（逐 key 结果独立） |
| syscall 摊薄 | 读部分、写/删无 | 读/写/删均有 |

## 10. 测试计划

- metastore：`BatchGetMapping`（命中/未命中混合、key 序）、`BatchPutMapping`（N 条一次提交、失败回滚）、`BatchDeleteMapping`（混合命中/未命中返回 per-key 结果、AliveCount 归零转 Reclaiming）。
- device：`AppendBatch`（对齐直写/尾块补零/非对齐兜底/跨段批量）。
- transport：shm 读写删混合批、回退路径不混批（读）、同流保序、队列满背压、关闭排空。
- 性能：对比逐条 vs 批处理读写删带宽/CPU，确认 syscall 摊薄收益。

## 11. 与 device 重构文档的衔接

本设计依赖 `REFACTOR_DEVICE_BATCH.md` 的两个产出：`device.AppendBatch`（写批量基础设施）与 `storage.BatchRead` 对 `ReadAtIntoBatch` 的透传。顺序建议：先落地 device/aio 批量写 → 再 metastore 批量映射（Get/Put/Delete）→ 最后 transport 队列流水线。