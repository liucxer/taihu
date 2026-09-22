# 缓冲与并发（buffer-and-concurrency）

> `aio`/`bufpool`/`device`/`metastore` 的缓冲所有权协议、单一完成泵模型、Ring 并发契约与锁纪律 —— 违反任一条都产生静默数据损坏或池泄漏，且都不会立刻报错。

---

## 一、缓冲所有权协议

### 1.1 Submit 之后、Wait 取回之前，buf 必须存活且不被改写

这是 `aio` 包文档里的「使用约束（调用方）」第一条 —— `internal/aio/aio.go:19-21`：

```go
// 使用约束（调用方）：
//   - buf 在 Submit 后、对应完成事件被 Wait 取回前必须保持存活且不被改写；
//   - Linux + O_DIRECT 时 buf 首地址、偏移、长度需 4K 对齐（由 bufpool/device 层保证）。
```

同一约束在 `WriteSpec` 上被重申 —— `internal/aio/aio.go:52`；批量接口对批内每个 spec 的 buf 同样适用 —— `internal/aio/aio.go:76`、`internal/aio/aio.go:85`（「各 buf 须存活到完成事件被取回」）。

`device` 层把这条约束落到实现上：`submitOp` 上方注明「buf 必须存活到完成事件取回（O_DIRECT 下内核直读调用方缓冲）」—— `internal/device/device.go:181`。

`AppendBatch` 把临时缓冲的存活期显式拉到「全部完成事件取回之后」—— `internal/device/device.go:445-450`：

```go
	var tmps [][]byte // 补零/对齐临时缓冲，须存活到全部事件取回
	defer func() {
		for _, t := range tmps {
			bufpool.Put(t)
		}
	}()
```

### 1.2 读路径返回值必须由调用方归还池

`Storage.ReadAt` 的注释就是这条契约 —— `internal/storage/storage.go:221-222`：

```go
// ReadAt 读取对象内 [off, off+size) 区间的数据并返回（返回值为从 bufpool 取出的池化
// 缓冲或 nil；调用方用毕必须 bufpool.Put(返回值) 归还，否则造成池泄漏）。
```

- `Device.ReadAt` 同契约：返回切片持有一块 bufpool 缓冲，「调用方不再使用后必须将返回值原样交还 bufpool.Put（归还池）」—— `internal/device/device.go:571-572`。
- `Device.Append` 内部申请的对齐缓冲用 `defer` 归还，且注释解释为何 defer 是安全的（submit 同步阻塞到完成）—— `internal/device/device.go:396-397`：

```go
	buf := bufpool.Get(int(aligned))
	defer bufpool.Put(buf) // submit 均阻塞至完成，返回后缓冲即可复用
```

- Compactor 读出的对象缓冲同样 `defer bufpool.Put(buf)`；注意它在 `buf == nil` 时提前返回，不会误 Put 一个 nil —— `internal/storage/compact.go:186-189`。

### 1.3 远程 Get 的 `(data, release, err)` 三元组

接口层把「必须调用 release」写进了方法语义 —— `internal/rpcclient/objectstore.go:22-26`：

```go
// 方法语义（两个实现一致）：
//   - Put：写入对象，size 为逻辑大小，in 提供数据（取 in[:size]），调用方负责数据就绪；
//   - Get：读 [off, off+size)，size=-1 读至结尾；返回 (data, release, err)，
//     用毕必须调用 release()（幂等）归还池化缓冲；
```

**幂等**不是嘴上说说，实现里用 `disposed` 标志兜住重复调用 —— `internal/transport/client.go:148-156`：

```go
	dispose := func() {
		if disposed {
			return
		}
		disposed = true
		if buf != nil {
			bufpool.Put(buf)
		}
	}
```

- 成功路径把同一个闭包交出去：`return out, dispose, nil` —— `internal/transport/client.go:208`、`internal/transport/client.go:216`。
- `size == 0` 时直接返回空闭包 `func() {}`（无缓冲可还）—— `internal/transport/client.go:133`。
- shm 数据面同契约 —— `internal/transport/client_shm_linux.go:125-126`（「返回 (data, release, err)：… 调用方用毕必须调用 release()（幂等）归还（流随之 PutBack 复用）」）。
- `rpcclient.Storage` 只是把 `rpcConn.Get` 透传，不额外包装 —— `internal/rpcclient/storage_rpc.go:52-59`。

## 二、`bufpool` 为何明确否决 `sync.Pool`

