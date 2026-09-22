# gbp 逐条三分类

> 来源：`research/golang-base-practices.md`（53 条，`gbp-001` ~ `gbp-053`）
> 分类：**适用 32 条 / 冲突 3 条 / 不适用 18 条，合计 53**
> 降权说明：本来源的「依据」不可逐条验证（53 个规则文件无 `references:` 字段，四份上游资料只在包级别被整体声明过一次；全库唯一一处点名上游的是 `idiomatic-embedding.md` 的 `(Uber Style)`，即 `gbp-038`）。因此**判定只按规则内容本身 + taihu 锚点**，下文不出现「Uber/Google 也这么规定」这类转述。
> 与 `triage.md` §3.1 一致：该节先期判定不适用的 `gbp-001~004`、`005~009`、`010~015`、`020`、`040`、`045`（共 18 条）在本文件里**逐条对得上，未推翻**，只补了理由。
> 锚点纪律：下表每个 `file:line` 都用 `sed -n '<N>p' <file>` 亲自打印过，行内容与陈述对得上。

---

## 一、适用（32 条）

| 编号 | 规则一句话 | taihu 锚点（file:line，已核实） | 正例/反例 |
|------|-----------|-------------------------------|----------|
| gbp-016 | 用 `fmt.Errorf("...: %w", err)` 包装错误保留调用链 | 正例 `internal/cluster/kv_tikv.go:106`（`return nil, fmt.Errorf("tikv txnkv connect: %w", err)`）；反例 `internal/cluster/register.go:50`（`return nil, err` 裸返回） | **两者都有**。非测试代码 `%w` 包装 38 处，同时裸 `return nil, err` / `return err` 共 185 处。已由 `architecture/code-style.md` 规则 8 规定主导形态，是**部分遵守**而非全仓统一 |
| gbp-017 | 用包级哨兵错误表达可预期错误条件 | `internal/ierr/ierr.go:11`（`ErrNotFound = errors.New("taihu: key not found")`）；包内未导出哨兵 `internal/aio/aio_internal.go:18`、`internal/transport/frame.go:34`；消费侧 `internal/transport/protocol/protocol.go:115`（`errors.Is(err, ierr.ErrNotFound)`） | **正例**，且比上游更严：`architecture/error-model.md` §1 把 `internal/ierr` 定为唯一事实源，§3 规定包内控制信号用小写未导出哨兵（`err` 而非 `Err` 前缀） |
| gbp-018 | 需要携带额外信息时定义自定义错误类型 | `internal/aio/aio_uring_linux.go:399`（`type uringParamError struct`）+ `:404`（`Error()`）；约束见 `architecture/error-model.md:38`、`:50` | **正例但收窄**。全仓唯一自定义 error 类型是不导出的诊断载体（带 `field` / `got`）；`architecture/error-model.md:50` 明确「不要新增导出的自定义 error 类型」，即上游那半边（导出类型 + `errors.As` 映射 JSON）在 taihu 被刻意否决 |
| gbp-019 | 永远不要忽略 error 返回值，真不需要时加注释 | 反例 `internal/transport/server_shm_linux.go:427`（`_ = rerr`，上一行刚拿到错误，此处静默丢弃且**无任何注释**）；正例（测试里归还池）`internal/transport/transport_shm_test.go:77`（`t.Cleanup(func() { _ = c.Close() })`） | **反例**。`server_shm_linux.go:427` 是「取到 `rerr` 又立刻丢掉、函数仍 `return nil`」的真吞错，恰好落在规则点名的那种写法上 |
| gbp-021 | panic 仅用于不可恢复错误，recover 只在 defer 中有效 | 非测试代码 `panic(` **零命中**（`internal/`、`pkg/`、`cmd/` 三处 grep 均空）；`recover()` 仅 2 处且都在 `internal/transport/protocol/protocol_test.go:325`、`:341` | **正例**。与 `architecture/code-style.md` 规则 4「非测试代码不 panic」同一结论；库侧无需 recover，因为没有 HTTP/框架边界 |
| gbp-022 | 每个 goroutine 都必须有明确退出条件，避免泄漏 | `internal/storage/compact.go:53`（`c.wg.Add(1)`）→ `:54`（`go c.run()`）→ `:59`（`close(c.stop)`）→ `:60`（`c.wg.Wait()`）；`internal/device/device.go:108`（`pumpDone`）、`:110`（`go d.pump()`）；`internal/transport/server_shm_linux.go:224`（`s.wg.Wait()`） | **正例**，且是标准形态：`stop chan struct{}` + `sync.WaitGroup` + `close` 后 `Wait`。`device` 的完成泵另有更严的退出条件（`closed` 且 `m` 空且 `inSubmit==0`），见 `engine/buffer-and-concurrency.md` §三 |
| gbp-023 | 正确使用 channel：超时、非阻塞、fan-out/fan-in 等模式 | 信号 channel `internal/aio/aio_other.go:37`（`wake: make(chan struct{}, 1)`）、`internal/transport/frame.go:55`（`done: make(chan struct{})`）；fan-in 收尾 `internal/benchkit/run.go:91`（`fin := make(chan error, cfg.Threads)`）→ `:212`（`wg.Wait()`）；cap=1 非阻塞回投 `internal/device/device.go:215`（`ch := make(chan aio.Event, 1)`，注释「cap=1，不阻塞」在 `:167`） | **正例**。`struct{}` 信号 channel、`done` 关闭广播、worker + 结果 channel 收尾的形态齐全 |
| gbp-024 | channel 缓冲应为 0 或 1，大缓冲需书面论证 | 有论证：`internal/transport/frame.go:31`（`// streamInCap 每流投递缓冲上限：读循环背压到流处理器消费速度。`）+ `:32`（`const streamInCap = 8`）；`internal/transport/batch.go:28`（`// 各字段 ≤0 表示对应流水线关闭…`，攒批理由在文件头 `:1-17`）；**无论证**：`pkg/taihu-client/index.go:39`（`ch: make(chan indexItem, 4096)`）；`internal/transport/batch.go:91`（`make(chan *writeTask, batchCap*2)`）、`internal/transport/server_shm_linux.go:114`（`target*2`） | **正例 + 反例**。`frame.go` 的 8 是规则要求的「书面论证」范式；`index.go:39` 的 4096 **一个字都没解释**（唯一邻近注释 `:26-27` 只说「异步批量写 KV」，不涉及容量）——这是可指认的缺口 |
| gbp-025 | 用 context 传播取消信号与超时 | 正例 `cmd/taihu/cmd/root.go:112`（`context.WithTimeout(parent, global.timeout)`，`--timeout` 可配）；子超时从父 ctx 派生 `cmd/taihu/cmd/cluster.go:279`、`:295`；反例 `internal/transport/server.go:83`（`context.WithTimeout(context.Background(), 5*time.Second)`，不接参数、与父 ctx 断开、无注释） | **正例为主 + 1 处反例**。该反例已在 `triage.md` A-02 / `conflicts.md` C-03 记录，此处不重复裁决 |
| gbp-026 | 用 errgroup 简化并发任务与错误传播 | 反例 `internal/benchkit/run.go:168`（`var wg sync.WaitGroup`）→ `:212`（`wg.Wait()`），错误经 chan 手工汇聚；`pkg/taihu-client/storage.go:221`（`ch := make(chan string)`）→ `:236`（`wg.Wait()`） | **反例（全仓零 errgroup）**。`grep -rn errgroup` 在 `internal/`、`pkg/`、`cmd/` 全空，`golang.org/x/sync v0.10.0` 在 `go.mod` 里是 **indirect**。fan-out 一律手写 `WaitGroup` + 错误 channel。**注意这条与 design.md §3.2 的关系**：锚点是「未采用」而非「已违反」，若要并入需先裁决（见附：拿不准） |
| gbp-027 | 正确使用 sync 包原语（Mutex/RWMutex/Once/Pool/Map） | `internal/transport/server.go:45`（`mu sync.Mutex`，零值即可用）、`internal/transport/frame.go:80`（`wmu sync.Mutex // 串行化连接写…`）、`internal/cluster/kv_mem.go:12`（`mu sync.RWMutex`）、`internal/transport/frame.go:51`（`once sync.Once`）、`internal/aio/aio_uring_linux.go:184`（`var uringOverflowOnce sync.Once`） | **正例，但 `sync.Pool` 一项刻意偏离**：`internal/bufpool/bufpool.go:10-13` 记录了否决 `sync.Pool` 的实测依据（GC 清空导致 4M/8M 大缓冲整批重分配，实测占服务端 CPU 36%）。规则其余 4 个原语 taihu 都在正确使用 |
| gbp-028 | 用 `go test -race` 检测数据竞争，CI 必须开启 | 反例 `Makefile:21-22`（`check: check-fmt check-layering check-sdk-only` + `go vet ./...`，**无 `-race`**）、`:25-28`（check-fmt）；全仓无 CI 文件（无 `.github/`、无 `.gitlab-ci.yml`）；`-race` 只出现在**开发笔记**里（`.trae/documents/aio-device-integration.md:44,50`） | **反例**。门禁里没有任何 `-race` 目标，也没有 CI 可挂；`-race` 目前靠人手敲。这是一个可指认的缺口 |
| gbp-029 | 遵循 Go 命名惯例（包名、缩写、接口 `-er`） | `internal/transport/client_admin.go:31`（`Meta(...) (segID, off, size int64, err error)`）、`internal/transport/protocol/protocol.go:350`（`func EncodeMetaResp(segID, off, size int64) []byte`）；`cmd/taihu/cmd/server.go:101`（`strconv.Itoa`）；全仓无 `ClientId` / `userId` / `Url` 形态 | **正例**。与 `architecture/code-style.md` 规则 6 同结论（`ClientID`、`ioUringIO`），且 `pkg/taihu-client` 目录名带连字符而包名是 `taihuclient`（`config.go:7`） |
| gbp-030 | 按 Go 惯例写文档注释，错误字符串小写无句点 | `internal/ierr/ierr.go:1`（`// Package ierr 定义 taihu 存储的公共错误 —— **唯一事实源**。` 以被描述项开头）、`:11`（`errors.New("taihu: key not found")` 小写、无句点）；未导出哨兵同样小写 `internal/transport/frame.go:34`（`errors.New("taihu: connection closed")`） | **正例**。注：注释**语言**是中文（`architecture/code-style.md` 规则 1 的刻意约定），但上游要的是「以被描述项开头、写为什么」这个结构，taihu 满足 |
| gbp-031 | 定义小接口并按需组合（接口隔离），接口由消费方定义 | `internal/rpcclient/objectstore.go:29-35`（5 方法 `ObjectStore`）、`:37`（`var _ ObjectStore = (*Storage)(nil)`）；`internal/cluster/kv_mem.go:16`（`var _ KV = (*MemoryKV)(nil)`）；`internal/metastore/kv_pebble.go:31`（`var _ Store = (*pebbleStore)(nil)`）；`pkg/taihu-client/storage.go:49`（`var _ rpcclient.ObjectStore = (*Storage)(nil)`） | **正例为主 + 一处带理由的偏离**。「小接口 + 编译期断言」齐全（8 处 `var _`）；但接口**声明在实现侧包**（`internal/rpcclient`），上游主张的「接口由消费方定义」被否决 —— 理由写在 `internal/rpcclient/objectstore.go:13`（「taihuclient 依赖本包，反向声明会成环」），并用两侧 `var _` 断言补上方法集漂移的检查 |
| gbp-032 | 接收者命名 1-2 字母，同一类型不混用值/指针接收者 | `internal/transport/server.go:57`（`&Server{...}` 值构造 + `func (s *Server)` 9 处）、`internal/cluster/kv_mem.go:12`（`func (k *MemoryKV)` 8 处）、`internal/aio/aio_uring_linux.go:158`（`func (r *uringRing)`）；全仓 `func (this ` / `func (self ` **零命中** | **正例**。与 `architecture/code-style.md` 规则 5 的实测表同结论 |
| gbp-033 | 结构体初始化一律用字段名，不用位置字面量 | `internal/transport/server.go:273`（`device.WriteJob{SegmentID: seg, Off: off, Data: buf[:size], Size: size}`）、`internal/storage/storage.go:121`（`metastore.ObjectMeta{SegmentID: segmentID, Offset: off, Size: size}`）、`internal/aio/aio_linux.go:217`（`Event{Data: evs[i].Data, Res: evs[i].Res}`） | **正例**。非测试代码的位置字面量扫描（`\b[A-Z]\w*\{[^"{}]*,[^"{}]*\}`）无命中；位置字面量只出现在 `_test.go` 的匿名表驱动结构里（如 `internal/layout/layout_test.go:50` 的 `{-1, 0}`），那是规则不管的场合 |
| gbp-034 | 用函数式选项设计灵活的配置 API | `internal/storage/options.go:7`（`type Option func(*options)`）、`:16-17`（`defaultOptions()` 先设默认）、`:23`（`WithAIOMode`）、`:29`（`WithAIOIOPoll`）；同形 `internal/device/options.go:7`、`:26`、`:33` | **正例**，且是规则描述的标准形态：未导出 `options` 结构 + `WithXxx` 变参 + 构造函数里先铺默认值 |
| gbp-035 | 用 defer 做资源释放与解锁，注意 LIFO 与求值时机 | `internal/transport/frame.go:195`（`defer c.wmu.Unlock()`）、`:194`（`c.wmu.Lock()`）；`internal/storage/compact.go:66`（`defer t.Stop()`）；`internal/device/device.go:397`（`defer bufpool.Put(buf) // submit 均阻塞至完成，返回后缓冲即可复用`）；`internal/cluster/kv_mem.go:62`（`defer k.mu.RUnlock()`） | **正例**。`device.go:397` 还带上了「为什么 defer 在这里安全」的解释，比规则要求的更多。注意测试文件里**刻意不用 defer 而用 `t.Cleanup`**（`testing/unit-tests.md` 规则 7），那是另一个主题，不冲突 |
| gbp-037 | 利用零值可用性，让自定义类型的零值有意义 | `internal/transport/server.go:45`（`mu sync.Mutex` 零值直接用）、`internal/storage/options.go:16-17`（`defaultOptions()`）、`internal/cluster/client.go:39`（`if timeout <= 0 { timeout = 5 * time.Second }` —— 零值表示「用默认」）、`internal/transport/batch.go:28`（`// 各字段 ≤0 表示对应流水线关闭`） | **正例**。「零值 = 未设置 → 取默认」与「零值 = 关闭」两种语义在 taihu 都有实例，且都写了注释 |
| gbp-038 | 用类型嵌入做组合复用，但不要在公开 API 里嵌入 | 全仓**无导出结构体嵌入**（扫描 `^type [A-Z]\w* struct \{` 的匿名字段，零命中）；嵌入只出现在未导出/测试场合：`cmd/taihu/cmd/bench_single.go:106`（`singleBenchConfig` 嵌入 `benchkit.Config`）、`pkg/taihu-client/testutil_test.go:15-16`（`errKV` 嵌入 `cluster.KV`） | **正例**。这是 53 条里唯一正文点名上游（`(Uber Style)`）的规则，而 taihu 恰好符合：公开面（`pkg/taihu-client`、`internal/rpcclient` 的导出类型）一个嵌入都没有 |
| gbp-039 | 正确使用空白标识符 `_`（忽略返回值、副作用导入、编译期断言） | 正例（编译期断言）：`internal/cluster/kv_mem.go:16`、`internal/rpcclient/objectstore.go:37`、`internal/transport/protocol/protocol.go:191-192`（8 处 `var _`）；反例（吞错且无注释）：`internal/transport/server_shm_linux.go:427`（`_ = rerr`） | **正例 + 反例**。正例那半（`var _ X = (*Y)(nil)`）是规则点名表扬的用法，taihu 用了 8 次；反例那半正是规则警告的「危险地吞错」，且没有规则要求的注释说明 |
| gbp-041 | 用表驱动测试提升可维护性 | `internal/transport/protocol/protocol_test.go:81`（`cases := []struct{...}` + `:95` `t.Run(tc.name, ...)`）；`internal/layout/layout_test.go:8`（`cases := []struct{...}`）；`internal/layout/layout_test.go:49-58`（纯映射类省掉 `name` 的内联形态，`{-1, 0}` 在 `:50`） | **正例**。与 `testing/unit-tests.md` 规则 4「表驱动是逻辑/纯函数测试的默认形态」一致；全仓 `t.Run(` 53 处、表驱动用例文件 18 个 |
| gbp-042 | 通过接口抽象 + mockgen 做依赖注入与 mock 测试 | 手写 mock 分支：`pkg/taihu-client/testutil_test.go:15-16`（`errKV` 嵌入 `cluster.KV`，只覆盖要打断的 `Scan`/`Get`/…，其余透传）；反例：全仓 `gomock` / `mockgen` **零命中**，无 `//go:generate mockgen` | **适用（走的是规则里的手写简版分支）**。规则同时给了 mockgen 与手写 mock 两条路，taihu 走的是后者且形态正是「嵌入接口 + 按需注入错误」；`testing/unit-tests.md` 规则 7 已把它定成惯例 |
| gbp-043 | 测试辅助函数必须调 `t.Helper()`，且校验逻辑留在测试里 | 正例：全仓 `t.Helper()` 132 处，`internal/transport/transport_shm_test.go:71-79` 的 `shmDial` 是范式（`t.Helper()` 第一句 + `t.Cleanup` 释放）；反例：`test/e2e/f_soak_test.go:223`（`fCPUProfilePath`）、`:230`（`fStartCPUProfile`）两个接收 `*testing.T` 的 helper **没有** `t.Helper()` | **正例为主 + 2 处反例**，与 `triage.md` F-08 的实测（93 个 helper / 2 个缺失）一致。两条反例影响接近零（`fCPUProfilePath` 根本不调 `t.*`；`fStartCPUProfile` 只在 goroutine 里 `t.Logf`），优先度最低 |
| gbp-047 | 基础类型转换用 strconv 而不是 fmt | 正例 `internal/aio/aio_uring_linux.go:406`（`strconv.FormatUint(uint64(e.got), 10)`）、`cmd/taihu/cmd/server.go:101`（`strconv.Itoa(rpcPort)`）；反例 `cmd/taihu/cmd/helpers.go:137`（`fmt.Sprintf("%d B", n)`）、`cmd/taihu/cmd/cluster.go:195`（`fmt.Sprintf("%d/%s", r.CursorSeg, off)`） | **正例 + 反例**。非测试代码 `fmt.Sprintf` 15 处，其中少数确实是基础类型转换（`helpers.go:137` 可换 `strconv.Itoa`），其余是格式化拼接，规则管不到 |
| gbp-048 | 已知大小时预分配 slice 与 map 容量 | slice：`internal/cluster/register.go:52`（`make([]InstanceInfo, 0, len(values))`）、`internal/aio/aio_linux.go:215`（`make([]Event, 0, got)`）、`internal/storage/storage.go:159`；map：`internal/metastore/kv_pebble.go:171`（`make(map[string]int, len(keys))`）、`pkg/taihu-client/registry.go:21`、`pkg/taihu-client/storage.go:170` | **正例**。由源容器推导容量的写法（`len(values)` / `len(keys)` / `len(missed)`）正是规则举的那种，分布很广 |
| gbp-049 | 用 golangci-lint 做综合检查并给出推荐配置 | 反例 `Makefile:21-22`（`check:` 只挂 `check-fmt`/`check-layering`/`check-sdk-only`，静态检查仅 `go vet ./...`）；全仓无 `.golangci.yml`（`find` 零命中） | **反例（未采用）**。建议稿里那些服务向 linter（`noctx` / `bodyclose` / `sqlclosecheck` / `gosec`）对 taihu 不成立，但 errcheck / gosimple / staticcheck / gofmt / revive 这几类在 taihu 是适用的 —— 需要人裁决是否只取这部分 |
| gbp-050 | 用 gofmt + goimports 保持格式一致并分组 import | 正例 `Makefile:25-28`（`check-fmt`：`gofmt -l .` + `grep -v '^third_party/'` 豁免 fork，注释在 `:24`）；三段式分组 `internal/transport/client.go:5-15`，规则文本见 `architecture/code-style.md` 规则 13；反例：`goimports` 全仓**零命中**（Makefile / scripts / spec 都没有） | **正例 + 半个缺口**。gofmt 有门禁；import 分组靠人守（`architecture/code-style.md` 规则 13 收尾自己写着「`make check-fmt` 不会替你发现分组错了」）；`goimports` 这个工具本身没用 |
| gbp-051 | 用 go vet 做静态分析发现潜在 bug | 正例 `Makefile:22`（`check:` 里的 `go vet ./...`）、`Makefile:66`（`check-linux` 里 `GOOS=linux … go vet ./...`） | **正例**。且比上游更严：darwin 与 linux 两个视角各跑一遍 vet，`platform/build-verification.md` 规则 1 记录了这条设计的理由（linux 专有文件在本机不进编译）；配套的 `architecture/code-style.md` 规则 3 还禁止用 `//nolint` 压掉 vet 告警 |
| gbp-052 | 用 staticcheck 做深度静态检查 | 反例：全仓无 `staticcheck.conf`，`Makefile` 与 `scripts/` 里 `staticcheck` 零命中 | **反例（未采用）**。同 gbp-049：工具未接入，但 `architecture/code-style.md:37`「禁止用 `//nolint` 压掉 vet 告警」的立场与 staticcheck 的严格取向一致 |
| gbp-053 | 用 revive 做可配置的 lint，含复杂度/长度上限 | 反例：全仓无 `revive.toml`，`revive` 零命中；仓库**无任何**复杂度/函数长度/参数个数的门禁 | **反例（未采用）**。注：`Makefile` 里无此工具，`code-style.md` 也没有复杂度类上限 —— 这条若并入等于新增一类治理规则，需人裁决 |

