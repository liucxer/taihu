# performance —— 性能实践（gbp 类 8）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/performance-*.md`，2 条。
> 2 条都适用。**但这一层的诚实前提是：本仓库的性能瓶颈在设备 IO，不在 Go 层。**

## 先读这段：这一层的权重是有限的

taihu 的性能由**块设备的 IOPS / 带宽 / 队列深度**决定，不由 Go 的分配次数决定。gbp 这两条规则讲的是「减少分配与转换开销」，在 HTTP 服务里那是 CPU 热点，在这里通常只占服务端 CPU 的一小部分。

**上游那两条规则附带的 benchmark 数字（`30000 ns/op → 8000 ns/op`、`4x faster`）在本仓库的语境下不成立**——它们测的是纯内存循环，而本仓库一次 Put 要落盘、要发 `io_uring`、要过 RPC。**不要拿那些数字来论证本仓库的优化优先级。**

所以本层的规则是**代码卫生要求，不是性能目标**：写得对，但你不会因为遵守它们而看到吞吐变化。真正的性能工作走 `internal/benchkit` 与 `doc/性能测试报告/`。

## 逐条裁决

### gbp-047 · Container Preallocation（HIGH）— **适用，当前符合**

**规则**：已知大小时预分配 slice/map 容量；`make([]T, 0, n)` 配 `append`，或 `make([]T, n)` 配下标赋值；从其他容器构造时按其大小预分配；大小未知时用合理初始估计（如 `make([]T, 0, 64)`）。

**对 taihu：适用，本仓库已是主流做法，且有一条很强的证据。**

实测（**2026-10-02** 在仓库根重跑下文本节的核查命令，排除 `_test.go` 与 `third_party`）：非测试代码里 `make([]...)` 共 **103** 处，**没有一处是裸 `make([]T)`**——即**每一处都显式给了长度或容量**。

| 形态 | 数量 | 例子 |
|---|---|---|
| `make([]T, 0, N)`（三参数，预估容量 + append） | 39 | `make([][]byte, 0, len(keys))`、`make([]Event, 0, max)`、`make([]ioEvent, 0, 64)` |
| `make([]T, len(src))` 一类精确长度（两参数、第二参数含 `len(`） | 40 | `make([][]byte, len(keys))`、`make([]batchSpec, len(specs))`、`make([]time.Duration, len(lat.vals))` |
| `make([]T, N)`，N 为变量/算术表达式（两参数、非 `byte`） | 7 | `make([]iocb, n)`、`make([]chan *writeTask, workers)`、`make([]int, e0-s0)` |
| `make([]byte, N)` 两参数定长/小计算缓冲 | 20 | `make([]byte, 8)`、`make([]byte, 4+len(key))`、`make([]byte, 24)` |
| **裸 `make([]T)`（无长度无容量）** | **0** | ✅ |

**口径说明（各行有重叠，不要直接相加；39+40+7+20=106 ≠ 103）：**

- 三参数 39 + 两参数 64 = **103**。
- 「含 `len(`」共 **71** 个：第 1 行的 39 个里有 31 个容量参数是 `len(...)`，加上第 2 行两参数含 `len(` 的 40 个，31+40=71。
- 「`[]byte`」共 **23** 个：两参数 20 个（第 4 行）+ 三参数 3 个（`make([]byte, 0, len(kvSegmentPrefix))` 等，计入第 1 行）。第 4 行里另有 4 个表达式含 `len(`（`4+len(key)` ×3、`4+len(entries)` ×1），同时被第 2 行口径计入。
- grep 正则 `[^)]*` 遇到类型名内含括号的 `make([]func(), len(keys))`（`internal/rpcclient/storage_rpc.go:117`）会在第一个 `)` 处截断显示；该行计入总数 103，但不在「含 `len(`」的 71 里——按正则口径报 71，实际含 `len` 的有 72 处。

map 也部分预分配：`internal/metastore/model.go:229` 的 `make(map[string]ObjectMeta, len(sorted))`、`internal/metastore/kv_pebble.go:137` 的 `make(map[string]int, len(keys))`、`pkg/taihu-client/registry.go:21` 的 `make(map[string]cluster.InstanceInfo, len(all))`、`pkg/taihu-client/storage.go:172` 的 `make(map[string]*rpcclient.Storage, len(old)+1)`。

**核实命令（注意用 `[^)]*` 而不是枚举类型的字符类——BSD grep 会在后者上失配）**：

```bash
# 总数（internal cmd pkg 即 103；examples 0 命中，一并扫不影响数字）
grep -rnoE 'make\(\[\][^)]*\)' --include='*.go' internal cmd pkg examples | grep -v _test.go | wc -l
# 裸 make([]T)：应为 0
grep -rnoE 'make\(\[\][A-Za-z0-9_.\[\]*]*\)' --include='*.go' internal cmd pkg examples | grep -v _test.go | wc -l
```

