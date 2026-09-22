# uber-go-style.md 逐条三分类

> 来源：研究/uber-go-style.md（134 条）
> 分类：适用 122 条 / 冲突 4 条 / 不适用 8 条，合计 = 134

分类口径（本轮判定统一按此执行，便于复核）：

- **适用** —— 规则对 taihu 这一项目形态成立，且能在代码里指出一个真实锚点（正例或反例）。
- **冲突** —— 规则与 taihu 现有做法矛盾：taihu 有意不遵守（有注释/设计声明支撑），或遵守就得改代码语义/引入规则指定的依赖。
- **不适用** —— 规则假设的项目形态 taihu 不具备，**或**在 taihu 里既找不到正例也找不到反例（无锚点不硬凑）。

**项目形态速览（判定依据）**：分布式存储引擎；Go 1.25.0；分层 `internal/{aio,bufpool,device,layout,metastore,storage,transport,rpcclient,cluster,ierr,benchkit,version}` + `pkg/taihu-client`(SDK) + `cmd/taihu`(CLI, cobra) + `test/e2e`；`third_party/` 为 112 个上游 fork（免于 gofmt）。无 HTTP 服务端、无 gRPC、无数据库/ORM、无 RPC 框架（自研二进制协议）；有 Linux 平台分裂（`_linux.go` / `_other.go`）与 io_uring/libaio 系统调用；零外部依赖是 `internal/aio` 的刻意设计目标（`internal/aio/aio.go:1` 包文档）。

---

## 一、适用（122 条）