**适用小计：32 条。**

---

## 二、冲突（3 条）

| 编号 | 规则一句话 | taihu 现状（file:line，已核实） | 为什么冲突 |
|------|-----------|-------------------------------|-----------|
| gbp-036 | 正确使用 slice 与 map（预分配、拷贝、遍历中删除）；规则正文断言「**边遍历边 `delete` 是未定义行为**，需先收集 key」 | taihu **正面在做**这件事，三处且都是当前 key：`internal/cluster/kv_mem.go:52`（`for key := range k.m { … delete(k.m, key) }`）、`internal/aio/aio_other.go:157`（`for seq, o := range r.inflight { … delete(r.inflight, seq); return true }`）、`internal/device/device.go:161`（`for _, ev := range evs { … delete(d.m, ev.Data) }`） | **上游这条子主张本身是错的**：Go 规范明确允许在 `range` 期间删除（当次已到达的 key 或尚未到达的 key 都合法，只是新增的条目不保证被遍历到）——「未定义行为」不成立。规则整体若要照抄进 spec，会把 `kv_mem.go:52` 这段正确代码判成违规，并误导人去改成「先收集 key 再删」。**须人裁决**：建议只采纳该条的前半（预分配，taihu 已符合，见 gbp-048）并**显式删掉「删除即未定义行为」这句** |
| gbp-044 | 用 `go test -bench` 量化性能，配合 benchstat 对比 | 自有代码 `func Benchmark` **零命中**（`grep -rn 'func Benchmark'` 在 `cmd/ examples/ internal/ pkg/ test/` 全空；16 个全在 `third_party/netpoll` 与 `third_party/shmipc-go`）；性能测量改走 `internal/benchkit/` 自研 harness 与 `test/e2e` 的 F/G 组；`testing/unit-tests.md:66`、`:75` 把这条写成了**明文约定**：「自有代码写性能测量**不在单测层**，在 `internal/benchkit/` 与 e2e 的 F/G 组」 | **与现有 spec 直接矛盾**。`testing/unit-tests.md:75` 已经把「不写 benchmark」定成仓库约定，而 gbp-044 要求把 benchmark 写回单测层。这不是「缺口」而是**两种已成形的方法论之争**：taihu 的选择有理由（`internal/benchkit` 能压满真实 IO、e2e 能覆盖 shm/TCP 两条数据面，`go test -bench` 做不到）。**须人裁决**：若维持 taihu，应落成 `testing/unit-tests.md` 的「刻意偏离上游规则」条目，而不是新增一条自相矛盾的规则 |
| gbp-046 | 用 testify 让断言更清晰，区分 assert 与 require | 自有代码 testify **零引用**（`cmd/ examples/ internal/ pkg/ test/` grep 命中 0）；`testing/unit-tests.md` 规则 2 标题就是「**不用 testify，断言手写**」，并写明失败消息惯例 `"<主体>(<入参>)=<got> want <want>"`（实例如 `internal/layout/layout_test.go:21-26`）；`go.mod:12` 里的 `stretchr/testify v1.9.0` 只是为了 `third_party/shmipc-go` 的 13 个上游 fork 测试（它们随 `go test ./...` 一起跑） | **与现有 spec 直接矛盾，且冲突面很大**。规则要求引入的断言库，正是 `testing/unit-tests.md` 规则 2 明文拒绝的。附带风险：照抄会让人误以为「`go.mod` 里有 testify 所以仓库在用」—— 规则 2 特意提醒「不要看 `go.mod`，要看代码」。**须人裁决**：建议维持 taihu，把这条按 `design.md` §3.3 落成「刻意偏离」并给出可检验的理由（手写断言保住了标准库 `testing` 一手到底 + 无第三方断言语义） |