**一个必须写明的例外：过滤循环不要预分配源长度。**

核查脚本对 `var x []T` 的命中从原 20 处涨到 **33 处（2026-10-02 重跑）**。逐个看过上下文后，它们**不是同一种东西**，不能再像旧版那样一概称为「带过滤条件的循环」：

- **9 处是真正的过滤循环**（`continue`/条件 `append`，最终长度**小于**源长度）：`internal/storage/compact.go:80`、`internal/storage/admin.go:56`、`internal/metastore/segments.go:327`、`internal/metastore/kv_pebble.go:207`、`cmd/taihu/cmd/server.go:81`、`cmd/taihu/cmd/helpers.go:83`、`pkg/taihu-client/picker.go:76`、`pkg/taihu-client/storage.go:246`、`pkg/taihu-client/storage.go:343`。
- **5 处源长度其实已知**，按下方 >50% 判据属于「可以改成预分配」的候选，而不是反例：`internal/device/device.go:508`（`specs`，源 `len(jobs)`）、`internal/device/device.go:509`（`tmps`，仅不对齐/带尾的 job 存活，比例不定）、`internal/transport/server_shm_linux.go:780`（`slots`，个数 = `(end-off)/ChunkSize` 的定长切块）、`cmd/taihu/cmd/server.go:318`（`lns`，≤ `len(ips)`，但在启动期端口抢占的有界重试里，收益微小）、`pkg/taihu-client/storage.go:257`（`rels`，≤ `len(keys)`）。这些留作顺手改，**不设门禁**。
- **5 处由网络/流式输入驱动，源大小本来就未知**：`internal/transport/client_admin.go:116`（分页聚合 key）、`internal/transport/server_shm_linux.go:687`（按数据帧攒 `jobs`）、`internal/transport/server_shm_linux.go:130`（timer 攒批 `drain`）、`internal/transport/server.go:356` 与 `internal/transport/server_shm_linux.go:324`（在途保序队列）。这里用 `var` 本身就是合理选择。
- **14 处是核查正则的噪声**：声明后整体赋值或由 `bufpool`/零拷贝赋值、从不靠 `append` 增长——`internal/transport/client.go:284`（随后在 `:320` 被赋为 `make([][]byte, len(keys))`）、`internal/transport/client.go:323`、`internal/transport/client.go:324`、`internal/transport/client_shm_linux.go:370`、`internal/transport/client_shm_linux.go:371`、`internal/transport/client_shm_linux.go:484`、`internal/transport/client_shm_linux.go:485`（响应汇入缓冲，池化/零拷贝赋值）、`internal/benchkit/run.go:146`、`internal/benchkit/run.go:208`、`internal/benchkit/run.go:277`、`cmd/taihu/cmd/bench_storage.go:263`（读结果持有变量）、`cmd/taihu/cmd/web.go:342`、`cmd/taihu/cmd/web.go:387`、`cmd/taihu/cmd/instance.go:60`（分支内整体赋值）。

过滤循环的代表仍是 compact 候选收集（原锚点 `compact.go:110` 已位移）：

```go
// internal/storage/compact.go:80-93 —— 只有空洞率超阈值的段才进 cands
var cands []cand
for id, a := range aggs {
    if !found || (sm.State != metastore.SegmentStateFull && sm.State != metastore.SegmentStateCompacting) {
        continue
    }
    if hole >= c.cfg.HoleThreshold || force {
        cands = append(cands, cand{id: id, holeRatio: hole, keys: a.keys})
    }
}
```

**这类循环不适用本条的上界预分配**，理由是 gbp 那条规则自己没覆盖的：如果过滤条件很挑（如 `compact.go` 要空洞率超阈值，可能几百个段只命中几个），`make([]cand, 0, len(aggs))` 会**先分配一大块再只用几个位置**——比让 runtime 按几何增长多分配几次更浪费。

**判据**：过滤后**预期存活比例 > 50%** → 预分配 `len(src)`；否则用 `var` 让它自己长。**不要机械地把过滤循环那 9 处都加上 `len(src)`；上面 5 处「源长度已知」的候选倒是可以按此判据逐个改。**

**一条本仓库特有的提醒**：`internal/aio` 的缓冲区**不走这条规则的思路**。那里的大缓冲由 `internal/bufpool` 管理（2026-09-24 曾迁至 `pkg/bufpool`，2026-10-02 迁回 `internal/bufpool`，原因见包注释），而 `sync.Pool` 是**被实测否决过的**——`internal/bufpool/bufpool.go:13-17` 记录了依据（GC 清空导致 4M/8M 大缓冲每轮重分配，实测冷分配约占服务端 CPU 36%）。**要动缓冲策略，先读那段，不要再提 `sync.Pool`。**