| 编号 | 规则一句话 | taihu 锚点（file:line，已核实） | 锚点是正例还是反例 |
|------|-----------|-------------------------------|-------------------|
| uber-001 | 接口按值传递，几乎不需要指向接口的指针 | internal/benchkit/run.go:83 `func Run(ctx context.Context, s Store, ...)`；grep 未见 `*Iface` 形参 | 正例 |
| uber-002 | 需改底层数据时实现侧必须用指针接收者 | internal/cluster/kv_mem.go:23 `func (k *MemoryKV) Put(...)`，:25 就地改 `k.m` | 正例 |
| uber-003 | 编译期接口校验，右侧写被断言类型的零值 | internal/cluster/kv_mem.go:16 `var _ KV = (*MemoryKV)(nil)`（指针 nil）；cmd/taihu/cmd/cli_core_test.go:48 `var _ cluster.KV = cliTestErrKV{}`（结构体空结构体）；internal/transport/protocol/protocol.go:191 `var _ ByteReader = netpoll.Reader(nil)`（接口 nil） | 正例 |
| uber-004 | 需校验的场景：API 契约 / 同接口的实现集合 / 违反会破坏使用者 | internal/cluster/kv_mem.go:16 与 internal/cluster/kv_tikv.go:30（`KV` 的实现集合）；pkg/taihu-client/storage.go:49（跨包契约）；internal/rpcclient/objectstore.go:15 注释说明该断言的意图 | 正例 |
| uber-005 | 值接收者方法在值和指针上都能调；指针接收者只能在可寻址值上调 | internal/cluster/kv_tikv.go:22 `func (t TLSConfig) enabled() bool`，调用点 internal/cluster/kv_tikv.go:99 `tls.enabled()`（值直接调值接收者方法） | 正例 |
| uber-006 | 只有指针接收者方法时值无法满足接口 | cmd/taihu/cmd/cli_core_test.go:48 `var _ cluster.KV = cliTestErrKV{}`，其方法全为值接收者（:33 `func (k cliTestErrKV) Put`）；对照 internal/cluster/kv_mem.go:16 必须写 `(*MemoryKV)` | 正例 |
| uber-007 | mutex 零值即有效，几乎不需要指向 mutex 的指针 | internal/cluster/kv_mem.go:12 `mu sync.RWMutex`（构造器 :20 不初始化它）；`new(sync.Mutex)` 全仓零命中 | 正例 |
| uber-008 | mutex 应是非指针命名字段；不要内嵌 | internal/transport/server.go:45 `mu sync.Mutex`；internal/transport/frame.go:80 `wmu sync.Mutex`；内嵌字段扫描无 mutex | 正例 |
| uber-009 | 接收 slice/map 并保留引用时必须复制 | internal/cluster/kv_mem.go:25 `k.m[string(key)] = append([]byte(nil), value...)`；internal/cluster/kv_tikv.go:203-204 同型 | 正例 |
| uber-010 | 返回内部 map/slice 必须返回副本 | internal/cluster/kv_mem.go:30 `Get` 在 :31-33 锁内取值后 :37 `return append([]byte(nil), v...), nil`；internal/cluster/kv_mem.go:97 `out[i] = append([]byte(nil), v...)` | 正例 |
| uber-011 | 用 defer 清理资源 | internal/metastore/segments.go:266 `defer m.mu.Unlock()`；internal/metastore/kv_pebble.go:193 `defer it.Close()`；internal/device/info_linux.go:19 `defer f.Close()` | 正例 |
| uber-012 | defer 开销极小，只在纳秒级函数里才该避免 | internal/device/device.go:397 `defer bufpool.Put(buf)` —— 连 IO 提交路径也用 defer，优先可读性 | 正例 |
| uber-013 | channel 通常为 1 或无缓冲，其他尺寸需高度审视 | 反例：internal/transport/batch.go:91 `make(chan *writeTask, batchCap*2)`；pkg/taihu-client/index.go:39 `make(chan indexItem, 4096)`。正例：internal/storage/compact.go:48 `make(chan struct{})`、internal/aio/aio_other.go:37 `make(chan struct{}, 1)` | 反例（含正例对照） |
| uber-014 | 枚举通常从非零值开始（iota+1） | internal/aio/aio.go:104 `ModeAuto Mode = iota`（=0）；internal/metastore/meta.go:77 `SegmentStateFree SegmentState = iota`（=0）；internal/transport/frame.go:65 `deliverOK deliverResult = iota`（=0）—— 三处枚举全部从 0 开始 | 反例 |
| uber-015 | 零值即期望默认行为时枚举可从 0 开始 | internal/aio/aio.go:104 `ModeAuto`(=0，注释「自动探测」就是期望默认)；internal/metastore/meta.go:77 `SegmentStateFree`(=0，零值 segment 元数据即空闲)；internal/transport/frame.go:65 `deliverOK`(=0，成功是默认) | 正例 |
| uber-016 | 处理时间一律用 `"time"` 包，不自行假设 24h/60min/365d | internal/storage/compact.go:29 `Interval: time.Minute` + :65 `time.NewTicker(c.cfg.Interval)`；grep 未见手写 `86400` 之类常量 | 正例 |
| uber-017 | 时间点用 `time.Time`，比较/加减用其方法 | 正例：internal/cluster/instance.go:39 `Aliveness(now time.Time, timeout time.Duration) bool`。反例：internal/cluster/capacity.go:18 `UpdateTime int64` | 正例（附反例） |
| uber-018 | 时间段用 `time.Duration` | internal/storage/compact.go:20 `Interval time.Duration`；pkg/taihu-client/config.go:49 `RefreshInterval time.Duration` | 正例 |
| uber-019 | 与外部系统交互尽量用 `time.Duration`/`time.Time` | cmd/taihu/cmd/root.go:91 `pf.DurationVar(&global.timeout, "timeout", 5*time.Second, ...)` —— CLI flag 直接用 Duration | 正例 |
| uber-020 | 无法用时用 int/float64，单位写进字段名 | internal/cluster/capacity.go:18 `UpdateTime int64 \`json:"update_time"\` // 记录写入时间（unix 秒）` —— 单位只在注释里，字段名没有；internal/cluster/instance.go:18 `StartTime int64` 同型 | 反例 |
| uber-021 | 无法用 `time.Time` 时按 RFC 3339 用 string | internal/rpcclient/admin.go:16 `Ping(...) (rtt time.Duration, serverTime int64, err error)` —— 时间点以 int64 unix 秒过线（且是协议契约）；internal/cluster/instance.go:19 `LastHeartbeat int64` | 反例 |
| uber-022 | 错误声明方式决策表（静态/动态 × 需匹配/不需匹配） | 不需匹配+静态：internal/aio/aio.go:38 `ErrFull = errors.New(...)`；需匹配+静态：internal/ierr/ierr.go:11 `ErrNotFound = errors.New(...)`；需匹配+动态：internal/aio/aio_uring_linux.go:399 `type uringParamError struct`（携带 field/got）；不需匹配+动态：cmd/taihu/cmd/server.go:320 `fmt.Errorf("no free port on %v in [%d,%d]", ...)` | 正例 |
| uber-023 | 导出错误变量/类型会成为包公开 API，需慎重 | internal/ierr/ierr.go:1-4 包文档把 ierr 立为「唯一事实源」，并限定 re-export 只有一处；internal/rpcclient/reexport.go:28-30 的 var 块只导出客户端可见的 5 个，注释明写「不含 ierr.ErrConflict：那是 compaction 内部的 CAS 控制信号」 | 正例 |
| uber-024 | 传播错误三选一：原样 / `%w` 加上下文 / `%v` 加上下文 | internal/cluster/kv_tikv.go:106 `fmt.Errorf("tikv txnkv connect: %w", err)`（%w 加上下文）；internal/cluster/register.go:14 `return err`（原样）。全仓 `%w` 38 处 | 正例 |
| uber-025 | 没有额外上下文可加时原样返回 | internal/cluster/register.go:14/#50/#67/#71、internal/cluster/client.go:48/#83/#100/#104、internal/cluster/capacity.go:43 均为 `return err` / `return nil, err` | 正例 |
| uber-026 | 调用方需访问底层错误时用 `%w`，并文档化+测试 | internal/transport/protocol/protocol.go:115 `errors.Is(err, ierr.ErrNotFound)`（消费者依赖可匹配性）；internal/ierr/ierr_test.go:43 专测「包装后仍可被 errors.Is 命中」、:51 测「包装后不误命中其他错误」 | 正例 |
| uber-028 | 错误上下文保持简洁，避免 "failed to" 堆叠 | internal/cluster/kv_tikv.go:106 `"tikv txnkv connect: %w"`；`grep -rn 'failed to' internal pkg cmd` 零命中 | 正例 |
| uber-029 | 送往其他系统的错误应一眼看出是错误 | internal/storage/compact.go:72 `log.Printf("compaction: moved=%d err=%v", moved, err)`（`err=` 标签）；cmd/taihu/cmd/server.go:183 `log.Printf("GetDiskCapacity: %v", err)` | 正例 |
| uber-030 | 全局错误值用 `Err`/`err` 前缀 | internal/ierr/ierr.go:11-22 `ErrNotFound`…`ErrConflict`；internal/transport/frame.go:34 `errConnClosed` | 正例 |
| uber-031 | 自定义错误类型用 `Error` 后缀 | internal/aio/aio_uring_linux.go:399 `type uringParamError struct`，:404 `func (e *uringParamError) Error() string`。全仓唯一自定义 error 类型 | 正例 |
| uber-032 | 每个错误只处理一次；不要既 log 又 return | `internal/storage/compact.go:72` 后台 compaction 出错只 log 不 return（循环继续=优雅降级）；脚本扫描「log 语句后紧跟 `return ... err`」零命中 | 正例 |
| uber-033 | 错误处理方式：errors.Is/As 分支 / log 并降级 / 返回定义良好的错误 / 包装返回 | internal/transport/protocol/protocol.go:115-121 `errors.Is` 分支映射到错误码；internal/aio/aio_other.go:133 `errors.As(err, &errno)` 取 errno；internal/storage/compact.go:72 降级；internal/cluster/kv_tikv.go:106 包装 | 正例 |
| uber-034 | 类型断言一律用 comma ok | 反例：internal/metastore/cache.go:55 `return e.Value.(*cacheEntry).meta, true`，:66 `e.Value.(*cacheEntry).meta = meta`，:96 `entry := back.Value.(*cacheEntry)`；pkg/taihu-client/route_cache.go:51/#62/#72；cmd/taihu/cmd/server.go:334 `ln.(*net.TCPListener)`。正例：internal/transport/client.go:183 `if rc, ok := msg.r.(interface{ ReadCopy([]byte) (int, error) }); ok` | 反例 |
| uber-035 | 生产代码必须避免 panic | `grep -rn 'panic(' internal pkg cmd` 去除 `_test.go` 后零命中；平台不支持时返回 error（internal/aio/probe_other.go:18） | 正例 |
| uber-036 | panic/recover 不是错误处理策略，只在不可恢复时 panic；初始化失败可 panic | internal/aio/aio.go:121 `return ModeAuto, fmt.Errorf("aio: invalid io-uring mode %q (want auto|on|off)", s)`（非法配置走 error 而非 panic）；internal/aio/probe_other.go:18 平台不支持也返回 error；`panic(` 生产代码零命中 | 正例 |
| uber-037 | 测试中也优先 t.Fatal/t.FailNow 而非 panic | 反例：internal/transport/protocol_test.go:390 `panic("unknown parser " + name)`；internal/storage/storage_test.go:26 `panic("alignedPayload requires 4K multiple")`。正例：全仓 t.Fatal/t.Fatalf 1981 处 | 反例 |
| uber-040 | 避免在公开结构体内嵌类型 | 生产代码仅 2 处内嵌且都在**未导出** struct（cmd/taihu/cmd/bench_single.go:106）；internal/rpcclient/objectstore.go:10-25 注释明确说明公开接口只依赖标准库类型以便外部实现 | 正例 |
| uber-041 | 不要用 Go 预声明标识符做名字 | internal/cluster/kv_tikv.go:8 `tikverr "github.com/tikv/client-go/v2/error"` —— 上游包名就叫 `error`，taihu 用别名避免遮蔽内置 `error`；grep 未见任何遮蔽内置名的标识符/字段名 | 正例 |
| uber-042 | 尽量避免 init()；不可避免时须确定、不依赖顺序、不操纵全局、不做 I/O | 反例：internal/transport/frame.go:25 `func init()` 调 `netpoll.SetAlignedAllocator` / `SetInputAlignedAllocator` / `SetInputNodeSize`（:26-28），**操纵了另一个包的全局状态并改变其分配行为**——不满足「不访问/操纵全局状态」 | 反例 |
| uber-043 | 不满足上述要求的代码应作为 main() 的辅助函数 | 反例：cmd/taihu/cmd/root.go:20 `func init() { log.SetLevel(zapcore.ErrorLevel) }`（:21 操纵全局日志级别）；cmd/taihu/cmd/root.go:84 的 init 向包级 rootCmd 注册 flag | 反例 |
| uber-044 | init() 可取的场景：复杂表达式、可插拔钩子/注册表、确定性预计算 | 正例：cmd/taihu/cmd/root.go:84 init 用 `rootCmd.PersistentFlags()` 做参数注册（与 `database/sql` dialect 注册同类）；internal/transport/frame.go:25 的分配器注册也属「可插拔钩子」形态 | 正例 |
| uber-045 | 只在 main() 里调 os.Exit 或 log.Fatal* | 反例：cmd/taihu/cmd/helpers.go:122 `os.Exit(1)` 位于 `printJSON`（普通函数，非 main）；examples/taihu-client/main.go:121 `log.Fatal(err)` | 反例 |
| uber-046 | 至多调用一次 os.Exit/log.Fatal | 正例：cmd/taihu/main.go:27 `os.Exit(run(os.Args[1:]))` 单次调用，业务逻辑收在可测试的 run()（:15）里——正是原文推荐的 `os.Exit(run(args))` 变体；但 cmd/taihu/cmd/helpers.go:122 是第二处 os.Exit | 正例（附反例） |
| uber-047 | 会被序列化的结构体字段都应打 tag | internal/cluster/instance.go:9-19 全部字段带 `json:` tag；internal/cluster/client.go:15-23、internal/cluster/capacity.go:14-18 同型；`yaml:` grep 零命中（无 yaml 序列化） | 正例 |
| uber-048 | 不要 fire-and-forget goroutine | pkg/taihu-client/index.go:101 `stop()`：`select` 判重后 `close(m.stopCh)` 并 `<-m.done` 等待退出；index.go:46 `start()`/`loop()`(:50 起) 配套 | 正例 |
| uber-049 | 每个 goroutine 要么有可预测停止时间，要么有停止信号，且须能阻塞等待其结束 | internal/storage/compact.go:48 构造 `stop: make(chan struct{})`，:58 `Stop()` 内 :59 `close(c.stop)` + :60 `c.wg.Wait()` | 正例 |
| uber-050 | 用 go.uber.org/goleak 做 goroutine 泄漏测试 | 反例：全仓 `goleak` 零命中；有真实后台 goroutine 的包均无泄漏检测 —— internal/storage/compact.go:53 `c.wg.Add(1)`、internal/metastore/segments.go:137 `m.wg.Add(1)`、pkg/taihu-client/index.go:47 `go m.loop()` | 反例 |
| uber-051 | 等多个 goroutine 用 sync.WaitGroup + wg.Wait() | internal/transport/server_shm_linux.go:46 `wg sync.WaitGroup`，:241/:259 `s.wg.Add(1)`，:224 `s.wg.Wait()`；cmd/taihu/cmd/server.go:270 `var serveWG sync.WaitGroup` | 正例 |
| uber-052 | 只等一个 goroutine 用 chan struct{} + close(done) + <-done | internal/metastore/segments.go:155 `close(m.stop)` + :156 `m.wg.Wait()`；pkg/taihu-client/index.go:101-110 `close(m.stopCh)` + `<-m.done`；internal/transport/frame.go:59 `close(st.done)` | 正例 |
| uber-053 | init() 中不应启动 goroutine | internal/transport/frame.go:25（全仓唯一非 cmd 的 init）与 cmd/taihu/cmd/*.go 的 11 个 init 内均无 `go` 语句 | 正例 |
| uber-054 | 包内后台 goroutine 必须由某对象管理并提供 Close/Stop/Shutdown | internal/storage/compact.go:48 `NewCompactor(...)` 返回 `*Compactor`，:58 `Stop()`；pkg/taihu-client/index.go:46 `start()` / :101 `stop()`；internal/metastore/segments.go:154 `stopGC()` | 正例 |
| uber-055 | worker 管理多个 goroutine 时应使用 WaitGroup | internal/transport/server_shm_linux.go:46 字段 wg，:241/:259 每个 worker 前 Add(1)，:224 统一 Wait | 正例 |
| uber-056 | 性能指引只适用于热路径 | internal/aio/aio_linux.go:142-143 提交路径预分配 `cbs`/`ptrs` 数组（热路径专门优化）；internal/transport/protocol/protocol.go:150 手工拼帧；而 CLI 输出层（cmd/taihu/cmd/helpers.go:131-137）用 fmt.Sprintf 不做优化 | 正例 |
| uber-057 | 基本类型与字符串互转优先 strconv 而非 fmt | 反例：cmd/taihu/cmd/bench_storage.go:237-238 `keyFor` 用 `fmt.Sprintf("%s/%d", c.prefix, seq)`，而它在逐 op 热路径上（:256 每次操作都调）；internal/benchkit/run.go:66 `KeyFor` 同型（:125/:178 在压测循环内）。正例：cmd/taihu/cmd/server.go:101 `strconv.Itoa(rpcPort)` | 反例 |
| uber-058 | 不要对固定字符串反复做 string→[]byte 转换 | 反例：同一常量 `kvPrefixMapping` 被重复转换 —— internal/metastore/kv_pebble.go:188 `prefix := []byte(kvPrefixMapping)`、:314 同句，internal/metastore/segments.go:385 同句；常量定义在 internal/metastore/kv_pebble.go:26 | 反例 |
| uber-059 | 尽量指定容器容量 | internal/transport/protocol/protocol.go:417 `make([]byte, 4+len(entries)*SegItemLen)`；internal/transport/batch.go:120 `make([]device.WriteJob, 0, total)`；internal/transport/server_admin.go:75 `make([]protocol.SegmentEntry, 0, len(entries))` | 正例 |
| uber-060 | make(map) 给容量提示 | internal/metastore/kv_pebble.go:171 `make(map[string]int, len(keys))`；:194 `make(map[string]ObjectMeta, len(missed))`；pkg/taihu-client/registry.go:21 | 正例 |
| uber-061 | make([]T, len, cap) 给容量 | internal/cluster/kv_mem.go:63 `make([]string, 0, len(k.m))`；internal/aio/aio_linux.go:185 `make([]ioEvent, 0, max)`；internal/rpcclient/dial.go:38 | 正例 |
| uber-062 | 软限制 99 字符，允许超限 | 反例（软化）：非测试代码 >99 字符共 833 行，最长 cmd/taihu/cmd/bench_cluster.go:151（191 字符）；属原文明确允许的「allowed to exceed」 | 反例 |
| uber-063 | 最重要的是保持一致 | internal/ierr/ierr.go:1-4 把「错误唯一事实源、re-export 只有一处」立成全仓约定；错误消息统一 `taihu: ` 前缀（internal/ierr/ierr.go:11-22）；文档注释统一中文 | 正例 |
| uber-065 | 相似的声明应分组 | 正例：internal/ierr/ierr.go:9 `var (...)` 一组 6 个错误；internal/rpcclient/reexport.go:31 var 块；internal/cluster/kv.go:24 4 个 key 前缀常量成组。反例：internal/transport/protocol/protocol.go:30-63 把 8 个同类协议常量写成 8 条独立 `const X = ...` | 正例（附反例） |
| uber-066 | 不要分组无关声明 | internal/bufpool/bufpool.go:23-27 的 const 块只含缓冲池尺寸相关的 3 条（logBlockSize/maxBufBucket/maxKeep）；internal/device/device.go:31-36、internal/metastore/cache.go:13-17 同型，未见把无关声明硬凑成组 | 正例 |
| uber-067 | 例外：相邻的局部变量声明即使无关也应分组 | 反例：internal/device/device.go:444 `var specs []pspec` 与 :445 `var tmps [][]byte` 相邻却写成两条独立 var，未用 `var (...)` 分组 | 反例 |
| uber-068 | import 分两组：标准库 / 其他 | internal/device/device.go:3-18（stdlib 一组、内部包一组）；脚本全量扫描 148 个非 third_party 文件，import 块内 stdlib/外部混排（无空行分隔）**0 违规** | 正例 |
| uber-069 | 包名全小写、无大写无下划线 | internal/ierr/ierr.go:5 `package ierr`、internal/bufpool/bufpool.go:15 `package bufpool`、internal/aio/aio.go:27 `package aio`。反例：internal/rpcclient/api_test.go:1 `package rpcclient_test`（Go 外部测试包强制约定，属规范豁免） | 正例（附 Go 强制的 `_test` 例外） |
| uber-070 | 包名简短精炼 | internal/aio/aio.go:27 `package aio`、internal/ierr/ierr.go:5 `package ierr` —— 全仓 17 个包名均 ≤7 字符 | 正例 |
| uber-071 | 包名不应是复数 | internal/cluster/kv.go:4 `package cluster`（不是 `clusters`）；全仓包名无复数形式 | 正例 |
| uber-072 | 包名不应叫 common/util/shared/lib | internal/bufpool/bufpool.go:15 `package bufpool` —— 全仓 17 个包名（aio/benchkit/bufpool/cluster/device/ierr/layout/metastore/protocol/rpcclient/storage/taihuclient/transport/version…）无 common/util/shared/lib | 正例 |
| uber-073 | 包名应无需在多数调用点重命名 | 全仓仅 1 处导入别名（internal/cluster/kv_tikv.go:8），且原因是上游包名 `error` 与内置名冲突，不是包名与路径不符 | 正例 |
| uber-074 | 函数名用 MixedCaps | internal/storage/storage.go:57 `func (s *Storage) MaxObjectSize() int64`；grep `^func ..._` 与 `^func (r T) ..._` 零命中 | 正例 |
| uber-077 | 其他场景避免导入别名，除非直接冲突 | 全仓唯一别名 internal/cluster/kv_tikv.go:8 `tikverr`，属「与内置标识符直接冲突」的正当例外 | 正例 |
| uber-078 | 函数按大致调用顺序排序 | internal/transport/protocol/protocol.go 按消息类型成对排（:156 EncodePutHeader / :247 ParsePutHeader、:326 EncodePong / :334 ParsePong…）；internal/transport/server.go:62 Serve → :96 serveConn → :109 dispatch → :202 handlePut/:297 handleGet/:364 handleDelete/:391 handleStat，调用链自上而下 | 正例 |
| uber-079 | 文件内函数按 receiver 分组 | internal/storage/storage.go 全部方法均为 `func (s *Storage)`，中间不夹其他 receiver 或游离函数（grep `^func [a-z]` 零命中）；internal/metastore/segments.go:46-382 全部 `func (m *segmentManager)` | 正例 |
| uber-080 | 导出函数应在文件前部（类型/const/var 之后） | internal/storage/storage.go:19 `type Storage` → :31 `NewStorage` → 各导出方法至 :482，文件内无未导出顶层函数 | 正例 |
| uber-081 | NewXyz 可紧随类型定义、先于该 receiver 其余方法 | internal/storage/storage.go:19 `type Storage` → :31 `func NewStorage(...)`；internal/transport/server.go:40 `type Server` → :50 `NewServer` → :56 `NewServerWithOptions` → 其余方法 | 正例 |
| uber-082 | 纯工具函数应放在文件靠后位置 | internal/metastore/segments.go:384 `func iterateMapping(db *pebble.DB, fn func(...) error) error` —— 全部 method 定义（:46-382）之后的文件末尾；internal/aio/aio.go:19-22 包文档进一步说明「本文件只含导出名，未导出的辅助函数在 aio_internal.go」 | 正例 |
| uber-083 | 减少嵌套：先处理错误/特殊分支，提前 return/continue | internal/device/device.go:91 `if err != nil { return nil, fmt.Errorf("taihu: open device %q: %w", nvmePath, err) }`；internal/transport/batch.go:108 `if len(batch) == 0 { continue }`；internal/transport/server.go:80 `if len(els) == 0` 早返回 | 正例 |
| uber-084 | if 两分支都赋值给同一变量时改写为单个 if | 反例：cmd/taihu/cmd/bench_single.go:83-87 `var endpoint string; if c.transport == "rpc" { endpoint = fmt.Sprintf("single(rpc:%s)", c.addr) } else { endpoint = fmt.Sprintf("single(shm:%s)", c.shm) }` —— 与原文反例同构 | 反例 |
| uber-085 | 顶层变量用标准 `var` 关键字 | internal/transport/frame.go:34 `var errConnClosed = errors.New("taihu: connection closed")`；internal/bufpool/bufpool.go:44 `var pool = newAlignedPool()`；顶层用 `:=` 零命中 | 正例 |
| uber-086 | 顶层变量不写类型，除非表达式类型与期望类型不一致 | internal/transport/frame.go:34 与 internal/device/device.go:48 都无类型标注；grep `^var X T = ` 仅命中 `var _ I = ...`（接口断言，非普通变量） | 正例 |
| uber-087 | 未导出顶层 var/const 加 `_` 前缀 | 反例：internal/bufpool/bufpool.go:44 `var pool = newAlignedPool()`、:161 `var exactPool = ...`、internal/metastore/kv_pebble.go:65 `var syncWO = ...`、internal/aio/aio_uring_linux.go:184 `var uringOverflowOnce sync.Once` —— 全部无 `_` 前缀（grep `^(var\|const) _[a-zA-Z]` 非断言零命中） | 反例 |
| uber-088 | 例外：未导出错误值可用 `err` 前缀不加下划线 | internal/transport/frame.go:34 `errConnClosed`、internal/device/device.go:48 `errDeviceClosed`、internal/aio/aio_internal.go:18 `errInvalidMaxEvents`、internal/transport/shm_frame_linux.go:28 `errShmBadFrame` | 正例 |
| uber-089 | 内嵌类型放在字段列表最前，且与普通字段间须有空行 | 反例：cmd/taihu/cmd/bench_single.go:106 `benchkit.Config` 是 struct 的**最后一个**字段，且与 :105 `cpuProfile string` 紧邻无空行；cmd/taihu/cmd/bench_cluster.go:146 同型 | 反例 |
| uber-090 | 内嵌应提供实在好处，无对用户不利的副作用 | cmd/taihu/cmd/bench_single.go:106 内嵌 `benchkit.Config` 后，压测公共字段（Mode/Size/Threads/Count/Prefix）直接提升为配置字段，被 internal/benchkit/run.go:83 `Run(ctx, s, c.Config, ...)` 直接消费，语义恰当 | 正例 |
| uber-091 | 内嵌**不应**：暴露无关方法/让用户观察内部/… | 反例（均在测试代码）：test/e2e/c_registry_test.go:62 `type cHostRewriteKV struct { cluster.KV; name string; hostname string }` 内嵌整个接口，把 KV 全部方法暴露到外层；internal/transport/transport_frame_test.go:369 `netpoll.Writer`、:385 `netpoll.Connection` 同型（用内嵌换「未实现方法自动透传」） | 反例 |
| uber-092 | 内嵌要自觉、有意图；litmus test：内层导出成员是否都会加到外层 | 正例：cmd/taihu/cmd/bench_single.go:106 通过 litmus（`benchkit.Config` 的导出字段正是该压测配置要的全部）；生产代码仅此 2 处内嵌，无「为美观/便利」的内嵌 | 正例 |
| uber-093 | 例外：mutex 不应被内嵌，即使外层未导出 | internal/transport/frame.go:80 `wmu sync.Mutex`（命名字段）；内嵌字段全量扫描的 7 处结果中无 `sync.Mutex`/`sync.RWMutex` | 正例 |
| uber-094 | 局部变量显式赋值应用 `:=` | internal/metastore/kv_pebble.go:171 `idxByKey := make(map[string]int, len(keys))`；全仓 grep 局部 `var x = ...`（显式赋值）零命中 | 正例 |
| uber-095 | 需要强调默认值时用 `var`（如空 slice） | internal/transport/client_admin.go:116 `var keys []string`；internal/device/device.go:444 `var specs []pspec`、:445 `var tmps [][]byte`；internal/storage/compact.go:110 `var cands []cand`；grep `:= []T{}` 零命中 | 正例 |
| uber-096 | nil 是合法的长度 0 slice，不要显式返回长度 0 的 slice | internal/cluster/kv_mem.go:37 `return append([]byte(nil), v...)`（空输入返回 nil 而非分配空 slice）；grep `return []T{}` 零命中 | 正例 |
| uber-097 | 判空一律 `len(s) == 0`，不与 nil 比较 | 反例：internal/transport/client.go:178 `if buf == nil`（buf 是 `[]byte`）；internal/transport/client_shm_linux.go:195 `if buf == nil && out == nil`。正例：internal/transport/batch.go:108 `if len(batch) == 0`、internal/transport/server.go:350 | 反例 |
| uber-098 | 零值 slice 无需 make() 即可直接用 | internal/device/device.go:444-445 `var specs []pspec` / `var tmps [][]byte` 声明后直接 append（:446 起），不经 make | 正例 |
| uber-099 | 注意 nil slice 与已分配长度 0 slice 不等价（如序列化行为不同） | internal/cluster/capacity.go:29 `if err != nil || v == nil`（用 nil 区分「无记录」与「空值」）；internal/cluster/instance.go:13 注释「旧 server 无此字段时为空」——JSON 层显式依赖 nil/空的可区分性 | 正例 |
| uber-100 | 尽量缩小变量/常量作用域；与减少嵌套冲突时不缩小 | internal/storage/storage.go:89 `if err := s.PutAppend(ctx, seg, off, size, in); err != nil`；:271 `if n := int64(len(data)); n < want`；internal/storage/storage.go:450 `if _, err := s.db.GetMapping(ctx, key); err != nil` | 正例 |
| uber-101 | 结果在 if 之外还需要时就不应缩小作用域 | internal/transport/server_admin.go:105 `prefix, err := protocol.ParseKeyReq(first.r)` —— 两个结果后续还要用，故在 if 之外声明，未塞进 if 初始化语句 | 正例 |
| uber-102 | 常量不必是全局，除非多函数/多文件使用或属对外契约 | internal/aio/probe_linux.go:48 `const maxOps = 256`（函数内）；internal/bufpool/bufpool.go:142 `const blockSize = 4096`（函数内）；internal/transport/server_admin.go:74 `const maxPerFrame = ...`（函数内）；internal/aio/aio_uring_linux.go:231-232 | 正例 |
| uber-103 | 避免裸参数；参数含义不明显时加 C 风格注释 | 反例：internal/transport/client_shm_linux.go:198 `recordRxDataFrame(int(rem), true)`、:206/:210 同型 —— bool 实参无 `/* zeroCopy */` 注释 | 反例 |
| uber-104 | 更好的做法是用自定义类型替代裸 bool | 反例：internal/transport/stats.go:38 `func recordRxDataFrame(rem int, zeroCopy bool)`（裸 bool）；internal/aio/aio_other.go:42 `func newIOUringRing(int, bool)` 连参数名都没有 | 反例 |
| uber-105 | 用原始字符串字面量避免手工转义 | 反例：cmd/taihu/cmd/server.go:291 flag usage 里手写 `\"多 stream 多 worker\"`。全仓非 tag 的反引号原始字符串仅出现在注释里（internal/rpcclient/objectstore.go:15） | 反例 |
| uber-106 | 初始化结构体几乎总应指定字段名 | internal/storage/options.go:17 `return options{aioMode: aio.ModeAuto}`；internal/device/options.go 同型；非测试代码位置式多字段字面量零命中 | 正例 |
| uber-107 | 例外：测试表字段 ≤3 时可省略字段名 | internal/ierr/ierr_test.go:12-19 `all := []struct{ name string; err error }` + `{"ErrNotFound", ErrNotFound}`（2 字段）；internal/layout/layout_test.go:8-14（3 字段 name/capacity/wantCnt，位置式）；internal/layout/layout_test.go:49 | 正例 |
| uber-108 | 用字段名初始化时省略零值字段 | internal/storage/options.go:17 `options{aioMode: aio.ModeAuto}` 只写非零值，零值的 `aioIOPoll` 省略；internal/device/options.go:7 同型 options struct | 正例 |
| uber-109 | 全字段省略时用 `var` 形式 | internal/cluster/register.go:54 `var info InstanceInfo`（decode 前先声明零值），:69 同型；internal/cluster/capacity.go:32 `var c CapacityRecord`、internal/cluster/client.go:87 `var info ClientInfo`；grep `:= T{}` 非测试零命中 | 正例 |
| uber-110 | 初始化结构体引用用 `&T{}` 而非 `new(T)` | 反例：internal/bufpool/bufpool.go:52 `p.pools[i] = new(bytePool)`；:129 `bp = new(bytePool)` | 反例 |
| uber-111 | 空 map / 程序化填充的 map 优先用 make(..) | 正例：internal/metastore/segments.go:47 `make(map[int64]*segEntry)`；internal/device/device.go:106-107；pkg/taihu-client/index.go:54。反例：cmd/taihu/cmd/cluster.go:231 `byInst := map[string]int{}` 后逐条填充 | 正例（附反例） |
| uber-112 | map 持固定元素集合时用 map 字面量 | cmd/taihu/cmd/cluster.go:350 `label := map[string]string{cluster.InstanceKeyPrefix: "实例注册", ...}[p]` —— 固定映射查表 | 正例 |
| uber-113 | 经验法则：固定元素用字面量，否则 make（尽量带 size hint） | 正例同 uber-112（cmd/taihu/cmd/cluster.go:350）；反例同 uber-111（cmd/taihu/cmd/cluster.go:231 程序化填充却用字面量初始化） | 正例（附反例） |
| uber-114 | Printf 之外的格式串应定义为 const | 反例：test/e2e/harness_test.go:670 `func waitFor(t *testing.T, timeout time.Duration, format string, cond func() bool, args ...interface{})`，:678 `fmt.Sprintf(format, args...)` —— 格式串是变量。生产代码扫描（所有 Fprintf/Sprintf 调用）格式串均为字面量 | 反例（仅测试） |
| uber-115 | Printf 风格函数应尽量用预定义名，便于 go vet 检查 | 反例：test/e2e/harness_test.go:670 `waitFor(..., format string, args ...)` 是包装 t.Fatalf 的 Printf 风格函数，名字不以 `f` 结尾 | 反例 |
| uber-116 | 无法用预定义名时函数名应以 `f` 结尾 | 反例：同上 —— `waitFor` 而非 `waitf`，go vet 无法识别为 printf 包装（须 `-printf.funcs=waitfor` 才能检查）。生产代码无自研 Printf 包装函数 | 反例 |
| uber-117 | 多组输入/输出条件下应用表驱动测试 | internal/layout/layout_test.go:8-28 `cases := []struct{...}` + `for _, c := range cases` + :19 `t.Run(c.name, ...)`；全仓 `t.Run(` 53 处、表驱动 `range cases` 20+ 处 | 正例 |
| uber-118 | 约定：slice 名 `tests`、用例名 `tt`、give/want 前缀 | 反例：惯用名是 `cases`/`c`/`tc` 而非 `tests`/`tt` —— internal/layout/layout_test.go:8 `cases := []struct{...}` + :18 `for _, c := range cases`；internal/ierr/ierr_test.go:12 `all := []struct{...}`；internal/transport/transport_frame_test.go:407 `cases` + :423 `for _, tc := range cases`。字段名用 `want` 但不用 `give` | 反例 |
| uber-119 | 子测试含复杂/条件逻辑时不应使用表测试 | 反例：internal/transport/transport_frame_test.go:438-518 的表用例靠 `wantErr` 走条件断言（:512 `if tc.wantErr && err == nil`、:515 `if !tc.wantErr && err != nil`），不同用例执行路径不同 | 反例 |
| uber-120 | 表测试理想：聚焦最窄行为、最小 test depth、所有字段都用上、逻辑对所有用例都执行 | 反例：internal/transport/transport_frame_test.go:412 的 `call func(c *Conn) error` 字段只在子测试内被调一次、各用例语义差别大（put/get/delete/stat/ping/meta/segments/listkeys 混在一张表） | 反例 |
| uber-122 | 分支路径/针对 mock 的 if/把函数放进表里会让表测试混乱 | 反例：internal/transport/transport_frame_test.go:412 `{"put-header-frame", 0, func(c *Conn) error { return c.Put(ctx, "k", int64(len(payload)), payload) }}` —— 正是原文点名的「把函数放进表里」 | 反例 |
| uber-123 | 行为只随输入变化时，把相似用例聚在同一张表更好 | internal/layout/layout_test.go:10-14 四个用例只有 capacity 不同（16TiB 整除 / 非整除 / 小于一段 / 恰好一段），聚在一张表体现「随输入变化」；internal/transport/protocol/protocol_test.go:112-114 同型 | 正例 |
| uber-124 | 允许用单个 shouldErr 字段区分成功/失败 | internal/transport/transport_frame_test.go:442 `wantErr bool` 字段 + :512/:515 单条分支路径（原文明确 acceptable） | 正例 |
| uber-128 | 预见需扩展的公开构造器用 Functional Options | internal/device/device.go:74 `func NewDevice(ctx, nvmePath string, segSize int64, opts ...Option)`；internal/storage/storage.go:31 `NewStorage(..., opts ...Option)`；internal/storage/options.go:5-7 注释「新增配置项时在此扩展，既有调用点无需改动签名」 | 正例 |
| uber-129 | 参数已有 3 个或更多时尤其应使用 | internal/storage/storage.go:31 `NewStorage(ctx, rocksdbDir, nvmePath string, l layout.Layout, opts ...Option)` —— 4 个必选参数 + 变参选项 | 正例 |
| uber-132 | 比选哪套 linter 更重要的是全库一致地 lint | Makefile:21-22 `check: check-fmt check-layering check-sdk-only` + `go vet ./...` —— 固定的全仓检查集，另有 Makefile:65-69 的跨平台 `check-linux` | 正例 |
| uber-133 | 至少推荐 errcheck/goimports/revive/govet/staticcheck | 反例：Makefile:22 只有 `go vet ./...`；Makefile:26 用 `gofmt -l` 代替 goimports；errcheck/revive/staticcheck 全仓零配置 | 反例 |
| uber-134 | 推荐 golangci-lint 作为 lint runner | 反例：仓库根无 `.golangci.yml`/`.golangci.yaml`（`ls .golangci*` 无匹配），lint 直接在 Makefile:22 调 `go vet` | 反例 |