**冲突小计：3 条。**

---

## 三、不适用（18 条）

| 编号 | 规则一句话 | 为什么不适用 |
|------|-----------|--------------|
| gbp-001 | 小到中型项目、简单 REST API 应选 Gin | 整条讲 HTTP Web 框架选型。taihu 无 HTTP 服务端；数据面是自实现的二进制帧协议（`internal/transport/protocol`），控制面走 TiKV。规则假设的项目形态不具备 |
| gbp-002 | 复杂微服务应选 Go-Kratos | Kratos 提供的是 gRPC/HTTP 双协议、服务发现、配置中心。taihu 无 gRPC、无 RPC 框架、无配置中心（配置走 cobra flag） |
| gbp-003 | 日志/鉴权/限流抽成 middleware | 正反例全建立在 Gin 的 `gin.Context` / `c.GetHeader("Authorization")` / JWT 上。taihu 无 HTTP handler 栈、无鉴权层；跨层关注点在 taihu 的形态是传输层的帧读写（`internal/transport/frame.go`）与日志三通道（`architecture/code-style.md` 规则 10-12），不是 middleware |
| gbp-004 | 服务必须支持优雅停机，等在途请求处理完 | 规则的载体是 `http.Server` + `signal.Notify` + `srv.Shutdown(ctx)`（面向 HTTP 在途请求）。taihu 无 HTTP 服务端。**补记**：taihu 有对应物 —— `cmd/taihu/cmd/server.go:256`（`sig := make(chan os.Signal, 1)`）与 `internal/transport/server.go` 的 `GracefulStop()`，但那条线索的价值在超时是否可配，已归 `triage.md` A-02 / `conflicts.md` C-03，不随本条并入 |
| gbp-005 | GORM 初始化必须配 logger + 连接池 + PrepareStmt | 存储引擎**本身**不是 ORM 的使用方。taihu 的持久化是自研的 `internal/layout` + `internal/metastore`（Pebble）+ `internal/storage`，磁盘格式自己定，没有 GORM 可初始化 |
| gbp-006 | GORM Hook 只放简单自动化逻辑 | 同上：无 GORM，无 `BeforeCreate` / `AfterFind` 生命周期 |
| gbp-007 | 多次写操作必须包在 `db.Transaction` 里 | 无 SQL、无 ORM 事务。taihu 的原子性靠 Pebble 的 `Batch`（`internal/metastore/kv_pebble.go`）与 `BatchPutCommit`，语义与 `db.Transaction` 不是一回事 |
| gbp-008 | 用 Goose 做受版本控制的 schema 迁移 | 无关系型 schema，也无迁移工具。要求存储引擎用 Goose 做版本化迁移，是**把被实现物当成依赖** —— 磁盘格式的版本兼容问题 taihu 有自己的机制（虽然当前有缺陷，见 `triage.md` F-01），不由 Goose 负责 |
| gbp-009 | 生产环境必须显式配置连接池四个参数 | 无数据库连接池。taihu 里形态相近的是 `bufpool`（`internal/bufpool/bufpool.go`），但那是 4K 对齐的内存桶、且**刻意否决了 `sync.Pool`**（`bufpool.go:10-13` 有实测依据），与 SQL 连接池的 `SetMaxOpenConns` 一族没有对应关系 |
| gbp-010 | 按 `cmd/ internal/{domain,application,infrastructure,interfaces} pkg/` 分层 | taihu 的分层是 `internal/{aio,bufpool,device,layout,metastore,storage,transport,rpcclient,cluster,ierr,benchkit,version}` —— 按**资源与机制**分层，不是按 DDD 业务域分层。改成 domain/application/infrastructure/interfaces 与既有的 `architecture/layering.md` 以及 `Makefile` 的 `check-layering` / `check-sdk-only` 门禁直接冲突（`Makefile:45`、`:52`） |
| gbp-011 | 领域层放纯业务逻辑，不依赖外部框架 | 以 `User` 实体 / `Email` 值对象 / `Repository` 接口为例的业务领域建模。taihu 没有业务域模型（没有 User 这类实体），它的「领域」是段/对象/映射，且必须贴着设备与内核（`internal/device`、`internal/aio`），「不依赖外部」在这里是不成立的约束 |
| gbp-012 | 应用层编排用例，按 CQRS 分离 Command 与 Query | 无 CQRS、无用例 Handler。taihu 的编排面是 RPC 服务端（`internal/transport/server.go` 的 handlePut/handleGet/…），与 Command/Query 分离是两套建模 |
| gbp-013 | 基础设施层实现领域接口，并做 model ↔ entity 转换 | 以 GORM 实现 `UserRepository` + `toDomain`/`toModel` 互转为例。taihu 无 ORM、无 entity/model 双层映射 |
| gbp-014 | 接口层把外部请求转成应用层的 command/query | 以 Gin handler + DTO + `ShouldBindJSON` + router 注册为例。taihu 无 HTTP 接口层；外部请求的入口是二进制帧解析（`internal/transport/protocol`） |
| gbp-015 | 用依赖注入解耦，推荐 Google Wire | 无 Wire、无 `wire.Build`（全仓 `wire` 零命中）。taihu 的装配是显式构造函数参数（如 `transport.NewServer(storage)`）+ 少量 `Option` 变参（`internal/storage/options.go`），这是刻意的：装配点少且都在 CLI 层，代码生成得不偿失 |
| gbp-020 | 定义统一的 API 错误响应结构体与错误码 | 无 REST 接口、无 JSON 错误响应体。**补记**：taihu 有更强的对应物 —— 跨进程边界**传 code 不传字符串**，`ErrCode` 枚举 + `MapStorageErr` / `MapCode` 在 `internal/transport/protocol/protocol.go:99-153`（`errors.Is` 分派在 `:115-121`），并有「服务端不该 `MapCode`、客户端不该 `MapStorageErr`」的用法边界（`architecture/error-model.md` §4）。规则的具体形态（HTTP 状态码映射）不适用 |
| gbp-040 | 生产代码目标 99% 测试覆盖率 + 按分层设目标 | 「99% 硬指标」不适合按层设，taihu 也不按百分比管理（现状 20,139 行测试 / 13,254 行非测试，比例高但不是门禁）。其分层目标表（domain 100% / application 99% / infra 95% / interface 90%）直接建立在 DDD 分层上，而那一层不适用（见 gbp-010） |
| gbp-045 | 用 build tag + testcontainers 写集成测试 | 正例是 `mysql.RunContainer` 起真实 MySQL 容器 + `httptest.NewServer` 打 HTTP API。taihu 无 MySQL、无 HTTP。**注意 build tag 那半 taihu 是有的且做得更好**（`//go:build e2e` 8 个文件、`//go:build linux` 10 个），但那属于 gbp-041 之外的另一个主题，不靠本条并入 |

