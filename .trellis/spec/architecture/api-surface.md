# 包对外面：可见性、删留与摆放

> 每个包都有一张对外的脸。这张脸要**窄**（默认私有）、要**真被用**（只被测试用的删掉）、要**能一眼看全**（集中到一个文件，且那个文件里**只有**导出内容）。而判断一个符号该不该留，看的是**可达性**不是**引用计数** —— 「外部没写这个名字」不等于「外部没用它」。

---

## 规则 1：包内标识符默认私有，确有外部调用才导出

`internal/` 下的包，外部模块**本来 import 不了**（Go 语言规则：`github.com/liucxer/taihu/internal/...` 只能被以 `github.com/liucxer/taihu` 为根的包 import；实测 `pkg/` 下 `grep -rn 'internal/aio'` 零命中）。所以这里的「导出」从一开始就不是为了模块外的消费方，只是为了同模块的兄弟包 —— **该导出的集合天然很窄，默认值应当是不导出**。

落到本仓库：后端构造函数一律不导出 —— `newLibAIORing`（`internal/aio/aio_linux.go:56`、`internal/aio/aio_other.go:31`）、`newIOUringRing`（`internal/aio/aio_uring_linux.go:200`、`internal/aio/aio_other.go:42`）；环境变量名 `envMode` 也只是包内常量（`internal/aio/aio_internal.go:15`），运维侧读到的仍是字符串值 `TAIHU_AIO_URING`，导出与否对外无感。

**这条与「测试要能构造」并不冲突**：包内测试用 `package aio` 同包声明（该包 8 个 `_test.go` 全部如此），看得见私有符号。全仓库唯一的外置测试包是 `internal/rpcclient/api_test.go:1` 的 `package rpcclient_test` —— 它存在的**唯一目的**是验证「导出面漏补 alias 会编译失败」（见 [error-model.md](./error-model.md) 的 re-export 链），不是为了访问私有符号。

## 规则 2：只被测试代码引用的符号，删掉，而不是留作私有

私有是「藏起来」，删除是「不存在」—— 后者才能让下一个人不再为它纠结。判据是**只有测试代码在用它**。

实例：`internal/aio` 曾有三个只被包内测试引用的符号，性质都是**测试脚手架而不是实现代码** —— `newRing`（1 行包装 `NewWithOptions(n, Options{Mode: ModeLibAIO})`）、`newWithMode`（1 行包装）、`Mode.string()`（`Mode` 的具名格式）。三个连同 `mode.go` 一并删除；该次收敛的提交对 `internal/aio` 的整体 diffstat 是 +205 / −261（净 −56，`c50473a`）—— 删除量被「16 个导出名并入 `aio.go`」的迁入量抵消了一部分，所以**别拿净行数当收敛成效的指标**。

删的时候要**区分「测试脚手架」与「真覆盖」**，两者在同一批里：

| 测试 | 处置 | 理由 |
|------|------|------|
| `TestNewRing`、`TestModeString` | **整条删除** | 它们测的就是被删的符号本身，符号没了测试自然没有意义 |
| `TestNewWithModeInvalidMaxEvents` | **保留并改写**为 `TestNewWithOptionsInvalidMaxEvents` | 它断言的是 `maxEvents` 边界校验，那是真覆盖，只是原本绕了一层包装调用 —— 改成直接调 `NewWithOptions` 即可 |

> 工作方式：这一条的判据由人给定，且要求**先分析、由人拍板**再动手。分析要给出「从 `internal/<pkg>` 之外是否可达」的逐符号清点，而不是直接开删。

## 规则 3：判据是「可达」，不是「被引用」—— 纯按引用计数删会砍掉活功能

这是本组规则里最容易踩的一条。`internal/aio` 的 16 个导出名里，**有 2 个在生产代码中零引用，但都不能删**：

