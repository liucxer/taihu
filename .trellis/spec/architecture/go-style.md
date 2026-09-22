# Go 语言层规则

> [code-style.md](./code-style.md) 管的是**本仓库特有的约定**（注释写什么语言、日志走哪条通道、import 怎么分组）；本文件管的是**Go 语言本身的规则在 taihu 的落地**——接收者怎么选、`nil` slice 怎么判、时间类型用什么、`init()` 能不能写。
>
> **为什么单开一个文件**：现有 spec 的规则是围绕**机制**长出来的（aio 的 ring 契约、bufpool 的所有权、protocol 的跨边界约定），对语言层主题确实一片空白——对 `time.Time`、`strconv`、`errgroup`、原始字符串、`make(chan` 缓冲容量、内嵌类型的 grep 在合并前**全部零命中**。这些规则不写在任何地方，但它们每一条都被跨包遵守着，是「隐性约定」里最大的一块。

**准入**：每条规则都必须能指到真实文件行号（本文件所有锚点均已逐条 `sed -n` 核对）。指不到的一律不写。

**与上游的关系**：本文件的主体来自 Uber Go Style Guide 的对撞结果，每条标注来源编号（`uber-NNN`）。**不采纳**的上游条目与其理由见文末。

**已有违反的处理**：本文件按「写应然规则 + 显式标注既有例外」落地。凡标注 `⚠️ 既有例外` 的，表示仓库现状与规则不符且**本轮不改代码**，判据随文写明——照 [api-surface.md](./api-surface.md) 规则 4 的先例：新代码照规则来，已有代码遇到再收。

---

## 规则 1：接收者按「要不要改接收者指向的值」选，接口满足性由整组方法决定

`uber-002 / uber-005 / uber-006`

判据只有一条：**方法需要修改接收者指向的数据，就用指针接收者；否则用值接收者**。`internal/cluster` 两个实现把这条展示得很干净：

```go
// 指针接收者：Put 要就地改 k.m
func (k *MemoryKV) Put(...) error {   // internal/cluster/kv_mem.go:23
	k.mu.Lock()
	k.m[string(key)] = append([]byte(nil), value...)
	...
```

```go
// 值接收者：enabled() 只读三个字段，不碰原值
func (t TLSConfig) enabled() bool { ... }   // internal/cluster/kv_tikv.go:22
```

**由此推出接口满足性**（本条最容易踩的地方）：一个类型**能不能**满足接口，取决于它**整组方法**的接收者形态——

| 类型 | 方法接收者 | 接口断言写法 | 位置 |
|------|-----------|-------------|------|
| `MemoryKV` | 8 个全是 `*MemoryKV` | `var _ KV = (*MemoryKV)(nil)` | `internal/cluster/kv_mem.go:16` |
| `cliTestErrKV` | 8 个全是 `cliTestErrKV` | `var _ cluster.KV = cliTestErrKV{}` | `cmd/taihu/cmd/cli_core_test.go:48` |

全指针接收者的类型，**值**不满足接口（值不可寻址到指针方法集）；全值接收者的类型，值和指针都满足。`uber-005` 的另一半由此可推：值接收者方法在值和指针上都能调，指针接收者方法只能在可寻址的值上调。

**实践建议**：同一个类型的方法集**不要混用两种接收者**。混用会让接口满足性变得难以一眼判断，也让「这个类型的零值能不能直接用」变成需要翻完全部方法才能回答的问题。

## 规则 2：接口按值传递，不要 `*Interface`

`uber-001`

接口值本身已经是「两个字长的描述符」，再取指针没有任何收益，只会多一层解引用、并让「传 `nil` 接口」与「传指向 `nil` 接口的指针」这两种情况变得无法区分。

taihu 全仓没有一处把接口作指针形参。`internal/benchkit` 是最典型的形态——`Store` 是接口，一路按值传到底：

```go
type Store interface { ... }                          // internal/benchkit/run.go:38
func Run(ctx context.Context, s Store, ...) error     // internal/benchkit/run.go:84
func runWorker(ctx context.Context, s Store, ...)     // internal/benchkit/run.go:116
func runPipelinedWorker(ctx context.Context, s Store, ...)  // internal/benchkit/run.go:160
```

## 规则 3：mutex 是非指针命名字段，且**绝不内嵌**

`uber-007 / uber-008 / uber-093`

`sync.Mutex` 的**零值就是有效的未加锁状态**，不需要构造、不需要初始化，因此「指向 mutex 的指针」几乎总是错的——那会把一个零值可用的类型变成一个可能为 `nil` 的类型。

正确形态是**非指针的命名字段**：

```go
type MemoryKV struct {
	mu sync.RWMutex     // internal/cluster/kv_mem.go:12 —— 零值即未加锁，无需构造
	m  map[string][]byte
}
```

**绝不内嵌**（`uber-093`）：内嵌会让 `Lock`/`Unlock` 提升为外层类型的**导出方法**，任何拿到该类型的人都能绕过它自己的锁纪律去加锁。`internal/transport` 的两个结构体都写成命名字段：

```go
type Server struct {
	...
	mu  sync.Mutex            // internal/transport/server.go:45
	els []netpoll.EventLoop
}
```

```go
type Conn struct {
	...
	wmu sync.Mutex            // internal/transport/frame.go:80
	mu      sync.Mutex        // internal/transport/frame.go:82
	streams map[uint32]*stream
}
```

**把锁的保护范围写在声明处**（`th-133` 在 transport 侧的同类做法，两者可互相印证）。三把锁里目前只有一把做到了：

