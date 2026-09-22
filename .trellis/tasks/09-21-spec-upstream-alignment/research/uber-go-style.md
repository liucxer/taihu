# Uber Go Style Guide — 规则全量清单

## 1. 文档元信息

| 项 | 值 |
|---|---|
| 来源 URL | https://raw.githubusercontent.com/uber-go/guide/master/style.md |
| 抓取方式 | `curl -sL`（raw 直取成功，全文 4107 行，未分段） |
| 抓取日期 | 2026-09-21 |
| 仓库 HEAD commit | `1d60a91aa5e87d443002e23c21903c49489dbde5`（uber-go/guide，抓取时 master） |
| style.md 最后一次内容变更 | `37c45b52bf9adf36ccff5db54187cdf4d45e0cc9`（2026-04-15，commit message: `Fix typo in atomic.md (#265)`） |
| 文件生成方式 | 声明为 `stitchmd` 自动生成，`src/` 目录才是真源 |
| 顶层章节数（H2） | 6 |
| 二级章节数（H3） | 48 |
| 三级章节数（H4） | 20 |
| 规则总条数 | **134**（编号 `uber-001` ~ `uber-134`，连续无空缺、无重复） |
| 类别分布 | 其他 64 · 命名 16 · 并发 15 · 接口 10 · 错误 10 · 工具链 7 · 性能 6 · panic 4 · defer 2 |
| 严重度标注 | 原文**没有**任何 severity/priority 分级体系。仅有自然语言强调词（must / should not / **粗体** / Exception / 软限制），下表「严重度」列照抄原文强调词，无强调则记「无」。统计：无 73 条；should 11；must 8；should not 4；always 3；Exception 2；其余为一次性措辞 |

### 顶层章节（H2）一览

1. Introduction
2. Guidelines
3. Performance
4. Style
5. Patterns
6. Linting

---

## 2. 规则全量清单

### 2.1 Guidelines

