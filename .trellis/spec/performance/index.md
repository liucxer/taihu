# performance —— 性能实践（gbp 类 8）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/performance-*.md`，2 条。
> 2 条都适用。**但这一层的诚实前提是：本仓库的性能瓶颈在设备 IO，不在 Go 层。**

## 先读这段：这一层的权重是有限的

taihu 的性能由**块设备的 IOPS / 带宽 / 队列深度**决定，不由 Go 的分配次数决定。gbp 这两条规则讲的是「减少分配与转换开销」，在 HTTP 服务里那是 CPU 热点，在这里通常只占服务端 CPU 的一小部分。

**上游那两条规则附带的 benchmark 数字（`30000 ns/op → 8000 ns/op`、`4x faster`）在本仓库的语境下不成立**——它们测的是纯内存循环，而本仓库一次 Put 要落盘、要发 `io_uring`、要过 RPC。**不要拿那些数字来论证本仓库的优化优先级。**

所以本层的规则是**代码卫生要求，不是性能目标**：写得对，但你不会因为遵守它们而看到吞吐变化。真正的性能工作走 `internal/benchkit` 与 `doc/性能测试报告/`。

## 逐条裁决

### gbp-047 · Container Preallocation（HIGH）— **适用，当前符合**

**规则**：已知大小时预分配 slice / map 容量；`make([]T, 0, n)` 配 `append`，或 `make([]T, n)` 配下标赋值；从其他容器构造时按其大小预分配；大小未知时用合理初始估计（如 `make([]T, 0, 64)`）。

**对 taihu：适用，本仓库已是主流做法，且有一条很强的证据。**

实测：非测试代码里 `make([]...)` 共 **80** 处，**没有一处是裸 `make([]T)`** —— 即**每一处都显式给了长度或容量**。

| 形态 | 数量 | 例子 |
|---|---|---|
| `make([]T, 0, N)`（预估容量 + append） | 29 | `make([][]byte, 0, len(keys))`、`make([]SegmentEntry, 0, len(entries))`、`make([]Event, 0, max)` |
| `make([]T, len(src))` 一类精确长度 | 25 | `make([]batchSpec, len(specs))`、`make([][]byte, len(keys))`、`make([]time.Duration, len(lat.vals))` |
| `make([]T, N)`，N 为变量 | 16 | `make([]device.WriteJob, 0, total)`、`make([]string, 0, c.Count)` |
| `make([]byte, N)` 的定长/小计算缓冲 | 10 | `make([]byte, 8)`、`make([]byte, 4+len(key))`、`make([]byte, 24)` |
| **裸 `make([]T)`（无长度无容量）** | **0** | ✅ |

合计 29 + 25 + 16 + 10 = **80**。

map 也部分预分配：`make(map[string]ObjectMeta, len(keys))`、`make(map[string]int, len(keys))`、`make(map[string]cluster.InstanceInfo, len(all))`、`make(map[string]*rpcclient.Storage, len(old))`。

**核实命令（注意用 `[^)]*` 而不是枚举类型的字符类——BSD grep 会在后者上失配）**：

```bash
# 总数
grep -rnoE 'make\(\[\][^)]*\)' --include='*.go' internal cmd pkg | grep -v _test.go | wc -l
# 裸 make([]T)：应为 0
grep -rnoE 'make\(\[\][A-Za-z0-9_.\[\]*]*\)' --include='*.go' internal cmd pkg | grep -v _test.go | wc -l
```

**一个必须写明的例外：过滤循环不要预分配源长度。**

本仓库有 20 处 `var x []T` 加循环内 `append`，它们**全部**带过滤条件（`continue` 或 `if`），最终长度**小于**源长度：

```go
// internal/storage/compact.go:110 —— 只有空洞率超阈值的段才进 cands
var cands []cand
for id, a := range aggs {
    if !found || ... { continue }
    if hole >= c.cfg.HoleThreshold || force {
        cands = append(cands, cand{...})
    }
}
```

**这类循环不适用本条的上界预分配**，理由是 gbp 那条规则自己没覆盖的：如果过滤条件很挑（如 `compact.go` 要空洞率超阈值，可能几百个段只命中几个），`make([]cand, 0, len(aggs))` 会**先分配一大块再只用几个位置**——比让 runtime 按几何增长多分配几次更浪费。

**判据**：过滤后**预期存活比例 > 50%** → 预分配 `len(src)`；否则用 `var` 让它自己长。**不要机械地把这 20 处都加上 `len(src)`。**