包注释给了实测数字与替代方案 —— `internal/bufpool/bufpool.go:10-14`：

```go
// 实现说明：桶内采用自管理 freelist（互斥锁 + LIFO 栈），而非 sync.Pool。
// sync.Pool 会在每次 GC 时清空其中的对象，导致 4M/8M 大缓冲被整批丢弃、
// 每轮重走对齐分配（mallocgcLarge）与清零（memclr），实测读路径该冷分配
// 约占服务端 CPU 36%。自管理 freelist 不受 GC 影响：大缓冲常驻长期复用，
// 首次分配完成清零后，后续 Get 零分配、零清零；每桶以 maxKeep 上限约束驻留内存。
```

围绕这条决策的其余事实：

- 每桶驻留上限 `maxKeep = 32`（如 4M 桶 32×4M = 128MB）；超出上限的归还缓冲**直接丢弃**交由 GC 回收 —— `internal/bufpool/bufpool.go:28-30`。
- 分桶按 2 的幂，下限 4KB（O_DIRECT 对齐粒度）、上限 8GB（`maxBufBucket = logBlockSize + 21`，覆盖 `SegmentSizeBytes`）—— `internal/bufpool/bufpool.go:24-27`。
- 分桶函数有下溢陷阱，注释记录了曾经的真实 panic —— `internal/bufpool/bufpool.go:74-77`：

```go
// bufBucket 返回 n 向上取 2 的幂（下限 4KB、上限 8GB）对应的桶索引。
// 前置条件 n > 0：n<=0 时 uint64(n-1) 会下溢成 2^64-1，bits.Len64 得 64，
// 算出的索引超出 pools 数组（曾表现为 Get(0) 索引越界 panic）。两个调用方
// （Get 经 get、Put 经 put）均已在前置处挡住 n<=0。
```

- 对齐分配必须用**三索引切片**固定 `cap == n`，否则 Put 按 cap 归一化后落进高一档桶，`Get(n)` 永远取不到本桶缓冲 —— `internal/bufpool/bufpool.go:138-140`（注释明说这是「实测复用率 0 的根因」）。
- 全部桶在包初始化时一次性建好，避免运行期并发懒初始化触发 `-race`（netpoll 对齐分配器会让多个 goroutine 同时首次触达同一新桶）—— `internal/bufpool/bufpool.go:46-48`。
- 精确尺寸池（`GetExact`/`PutExact`）按 `len==cap` 分桶、**不按 2 幂取整**，服务对象是 netpoll 收流节点与客户端 `Get` 零拷贝移交缓冲 —— `internal/bufpool/bufpool.go:153-158`。

## 三、单一完成泵模型

`device` 包文档 —— `internal/device/device.go:8-10`：

```go
// 磁盘 IO 经 internal/aio 异步提交（Linux: libaio；其他平台: goroutine 兜底），
// 由单一完成泵 goroutine 串行取回完成事件并分发到各提交方；对外 Append/ReadAt
// 仍保持同步语义（提交后阻塞至本请求完成）。不同偏移的并发读写安全。
```

泵自身的契约 —— `internal/device/device.go:145-148`：

```go
// pump 完成泵：唯一调用 ring.Wait 的 goroutine，串行取回完成事件并按 seq 分发到提交方。
// 事件先到而提交方尚未注册通道时暂存 pending，由提交方注册时消费（不丢失、不重复）。
// 退出条件 closed 且无注册在途（m 空）且无未落定的提交（inSubmit==0），保证退出时
// 无任何 in-flight IO，ring/fd 可安全销毁。
```

**唯一性可以在代码里验证**：整个 `internal/` 只有一处 `ring.Wait` 调用点 —— `internal/device/device.go:152`，由 `go d.pump()` 单点启动 —— `internal/device/device.go:110`。

其余实现事实：

- 空闲轮询周期 200ms（`pumpTimeout`）：无事件时泵每 200ms 醒来一次用于 `Close` 及时退出；有事件时 `io_getevents` 立即返回（min=1），**不增加请求延迟** —— `internal/device/device.go:34-36`。
- 与泵的同步点：提交前锁内查 `closed`（未提交就快速失败）并自增 `inSubmit`，使泵不会在「已 io_submit、事件尚未落定」的窗口内退出 —— `internal/device/device.go:183-186`。
- 提交方锁内消费 `pending` 或注册 `m[seq] = ch`，两条路径都不会永久阻塞 —— `internal/device/device.go:232-241`。
- `batchWait` 镜像同一套 pending/m 语义，但多一个 `<-d.pumpDone` 出口（泵已退出时返回 `errDeviceClosed`）—— `internal/device/device.go:761-788`。
- `Device.mu` 保护的字段明确写在声明上：`m/pending/inSubmit/closed` —— `internal/device/device.go:57`。
- 完成侧瞬时错误（EAGAIN/EINTR）按原参数重提，上限 `complRetryMax = 8` 次，退避从 `submitRetry` 起翻倍且不超过 `complRetryCap = 2ms` —— `internal/device/device.go:39-42`、`internal/device/device.go:276-286`。