| 编号 | 规则陈述 | 原文小节 | 类别 | 原文严重度 |
|---|---|---|---|---|
| uber-001 | 几乎从不需要指向接口的指针；接口应按值传递（底层数据仍可以是指针） | Pointers to Interfaces | 接口 | 无 |
| uber-002 | 若希望接口方法能修改底层数据，实现侧必须使用指针（值接收者做不到） | Pointers to Interfaces | 接口 | must |
| uber-003 | 在编译期验证接口实现：`var _ Interface = (*T)(nil)`；赋值右侧应为被断言类型的零值（指针/slice/map 用 nil，结构体用空结构体） | Verify Interface Compliance | 接口 | 无（"where appropriate"） |
| uber-004 | 需要做编译期接口校验的场景：作为 API 契约要求实现特定接口的导出类型、实现同一接口的类型集合中的类型、违反接口会破坏使用者的其他情况 | Verify Interface Compliance | 接口 | 无 |
| uber-005 | 值接收者的方法在值和指针上都能调用；指针接收者的方法只能在指针或可寻址值上调用（map 中元素不可寻址），因此 map 里存值就无法调用指针接收者方法 | Receivers and Interfaces | 接口 | 无（描述性） |
| uber-006 | 接口可被指针满足，即使方法用值接收者；但仅有指针接收者方法时，值无法满足接口 | Receivers and Interfaces | 接口 | 无（描述性） |
| uber-007 | `sync.Mutex`/`sync.RWMutex` 的零值即有效，几乎从不需要指向 mutex 的指针 | Zero-value Mutexes are Valid | 并发 | 无 |
| uber-008 | 若结构体以指针使用，mutex 应为其上的非指针字段；不要内嵌 mutex，即使结构体未导出 | Zero-value Mutexes are Valid | 并发 | 无（"Do not"） |
| uber-009 | 接收 slice/map 参数时，若保存其引用就必须复制，否则调用方后续修改会污染内部状态 | Copy Slices and Maps at Boundaries | 其他 | 无（"be wary"） |
| uber-010 | 返回内部 map/slice 时必须返回副本，否则暴露的内部状态绕过了锁保护，会产生数据竞争 | Copy Slices and Maps at Boundaries | 并发 | 无 |
| uber-011 | 用 defer 清理资源（文件、锁等） | Defer to Clean Up | defer | 无（"Use defer"） |
| uber-012 | defer 开销极小，只有在能证明函数执行时间在纳秒量级时才应避免使用；可读性收益大于成本 | Defer to Clean Up | defer | 无 |
| uber-013 | channel 大小通常应为 1 或无缓冲（0）；其他大小必须经受高度审视（如何确定大小、什么能防止满载阻塞写入方、阻塞时怎么办） | Channel Size is One or None | 并发 | must be subject to a high level of scrutiny |
| uber-014 | 枚举通常应从非零值开始（`iota + 1`），因为变量默认值为 0 | Start Enums at One | 其他 | 无（"usually"） |
| uber-015 | 零值即期望的默认行为时，枚举可以从 0 开始 | Start Enums at One | 其他 | 无（例外说明） |
| uber-016 | 处理时间一律使用 `"time"` 包，不要自己假设「一天 24 小时 / 一小时 60 分 / 一年 365 天」 | Use `"time"` to handle time | 其他 | always |
| uber-017 | 时间点用 `time.Time`；比较、加减时间用 `time.Time` 上的方法 | Use `time.Time` for instants of time | 其他 | 无 |
| uber-018 | 时间段用 `time.Duration` | Use `time.Duration` for periods of time | 其他 | 无 |
| uber-019 | 与外部系统交互时尽量使用 `time.Duration` 和 `time.Time`（flag 支持 Duration；JSON/SQL/YAML 支持 Time） | Use `time.Time` and `time.Duration` with external systems | 其他 | 无（"when possible"） |
| uber-020 | 无法在这些交互中使用 `time.Duration` 时，用 `int`/`float64` 并把单位写进字段名（如 `IntervalMillis`） | Use `time.Time` and `time.Duration` with external systems | 命名 | 无 |
| uber-021 | 无法使用 `time.Time` 时，除非另有约定，用 `string` 并按 RFC 3339 格式化时间戳 | Use `time.Time` and `time.Duration` with external systems | 其他 | 无 |
| uber-022 | 选错误声明方式的决策表：不需匹配+静态→`errors.New`；不需匹配+动态→`fmt.Errorf`；需匹配+静态→顶层 `var` + `errors.New`；需匹配+动态→自定义 `error` 类型 | Error Types | 错误 | 无 |
| uber-023 | 从包中导出错误变量或类型时，它们会成为该包公开 API 的一部分，需慎重 | Error Types | 错误 | 无 |
| uber-024 | 传播错误有三种选择：原样返回 / 用 `fmt.Errorf` + `%w` 添加上下文 / 用 `fmt.Errorf` + `%v` 添加上下文 | Error Wrapping | 错误 | 无 |
| uber-025 | 没有额外上下文可加时原样返回错误，保持原始错误类型与消息 | Error Wrapping | 错误 | 无 |
| uber-026 | 用 `%w` 当调用方需要访问底层错误；这是多数包装错误的良好默认；此时应把被包装的错误作为函数契约的一部分文档化并测试 | Error Wrapping | 错误 | good default for most |
| uber-027 | 用 `%v` 来遮蔽底层错误（调用方无法匹配，日后可改回 `%w`） | Error Wrapping | 错误 | 无 |
| uber-028 | 为返回的错误添加上下文时保持简洁，避免 "failed to" 这类显而易见的堆叠短语 | Error Wrapping | 错误 | 无（"keep ... succinct"） |
| uber-029 | 错误被送到其他系统时应让人一眼看出是错误（如日志里的 `err` 标签或 "Failed" 前缀） | Error Wrapping | 错误 | 无 |
| uber-030 | 全局错误值用 `Err`/`err` 前缀（视是否导出）；此指引优先于「未导出全局加 `_` 前缀」 | Error Naming | 命名 | 无（"supersedes"） |
| uber-031 | 自定义错误类型用 `Error` 后缀 | Error Naming | 命名 | 无 |
| uber-032 | 每个错误通常只应处理一次；例如不要既 log 又 return，因为上游调用方可能还会处理 | Handle Errors Once | 错误 | 无（"should typically"） |
| uber-033 | 错误处理方式包括：契约定义了特定错误则用 `errors.Is`/`errors.As` 匹配并分支处理；可恢复则 log 并优雅降级；属于领域失败条件则返回定义良好的错误；否则包装或原样返回 | Handle Errors Once | 错误 | 无 |
| uber-034 | 类型断言一律使用 "comma ok" 形式；单返回值形式在类型不符时会 panic | Handle Type Assertion Failures | panic | always |
| uber-035 | 生产环境代码必须避免 panic（panic 是级联故障的主要来源）；出错应返回 error 让调用方决定如何处理 | Don't Panic | panic | must |
| uber-036 | panic/recover 不是错误处理策略；只有在发生不可恢复的事情（如 nil 解引用）时才应 panic；程序初始化阶段失败可以 panic | Don't Panic | panic | 无 |
| uber-037 | 即使在测试中也应优先用 `t.Fatal`/`t.FailNow` 而非 panic，以确保测试被标记为失败 | Don't Panic | panic | 无（"prefer"） |
| uber-038 | 原子操作使用 `go.uber.org/atomic` 而非 `sync/atomic`——后者裸类型容易忘记用原子操作读写，前者提供类型安全并含 `atomic.Bool` | Use go.uber.org/atomic | 并发 | 无 |
| uber-039 | 避免可变全局变量，改用依赖注入；这条同样适用于函数指针等其他类型的值 | Avoid Mutable Globals | 其他 | 无 |
| uber-040 | 避免在公开结构体中内嵌类型（会泄漏实现细节、阻碍类型演进、混淆文档）；只手写需要委派给抽象类型的那些方法 | Avoid Embedding Types in Public Structs | 其他 | 无 |
| uber-041 | 不要用 Go 预声明标识符（内置名）作为名字：轻则遮蔽原标识符，重则引入难以 grep 的潜伏 bug | Avoid Using Built-In Names | 命名 | should not |
| uber-042 | 尽量避免 `init()`；不可避免时它应当：1) 无论环境或调用方式都完全确定；2) 不依赖其他 `init()` 的顺序或副作用；3) 不访问/操纵全局或环境状态（机器信息、环境变量、工作目录、程序参数等）；4) 不做 I/O（文件系统、网络、系统调用） | Avoid `init()` | 其他 | Avoid / "should attempt to" |
| uber-043 | 无法满足上述要求的代码应作为 `main()` 的辅助函数（或程序生命周期中别处调用）或直接写在 `main()` 里；供其他程序使用的库尤其不应做 "init magic" | Avoid `init()` | 其他 | 无 |
| uber-044 | `init()` 可能更可取/必要的场景：无法写成单次赋值的复杂表达式；可插拔钩子（`database/sql` dialect、编码类型注册表等）；Cloud Functions 等的确定性预计算优化 | Avoid `init()` | 其他 | 无（例外列举） |
| uber-045 | 只在 `main()` 中调用 `os.Exit` 或 `log.Fatal*`；其他所有函数都应返回 error 表示失败 | Exit in Main | 其他 | **only in `main()`** |
| uber-046 | 尽量在 `main()` 中至多调用一次 `os.Exit`/`log.Fatal`；多个会导致程序终止的错误场景应抽到单独函数并返回 error | Exit Once | 其他 | **at most once** |

### 2.2 Guidelines（续）

| 编号 | 规则陈述 | 原文小节 | 类别 | 原文严重度 |
|---|---|---|---|---|
| uber-047 | 被序列化进 JSON/YAML 等支持 tag 命名的格式的结构体字段，都应标注相应 tag（序列化形式是系统间契约，改名会破坏契约） | Use field tags in marshaled structs | 其他 | should |
| uber-048 | 不要 fire-and-forget goroutine：goroutine 虽轻量但非免费（栈内存 + 调度 CPU），不受控地大量启动会导致性能问题，且会阻止对象被 GC 与资源释放 | Don't fire-and-forget goroutines | 并发 | 无 |
| uber-049 | 每个 goroutine 必须：要么有可预测的停止时间，要么有信号能通知它停止；两种情况都须有办法阻塞等待其结束 | Don't fire-and-forget goroutines | 并发 | must |
| uber-050 | 用 `go.uber.org/goleak` 在可能启动 goroutine 的包里做 goroutine 泄漏测试 | Don't fire-and-forget goroutines | 工具链 | 无 |
| uber-051 | 等待多个 goroutine 用 `sync.WaitGroup` + `wg.Wait()` | Wait for goroutines to exit | 并发 | 无 |
| uber-052 | 只有一个 goroutine 需要等待时，用一个 `chan struct{}`，goroutine 结束时 `close(done)`，调用方 `<-done` | Wait for goroutines to exit | 并发 | 无 |
| uber-053 | `init()` 中不应启动 goroutine | No goroutines in `init()` | 并发 | should not |
| uber-054 | 若包需要后台 goroutine，必须暴露一个负责管理其生命周期的对象，该对象必须提供 `Close`/`Stop`/`Shutdown` 之类的方法来通知后台 goroutine 停止并等待其退出 | No goroutines in `init()` | 并发 | must |
| uber-055 | worker 管理多个 goroutine 时应使用 `WaitGroup` | No goroutines in `init()` | 并发 | 无 |