### gbp-048 · Use strconv Instead of fmt（MEDIUM）— **适用，有 2 处同型真实偏离**

**规则**：基本类型转换用 `strconv` 不用 `fmt`；`strconv.Itoa` / `FormatInt` / `ParseInt` 等；复杂格式化、调试输出、格式化写 `io.Writer` 仍用 `fmt`；**循环内反复转换要改掉**。

**对 taihu：适用，当前基本符合。** 实测（**2026-10-02** 重跑文末核查脚本）：

| 项 | 数量 | 说明 |
|---|---|---|
| `strconv.*` | 11 | **`internal/web` 6 处（HTTP 查询参数/响应头，全部为正确用法）**：`internal/web/handlers.go:122`（`ParseBool` 解析 `confirm`）、`internal/web/key_handlers.go:51`（`Atoi` 解析 `limit`）、`internal/web/key_handlers.go:75`（`ParseInt` 解析 `off`）、`internal/web/key_handlers.go:80`（`ParseInt` 解析 `size`）、`internal/web/key_handlers.go:94`（`Itoa` 设 `Content-Length` 头）、`internal/web/key_handlers.go:124`（`ParseInt` 解析表单 `size`）。其余 5 处：`internal/aio/ring_uring_linux.go:237`（`FormatUint`，io_uring 参数校验错误信息）、`cmd/taihu/cmd/server.go:102`、`cmd/taihu/cmd/server.go:177`、`cmd/taihu/cmd/server.go:321`（`Itoa` 配 `net.JoinHostPort`）、`examples/taihu-client/main.go:50`（`Itoa` 拼 ClientID） |
| `fmt.Sprintf` | 17 | 其中 **15 处豁免**（多值拼接、`%v` 包错误、单位/诊断输出，均不在测量回路），**2 处是同型真实偏离**，见下 |

路径变迁注记：原 `strconv.FormatUint` 锚点位于已删除的旧文件 `aio_uring_params_linux.go:105`（旧路径勿再引用）——参数校验（含 `uringParamError`）后并入 `internal/aio/ring_uring_linux.go`，现锚点为 `internal/aio/ring_uring_linux.go:237`。

**关于 `internal/web` 的 6 处：这是 gbp-048 的正面样本，不是偏离。** HTTP 查询参数和表单值到手上都是字符串，解析成 bool/int **只能走 `strconv.ParseBool`/`Atoi`/`ParseInt`**（还要处理 `err` 与默认值），整型响应头同理走 `Itoa`。新增 HTTP 管理面（`internal/web`）时照此办理，不要引 `fmt.Sscan` 一类。

15 处豁免的都符合规则的「When to Use fmt」：多值拼接如 `fmt.Sprintf("%d/%s", r.CursorSeg, off)`（`cmd/taihu/cmd/cluster.go:195`）、`fmt.Sprintf("%s+%s+%d", ...)`（`cmd/taihu/cmd/helpers.go:37`）、`fmt.Sprintf("auto: probe ok, features=0x%x%s", ...)`（`internal/aio/aio.go:196`）；`%v` 包错误如 `internal/aio/probe_linux.go:104`、`internal/web/web.go:227`；单位展示 `cmd/taihu/cmd/helpers.go:145-151`（`%.2f GB`/`%d B` 等 4 处）；一次性诊断 `cmd/taihu/cmd/instance.go:135`、`cmd/taihu/cmd/cluster.go:353`。另有 `cmd/taihu/cmd/server.go:345` 的 `fmt.Sprintf(":%d", port)` 是其中唯一的单值 `%d`，机械上可换成 `":"+strconv.Itoa(port)`（同文件 `:321` 就用了 `net.JoinHostPort` 的等价写法），但它在启动期端口抢占的有界重试循环里、不在测量回路，保留 `fmt` 不判为偏离。

**⚠️ 2 处同型真实偏离：bench key 生成用 `fmt.Sprintf("%s/%d", ...)` 而非 `strconv.Itoa`**

第 1 处（原 spec 已记录，2026-10-02 复核**仍未修**，锚点从 `run.go:67` 位移到 `:80-82`）：

```go
// internal/benchkit/run.go:80-82
// KeyFor 返回第 seq 个对象的 key。
func KeyFor(prefix string, seq int) string {
	return fmt.Sprintf("%s/%d", prefix, seq)
}
```

第 2 处（**2026-10-02 复核新补录**：git 记录显示 `fc70f49` 合并统一命令时即已存在，旧版 spec 的 grep 漏统）：