| 锁 | 保护范围 | 写法 |
|----|---------|------|
| `Conn.wmu` | ✅ 写了 | 类型文档 `internal/transport/frame.go:75-76`（「写由 wmu 串行化…`WriteBinary` 引用调用方缓冲直到 Flush 排空，故 wmu 必须在 Flush 返回后才释放」）+ 行内注释 `:80` |
| `Conn.mu` | ❌ 没写 | `internal/transport/frame.go:82` 光秃秃一行 |
| `Server.mu` | ❌ 没写 | `internal/transport/server.go:45` 无注释；`:46` 的注释是给 `els` 的，不是给 `mu` 的 |

## 规则 4：接收 slice / map 并保留引用时，必须复制

`uber-009`

Go 的 slice 和 map 是引用类型。函数**存下**了调用方传进来的 slice/map，就等于两者共享了底层存储——调用方之后复用那块数组，你这边看到的数据会跟着变。

`internal/cluster/kv_mem.go:25` 是这条的教科书写法：

```go
func (k *MemoryKV) Put(_ context.Context, key, value []byte) error {
	k.mu.Lock()
	k.m[string(key)] = append([]byte(nil), value...)
	k.mu.Unlock()
```

`append([]byte(nil), value...)` 不依赖调用方的缓冲——**存进 map 的是副本**。

> 注意这条与 `.trellis/spec/engine/buffer-and-concurrency.md` 的缓冲所有权协议**方向相反、互不替代**：那边管的是**缓冲区怎么归还池**（谁分配谁归还），这边管的是**入参怎么防御性拷贝**（谁存谁复制）。一个函数可以同时受两条约束。

## 规则 5：资源清理用 `defer`

`uber-011 / uber-012`

解锁、关迭代器、归还缓冲——凡是有「配对动作」的，用 `defer` 而不是在每个返回路径上重复写。全仓非测试代码 **138 处 `defer`**。

三种典型形态（各代表一类配对）：

```go
defer m.mu.Unlock()                 // internal/metastore/segments.go:266   解锁
defer it.Close()                    // internal/metastore/kv_pebble.go:193  关迭代器
defer bufpool.Put(buf)              // internal/device/device.go:397        归还缓冲
```

第三处还带了理由注释 `// submit 均阻塞至完成，返回后缓冲即可复用`——**归还时机本身有前提时，把前提写在 `defer` 那一行**。

**不要以「`defer` 慢」为由回避它**（`uber-012`）：它的开销在纳秒量级，只有在真正纳秒级的函数里才有讨论余地。taihu 的热路径（`internal/device` 的完成泵、`internal/transport` 的帧处理）都在用 `defer`，未见到被 `defer` 拖慢的证据。

> 测试侧的配对动作走 `t.Cleanup` 而不是 `defer`——见 `.trellis/spec/testing/unit-tests.md`。

## 规则 6：channel 默认为无缓冲或容量 1；更大的容量必须写明理由

`uber-013`

无缓冲 channel 表达「交接」，容量 1 表达「一个待处理槽位」。**容量大于 1 是一个需要解释的设计决定**，因为它意味着：生产快于消费时队列会堆积，堆积的上限就是那个容量，而溢出时的行为（阻塞生产者）由它决定。

`internal/transport` 给出了**有论证**与**无论证**两种样子：

```go
// streamInCap 每流投递缓冲上限：读循环背压到流处理器消费速度。
const streamInCap = 8          // internal/transport/frame.go:31-32
```

而下面两处的容量是算出来的或拍出来的，**都没有说明为什么是这个数**：

```go
b.queues[i] = make(chan *writeTask, batchCap*2)     // internal/transport/batch.go:91
ch:     make(chan indexItem, 4096),                 // pkg/taihu-client/index.go:39
```

**规则**：新写容量大于 1 的 channel 时，按 `internal/transport/frame.go:31` 的样子写清「这个上限在背压/堆积上的含义」。

> ⚠️ **既有例外**：上面两处目前无论证。`internal/transport/batch.go:91` 的 `batchCap*2` 尚有文件头 `internal/transport/batch.go:1-17` 的攒批设计说明可间接推出，`pkg/taihu-client/index.go:39` 的 `4096` 则完全没交代。本轮不改代码。

## 规则 7：枚举从 0 开始，零值即「默认/未设置」

`uber-014 / uber-015`

上游的经验法则是「枚举从非零值开始，好让 `0` 能表示未设置」。taihu 的实际做法**相反**，而且是成体系的——三处枚举全部从 0 起，并把 0 定义为一个**有意义的状态**：

```go
ModeAuto Mode = iota                                                  // internal/aio/aio.go:104
SegmentStateFree       SegmentState = iota                            // internal/metastore/meta.go:77
deliverOK    deliverResult = iota                                     // internal/transport/frame.go:65
```

三处的 0 值分别表示「自动探测后端」「段空闲可分配」「投递成功」——**都是合法且有意义的默认**，不是「未设置」。这正是 `uber-015` 允许的例外情形。

**所以 taihu 采用：零值即默认语义，枚举从 0 开始。** 若将来出现一个「0 无法表示合法默认」的枚举，才改用 `iota + 1`，并在定义处写明为什么 0 不能是默认。

## 规则 8：时间一律走 `time` 包

`uber-016 / uber-017 / uber-018 / uber-019 / uber-020 / uber-021`

**不要自己算 24 小时、60 分钟、365 天的秒数**——闰秒、夏令时、`time` 包的单调时钟处理都不是手工算术能覆盖的（`uber-016`）。

**时间段用 `time.Duration`**（全仓非测试代码 **38 处**）：

```go
Interval time.Duration      // internal/storage/compact.go:20    「扫描周期」
Interval: time.Minute,      // internal/storage/compact.go:29
t := time.NewTicker(c.cfg.Interval)     // internal/storage/compact.go:65
RefreshInterval time.Duration           // pkg/taihu-client/config.go:49
pf.DurationVar(&global.timeout, "timeout", 5*time.Second, "单次交互超时")   // cmd/taihu/cmd/root.go:91
```

