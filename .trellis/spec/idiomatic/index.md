# idiomatic —— Go 惯用法（gbp 类 6）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/idiomatic-*.md`，11 条。
> **11 条全部适用**——这是本 spec 里最没有分歧的一层。

## 逐条裁决

### gbp-029 · Naming Conventions（MEDIUM）— **适用**

**规则**：短小写的包名；缩写大小写一致（`userID` / `httpURL`，**不是** `userId`）；布尔函数用 `Is` / `Has` / `Can` 前缀；常量不用 `MAX_RETRIES` 这种全大写蛇形；单方法接口用 `-er` 后缀。

**对 taihu：适用，当前符合。** 缩写写法实测：

| 写法 | 出现次数 | 判定 |
|---|---|---|
| `ClientID` | 14 | ✅ |
| `ClientId` | **0** | ✅ 没有人写成驼峰变体 |

**要点**：`ID`（26 处）、`JSON`（19 处）、`API`（3 处）这类缩写**一律全大写**。`Id` / `Json` / `Api` 是错。

**包名**：本仓库包名一律小写单词，无下划线、无驼峰。注意**目录名与包名可以不同**：`pkg/taihu-client/` 的包名是 `taihuclient`（目录用连字符、包名不能有连字符）。

### gbp-030 · Comment Guidelines（MEDIUM）— **适用**

**规则**：注释以被描述项开头（`// Request represents...`，**不是** `// This struct represents...`）；错误字符串小写、无句点；删掉无信息量的注释。

**对 taihu：适用，本仓库的做法比规则更严。** 本仓库的注释约定是：

1. **中文**，说明**为什么**而不是复述代码在做什么。
2. 文档注释以**符号名**开头（`// NewStorage 创建...`）。
3. 强调处用 Markdown 粗体（`**唯一事实源**`）。
4. **错误字符串**小写、无结尾句点，与 gbp-030 一致——见 [error/](../error/index.md)。

**「删掉无信息量的注释」在本仓库的具体含义**：`// 增加 i` 这类复述代码的注释不要；但**解释约束、时序、陷阱的注释必须有**。判据是：**删掉这行注释，下一个人会不会踩坑？** 会 → 留着。

本仓库最该留的那类注释的例子：`internal/aio/aio_fallback_other.go:103-107` 那一段（解释为什么**绝不能**用 `os.NewFile(fd)` 包装后再丢——会给调用方持有的同一个 fd 挂 finalizer，GC 一跑就关掉别人的 fd）。

### gbp-031 · Interface Design (Small Interfaces First)（HIGH）— **适用**

**规则**：定义小接口并按需组合；反例是一个 9 方法的大接口；**接口应由消费方定义**；配编译期断言 `var _ io.Reader = (*MyReader)(nil)`。

**对 taihu：适用，且本仓库有大量正例。** 编译期断言实测 8 处：

```go
internal/cluster/kv_tikv.go:30           var _ KV = (*TiKVKV)(nil)
internal/cluster/kv_mem.go:16            var _ KV = (*MemoryKV)(nil)
internal/transport/protocol/protocol.go:191  var _ ByteReader = netpoll.Reader(nil)
internal/transport/protocol/protocol.go:192  var _ ByteReader = (*SliceReader)(nil)
internal/rpcclient/objectstore.go:37     var _ ObjectStore = (*Storage)(nil)
internal/metastore/kv_pebble.go:31       var _ Store = (*pebbleStore)(nil)
pkg/taihu-client/storage.go:49           var _ rpcclient.ObjectStore = (*Storage)(nil)
examples/taihu-client/main.go:40         var _ clusterClient = (*taihuclient.Storage)(nil)
```

**注意 `internal/transport/protocol/protocol.go:191` 那处的形态与其余 7 处不同**：它断言的是**标准库/上游类型** `netpoll.Reader` 满足本地接口 `ByteReader`，其余断言的是本仓库自己的类型满足接口。两者都是断言，但 `netpoll.Reader(nil)` 是**类型转换写法**而非 `(*T)(nil)`——统计时不要把这一处当成「类型转换」误判掉。

**要点**：断言的价值是**把「实现漏了方法」从运行时错误变成编译错误**。新增接口实现时**必须**加这行。

### gbp-032 · Receiver Naming and Selection（HIGH）— **适用**

**规则**：接收者命名 1-2 字母（`c` / `r` / `b`），**同一类型指针/值接收者不要混用**；反例是 `func (this *Consumer)` / `func (self *Reader)`。

**对 taihu：适用，当前符合。** 实测接收者命名分布：

| 字母 | 次数 | | 字母 | 次数 |
|---|---|---|---|---|
| `s` | 114 | | `a` | 12 |
| `c` | 51 | | `p` | 10 |
| `r` | 44 | | `w` | 7 |
| `k` | 30 | | `b` | 6 |
| `m` | 28 | | `it` / `rc` | 5 / 4 |
| `d` | 18 | | （其余） | ≤4 |
| `f` | 13 | | | |