### io_uring 后端侧

`Submit*` 与 `Wait` 的并发关系写在类型注释上 —— `internal/aio/aio_uring_linux.go:151-152`：

```go
// 并发约定：Submit* 可由多个 goroutine 并发调用（由 mu 串行化，单生产者填 SQE）；
// Wait 由单一完成泵 goroutine 持有；两者通过原子读写的 SQ/CQ head/tail 交互。
```

- 「单生产者填 SQE」靠 `mu` 保护 `seq / sqeTail / inflight / closed` —— `internal/aio/aio_uring_linux.go:158`。
- SQ 侧：`sq_head` 由内核推进**必须原子读**；`tail-head` 在 uint32 上回绕，但差值恒 ≤ `sq_entries`，故无需取模 —— `internal/aio/aio_uring_linux.go:428-432`。
- SQ tail 的原子写是 release 语义：「SQE 字节先于 tail 对内核可见」—— `internal/aio/aio_uring_linux.go:525`。
- CQ 侧：`cq.tail` 由内核写，原子读即 acquire；`cqHead` 用 release 语义把 CQ 空间还给内核 —— `internal/aio/aio_uring_linux.go:576-584`。
- **未消费的 SQE 必须撤销发布**（`rewind`），否则会被重复提交 —— `internal/aio/aio_uring_linux.go:554-555`。
- `EINTR` 等提交失败绝不能盲目重试：SQE 可能已被内核取走，必须以内核推进的 `sq_head` 为准 —— `internal/aio/aio_uring_linux.go:540-543`。
- 关闭顺序有硬性要求：io_uring 在 `Close` 里**必须先置 `closed` 再解映射**，否则提交会访问已失效地址导致 SIGSEGV（libaio 在此只是返回 EBADF）—— `internal/aio/aio_uring_linux.go:646-648`。

## 四、`Ring` 的并发契约

接口上的正式契约就一句 —— `internal/aio/aio.go:66-67`：

```go
// Ring 异步 IO 完成队列。同一 Ring 可被多个 goroutine 并发 Submit，
// Wait 应串行调用（或由单一完成泵 goroutine 持有）。
```

配套的几条语义：

- `ErrFull` 表示提交队列已满（`io_submit` 返回 EAGAIN），**应先 Wait 取回完成事件后重试** —— `internal/aio/aio.go:38-39`。
- `Wait` 取回完成事件：阻塞至至少 `min` 个完成或 `timeout` 到期；事件按完成顺序返回，但 **Linux 实现顺序不保证与提交顺序一致，靠 `Event.Data` 关联** —— `internal/aio/aio.go:88-91`。
- 批量提交可能被内核截断（`submitted` 可能 < `len(specs)`），**调用方须把未排队部分追加提交**；队列满且一条未排入时返回 `ErrFull` —— `internal/aio/aio.go:73-77`、`internal/aio/aio.go:82-86`。
- `device` 层对 `ErrFull` 的处理：让出 `submitRetry = 100µs` 后整块重试，**不推进 seq（不丢 IO）**，并先把预增的 `inSubmit` 撤回 —— `internal/device/device.go:37-38`、`internal/device/device.go:521-527`：

```go
		if err == aio.ErrFull || n == 0 {
			// 队列满且一条未排入：让出后重试整块（未推进 seq，不丢 IO）。
			d.mu.Lock()
			d.inSubmit -= len(chunk)
			d.mu.Unlock()
			time.Sleep(submitRetry)
			continue
		}
```

## 五、锁纪律

`metastore` 用 `*Locked` 后缀标记「调用方须持有锁」的方法。全部出现位置如下（`grep -rn "调用方须持有" internal/` 的完整结果）：