**（a）配置可达 —— `ModeLibAIO`。** `grep -rn 'aio\.ModeLibAIO'` 在 `internal/aio` 之外无命中，外部没人写过这个名字。但它由 `ParseMode("off")` 返回（`internal/aio/aio.go:118-119`），而 `cmd/taihu/cmd/root.go:118` 调 `aio.ParseMode(global.ioUring)` —— `-io-uring=off` 是 CLI 上公开的取值。名字没被写，**取值被配置到达**。

**（b）返回值可达 —— `Info`。** 同样零引用，但它是导出函数 `Probe()` 的返回类型（`internal/aio/aio.go:207`）。`internal/device/device.go:82-83` 写着：

```go
if o.aioIOPoll && (o.aioMode == aio.ModeIOUring ||
	(o.aioMode == aio.ModeAuto && aio.Probe().Supported)) {
```

`aio.Info` 这个名字一次都没出现，依赖却真实存在 —— 收起来会让 `aio.Probe().Supported` 变成对不可命名类型的字段访问。

**判据**：删之前逐条问「外部能不能通过**配置取值**、通过**返回值/参数类型**、通过**接口满足**到达它」。三条都不成立才是真死代码。作为对照，其余 14 个导出名都有实打实的生产引用点（`internal/device`、`internal/storage`、`cmd/taihu/cmd/root.go`）。

## 规则 4：对外面文件里**只有**导出内容

一个包有一个「对外面文件」—— 想知道这个包对外提供什么，只开这一个文件就够了。而「一眼看全」的前提是**里面没有别的东西**：未导出的常量、变量、辅助函数**都不得与导出名同处这一个文件**，必须按职责挪到别的文件去。

理由不是洁癖。混着放会让两条信息重新纠缠在一起：

- **读的人分不清**。看到 `errInvalidMaxEvents` 得先判断它是不是对外契约的一部分 —— 而它只是 `NewWithOptions` 内部的参数校验哨兵。
- **机械盘点失效**。`grep -E '^(func|type|var|const) [A-Z]'` 这种「这个包对外提供什么」的快速清点，会因为文件里混着私有 helper 而需要人工二次筛选。

**机械判据**（下面两条都无输出才算过）：

```bash
# 1) 行式未导出顶层声明 —— 方法接收者形式也算
grep -nE '^[[:space:]]*(func|type|var|const)[[:space:]]+(\([^)]*\)[[:space:]]*)?[a-z]' internal/aio/aio.go

# 2) 块式声明（var ( / const ( / type ( ）里混进的未导出项
awk '/^\)/{p=0} p && /^[[:space:]]*[a-z]/ {print FILENAME":"FNR": "$0} /^(var|const|type)[[:space:]]*\($/{p=1}' internal/aio/aio.go
```

两条判据已在 `internal/aio/aio.go` 上实测过两端：收敛前报出 7 项（清单见附录），收敛后输出为空。第 2 条**不会**误报只含 `ModeAuto` / `ModeLibAIO` / `ModeIOUring` 的那个导出常量块 —— 这是它唯一的假阳性风险点，写规则时专门验过。

**不算违反的两种情况**：

- **接口实现类型上的导出方法名**（如 `func (r *uringRing) Wait(...)`）。它们必须导出才能满足 `Ring` 接口，而类型本身未导出、包外无人能命名它，本就不属于对外面；何况它们在实现文件里，不在对外面文件里。
- **包文档与 `package` 子句、import 块、注释**。它们不是「内容」，`internal/aio/aio.go:1-26` 的包文档正是本规则的载体之一。

实现文件则按**实现 / 平台 / 职责**分，不按「导出 / 未导出」分：

| 文件 | 职责 |
|------|------|
| `aio.go` | **对外面**：全部导出名的声明与文档（当前 16 个） |
| `aio_linux.go` | libaio 后端（linux） |
| `aio_uring_linux.go` | io_uring 后端（linux） |
| `aio_other.go` | 非 Linux 兜底后端（`!linux`） |
| `probe_linux.go` / `probe_other.go` | 平台相关的探测与校验**实现** |
| `aio_internal.go` | **包内私有支持**：未导出的常量 / 变量 / helper（`envMode`、`errInvalidMaxEvents`、`iopollSuffix`、`logBackend`），平台无关 |
| `probe_cache.go` | **包内私有支持**：`Probe` 的结论缓存与转发，平台无关（见规则 5(a)） |

