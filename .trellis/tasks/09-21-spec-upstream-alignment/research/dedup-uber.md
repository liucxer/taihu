# uber 适用规则 × 现有 spec 去重

> 输入：triage-uber.md 的 122 条「适用」
> 输出：已覆盖 15 / 部分覆盖 15 / 新增 92 / 措辞可改善 0，合计 = 122
>
> 判定口径（与「铁律」对齐）：
>
> - **已覆盖** —— 现有 spec 里有**规定性正文**表达同一要求（必须是命令句/禁止句，不能只是描述性提及），且已给出具体条目。
> - **部分覆盖** —— 现有规则碰了同一个代码构造，但**关切不同、范围更窄或判据不同**；缺的那部分写进「缺的是哪部分」。
> - **新增** —— 现有 spec 正文里没有任何一条规则触及该构造。附近若只有描述性文字（记录某个实现事实），仍判新增并在末列注明。
> - 现有 spec 的 `guides/` 层（`th-001`~`th-023`）按约定**不作为已覆盖依据**，本轮完全未引用。

---

## 一、新增（92 条）—— 本轮真正的产出

拟落位置说明：本轮有 92 条新增，其中绝大部分是**纯 Go 语言层面的风格**，现有 8 层里没有对应容器。建议新立一层文件 `.trellis/spec/architecture/go-style.md`（下称 **go-style.md（新）**），把语言级风格集中起来；少数与已有层强耦合的落到既有文件（下表逐条标出）。