```go
// cmd/taihu/cmd/bench_storage.go:236-239
// keyFor 生成 <prefix>/<seq>，写与读共用同一生成规则，保证 key 可对齐。
func (c *storageBenchConfig) keyFor(seq int) string {
	return fmt.Sprintf("%s/%d", c.prefix, seq)
}
```

**为什么这两处算问题**：它们**都在测量回路里**，每个对象、每次操作调一次：

- `internal/benchkit/run.go:139`（`runWorker`，write/read 逐 op）、`internal/benchkit/run.go:206`（`runPipelinedWorker`，pipeline 模式逐 op）、`internal/benchkit/run.go:269`（`runBatchedWorker`，每批生成 key）；
- `cmd/taihu/cmd/bench_cluster.go:118`（读模式预热路由缓存，按 `c.Count` 逐 key 调 `benchkit.KeyFor`）；
- `cmd/taihu/cmd/bench_storage.go:256`（storage bench 的 `runWorker` 循环内调 `c.keyFor(k)`）。

这正是规则里「Conversions in Loops」点名的那种，正确写法是字符串拼接：

```go
// KeyFor
key := prefix + "/" + strconv.Itoa(seq)
// storageBenchConfig.keyFor
key := c.prefix + "/" + strconv.Itoa(seq)
```

**注意这条处置的收益不在吞吐上**：`internal/benchkit` 与 storage bench 测的都是设备 IO，一次 `Sprintf` 的分配相对一次 io_uring 提交可以忽略。**改它们是为了不让客户端侧的分配出现在基准数据里**——如果哪天要看客户端 CPU 占比，这两处会成为噪声源。

**所以这两处的处置优先级低**，不改不影响任何门禁。改动时注意 `KeyFor` 现有 **7 个调用点**（生产 4：`internal/benchkit/run.go:139`、`:206`、`:269`、`cmd/taihu/cmd/bench_cluster.go:118`；测试 3：`internal/benchkit/benchkit_test.go:40` 的 seed 填充、`:137` 断言 `"rbench/7"`、`:140` 断言 `"/0"`），**输出必须逐字节不变**。`keyFor` 目前只有 `cmd/taihu/cmd/bench_storage.go:256` 一个调用点、无直接测试断言，但写读共用同一生成规则的语义（见 `:236` 注释）同样要求改后输出不变。

---

## 本层与其它层的关系

- **预分配的「什么时候不要用」** 是 [idiomatic/](../idiomatic/index.md) 的 **gbp-036** 没写全的地方——上游那条规则的 slice/map 部分与本条重叠，但它对「过滤循环」的取舍没有讨论。**以本条为准。**
- **缓冲区池化**（`internal/bufpool`，2026-10-02 起从 `pkg/bufpool` 迁回）是性能层最真实的优化点，但它**不属于 gbp 任何一条规则**——它是本仓库自己实测出来的。要看它读 `internal/bufpool/bufpool.go:13-17` 的实现说明与包注释。
- **性能测量的正确入口**是 `internal/benchkit` 与 `test/e2e/` 的 F/G 分组（`test/e2e/f_soak_test.go`、`test/e2e/g_soak_long_test.go`），不是 `go test -bench`（原因见 [testing/](../testing/index.md) 的 gbp-044）。

## 核查脚本

以下命令均在仓库根执行，排除 `_test.go`；目录写 `internal cmd pkg examples`（`third_party` 不扫；`examples` 仅对 strconv 贡献 1 处命中，对其余三命令 0 命中）。2026-10-02 的实测结果附在每条注释后。

```bash
# gbp-047：预分配形态分布（103 行；三参数 39、两参数 64）
grep -rnoE 'make\(\[\][^)]*\)' --include='*.go' internal cmd pkg examples | grep -v _test.go \
  | sed 's/.*:make/make/' | sort | uniq -c | sort -rn

# gbp-047：裸 make([]T)，应为 0
grep -rnoE 'make\(\[\][A-Za-z0-9_.\[\]*]*\)' --include='*.go' internal cmd pkg examples | grep -v _test.go | wc -l

# gbp-047：「var x []T」候选（33 行命中；需人工分类：9 过滤循环 / 5 源长已知可预分配 /
# 5 流式输入大小未知 / 14 正则噪声，分类明细见上文，不要一律预分配）
grep -rnE '^\s*var [a-zA-Z0-9_]+ \[\]' --include='*.go' internal cmd pkg examples | grep -v _test.go

# gbp-048：strconv vs fmt 用量（11 / 17；strconv 的 11 含 examples/taihu-client/main.go:50，
# 不含 examples 扫 internal cmd pkg 为 10）
grep -rn 'strconv\.' --include='*.go' internal cmd pkg examples | grep -v _test.go
grep -rn 'fmt\.Sprintf' --include='*.go' internal cmd pkg examples | grep -v _test.go
```
