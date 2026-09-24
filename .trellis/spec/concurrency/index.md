# concurrency —— 并发模式（gbp 类 5）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/concurrency-*.md`，7 条。
> 6 条适用 / 1 条不适用（errgroup）+ 1 条**当前有真实偏离**（race detection）。

## 本仓库的并发形态

这是**存储引擎**，并发是它的核心而不是附加物。当前的规模：

| 项 | 数量 | 核实命令 |
|---|---|---|
| 非测试代码的 goroutine 启动点 | **35** | `grep -rnE '^\s*go [a-zA-Z(]' --include='*.go' internal cmd pkg \| grep -v _test.go` |
| channel 总数（非测试 `make(chan ...)`） | **32** | `grep -rn 'make(chan ' --include='*.go' internal cmd pkg \| grep -v _test.go` |
| ├ 无缓冲（`make(chan struct{})`） | 16 | `grep -rn 'make(chan struct{})' --include='*.go' internal cmd pkg \| grep -v _test.go` |
| └ 有缓冲 | 16（其中**仅 1 处**缓冲 > 1，见 gbp-024） | 总数减去无缓冲 |
| `sync.Mutex` / `RWMutex` 字段 | 20 | `grep -rn 'sync\.Mutex\|sync\.RWMutex' --include='*.go' internal cmd pkg \| grep -v _test.go` |

**35 个启动点每一个都要能说清「谁等它、怎么停」**——这是本层最重的一条（gbp-022）。

**统计时的一个坑（我自己踩过）**：只 grep `go func(` 会得到 **15**，**漏掉 20 个**。因为本仓库大量使用**方法调用形式**的启动：`go s.handlePut(c, st)`、`go b.run(b.queues[i])`、`go conn.readLoop()`、`go m.loop()`、`go d.pump()`、`go c.run()`、`go s.acceptLoop()`、`go cluster.RunHeartbeat(...)`。**用上面表格里的 `^\s*go [a-zA-Z(]` 那个模式**，别用 `go func(`。

## 逐条裁决

### gbp-022 · Goroutine Lifecycle Management（CRITICAL）— **适用（本层最重要）**

**规则**：每个 goroutine 都必须有明确退出条件，避免泄漏。反例是 `go func(){ for { processTask() } }()` 永不退出；正例是 `Worker` 结构体带 `done chan struct{}` + `sync.WaitGroup`，`select` 上 `ctx.Done()` / `done` / 任务，`Stop()` 里 `close(done)` 再 `wg.Wait()`。

**对 taihu：适用。** 判据是**三个问题都要能答上来**：

1. **它的存活期由什么限死？**（一次同步调用？一个循环？）
2. **它怎么知道该停？**（`stopCh` 关闭？`ctx` 取消？自然结束？）
3. **谁阻塞等它结束？**（`WaitGroup`？`done` channel？公开 API？）

**答不上第 3 问的，要么补 join 点，要么在代码注释里写明为什么不需要**——「它很短」不是理由，除非你能指出限死它存活期的那行代码。

**正例**：`pkg/taihu-client/index.go:46-51` —— `start()` 起 `go m.loop()`，`loop()` 第一行就是 `defer close(m.done)`，`done` 是对外可等的 join 点；停止信号走 `stopCh`。

**第二类正例（不是 WaitGroup，但同样答得上来）**：`internal/transport/server.go:140-154` 有 8 处 `go s.handleXxx(c, st)`——**每个请求一个 goroutine**，且**没有 `wg.Add`**。它的第 3 问答案是**流生命周期**：存活期由 `st`（一个请求/响应流）限死，停止信号是 `st.done` / `c.closed`（`:127-135` 的 `select` 就在等这两个），收尾走 `internal/transport/server.go:161` 的 `endStream(st)` 注销流。

**这个例子说明第 3 问不必然等于「有个 `WaitGroup`」**——但必须有一个**能等的东西**。这里的代价是**进程退出时不保证在途请求全部收尾**，这是服务端关停路径要单独处理的事，不是随手能改的性质。

**注意**：`select` 里有 `ctx.Done()` 不等于满足本条——**没有 join 点的 goroutine，调用方无论如何都等不到它结束**，进程退出时它的收尾工作可能被截断。

### gbp-023 · Channel Usage Patterns（HIGH）— **适用**

**规则**：正确使用 channel 模式：`struct{}` 信号 channel、`select` + `time.After` 收发超时、`default` 非阻塞收发、fan-out / fan-in（收尾 `wg.Wait()` 后 `close(out)`）；发送方负责关闭。

**对 taihu：适用。** 本仓库的主流形态正是 `struct{}` 信号 channel（16 处无缓冲 `make(chan struct{})`）。

**要点**：`close` 永远由**发送方**执行；接收方关闭会 panic。fan-in 的收尾必须是「所有生产者 `wg.Wait()` 之后再 `close(out)`」——顺序颠倒会 panic 或截断。

### gbp-024 · Channel Size Selection（HIGH）— **适用，且本仓库有 1 处真实偏离**

**规则**：channel 缓冲大小应为 **0 或 1**；更大的缓冲必须**书面论证**。反例是 `make(chan Task, 1000)` 掩盖背压与内存问题。选型表：同步 = 0 / 通知 = 1 / 信号量 = N / 批量 = 批大小。

**对 taihu：适用。** 但有一处**当前不符合**：

| 位置 | 缓冲 | 状态 |
|---|---|---|
| `pkg/taihu-client/index.go:39` | `make(chan indexItem, **4096**)` | ⚠️ **无书面论证** |