后缀里既没有 `_linux` 也没有 `_other` 的，就是「平台无关的包内私有」这一类 —— 它们与 `aio.go` 的区别不是导出与否的**偶然**分别，而是职责分别。

> **这是新立的约定，目前只有 `internal/aio` 一个实例（已于 2026-09-21 完全落地，见附录），别把它说成既有全仓惯例。** 实测其余包的对外面仍是散着的：`internal/device` 散在 4 个文件、`internal/transport` 散在 9 个、`internal/cluster` 散在 7 个。**新增包照这条来；已有包不要求追溯改造**，遇到了再收。

## 规则 5：平台分裂的符号 —— 缓存上移到私有文件，对外面只做薄转发

规则 4 落地时会撞上一个硬约束：有些能力天然是**平台分裂**的，实现在 `probe_linux.go` 与 `probe_other.go` 里各有一份（`probe()`、`checkIOPoll()`、`kernelRelease()`、`backendName()`、`ringQueueDepth()`）。这不是冗余，是编译期约束：io_uring 的探测要调 `io_uring_setup`，非 Linux 没有那个系统调用号，**同一份代码放不进一个文件**。

平台分裂不可消除，但**分裂的范围可以收窄** —— 只让真正的差异留在平台文件里，其余归位。按「有没有平台无关的部分」分两种手法：

**（a）缓存上移** —— 平台无关的逻辑搬出平台文件。`Probe()` 的结论缓存（`probeMu` / `probeInfo` / `probeDone`）与操作系统无关，只是当年被复制了两份；上移后平台文件里只剩探测**本身** `probe() (Info, bool)`（`internal/aio/probe_linux.go:19`、`internal/aio/probe_other.go:9`）与各自真正需要的辅助。**顺带消掉了 `Info` 与 `Probe` 在两个平台文件里的重复声明**。

**上移的落点受规则 4 约束**：缓存是未导出的，所以它**不能**落在对外面文件 `aio.go`，而要在包内另立一个**平台无关的私有文件**（后缀既不是 `_linux` 也不是 `_other`）。对外面文件里只留 `Probe` 那一层薄转发 —— 手法与下面 (b) 的 `CheckIOPoll` 完全相同。

**（b）薄转发** —— 整个函数体都是平台相关的，对外面文件只放声明并转发：

```go
// CheckIOPoll 校验目标块设备是否开启了队列级轮询 —— IOPOLL 的前置条件。
// ...
// 实现按平台分文件：Linux 读 sysfs 校验，非 Linux 恒报不支持（见 probe_other.go）。
func CheckIOPoll(devPath string) error {
	return checkIOPoll(devPath)
}
```
（`internal/aio/aio.go:217-219`；实现见 `internal/aio/aio_uring_linux.go:20` 与 `internal/aio/probe_other.go:17`，**报错文案一字未改**）

**取舍**：也曾考虑「直接用 sysfs 实现、删掉非 Linux 存根」—— 代码更少、没有转发层。否掉的理由是错误信息：那样 macOS 上跑 `-io-uring=on -io-uring-iopoll` 会从 `aio: IOPOLL 仅 Linux 支持` 退化成「读取 `/sys/class/block/...` 失败」。**这一层转发换来的是平台正确的报错。**

## 规则 6：边界上不交出内部状态 —— 返回内部 map/slice 必须给副本

上游 `uber-010`：返回内部 map/slice 的引用，等于让调用方绕过锁直接改包内状态，产生数据竞争。

`internal/cluster/kv_mem.go` 是本仓库的正例，**读写两侧都在边界上复制**：

```go
func (k *MemoryKV) Put(_ context.Context, key, value []byte) error {   // :23
	k.mu.Lock()
	k.m[string(key)] = append([]byte(nil), value...)                    // :25  存进去的是副本
	k.mu.Unlock()
	return nil
}

func (k *MemoryKV) Get(_ context.Context, key []byte) ([]byte, error) { // :30
	k.mu.RLock()
	v, ok := k.m[string(key)]
	k.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	return append([]byte(nil), v...), nil                                // :37  取出来的也是副本
}
```

