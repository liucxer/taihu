# 技术设计：修正 `internal/aio` 审计发现的规范偏差

> 本文件只写**技术设计**（边界、契约、取舍、顺序、验证、回滚）。需求与验收标准在 `prd.md`，执行清单在 `implement.md`。

## 1. 本任务的形状

13 条需求分三类，**它们对「验证」的要求完全不同**，这是本设计的核心：

| 类 | 需求 | 验证方式 | 风险 |
|---|---|---|---|
| **A 纯 spec 文本** | R4 R5 R6 R8 R9 R10 R11 | `verify_spec_refs.py` + 人工复核引用的 `file:line` | 低（写错就是写错，机器可查） |
| **B 补测试 / 行为不变重构** | R1 R3 R12 | `make check` + `make check-linux` + 覆盖率前后对比 | 中（重构落在热路径） |
| **C 行为变更** | **R2（唯一一条）** | 上述全部 + 逐调用方影响面复核 | 中 |

**执行顺序必须按 A → C → B 的一条特定路径走**，理由见 §4。

## 2. 决策记录

### D1 · R7 走「归类」而非「补 join 点」

**问题**：`aio_other.go:98` 每次 `Submit` 起一个 goroutine，`Close()`（`:190-196`）不等待。§六 的 6.1–6.5 里没有它的位置，而 §六 末尾写着「不在这节里的『不遵守』不是偏离，是遗漏 —— 按缺陷处理」。

**两个选项**：
- (a) 在 §6 追加一类并给出可检验理由；
- (b) 给 `ring` 加 `sync.WaitGroup`，`Close` 里 `wg.Wait()`，使其落回 6.1。

**选 (a)。** 判据是 §六 抬头引的 `uber-048`/`049` 原文：「每个 goroutine 要么有**可预测的结束时间**，要么有**通知它停止的信号**；两种情况都还要有**办法阻塞等待它结束**」。逐条对 `aio_other.go:98`：

| 要求 | 满足？ | 依据 |
|---|---|---|
| 可预测的结束时间 | ✅ | 存活期被**一次同步 `unix.Pread`/`Pwrite`** 限死（`aio_other.go:107-113`），无循环、无阻塞等待 |
| 有办法阻塞等待它结束 | ✅ | `ring.Wait`（`:140-187`）的 `collect()` 在 `:155` 阻塞于 `<-o.done`，而 `o.done` 正是该 goroutine 在 `:117` `close` 的 —— **join 点就是公开 API `Wait()`** |

即：**它已经满足规则的字面要求**，只是没被归类。这与 §6.2（handler 有停止信号但**无** join 点）和 §6.3（worker 既无停止信号也**无** join 点）是不同性质 —— 那两处是「明知不满足、登记为已知例外」，这里是「满足但漏登记」。

**为什么不选 (b)**：给 `Close()` 加 `wg.Wait()` 会让它有**阻塞语义**，而 `Ring.Close` 的契约（`aio.go:93-94`）只写「不应再 Submit」，没有「会等待在途完成」的承诺 —— 加 wait 是**改变公开接口语义**，代价远大于收益。而且 `ring.Close` 并不关 fd（fd 由调用方持有），所以「Close 不等」不引入 fd 复用竞态。

**副作用（须在 spec 里写清）**：新类必须写明「join 点是 `Wait()`，不是 `Close()`」，否则后来者会照着 6.1 的表去找 `Close` 里的 join 点，找不到就会误判为缺陷。

**D-11 仍然不裁定**：`ring.Close` 不持锁写 `r.ctx`、而 `uringRing.Close` 持锁 —— 这是 PRD「不在本任务范围」里列出的存疑项，本决策不触碰它。

### D2 · R11 走「收窄措辞」而非「全仓统一」

**问题**：`unit-tests.md:176` 写「资源用 `t.Cleanup` 释放而不是 `defer`」，但仓库主流相反（`defer X.Close()` 90 处 vs `t.Cleanup(` 57 处），`internal/aio` 自身也是两种并存。

**选收窄。** 关键证据来自它**自己引的**范例 `transport_shm_test.go:71-79`：

```go
// shmDial 拨号一条 shm 连接并在测试结束关闭。
func shmDial(t *testing.T, uds string) *ShmConn {
	t.Helper()
	c, err := DialShm(uds, 1)
	...
	t.Cleanup(func() { _ = c.Close() })   // ← 自己登记，调用方不必写 defer
	return c
}
```

`shmDial` 的特征是**把资源借出并返回给调用方**，所以它必须自己登记清理 —— 否则调用方漏写就泄漏。而 `internal/aio` 的 helper 分成两类，**只有一类属于这个形态**：