| 位置 | 方法 | 注释原文 |
|---|---|---|
| `internal/metastore/segments.go:159` | `ensureLocked` | 取段条目，不存在则创建（默认 Active）。调用方须持有 m.mu。 |
| `internal/metastore/segments.go:169` | `persistLocked` | 持久化段状态。调用方须持有 m.mu。 |
| `internal/metastore/segments.go:175` | `putObjectLocked` | …调用方须持有 m.mu；batch 由调用方原子提交。 |
| `internal/metastore/segments.go:219` | `delObjectLocked` | …调用方须持有 m.mu；batch 由调用方原子提交。计数归零的段立即转 Reclaiming。 |
| `internal/metastore/kv_pebble.go:370` | `ensureLoaded` | 懒加载 pebble 句柄与持久化游标（首次分配时）。调用方须持有 a.mu。 |
| `internal/metastore/kv_pebble.go:385` | `allocOneLocked` | …调用方须持有 a.mu；批量分配后统一持久化游标一次。 |

其它锁约定：

- **锁序固定**：`allocator.mu` → `segmentManager.mu`（GC / PutMapping 路径只持 `segmentManager.mu`，无反向）—— `internal/metastore/segments.go:26`。
- 需要跨两个锁时**取快照后释放，不做嵌套持有** —— `internal/metastore/kv_pebble.go:555-556`：

```go
// UsedBytes 统计已用物理字节：Full/Reclaiming 段计整段大小，Active 段计已写偏移（curOff）。
// 锁序 allocator.mu → segmentManager.mu；此处取快照后释放 a.mu 再取 m.mu，无嵌套持有。
```

- **持锁只覆盖廉价部分**：allocator 的锁只覆盖游标分配，真正的设备写由调用方在锁外执行 —— `internal/metastore/kv_pebble.go:336-337`、`internal/storage/storage.go:17-18`。

## 六、goroutine 生命周期纪律

上游 `uber-048` / `049`：不要 fire-and-forget —— 每个 goroutine 要么有**可预测的结束时间**，要么有**通知它停止的信号**；两种情况都还要有**办法阻塞等待它结束**。

实测非测试代码共 **35 处 `go` 语句**。按生命周期形态归类：

### 6.1 后台循环：`stopCh` + `done` 成对，这是本仓库的招牌形态

有归属对象的后台循环一律「一个 `chan struct{}` 发停止信号 + 一个 chan 等待退出」：

| 位置 | 停止信号 | 等待 | 归属对象的对外方法 |
|------|----------|------|--------------------|
| `pkg/taihu-client/index.go:47` `go m.loop()` | `stopCh`（`:31` 声明、`:40` 构造、`:106` `close`） | `m.done`（`:51` `defer close(m.done)`） | `Close()`（`pkg/taihu-client/storage.go:339`） |
| `internal/benchkit/report.go:29` | `p.stopCh`（`:27`） | `p.doneCh`（`:28`、`:30` `defer close`） | `progress.start()` 的配对停止方法 |
| `internal/storage/compact.go:54` `go c.run()` | `c.stop`（`:42` `chan struct{}`、`:59` `close`） | `c.wg`（`:43` `sync.WaitGroup`） | `Compactor.Stop()`（`:58`） |
| `internal/device/device.go:110` `go d.pump()` | `d.closed`（`:61`，**锁保护的 bool 而非 channel**） | `d.pumpDone`（`:62`，`Close` 里 `<-d.pumpDone` 于 `:122`） | `Device.Close()`（`:115`） |

这正是上游 `uber-052` 的处方：只有一个 goroutine 要等时用 `chan struct{}`，结束时 `close(done)`，调用方 `<-done`。**多个 goroutine 要等时用 `sync.WaitGroup`**（上游 `uber-051`/`055`）—— 实测 7 处：`internal/metastore/segments.go:34`、`internal/storage/compact.go:43`、`internal/transport/server_shm_linux.go:46,660`、`internal/benchkit/run.go:168`、`pkg/taihu-client/storage.go:222`、`cmd/taihu/cmd/server.go:270`。

**规则**：新增后台 goroutine 时照上表选形态 —— 单 goroutine 用 `stopCh`/`done` 对，多 goroutine 用 `WaitGroup`。**归属对象必须提供 `Close`/`Stop`/`Shutdown` 方法**（上游 `uber-054` 的 must 条款），实测 16 个类型已有该方法（`internal/transport/server.go:91` 的 `Server.Stop` 是其中一例）。

### 6.2 每请求 handler goroutine：有停止信号，但没有 join 点

`internal/transport/server.go:137-155` 在 `dispatch` 里按 op 起 handler goroutine：

```go
switch op {
case protocol.OpPutHeader:
    go s.handlePut(c, st)
...
```