**时间点，能传 `time.Time` 就传**（`uber-017`）——比较和加减都交给它的方法，不要自己减时间戳：

```go
func (i *InstanceInfo) Aliveness(now time.Time, timeout time.Duration) bool {   // internal/cluster/instance.go:39
```

**无法用 `time.Time` 时**（`uber-020` / `uber-021`）：上游要求单位写进字段名、或用 RFC 3339 字符串。

> ⚠️ **既有例外，且是本文件里范围最大的一处**：跨进程/跨节点的时间点统一用 **`int64` unix 秒**，共 5 处结构体字段——`internal/cluster/capacity.go:18`（`UpdateTime int64 \`json:"update_time"\``）、`internal/cluster/instance.go:18`、`internal/cluster/client.go:22`、`cmd/taihu/cmd/client.go:66`、`cmd/taihu/cmd/cluster.go:63`（都是 `StartTime int64 \`json:"start_time"\``）。
>
> **判据**：这些不是「随手用了 int64」，而是**协议契约**——它们直接对应 JSON 字段 `start_time` / `update_time`，要跟 PD/TiKV 的可见格式对齐，且需在多个进程间可比较。`internal/rpcclient/admin.go:16` 的 `Ping` 返回值同理：`rtt time.Duration`（本进程内的时间段，用 `Duration`）+ `serverTime int64`（对端的时间点，走协议用 int64）。
>
> 反过来，**单位在注解里写着、字段名本身没带**（`UpdateTime` 而不是 `UpdateTimeUnixSec`）——这一点与 `uber-020` 的「单位写进字段名」确实不符，属已知偏差。本轮不改代码。
>
> 新代码：本进程内的时间点用 `time.Time`；只有落到**跨进程协议字段**上才用 int64 unix 秒，且须在字段注释里写明单位（照 `internal/cluster/capacity.go:18` 的 `// 记录写入时间（unix 秒）`）。

## 规则 9：类型断言一律用 comma-ok 两值形式

`uber-034`

单值断言 `x.(T)` 在类型不符时**直接 panic**。只有「这一步不符就说明代码错了、应该崩」才用单值形式；只要类型不符是**可预期的输入**，就必须用两值形式。

反例（5 处，全是容器取出的元素做单值断言）：

```go
return e.Value.(*cacheEntry).meta, true          // internal/metastore/cache.go:55
e.Value.(*cacheEntry).meta = meta                // internal/metastore/cache.go:66
entry := back.Value.(*cacheEntry)                // internal/metastore/cache.go:96
return e.Value.(*routeEntry).name, true          // pkg/taihu-client/route_cache.go:51
return ln.(*net.TCPListener), port, nil          // cmd/taihu/cmd/server.go:334
```

正例——`internal/transport/client.go:183` 断言的是一个**可选的优化接口**，类型不符完全正常，所以走两值：

```go
if rc, ok := msg.r.(interface{ ReadCopy([]byte) (int, error) }); ok {
```

> ⚠️ **既有例外**：上面 5 处。前四处断言的是**本包自己刚放进去的类型**（`cacheEntry` / `routeEntry` 只由该文件构造），第五处的 `net.Listen("tcp", ...)` 保证返回 `*net.TCPListener`——即「不可预期失败」这一例外情形，当前无实际 panic 风险。**新代码若断言的类型来自外部输入或跨包，一律 comma-ok。**

## 规则 10：内嵌是「把内层的导出成员提升到外层」，要有明确意图

`uber-040 / uber-089 / uber-090 / uber-092`

内嵌不是「少写几个字段名」的捷径——它会把内层类型的**全部导出成员**提升为外层的成员。上游给的 litmus test：**问自己「内层的每一个导出成员，是不是都该出现在外层的对外面上」**；有一个不该，就不该内嵌。

全仓非测试代码**只有 2 处内嵌**，都在 CLI 的压测配置里：

```go
type singleBenchConfig struct {          // cmd/taihu/cmd/bench_single.go:100
	transport  string
	addr       string
	...
	cpuProfile string
	benchkit.Config                     // :106
}
```

```go
	benchkit.Config                     // cmd/taihu/cmd/bench_cluster.go:146
```

这两处**符合**意图要求——`benchkit.Config`（`internal/benchkit/run.go:16`）本来就是「压测配置」这个整体，让 CLI 直接复用它是目的本身。而且两个宿主结构体 `singleBenchConfig` 均**未导出**，满足 `uber-040` 的「避免在公开结构体内嵌」。

> ⚠️ **既有例外**：内嵌字段都写在字段列表**最后**（`cmd/taihu/cmd/bench_single.go:106` 排在 `:105 cpuProfile` 之后，`cmd/taihu/cmd/bench_cluster.go:146` 同理）。`uber-089` 要求**放在最前**并与普通字段隔一个空行——理由是读者一眼就能看到「这个类型还内嵌了什么」。现有写法把这条信息埋在了末尾。本轮不改代码；**新写的内嵌类型放字段列表首位**。

## 规则 11：不要用 Go 预声明标识符做名字

`uber-041`

`error`、`string`、`len`、`cap`、`new`、`make`、`copy` 这些预声明标识符可以被遮蔽，但遮蔽之后那一行附近的代码会变得极难读——读者得先在脑子里替换掉它的惯常含义。

taihu 有一处**教科书级的正例**：`github.com/tikv/client-go/v2/error` 这个包的**包名就叫 `error`**，直接 import 会把预声明的 `error` 类型遮蔽掉。`internal/cluster/kv_tikv.go:8` 给它起了别名：

```go
tikverr "github.com/tikv/client-go/v2/error"
```

这是**全仓非测试代码里唯一一处导入别名**（实测），也正是该用别名的场合。相关讨论见 [code-style.md](./code-style.md) 的 import 分组一节。