`Scan` 同理（`internal/cluster/kv_mem.go:77` 的 `append([]byte(nil), k.m[key]...)`）。

**规则**：凡返回**可变**的内部容器（slice / map），在返回值处复制一层。**复制点恒在边界**（Get/Put/Scan 这类对外方法），不在内部调用路径上 —— 内部路径复制纯属浪费。

⚠️ 同一个道理在**参数侧**也成立，但那条归 [go-style.md](./go-style.md) 规则 4（「接收 slice / map 并保留引用时，必须复制」）管；本节只管**返回值**这一侧。

**例外**：本仓库的池化缓冲（`bufpool`）刻意不复制 —— 它靠 `(data, release, err)` 三元组的**显式归还契约**替代复制，见 [../engine/buffer-and-concurrency.md](../engine/buffer-and-concurrency.md) 第一节。这是为热路径性能付的代价，**新代码不要拿它当"返回值可以不复制"的先例**。

## 规则 7：接受接口、返回结构体；依赖从构造函数进

上游 `ecc-002`（Accept interfaces, return structs）与 `ecc-011`（依赖注入用构造函数，不用全局）。

**返回值这一半，实测 11 个导出构造函数里 10 个返回具体类型**：

| 返回具体类型 | 位置 |
|--------------|------|
| `*MemoryKV`、`*TiKVKV` | `internal/cluster/kv_mem.go:19`、`internal/cluster/kv_tikv.go:98` |
| `*Conn`、`*Server` | `internal/transport/client.go:21`、`internal/transport/server.go:50,56` |
| `*SliceReader` | `internal/transport/protocol/protocol.go:202` |
| `*Storage`、`*Storage` | `internal/storage/storage.go:31`、`pkg/taihu-client/storage.go:52` |
| `*Device`、`*Compactor` | `internal/device/device.go:74`、`internal/storage/compact.go:47` |
| `*Storage` | `pkg/taihu-client/tikv.go:41` |

**唯一例外**是 `internal/aio/aio.go:136` 的 `NewWithOptions(maxEvents int, o Options) (Ring, error)` —— 它必须返回接口，因为三个后端（libaio / io_uring / 非 Linux 兜底）是三个不同的具体类型，调用方不该知道拿到的是哪个。**这是"有多个实现"的正当例外，不是可以随便返回接口的先例**：新构造函数若有单选的具体类型，返回那个类型。

**接受接口这一半**：`internal/rpcclient/objectstore.go:20-21` 把这条写进了文件——接口签名只用标准库类型，外部实现也能满足它（另见该文件 `:45` 的编译期断言 `var _ ObjectStore = (*Storage)(nil)`）。

**依赖从构造函数进，不用全局**。实测非测试代码共 **59 处包级 `var` 声明**，全都可以归入下面几类，**没有一类是「本该由构造函数传入的依赖」**：

| 类别 | 实例 | 为什么可接受 |
|------|------|--------------|
| 编译期接口断言 | `var _ KV = (*MemoryKV)(nil)`（`internal/cluster/kv_mem.go:16`）、`var _ ByteReader = (*SliceReader)(nil)`（`internal/transport/protocol/protocol.go:192`） | 不是状态，零值即用 |
| sentinel 错误值 | `internal/ierr/ierr.go:9`、`internal/aio/aio.go:38,41`、`internal/device/device.go:48` 等 | 错误值本就该是包级不可变量，见 [error-model.md](./error-model.md) |
| 常量 / 指标 / 名字表 | `internal/layout/layout.go:8`、`internal/version/version.go:13`、`internal/transport/stats.go:14` | 声明即终值 |
| CLI 命令树与 flag 结构 | `cmd/taihu/cmd/server.go:39` 等 12 个 `cobra.Command`、`cmd/taihu/cmd/root.go:25` 的 `global` | 进程入口的既有形态，见下方"例外" |
| 刻意的进程级单例 | `internal/bufpool/bufpool.go:44,161` 的桶池、`internal/metastore/kv_pebble.go:65` 的 `syncWO` | 全局唯一共享且自身有锁保护，见 [../engine/buffer-and-concurrency.md](../engine/buffer-and-concurrency.md) 第二节 |
| 有文档的缓存 | `internal/aio/probe_cache.go:12-16` 的 `probeMu`/`probeInfo`/`probeDone`、`internal/aio/aio_uring_linux.go:184` 的 `uringOverflowOnce` | 缓存/一次性动作，`sync.Once` 或有锁 |
| 测试缝 | `cmd/taihu/cmd/helpers.go:32`、`pkg/taihu-client/tikv.go:35` | 见下 |