### 2.3 Performance

| 编号 | 规则陈述 | 原文小节 | 类别 | 原文严重度 |
|---|---|---|---|---|
| uber-056 | 性能相关指引只适用于热路径 | Performance | 性能 | 无（"apply only to the hot path"） |
| uber-057 | 基本类型与字符串互转时 `strconv` 比 `fmt` 快，优先用 `strconv` | Prefer strconv over fmt | 性能 | 无（"Prefer"） |
| uber-058 | 不要对固定字符串反复做 string→[]byte 转换；转换一次并保存结果复用 | Avoid repeated string-to-byte conversions | 性能 | 无（"Do not"） |
| uber-059 | 尽量指定容器容量以便一次性预分配内存，减少后续因扩容/拷贝产生的分配 | Prefer Specifying Container Capacity | 性能 | 无（"where possible"） |
| uber-060 | 用 `make()` 初始化 map 时尽量给容量提示 `make(map[T1]T2, hint)`；注意 map 容量只是近似桶数的提示，不保证完全预分配 | Specifying Map Capacity Hints | 性能 | 无 |
| uber-061 | 用 `make()` 初始化 slice（尤其在 append 场景）时尽量给容量 `make([]T, length, capacity)`；与 map 不同，slice 容量是硬保证，后续 append 零分配（直到 len 触达 cap） | Specifying Slice Capacity | 性能 | 无 |

### 2.4 Style