## 规则 12：`init()` 能不用就不用；用了必须满足四条约束

`uber-042 / uber-043 / uber-044 / uber-053`

`init()` 的代价是**隐式执行**：它不在任何调用链上，读代码的人不会看到它，而它可能在任何一次 import 时运行。上游给的判据：`init()` 里**不得**依赖其他 `init()` 的顺序、不得操纵全局状态、不得做 I/O、不得启动 goroutine。

全仓非测试代码共 **12 处 `init()`**，其中 **11 处在 `cmd/taihu/cmd/`**，全部是 cobra 的 flag 注册：

```go
func init() {                       // cmd/taihu/cmd/root.go:84
	f := ...
```

这类用法属于 `uber-044` 认可的「可插拔注册表」情形——把命令的 flag 声明与命令定义放在一起，是 cobra 的惯用结构。

剩下的两处需要单独讨论（都在 A 类冲突里）：

**(a) `cmd/taihu/cmd/root.go:20` —— 操纵全局日志级别。**

```go
func init() {
	log.SetLevel(zapcore.ErrorLevel)     // :21
}
```

它改的是 `pingcap/log` 的**包级全局状态**，字面上违反「不得操纵全局」。但它的目的是抑制 tikv client-go 的 INFO 刷屏，且进程入口本身就是全局状态的所有者——见 [code-style.md](./code-style.md) 日志通道一。**判为可接受**：`main` 包对全局日志级别的设置属于进程初始化的正当部分。

**(b) `internal/transport/frame.go:25` —— 操纵另一个包的全局分配器。**

```go
func init() {
	netpoll.SetAlignedAllocator(bufpool.Get, bufpool.Put)                   // :26
	netpoll.SetInputAlignedAllocator(bufpool.GetExact, bufpool.PutExact)    // :27
	netpoll.SetInputNodeSize(protocol.InputNodeSize)                        // :28
}
```

这一处**与 `uber-042` 的「不得操纵全局状态」直接冲突**，而且它是**库代码**（不是 `main`）在 import 时改另一个包的全局行为——任何 import `internal/transport` 的程序都会被动接受这套分配器。三行同时满足 `uber-053`（没启动 goroutine）和「确定性、不依赖顺序」，纯粹是全局状态这一条不合。

> ⚠️ **既有例外**：`internal/transport/frame.go:25` 的 `init()` 保留。判据：`netpoll` 的分配器是**模块级单例**，设计上就要求一次性设置；把它挪进某个构造函数会导致「谁先构造谁设置」，反而引入顺序依赖。三行全部是常量或纯函数引用，无 I/O、无 goroutine、无顺序依赖。**这是 taihu 对 `uber-042` 的刻意偏离**，理由是可验证的（已在冲突清单 C-04 逐条核实）。新代码若需要类似的跨包全局设置，须先在本文件登记同样的理由。

## 规则 13：字符串与基本类型的互转、以及固定字符串的字节化

`uber-057 / uber-058 / uber-105`

**（a）基本类型 ↔ 字符串用 `strconv`，不要 `fmt`**（`uber-057`）。`fmt.Sprintf` 要解析格式串、走反射路径装箱参数，而 `strconv` 是直接转换——在热路径上差距明显。

反例（两处都在**逐 op 循环内**）：

```go
func (c *storageBenchConfig) keyFor(seq int) string {   // cmd/taihu/cmd/bench_storage.go:237
	return fmt.Sprintf("%s/%d", c.prefix, seq)          // :238
}
```

```go
func KeyFor(prefix string, seq int) string {            // internal/benchkit/run.go:66
	return fmt.Sprintf("%s/%d", prefix, seq)            // :67
}
```

调用点证实它们在热路径上：`cmd/taihu/cmd/bench_storage.go:256` 每 op 调一次 `c.keyFor(k)`；`internal/benchkit/run.go:125` 与 `:178` 分别在两个压测 worker 循环里调 `KeyFor`。

正例：`cmd/taihu/cmd/server.go:101` 的 `strconv.Itoa(rpcPort)`。

**（b）固定字符串不要反复做 `string → []byte`**（`uber-058`）。同一个常量在多个函数里各转一次，每次都是一份新分配。

```go
kvPrefixMapping = "m\x00"                        // internal/metastore/kv_pebble.go:27
```

它被转了三遍：

```go
prefix := []byte(kvPrefixMapping)                // internal/metastore/kv_pebble.go:188
prefix := []byte(kvPrefixMapping) // "m\x00"     // internal/metastore/kv_pebble.go:314
prefix := []byte(kvPrefixMapping)                // internal/metastore/segments.go:385
```

> ⚠️ **既有例外**：上面两处。`[1]` 的判断另有依据——`bench` 路径是 CLI 压测工具而非存储引擎热路径，收益有限；`[2]` 若要修，正确做法是在 `var` 里定义一次 `[]byte` 常量（Go 的 `const` 不支持 slice，需改用 `var`），属代码改动。**新代码：基本类型转字符串用 `strconv`；固定字符串需要 `[]byte` 时在包级 `var` 里转一次。**

**（c）含引号的格式串用反引号原始字符串**（`uber-105`）。反例：

```go
f.Int("batch", 0, "shm 批读批量：>0 启用\"多 stream 多 worker\"聚合批读（一次 io_submit 提交多个任务）；0 关闭")   // cmd/taihu/cmd/server.go:291
```

`\"` 手工转义既难读又容易写错，写成 `` `...启用"多 stream 多 worker"聚合批读...` `` 即可。

> ⚠️ **既有例外**：`cmd/taihu/cmd/server.go:291` 一处。全仓非测试代码里，反引号原始字符串目前只出现在注释里。本轮不改代码。

## 规则 14：容器尽量给容量提示

`uber-059 / uber-060 / uber-061`