---

## 二、冲突（4 条）

| 编号 | 规则一句话 | taihu 现状（file:line，已核实） | 为什么冲突 |
|------|-----------|-------------------------------|-----------|
| uber-038 | 原子操作用 `go.uber.org/atomic` 而非 `sync/atomic` | 12 个文件 import `"sync/atomic"`（internal/transport/batch.go:20、internal/transport/server_shm_linux.go:22、internal/transport/frame.go:11、internal/transport/stats.go:7、internal/rpcclient/dial.go:9、internal/aio/aio_uring_linux.go:11、internal/benchkit/run.go:11、internal/benchkit/report.go:9、internal/device/device.go:20、pkg/taihu-client/picker.go:5、pkg/taihu-client/storage.go:8、cmd/taihu/cmd/bench_storage.go:18）；全仓无一处 import `go.uber.org/atomic`（仅 go.mod 里 indirect）；实际用的是标准库**泛型化原子类型**，如 internal/benchkit/report.go:15 `ops *atomic.Int64`、internal/benchkit/run.go:87 `var ops atomic.Int64` | 规则把「类型安全的原子操作」绑定在 Uber 自研包上。Go 1.19 起 `sync/atomic` 已提供 `atomic.Int64/Bool/Pointer` 等类型化 API，规则的原意（防裸类型误用、含 `atomic.Bool`）由标准库完整覆盖；taihu 采用标准库版本，遵守该条就等于引入一个为同一能力而生的第三方依赖，与「`internal/aio` 纯 Go 不依赖外部库」的既定设计取向相悖 |
| uber-039 | 避免可变全局变量，改用依赖注入（含函数指针等） | pkg/taihu-client/tikv.go:35 `var newTiKVKV = func(ctx context.Context, pdAddrs []string, tls cluster.TLSConfig) (cluster.KV, error) {` —— 包级可变函数指针；cmd/taihu/cmd/helpers.go:63 同型。测试里保存/替换/恢复该全局：pkg/taihu-client/tikv_test.go:22-26、:35-40，cmd/taihu/cmd/bench_cli_test.go:58-60 `old := newTiKVKV; newTiKVKV = func(...){...}; t.Cleanup(func(){ newTiKVKV = old })`。代码注释明确它是**有意的**「测试缝隙」（cmd/taihu/cmd/server.go:111「newTiKVKV 是包级测试缝隙，生产恒为 cluster.NewTiKVKV」） | 规则要求的就是「不要可变全局、改用依赖注入」，而 taihu 正是为了给 CLI/SDK 的 TiKV 连接路径造单测缝隙才刻意保留了这个包级函数变量（见提交 c5e3c87「抽出测试缝隙使 CLI/SDK 可单测」）。遵守该条就得把 `newTiKVKV` 改成显式注入的参数/字段，会改动 `connectKV` 等一批调用点签名与 CLI 装配路径 |
| uber-130 | 推荐实现：声明带未导出方法的 `Option` 接口，选项记录在未导出 `options` struct | internal/storage/options.go:7 `type Option func(*options)`、:10-13 未导出 `options` struct、:23-25 `func WithAIOMode(m aio.Mode) Option { return func(o *options) { ... } }`；internal/device/options.go:7 同型 | taihu 只有「未导出 options struct」这一半，**Option 用的是闭包函数类型而非接口**，与原文「Our suggested way」相反。改成接口式会改动 `WithAIOMode`/`WithAIOIOPoll` 的返回类型、`options` 的填充方式与 cmd/taihu/cmd/root.go:117-125 `aioOptions()` 的构造 |
| uber-131 | 相比闭包，更推荐 Option 接口方式（选项可比较、可实现 fmt.Stringer） | internal/storage/options.go:7 `type Option func(*options)`；internal/device/options.go:7 同型 —— 闭包式选项无法比较、无法实现其他接口 | 规则明确贬低闭包实现。taihu 选择了闭包：选项数量少（每包 2 个）、无需在测试中比较选项、也不需要 `fmt.Stringer`。遵守该条属纯风格迁移，会改掉两个包的公开 API（`Option` 类型定义） |