| helper | 形态 | 现状 | 收窄后 |
|---|---|---|---|
| `newTestFile`（`aio_test.go:22-33`） | **借出** `*os.File` | `:31` 已用 `t.Cleanup` ✅ | 合规（正是规则要的形态） |
| `assertRoundTrip`（`:52-54`） | 不动借出；关的是**调用方传入**的 `Ring` | `:54` `defer r.Close()` | **不在规则范围内** |
| `assertMultipleInflight` / `assertReadBeyondEOF` / `assertWaitTimeout` / `assertWaitTimeoutExpires` / `assertFdSurvivesGC` / `assertRoundTripODirect` | 同上 | `defer` | 同上 |

也就是说：**收窄后 `internal/aio` 的 18 处 `defer` 一处都不用改**，而它该用 `t.Cleanup` 的地方（`newTestFile:31`）**本来就用对了**。这比「改 90 处」或「只改 aio 的 18 处」都更准确地描述了这个仓库的实际约定。

### D3 · R2 采纳「收紧」而非「保持现状」

**问题**：`aio_other.go:32` 只判下界，Linux 两侧判双边，文档声明 `[1, 65536]`。

**选收紧。** 三条独立理由：
1. **文档已经声明了契约**，代码没兑现 —— 是代码错，不是文档错（`aio_internal.go:18` 的文案、`aio.go:135` 的 doc 都写 `[1, 65536]`）。
2. **有测试在证明它该被修**：`mode_test.go:47-57` 断言三种模式都拒绝三个越界值 —— 它现在被 `//go:build linux` 挡着才没在 macOS 上失败。**这个门控正在掩盖一个真实的平台行为不一致**，这本身比那个 1 行 bug 更值得修。
3. **影响面为零**：唯一生产调用方传编译期常量 `aioDepth = 256`（`internal/device/device.go:33,95`），永远落在合法区间内。

## 3. 机制设计：`probe_cache.go` 怎么在不改生产逻辑的前提下测（R3）

`probe_cache.go` **没有测试缝隙** —— `probe()` 是平台文件里声明的**函数**（`probe_linux.go:19` / `probe_other.go:9`），不是可替换的包级变量，测试无法替换它。

**不新造缝隙**（PRD R3 明确禁止改该文件的生产逻辑）。沿用**仓内既有的白盒手法** —— `mode_test.go:128-139` 已经开了先例：

```go
probeMu.Lock()
savedInfo, savedDone := probeInfo, probeDone
probeMu.Unlock()
t.Cleanup(func() {          // 还原，避免污染同包其他用例
	probeMu.Lock()
	probeInfo, probeDone = savedInfo, savedDone
	probeMu.Unlock()
})
```

测试要覆盖 `probe_cache.go` 的 8 条语句，分三个用例：

| 用例 | 手法 | 覆盖的语句 | 平台相关性 |
|---|---|---|---|
| **热缓存命中** | 预置 `probeDone = true`、`probeInfo = <哨兵值>`，调 `Probe()` | `:26-28`（`if probeDone` → return） | **完全平台无关** |
| **冷缓存穿透** | 预置 `probeDone = false`，连调两次，第二次前再预置哨兵值 | `:29-33`（调 `probe()`、`if info.Supported \|\| deterministic`、赋值、return） | 平台无关（断言的是「第二次是否穿透」，不是 `probe()` 的内容） |
| **并发安全** | N 个 goroutine 同时冷启动调 `Probe()`，断言全部返回**同一个** `Info` | `:24-25`（Lock / defer Unlock） | 平台无关 |

**为什么「热缓存」用例能证明缓存生效**：预置的哨兵值是 `probe()` 在任何平台都不可能返回的值。若 `Probe()` 返回了哨兵，就证明它**没有穿透**到底层 —— 这比「调用两次结果相同」强得多（后者在 `probe()` 恰好确定性返回时无法区分）。

**断言必须平台无关**：该测试文件**不带 build tag**，要在 macOS 与 Linux 上都过。所以不许断言 `Probe().Supported` 的具体取值（Linux 上取决于内核）。

## 4. 执行顺序（不可交换）

```
① R2  修 aio_other.go:32 的上界              ← 行为变更，先做
      ↓ 依赖
② R1  移 TestParseMode（独立）+ 移 TestNewWithOptionsInvalidMaxEvents（依赖①）
      ↓
③ R3  probe_cache.go 补测
      ↓
④ 实测新覆盖率 → 回填 spec 的数字
      ↓
⑤ R12 五处行为不变的重构
      ↓
⑥ R4–R11 七条纯 spec 文本修正
      ↓
⑦ 全量验证 + 提交
```

**为什么 R2 必须排在 R1 前**：`TestNewWithOptionsInvalidMaxEvents` 只有在①之后才能在 macOS 上通过（PRD F17 实测）。顺序颠倒会让②的测试在 macOS 上失败，而失败原因会被误读成「测试写错了」。