已知大小的 slice / map，在 `make` 时把 size hint 给上——省掉扩容时的反复拷贝。正例（都是「先知道要放多少」的场合）：

```go
p := make([]byte, 4+len(entries)*SegItemLen)          // internal/transport/protocol/protocol.go:417
jobs := make([]device.WriteJob, 0, total)             // internal/transport/batch.go:120
idxByKey := make(map[string]int, len(keys))           // internal/metastore/kv_pebble.go:171
found := make(map[string]ObjectMeta, len(missed))     // internal/metastore/kv_pebble.go:194
keys := make([]string, 0, len(k.m))                   // internal/cluster/kv_mem.go:63
r.events = make([]ioEvent, 0, max)                    // internal/aio/aio_linux.go:185
```

注意 `internal/cluster/kv_mem.go:63` 这一处——`len(k.m)` 是在**持锁状态下取的**，所以这个 hint 的值是可靠的（不持锁读 map 长度会 race，加容量提示之前先确认这一步）。

## 规则 15：相似的声明分组，无关的不分组

`uber-065 / uber-066 / uber-067`

分组的目的是让人**一眼看出这几件事属于同一类**。所以：

- 同类常量 / 变量写进一个 `var (...)` / `const (...)` 块 —— `internal/ierr/ierr.go:9`（6 个错误 sentinel）、`internal/rpcclient/reexport.go:31`（re-export 链）、`internal/cluster/kv.go:24`（4 个 key 前缀常量，同一条注释统辖）。
- **无关的声明不要为了「整齐」硬塞进一个块** —— `internal/bufpool/bufpool.go:23` 与 `internal/device/device.go:31` 的 `const (` 块内都是同一主题的常量。
- 例外（`uber-067`）：**相邻的局部变量**即使无关也应合并声明。

`internal/transport/protocol/protocol.go` 是这条最纠结的实例——同文件里**两种风格并存**：

| 位置 | 形态 |
|------|------|
| `:30`、`:35`、`:40`、`:48`、`:51`、`:54`、`:60`、`:63` | **8 条独立 `const` 声明**，每条各带一段自己的取值理由注释 |
| `:68`、`:102` | 两个 `const (` 分组块 |

那 8 条确实是同类（全是协议线格式的长度常量），但它们**每一条的取值理由都不一样**（`ShmDataPad` 讲 4K 对齐、`InputNodeSize` 讲 O_DIRECT 直读、`SegItemLen` 讲线字节数）。硬并成一个块会让这些理由挤在一起。这是「分组」与「逐条注释」之间的真实取舍。

> ⚠️ **既有例外**：`internal/device/device.go:444-445` 相邻的两个局部变量写成两条独立 `var`，未用 `var (...)` 合并（正是 `uber-067` 点名要合并的情形）：

```go
var specs []pspec                    // :444
var tmps [][]byte                    // :445   补零/对齐临时缓冲，须存活到全部事件取回
```

**判断口径**：以「读者能不能一眼看出这几件事同类」为准，不以形式整齐为准。拿不准时按 `uber-065` 分组。

## 规则 16：包名默认不重命名，别名为避冲突而存在

`uber-069 / uber-070 / uber-071 / uber-072 / uber-073 / uber-074 / uber-077`

**（a）包名形态**（`uber-069` / `uber-071` / `uber-072`）：全小写、无下划线、不用复数、不用 `common`/`util`/`shared`/`lib` 这类无信息量的桶名。全仓 **16 个包名**实测：

```
aio  benchkit  bufpool  cluster  cmd  device  ierr  layout  main
metastore  protocol  rpcclient  storage  taihuclient  transport  version
```

全部满足上述形态。`uber-070` 的「简短」是软要求——最长的是 `taihuclient`（11 字符），它是目录名 `taihu-client` 去连字符的结果（见 [code-style.md](./code-style.md) 命名一节），并非缩写不当。

**（b）函数名用 MixedCaps，不用下划线**（`uber-074`）。如 `func (s *Storage) MaxObjectSize() int64`（`internal/storage/storage.go:57`）。

**（c）导入别名只在冲突时用**（`uber-073` / `uber-077`）。全仓非测试代码**只有一处**导入别名，就是规则 11 讲的 ` tikverr `。**给包起别名会让读者在 `import` 块和调用点之间来回跳**——除非本名不可用（或与另一 import 冲突），不要起。

## 规则 17：文件内声明的顺序服务「读的人怎么找」

`uber-078 / uber-079 / uber-080 / uber-081 / uber-082`

四条上游要求：函数按大致调用顺序排（`078`）、按 receiver 分组（`079`）、导出函数在文件前部（`080`）、`NewXyz` 紧随其类型定义（`081`）、纯工具函数放文件靠后（`082`）。

`internal/storage/storage.go` 是**完全满足**的样板：

| 行 | 内容 | 对应 |
|----|------|------|
| `:19` | `type Storage struct` | 类型先出现 |
| `:31` | `func NewStorage(...)` | `NewXyz` 紧跟类型（`081`） |
| `:57`–`:482` | 全部 `func (s *Storage)` 连续排布，无其他 receiver 插入 | 按 receiver 分组（`079`） |

`internal/transport/server.go` 同样做到了 `NewServer`/`NewServerWithOptions`（`:50`/`:56`）紧跟 `type Server`（`:40`），**但并未严格按 receiver 分组**——`(c *Conn)` 的方法插在 `(s *Server)` 中间：

```
:62  :76  :91  :96  :109   (s *Server) ... Serve/GracefulStop/Stop/serveConn/dispatch
:161 :177                  (c *Conn)  ... endStream/drainPutTail
:202 :297 :364 :391        (s *Server) ... handlePut/handleGet/handleDelete/handleStat
```