---

## 三、不适用（8 条）

| 编号 | 规则一句话 | 为什么不适用 |
|------|-----------|--------------|
| uber-027 | 用 `%v` 遮蔽底层错误（调用方无法匹配，日后可改回 `%w`） | 在 taihu 里既无正例也无反例：`fmt.Errorf` + `%v` + `err` 的写法全仓零命中（`grep -rnE 'fmt\.Errorf\("[^"]*%v", *err\)' internal pkg cmd` 空）。taihu 的错误传播策略是「可匹配的一律 `%w`、其余原样返回」，不存在需要刻意遮蔽底层错误的场景。无锚点，不硬凑 |
| uber-064 | 推行规范建议在包或更大粒度上变更 | 这是「把规范落到代码库」的实施粒度建议（过程性规则），不描述任何代码形态；taihu 源码里没有对应锚点 |
| uber-075 | 例外：测试函数可含下划线做用例分组（`TestMyFunction_WhatIsBeingTested`） | taihu 测试函数全部为 `TestXxx`，`grep -rn 'func Test[A-Za-z0-9]*_'` 零命中 —— 该例外从未被使用，既无正例也无反例 |
| uber-076 | 包名与导入路径末段不符时必须用导入别名 | taihu 唯一一处导入别名（internal/cluster/kv_tikv.go:8 `tikverr`）的原因不是路径不符：`github.com/tikv/client-go/v2/error` 的包名与路径末段一致（都叫 `error`），别名是为了避免遮蔽内置名（见 uber-041）。规则描述的场景在 taihu 不存在 |
| uber-121 | `test depth` 的定义（一次测试中连续断言的个数） | 这是为 uber-119/120/122 服务的**术语定义**，本身不构成可执行规则，没有可指认的正/反例锚点 |
| uber-125 | 表测试 vs 独立测试没有严格准则，可读性/可维护性优先 | 元建议（原文自述 "no strict guidelines"），不含可判定的要求；无锚点。其精神已由 uber-117/119/123/124 在 taihu 的具体锚点覆盖 |
| uber-126 | 并行测试/特殊循环必须显式在循环作用域内复制循环变量 | 前提在本项目不成立：全仓 `t.Parallel()` 零命中（taihu 测试不用并行子测试），且 go.mod 声明 `go 1.25.0`，循环变量自 Go 1.22 起已按迭代独立，该规则要防的捕获问题在语言层已消除 |
| uber-127 | 用了 `t.Parallel()` 时必须声明作用域限定在本次迭代的 `tt` | 同 uber-126：`t.Parallel()` 零命中，规则触发条件在 taihu 不存在；Go 1.25 亦无此必要 |