**一条本仓库特有的提醒**：`internal/aio` 的缓冲区**不走这条规则的思路**。那里的大缓冲由 `internal/bufpool` 管理，而 `sync.Pool` 是**被实测否决过的**——`internal/bufpool/bufpool.go:10-14` 记录了依据（GC 清空导致 4M/8M 大缓冲每轮重分配）。**要动缓冲策略，先读那段，不要再提 `sync.Pool`。**

### gbp-048 · Use strconv Instead of fmt（MEDIUM）— **适用，有 1 处真实偏离**

**规则**：基本类型转换用 `strconv` 不用 `fmt`；`strconv.Itoa` / `FormatInt` / `ParseInt` 等；复杂格式化、调试输出、格式化写 `io.Writer` 仍用 `fmt`；**循环内反复转换要改掉**。

**对 taihu：适用，当前基本符合。** 实测：

| 项 | 数量 | 说明 |
|---|---|---|
| `strconv.*` | 4 | `strconv.Itoa` 3 处：`cmd/taihu/cmd/server.go:101`、`cmd/taihu/cmd/server.go:176`、`cmd/taihu/cmd/server.go:306`。`strconv.FormatUint` 1 处：`internal/aio/aio_uring_linux.go:413` |
| `fmt.Sprintf` | 15 | 其中 **14 处是复杂格式化**（多参数、`%v` 包错误、`%.2f GB`），属规则的豁免范围 |

14 处豁免的都符合规则的「When to Use fmt」：`fmt.Sprintf("io_uring_setup 失败: %v", err)`、`fmt.Sprintf("%.2f GB", ...)`、`fmt.Sprintf("%d/%s", r.CursorSeg, off)` 这类**多值拼接**用 `strconv` 反而更难读。

**⚠️ 1 处真实偏离：`internal/benchkit/run.go:67`**

```go
// KeyFor 返回第 seq 个对象的 key。
func KeyFor(prefix string, seq int) string {
	return fmt.Sprintf("%s/%d", prefix, seq)
}
```

**为什么这处算问题**：它**在测量回路里**——`internal/benchkit/run.go:125`（写阶段）与 `:178`（读阶段）每个对象、每次操作都调一次。这正是规则里「Conversions in Loops」点名的那种：

```go
key := prefix + "/" + strconv.Itoa(seq)
```

**注意这条处置的收益不在吞吐上**：`internal/benchkit` 测的是设备 IO，一次 `Sprintf` 的分配相对一次 io_uring 提交可以忽略。**改它是为了不让客户端侧的分配出现在基准数据里**——如果哪天要看客户端 CPU 占比，这处会成为噪声源。

**所以这一处的处置优先级低**，不改不影响任何门禁。改动时注意 `KeyFor` 有 5 个调用点（含 2 处测试断言 `"rbench/7"`、`"/0"`），**输出必须逐字节不变**。

---

## 本层与其它层的关系

- **预分配的「什么时候不要用」** 是 [idiomatic/](../idiomatic/index.md) 的 **gbp-036** 没写全的地方——上游那条规则的 slice/map 部分与本条重叠，但它对「过滤循环」的取舍没有讨论。**以本条为准。**
- **缓冲区池化**（`internal/bufpool`）是性能层最真实的优化点，但它**不属于 gbp 任何一条规则**——它是本仓库自己实测出来的。要看它读 `internal/bufpool/bufpool.go` 的包注释。
- **性能测量的正确入口**是 `internal/benchkit` 与 `test/e2e/` 的 F / G 分组，不是 `go test -bench`（原因见 [testing/](../testing/index.md) 的 gbp-044）。

## 核查脚本

```bash
# gbp-047：预分配形态分布
grep -rnoE 'make\(\[\][^)]*\)' --include='*.go' internal cmd pkg | grep -v _test.go \
  | sed 's/.*:make/make/' | sort | uniq -c | sort -rn

# gbp-047：过滤循环的候选（逐个判断过滤强度，不要一律预分配）
grep -rnE '^\s*var [a-zA-Z0-9_]+ \[\]' --include='*.go' internal cmd pkg | grep -v _test.go

# gbp-048：strconv vs fmt 用量
grep -rn 'strconv\.' --include='*.go' internal cmd pkg | grep -v _test.go
grep -rn 'fmt\.Sprintf' --include='*.go' internal cmd pkg | grep -v _test.go
```