这其实是**按调用顺序**排的结果（`dispatch` 分发到 `endStream`，再到各 `handleXxx`），更贴合 `uber-078`。两条上游要求在真实文件里会打架，**这里优先「调用顺序」**。

`internal/transport/protocol/protocol.go` 前半段是「先全部 `Encode*`，再全部 `Parse*`」，后半段转为**成对出现**（`EncodePong` `:326` 紧跟 `ParsePong` `:334`，`EncodeMetaResp` `:350` 紧跟 `ParseMetaResp` `:359`）——成对的写法更利于对照编解码两侧，**推荐后一种**。

`uber-082` 的实例：`internal/metastore/segments.go:384` 的 `iterateMapping` 是纯工具函数，排在文件末尾。

## 规则 18：减少嵌套——先处理错误与特殊分支，提前返回

`uber-083 / uber-084`

正常路径应当留在**最外层缩进**，异常分支提前 `return` / `continue` 掉。这是 taihu 通行的写法：

```go
f, err := openDevice(nvmePath)                                            // internal/device/device.go:89
if err != nil {
	return nil, fmt.Errorf("taihu: open device %q: %w", nvmePath, err)     // :91
}
```

```go
if len(batch) == 0 {                                                      // internal/transport/batch.go:108
	return
}
```

`uber-084` 的细化：**`if` 两个分支都给同一个变量赋值时，就是一个 `if`**。反例：

```go
var endpoint string                              // cmd/taihu/cmd/bench_single.go:82
if c.transport == "rpc" {
	endpoint = fmt.Sprintf("single(rpc:%s)", c.addr)     // :84
} else {
	endpoint = fmt.Sprintf("single(shm:%s)", c.shm)      // :86
}
```

可以写成 `endpoint := fmt.Sprintf("single(%s:%s)", c.transport, c.addrOrShm)` 之类，或至少把赋值提成一条表达式。

> ⚠️ **既有例外**：`cmd/taihu/cmd/bench_single.go:82-87` 一处。本轮不改代码。

## 规则 19：变量声明的形态与作用域

`uber-085 / uber-086 / uber-087 / uber-094 / uber-095 / uber-100 / uber-101 / uber-102`

**（a）顶层变量用标准 `var` 关键字，省略类型**（`085` / `086`）——除非表达式类型与期望类型不一致：

```go
var errConnClosed = errors.New("taihu: connection closed")   // internal/transport/frame.go:34
var pool = newAlignedPool()                                  // internal/bufpool/bufpool.go:44
var exactPool = &exactSizePool{freelist: make(map[int][][]byte)}   // internal/bufpool/bufpool.go:161
var syncWO = &pebble.WriteOptions{Sync: true}                // internal/metastore/kv_pebble.go:65
var uringOverflowOnce sync.Once                              // internal/aio/aio_uring_linux.go:184
```

**（b）局部变量显式赋值用 `:=`，需要强调默认值时用 `var`**（`094` / `095`）。`internal/metastore/kv_pebble.go:171` 的 `idxByKey := make(map[string]int, len(keys))` 是前者；`internal/device/device.go:444-445` 的 `var specs []pspec` 是后者（强调「这是个空 slice，且必须非 nil/必须显式」）。

**（c）尽量缩小作用域**（`100`）。`internal/storage/storage.go:89` 的 `if err := s.PutAppend(...); err != nil` 把 `err` 限制在 `if` 内。

**（d）但结果在 `if` 之外还需要时，就不要缩小**（`101`）：

```go
prefix, err := protocol.ParseKeyReq(first.r)     // internal/transport/server_admin.go:105
```

**（e）常量不必是全局**（`102`）——只在一个函数里用的常量就写在函数里，且要保持靠近使用点：

```go
const maxOps = 256                                       // internal/aio/probe_linux.go:48（函数内）
const blockSize = 4096                                   // internal/bufpool/bufpool.go:142（函数内）
```

> ⚠️ **既有例外，本条范围最大**：`uber-087` 要求**未导出顶层变量/常量加 `_` 前缀**（如 `_errConnClosed`），以便读者一眼看出「这个不对外」。**taihu 完全没采用这个约定**——实测非测试代码里有 **40 处** `var <小写名>` 顶层声明，全部无 `_` 前缀（上表那 5 个都是）。
>
> **为什么不采用**：`_` 前缀在 Go 社区是小众约定，与本仓库既有的 297 条规则、以及 `internal/ierr` 那种「未导出 sentinel 用 `err` 前缀」的习惯都不一致；引入它意味着重命名 40 个符号，且会让 `_foo` 与语言里本来就有的 `_`（blank identifier）在视觉上打架。**新代码沿用现状：不加 `_` 前缀**，靠 `golint` 语义上的「首字母小写即未导出」来判断。

## 规则 20：`nil` slice 与长度 0 的 slice

`uber-096 / uber-097 / uber-098 / uber-099`

**（a）判空一律用 `len(s) == 0`，不与 `nil` 比较**（`097`）。`len(nil slice) == 0` 成立，所以 `len` 覆盖了两种空；而 `s == nil` 只覆盖其中一种，会漏掉「已分配但长度为 0」的情况。

正例：`internal/transport/batch.go:108` 的 `if len(batch) == 0`、`internal/transport/server.go:350`。

反例：

```go
if buf == nil {                          // internal/transport/client.go:178
```

```go
if buf == nil && out == nil {            // internal/transport/client_shm_linux.go:195
```

> 注意 `internal/cluster/capacity.go:29` 的 `if err != nil || v == nil` **不算**违反——那条判的是「键不存在」（TiKV 的 `v` 是 `[]byte` 但语义上是 optional），与「slice 空不空」是两回事。

**（b）`nil` 是合法的长度 0 slice，可以直接用**（`096` / `098`）——`append` 到 `nil` slice 是合法的，不需要先 `make`：

```go
var specs []pspec                 // internal/device/device.go:444
```