**要点一**：`this` / `self` / `me` **零出现**——不要引入。
**要点二**：同一类型的接收者字母**恒定**。`*Storage` 的方法全用 `s`，`*Client` 全用 `c`；换一个方法改成 `st` 是违规。
**要点三**：同一类型的接收者**不要混用值/指针**。选择依据：要修改状态、含 `Mutex`、结构体较大 → 指针；小不可变值、`map` / `func` / `chan` → 可值接收者。

### gbp-033 · Struct Initialization（MEDIUM）— **适用**

**规则**：结构体初始化**一律用字段名**，不用位置字面量；反例是 `User{"John", "john@example.com", 25, true}` 依赖字段顺序。

**对 taihu：适用，当前符合。** 本 spec 曾记下一处真实偏离，现已订正——`internal/aio/aio_uring_params_linux.go` 原有 17 处位置式字面量，全部补上字段名：

| 原位置 | 条数 | 原形态 |
|---|---|---|
| `internal/aio/aio_uring_params_linux.go`（`uringRingFields`） | 13 | `{"sq_off.head", true, p.SQOff.Head, 4}`（4 字段） |
| `internal/aio/aio_uring_params_linux.go`（`verifyUringRing` 内匿名校验 struct） | 4 | `{"sq_off.ring_mask", u32At(...), p.SQEntries - 1}`（3 字段） |

**它当初为什么是问题**：这些字面量的字段顺序**必须**与 `uringRingField` 的定义一致；一旦有人调整了结构体字段顺序，这 17 处会**静默错位**——字段类型恰好都能兼容时编译器不会报错，运行时的断言就成了假的。而 `aio_uring_params_linux.go` 恰恰是靠这组断言校验 io_uring 结构体布局的，断言本身错了比没有断言更糟。**新增同类表时必须带字段名。**

核实命令：见本文件末尾的「核查脚本」（现应为 0 处）。

### gbp-034 · Functional Options Pattern（HIGH）— **适用**

**规则**：用函数式选项设计灵活的配置 API：`type Option func(*Server)` + `WithHost` / `WithPort` / ... + `NewServer(opts ...Option)`，先设默认值再遍历应用。

**对 taihu：适用，本仓库主流正是这个形态**：

- `internal/storage/storage.go:31` — `NewStorage(ctx, rocksdbDir, nvmePath, l, opts ...Option)`
- `internal/device/device.go:74` — `NewDevice(ctx, nvmePath, segSize, opts ...Option)`

**已知的一处不一致**：`internal/aio` 用的是 `Options` **结构体**（`NewWithOptions(maxEvents int, o Options)`），不是变参 `Option`。

**本 spec 不裁定这个不一致**（两种都是合法设计，结构体形态在参数少且必填时更清晰），但**要知道它是个例外**——新写配置 API 时默认用变参 `Option`，用结构体要能说出理由。

### gbp-035 · defer Usage Guidelines（MEDIUM）— **适用**

**规则**：用 `defer` 做资源释放与解锁；注意 LIFO 顺序、参数在 `defer` 时求值（`defer fmt.Println(i)` 打印的是当时的 `i`）、循环内 `defer` 会累积需抽成独立函数。

**对 taihu：适用。** `defer` 的非测试出现数是 **138**（含测试为 330）——核实命令：`grep -rnE '^[[:space:]]*defer ' --include='*.go' internal cmd pkg | grep -v _test.go`。**注意用这条命令**：`grep -rn 'defer '` 不加行首锚点会把注释与字符串里的 "defer" 也算进去，而 `\b` 在 BSD grep 上不可靠。

**要点一**：`Lock()` 后**立刻** `defer Unlock()`。手写 `Unlock` 只在有**必须提前解锁的分支**时可接受，且每条返回路径都要覆盖——见 [concurrency/](../concurrency/index.md) 的 gbp-027。

**要点二（本仓库踩过的坑）**：`defer` 在**循环内**会累积到函数返回才执行。`internal/aio` 的 `SubmitReadBatch` 一族是循环提交，若在循环里 `defer` 释放资源，会在批大小 4096 时一口气攒 4096 个待释放对象。

**要点三**：用 `t.Cleanup` 而不是 `defer` 的场景见 [testing/](../testing/index.md) 的 gbp-043。

### gbp-036 · Slice and Map Operations（MEDIUM）— **适用（前半）**

**规则**：slice 与 map 的正确用法——`make([]T, 0, n)` 预分配、`copy` 浅拷贝、`if v, ok := m["key"]` 判存在。

**⚠️ 上游这条规则的后半是错的，本 spec 显式剔除。**

上游原文断言「**边遍历边 `delete` 是未定义行为**，需先收集 key」。**这是错的**：Go 语言规范明确允许在 `range` 期间删除元素（只是**新增**的条目不保证被遍历到）。

**为什么必须剔除**：照抄这半句会把本仓库**正确**的代码判成违规——`internal/cluster/kv_mem.go:52`、`internal/aio/aio_fallback_other.go:159`、`internal/device/device.go:161` 都有在 `range` 中 `delete` 的正确写法。

**保留的是前半**：预分配。见 [performance/](../performance/index.md) 的 gbp-048。

### gbp-037 · Zero Value Utilization（MEDIUM）— **适用**