---

## 附：自检

### 三类条数合计

| 类别 | 条数 | 编号 |
|------|------|------|
| 适用 | **122** | 001-026、028-037、040-063、065-074、077-124（缺 121）、128、129、132-134 |
| 冲突 | **4** | 038、039、130、131 |
| 不适用 | **8** | 027、064、075、076、121、125、126、127 |
| **合计** | **134** | 与 uber-001 ~ uber-134 逐条一一对应，无遗漏、无重复 |

（校验：134 − 4 − 8 = 122 ✓）

### 我实际跑过的验证命令（代表）

```bash
# 接口 / mutex / 内嵌
grep -rn 'var _ ' --include='*.go' . | grep -v third_party
grep -rn 'new(sync\.' --include='*.go' . | grep -v third_party            # 零命中
grep -rn -P '^\t(sync\.)?(RWMutex|Mutex)$' --include='*.go' .             # 零命中
# 内嵌字段全量扫描（python 逐 struct 体匹配单标识符行）→ 7 处，生产仅 2 处
# 边界复制 / 错误语义
grep -rn 'append(\[\]byte(nil)' --include='*.go' internal pkg cmd
grep -rn 'failed to' --include='*.go' internal pkg cmd                    # 零命中
grep -rnE 'fmt\.Errorf\("[^"]*%v", *err\)' --include='*.go' internal pkg cmd  # 零命中
python3 - <<'EOF'   # log 语句后 3 行内 return err 的扫描 → 零命中
EOF
# 工具链与时间
awk 'length > 99 {c++} END {print c+0}' $(find internal pkg cmd -name '*.go' ! -name '*_test.go')  # 833
awk '{ if (length > m) {m=length; f=FILENAME":"FNR} } END {print m, f}' ...                        # 191 cmd/taihu/cmd/bench_cluster.go:151
ls .golangci*                                                             # no matches
grep -n 'lint\|vet\|gofmt' Makefile
# import 分组（python 逐文件解析 import 块判 stdlib/外部相邻性，148 个非 third_party 文件 0 违规）
# 逐个锚点回读
sed -n 'Np' <file>     # 对本文档每一条锚点均执行过
```