**不适用小计：18 条。**

---

## 附：自检

### 三类条数合计

| 类别 | 条数 | 编号 |
|------|------|------|
| 适用 | **32** | 016,017,018,019,021,022,023,024,025,026,027,028,029,030,031,032,033,034,035,037,038,039,041,042,043,047,048,049,050,051,052,053 |
| 冲突 | **3** | 036,044,046 |
| 不适用 | **18** | 001,002,003,004,005,006,007,008,009,010,011,012,013,014,015,020,040,045 |
| **合计** | **53** | 32 + 3 + 18 = 53 ✅ |

一级类别归属核对（同 `golang-base-practices.md` §4 的分类表）：

- Framework Selection 4 条 → 全部不适用（001-004）
- Database & ORM 5 条 → 全部不适用（005-009）
- DDD 6 条 → 全部不适用（010-015）
- Error Handling 6 条 → 适用 5（016-019, 021）+ 不适用 1（020）
- Concurrency 7 条 → 适用 7（022-028）
- Idiomatic 11 条 → 适用 10（029-035, 037-039）+ 冲突 1（036）
- Testing 7 条 → 适用 3（041-043）+ 冲突 2（044, 046）+ 不适用 2（040, 045）
- Performance 2 条 → 适用 2（047, 048）
- Lint 5 条 → 适用 5（049-053）