```go
return append([]byte(nil), v...), nil     // internal/cluster/kv_mem.go:37（Get 的防御性拷贝）
```

**（c）但 `nil` 与「已分配、长度 0」**在 JSON 序列化等场合**不等价**（`099`）——`nil` slice 序列化成 `null`，长度 0 的序列化成 `[]`。需要后者时显式 `make([]T, 0)`。`internal/cluster/instance.go:13` 的 `Addrs []string` 就是要看住这一点的地方。

## 规则 21：避免裸参数；含义不明显的加注释或用具名类型

`uber-103 / uber-104`

调用点出现 `foo(x, true, false)` 时，读代码的人无法知道那两个布尔值是什么意思。两种解法，按场合选：

**（a）在调用点加 C 风格注释**（`103`）：

```go
recordRxDataFrame(int(rem), true)      // internal/transport/client_shm_linux.go:198
```

该函数的定义处**有**说明（`internal/transport/stats.go:36-37` 的文档注释写明 `zeroCopy 为 true 表示零拷贝移交`），但调用点看不到。加注释即可：

```go
recordRxDataFrame(int(rem), /* zeroCopy */ true)
```

**（b）更好的做法是用具名类型替代裸 `bool`**（`104`）——如果这个 `bool` 在多个调用点反复出现，定义一个 `type zeroCopy bool` 之类，或者把函数拆成两个。

反例里最典型的是平台桩——`internal/aio/aio_other.go:42` 连**参数名**都没有：

```go
func newIOUringRing(int, bool) (Ring, error) {
	return nil, errors.New("aio: io_uring 仅 Linux 支持")
}
```

> ⚠️ **既有例外**：`internal/transport/client_shm_linux.go:198`、`:206`、`:210` 三处裸 bool 实参；`internal/aio/aio_other.go:42` 的匿名参数。本轮不改代码。**新代码：参数含义不明显的，定义时就起名，调用点加 `/* name */` 注释。**

## 规则 22：结构体与容器的初始化形态

`uber-106 / uber-108 / uber-109 / uber-110 / uber-111 / uber-112 / uber-113`

**（a）结构体几乎总应指定字段名**（`106` / `108`）——位置初始化在字段增减时会静默移位：

```go
return options{aioMode: aio.ModeAuto}     // internal/storage/options.go:17
```

用字段名时**省略值为零值的字段**（`108`）；**全字段都是零值时用 `var`**（`109`）：

```go
var info InstanceInfo      // internal/cluster/register.go:54
var c CapacityRecord       // internal/cluster/capacity.go:32
```

**（b）引用类型用 `&T{}` 而非 `new(T)`**（`110`）。`new(T)` 返回 `*T` 且只能得到零值，无法同时初始化字段；`&T{}` 两种都能做，形态也统一。反例：

```go
p.pools[i] = new(bytePool)     // internal/bufpool/bufpool.go:52
bp = new(bytePool)             // internal/bufpool/bufpool.go:129
```

对照正例：`internal/metastore/segments.go:47` 的 `&segmentManager{db: db, segs: make(...), stop: make(...)}`——`new()` 在这里根本做不到。

**（c）固定元素集合用 map 字面量，程序化填充用 `make` 并给 size hint**（`111` / `112` / `113`）：

```go
label := map[string]string{...}     // cmd/taihu/cmd/cluster.go:350   固定集合，字面量
```

```go
byInst := map[string]int{}          // cmd/taihu/cmd/cluster.go:231   随后逐条填充，却用了字面量
```

`cmd/taihu/cmd/cluster.go:231` 应写成 `make(map[string]int, len(instances))` 之类。

> ⚠️ **既有例外**：`internal/bufpool/bufpool.go:52`、`:129` 的 `new(bytePool)`；`cmd/taihu/cmd/cluster.go:231` 的空字面量 map + 程序化填充。本轮不改代码。

## 规则 23：预见会扩展的公开构造器用 Functional Options

`uber-128 / uber-129`

参数已经 3 个或更多、或**预期还会加**的公开构造函数，把可选参数收进 `...Option`，这样新增选项不会破坏既有调用方。

taihu 的两个核心构造器都是这个形态：

```go
func NewDevice(ctx context.Context, nvmePath string, segSize int64, opts ...Option) (*Device, error)      // internal/device/device.go:74
func NewStorage(ctx context.Context, rocksdbDir, nvmePath string, l layout.Layout, opts ...Option) (*Storage, error)   // internal/storage/storage.go:31
```

注意两者的分工：**必填参数仍在签名里**（`nvmePath`、`layout`），只有**可选**的才进 `Option`。不要把所有参数都塞进 options——那会让「这个构造器到底必须给什么」变得不可见。

---

## 刻意偏离上游规则

本节登记「**明确知道上游怎么说、但 taihu 有意不照做**」的条目。三要素缺一不可：上游主张 / taihu 的做法（带锚点）/ 为什么偏离（具体到可检验）。

**不在这节里的「不遵守」不是偏离，是遗漏** —— 写不出可检验理由的，按缺陷处理。

### 1. `Option` 用闭包，不用接口