| 编号 | 规则陈述 | 原文小节 | 类别 | 原文严重度 |
|---|---|---|---|---|
| uber-062 | 避免需要读者横向滚动或频繁转头的过长行；建议软限制 **99 字符**，作者应尽量在触达前换行，但这不是硬限制，允许超限 | Avoid overly long lines | 其他 | soft limit / **not a hard limit** / "allowed to exceed" |
| uber-063 | 最重要的是**保持一致**；一致代码更易维护、推理和迁移；同一代码库内多种风格会带来维护开销、不确定性和认知失调 | Be Consistent | 其他 | **be consistent** |
| uber-064 | 将规范应用到代码库时建议在包或更大粒度上变更；在子包粒度应用会往同一份代码里引入多种风格 | Be Consistent | 其他 | recommended |
| uber-065 | 相似的声明应分组（import、const、var、type 均适用），函数内也可使用分组 | Group Similar Declarations | 其他 | 无（"supports"） |
| uber-066 | 只分组相关的声明，不要分组无关声明 | Group Similar Declarations | 其他 | "Do not" |
| uber-067 | 例外：变量声明（尤其函数内）若与其他变量相邻声明，即使互不相关也应分组在一起 | Group Similar Declarations | 其他 | Exception |
| uber-068 | import 应分两组：标准库 / 其他（goimports 的默认分组行为） | Import Group Ordering | 其他 | should |
| uber-069 | 包名应全小写、无大写无下划线 | Package Names | 命名 | 无 |
| uber-070 | 包名应简短精炼（名字在每个调用点都会被看到） | Package Names | 命名 | 无 |
| uber-071 | 包名不应是复数（如 `net/url` 而非 `net/urls`） | Package Names | 命名 | 无 |
| uber-072 | 包名不应叫 "common"、"util"、"shared"、"lib"——这些是无信息量的坏名字 | Package Names | 命名 | 无 |
| uber-073 | 包名应不需要在多数调用点通过命名导入重命名 | Package Names | 命名 | 无 |
| uber-074 | 函数名遵循 Go 社区惯例使用 MixedCaps | Function Names | 命名 | 无（"We follow"） |
| uber-075 | 测试函数例外：可含下划线用于分组相关用例，如 `TestMyFunction_WhatIsBeingTested` | Function Names | 命名 | 例外 |
| uber-076 | 若包名与导入路径最后一段不匹配，必须使用导入别名 | Import Aliasing | 命名 | must |
| uber-077 | 其他场景应避免导入别名，除非 import 之间存在直接冲突 | Import Aliasing | 命名 | 无（"should be avoided"） |
| uber-078 | 函数应按大致调用顺序排序 | Function Grouping and Ordering | 其他 | should |
| uber-079 | 文件内的函数应按 receiver 分组 | Function Grouping and Ordering | 其他 | should |
| uber-080 | 导出函数应出现在文件前部（在 `struct`/`const`/`var` 定义之后） | Function Grouping and Ordering | 其他 | should |
| uber-081 | `newXYZ()`/`NewXYZ()` 可出现在类型定义之后、该 receiver 其余方法之前 | Function Grouping and Ordering | 其他 | may |
| uber-082 | 由于函数按 receiver 分组，纯工具函数应放在文件靠后位置 | Function Grouping and Ordering | 其他 | should |
| uber-083 | 代码应尽可能减少嵌套：先处理错误/特殊分支，提前 `return` 或 `continue`，减少多层嵌套的代码量 | Reduce Nesting | 其他 | should |
| uber-084 | 若变量在 if 两个分支中都被赋值，可改写为单个 if（消除不必要的 else） | Unnecessary Else | 其他 | 无 |
| uber-085 | 顶层变量使用标准 `var` 关键字 | Top-level Variable Declarations | 其他 | 无（"use"） |
| uber-086 | 顶层变量不要写类型，除非表达式类型与期望类型不一致 | Top-level Variable Declarations | 其他 | "Do not ... unless" |
| uber-087 | 未导出的顶层 `var` 和 `const` 加 `_` 前缀，使它们在别处被使用时一眼看出是全局符号 | Prefix Unexported Globals with _ | 命名 | 无 |
| uber-088 | 例外：未导出的错误值可用 `err` 前缀而不加下划线（见 Error Naming） | Prefix Unexported Globals with _ | 命名 | **Exception** |
| uber-089 | 内嵌类型应放在结构体字段列表的最前面，且内嵌字段与普通字段之间必须有空行分隔 | Embedding in Structs | 其他 | should / **must** |
| uber-090 | 内嵌应提供实在的好处（以语义恰当的方式增加或增强功能），且不得有对用户不利的副作用 | Embedding in Structs | 其他 | should |
| uber-091 | 内嵌**不应**：纯为美观/便利；让外层类型更难构造或使用；影响外层类型的零值（外层零值有用则内嵌后仍应有用）；把无关函数或字段作为副作用暴露到外层；暴露未导出类型；影响外层类型的拷贝语义；改变外层类型的 API 或类型语义；内嵌非规范形式的内层类型；暴露外层类型的实现细节；让用户能观察或控制类型内部；通过包装以一种会让用户合理感到意外的方式改变内层函数的行为 | Embedding in Structs | 其他 | **should not** |
| uber-092 | 内嵌要自觉且有意图。试金石：「内层所有导出的方法/字段，是否都会直接加到外层类型上」——答案是「一部分」或「否」就不要内嵌，改用字段 | Embedding in Structs | 其他 | 无（判断标准） |
| uber-093 | 例外：mutex 不应被内嵌，即使在外层类型未导出时也一样 | Embedding in Structs | 并发 | Exception |
| uber-094 | 局部变量若被显式赋某个值，应使用短变量声明 `:=` | Local Variable Declarations | 其他 | should |
| uber-095 | 需要强调默认值更清晰时使用 `var` 关键字，例如声明空 slice 用 `var filtered []int` 而非 `filtered := []int{}` | Local Variable Declarations | 其他 | 无 |
| uber-096 | `nil` 是合法的长度 0 的 slice，因此不要显式返回长度 0 的 slice，应返回 `nil` | nil is a valid slice | 其他 | should not |
| uber-097 | 判断 slice 是否为空一律用 `len(s) == 0`，不要与 `nil` 比较 | nil is a valid slice | 其他 | always |
| uber-098 | 零值 slice（用 `var` 声明的）无需 `make()` 即可直接使用 | nil is a valid slice | 其他 | 无 |
| uber-099 | 注意 nil slice 与已分配的长度 0 slice 并不等价（一个是 nil 另一个不是），某些场景下行为不同（如序列化） | nil is a valid slice | 其他 | 无（提醒） |
| uber-100 | 尽可能缩小变量与常量的作用域；但如果与「减少嵌套」冲突，则不要缩小 | Reduce Scope of Variables | 其他 | "Where possible" / "Do not ... if it conflicts" |
| uber-101 | 若函数调用的结果在 if 之外还需要使用，就不应试图缩小作用域 | Reduce Scope of Variables | 其他 | should not |
| uber-102 | 常量不必是全局的，除非被多个函数或文件使用，或属于包的对外契约 | Reduce Scope of Variables | 其他 | 无 |
| uber-103 | 避免裸参数：函数调用中参数含义不明显时，加 C 风格注释 `/* ... */` 标注参数名 | Avoid Naked Parameters | 其他 | 无 |
| uber-104 | 更好的做法是用自定义类型替代裸 `bool` 参数，代码更可读且类型安全，且将来该参数可以有超过两种状态 | Avoid Naked Parameters | 其他 | Better yet |
| uber-105 | 使用原始字符串字面量（反引号）避免手工转义（手工转义的字符串远更难读） | Use Raw String Literals to Avoid Escaping | 其他 | 无 |
| uber-106 | 初始化结构体时几乎总应指定字段名（现已由 `go vet` 强制） | Use Field Names to Initialize Structs | 其他 | "almost always" |
| uber-107 | 例外：测试表中字段数为 3 或更少时可以省略字段名 | Use Field Names to Initialize Structs | 其他 | **Exception: *may*** |
| uber-108 | 用字段名初始化结构体时，应省略零值字段（让 Go 自动置零），除非它们提供了有意义的上下文 | Omit Zero Value Fields in Structs | 其他 | should / "unless" |
| uber-109 | 当结构体所有字段都被省略时，用 `var` 形式声明（`var user User` 而非 `user := User{}`） | Use `var` for Zero Value Structs | 其他 | 无 |
| uber-110 | 初始化结构体引用用 `&T{}` 而非 `new(T)`，以与结构体初始化保持一致 | Initializing Struct References | 其他 | 无（"Use ... instead of"） |

### 2.5 Style（续）

| 编号 | 规则陈述 | 原文小节 | 类别 | 原文严重度 |
|---|---|---|---|---|
| uber-111 | 空 map 和程序化填充的 map 优先用 `make(..)` 初始化：让 map 的初始化与声明在视觉上可区分，也便于日后加上 size hint | Initializing Maps | 其他 | Prefer |
| uber-112 | 若 map 持有固定的一组元素，用 map 字面量初始化 | Initializing Maps | 其他 | 无 |
| uber-113 | 基本经验：初始化时加入固定元素集合用 map 字面量，否则用 `make`（并尽量给 size hint） | Initializing Maps | 其他 | rule of thumb |
| uber-114 | 若在 Printf 系列函数的字符串字面量之外声明格式字符串，应将其定义为 `const`，以便 `go vet` 做静态分析 | Format Strings outside Printf | 工具链 | 无（"make them const"） |
| uber-115 | 声明 Printf 风格函数时应确保 `go vet` 能识别并检查其格式串：尽量用预定义的 Printf 风格函数名 | Naming Printf-style Functions | 工具链 | should |
| uber-116 | 若无法使用预定义名，自定义的函数名应以 `f` 结尾（如 `Wrapf` 而非 `Wrap`），`go vet` 才能被要求检查（如 `go vet -printfuncs=wrapf,statusf`） | Naming Printf-style Functions | 工具链 | must |

### 2.6 Patterns