**为什么覆盖率回填（④）排在 spec 修正（⑥）前**：R4 要求把实测的新数字写进规则 12，而数字来自 ②③。若先写 spec 再测，spec 里的数字就是猜的 —— 这正是上一任务栽过的跟头（PRD Notes 里记的第一版覆盖率表）。

**为什么 R12 排在 spec 修正前**：R12 是代码改动，可能影响覆盖率数字的**分母**（删/加语句）。R12 的五处都是等价的改写，理论上不改分母，但先把代码定稿再取数，可以少取一次。

## 5. 兼容性

| 维度 | 影响 |
|---|---|
| **公开 API** | 无变化。`Ring` 接口、`Mode`、`NewWithOptions`、`Probe` 的签名与语义全部不动 |
| **行为（R2）** | 非 Linux 平台上 `NewWithOptions(65537, …)` 由「成功」变「返回 `errInvalidMaxEvents`」。**是收紧**，且生产路径传 `256`，不可达 |
| **Linux 生产路径** | 完全不变。R2 只改 `aio_other.go`（`!linux`）；R1/R3 只动测试；R12 的五处是等价改写 |
| **测试** | `mode_test.go` 减少两个测试（移走，不是删除）；新增两个测试文件（无 tag） |
| **spec 引用** | 修正 2 处 off-by-N 锚点；新增/改写的规则必须过 `verify_spec_refs.py` |

## 6. 验证策略

```bash
# 每步都跑：格式 + 分层不变量 + vet
make check

# 改过 .go 后必跑：linux 视角的 vet + 双架构 build + go test -c
make check-linux

# 覆盖率前后对比（本任务的核心量化指标）
go test -cover $(go list ./... | grep -v third_party | grep -v test/e2e)

# spec 引用完整性（脚本是上一任务的 research 产物；R13 禁止把它提升为仓库工具）
python3 .trellis/tasks/archive/2026-09/09-21-spec-upstream-alignment/research/verify_spec_refs.py .

# 占位清零
grep -rn "To be filled by the team" .trellis/spec/ || echo "无残留占位"

# 本机既有失败集合不许变（不是「全绿」）
go test ./... 2>&1 | grep -E '^(FAIL|---)' | sort > /tmp/after.txt
diff /tmp/before.txt /tmp/after.txt
```

**`/tmp/before.txt` 必须在动手前先取一份基线**（见 `implement.md` 第 0 步）。

**R12 的热路径改动额外要求**：按 `engine/buffer-and-concurrency.md` §七「热路径上的决策必须有实测支撑」，提交信息里要写明这些改写为什么不影响性能（`defer` 的代价、位置式字面量改具名是编译期等价、`if` 分支外提不改变分支数）。本任务**不要求**跑基准 —— 五处都是编译器可见的等价改写，且 `internal/aio` 没有 benchmark（规则 3 记的 `func Benchmark` 0 命中）。

## 7. 回滚形态

本任务**全部落在一个提交里**（除非你要求拆），所以回滚 = revert 那一个提交。按类的回滚代价：

| 类 | 单独回滚 | 说明 |
|---|---|---|
| A（spec 文本） | 零代价 | 纯 markdown，不影响构建 |
| B（测试 / 重构） | 零代价 | 行为不变 |
| **C（R2）** | 零代价 | 只影响非 Linux 的越界入参，生产不可达 |

**没有需要数据迁移或兼容窗口的改动** —— 这是本任务敢把 13 条一起做的原因。

## 8. 风险与对策

| 风险 | 对策 |
|---|---|
| **R12 的 `defer r.mu.Unlock()` 落在热路径** | 五处都是等价改写；`make check-linux` 强制过；提交信息写明理由。若复查时认为 `aio_linux.go` 的两处 `Unlock` 有「先解锁再处理 errno」的刻意意图，则**改为补注释说明意图**而不是改 `defer` —— 见 `implement.md` 的对应步骤 |
| **移测试时漏掉对平台符号的依赖** | 每个测试移出前先单独摘到无 tag 文件跑一次（本设计的三个决策都是这么定下来的，PRD F16/F17 已证实该手法可用） |
| **改 spec 数字时引入新的算错** | 所有计数都附可复现命令（R10 的要求），且写完立刻重跑该命令比对 |
| **R7(a) 的新类别被当成万能豁免** | 新类别的正文必须写明**它为什么满足规则**（可预测结束时间 + `Wait()` 是 join 点），而不是「允许不等待」。这一条是 R7 验收的一部分 |
| **顺手改了别的东西** | `git status --porcelain` 的验收项要求只出现预期文件 |