| uber 编号 | 规则一句话 | taihu 锚点 | 拟落 spec 文件 | 为什么现有 spec 没覆盖 |
|-----------|-----------|-----------|---------------|----------------------|
| uber-001 | 接口按值传递，几乎不需要指向接口的指针 | internal/benchkit/run.go:83 | go-style.md（新） | 接口规则只有 th-287（小、未导出、声明在使用方文件）与 th-288/289（跨模块才导出、签名只用标准库类型）——全是「接口怎么设计」，没有「接口怎么传」 |
| uber-002 | 需改底层数据时实现侧必须用指针接收者 | internal/cluster/kv_mem.go:23 | go-style.md（新） | th-045 只规定 receiver 的**命名**（单字母、同类型恒定）；值/指针接收者的选择在现有 spec 里完全无成文规则 |
| uber-005 | 值接收者方法在值和指针上都能调；指针接收者只能在可寻址值上调 | internal/cluster/kv_tikv.go:22、:99 | go-style.md（新） | 同 002：接收者语义的差异没有任何规则涉及。属语言机制说明，宜作为 002 的附注而非独立条目 |
| uber-006 | 只有指针接收者方法时值无法满足接口 | cli_core_test.go:48 对照 kv_mem.go:16 | go-style.md（新） | th-290 只要求「新增实现补 `var _ I = (*T)(nil)`」，没有解释值/指针接收者与接口满足性的因果关系 |
| uber-007 | mutex 零值即有效，几乎不需要指向 mutex 的指针 | internal/cluster/kv_mem.go:12 | go-style.md（新） | 并发规则（th-128~th-151）管锁序、持锁范围、完成泵模型等**用法**；mutex 的声明形态无一字 |
| uber-008 | mutex 应是非指针命名字段；不要内嵌 | internal/transport/server.go:45、frame.go:80 | go-style.md（新） | 同上；th-133 只要求「`Device.mu` 保护的字段写在声明处」，是注释规则不是声明形态规则 |
| uber-009 | 接收 slice/map 并保留引用时必须复制 | internal/cluster/kv_mem.go:25 | go-style.md（新） | 缓冲所有权三节（th-118/119/120）管的是缓冲**归还**与池化归属，方向与「入参防御性拷贝」相反 |
| uber-011 | 用 defer 清理资源 | internal/metastore/segments.go:266、kv_pebble.go:193 | go-style.md（新） | 现有只在代码引用里出现 defer（buffer-and-concurrency.md:44-51），是一条事实而不是规则；测试层反而明令用 `t.Cleanup`（th-251），生产侧无对偶规则 |
| uber-012 | defer 开销极小，只在纳秒级函数里才该避免 | internal/device/device.go:397 | go-style.md（新） | 属 011 的性能理由，建议并入 011 一条，不单列 |
| uber-013 | channel 通常为 1 或无缓冲，其他尺寸需高度审视 | internal/transport/batch.go:91、pkg/taihu-client/index.go:39 | go-style.md（新） | 现有 spec 提到通道的地方全是语义约束（th-277 流通道 `in` 永不被 close、th-196 `stop` 通道），没有一条涉及缓冲容量 |
| uber-014 | 枚举通常从非零值开始（iota+1） | internal/aio/aio.go:104、meta.go:77、frame.go:65 | go-style.md（新） | 现有 spec 里唯一的枚举正文是 metadata-and-compaction.md:72-79 的段状态常量表，那是**描述**（记录 5 个状态的语义），不是规则 |
| uber-015 | 零值即期望默认行为时枚举可从 0 开始 | 同上三处 | go-style.md（新） | 同 014。014/015 是同一件事的两半（默认从 0 / 例外也从 0），建议合成一条「taihu 枚举从 0 开始、零值即默认语义」 |
| uber-016 | 处理时间一律用 `"time"` 包，不自行假设 24h/60min/365d | internal/storage/compact.go:29、:65 | go-style.md（新） | 现有 spec 没有任何时间类型/时间常量的规则（对 `Duration`、`time.Time` 的 greps 零命中） |
| uber-017 | 时间点用 `time.Time`，比较/加减用其方法 | internal/cluster/instance.go:39（正例）/ capacity.go:18（反例） | go-style.md（新） | 同 016 |
| uber-018 | 时间段用 `time.Duration` | internal/storage/compact.go:20、pkg/taihu-client/config.go:49 | go-style.md（新） | 同 016 |
| uber-019 | 与外部系统交互尽量用 `time.Duration`/`time.Time` | cmd/taihu/cmd/root.go:91 | go-style.md（新） | 同 016。016~019 是一簇，宜合为一条「时间一律走 `time` 包：点用 `time.Time`、段用 `time.Duration`」 |
| uber-020 | 无法用时用 int/float64，单位写进字段名 | internal/cluster/capacity.go:18、instance.go:18 | go-style.md（新） | 现有 spec 没有命名与单位相关的规则（th-046 只管首字母缩写） |
| uber-021 | 无法用 `time.Time` 时按 RFC 3339 用 string | internal/rpcclient/admin.go:16、internal/cluster/instance.go:19 | go-style.md（新） | 现有 spec 里「时间点以 int64 unix 秒过线」只在 wire-protocol 里作为协议事实存在，无规则；且这条与 taihu 现状（int64）相反，落 spec 需先裁定 |
| uber-025 | 没有额外上下文可加时原样返回 | internal/cluster/register.go:14、client.go:48 | architecture/code-style.md 扩章 | th-049 只规定「包装成什么形态」，th-234 只规定 SDK「原样透出」，都不构成「无上下文可加时不要包装」的通用规则 |
| uber-029 | 送往其他系统的错误应一眼看出是错误 | internal/storage/compact.go:72、cmd/taihu/cmd/server.go:183 | architecture/code-style.md 扩章 | 日志三通道（th-053~057）管的是**子系统归属**（前缀），不管「错误在日志行里是否可辨识」；`err=%v` 标签的写法无规则 |
| uber-031 | 自定义错误类型用 `Error` 后缀 | internal/aio/aio_uring_linux.go:399 | architecture/error-model.md 扩章 | error-model.md §2 只管「不要新增导出的自定义 error 类型」，对**命名**只字未提（`uringParamError` 满足后缀只是事实） |
| uber-034 | 类型断言一律用 comma ok | internal/metastore/cache.go:55/66/96、pkg/taihu-client/route_cache.go:51/62/72 | go-style.md（新） | 现有 spec 只在 error-model.md:40 用到「类型断言」一词（说明 uringParamError 不是给调用方断言的），无 comma-ok 规则。注意本条是**反例**，落 spec 属新增约束 |
| uber-037 | 测试中也优先 `t.Fatal`/`t.FailNow` 而非 panic | internal/transport/protocol_test.go:390、storage_test.go:26 | testing/unit-tests.md 扩章 | th-243 只规定「断言手写 `if got != want { t.Fatalf }`」，没禁止测试里 panic；且 code-style.md:41 **明确允许**测试辅助函数 panic——本条会收窄现行许可，需先裁定 |
| uber-040 | 避免在公开结构体内嵌类型 | cmd/taihu/cmd/bench_single.go:106 | go-style.md（新） | 内嵌在现有 spec 里只被**要求**过一次（th-237 要求测试 fake 嵌入接口），没有一条限制公开 struct 的内嵌 |
| uber-041 | 不要用 Go 预声明标识符做名字 | internal/cluster/kv_tikv.go:8（`tikverr` 别名） | go-style.md（新） | th-046 管首字母缩写大小写（`ClientID`/`JSON`），不涉及内置名遮蔽；全仓无相关规则 |
| uber-042 | 尽量避免 init()；不可避免时须确定、不依赖顺序、不操纵全局、不做 I/O | internal/transport/frame.go:25、cmd/taihu/cmd/root.go:20 | go-style.md（新） | 现有 spec 对 init **只有正面要求**（th-083/cli/index.md:24 要求子命令在 init 里 AddCommand），没有任何限制性规则。本条会与该正面要求并存，落 spec 时需写清「注册类 init 是允许的例外」 |
| uber-043 | 不满足上述要求的代码应作为 main() 的辅助函数 | cmd/taihu/cmd/root.go:20、:84 | go-style.md（新） | 同 042 |
| uber-044 | init() 可取的场景：复杂表达式、可插拔钩子/注册表、确定性预计算 | cmd/taihu/cmd/root.go:84、internal/transport/frame.go:25 | go-style.md（新） | 现有 spec 无「init 可取范围」的规则；cli/index.md:24 只是要求用 init 注册，未做分类（属同一条新增规则的实例） |
| uber-050 | 用 `go.uber.org/goleak` 做 goroutine 泄漏测试 | 全仓零命中；compact.go:53、segments.go:137、index.go:47 | testing/unit-tests.md 扩章 | 现有测试层只规定「不得 import testify」（th-243），无泄漏检测要求。**注意**：这条要引入测试期第三方依赖，与「internal/ 零外部依赖」取向有张力，落 spec 前需 owner 裁定（triage 已把该点标为拿不准） |
| uber-051 | 等多个 goroutine 用 sync.WaitGroup + `wg.Wait()` | internal/transport/server_shm_linux.go:46/224/241、cmd/taihu/cmd/server.go:270 | engine/buffer-and-concurrency.md 扩章 | 现有并发层写满锁序、泵模型、Ring 契约，但**没有一条关于 goroutine 汇合方式**的规则（WaitGroup vs channel） |
| uber-052 | 只等一个 goroutine 用 `chan struct{}` + `close(done)` + `<-done` | segments.go:155-156、index.go:101-110、frame.go:59 | engine/buffer-and-concurrency.md 扩章 | th-196 只对 `Compactor.Stop` 规定了 `close(c.stop)` + `wg.Wait()`；th-277 管的是「流通道 `in` 永不被 close」（另一件事）。通用停止信号形态无规则 |
| uber-053 | init() 中不应启动 goroutine | internal/transport/frame.go:25、cmd/taihu/cmd/*.go 的 11 处 init | go-style.md（新） | 同 042/043/044，属 init 簇 |
| uber-055 | worker 管理多个 goroutine 时应使用 WaitGroup | internal/transport/server_shm_linux.go:46/241/259/224 | engine/buffer-and-concurrency.md 扩章 | 同 051 |
| uber-056 | 性能指引只适用于热路径 | internal/aio/aio_linux.go:142-143 对比 cmd/taihu/cmd/helpers.go:131-137 | engine/buffer-and-concurrency.md 扩章 | 现有性能规则（th-122/123/129/151/168/186/191/192）全是**具体热路径**的优化，没有一条界定「性能规则的适用范围」这一元规则 |
| uber-057 | 基本类型与字符串互转优先 `strconv` 而非 `fmt` | cmd/taihu/cmd/bench_storage.go:237、internal/benchkit/run.go:66 | go-style.md（新） | `strconv` 在现有 spec 里零命中 |
| uber-058 | 不要对固定字符串反复做 string→[]byte 转换 | internal/metastore/kv_pebble.go:188、:314、segments.go:385 | go-style.md（新） | 无相关规则；th-058 是 import 分组，同号不同事 |
| uber-059 | 尽量指定容器容量（slice） | internal/transport/protocol/protocol.go:417、batch.go:120、server_admin.go:75 | go-style.md（新） | 无任何容量提示规则（`make(` 带 cap 的 grep 在 spec 里零命中） |
| uber-060 | `make(map)` 给容量提示 | internal/metastore/kv_pebble.go:171、:194 | go-style.md（新） | 同 059 |
| uber-061 | `make([]T, len, cap)` 给容量 | internal/cluster/kv_mem.go:63、internal/aio/aio_linux.go:185 | go-style.md（新） | 同 059。059~061 是一簇，宜合为一条「预知规模的容器一律给容量提示」 |
| uber-062 | 软限制 99 字符，允许超限 | cmd/taihu/cmd/bench_cluster.go:151（191 字符） | go-style.md（新） | 现有格式化规则只有 th-097「必须 gofmt 干净」，gofmt 不管行长 |
| uber-065 | 相似的声明应分组 | internal/ierr/ierr.go:9、internal/rpcclient/reexport.go:31、internal/cluster/kv.go:24 | go-style.md（新） | error-model.md:19「6 个 sentinel 集中声明」是**描述性**记录，不是规则；无分组约定 |
| uber-066 | 不要分组无关声明 | internal/bufpool/bufpool.go:23-27、device.go:31-36 | go-style.md（新） | 同 065 |
| uber-067 | 例外：相邻的局部变量声明即使无关也应分组 | internal/device/device.go:444-445 | go-style.md（新） | 同 065 |
| uber-069 | 包名全小写、无大写无下划线 | internal/ierr/ierr.go:5、internal/bufpool/bufpool.go:15 | go-style.md（新） | th-046 管标识符里的首字母缩写，**不管包名**；th-215 只管 `pkg/taihu-client` 目录名与包名不一致这一特例 |
| uber-070 | 包名简短精炼 | 全仓 17 个包名 ≤7 字符 | go-style.md（新） | 同 069 |
| uber-071 | 包名不应是复数 | internal/cluster/kv.go:4 | go-style.md（新） | 同 069 |
| uber-072 | 包名不应叫 common/util/shared/lib | 全仓 17 个包名无此类 | go-style.md（新） | 同 069 |
| uber-073 | 包名应无需在多数调用点重命名 | internal/cluster/kv_tikv.go:8（唯一别名） | go-style.md（新） | 同 069 |
| uber-074 | 函数名用 MixedCaps | internal/storage/storage.go:57 | go-style.md（新） | 现有命名规则只有 th-045（receiver）、th-046（首字母缩写）、th-249（测试辅助前缀）；「函数名不得带下划线」零命中 |
| uber-077 | 其他场景避免导入别名，除非直接冲突 | internal/cluster/kv_tikv.go:8 | go-style.md（新） | 现有 spec 只有 th-215（目录名/包名不一致时调用方只能用包名），无「别名应尽量避免」的规则；069~073+077 是一簇（包与导入命名），建议合为一条 |
| uber-078 | 函数按大致调用顺序排序 | internal/transport/protocol/protocol.go:156/247/326/334、server.go:62→96→109→202 | go-style.md（新） | 现有文件组织规则（th-034/036）管的是「一个文件里放什么」（对外面只放导出名、实现文件按实现/平台/职责分），不管**顺序** |
| uber-079 | 文件内函数按 receiver 分组 | internal/storage/storage.go、internal/metastore/segments.go:46-382 | go-style.md（新） | 同 078 |
| uber-080 | 导出函数应在文件前部 | internal/storage/storage.go:19→31→482 | go-style.md（新） | th-034 要求「对外面文件里只有导出内容」，那是**分文件**策略，与「同一文件内的先后次序」是两件事 |
| uber-081 | NewXyz 可紧随类型定义、先于该 receiver 其余方法 | storage.go:19→31、server.go:40→50→56 | go-style.md（新） | 同 080 |
| uber-082 | 纯工具函数应放在文件靠后位置 | internal/metastore/segments.go:384、internal/aio/aio.go:19-22 | go-style.md（新） | th-034/038 把未导出 helper 赶到**另一个文件**（aio_internal.go / probe_cache.go），比「放在文件末尾」更严；但这条规则本身（同文件内的位置约定）不存在 |
| uber-083 | 减少嵌套：先处理错误/特殊分支，提前 return/continue | internal/device/device.go:91、transport/batch.go:108、server.go:80 | go-style.md（新） | 无任何控制流形态规则 |
| uber-084 | if 两分支都赋值给同一变量时改写为单个 if | cmd/taihu/cmd/bench_single.go:83-87 | go-style.md（新） | 同 083 |
| uber-085 | 顶层变量用标准 `var` 关键字 | internal/transport/frame.go:34、bufpool.go:44 | go-style.md（新） | 无顶层声明形态规则 |
| uber-086 | 顶层变量不写类型，除非表达式类型与期望类型不一致 | frame.go:34、device.go:48 | go-style.md（新） | 同 085 |
| uber-087 | 未导出顶层 var/const 加 `_` 前缀 | internal/bufpool/bufpool.go:44、:161、kv_pebble.go:65 | go-style.md（新） | 无相关规则。**注意**：taihu 现状与这条相反（不加 `_`），且 error-model.md:64 只对 sentinel 规定了 `err` 前缀——落 spec 需先裁定是「遵守 uber-087」还是「明确不遵守」 |
| uber-089 | 内嵌类型放在字段列表最前，且与普通字段间须有空行 | cmd/taihu/cmd/bench_single.go:106、bench_cluster.go:146 | go-style.md（新） | 无内嵌摆放规则；040/089/090/092/093 是一簇（内嵌），建议合成一条 |
| uber-090 | 内嵌应提供实在好处，无对用户不利的副作用 | cmd/taihu/cmd/bench_single.go:106 | go-style.md（新） | 同 089 |
| uber-092 | 内嵌要自觉、有意图；litmus test：内层导出成员是否都会加到外层 | cmd/taihu/cmd/bench_single.go:106 | go-style.md（新） | 同 089 |
| uber-093 | 例外：mutex 不应被内嵌，即使外层未导出 | internal/transport/frame.go:80（命名字段） | go-style.md（新） | 同 089 + 007/008（mutex 声明形态整簇缺失） |
| uber-094 | 局部变量显式赋值应用 `:=` | internal/metastore/kv_pebble.go:171 | go-style.md（新） | 无局部声明形态规则 |
| uber-095 | 需要强调默认值时用 `var`（如空 slice） | transport/client_admin.go:116、device.go:444-445、compact.go:110 | go-style.md（新） | 同 094 |
| uber-096 | nil 是合法的长度 0 slice，不要显式返回长度 0 的 slice | internal/cluster/kv_mem.go:37 | go-style.md（新） | 无 nil-slice 规则 |
| uber-097 | 判空一律 `len(s) == 0`，不与 nil 比较 | transport/client.go:178、client_shm_linux.go:195（反例） | go-style.md（新） | 同 096 |
| uber-098 | 零值 slice 无需 `make()` 即可直接用 | internal/device/device.go:444-445 | go-style.md（新） | 同 096 |
| uber-099 | 注意 nil slice 与已分配长度 0 slice 不等价 | internal/cluster/capacity.go:29、instance.go:13 | go-style.md（新） | 现有 spec 里「空值 vs 无记录」的区分只在代码注释中，无规则 |
| uber-100 | 尽量缩小变量/常量作用域 | internal/storage/storage.go:89、:271、:450 | go-style.md（新） | th-034/087 管的是**包级**符号的可见性与测试缝隙，与局部作用域无关 |
| uber-101 | 结果在 if 之外还需要时就不应缩小作用域 | internal/transport/server_admin.go:105 | go-style.md（新） | 同 100 |
| uber-102 | 常量不必是全局，除非多函数/多文件使用或属对外契约 | internal/aio/probe_linux.go:48、bufpool.go:142、server_admin.go:74 | go-style.md（新） | 无「常量放哪一层」的规则。注意 guides 层的 th-005（常量单点定义）按约定不得作为依据，且它讲的是跨文件去重、不是局部 vs 全局 |
| uber-103 | 避免裸参数；参数含义不明显时加 C 风格注释 | internal/transport/client_shm_linux.go:198、:206、:210（反例） | go-style.md（新） | 无参数形态规则 |
| uber-104 | 更好的做法是用自定义类型替代裸 bool | internal/transport/stats.go:38、internal/aio/aio_other.go:42 | go-style.md（新） | 同 103。platform/file-splitting.md:79 引用了 `newIOUringRing(int, bool)` 这段代码，但引用目的是讲平台桩，不是讲裸参数 |
| uber-105 | 用原始字符串字面量避免手工转义 | cmd/taihu/cmd/server.go:291（反例） | go-style.md（新） | 无反引号/转义规则 |
| uber-106 | 初始化结构体几乎总应指定字段名 | internal/storage/options.go:17、internal/device/options.go | go-style.md（新） | 无结构体字面量规则；th-108 讲的是「JSON 输出用带 tag 的匿名 struct」，不是字段名 |
| uber-108 | 用字段名初始化时省略零值字段 | internal/storage/options.go:17 | go-style.md（新） | 同 106 |
| uber-109 | 全字段省略时用 `var` 形式 | internal/cluster/register.go:54、:69、capacity.go:32、client.go:87 | go-style.md（新） | 同 106 |
| uber-110 | 初始化结构体引用用 `&T{}` 而非 `new(T)` | internal/bufpool/bufpool.go:52、:129（反例） | go-style.md（新） | 无相关规则 |
| uber-111 | 空 map / 程序化填充的 map 优先用 `make(..)` | internal/metastore/segments.go:47、device.go:106-107、index.go:54 | go-style.md（新） | 无 map 构造规则 |
| uber-112 | map 持固定元素集合时用 map 字面量 | cmd/taihu/cmd/cluster.go:350 | go-style.md（新） | 同 111 |
| uber-113 | 经验法则：固定元素用字面量，否则 `make`（尽量带 size hint） | cmd/taihu/cmd/cluster.go:350 / :231 | go-style.md（新） | 同 111（含 060）。111~113 + 060 建议合成一条「map 构造方式的选择」 |
| uber-114 | Printf 之外的格式串应定义为 const | test/e2e/harness_test.go:670、:678（反例） | testing/unit-tests.md 扩章 | 无格式串规则。platform/build-verification.md:132 只在描述「fork 并模块后 vet 新报 3 处问题」时提到「非常量格式串」被修，那是事件记录不是规则（且该处说的是 fork 代码） |
| uber-115 | Printf 风格函数应尽量用预定义名，便于 go vet 检查 | test/e2e/harness_test.go:670（反例） | testing/unit-tests.md 扩章 | 无自研 Printf 包装函数的命名规则；th-249 只管「测试辅助函数带主体前缀」 |
| uber-116 | 无法用预定义名时函数名应以 `f` 结尾 | test/e2e/harness_test.go:670（`waitFor` 反例） | testing/unit-tests.md 扩章 | 同 115。114~116 是一簇（格式串与 Printf 包装），建议合成一条 |
| uber-119 | 子测试含复杂/条件逻辑时不应使用表测试 | internal/transport/transport_frame_test.go:438-518（反例） | testing/unit-tests.md 扩章 | th-245 只正向规定「逻辑/纯函数测试必须表驱动」，没有反向的「什么时候别用表」判据 |
| uber-120 | 表测试理想：聚焦最窄行为、最小 test depth、所有字段都用上、逻辑对所有用例都执行 | transport_frame_test.go:412（反例，函数字段/多语义混表） | testing/unit-tests.md 扩章 | th-245 规定的是**形态**（`cases` + `t.Run`），不是**质量判据**；现有 spec 无「用例字段是否都被用上」「逻辑是否对所有用例都跑」这类要求 |
| uber-122 | 分支路径/针对 mock 的 if/把函数放进表里会让表测试混乱 | transport_frame_test.go:412（「把函数放进表」反例） | testing/unit-tests.md 扩章 | 同 119/120；现有 spec 只在 unit-tests.md:98 允许「纯映射类函数省掉 name 字段」，没有反模式清单 |
| uber-124 | 允许用单个 `shouldErr` 字段区分成功/失败 | transport_frame_test.go:442、:512、:515 | testing/unit-tests.md 扩章 | 现有 spec 对表用例的**字段设计**完全没有规定；`wantErr` 的写法只是代码事实 |
| uber-128 | 预见需扩展的公开构造器用 Functional Options | internal/device/device.go:74、internal/storage/storage.go:31、options.go:5-7 | go-style.md（新）/ sdk/public-api.md | 现有 spec 没有任何「选项模式」的规则（sdk/public-api.md:9-23 只规定两个构造入口与返回值）。**注意**：uber-130/131 已判「冲突」（taihu 用闭包而非 Option 接口），落 spec 时只能写「变参选项」这一半，不能写「Option 接口」 |
| uber-129 | 参数已有 3 个或更多时尤其应使用 | internal/storage/storage.go:31 | go-style.md（新） | 同 128 |

**新增簇内的小结**（供后续合并成条目用）：001/002/005/006（接收者与接口传递）、007/008/093（mutex 声明）、014/015（枚举起点）、016~019（时间类型）、040/089/090/092/091（内嵌，091 见部分覆盖）、042/043/044/053（init）、051/052/055（goroutine 汇合与停止）、059~061（容量提示）、065~067（声明分组）、069~073/077（包与导入命名）、078~082（文件内排序）、083/084（控制流）、085/086（顶层声明）、094/095（局部声明）、096~099（nil slice）、100~102（作用域）、103/104（裸参数）、106/108/109/110（结构体初始化）、111~113（map 构造）、114~116（格式串与 Printf 包装）、119/120/122/124（表测试质量与反模式）。

---

## 二、部分覆盖（15 条）—— 需补哪一部分

| uber 编号 | 规则一句话 | 现有规则（file:行 或 th-NNN） | 缺的是哪部分 |
|-----------|-----------|------------------------------|-------------|
| uber-010 | 返回内部 map/slice 必须返回副本 | `transport/interfaces-and-reexport.md:163`（描述 MemoryKV「所有方法都返回深拷贝」） | 现有文字是在**描述** MemoryKV 这一个实现的性质，不是规定「凡是返回内部 map/slice 的 API 都必须返回副本」。缺一条跨层的返回值防御性拷贝规则（含 `Scan`/`ListIndexKeys` 这类枚举接口） |
| uber-024 | 传播错误三选一：原样 / `%w` 加上下文 / `%v` 加上下文 | th-049（`architecture/code-style.md:79-93`） | 覆盖了「`%w` 加上下文」这一支（还额外规定了 op 前缀形态与不加句号）；缺「没有额外上下文可加时**原样返回**」这一支的成文规则。`%v` 遮蔽那支按 triage 属「不适用」 |
| uber-026 | 调用方需访问底层错误时用 `%w`，并文档化+测试 | `architecture/error-model.md:74-99`、`platform/build-verification.md:132` | 覆盖了「可匹配性由 `errors.Is` 消费、并被 `TestMapStorageErrDefaultIsInternal` 一类测试钉住」；缺一条要求「新增 `%w` 包装点必须说明调用方是否可匹配、并补对应用例」的规则 |
| uber-032 | 每个错误只处理一次；不要既 log 又 return | th-100 / th-101（`cli/command-and-output.md:26-38`） | 现有规则只在 **CLI 层**成立（「错误只在 `Execute` 打印一次」「子命令内一律 return，不得顺手 Fprintln(os.Stderr)」）；库代码侧「记录并继续」的写法（`internal/storage/compact.go:72`）只有描述。缺跨层规则 |
| uber-033 | 错误处置四选一：errors.Is 分支 / log 并降级 / 返回定义良好的错误 / 包装返回 | `architecture/error-model.md:36-50`、`:74-99` | 覆盖了其中的 errors.Is 分支映射、sentinel 返回、带上下文包装三支；缺「**记录并降级**」作为合法处置的规则——现有日志规则（th-053~057）只管输出通道，不管错误处置策略 |
| uber-047 | 会被序列化的结构体字段都应打 tag | th-108（`cli/command-and-output.md:83`） | 现有规则只覆盖 CLI `--json` 分支「用带 tag 的匿名 struct，不得用 map 拼」。缺一条跨层规则：`internal/cluster` 的 `InstanceInfo`/`CapacityRecord`/`ClientInfo` 等所有会被 JSON 序列化的结构体字段都必须带 tag |
| uber-048 | 不要 fire-and-forget goroutine | th-223（`sdk/index.md:88`） | 现有规则只是 SDK 层的一条检查项（「新增后台 goroutine 时必须确认 `Close()` 能停掉它」）。缺跨层规则：`internal/device` 的完成泵、`internal/metastore` 的 GC、`internal/storage` 的 Compactor 三条链没有被同一条规则约束 |
| uber-049 | 每个 goroutine 要么有可预测停止时间，要么有停止信号，且须能阻塞等待其结束 | th-223（`sdk/index.md:88`）、th-196（`engine/metadata-and-compaction.md:193`） | 只规定了两个**具体对象**（`Storage.Close`、`Compactor.Stop` 的 `close(c.stop)` + `wg.Wait()`）。缺一条通用规则：任何 `go` 语句都要能回答「什么时候停」「谁等它停」（完成泵的退出条件 `closed && m 空 && inSubmit==0` 写在 engine 里，但那是描述） |
| uber-054 | 包内后台 goroutine 必须由某对象管理并提供 Close/Stop/Shutdown | th-223、th-196（同上） | 同 048/049：规则以单个方法/对象为例（`Compactor.Stop`、`Storage.Close`、`stopGC`），没有上升为「包内后台 goroutine 必须挂在某个对象上并暴露停止入口」。缺的是这条**通用约束**及其判据 |
| uber-063 | 最重要的是保持一致 | th-045（`architecture/code-style.md:47-57`）、th-053（`:109`）、th-058（`:159-181`）、th-286 | 现有 spec 在 receiver 命名、日志通道、import 分组、统计出口各处**分别**要求统一，是这四条元原则的实例；缺的是把这些实例上升为一条跨层元原则（「新写的代码若与既有形态不一致，优先改新代码；要改全局必须一次改完」），以及「不一致时以哪一层为准」的裁决顺序 |
| uber-091 | 内嵌**不应**：暴露无关方法 / 让用户观察内部 / … | th-237（`sdk/public-api.md:137-148` + `testing/unit-tests.md:192`） | 现有规则只在**测试代码**里正面要求内嵌（fake 嵌入接口 + 只覆写要打断的路径，且只允许出现在 `_test.go`），这等于用「限定作用域」间接兜住了一部分风险。缺生产代码侧的内嵌「不应」清单（`test/e2e/c_registry_test.go:62`、`transport_frame_test.go:369/385` 这三处测试内嵌也不满足「只覆写要打断的路径」） |
| uber-107 | 例外：测试表字段 ≤3 时可省略字段名 | `testing/unit-tests.md:98-114` | 现有授权是「纯映射类函数可以省掉 `name` 字段」，判据是**函数性质**而非**字段数**。缺的是更一般的判据（「字段 ≤3 才允许位置式初始化；超过就必须写字段名」），以及与之配套的「多字段表必须带 name」的硬约束 |
| uber-123 | 行为只随输入变化时，把相似用例聚在同一张表更好 | th-245（`testing/unit-tests.md:77-114`） | th-245 只规定「逻辑/纯函数测试必须表驱动」；`unit-tests.md:98` 的 Align4k 表（6 个只有输入不同的用例）正是这条规则的同型实例，但 spec 没有写出「行为只随输入变化 → 聚一表；用例间执行路径不同 → 拆开」这一判据。缺判据 |
| uber-133 | 至少推荐 errcheck / goimports / revive / govet / staticcheck | th-026（`architecture/layering.md:58-67`）、th-043（`architecture/code-style.md:28-37`） | th-026 把提交前检查集固定为 `check-fmt + check-layering + check-sdk-only + go vet`，只覆盖了其中的 **govet**。缺 errcheck / revive / staticcheck 的要求；goimports 被 `gofmt -l` 取代（spec 明确选了 gofmt）。且 spec 禁止 `//nolint`（th-043），若引入这三者需要同时写清告警的处置方式 |
| uber-134 | 推荐 golangci-lint 作为 lint runner | th-026（`architecture/layering.md:58-67`） | th-026 已经把 lint 入口固定为 `make check`（Makefile 直接调 `go vet`），并规定四个子目标「全部以 OK 结尾才算过」。缺 runner 选型的讨论；引入 golangci-lint 会与这条固定入口并存或冲突，需先裁定 |

---

## 三、措辞可改善（0 条）

本轮**没有**判为「措辞可改善」的条目。理由：

- 少数主题重合但表述不同的，都是**同主题不同约定**而非措辞优劣 —— 典型是 uber-118（`tests`/`tt`/`give`）与 th-245（`cases`/`tc`/`want`），taihu 已选定后者，应判「已覆盖」而不是拿上游的表术改写。
- 现有 spec 的表述普遍比上游**更具体**（每条都带 `file:line` 锚点、实测 grep 命令与「为什么」，例如 th-049、th-058、th-062），在本轮 122 条里没有出现「上游表述更好/更准确、值得改写现有条目」的情形。
- 唯一接近的是 th-049（`code-style.md:84` 把 `fmt.Errorf("DialPoolMulti: empty addrs")` 引为正例，却与同条「op 前缀小写英文」自相矛盾）—— 但那是**现有规则自身举例与自身措辞冲突**，属既有缺陷，与「上游表述更好」无关，故不计入本类（该问题已在 `taihu-spec-inventory.md` §2.2 记录）。

---

## 四、已覆盖（15 条）—— 不重复写

| uber 编号 | 规则一句话 | 对应现有规则（file:行 或 th-NNN） | 为什么算同一条 |
|-----------|-----------|----------------------------------|---------------|
| uber-003 | 编译期接口校验，右侧写被断言类型的零值 | th-290，`transport/interfaces-and-reexport.md:55-76` | 同一条：正文规定「`var _ 接口 = (*实现)(nil)` 是常态；新增实现第一件事是补这条断言」，并逐一列出 7 条现有断言，其中 `protocol.go:191` 正是接口零值形态 `netpoll.Reader(nil)`、`kv_mem.go:16` 是 `(*MemoryKV)(nil)` |
| uber-004 | 需校验的场景：API 契约 / 同接口的实现集合 / 违反会破坏使用者 | th-289 + th-290，`transport/interfaces-and-reexport.md:39-76`；`pkg/taihu-client/storage.go:47-48` | 同一条：§3 说明该断言的作用是「两个客户端方法集不得漂移」（= 同接口的实现集合），§2 规定「只在跨模块边界导出接口、签名只用标准库类型」（= API 契约），`storage.go:47-48` 的注释给出「接口声明处无法反向断言，故放在这里」的判据 |
| uber-022 | 错误声明方式决策表（静态/动态 × 需匹配/不需匹配） | th-061，`architecture/error-model.md:36-50` | 同一条：正文给出同一张决策——「不要新增导出的自定义 error 类型。需要区分错误类别时，用 `internal/ierr` 的 sentinel + `errors.Is`（静态+需匹配）；需要携带诊断细节时，用未导出类型（动态+需匹配）或 `fmt.Errorf` 带上下文（不需匹配）」 |
| uber-023 | 导出错误变量/类型会成为包公开 API，需慎重 | th-030，`architecture/api-surface.md:7-13`；`architecture/error-model.md:50`、`:103-122` | 同一条：api-surface 规则 1 规定「标识符默认私有，确有外部调用才导出」；error-model §5/§6 把导出的错误收窄成 re-export 白名单并给出判据「客户端会不会收到」——两者合起来就是「导出错误 = 公开 API，需慎重」 |
| uber-028 | 错误上下文保持简洁，避免 "failed to" 堆叠 | th-049，`architecture/code-style.md:79-93` | 同一条：正文把包装串形态钉死为「小写 op + `: %w`，无 err 时也不加句号」，并说明「跨库边界再加 `taihu: ` 前缀」；`failed to X: ...` 这种堆叠与该形态不相容（实测 `failed to` 全仓零命中） |
| uber-030 | 全局错误值用 `Err`/`err` 前缀 | th-062，`architecture/error-model.md:54-64` | 同一条：正文原话「命名一律 `err` 前缀（无 `Err` 大写）—— Go 里未导出标识符本就小写，用 `errXxx` 与导出的 `ErrXxx` 一眼区分」，一条同时覆盖导出（`Err`）与未导出（`err`）两侧 |
| uber-035 | 生产代码必须避免 panic | th-044，`architecture/code-style.md:39-41` | 同一条：正文「非测试代码不 `panic()`。库代码一律返回 `error`」，并给出可直接复核的 grep 命令与实测结果 |
| uber-036 | panic/recover 不是错误处理策略，只在不可恢复时 panic；初始化失败可 panic | th-044，`architecture/code-style.md:39-41` | 同一条：正文把 panic 的允许范围限定为「只出现在 `_test.go` 里（测试辅助函数遇到不可恢复的装配错误时才用）」，并把非法配置（`aio.go:121`）、平台不支持（`probe_other.go:18`）两条路径都钉到返回 error —— 比上游更严，要求一致 |
| uber-045 | 只在 `main()` 里调 `os.Exit` 或 `log.Fatal*` | th-099，`cli/command-and-output.md:24`；th-086，`cli/index.md:27` | 同一条：前者「不要新增其它退出码，也不要在子命令里调 `os.Exit`」，后者「参数校验放在 `RunE` 开头并**返回 error**，不要中途 `os.Exit`」 |
| uber-046 | 至多调用一次 `os.Exit`/`log.Fatal` | th-098 + th-099，`cli/command-and-output.md:9-24` | 同一条：正文规定「退出码只有 0/1 两种，由 `run()` 一处决定」、「`main` 只做 `os.Exit(run(os.Args[1:]))`」，并把第二处 `helpers.go:122` 明确标为既有例外、不得效仿 —— 即「至多一次」被写成了可判定的形态 |
| uber-068 | import 分两组：标准库 / 其他 | th-058，`architecture/code-style.md:159-181` | 同一条：正文规定 import 三段式分组（标准库 / 第三方含 `third_party/` fork / 本项目）组间空行，并给出 `internal/transport/client.go:5-15` 与 `cmd/taihu/cmd/key.go:3-14` 两个形态；比上游的「两组」更细，要求方向一致 |
| uber-088 | 例外：未导出错误值可用 `err` 前缀不加下划线 | th-062，`architecture/error-model.md:54-64` | 同一条：正文规定包内控制信号「一律 `err` 前缀（无 `Err` 大写）」，并列出 `errInvalidMaxEvents`/`errConnClosed`/`errDeviceClosed` 三个实例 —— 正是「err 前缀且不加下划线」 |
| uber-117 | 多组输入/输出条件下应用表驱动测试 | th-245，`testing/unit-tests.md:77-114` | 同一条：正文「表驱动是逻辑/纯函数测试的默认形态：`cases := []struct{...}` + `for _, tc := range cases` + `t.Run(`，用例名是中文短句」，并给了 `protocol_test.go:81-103` 与 `layout_test.go:8-20` 两个完整实例（边界值专列用例） |
| uber-118 | 约定：slice 名 `tests`、用例名 `tt`、give/want 前缀 | th-245，`testing/unit-tests.md:79-96` | 同主题同规则：现有 spec 已把表驱动的命名形态固定为 `cases` / `tc` / `want`（正文里的 `for _, tc := range cases` 是**规定形态**而非举例），与上游是同一件事的两套名字；taihu 已选定前者，上游那套名字不应引入（这也是本条不判「措辞可改善」的原因） |
| uber-132 | 比选哪套 linter 更重要的是全库一致地 lint | th-026，`architecture/layering.md:58-67`；th-043，`architecture/code-style.md:28-37` | 同一条：前者把提交前检查集固定为 `make check` 四项并要求「四个子目标全部以 `OK` 结尾才算过」，是在规定「全库跑同一套检查」而不是「跑哪套」；后者禁止用 `//nolint` 压告警（「压掉就等于门禁失效」），正面表达「一致性优先于局部豁免」 |

---

## 附：自检

### 四类条数合计

| 类别 | 条数 | 编号 |
|------|------|------|
| 已覆盖 | **15** | 003、004、022、023、028、030、035、036、045、046、068、088、117、118、132 |
| 部分覆盖 | **15** | 010、024、026、032、033、047、048、049、054、063、091、107、123、133、134 |
| 新增 | **92** | 001、002、005、006、007、008、009、011、012、013、014、015、016、017、018、019、020、021、025、029、031、034、037、040、041、042、043、044、050、051、052、053、055、056、057、058、059、060、061、062、065、066、067、069、070、071、072、073、074、077、078、079、080、081、082、083、084、085、086、087、089、090、092、093、094、095、096、097、098、099、100、101、102、103、104、105、106、108、109、110、111、112、113、114、115、116、119、120、122、124、128、129 |
| 措辞可改善 | **0** | —— |
| **合计** | **122** | 与 triage-uber.md 的「适用 122 条」清单逐条一一对应，无遗漏、无重复 |

校验：15 + 15 + 92 + 0 = 122 ✓；122 + 4（冲突）+ 8（不适用）= 134 ✓

### 我读过正文的 spec 文件（25 个中除 `guides/` 3 个外的全部 22 个）

逐文件通读（非仅 grep）：

- `architecture/`：`index.md`、`code-style.md`、`error-model.md`、`api-surface.md`、`layering.md`、`commits.md`（6/6）
- `cli/`：`index.md`、`command-and-output.md`（2/2）
- `engine/`：`index.md`、`buffer-and-concurrency.md`、`metadata-and-compaction.md`（3/3）
- `platform/`：`index.md`、`file-splitting.md`、`build-verification.md`（3/3）
- `sdk/`：`index.md`、`public-api.md`（2/2）
- `testing/`：`index.md`、`unit-tests.md`、`e2e-tests.md`（3/3）
- `transport/`：`index.md`、`wire-protocol.md`、`interfaces-and-reexport.md`（3/3）

`guides/` 下 3 个文件（`index.md`、`code-reuse-thinking-guide.md`、`cross-layer-thinking-guide.md`）按约定**未读正文、未作为任何判定依据**；为防误判，我另外对 `th-005`（常量单点定义）这一条可能被误用为「uber-102 已覆盖」的候选做了显式排除说明（见新增表 uber-102 行）。

辅助检索（用于确认「现有 spec 完全没有这个主题」）：对 spec 全量 grep 了 `strconv`、`MixedCaps`、`make(map`、`cap(`、`容量提示`、`comma ok`、`类型断言`、`内嵌`、`time.Time`、`Duration`、`结构体字面量`、`字段名`、`裸参数`、`包名`、`别名`、`init(`、`defer`、`tag`、`golangci`、`staticcheck`、`errcheck`、`revive`、`goimports`、`goroutine`、`WaitGroup`、`goleak` —— 后 10 个主题的命中全部集中在「go vet 门禁」「日志/并发描述」等无关上下文，无一条构成规则。

### 我拿不准的条目

| 编号 | 判入 | 不确定在哪 |
|------|------|-----------|
| uber-010 | 部分覆盖 | `transport/interfaces-and-reexport.md:163`「所有方法都返回深拷贝」是描述性文字，按铁律「描述 ≠ 规则」应判新增；但它锚定的是「返回副本」这同一行为、且写在一份规范文档的正文里，一个照做的实现者会把它当约束。倾向：部分覆盖（缺的是一条跨层规则），若严格按「只看规定性正文」则应改判新增 |
| uber-011 | 新增 | 现有 spec 的代码引用里大量出现 `defer bufpool.Put(...)`（buffer-and-concurrency.md:44-51、metadata-and-compaction.md:191），且 th-119 要求「读路径返回的池化缓冲必须归还」。若认为「必须归还 + 引用里全是 defer」已足以构成「用 defer 清理」的规则，则应改判部分覆盖。倾向：新增（现有规则管的是「还」不是「用 defer 还」） |
| uber-091 | 部分覆盖 | th-237 是**要求**在测试里内嵌接口，与 uber-091「内嵌不应暴露无关方法」方向相反；它靠「只允许出现在 `_test.go`」兜住了风险，算不算「覆盖了一部分」取决于口径。倾向：部分覆盖（限定作用域=部分约束）；若按「关切不同」严格判，应为新增 |
| uber-107 | 部分覆盖 | `unit-tests.md:98-114` 授权「纯映射类函数可省掉 `name` 字段」并给出位置式初始化实例，与 uber-107 的许可**同形但判据不同**（函数性质 vs 字段数）。若认为判据不同即非覆盖，应改判新增 |
| uber-118 | 已覆盖 | 现有 spec 固定的是 `cases`/`tc`/`want`，与上游的 `tests`/`tt`/`give` 是**两套互相冲突的名字**。判「已覆盖」的理由是「同一件事已有规则，且 taihu 已选定，不应再引入上游命名」；若认为「名字不同即不是同一条」，则应改判新增（或另立「明确采用 cases/tc、不采用 tests/tt」的裁定条目） |
| uber-123 | 部分覆盖 | `unit-tests.md:98` 的 Align4k 表是同型实例，但 spec 未把它写成判据。若按「实例不算覆盖」严格判，应为新增 |
| uber-128 | 新增 | taihu 事实上已在用变参选项（`WithAIOMode` 等），只是形态是闭包而非 Option 接口；且 uber-130/131 已判「冲突」。若认为「变参选项」这半边已由代码事实 + sdk/public-api.md 的构造入口规则隐含覆盖，则应为部分覆盖。倾向：新增，但落 spec 时只能写「变参选项」，不能写「Option 接口」 |
| uber-021 | 新增 | 这条与 taihu 现状（时间点以 int64 unix 秒过 wire，且是协议契约）方向相反，落 spec 时应写成「明确不遵守，理由：wire 契约稳定性」而不宜直接照搬，性质上更接近 triage 的「冲突」类。本轮按任务口径（只对 122 条「适用」做四选一）判为新增，但**建议 owner 在落库前重新裁定它是否该改归「冲突」** |
| uber-037 | 新增 | `architecture/code-style.md:41` **明确允许**测试辅助函数 panic，本条要求收窄为 `t.Fatal`/`t.FailNow`。落地前需先在 spec 内裁定「测试里到底允不允许 panic」，否则会与现有正文冲突 |
| uber-087 | 新增 | 这条与 taihu 现状完全相反（现有顶层未导出变量一律不加 `_` 前缀，且 error-model.md:64 只为 sentinel 规定了 `err` 前缀）。同 uber-021，性质更接近「冲突」；本轮按口径判新增，落库前需裁定 |
| uber-050 | 新增 | 与 triage 一致：是否引入 `go.uber.org/goleak` 触及「`internal/aio` 零外部依赖」的项目取向，我无法从代码判断这是项目纪律还是仅 `internal/aio` 一包的目标，需 owner 决策；若判为触犯纪律，应改归「冲突」 |

### 与 triage-uber.md「拿不准」的交叉核对

triage 标注为拿不准的 7 条（027、050、064、121、125、126、127、075、076、014、022 — 共 11 条编号出现在其末表）里，落在本轮 122 条范围内的只有 **uber-050、uber-014、uber-022** 三条：

- **uber-014** —— 本轮判新增（枚举簇）。triage 的疑虑是「014 是否已被 015 完全吸收」。本轮的处置是把 014/015 明确写成**同一簇、建议合成一条**，该疑虑在落 spec 时按「合成一条」消解。
- **uber-022** —— 本轮判已覆盖（`error-model.md:36-50`）。triage 的疑虑是「需匹配+动态那一格只有 `uringParamError` 一个实例、且没被 `errors.As` 匹配过」。那是对**代码锚点说服力**的疑虑，不影响「spec 是否已有这条规则」的判定，故本轮判已覆盖不受影响。
- **uber-050** —— 本轮判新增，并已把 triage 的依赖纪律疑虑写进「为什么现有 spec 没覆盖」列与上面的拿不准表。