| 编号 | 规则陈述 | 原文小节 | 类别 | 原文严重度 |
|---|---|---|---|---|
| uber-117 | 当被测系统需要在多组输入/输出条件下测试时，应使用表驱动测试（配合 subtests）以减少重复、提升可读性 | Test Tables | 其他 | should |
| uber-118 | 约定：struct slice 命名为 `tests`，每个用例命名为 `tt`；鼓励用 `give`/`want` 前缀显式命名每个用例的输入与期望输出 | Test Tables | 命名 | 约定 / encourage |
| uber-119 | 表测试在 subtest 内需要复杂或条件逻辑（如条件断言、分支）时**不应**使用；这类大而复杂的表测试应拆成多个测试表或多个独立 `Test...` 函数 | Avoid Unnecessary Complexity in Table Tests | 其他 | **should NOT** |
| uber-120 | 表测试的目标：聚焦最窄的行为单元；最小化 "test depth"；避免条件断言；确保所有表字段在每个测试中都被使用；确保所有测试逻辑对所有表用例都执行 | Avoid Unnecessary Complexity in Table Tests | 其他 | ideals to aim for |
| uber-121 | "test depth" 指一次测试中需要前面断言成立才能继续的连续断言数量（类似圈复杂度）；"shallower" 的测试断言间关系更少，更不容易默认变成有条件断言 | Avoid Unnecessary Complexity in Table Tests | 其他 | 无（定义） |
| uber-122 | 表测试若使用多条分支路径（如 `shouldError`、`expectCall`）、大量针对特定 mock 期望的 `if`（如 `shouldCallFoo`）、或把函数放进表里（如 `setupMocks func(*FooMock)`），会变得混乱难读 | Avoid Unnecessary Complexity in Table Tests | 其他 | 无（反例列举） |
| uber-123 | 当测试的行为仅随输入变化而变化时，把相似用例聚在同一个表测试里可能更好，能更好体现行为随输入的变化，而不是拆成难以对比的独立测试 | Avoid Unnecessary Complexity in Table Tests | 其他 | may be preferable |
| uber-124 | 若测试体简短直接，允许用一个 `shouldErr` 之类的表字段来区分成功/失败的单条分支路径 | Avoid Unnecessary Complexity in Table Tests | 其他 | acceptable |
| uber-125 | 在表测试 vs 独立测试之间取舍时，没有严格准则，但可读性与可维护性应始终放在首位 | Avoid Unnecessary Complexity in Table Tests | 其他 | **no strict guidelines** |
| uber-126 | 并行测试（以及那些在循环体内启动 goroutine 或捕获引用的特殊循环）必须显式在循环作用域内赋值循环变量，以确保其持有期望值 | Parallel Tests | 并发 | must |
| uber-127 | 用了 `t.Parallel()` 时必须声明一个作用域限定在本次循环迭代的 `tt` 变量；否则多数或全部测试会拿到非期望值，或拿到运行中会变化的值 | Parallel Tests | 并发 | must |
| uber-128 | 对构造器等预见到将来需要扩展的公开 API 的可选参数，使用 Functional Options 模式 | Functional Options | 接口 | 无（"Use this pattern for"） |
| uber-129 | 当函数参数已有 3 个或更多时，尤其应使用 Functional Options 模式 | Functional Options | 接口 | especially if |
| uber-130 | 推荐实现方式：声明带未导出方法的 `Option` 接口，把选项记录在未导出的 `options` struct 上 | Functional Options | 接口 | 建议（"Our suggested way"） |
| uber-131 | 相比闭包实现方式，更推荐上述 `Option` 接口方式：对作者更灵活，对用户更易调试与测试——选项可在测试与 mock 中相互比较（闭包做不到），且选项可实现其他接口（如 `fmt.Stringer` 提供可读字符串表示） | Functional Options | 接口 | 建议（"we believe"） |

### 2.7 Linting

| 编号 | 规则陈述 | 原文小节 | 类别 | 原文严重度 |
|---|---|---|---|---|
| uber-132 | 比选用哪套「受认可」的 linter 更重要的是：在整个代码库中一致地 lint | Linting | 工具链 | 无（"More importantly than"） |
| uber-133 | 至少推荐使用这几个 linter：`errcheck`（确保错误被处理）、`goimports`（格式化并管理 import）、`revive`（指出常见风格错误）、`govet`（分析常见错误）、`staticcheck`（各类静态分析检查） | Linting | 工具链 | We recommend ... at a minimum |
| uber-134 | 推荐用 `golangci-lint` 作为 Go 代码的 lint runner（在大型代码库中性能好、可一次配置并使用多个规范 linter）；仓库提供示例 `.golangci.yml` 作为基线，鼓励团队按需增加 linter | Lint Runners | 工具链 | We recommend |

---

## 3. 带代码示例的规则（正例 / 反例在讲什么）

> 原文的代码示例绝大多数以 `<table>` 的 **Bad / Good** 两列并排给出（少数是 No error matching / Error matching、Description / Code 等其他列名）。下表概述每个示例的正反例在讲什么，不抄代码全文。