- **推荐实现方式：声明带未导出方法的 `Option` 接口。相比闭包，接口方式对作者更灵活、对用户更易调试 —— 选项可在测试与 mock 中相互比较（闭包做不到），且可实现其他接口（如 `fmt.Stringer`）。** —— 上游见 `uber-130` / `uber-131`（原文自述是 "Our suggested way" / "we believe"，属推荐而非硬性）。
  **taihu 的做法**与上游**有一半一致**：`options` 是未导出 struct、`Option` 也从构造器收进 `...Option`（规则 23），只有 `Option` 的**载体**是闭包 —— `internal/storage/options.go:7` 与 `internal/device/options.go:7` 都是 `type Option func(*options)`，`WithAIOMode` / `WithAIOIOPoll` 返回的是这个函数类型。
  **为什么偏离**：接口式相对闭包的两条优势（可比较、可实现 `fmt.Stringer`）在本仓库**都用不上** —— 两个包各只有 2 个选项，既没有一处测试需要比较两个选项是否相等，也没有一处要把选项打印出来调试。采纳的代价却是实打实的：要改掉 `Option` 的公开类型定义、`options` 的填充方式，以及 CLI 侧 `aioOptions()`（`cmd/taihu/cmd/root.go:117-125`）的构造路径。**这条偏离是「收益为零、改动公开 API」的典型**，与规则 23 一起看：Functional Options 的**形态**照收，**载体**按本仓库规模选最简的。若将来某个包的选项涨到需要比较或需要 `String()`，再单独把那个包改成接口式，不必全仓统一。

### 2. 测试缝隙用包级函数变量，不用显式注入

- **避免可变全局变量，改用依赖注入；这条同样适用于函数指针等其他类型的值。** —— 上游见 `uber-039`。
  **taihu 的做法**是**刻意保留两个包级可变函数变量**：`cmd/taihu/cmd/helpers.go:63` 与 `pkg/taihu-client/tikv.go:35` 各一个 `var newTiKVKV = func(ctx context.Context, pdAddrs []string, tls cluster.TLSConfig) (cluster.KV, error)`，测试里保存 → 替换 → `t.Cleanup` 恢复。`cmd/taihu/cmd/server.go:111` 的注释写明它的身份：「**`newTiKVKV` 是包级测试缝隙，生产恒为 `cluster.NewTiKVKV`**」。
  **为什么偏离**：这条不是风格偏好，是**测试可否存在**的问题。TiKV 连接路径要在真实集群上才跑得通，而 CLI 与 SDK 层的逻辑（参数解析、校验、退出码）必须能单测 —— 唯一的办法是在「决定连哪里」这个点上留一个可替换的接缝。本仓库一共有两个此类接缝，都是同样的形态：`cmd/taihu/cmd/helpers.go:32` 的 `var kvConnect = connectKVReal`（注释：「生产路径恒为 `connectKVReal`，行为不变」）与同文件 `:63` 的 `newTiKVKV`。

改成显式注入意味着把两个变量提升成参数或字段，一路穿过 `connectKV` 的调用点与 CLI/SDK 装配路径，**改动面远大于收益，且注入本身并不能让测试变得更能发现缺陷** —— 单测里要替换的还是同一个函数值，只是换了个传递方式。代价也认：这两个全局可被并发改写，所以约束是「**只能在测试里换、必须 `t.Cleanup` 还原**」—— 提交 `c5e3c87` 的标题即为此事（「抽出测试缝隙使 CLI/SDK 可单测」）。
  **判据**：新增包级可变变量时，先回答「它是不是一个**只能在测试里被改写**的接缝」。是 → 照上述形态写并留下注释；不是（生产路径上也会读写的可变状态）→ 按上游规则改掉。

---

## 附：不采纳的上游条目

| 上游编号 | 内容 | 不采纳的理由 |
|---------|------|-------------|
| `uber-062` | 行宽软限制 99 字符 | 上游原文自己写明 **「allowed to exceed」**——它是软限制而非规则。仓库里超限最长的 `cmd/taihu/cmd/bench_cluster.go:151`（191 字符）不构成违反。**不单列规则** |

## 附：上游编号 → 本文件规则 对照

| 规则 | 上游编号 |
|------|---------|
| 规则 1 接收者的选择 | `uber-002` `uber-005` `uber-006` |
| 规则 2 接口按值传递 | `uber-001` |
| 规则 3 mutex 不内嵌 | `uber-007` `uber-008` `uber-093` |
| 规则 4 入参防御性拷贝 | `uber-009` |
| 规则 5 资源清理用 defer | `uber-011` `uber-012` |
| 规则 6 channel 缓冲容量 | `uber-013` |
| 规则 7 枚举从 0 开始 | `uber-014` `uber-015` |
| 规则 8 时间走 time 包 | `uber-016` `uber-017` `uber-018` `uber-019` `uber-020` `uber-021` |
| 规则 9 类型断言 comma-ok | `uber-034` |
| 规则 10 内嵌要有意图 | `uber-040` `uber-089` `uber-090` `uber-092` |
| 规则 11 避开预声明标识符 | `uber-041` |
| 规则 12 init() 的约束 | `uber-042` `uber-043` `uber-044` `uber-053` |
| 规则 13 字符串互转与字节化 | `uber-057` `uber-058` `uber-105` |
| 规则 14 容器容量提示 | `uber-059` `uber-060` `uber-061` |
| 规则 15 声明分组 | `uber-065` `uber-066` `uber-067` |
| 规则 16 包名与导入别名 | `uber-069` `uber-070` `uber-071` `uber-072` `uber-073` `uber-074` `uber-077` |
| 规则 17 文件内声明顺序 | `uber-078` `uber-079` `uber-080` `uber-081` `uber-082` |
| 规则 18 减少嵌套 | `uber-083` `uber-084` |
| 规则 19 变量声明与作用域 | `uber-085` `uber-086` `uber-087` `uber-094` `uber-095` `uber-100` `uber-101` `uber-102` |
| 规则 20 nil slice 与空 slice | `uber-096` `uber-097` `uber-098` `uber-099` |
| 规则 21 避免裸参数 | `uber-103` `uber-104` |
| 规则 22 初始化形态 | `uber-106` `uber-108` `uber-109` `uber-110` `uber-111` `uber-112` `uber-113` |
| 规则 23 Functional Options | `uber-128` `uber-129` |

**合计覆盖 75 条上游规则 + 1 条不采纳（`uber-062`）= 76 条**，与研究清单中对撞出的条数一致。