它们的停止信号是齐的 —— `internal/transport/server.go:127-135` 的投递口对每个流都盯着 `st.done` 与 `c.closed`：

```go
select {
case st.in <- frameMsg{op, sub}:
case <-st.done:
    _ = sub.Release()
    return deliverFatal // 处理器已结束仍收到该流帧：协议错误
case <-c.closed:
    _ = sub.Release()
    return deliverFatal
}
```

**但缺 join 点**：`Server` 的字段只有 `storage` / `pipeline` / `mu` / `els`（`internal/transport/server.go:40-47`），**没有 `WaitGroup`**；`GracefulStop`（`:76-88`）的注释写「等待在途连接处理完毕」，实际等的只是 netpoll 的 EventLoop（`el.Shutdown(ctx)`），而 `dispatch` 起的 handler goroutine 由 taihu 自己持有、netpoll 不感知。

**记为已知例外**，理由可验证：handler 的存活上界由流的生命周期决定（`st.done` / `c.closed` 一关就退出），且停机时 `el.Shutdown` 会先关连接，handler 随之收到信号 —— 实际影响是"停机返回后可能仍有 handler 在跑最后一帧"，不是泄漏。**新增同类 goroutine 时**：若能挂到已有的归属对象上就别再起裸 goroutine；确实要起，把这个对象也补上 `WaitGroup` + `Close`（照 6.1 的表）。

### 6.3 批处理 worker：既无停止信号，也无 join 点（已知例外）

`internal/transport/batch.go:90-93` 起 worker：

```go
for i := range b.queues {
    b.queues[i] = make(chan *writeTask, batchCap*2)
    go b.run(b.queues[i])
}
```

`batchWriter.run`（`:105-`）是**无退出条件的 `for` 循环** —— `b` 上既没有 `stop`/`closed` 字段，也没有 `Close()` 方法；`pipeline` 被 `Server` 持有（`internal/transport/server.go:43`）且 `GracefulStop` 不碰它。`batchDeleter`（`:175` 的 `go b.run(...)`）同形。

**记为已知例外**，两点可验证的边界让它可以接受：

1. **数量有界**：worker 数由配置决定（`PipelineConfig.WriteWorkers` / `DeleteWorkers`，`internal/transport/batch.go:29-37`），不是每请求增长 —— 不构成 `uber-048` 担心的"不受控地大量启动"
2. **每个 Server 只建一次**：`newPipeline` 在 `NewServerWithOptions`（`internal/transport/server.go:57`）里调用一次

**代价是明确的**：进程退出时这些 worker 没有 join 点，`submit` 里 `return <-t.done`（`internal/transport/batch.go:101`）的在途任务若遇到停机就没有确定性结果。**新增此类 worker 时照 6.1 的表补 `stop` + `WaitGroup` + `Close`**，不要沿用这里的形态。

### 6.4 进程入口与基准工具：不适用上述约束

`cmd/taihu/cmd/server.go` 的 5 处（`:196`、`:233`、`:240`、`:255`、`:273`）、`cmd/taihu/cmd/bench_storage.go:139,299`、`cmd/taihu/cmd/helpers.go:42`、以及 `internal/benchkit/` 的 3 处（`run.go:94`、`run.go:171`、`report.go:29`）是**进程级生命周期** —— 它们要么随进程退出而结束，要么由命令的 `ctx` 取消。`cmd/taihu/cmd/server.go:270` 的 `serveWG` 是为 listener 收敛用的（上游 `uber-055`）。**库代码不要模仿这一类的写法**，库代码照 6.1。

### 6.5 无 `init()` 里起 goroutine

上游 `uber-054` 的标题条款。实测自有代码无命中：`grep -rnA4 'func init()' --include='*.go' internal pkg cmd | grep 'go func'` 无输出。唯一的 `init()` 是 `cmd/taihu/cmd/root.go:20`，只调了一次 `log.SetLevel`（见 [../architecture/code-style.md](../architecture/code-style.md) 的日志三通道）。**保持这样**：`init()` 里起 goroutine 会让启动顺序不可控，也无法等待其退出。

---

## 七、性能指引只适用于热路径

上游 `uber-056` 原文："Performance-specific guidelines apply only to the hot path."

本文件前五节的严格约束（缓冲所有权、单一完成泵、锁纪律、对齐要求）**只对热路径成立** —— 即每请求调用的 `Append`/`ReadAt`/`Get`/`Put`/`Delete`，以及完成泵主循环。以下路径**不必**套用：