| 原文小节 | 反例（Bad）在讲什么 | 正例（Good）在讲什么 |
|---|---|---|
| Pointers to Interfaces | —（无 Bad/Good 表） | 说明接口的两个字段组成与「方法需修改底层数据时必须用指针」 |
| Verify Interface Compliance | `Handler` 类型未做编译期接口校验，停止匹配 `http.Handler` 时无法在编译期发现 | 加 `var _ http.Handler = (*Handler)(nil)` 后，一旦不匹配就编译失败；另给出结构体类型（非指针）用空结构体做右侧零值的写法 |
| Receivers and Interfaces | —（无 Bad/Good 表） | 两个示例：map 中存值无法调用指针接收者方法（`sVals[1].Write` 不可编译）；仅有指针接收者的类型，其值无法满足接口（`i = s2Val` 不可编译） |
| Zero-value Mutexes are Valid | `new(sync.Mutex)` 后再 Lock；以及内嵌 `sync.Mutex` 导致 `Lock`/`Unlock`/`Mutex` 意外成为 `SMap` 导出 API 的一部分 | 用 `var mu sync.Mutex`；以及把 mutex 写成非指针命名字段 `mu sync.Mutex`，把锁变成对外隐藏的实现细节 |
| Copy Slices and Maps — Receiving | `SetTrips` 直接保存传入 slice 的引用，调用方改 `trips[0]` 会污染 `d1.trips` | 先用 `make` + `copy` 复制再保存，之后调用方随便改都不影响内部状态 |
| Copy Slices and Maps — Returning | `Snapshot()` 直接返回内部 map，快照脱离了 mutex 保护，访问它会数据竞争 | 在锁内 `make` 一个 map 逐项拷贝后返回，快照是副本 |
| Defer to Clean Up | 多个 return 分支手工 `p.Unlock()`，容易漏掉解锁 | `p.Lock(); defer p.Unlock()`，可读性更好 |
| Channel Size is One or None | `make(chan int, 64)`，注释「应该够任何人用了」 | 大小为 1 或有缓冲为 0 的无缓冲 channel |
| Start Enums at One | `iota` 从 0 开始，ADD=0 与零值混淆 | `iota + 1`，Add=1；另给出零值即期望默认行为时（LogToStdout=0）从 0 开始的示例 |
| Use time.Time for instants | `isActive(now, start, stop int)` 用整数比较时间 | 参数改为 `time.Time`，用 `Before`/`Equal` 比较 |
| Use time.Duration for periods | `poll(delay int)` 调用方看不出是秒还是毫秒 | 参数改为 `time.Duration`，调用 `poll(10*time.Second)`；另给出 `AddDate` 与 `Add` 按意图选择的对比 |
| Use time.Time / Duration with external systems | `Interval int` + JSON `{"interval": 2}`，单位不明 | `IntervalMillis int` + `json:"intervalMillis"`，把单位写进字段名 |
| Error Types | `errors.New` 直接返回而不导出变量，调用方无法用 `errors.Is` 匹配 | 导出 `var ErrCouldNotOpen` 供匹配；另给出动态消息场景：`fmt.Errorf` vs 定义 `NotFoundError` 自定义类型供 `errors.As` 匹配 |
| Error Wrapping | `fmt.Errorf("failed to create new store: %w", err)`，多层堆叠后变成 `failed to x: failed to y: failed to create new store: the error` | `fmt.Errorf("new store: %w", err)`，堆叠后是 `x: y: new store: the error` |
| Error Naming | —（只有 Good 例） | 全局错误用 `ErrXxx`/`errXxx` 前缀（注释说明为何导出），自定义类型用 `NotFoundError` 后缀 |
| Handle Errors Once | **Bad**：log 之后又把同一个 error 返回，导致上游重复处理、日志噪音 | 三种 Good：包装后返回（`%w` 保证可匹配）；log 并优雅降级（如写 metrics 失败不应打断程序）；用 `errors.Is` 匹配特定错误（如 `ErrUserNotFound` 用 UTC 兜底）后降级，其余仍包装返回 |
| Handle Type Assertion Failures | `t := i.(string)`，类型不符直接 panic | `t, ok := i.(string)` + `if !ok` 优雅处理 |
| Don't Panic | `run(args)` 参数为空时 `panic`；测试里 `os.CreateTemp` 失败时 `panic` | `run(args) error` 返回 `errors.New`；测试里用 `t.Fatal` 标记失败 |
| Use go.uber.org/atomic | `running int32` 加注释说明是 atomic，容易忘记用原子操作读写 | `running atomic.Bool`，类型安全且提供 `atomic.Bool` |
| Avoid Mutable Globals | `var _timeNow = time.Now` 可被改动的全局函数指针；测试里保存/替换/恢复该全局 | 把 `now func() time.Time` 作为 `signer` 结构体字段注入；测试里直接设置实例字段，不碰全局 |
| Avoid Embedding Types in Public Structs | `ConcreteList` 内嵌 `*AbstractList`（或内嵌 `AbstractList` 接口），泄漏抽象实现细节并锁死演进空间 | 用命名字段 `list *AbstractList`（或 `list AbstractList`）并手写委派方法 `Add`/`Remove` |
| Avoid Using Built-In Names | `var error string`、`func handleErrorMessage(error string)` 遮蔽内置；结构体字段命名为 `error`/`string` 让 grep 结果有歧义 | 改名 `errorMessage`、`err error` 等，grep `error`/`string` 时不再有歧义 |
| Avoid init() | 用 `init()` 里给 `_defaultFoo` 赋值；用 `init()` 里做 `_config` 的加载（含 I/O、可能失败） | 用包级变量直接初始化 `var _defaultFoo = Foo{...}` 或 `defaultFoo()`；把配置加载写成 `func loadConfig() Config`，由 `main()` 调用 |
| Exit in Main | `main()` 调用的 `readFile` 内部直接 `os.Exit`/`log.Fatal`，控制流不直观、难测试、跳过 defer 清理 | `readFile` 返回 `(body, error)`，只在 `main()` 里 `log.Fatal(err)` |
| Exit Once | `main()` 里多处 `os.Exit`/`log.Fatal` 分支（参数错误、文件读取失败等） | `main()` 只写 `if err := run(); err != nil { log.Fatal(err) }`，业务逻辑全在可测试的 `run()` 里；另给出 `os.Exit(run(args))` 与自定义退出码的变体 |
| Use field tags in marshaled structs | `Price int` / `Name string` 无 tag，序列化后的字段名会被重构/改名意外破坏 | 加 `json:"price"` / `json:"name"`，之后把 `Name` 重命名为 `Symbol` 也安全 |
| Don't fire-and-forget goroutines | 无限循环 `go func(){ for { flush(); time.Sleep(delay) } }()`，无停止途径，只能随进程退出 | 用 `stop`/`done` 两个 channel：`close(stop)` 通知停止，`<-done` 等待退出 |
| No goroutines in init() | `init()` 里无条件 `go doWork()`，用户对 goroutine 无控制无停止手段 | 提供 `NewWorker(...)` 返回带 `stop` channel 的 `Worker`，用户按需创建并通过 `Close`/`Stop` 关闭释放资源 |
| Prefer strconv over fmt | `fmt.Sprint(rand.Int())`，基准 143 ns/op、2 allocs/op | `strconv.Itoa(rand.Int())`，基准 64.2 ns/op、1 alloc/op |
| Avoid repeated string-to-byte conversions | 循环内每次 `w.Write([]byte("Hello world"))`，22.2 ns/op | 循环外 `data := []byte("Hello world")` 一次转换后复用，3.25 ns/op |
| Specifying Map Capacity Hints | `make(map[string]os.DirEntry)` 无 size hint，map 动态扩容导致多次分配 | `make(map[string]os.DirEntry, len(files))`，赋值时分配更少 |
| Specifying Slice Capacity | `make([]int, 0)` 后 append，基准 2.48s | `make([]int, 0, size)` 后 append，基准 0.21s |
| Group Similar Declarations | 多个单行 `import "a"`/`import "b"`、分散的 `const`/`var`、无关声明被硬凑成一组、函数内零散 `:=` 声明 | 用 `import (...)`、`const (...)`、`var (...)` 分组；无关声明拆开；函数内相邻变量用 `var (...)` 分组 |
| Import Group Ordering | import 块里标准库与第三方混排 | 标准库一组，其余一组（goimports 默认） |
| Import Aliasing | 给 `runtime/trace` 起别名 `runtimetrace`（无必要） | 直接用 `"runtime/trace"`；只在包名与路径末段不一致（如 `client "example.com/client-go"`）或冲突时起别名 |
| Function Grouping and Ordering | 方法定义在类型定义之前、函数不按调用顺序/receiver 分组 | 先 `type something struct`，再 `newSomething()`，再各 receiver 的方法，工具函数靠后 |
| Reduce Nesting | 正向条件层层嵌套（`if v.F1 == 1 { ... if err := v.Call(); err == nil { ... } }`） | 反向条件提前 `continue`/`return`（`if v.F1 != 1 { log; continue }`），主逻辑在浅层 |
| Unnecessary Else | `var a int; if b { a = 100 } else { a = 10 }` | `a := 10; if b { a = 100 }` |
| Top-level Variable Declarations | `var _s string = F()`，重复书写了可推断的类型 | `var _s = F()`；另给出表达式类型与期望类型不一致时必须写类型的例子（`var _e error = F()`） |
| Prefix Unexported Globals with _ | 顶层 `defaultPort`/`defaultUser` 无前缀，容易被误当作局部/其他文件的变量 | 改为 `_defaultPort`/`_defaultUser` |
| Embedding in Structs | 内嵌字段混在普通字段中间且无空行；内嵌 `sync.Mutex` 让 `Lock`/`Unlock` 意外暴露；内嵌 `io.ReadWriter` 指针使零值不可用；内嵌 `sync.Mutex`/`sync.WaitGroup`/`bytes.Buffer` 一堆 | 内嵌字段在前 + 空行分隔；`countingWriteCloser` 有目的性地包装 `Write`；内嵌 `bytes.Buffer` 保住有用的零值；改用命名字段 `mtx`/`wg`/`buf` |
| Local Variable Declarations | `var s = "foo"` 显式赋值却用 var；`filtered := []int{}` 声明空 slice | `s := "foo"`；`var filtered []int` 更能体现默认值语义 |
| nil is a valid slice | `return []int{}` 返回长度 0 的 slice；`isEmpty` 用 `s == nil` 判断；`nums := []int{}` 后再 append | `return nil`；`len(s) == 0`；`var nums []int` 直接 append |
| Reduce Scope of Variables | `err := os.WriteFile(...)` 在 if 外声明；把 `os.ReadFile` 的 data/err 塞进 if 初始化语句导致 if 外还要用；`_defaultPort` 常量提到全局 | `if err := os.WriteFile(...); err != nil` 收进 if；需要 if 外结果时正常在 if 外声明；常量放进使用它的函数里 |
| Avoid Naked Parameters | `printInfo("foo", true, true)` 无法看出两个 bool 的含义 | `printInfo("foo", true /* isLocal */, true /* done */)`；更推荐用 `type Region int` 之类的自定义类型替换裸 bool |
| Use Raw String Literals to Avoid Escaping | `"unknown name:\"test\""` 手工转义难以阅读 | 反引号原始字符串 `` `unknown error:"test"` `` |
| Use Field Names to Initialize Structs | `User{"John", "Doe", true}` 位置式初始化，字段增删/换序会静默出错 | 写字段名 `User{FirstName: "John", LastName: "Doe", Admin: true}`；测试表 ≤3 字段时可例外省略 |
| Omit Zero Value Fields in Structs | `User{FirstName: "John", LastName: "Doe", MiddleName: ""}` 显式写了零值，属噪音 | 省略 `MiddleName`；测试表里零值字段仍写出来以保留上下文 |
| Use `var` for Zero Value Structs | `user := User{}` | `var user User`，与 map 初始化、空 slice 声明的风格一致 |
| Initializing Struct References | `sptr := new(T); sptr.Name = "bar"` | `sptr := &T{Name: "bar"}`，与结构体初始化方式一致 |
| Initializing Maps | `m1 = map[T1]T2{}` 声明与初始化在视觉上相似 | `m1 = make(map[T1]T2)` 视觉上可区分；另给出固定元素集合时用 map 字面量而非逐条赋值 |
| Format Strings outside Printf | `msg := "unexpected values %v, %v\n"` 使 `go vet` 无法静态分析格式串 | `const msg = "..."` |
| Test Tables | 一串重复的 `net.SplitHostPort` + 逐条断言的平铺测试 | 用 `tests := []struct{ give, want * string }{...}` + `for _, tt := range tests` 子测试，字段用 `give`/`want` 前缀 |
| Avoid Unnecessary Complexity in Table Tests | `TestComplicatedTable` 的子测试里带大量条件分支与函数字段 | 拆成多个专注单一行为的独立 `Test...` 函数（如 `TestShouldCallX`），mock 设置直白 |
| Parallel Tests | —（只有 Good 例） | 循环体内 `tt := tt` 显式重新声明，因为启用了 `t.Parallel()` |
| Functional Options | `Open(addr string, cache bool, logger *zap.Logger)` 强制用户即使想要默认值也必须传参，调用点出现 `db.Open(addr, db.DefaultCache, zap.NewNop())` 之类的噪音 | 声明 `Option` 接口与 `WithCache`/`WithLogger` 等选项，调用点变成 `db.Open(addr)` / `db.Open(addr, db.WithLogger(log))`；并给出基于 `options` struct + 未导出方法的完整实现 |
| Linting | —（无代码表） | 仅列 linter 与 golangci-lint 推荐 |

