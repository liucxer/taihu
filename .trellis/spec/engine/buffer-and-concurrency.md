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

同一约束在 `WriteSpec` 上被重申 —— `internal/aio/aio.go:55-56`；批量接口对批内每个 spec 的 buf 同样适用 —— `internal/aio/aio.go:80`、`internal/aio/aio.go:89`（「各 buf 须存活到完成事件被取回」）。

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

接口上的正式契约就一句 —— `internal/aio/aio.go:70-71`：

```go
// Ring 异步 IO 完成队列。同一 Ring 可被多个 goroutine 并发 Submit，
// Wait 应串行调用（或由单一完成泵 goroutine 持有）。
```

配套的几条语义：

- `ErrFull` 表示提交队列已满（`io_submit` 返回 EAGAIN），**应先 Wait 取回完成事件后重试** —— `internal/aio/aio.go:38-39`。
- `Wait` 取回完成事件：阻塞至至少 `min` 个完成或 `timeout` 到期；事件按完成顺序返回，但 **Linux 实现顺序不保证与提交顺序一致，靠 `Event.Data` 关联** —— `internal/aio/aio.go:92-95`。
- 批量提交可能被内核截断（`submitted` 可能 < `len(specs)`），**调用方须把未排队部分追加提交**；队列满且一条未排入时返回 `ErrFull` —— `internal/aio/aio.go:78-81`、`internal/aio/aio.go:87-90`。
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

## 六、易踩点（都是实测踩过的）

- **TCP 路径的零拷贝移交曾被停用**：`netpoll` 的 `TakeTry`（整响应恰一帧时直接移交收流节点缓冲）实测存在静默数据错配 —— 移交后整块缓冲被归还池并复用，内容被后续收流覆盖；仅收紧「帧独占整块」（`base==0`）仍会复现，故 TCP 路径已停用，netpoll 侧保留护栏与单测；**shm 数据面的单帧移交不受影响** —— `internal/transport/client.go:117-120`。
- **完成侧的 EAGAIN 不是永久错误**：nvme 上 4MiB O_DIRECT 写经 io_uring 偶发以 `-EAGAIN` 完成，同负载 libaio 不复现；若不重试就会把一次本可成功的 Put/Read 无谓失败 —— `internal/device/device.go:259-264`（「已实测」）。
- **`ErrConflict` 不是客户端错误**：它是 compaction 内部的 CAS 控制信号，re-export 里被刻意排除 —— `internal/rpcclient/reexport.go:30`。相关搬移语义见 [metadata-and-compaction.md](./metadata-and-compaction.md)。