- 启动 / 停机 / 配置解析（`cmd/taihu/cmd/server.go` 的 `init()` 与 flag 处理）
- 维护性后台任务（`internal/storage/compact.go` 的 compaction、`internal/metastore` 的段状态迁移）
- 基准与诊断工具（`internal/benchkit/`）
- 测试代码

**判据**：这段代码是否在**每个请求**上执行。是，才值得为它引入池化、批处理、对齐或原子操作；否，写清楚直白的那版。

反过来也成立 —— 热路径上的决策必须有**实测**支撑，且理由要留在代码里：`internal/bufpool/bufpool.go:10-14` 记录了否决 `sync.Pool` 的实测数字（读路径冷分配约占服务端 CPU **36%**），`internal/transport/client.go:117-120` 记录了零拷贝移交路径因**静默数据错配**被停用。**没有实测数字的"优化"不要进热路径。**

---

## 八、易踩点（都是实测踩过的）

- **TCP 路径的零拷贝移交曾被停用**：`netpoll` 的 `TakeTry`（整响应恰一帧时直接移交收流节点缓冲）实测存在静默数据错配 —— 移交后整块缓冲被归还池并复用，内容被后续收流覆盖；仅收紧「帧独占整块」（`base==0`）仍会复现，故 TCP 路径已停用，netpoll 侧保留护栏与单测；**shm 数据面的单帧移交不受影响** —— `internal/transport/client.go:117-120`。
- **完成侧的 EAGAIN 不是永久错误**：nvme 上 4MiB O_DIRECT 写经 io_uring 偶发以 `-EAGAIN` 完成，同负载 libaio 不复现；若不重试就会把一次本可成功的 Put/Read 无谓失败 —— `internal/device/device.go:259-264`（「已实测」）。
- **`ErrConflict` 不是客户端错误**：它是 compaction 内部的 CAS 控制信号，re-export 里被刻意排除 —— `internal/rpcclient/reexport.go:30`。相关搬移语义见 [metadata-and-compaction.md](./metadata-and-compaction.md)。

---

## 刻意偏离上游规则

本节登记「**明确知道上游怎么说、但 taihu 有意不照做**」的条目。三要素缺一不可：上游主张 / taihu 的做法（带锚点）/ 为什么偏离（具体到可检验）。

**不在这节里的「不遵守」不是偏离，是遗漏** —— 写不出可检验理由的，按缺陷处理。

### 1. 缓冲就地复用，默认可变

- **默认不可变，避免就地修改。** —— 上游见 `ecc-020`（ECC 把它标为 CRITICAL）。
  **taihu 的做法**是反复复用同一块缓冲：`Get` 回来的缓冲在消费方之间移交、由持有方按 `bufpool.Put` 归还，上层可以对同一块内存先读后写 —— 归还契约见 `internal/bufpool/bufpool.go:5-8`，存活期约束见 `internal/aio/aio.go:20-21`。
  **为什么偏离**：这是存储引擎的性能前提，不是风格选择。`internal/bufpool/bufpool.go:10-14` 记着**实测依据** —— 改用 `sync.Pool` 后 GC 会清空其中对象，4M/8M 大缓冲被整批丢弃、每轮重走对齐分配与清零，**该冷分配实测约占服务端 CPU 36%**。改自管理 freelist 后大缓冲常驻复用，首次分配清零后 `Get` 零分配、零清零（每桶以 `maxKeep` 约束驻留内存）。展开见本文 §二。

### 2. 零拷贝移交的边界：TCP 停用、shm 保留

- **共享可变状态会引发静默错误。** —— 上游见 `ecc-020`。
  **这一条不是偏离，是反向印证** —— taihu 真的踩过那个坑。`netpoll` 的 `TakeTry` 零拷贝移交（整响应恰一帧时直接移交收流节点缓冲）实测存在**静默数据错配**：移交后整块缓冲被归还池并复用，内容被后续收流覆盖；仅收紧「帧独占整块」（`base==0`）仍会复现，**故 TCP 路径已停用**（`internal/transport/client.go:117-120`），退回「恰一次用户态拷贝」，netpoll 侧保留护栏与单测。shm 数据面的单帧移交（帧独占、无跨流复用）**保留**零拷贝。
  **记在这里的原因**：它界定了第 1 条偏离的**边界** —— 复用只留在能证明安全的地方。新增任何复用或移交路径时，先回答「这块缓冲在移交之后还会不会被写」。