---

## 4. 原文明确标注「偏好 / 例外 / 有争议 / 非硬性」的条目

以下条目原文使用了明确的软化措辞，**不是硬性规定**，对照 taihu 代码时应作为「可选偏好」而非「必须改」：

| 编号 | 软化措辞（原文照抄） | 说明 |
|---|---|---|
| uber-062 | "soft line length limit of **99 characters** ... but it is not a hard limit. Code is allowed to exceed this limit." | 99 字符只是软限制，明确允许超限 |
| uber-063 / uber-064 | "Some of the guidelines ... are situational, contextual, or subjective. Above all else, **be consistent**." | 原文自我声明部分规则是情境性/主观的；一致性优先于任何单条规则 |
| uber-013 | "Any other size must be subject to a high level of scrutiny." | 不是禁止，而是要求论证 |
| uber-014 / uber-015 | "you should **usually** start your enums on a non-zero value" / "There are cases where using the zero value makes sense" | 有明确的零值例外 |
| uber-026 / uber-027 | "%w ... This is a good default for most wrapped errors" | `%w` 是「多数情况的良好默认」，不是唯一选择 |
| uber-036 | "An exception to this is program initialization" | panic 的初始化例外 |
| uber-037 | "Even in tests, prefer t.Fatal or t.FailNow over panics" | prefer，非 must |
| uber-042 / uber-043 / uber-044 | "Avoid init() where possible" / "some situations in which init() may be preferable or necessary" | 明确列出 init() 合理的场景 |
| uber-046 | "If possible, prefer to call os.Exit or log.Fatal at most once" | If possible / prefer |
| uber-056 | "Performance-specific guidelines apply only to the hot path." | 整章 Performance 仅在热路径适用 |
| uber-065 / uber-066 / uber-067 | "Only group related declarations. Do not group declarations that are unrelated." + "**Exception**: Variable declarations ... should be grouped together if declared adjacent to other variables ... even if they are unrelated." | 分组规则自身含相反方向的例外 |
| uber-077 | "In all other scenarios, import aliases should be avoided unless there is a direct conflict" | 有冲突例外 |
| uber-088 | "**Exception**: Unexported error values may use the prefix err without the underscore." | 明确例外 |
| uber-092 | "A good litmus test is, 'would all of these exported inner methods/fields be added directly to the outer type'; if the answer is 'some' or 'no', don't embed" | 判断标准而非机械规则 |
| uber-093 | "Exception: Mutexes should not be embedded" | 对 uber-090 的例外 |
| uber-100 | "Do not reduce the scope if it conflicts with Reduce Nesting" | 两条规则冲突时 Reduce Nesting 优先 |
| uber-102 | "Constants do not need to be global unless ..." | 有条件 |
| uber-104 | "Better yet, replace naked bool types with custom types" | 建议递进，非强制 |
| uber-107 | "**Exception**: Field names *may* be omitted in test tables when there are 3 or fewer fields." | 明确 may |
| uber-108 | "omit fields that have zero values **unless they provide meaningful context**" | 有条件 |
| uber-119 / uber-125 | "Table tests should **NOT** be used whenever there needs to be complex or conditional logic" 但 "**While there are no strict guidelines**, readability and maintainability should always be top-of-mind" | 强烈建议 + 自承无严格准则 |
| uber-121 / uber-123 / uber-124 | "it **may be preferable** to group similar cases" / "it's **acceptable** to have a single branching pathway" | 明确可接受 |
| uber-127 | 循环变量重新声明这条依赖是否用了 `t.Parallel()`（Go 1.22 起 loopvar 语义已变，原文未讨论版本差异） | 与 Go 版本相关 |
| uber-129 / uber-130 / uber-131 | "Our **suggested** way of implementing this pattern" / "Note that there's a method of implementing this pattern with closures but **we believe** that the pattern above provides more flexibility" | 自承是一种实现偏好；原文明确承认闭包实现方式存在且合理 |
| uber-133 / uber-134 | "We recommend using the following linters **at a minimum**" / "We **recommend** golangci-lint" | 推荐而非规定 |
| uber-001 / uber-002 | "You **almost never** need a pointer to an interface." | almost never，含边界情形 |
| uber-076 vs uber-077 | "Import aliasing **must** be used if the package name does not match the last element of the import path" 与 "In all other scenarios, import aliases **should be avoided**" | must 与 should 并存 |
| uber-085 / uber-089 | 顶层变量 "Do not specify the type, **unless** it is not the same type as the expression"；内嵌 "there **must** be an empty line separating" | 前者有条件，后者是真 must |