**规则**：利用零值可用性简化代码，并让自定义类型的零值有意义；`sync.Mutex` / `bytes.Buffer` / `WaitGroup` / `Once` 零值即可用；用 `EnableCache` 而不是双重否定 `DisableDisableCache`；`*string` 为 nil 表示未设置。

**对 taihu：适用。**

**要点一**：本仓库大量使用零值可用的并发原语（22 个 `sync.Mutex` / `RWMutex` 字段多数嵌在结构体里，靠零值就绪）。**新增锁字段不要写构造函数初始化它**——那就等于说它的零值不可用。

**要点二**：本仓库的锁多数**不内嵌**而是命名（`mu sync.Mutex`），这一点与 gbp-027 的「不要内嵌」一致。

**要点三**：布尔选项避免双重否定。`Options.IOPoll` 这种正向命名是正例。

### gbp-038 · Type Embedding（MEDIUM）— **适用**

**规则**：用类型嵌入做组合复用，但**不要在公开 API 里嵌入**；反例是公开的 `Client` 嵌入 `http.Client` 泄漏实现，应改为显式字段加委托。

**对 taihu：适用。** 嵌入在**内部实现**里可以（复用方法集），但**导出类型不得嵌入外部类型**——嵌入会把被嵌入类型的**全部方法**提升为导出 API 的一部分，之后想换实现就是破坏性变更。

**正例**：`internal/transport/protocol/protocol.go:191` 的 `var _ ByteReader = netpoll.Reader(nil)` —— 这是**断言** `netpoll.Reader` 满足接口，不是嵌入，两者不要混淆。

### gbp-039 · Blank Identifier Usage（MEDIUM）— **适用**

**规则**：正确使用 `_`：忽略返回值、副作用导入、编译期断言；反例是 `data, _ := json.Marshal(obj)` 危险地吞错，**必须忽略时要加注释说明**。

**对 taihu：适用。** 三种合法用途在本仓库都有：

1. **忽略返回值** —— 必须加理由（同 gbp-019）。
2. **副作用导入** —— `cmd/taihu/cmd/server.go:14` 的 `_ "net/http/pprof"`（注册 pprof 到默认 mux，为副作用）。
3. **编译期断言** —— `var _ KV = (*TiKVKV)(nil)`，见 gbp-031。

**注意**：第 3 种与「忽略返回值」不是一回事，统计 `_ =` 时不要混在一起数——`internal/aio` 的 **12 处** `_ =` 里，**6 处是编译期布局断言**、**6 处是真正的 error 丢弃**：

| 类别 | 数量 | 位置 |
|---|---|---|
| 编译期断言 | 6 | `internal/aio/aio_uring_uapi_linux.go:112-117`，形态 `_ = [1]byte{}[unsafe.Sizeof(ioUringSQE{})-ioUringSQESize]` |
| error 丢弃 | 6 | `unix.Close` ×2：`internal/aio/probe_linux.go:31`、`internal/aio/aio_uring_linux.go:120`。`unix.Munmap` ×4：`internal/aio/aio_uring_linux.go:170`、`internal/aio/aio_uring_linux.go:211`、`internal/aio/aio_uring_linux.go:214`、`internal/aio/aio_uring_linux.go:217` |

**断言那 6 处的形态要注意**：它**不是** `_ = unsafe.Sizeof(...)`（那样写不构成断言，编译器会直接优化掉），而是 `_ = [1]byte{}[size - want]` ——**用数组下标越界把「结构体大小不符」变成编译错误**。用 `grep '_ = unsafe.Sizeof'` 找它们是找不到的（命中 0）。

全仓非测试 `_ = ` 共 **139** 处（`grep -rn '_ = ' --include='*.go' internal pkg cmd | grep -v _test.go`）。

---

## 核查脚本

本层涉及的两类机械核查，用下面的脚本一次跑完（仓库根执行）：

```bash
# gbp-029：缩写写法（应全大写）
for w in ClientID ClientId ServerID ServerId HTTPUrl HTTPURL; do
  printf '%-10s %s\n' "$w" "$(grep -rhoE "\b$w\b" --include='*.go' internal pkg cmd | wc -l)"
done

# gbp-032：接收者命名（应全是 1-2 字母）
grep -rhoE '^func \([a-zA-Z_]+ \*?[A-Za-z]+\)' --include='*.go' internal pkg cmd \
  | grep -v _test | sed -E 's/^func \(([a-zA-Z_]+) .*/\1/' | sort | uniq -c | sort -rn

# gbp-031：编译期断言清单
grep -rn 'var _ [A-Za-z.]* = ' --include='*.go' internal pkg cmd | grep -v _test.go

# gbp-033：位置式结构体字面量（应为 0 处）
# 模式只锚到第一个字段就停 —— `{` 之后可能是 true/false，也可能是 u32At(...)
# 早先写成 `\{"[a-z_.]+", (true|false),` 只能命中 13 处，漏掉匿名校验 struct 那 4 条
grep -rnE '\{"[a-z_.]+", ' --include='*.go' internal cmd pkg | grep -v _test.go
```