**锚点核实原则**：本文档每一条 file:line 都用 `sed -n 'Np'` 或 `grep -n` 回读过原文确认内容；凡是回读发现行号偏移的（如 `kv_tikv.go` 的 `TLSConfig.enabled` 实际在 :22、`cmd/root.go` 的 `DurationVar` 实际在 :91、`aio.go` 的 `invalid io-uring mode` 实际在 :121、`harness_test.go` 的 `waitFor` 签名实际在 :670）都已按回读结果改正。

### 我拿不准的条目

| 编号 | 归入 | 不确定在哪 |
|------|------|-----------|
| uber-027 | 不适用 | 规则本身对任何 Go 项目都成立，归「不适用」纯因 taihu 里找不到正/反例锚点。若按「项目形态」而非「有无锚点」判定，它更接近「适用但未使用」。倾向：spec 里可写成「taihu 不使用 `%v` 遮蔽，一律 `%w` 或原样返回」，即把该条降级为一条说明而非规则 |
| uber-050 | 适用（反例） | 规则要求引入 `go.uber.org/goleak` 这一测试期第三方依赖。taihu 的 `internal/` 生产代码刻意零外部依赖（`internal/aio/aio.go:1` 包文档），但 go.mod 里确实存在 testify 等直接 require（尽管全仓 Go 代码未 import）。因此「引入 goleak 是否触犯项目的依赖纪律」我无法从代码判断——需 owner 决策。若判为触犯，应改归「冲突」 |
| uber-064 / uber-121 / uber-125 | 不适用 | 三条都是过程性/定义性/元建议内容，不描述代码形态。归「不适用」是按「无锚点不硬凑」的纪律执行；若 spec 希望保留方法论条目，可单独放「附则」而非「不适用」 |
| uber-075 / uber-076 | 不适用 | 两条都是「例外/条件」型规则，taihu 恰好没有触发条件，因此既非正例也非反例。归「不适用」是正确的（规则的前提在本仓不存在），但这不等于「规则不该进 spec」——它们是 uber-074/uber-041 的必要补充说明，建议在 spec 里以「例外」形式附在对应主规则下 |
| uber-014 | 适用（反例） | 三处枚举全部从 0 开始，严格说都落在 uber-015 的「零值即默认」例外里。把它们同时标成 014 的反例是「形式上违反、实质上合规」。若 spec 更看重实质，014 可视作已被 015 完全吸收（无实际约束力） |
| uber-022 | 适用（正例） | 「需匹配 + 动态 → 自定义 error 类型」这一格，taihu 只有 `uringParamError` 一个实例，且它在测试里只用 `.Error()` 取消息、没有用 `errors.As` 匹配过。所以这一格是「形式满足、匹配用途未被实际使用」，锚点真实但说服力偏弱 |

（其余条目锚点均为直接 grep/回读所得，无不确定。）