---

## 5. 非规则内容（与 Go 语言本身无关，或不是编码规则）

以下内容**未计入规则清单**，或虽计入但性质存疑，对照 spec 时应单独处理：

| 章节 | 性质 | 说明 |
|---|---|---|
| Introduction（H2） | 前言 / 资源导引 | 介绍文档由来（Prashant Varanasi、Simon Newton）、目标读者、三条外部参考（Effective Go、Go Common Mistakes、Go Code Review Comments）。含两条工具建议：「所有代码经 `golint` 和 `go vet` 应无错」、编辑器保存时跑 `goimports`、跑 `golint`/`go vet`。**注意：`golint` 已废弃**，同文 Linting 一节明确指出 `revive` 是其现代后继，故 Introduction 的 `golint` 建议属过时内容，不应照搬进 spec |
| Linting（H2）/ Lint Runners（H3） | 工具链配置指导 | 严格说不是 Go 语言规则，而是 linter 选型与 runner 配置建议。本清单按任务约定归入「工具链」类别（uber-132 ~ uber-134），但它描述的是 CI/编辑器配置，不是代码写法 |
| Performance 一节中的基准数字 | 佐证材料 | `143 ns/op` vs `64.2 ns/op` 等数字是示例基准输出，不是规则 |
| Test Tables 中的测试库选择 | 隐含工具依赖 | 示例使用 `testify`（`require.NoError`、`assert.Equal`）与 `gomock`，但原文未把「必须用 testify/gomock」立为规则，仅是示例代码。taihu 若无这些依赖，不应据此引入 |
| `Use go.uber.org/atomic` / `go.uber.org/goleak` | 第三方依赖推荐 | 是规则，但绑定的是 Uber 自研依赖。taihu 若不用 `go.uber.org/*`，应保留其「意图」（类型安全原子操作、泄漏检测）而非照搬依赖 |
| 原文中的 HTML 注释 | 未完成待办 | 文中残留若干 `<!-- TODO: ... -->`（String 方法 for enums、类型断言单返回值可接受的情形、`_test` 包用法、用参数结构体替换 functional options），说明这些话题原文**尚未覆盖**，不要臆造规则 |

---

## 6. 安全说明

抓取到的原文**未发现任何 prompt injection / 越权指令**。已扫描 `ignore previous`、`disregard`、`you should now`、`as an AI`、`system prompt`、`execute`、`curl`、`wget` 等模式，零命中。全文是一部正常的技术风格指南，可安全作为数据使用。