计：4+5+6+(5+1)+(7)+(10+1)+(3+2+2)+2+5 = 4+5+6+6+7+11+7+2+5 = **53** ✅

### 实际跑过的验证命令

```bash
# 1. 判据类（一次性确认整类事实）
grep -rn 'func Benchmark' --include='*.go' . | grep -v third_party          # 空 → gbp-044
grep -rn 'errgroup' --include='*.go' . | grep -v third_party               # 空 → gbp-026
grep -rn 'gomock\|mockgen' --include='*.go' . | grep -v third_party        # 空 → gbp-042
grep -rn 'stretchr/testify' --include='*.go' . | grep -v third_party       # 空 → gbp-046
grep -rn 'panic(' --include='*.go' internal/ pkg/ cmd/ | grep -v _test.go  # 空 → gbp-021
grep -rn 'func (this \|func (self ' --include='*.go' . | grep -v third_party  # 空 → gbp-032
grep -rn 'goimports' Makefile scripts/ .trellis/spec/                      # 空 → gbp-050
grep -rn '\-race' Makefile                                                  # 空 → gbp-028
find . -maxdepth 2 -name '.golangci*' -o -name 'staticcheck.conf' -o -name 'revive.toml'  # 空 → gbp-049/052/053
ls -a | grep -iE 'gitlab-ci|jenkins|travis|circle|drone|\.github'          # 空 → gbp-028
grep -c 't.Helper()' -r --include='*_test.go' .                            # 合计 132 → gbp-043

# 2. 逐条锚点核实（每条都单独打印过，行内容与规则陈述逐字对上）
sed -n '83p'   internal/transport/server.go            # ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
sed -n '31,32p' internal/transport/frame.go            # streamInCap 的注释 + const streamInCap = 8
sed -n '427p'  internal/transport/server_shm_linux.go  # _ = rerr
sed -n '399p'  internal/aio/aio_uring_linux.go         # type uringParamError struct {
sed -n '11p'   internal/ierr/ierr.go                   # ErrNotFound = errors.New("taihu: key not found")
sed -n '115p'  internal/transport/protocol/protocol.go # case errors.Is(err, ierr.ErrNotFound):
sed -n '52p'   internal/cluster/kv_mem.go              # delete(k.m, key)  ← 在 range 内
sed -n '223p;230p' test/e2e/f_soak_test.go             # fCPUProfilePath / fStartCPUProfile（无 t.Helper）
sed -n '15,16p' pkg/taihu-client/testutil_test.go      # type errKV struct { cluster.KV
sed -n '39p'   pkg/taihu-client/index.go               # ch: make(chan indexItem, 4096)
sed -n '106p'  internal/cluster/kv_tikv.go             # fmt.Errorf("tikv txnkv connect: %w", err)
sed -n '50p'   internal/cluster/register.go            # return nil, err
sed -n '39p'   internal/cluster/client.go              # timeout = 5 * time.Second（零值兜底）
sed -n '7p;23p' internal/storage/options.go            # type Option func(*options) / WithAIOMode
sed -n '53,66p' internal/storage/compact.go            # wg.Add → go run → close(stop) → wg.Wait
sed -n '171p'  internal/metastore/kv_pebble.go         # make(map[string]int, len(keys))
sed -n '137p'  cmd/taihu/cmd/helpers.go                # fmt.Sprintf("%d B", n)
sed -n '406p'  internal/aio/aio_uring_linux.go         # strconv.FormatUint(uint64(e.got), 10)
sed -n '21,28p' Makefile                               # check: 目标 + go vet ./... (:22) + check-fmt (:25-28, gofmt -l .)
# （其余锚点同法逐条打印，不再全部罗列）

# 3. 与既有 spec 的一致性核对（用于判「冲突」）
cat .trellis/spec/testing/unit-tests.md        # 规则 2「不用 testify」、规则 3 的 Benchmark 命中表
cat .trellis/spec/architecture/code-style.md   # 规则 4 panic、5 receiver、6 缩写、8 错误包装、13 import 分组
cat .trellis/spec/architecture/error-model.md  # §2 自定义 error 类型只有一处且不导出
cat .trellis/spec/engine/buffer-and-concurrency.md  # §二 bufpool 否决 sync.Pool
```