`pkg/taihu-client/index.go:26-27` 的类型注释解释了「尽力而为、失败丢弃、读 miss 由回源兜底」的**语义**，`indexBatchMax = 128`（`:14`）与 `indexFlushInterval = 100ms`（`:11`）也都是命名常量——**唯独 4096 没有**：没有常量名、没有注释、没有推导过程。为什么不是 1024 或 65536？这个数字目前无法被复核。

**处置**：要么在 `pkg/taihu-client/index.go:39` 补一行论证（例如「批写入上限 × 实例数」这类可推导的依据），要么把它降到 1 并改用批量语义显式控制。**本 spec 不替你选**，但**不能不选**。

**其余 15 处非测试有缓冲 channel 缓冲都是 1，符合本条。** 它们集中在 `internal/transport`（`make(chan error, 1)` 三处、`make(chan shmBatchResult, 1)`）与 `internal/device`（`make(chan aio.Event, 1)` 两处）。**`1` 是「通知」语义**——正是规则选型表里的「通知 = 1」，不需要论证。

**测试文件里的缓冲不适用本条的论证要求**（`transport_batch_test.go` 的 `int, 8` / `int, 4` 是构造批次场景用的）。

### gbp-025 · Context Cancellation Propagation（CRITICAL）— **适用**

**规则**：用 `context` 传播取消信号与超时；`r.Context()` 派生 `WithTimeout` + `defer cancel()`；用 `errors.Is(err, context.DeadlineExceeded/Canceled)` 分流。

**对 taihu：适用。** `context` 已在 `internal/cluster`（`client.go` / `register.go` / `capacity.go` / `kv*.go`）与 `internal/transport`（`server.go` / `client.go` / `batch.go` / `client_admin.go`）广泛使用。

**要点**：`WithTimeout` / `WithCancel` 的返回值 `cancel` **必须 `defer cancel()`**，否则 `context` 泄漏（`govet` 的 `lostcancel` 能抓，见 [lint/](../lint/index.md) 的 gbp-051）。

**一条本仓库特有的提醒**：`internal/aio` 的 `Ring.Wait` **不接 `context`**，超时走自己的 `timeout *time.Duration` 参数（`internal/aio/aio.go` 的接口声明）。改那一层时不要「顺手统一成 context」——异步 IO 的提交与完成不走 context 模型。

### gbp-026 · errgroup Concurrency Control（HIGH）— **不适用**

**规则**：用 `errgroup.WithContext` 简化并发任务与错误传播；`g.Wait()` 任一失败即取消其他；`g.SetLimit(N)` 限流。

**对 taihu：不适用（未引入）。** `grep -rl 'errgroup' --include='*.go' internal cmd pkg examples` → 0，`golang.org/x/sync` 不是直接依赖。

本仓库的并发任务收敛走**自实现的机制**：`internal/transport` 的 batch workers、`internal/device` 的单一完成泵、`internal/benchkit` 的分片 worker（`internal/benchkit/run.go:94` 靠 `fin` channel 回收、`internal/benchkit/run.go:171` 靠 `wg.Done()`）。这些都有各自的错误聚合与取消路径。

**要不要引入 errgroup？** 本 spec 不裁决。但**如果引入**，必须说明它与现有收敛机制的关系——两套并存比用哪一套更糟。

### gbp-027 · sync Package Primitives Usage（HIGH）— **适用**

**规则**：正确使用 `Mutex` / `RWMutex` / `Once` / `Pool` / `Map`；`Lock` 后 `defer Unlock`。

**对 taihu：适用。** 20 处 `sync.Mutex` / `RWMutex` 字段。

**要点一（与 gbp-035 同源）**：`Lock()` 之后**立刻** `defer Unlock()`。手写 `Unlock` 只在一种情况下可接受：**中间有必须提前解锁的分支**，且每一条返回路径都覆盖到了。

```go
// 可接受：提前解锁是刻意的
mu.Lock()
if done { mu.Unlock(); return }
...
mu.Unlock()
```

**要点二（本仓库特有）**：锁字段必须有**保护范围注释**。20 处里已有写法可参照（如 `internal/aio/aio_libaio_linux.go:51` 的 `mu sync.Mutex // 保护 seq 与 iocb 复用`）。**没有注释的锁，下一个人不敢动它**——他无法判断改变临界区是否安全。

**要点三**：`sync.Pool` 在本仓库是**被明确否决过的**——`internal/bufpool/bufpool.go:10-14` 记录了实测依据（GC 清空导致 4M/8M 大缓冲每轮重分配）。若要在这里重新提议 `sync.Pool`，先读那段。

### gbp-028 · Race Detection（CRITICAL）— **不适用（当前未启用，是已知缺口）**

**规则**：用 `go test -race` 检测数据竞争，CI 必须开启；race detector 有 2-20x 开销，不要用于生产。

**对 taihu：当前未启用。** `Makefile` 里 `race` 零命中（`grep -c 'race' Makefile` → 0）。

**这是本层唯一的已知缺口**，且对存储引擎来说不是小事——20 把锁、**35** 个 goroutine 启动点、32 个 channel，正是 race detector 最有价值的场景。

**为什么现在没上**：`internal/aio` 的 Linux 路径带 `unsafe`（结构体 size/offset 断言、`uringRing` 的字段布局）与 io_uring 的 mmap 共享内存，race detector 在这些区域会产生大量误报或直接不适配。**这是一个需要单独论证的改动，不是「在 Makefile 里加个 `-race`」那么简单。**

**所以本条的状态是「已知缺口」，不是「已遵守」**——不要因为它写在 spec 里就当作做过了。