依赖一律走构造参数：`NewServer(storage *storage.Storage)`、`NewCompactor(st *Storage, cfg CompactorConfig)`、`NewDevice(ctx, nvmePath, segSize, opts...)`。

**两处例外要说明白，不要假装不存在**：

- **`cmd/taihu/cmd/` 的 `global`（`cmd/taihu/cmd/root.go:25`）是可变的包级状态** —— 它是 cobra 的 flag 绑定目标。**这是进程入口的特权**：CLI 只跑一次、无并发调用方、值在 `PersistentPreRunE` 前就定死。**库代码不要模仿**，`internal/` 与 `pkg/` 里没有同类写法。
- **测试缝**必须是**有注释说明的**包级函数变量（不是状态）：

```go
// kvConnect 是 connectKV 的实际实现。作为测试缝隙抽成包级变量：单测里替换为
// cluster.NewMemoryKV()，使 CLI 命令可在无 TiKV/PD 的环境下端到端跑通；
// 生产路径恒为 connectKVReal，行为不变。
var kvConnect = connectKVReal
```
（`cmd/taihu/cmd/helpers.go:29-32`；`pkg/taihu-client/tikv.go:34-37` 的 `newTiKVKV` 同形）

**新增测试缝照这个形态**：包级函数变量 + 注释写明「生产路径恒为谁、行为不变」。**不写注释的包级可变变量一律视为全局状态，不接受。**

## 规则 8：内嵌不在公开 API 里用

上游 `uber-091` 与 `gbp-038`（后者明确点名「不要在公开 API 里嵌入，应改为显式字段加委托」，并标注该结论来自 Uber Style）。

**这一条不在本文件展开** —— 它讲的是内嵌的意图与落点，已由 [go-style.md](./go-style.md) **规则 10** 完整规定（含全仓仅 2 处内嵌的清点、两处宿主结构体均未导出、以及「新写的内嵌类型放字段列表首位」的既有例外说明）。此处只作索引，避免同一主题两处规定。

---

## 附：`internal/aio` —— 本组规则的参考实现

按各条规则依次收紧的完整过程（2026-09-21）：

| 要求 | 动作 | 结果 |
|------|------|------|
| 默认私有，除非有外部调用 | `New`→`newRing`、`NewWithMode`→`newWithMode`、`EnvMode`→`envMode`、`Mode.String()`→`Mode.string()` | 4 个名字收起 |
| 外部不使用的都删除（先分析后拍板） | 逐符号清点「从包外是否可达」 | 产出规则 3 的两条反例判据 |
| 只被测试代码在用的才删 | 删 `newRing`/`newWithMode`/`Mode.string()` | `TestNewRing`/`TestModeString` 一并删除，`TestNewWithModeInvalidMaxEvents` 改写保留 |
| 对外面集中到一个文件 | 16 个导出名并入 `aio.go`，删 `mode.go` | `aio.go` 99 → 259 行 |
| 对外面文件里**只剩**导出内容 | 未导出项迁往包内私有文件 | 7 项迁出 `aio.go`（219 行），机械判据归零 |