### 我拿不准的条目

1. **gbp-026（errgroup）** —— 我判「适用（反例）」，但这条是**边界情形**。理由：`design.md` §3.2 的推论说「找不到任何锚点的规则进不适用附录」，而 errgroup 在 taihu 的确零使用；可我找到的锚点是**反向的** —— `internal/benchkit/run.go:168` 与 `pkg/taihu-client/storage.go:221` 都是「手写 `WaitGroup` + 错误 channel」的 fan-out，即 taihu 用另一种方式在做同一件事。**若审阅者认为「锚点必须是正例或真正的反例（而不是『没用某库』）」，这条应改判不适用。** 我倾向保留在适用侧，因为「有没有锚点」和「用不用库」是两回事，且 fan-out 场景项目形态上确实具备。
2. **gbp-031（接口由消费方定义）** —— 我判「适用」，因为规则的主干（小接口 + 编译期断言）在 taihu 是硬正例；但子主张「接口应由消费方定义」被 taihu 明确否决，且理由写在代码注释里（`internal/rpcclient/objectstore.go:13`：反向声明会成环）。**若审阅者认为一处明确偏离就应升级为冲突**（以便落进「刻意偏离」清单），这条应改判冲突。我没有升级，是因为 taihu 已经用两侧 `var _` 断言补上了该主张想防的问题，偏离已被自洽处理。
3. **gbp-049 / gbp-052 / gbp-053（lint 工具链三条）** —— 我判「适用（反例）」。这三条的共同问题是：taihu 的「反例」是**未采用某工具**，而不是做错了什么。`triage.md` §6.3 也把 049/052 列为「混合」而非不适用，所以我不放进 18 条不适用里。但三条要否并入 spec（尤其是 gbp-053 的复杂度/长度上限，等于新增一类治理规则）是**纯工具链决策**，须人裁决。附带一条事实供裁决参考：`Makefile` 里既没有这三个工具，`architecture/code-style.md:37` 也已经站在「不靠 nolint 压告警」这一侧。
4. **gbp-024（channel 缓冲）里的 `index.go:39`** —— 我判「无论证」是因为**代码里没有**，但 4096 与 `indexBatchMax = 128`（`pkg/taihu-client/index.go:14`）同处一个文件，我无法排除作者是「按经验取的量级」而非疏漏。若作者能补一句理由，这条的反例可撤。
5. **gbp-018（自定义错误类型）** —— 判「适用」而非冲突，依据是规则文本只说「定义自定义错误类型」没说必须导出，而 `architecture/error-model.md:50` 禁的是**导出**的那类。若审阅者按上游给的三个导出范例（`ValidationError` / `NotFoundError` / `BusinessError`）理解该规则，则 taihu 的「只允许未导出诊断类型」构成偏离，应升级为冲突。我按规则文本判，未升级。
6. **gbp-047（strconv vs fmt）的 `helpers.go:137`** —— `fmt.Sprintf("%d B", n)` 严格说是「数字 + 单位」的格式化，不是纯类型转换；把它列为反例有一点严苛。撤掉它这条仍然是「正例为主」。