**过程中唯一一次差点删错**：分析时把 io_uring 整块（`aio_uring_linux.go` 一个文件 662 行，占包内 1529 行非测试代码的 43%）摆成「待判断是否可删」的大项，被人一句「io_uring 属于业务在用的嘛」纠正。核实链路后确认它在**生产默认路径**上：`cmd/taihu/cmd/server.go` → `aioOptions()`（`cmd/taihu/cmd/root.go:117`）→ `storage.WithAIOMode` → `device.WithAIOMode` → `aio.NewWithOptions`（`internal/device/device.go:95`），且 `-io-uring` 默认值就是 `auto`（`cmd/taihu/cmd/root.go:37`、`:94`），探测通过即建 io_uring。它在 BCLinux 4.19.90 上回退 libaio 是**那台机器**没走进去，不是链路没用它。

**收敛时迁出的 7 项**（按规则 4 的机械判据清点，迁出后判据归零）：

| 符号 | 性质 | 当时的引用方 | 去向 |
|------|------|--------------|------|
| `errInvalidMaxEvents` | 参数校验哨兵 | 三个后端文件（`aio_linux.go:58`、`aio_other.go:33`、`aio_uring_linux.go:202`） | `aio_internal.go` |
| `envMode` | 环境变量名 | `NewWithOptions`、`mode_test.go` | `aio_internal.go` |
| `iopollSuffix` | 日志拼接 | 仅 `NewWithOptions` 的日志路径 | `aio_internal.go` |
| `logBackend` | 启动日志 | 仅 `NewWithOptions` | `aio_internal.go` |
| `probeMu` / `probeInfo` / `probeDone` | 探测结论缓存 | `Probe`、`mode_test.go` | `probe_cache.go` |

拆成两个文件而不是一个，是因为缓存那组的落点由规则 5(a) 额外约束 —— 它必须与 `probe()` 的平台实现同层，落在**平台无关的私有文件**里；`Probe` 则在 `aio.go` 退化成一行 `return probeCached()`。也就是说：规则 4 决定「未导出项不能留在 `aio.go`」，规则 5 决定「迁到哪个私有文件」。

> 判据只查 `aio.go` 这一个文件，不查包内其余文件 —— 收敛的目标不是「包内没有未导出名」（那不可能），而是「**对外面文件里没有**」。迁出后 `aio_internal.go` 与 `probe_cache.go` 里的未导出项，判据**不会**、也不该报出。

**已知遗留**：`Info` 的 `KernelRelease` / `SQEntries` / `CQEntries` 三个字段，生产代码**只写不读**（启动日志实际调的是 `kernelRelease()` 函数与 `ringQueueDepth(r)`，不是这几个字段），同结构的 `Supported` / `Reason` / `Features` 都有真实读取点。按规则 2、3 的判据它们属于待删项，但当时未作裁定，现状是保留（`internal/aio/aio.go:186-193` 仍是 6 个字段）。

**另一处未裁定的代码缺陷**：`maxEvents` 上界校验在 `internal/aio/aio_linux.go:57` 与 `internal/aio/aio_uring_linux.go:201` 都有，`internal/aio/aio_other.go:32` **只有下界**（`if maxEvents <= 0`）。三处内部表述互相矛盾：错误文案说的是 `[1, 65536]`（`internal/aio/aio_internal.go:18`）、`NewWithOptions` 的文档说「非 Linux 平台忽略上限语义」（`internal/aio/aio.go:135`）、而 `internal/aio/mode_test.go:46` 断言三种模式都该报错。已由 `go test -overlay` 实测确认：把该测试文件临时去掉构建约束后，`TestNewWithOptionsInvalidMaxEvents` 在 darwin 上**确实失败**。（注意 `internal/aio/mode_test.go:1` 带 `//go:build linux`，常规 `go test ./internal/aio/` 在 darwin 上**不编译它**，所以本机跑是绿的 —— 必须 overlay 才看得到这个失败。）当前无生产影响 —— 唯一调用方传的是编译期常量 `aioDepth = 256`（`internal/device/device.go:33`、`:95`）—— 但这是一处真实的语义分裂，待裁定。
