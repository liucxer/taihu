# taihu 现有 8 层 spec 规则全量清单

> 盘点对象：`.trellis/spec/` 下 8 层共 25 个 markdown 文件（3 741 行）。
> 盘点时间：2026-09-21，基线 commit `a1907b9`（工作区干净）。
> 目的：把现有规范拆成**可逐条对撞**的清单，供「并入上游规范（Uber / ECC / cexll-golang-base-practices）」时做「适用 / 冲突 / 不适用」三分类。
>
> 统计口径：**只收「规定性」内容**（必须 / 不得 / 应当 / 默认 / 一律 / 禁止）。纯背景介绍、纯举例、纯历史沿革不计为规则；但 spec 自己把某个反例写进正文（如已失效路径、已知遗留缺陷）时，会计入第二部分而不是第一部分。

---

## 第一部分：规则索引表

决策类型受控词表：`命名` `错误处理` `并发` `接口与抽象` `资源生命周期` `日志` `注释与文档` `文件组织` `构建与平台` `测试` `依赖方向` `提交信息` `性能` `元规则（怎么改 spec 本身）`

### guides 层（`guides/`，3 文件）

| 编号 | 所属层 | 所在文件 | 规则一句话陈述 | 锚定的真实文件:行号 | 管什么类型的决策 |
|---|---|---|---|---|---|
| th-001 | guides | `guides/index.md:70-79` | 改任何一个值之前必须先 `grep -r "<该值>" .` 全仓搜一遍 | `Makefile:52-58`（check-sdk-only 的正则字面量即此法产物） | 元规则、依赖方向 |
| th-002 | guides | `guides/index.md:56-66` | AI 交叉评审的 CRITICAL/WARNING 结论必须回源码核实后才定优先级，预算约 35% 假阳性 | `internal/aio/api-surface` 相关核实（`internal/aio/aio_linux.go:57`） | 元规则 |
| th-003 | guides | `guides/code-reuse-thinking-guide.md:20-38` | 写新代码前必须先搜同名函数与同类逻辑；已存在则复用或扩展，不得另写一份 | `internal/benchkit/run.go:84-112`（压测执行循环的唯一实现） | 接口与抽象、文件组织 |
| th-004 | guides | `guides/code-reuse-thinking-guide.md:36-38` | 发现自己在从另一个文件抄代码时必须停止，抽成共享实现 | `internal/benchkit/run.go:36-43`（`Store` 抽象） | 接口与抽象 |
| th-005 | guides | `guides/code-reuse-thinking-guide.md:55-59` | 同一常量不得在多个文件各定义一份，必须单点定义 | `internal/transport/protocol/protocol.go:30-63`（协议常量单点） | 文件组织、命名 |
| th-006 | guides | `guides/code-reuse-thinking-guide.md:81-82` | 同一个无类型 payload 字段被 2 处以上读取前，必须先建共享 type guard / normalizer / projection | `internal/transport/protocol/protocol.go:247-264`（`ParsePutHeader` 单点解契约） | 接口与抽象、文件组织 |
| th-007 | guides | `guides/code-reuse-thinking-guide.md:87-96` | 抽象门槛：同段代码出现 3 次以上才抽；只用一次、一行、抽象比重复更复杂则不抽 | `internal/benchkit/run.go:70-80` | 接口与抽象 |
| th-008 | guides | `guides/code-reuse-thinking-guide.md:108-133` | 由 action/kind/status 派生的状态必须收敛到一个 switch/reducer，不得散落 if/else | `internal/metastore/meta.go:76-82`（段状态枚举集中） | 接口与抽象、文件组织 |
| th-009 | guides | `guides/code-reuse-thinking-guide.md:136-144` | 提交前自查：搜过既有代码、无该共享的拷贝、常量单点、同构模式同结构 | `internal/transport/protocol/protocol_test.go:555-575` | 元规则 |
| th-010 | guides | `guides/code-reuse-thinking-guide.md:193-206` | 新增 `src/templates/trellis/scripts/` 下文件必须同时注册进 `getAllScripts()` | 本仓库无此路径（Trellis 工具仓的规则） | 元规则、文件组织 |
| th-011 | guides | `guides/code-reuse-thinking-guide.md:215-223` | 改 `.trellis/scripts/` 后必须 rsync 同步到模板目录，两边保持逐字一致 | `.trellis/scripts/` 下无 `.py`（本仓只有 `proxy.py`/`proxy_client.py`，非 trellis 脚本） | 元规则、文件组织 |
| th-012 | guides | `guides/cross-layer-thinking-guide.md:22-51` | 跨层改动前必须先画出完整数据流并标出每个边界的格式与校验责任方 | `internal/transport/protocol/protocol.go:1-13`（帧格式契约） | 元规则、接口与抽象 |
| th-013 | guides | `guides/cross-layer-thinking-guide.md:62-66` | 同一件事只在一处校验（入口），不得多层重复校验 | `internal/cluster/kv_tikv.go:134-148`（not-found 归一在 KV 层一次） | 错误处理、接口与抽象 |
| th-014 | guides | `guides/cross-layer-thinking-guide.md:68-72` | 每层只应知道自己的邻居，不得泄漏下层 schema | `internal/metastore/meta.go:117-122`（protocol 刻意不 import metastore） | 依赖方向、接口与抽象 |
| th-015 | guides | `guides/cross-layer-thinking-guide.md:93-101` | append-only 日志 / JSON 流 / RPC payload / 配置文件必须只有一个 owner 负责类型定义、type guard、投影与 reducer | `internal/transport/protocol/protocol.go:76-84`（op 常量 + `OpGetDataFinal` 派生单点） | 接口与抽象、文件组织 |
| th-016 | guides | `guides/cross-layer-thinking-guide.md:101` | 渲染/展示代码只能格式化字段，不得重新定义 payload 契约 | `cmd/taihu/cmd/cluster.go:54-66`（JSON 结构体随命令定义） | 接口与抽象 |
| th-017 | guides | `guides/cross-layer-thinking-guide.md:121-122` | 派生状态必须回指源事件标识（`seq`/`id`/`version`），不得另造游标 | `internal/transport/frame.go:69-71`（`seq` 由 pump 单点分配） | 并发、接口与抽象 |
| th-018 | guides | `guides/cross-layer-thinking-guide.md:128-137` | 改命令模板后必须同步所有平台副本（`.md` 与 `.toml`）并跑跨层检查 | 本仓库无多平台命令模板（Trellis 工具仓规则） | 元规则 |
| th-019 | guides | `guides/cross-layer-thinking-guide.md:143-158` | 改运行时被解析的模板后必须同时验证 fresh init 与 update 两条路径 | 本仓库无此模板体系（Trellis 工具仓规则） | 元规则 |
| th-020 | guides | `guides/cross-layer-thinking-guide.md:162-179` | 改版本化文档时必须确认 MDX 路径、`docs.json` 路由、版本选择器三者同属一条 release line | 本仓库无 docs-site（Trellis 工具仓规则） | 元规则 |
| th-021 | guides | `guides/cross-layer-thinking-guide.md:197-217` | 模式探测类 CLI 必须：探测覆盖所有路径、区分 404 与瞬时错误、瞬时错误不得静默切模式、上下文变化时重置共享状态、快捷路径错误处理质量一致 | `internal/aio/probe_cache.go:23-33`（`probeCached` 探测结论缓存） | 错误处理、接口与抽象 |
| th-022 | guides | `guides/cross-layer-thinking-guide.md:213` | 读元数据必须读完整个响应或用流式解析，不得把固定大小前缀当完整 JSON 解析 | `internal/benchkit/run.go:84-112` | 错误处理、性能 |
| th-023 | guides | `guides/cross-layer-thinking-guide.md:291-300` | 功能跨 3+ 层 / 多方参与 / 数据格式复杂 / 曾出过 bug 时才写 flow 文档 | `doc/设计文档/`（仓库现有落点） | 注释与文档 |

### architecture 层（`architecture/`，6 文件）

| 编号 | 所属层 | 所在文件 | 规则一句话陈述 | 锚定的真实文件:行号 | 管什么类型的决策 |
|---|---|---|---|---|---|
| th-024 | architecture | `architecture/layering.md:27-40` | `internal/` 不得 import `pkg/` | `Makefile:45-50` | 依赖方向 |
| th-025 | architecture | `architecture/layering.md:42-56` | `pkg/` 非测试代码不得 import 引擎五包（`storage\|device\|aio\|bufpool\|layout`） | `Makefile:52-58` | 依赖方向 |
| th-026 | architecture | `architecture/layering.md:58-67` | 提交前必跑 `make check`（= check-fmt + check-layering + check-sdk-only + go vet） | `Makefile:21-22` | 元规则、构建与平台 |
| th-027 | architecture | `architecture/layering.md:71-84` | 分层门禁必须用 grep 源码实现，不得改成 `go list`/AST 方案（宁可误报也不漏报） | `Makefile:38-41` | 构建与平台、元规则 |
| th-028 | architecture | `architecture/layering.md:113-128` | 反向依赖被刻意断开时，必须写清为什么不连、拿什么替代 | `internal/metastore/meta.go:112-122`、`internal/transport/protocol/protocol_test.go:28` | 依赖方向、注释与文档 |
| th-029 | architecture | `architecture/layering.md:132-136` | 跨包类型可见性必须由外部测试包兜底（`package X_test` 逐个命名） | `internal/rpcclient/api_test.go:1`、`:8` | 测试、接口与抽象 |
| th-030 | architecture | `architecture/api-surface.md:7-13` | 包内标识符默认私有，确有外部调用才导出 | `internal/aio/aio.go:135`（`NewWithOptions` 是唯一导出构造入口） | 接口与抽象、命名 |
| th-031 | architecture | `architecture/api-surface.md:15-28` | 只被测试代码引用的符号一律删掉，而不是留作私有 | `internal/aio/aio_internal.go:18`（`errInvalidMaxEvents` 迁出而非留 `aio.go`） | 文件组织、接口与抽象 |
| th-032 | architecture | `architecture/api-surface.md:30-45` | 删符号的判据是「可达」（配置取值 / 返回值类型 / 接口满足）不是「被引用」 | `internal/aio/aio.go:118-119`、`cmd/taihu/cmd/root.go:118`、`internal/aio/aio.go:207`、`internal/device/device.go:82-83` | 接口与抽象、元规则 |
| th-033 | architecture | `architecture/api-surface.md:28` | 删只被测试引用的符号必须先逐符号分析「从包外是否可达」并由人拍板，不得直接开删 | 同类判据见 `internal/aio/aio.go:118-119` | 元规则 |
| th-034 | architecture | `architecture/api-surface.md:47-66` | 对外面文件里只允许出现导出内容；未导出常量/变量/helper 必须迁到别的文件（有两条机械判据） | `internal/aio/aio.go`（判据两条均无输出）、`internal/aio/aio_internal.go:15-19` | 文件组织 |
| th-035 | architecture | `architecture/api-surface.md:68-71` | 接口实现类型上的导出方法名、包文档与 import 块不算违反「对外面只有导出内容」 | `internal/aio/aio_uring_linux.go:320-330` | 文件组织 |
| th-036 | architecture | `architecture/api-surface.md:73-85` | 实现文件按「实现 / 平台 / 职责」分，不按「导出 / 未导出」分 | `internal/aio/`（aio.go / aio_linux.go / aio_internal.go / probe_cache.go 分工） | 文件组织 |
| th-037 | architecture | `architecture/api-surface.md:87` | 对外面集中约定只对新包生效，已有包不要求追溯改造 | `internal/device`（散在 4 文件）、`internal/transport`（9）、`internal/cluster`（7） | 文件组织、元规则 |
| th-038 | architecture | `architecture/api-surface.md:89-111` | 平台分裂符号的缓存必须上移到平台无关的私有文件，对外面只做薄转发 | `internal/aio/probe_cache.go:23-33`、`internal/aio/aio.go:217-219` | 文件组织、接口与抽象 |
| th-039 | architecture | `architecture/api-surface.md:111` | 平台转发层不得为「少一层代码」而删掉——它换来的是平台正确的报错文案 | `internal/aio/aio.go:217-219` vs `internal/aio/probe_other.go:17` | 错误处理、接口与抽象 |
| th-040 | architecture | `architecture/code-style.md:9-17` | 注释默认中文整句，讲「为什么」而不是「是什么」 | `internal/transport/server.go:1`、`internal/rpcclient/dial.go:1` | 注释与文档 |
| th-041 | architecture | `architecture/code-style.md:17` | 引用设计文档时必须带小节号（`§4` / `§5.1` / `§7.1`） | `cmd/taihu/cmd/server.go:1`、`cmd/taihu/cmd/bench_storage.go:1` | 注释与文档 |
| th-042 | architecture | `architecture/code-style.md:19-26` | 只对「这句是约束不是描述」的关键判断加 Markdown 粗体，不得全篇加粗 | `internal/ierr/ierr.go:1`、`internal/rpcclient/reexport.go:19`、`internal/metastore/meta.go:118` | 注释与文档 |
| th-043 | architecture | `architecture/code-style.md:28-37` | 非测试代码里不得出现 TODO / FIXME / XXX / nolint | 全仓 grep 无输出（除 `third_party/`） | 注释与文档、构建与平台 |
| th-044 | architecture | `architecture/code-style.md:39-42` | 非测试代码不得 `panic()`，库代码一律返回 `error` | 全仓 grep 无输出（除 `third_party/` 与 `_test.go`） | 错误处理 |
| th-045 | architecture | `architecture/code-style.md:47-56` | receiver 用单字母，同一类型恒定同一字母 | `internal/storage/storage.go`、`internal/transport/server.go:9`、`internal/aio/aio_uring_linux.go` | 命名 |
| th-046 | architecture | `architecture/code-style.md:58-65` | 首字母缩写全大写，不得写 `ClientId` / `Url` / `Json` | `pkg/taihu-client/config.go:71`、`cmd/taihu/cmd/root.go:36`、`internal/aio/aio_uring_linux.go:75` | 命名 |
| th-047 | architecture | `architecture/code-style.md:67-69` | 平台专有文件用 `_linux` / `_other` 后缀，同一平台两份成对出现 | `internal/aio/aio_uring_linux.go`、`internal/transport/server_shm_linux.go`/`server_shm_other.go` | 命名、构建与平台 |
| th-048 | architecture | `architecture/code-style.md:71` | `_linux` 侧靠后缀即可不写 build tag；`_other` 侧必须显式写 `//go:build !linux` | `internal/aio/aio_other.go:1`；唯一冗余是 `internal/rpcclient/putwriter_linux.go:1` | 构建与平台 |
| th-049 | architecture | `architecture/code-style.md:79-88` | 错误包装统一 `fmt.Errorf("op: %w", err)`，op 前缀小写英文、不加句号 | `internal/cluster/kv_tikv.go:106`、`internal/rpcclient/dial.go:33`、`internal/storage/compact.go:191` | 错误处理 |
| th-050 | architecture | `architecture/code-style.md:87` | 跨库边界的错误用 `taihu: ` 前缀标明来源 | `internal/transport/frame.go:34`、`internal/device/device.go:48` | 错误处理 |
| th-051 | architecture | `architecture/code-style.md:89-93` | 需要操作人照做的环境指引允许例外用中文写 | `internal/aio/aio_uring_linux.go:24,27` | 错误处理、注释与文档 |
| th-052 | architecture | `architecture/code-style.md:95-103` | 面向用户的 CLI 文案用中文；内部包装仍是英文小写前缀 | `cmd/taihu/cmd/key.go:80`、`cmd/taihu/cmd/key.go:95` | 错误处理、命名 |
| th-053 | architecture | `architecture/code-style.md:107-111` | 本仓库只有三条日志通道，新增日志必须落进其中一条，不得开第四条 | `internal/aio/aio_internal.go:33`、`internal/storage/compact.go:72`、`cmd/taihu/cmd/server.go:245` | 日志 |
| th-054 | architecture | `architecture/code-style.md:111-124` | `pingcap/log` 只用来压级别，不得用来打日志（全仓只应有一个 import 点） | `cmd/taihu/cmd/root.go:10`、`root.go:20-22` | 日志 |
| th-055 | architecture | `architecture/code-style.md:125-141` | 诊断输出走标准库 `log`，库内 `log.Printf` 必须带包级/子系统级显式前缀 | `internal/aio/aio_internal.go:33,37`、`internal/aio/aio_uring_linux.go:573`、`internal/storage/compact.go:72,165` | 日志 |
| th-056 | architecture | `architecture/code-style.md:141` | `cmd/taihu/cmd/server.go` 的面运维日志前缀不统一是 CLI/服务端进程入口的特权，库代码不得模仿 | `cmd/taihu/cmd/server.go:139-276` | 日志 |
| th-057 | architecture | `architecture/code-style.md:143-155` | 周期性统计一律用 `[stat]` 前缀，不得混进诊断前缀 | `cmd/taihu/cmd/server.go:245-251` | 日志 |
| th-058 | architecture | `architecture/code-style.md:159-181` | import 三段式分组：标准库 / 第三方（含 `third_party/` fork）/ 本项目，组间空行 | `internal/transport/client.go:5-15`、`cmd/taihu/cmd/key.go:3-14` | 文件组织 |
| th-059 | architecture | `architecture/error-model.md:7-32` | `internal/ierr` 是库错误的唯一事实源，`internal/` 下不得自建错误别名层 | `internal/ierr/ierr.go:1-5`、`:9-22` | 错误处理、依赖方向 |
| th-060 | architecture | `architecture/error-model.md:30` | `ErrConflict` 是 compaction 内部 CAS 控制信号，不对外 | `internal/ierr/ierr.go:20-22`、`internal/storage/compact.go:152` | 错误处理 |
| th-061 | architecture | `architecture/error-model.md:36-50` | 不得新增导出的自定义 error 类型；区分类别用 sentinel + `errors.Is`，带诊断细节用未导出类型 | `internal/aio/aio_uring_linux.go:399`（全仓唯一 `Error struct`） | 错误处理、接口与抽象 |
| th-062 | architecture | `architecture/error-model.md:54-64` | 包内控制信号用未导出 sentinel，命名一律 `err` 前缀 | `internal/aio/aio_internal.go:18`、`internal/transport/frame.go:34`、`internal/device/device.go:48` | 错误处理、命名 |
| th-063 | architecture | `architecture/error-model.md:64` | 未导出 sentinel 不得进包的对外面文件 | `internal/aio/aio_internal.go:18`（不在 `aio.go`） | 文件组织、错误处理 |
| th-064 | architecture | `architecture/error-model.md:66` | sentinel 名字不足以自解释时必须写文档注释 | `internal/aio/aio_internal.go:17`；反例见 `internal/transport/frame.go:34`（无注释，spec 自认） | 注释与文档、错误处理 |
| th-065 | architecture | `architecture/error-model.md:68` | 不同层的平台桩各自持一份同名未导出 sentinel 是刻意的，不得合并到 `ierr` | `internal/transport/server_shm_other.go:15`、`internal/rpcclient/dial_shm_other.go:12` | 错误处理、文件组织 |
| th-066 | architecture | `architecture/error-model.md:72-97` | 跨进程边界只传 4 字节大端 code，不得传 error 字符串 | `internal/transport/protocol/protocol.go:99-153` | 错误处理、接口与抽象 |
| th-067 | architecture | `architecture/error-model.md:99` | `MapStorageErr` + `EncCode` 只允许出现在服务端，`MapCode` 只允许出现在客户端，不得越线 | `internal/transport/server.go:226-406`、`internal/transport/client.go:106-280` | 依赖方向、错误处理 |
| th-068 | architecture | `architecture/error-model.md:103-122` | 对外 re-export 只走两层，且故意不含 `ErrConflict` | `internal/rpcclient/reexport.go:27-42`、`pkg/taihu-client/reexport.go:29-31` | 接口与抽象、错误处理 |
| th-069 | architecture | `architecture/error-model.md:122` | 加错误必须两层一起加，否则外部拿到无法命名的错误值 | `pkg/taihu-client/reexport.go:29-43` | 接口与抽象 |
| th-070 | architecture | `architecture/error-model.md:124-130` | re-export 名单跟着导出签名走：SDK 调得到、能收到的才需要名字 | `internal/rpcclient/putwriter.go:11`、`pkg/taihu-client/storage.go:16-21` | 接口与抽象 |
| th-071 | architecture | `architecture/error-model.md:134-138` | 补 alias 必须有编译期兜底测试，且改 re-export 时同步改它 | `internal/rpcclient/api_test.go:1,8` | 测试、接口与抽象 |
| th-072 | architecture | `architecture/commits.md:7-15` | 标题格式恒为 `type(scope): <中文标题>` | `git log`（143→145 条中仅 2 条非中文标题） | 提交信息 |
| th-073 | architecture | `architecture/commits.md:15` | scope 用包名或子系统名（`internal/aio` → `aio`），一次改多模块并写 | `git log`（`refactor(cli,sdk):`、`fix(storage,transport):`） | 提交信息 |
| th-074 | architecture | `architecture/commits.md:17-27` | 没有单一归属的仓库级改动省略 scope | `fed67b4`、`8e2e67b`、`d65adcc` | 提交信息 |
| th-075 | architecture | `architecture/commits.md:31-46` | 正文讲「为什么改」，不复述 diff；须写对既有结论的影响 | `64eaec3` | 提交信息 |
| th-076 | architecture | `architecture/commits.md:48-65` | 大改动用 `##` 小节 + `-` 开头中文 bullet，bullet 是粗粒度清单不是逐行 diff | `9bcc797` | 提交信息 |
| th-077 | architecture | `architecture/commits.md:67-69` | bullet 里带 `文件:行号` 可接受，行号漂移不必回头修提交信息 | `4ebf370` | 提交信息 |
| th-078 | architecture | `architecture/commits.md:73-97` | 纯重构必须补「行为不变：」核对段，且段内须回答「哪些行为未变」+「拿什么核对的」 | `c50473a`、`522ae0c` | 提交信息 |
| th-079 | architecture | `architecture/commits.md:97` | 写不出核对方式（第 2 类内容）就说明不是纯重构，不该加这段 | `9bcc797`（新增重试行为，故意无此段） | 提交信息 |
| th-080 | architecture | `architecture/commits.md:99-101` | 不得用「行为不变」掩盖行为变化；标题动词要与是否带核对段一致 | `9bcc797` | 提交信息 |
| th-081 | architecture | `architecture/commits.md:105-115` | 顺手修的东西要说明为什么必须在这笔修，否则应拆出去 | `522ae0c` | 提交信息 |
| th-082 | architecture | `architecture/commits.md:119-129` | 本仓库不做自动提交（`session_auto_commit: false`），不得让脚本插入提交 | `.trellis/config.yaml` | 提交信息、构建与平台 |

### cli 层（`cli/`，2 文件）

| 编号 | 所属层 | 所在文件 | 规则一句话陈述 | 锚定的真实文件:行号 | 管什么类型的决策 |
|---|---|---|---|---|---|
| th-083 | cli | `cli/index.md:24` | 新子命令必须挂到正确的父命令下，并在该文件 `init()` 里 `AddCommand` | `cmd/taihu/cmd/root.go:99-107`、`key.go:384`、`instance.go:118` | 文件组织 |
| th-084 | cli | `cli/index.md:25` | 全局参数只加在 `rootCmd.PersistentFlags()`；命令私有参数加自己的 `Flags()` | `cmd/taihu/cmd/root.go:85-97`、`server.go:286-296` | 接口与抽象 |
| th-085 | cli | `cli/index.md:26` | 集群类命令（依赖 `--pd`）入口第一件事是 `requirePD()` | `cmd/taihu/cmd/helpers.go:19-24`、`cluster.go:36-38` | 错误处理 |
| th-086 | cli | `cli/index.md:27` | 参数校验放在 `RunE` 开头并返回 error，不得中途 `os.Exit` | `cmd/taihu/cmd/bench_storage.go:204-221`、`bench_cluster.go:166-183` | 错误处理 |
| th-087 | cli | `cli/index.md:28` | 依赖外部环境的路径必须抽成包级函数变量留测试缝隙 | `cmd/taihu/cmd/helpers.go:29-32`、`helpers.go:59-65`、`bench_storage.go:44-47` | 测试、接口与抽象 |
| th-088 | cli | `cli/index.md:29` | 新输出先写 `--json` 分支，再写人类可读分支 | `cmd/taihu/cmd/cluster.go:81-95`、`key.go:160-164` | 接口与抽象 |
| th-089 | cli | `cli/index.md:30` | 命令行长参数一律双横线 `--x`，并同步 `configs/taihu-server.example.sh` | `cmd/taihu/cmd/bench_storage.go:63`、`configs/taihu-server.example.sh:8-9` | 命名、注释与文档 |
| th-090 | cli | `cli/index.md:31` | 压测工具优先复用 `benchkit.Run`，不得另抄一份执行循环 | `internal/benchkit/run.go:84-112` | 接口与抽象、性能 |
| th-091 | cli | `cli/index.md:38` | 命令树以 `root.go` 的 `AddCommand` 清单为权威，新增顶层命令必须同时改 `main.go` 包注释 | `cmd/taihu/cmd/root.go:99-107`、`cmd/taihu/main.go:1-3` | 文件组织、注释与文档 |
| th-092 | cli | `cli/index.md:52` | 父命令只是分组容器，`RunE`/`Run` 必须留空 | `cmd/taihu/cmd/bench.go:8-27`、`cluster.go:16-24` | 接口与抽象 |
| th-093 | cli | `cli/index.md:54` | 全局参数在 `root.go` 一处定义、经包级 `global` 结构体读取；超时统一 `ctxWithTimeout`、AIO 统一 `aioOptions()` | `cmd/taihu/cmd/root.go:85-97`、`root.go:111-113`、`root.go:117-126` | 接口与抽象、文件组织 |
| th-094 | cli | `cli/index.md:60-73` | CLI 命令单测必须能在「无 TiKV/PD、无真实块设备、固定端口为零」下端到端跑通 `RunE` | `cmd/taihu/cmd/cli_core_test.go:3-6`、`bench_cli_test.go:23-26` | 测试 |
| th-095 | cli | `cli/index.md:75` | `server` 与 `bench cluster` 故意不走 `connectKV`，改这两条路径要替换 `newTiKVKV` | `cmd/taihu/cmd/server.go:110-114`、`bench_cluster.go:80-85` | 接口与抽象、错误处理 |
| th-096 | cli | `cli/index.md:81-88` | 每个有必填项的命令都有一个集中的 `validate()`，`RunE` 拿到 flag 后先调它再干活、不夹带打印 | `cmd/taihu/cmd/bench_storage.go:204-221`、`bench_single.go:126-143`、`bench_cluster.go:166-183` | 错误处理 |
| th-097 | cli | `cli/index.md:96` | 本层所有 `.go` 必须 gofmt 干净；`third_party/` 是例外，改它也别 reformat | `Makefile:24-28` | 构建与平台 |
| th-098 | cli | `command-and-output.md:9-24` | 退出码只有 0（成功）与 1（命令返回 error）两种，由 `run()` 一处决定 | `cmd/taihu/main.go:15-24`、`main.go:26-28` | 错误处理、接口与抽象 |
| th-099 | cli | `command-and-output.md:24` | 不得新增其它退出码，也不得在子命令里调 `os.Exit` | 全仓 `os.Exit` 仅 `cmd/taihu/main.go:27` 与 `helpers.go:122` | 错误处理 |
| th-100 | cli | `command-and-output.md:26-38` | 错误只在 `Execute` 打印一次，格式 `taihu: <err>` 落 stderr | `cmd/taihu/cmd/root.go:76-82`、`root.go:71-72` | 错误处理、日志 |
| th-101 | cli | `command-and-output.md:38` | 子命令内一律 `return fmt.Errorf(...)`，不得顺手 `fmt.Fprintln(os.Stderr, ...)` | `cmd/taihu/cmd/cluster.go:48-51`、`key.go:37-41`、`bench_storage.go:105-109` | 错误处理、日志 |
| th-102 | cli | `command-and-output.md:40` | `printJSON` 的序列化失败是唯一例外，新增输出路径不得效仿 | `cmd/taihu/cmd/helpers.go:119-123` | 错误处理 |
| th-103 | cli | `command-and-output.md:46-56` | tikv client-go 的 `pingcap/log` 必须压到 ErrorLevel 以保 CLI 输出纯净 | `cmd/taihu/cmd/root.go:18-22` | 日志 |
| th-104 | cli | `command-and-output.md:56` | 一次性命令不得新增 `log.Printf` 诊断输出，报错就 `return error`，stdout 只留给命令结果 | `cmd/taihu/cmd/server.go`（全仓仅此文件有 `log.Printf`） | 日志 |
| th-105 | cli | `command-and-output.md:58` | `server` 用标准库 `log` 是例外，不得据此去改 server 的日志 | `cmd/taihu/cmd/server.go:11` | 日志 |
| th-106 | cli | `command-and-output.md:64` | server 每 5 秒打一组 `[stat] ` 前缀统计行，新增周期诊断必须沿用该前缀 | `cmd/taihu/cmd/server.go:239-253` | 日志、性能 |
| th-107 | cli | `command-and-output.md:81` | `--json` 是根级全局开关，由 `printJSON` 一处实现 | `cmd/taihu/cmd/root.go:92`、`helpers.go:117-125` | 接口与抽象 |
| th-108 | cli | `command-and-output.md:83` | 每个命令先写 `if global.json { printJSON(...); return nil }` 再写人类可读分支；JSON 用带 tag 的匿名 struct，不得用 map 拼；漏写 JSON 分支属不完整实现 | `cmd/taihu/cmd/cluster.go:54-66`、`instance.go:23-29`、`client.go:57-69` | 接口与抽象 |
| th-109 | cli | `command-and-output.md:85` | 二进制数据与 JSON 元信息不能共用 stdout | `cmd/taihu/cmd/key.go:186-188`、`key.go:206-225` | 接口与抽象 |
| th-110 | cli | `command-and-output.md:91` | 表格用定宽 `%-Ns` + 一行表头；字节数一律过 `humanBytes`，不得自己除 1024；空结果打一行说明 | `cmd/taihu/cmd/cluster.go:89-95`、`helpers.go:128-139` | 接口与抽象、命名 |
| th-111 | cli | `command-and-output.md:97` | `benchkit.Store` 是压测数据面的唯一抽象，新压测工具不得另抄执行循环 | `internal/benchkit/run.go:36-43` | 接口与抽象、性能 |
| th-112 | cli | `command-and-output.md:99` | 报告标题与进度行格式由 benchkit 统一，工具名与端点由调用方传入 | `internal/benchkit/run.go:74`、`bench_single.go:88`、`bench_cluster.go:122` | 接口与抽象、命名 |
| th-113 | cli | `command-and-output.md:101` | `bench storage` 是唯一不走 benchkit 的压测命令（签名不匹配导致的既有重复），不得产出第三份副本 | `cmd/taihu/cmd/bench_storage.go:283-359` | 接口与抽象 |
| th-114 | cli | `command-and-output.md:103` | 压测结论落到 `doc/性能测试报告/`，文件名 `<YYYYMMDDHHMM>_<commit7>_<标题>.md`；报告文件不移动、不重命名，并同步 `doc/README.md` 索引 | `doc/README.md:48`、`doc/性能测试报告/` | 命名、注释与文档 |

### engine 层（`engine/`，3 文件）

| 编号 | 所属层 | 所在文件 | 规则一句话陈述 | 锚定的真实文件:行号 | 管什么类型的决策 |
|---|---|---|---|---|---|
| th-115 | engine | `engine/index.md:20` | `layout` 必须独立成包以打断 `device` 与 `metastore` 的循环依赖 | `internal/layout/layout.go:1-2` | 依赖方向 |
| th-116 | engine | `engine/index.md:21` | 异步 IO 的唯一出口是 `internal/aio`，任何新代码不得绕过它直接 syscall | `internal/aio/aio.go:10-11` | 接口与抽象、依赖方向 |
| th-117 | engine | `engine/index.md:22` | 整盘段数不得硬编码，必须由启动时读到的真实容量经 `ComputeLayout` 计算后注入 | `internal/layout/layout.go:6-7`、`:22-23` | 构建与平台、接口与抽象 |
| th-118 | engine | `buffer-and-concurrency.md:9-32` | buf 在 `Submit` 之后、对应完成事件被 `Wait` 取回之前必须保持存活且不被改写 | `internal/aio/aio.go:19-21`、`:52`、`:76`、`:85`、`internal/device/device.go:181`、`:445-450` | 资源生命周期、并发 |
| th-119 | engine | `buffer-and-concurrency.md:34-51` | 读路径返回的池化缓冲必须由调用方 `bufpool.Put` 归还，否则池泄漏 | `internal/storage/storage.go:221-222`、`internal/device/device.go:571-572`、`:396-397`、`internal/storage/compact.go:186-189` | 资源生命周期 |
| th-120 | engine | `buffer-and-concurrency.md:53-81` | 远程 `Get` 返回 `(data, release, err)`，`release` 必须调用且必须幂等 | `internal/rpcclient/objectstore.go:22-26`、`internal/transport/client.go:148-156` | 资源生命周期、接口与抽象 |
| th-121 | engine | `buffer-and-concurrency.md:83-93` | `bufpool` 必须用自管理 freelist 而非 `sync.Pool`（GC 清空会导致大缓冲整批丢弃） | `internal/bufpool/bufpool.go:10-14` | 性能、资源生命周期 |
| th-122 | engine | `buffer-and-concurrency.md:97` | 每桶驻留上限 `maxKeep = 32`，超出上限的归还缓冲直接丢弃交 GC | `internal/bufpool/bufpool.go:28-30`、`:103`、`:117` | 资源生命周期、性能 |
| th-123 | engine | `buffer-and-concurrency.md:98` | 分桶按 2 的幂，下限 4KB（`BlockSize`）、上限 8GB（`maxBufBucket`） | `internal/bufpool/bufpool.go:24-27` | 性能、接口与抽象 |
| th-124 | engine | `buffer-and-concurrency.md:99-106` | 分桶函数的前置条件是 `n > 0`；两个调用方必须在前置处挡住 `n<=0` | `internal/bufpool/bufpool.go:74-77` | 错误处理、并发 |
| th-125 | engine | `buffer-and-concurrency.md:108` | 对齐分配必须用三索引切片固定 `cap == n`，否则 Put 会落进高一档桶 | `internal/bufpool/bufpool.go:138-140` | 性能、资源生命周期 |
| th-126 | engine | `buffer-and-concurrency.md:109` | 全部桶必须在包初始化时一次性建好，避免运行期并发懒初始化触发 `-race` | `internal/bufpool/bufpool.go:46-48` | 并发、资源生命周期 |
| th-127 | engine | `buffer-and-concurrency.md:110` | 精确尺寸池（`GetExact`/`PutExact`）按 `len==cap` 分桶，不得按 2 幂取整 | `internal/bufpool/bufpool.go:153-158` | 性能、接口与抽象 |
| th-128 | engine | `buffer-and-concurrency.md:112-131` | 磁盘 IO 必须由单一完成泵 goroutine 串行取回完成事件；`ring.Wait` 全仓只能有一个持有者 | `internal/device/device.go:8-10`、`:145-148`、`:152`、`:110` | 并发 |
| th-129 | engine | `buffer-and-concurrency.md:135` | 完成泵空闲轮询周期固定 200ms（`pumpTimeout`），有事件时 `min=1` 立即返回 | `internal/device/device.go:34-36` | 性能、并发 |
| th-130 | engine | `buffer-and-concurrency.md:136` | 提交前必须在锁内查 `closed` 并自增 `inSubmit`，使泵不会在事件未落定窗口内退出 | `internal/device/device.go:183-186` | 并发、资源生命周期 |
| th-131 | engine | `buffer-and-concurrency.md:137` | 提交方锁内消费 `pending` 或注册 `m[seq] = ch`，两条路径都不得永久阻塞 | `internal/device/device.go:232-241` | 并发 |
| th-132 | engine | `buffer-and-concurrency.md:138` | `batchWait` 必须镜像同一套 pending/m 语义，并多一个 `<-d.pumpDone` 出口 | `internal/device/device.go:761-788` | 并发 |
| th-133 | engine | `buffer-and-concurrency.md:139` | `Device.mu` 保护的字段必须在声明处写清（`m`/`pending`/`inSubmit`/`closed`） | `internal/device/device.go:57` | 并发、注释与文档 |
| th-134 | engine | `buffer-and-concurrency.md:140` | 完成侧瞬时错误按原参数重提，上限 `complRetryMax = 8`，退避从 100µs 起翻倍且不超 `complRetryCap = 2ms` | `internal/device/device.go:39-42`、`:276-286` | 错误处理、性能 |
| th-135 | engine | `buffer-and-concurrency.md:146-148` | `Submit*` 可由多 goroutine 并发调用（由 `mu` 串行化，单生产者填 SQE）；`Wait` 由单一完成泵持有 | `internal/aio/aio_uring_linux.go:151-152`、`:158` | 并发 |
| th-136 | engine | `buffer-and-concurrency.md:151` | 「单生产者填 SQE」靠 `mu` 保护 `seq`/`sqeTail`/`inflight`/`closed` | `internal/aio/aio_uring_linux.go:158` | 并发 |
| th-137 | engine | `buffer-and-concurrency.md:152` | `sq_head` 由内核推进必须原子读；`tail-head` 差值恒 ≤ `sq_entries`，不得取模 | `internal/aio/aio_uring_linux.go:428-432` | 并发 |
| th-138 | engine | `buffer-and-concurrency.md:153` | SQ tail 的原子写必须是 release 语义（SQE 字节先于 tail 对内核可见） | `internal/aio/aio_uring_linux.go:525` | 并发 |
| th-139 | engine | `buffer-and-concurrency.md:154` | `cq.tail` 原子读即 acquire；`cqHead` 用 release 语义把 CQ 空间还给内核 | `internal/aio/aio_uring_linux.go:576-584` | 并发 |
| th-140 | engine | `buffer-and-concurrency.md:155` | 未消费的 SQE 必须 `rewind` 撤销发布，否则会被重复提交 | `internal/aio/aio_uring_linux.go:554-555` | 并发、错误处理 |
| th-141 | engine | `buffer-and-concurrency.md:156` | `EINTR` 等提交失败绝不能盲目重试，必须以内核推进的 `sq_head` 为准 | `internal/aio/aio_uring_linux.go:540-543` | 错误处理、并发 |
| th-142 | engine | `buffer-and-concurrency.md:157` | io_uring 在 `Close` 里必须先置 `closed` 再解映射，否则 SIGSEGV | `internal/aio/aio_uring_linux.go:646-648` | 资源生命周期、并发 |
| th-143 | engine | `buffer-and-concurrency.md:161-172` | `Ring` 接口契约：同一 Ring 可多 goroutine 并发 `Submit`，`Wait` 应串行调用 | `internal/aio/aio.go:66-67` | 并发、接口与抽象 |
| th-144 | engine | `buffer-and-concurrency.md:170` | `ErrFull` 表示提交队列满，调用方应先 `Wait` 取回完成事件后重试 | `internal/aio/aio.go:38-39` | 错误处理、并发 |
| th-145 | engine | `buffer-and-concurrency.md:171` | `Wait` 事件顺序不保证与提交顺序一致，必须靠 `Event.Data` 关联 | `internal/aio/aio.go:88-91` | 并发、接口与抽象 |
| th-146 | engine | `buffer-and-concurrency.md:172` | 批量提交可能被内核截断，调用方必须把未排队部分追加提交 | `internal/aio/aio.go:73-77`、`:82-86` | 并发、接口与抽象 |
| th-147 | engine | `buffer-and-concurrency.md:173-184` | `device` 对 `ErrFull` 让出 `submitRetry = 100µs` 后整块重试，不得推进 seq，并先撤回预增的 `inSubmit` | `internal/device/device.go:37-38`、`:521-527` | 并发、错误处理 |
| th-148 | engine | `buffer-and-concurrency.md:188-198` | metastore 中用 `*Locked` 后缀标记「调用方须持有锁」的方法 | `internal/metastore/segments.go:159,169,175,219`、`kv_pebble.go:370,385` | 命名、并发 |
| th-149 | engine | `buffer-and-concurrency.md:201` | 锁序固定 `allocator.mu` → `segmentManager.mu`，不得反向 | `internal/metastore/segments.go:26` | 并发 |
| th-150 | engine | `buffer-and-concurrency.md:202-207` | 需要跨两个锁时必须取快照后释放，不得嵌套持有 | `internal/metastore/kv_pebble.go:555-556` | 并发 |
| th-151 | engine | `buffer-and-concurrency.md:209` | 持锁只覆盖廉价部分，真正的设备写由调用方在锁外执行 | `internal/metastore/kv_pebble.go:336-337`、`internal/storage/storage.go:17-18` | 并发、性能 |
| th-152 | engine | `buffer-and-concurrency.md:213` | TCP 路径的零拷贝移交（`netpoll.TakeTry`）已停用，shm 单帧移交不受影响 | `internal/transport/client.go:117-120` | 性能、错误处理 |
| th-153 | engine | `metadata-and-compaction.md:16-21` | pebble 无列族，两个逻辑命名空间必须用 key 前缀隔离（`mapping = "m\x00"`、`state = "s\x00"`） | `internal/metastore/kv_pebble.go:26-29` | 文件组织、接口与抽象 |
| th-154 | engine | `metadata-and-compaction.md:19` | 用户 key 必须整体映射到 mapping 前缀之下，避免与内部键冲突 | `internal/metastore/kv_pebble.go:24-25` | 接口与抽象 |
| th-155 | engine | `metadata-and-compaction.md:21` | 迭代区间一律用「前缀末字节 +1」求独占上界 | `internal/metastore/kv_pebble.go:86-91` | 接口与抽象 |
| th-156 | engine | `metadata-and-compaction.md:23-32` | 编码固定 little-endian，每个 value 头部 1 字节 version 以便演进 | `internal/metastore/meta.go:13-18` | 接口与抽象 |
| th-157 | engine | `metadata-and-compaction.md:34` | 三个 `decodeXxx` 必须以「长度不等于常量则报错」作为第一道校验 | `internal/metastore/meta.go:37-40`、`:63-66`、`:101-104` | 错误处理 |
| th-158 | engine | `metadata-and-compaction.md:35` | pebble 写入默认 `Sync`，保证「先写设备数据 → 写 mapping → 更新 cursor」的持久化顺序 | `internal/metastore/kv_pebble.go:22`、`:65` | 资源生命周期、错误处理 |
| th-159 | engine | `metadata-and-compaction.md:39` | 领域态与 wire 态必须分开，不得让 `protocol` import `metastore`，平齐性由一致性测试守 | `internal/metastore/meta.go:117-122`、`internal/transport/protocol/protocol_test.go:28` | 依赖方向、测试 |
| th-160 | engine | `metadata-and-compaction.md:43` | `Store` 的每个方法都必须有中文语义注释 | `internal/metastore/store.go:20-86` | 注释与文档 |
| th-161 | engine | `metadata-and-compaction.md:45` | `GetMapping` 在 key 不存在时必须返回 `ierr.ErrNotFound` | `internal/metastore/store.go:21` | 错误处理 |
| th-162 | engine | `metadata-and-compaction.md:46-55` | `BatchGetMapping` 结果按入参 key 顺序返回，任一 key 缺失整体返回 `ierr.ErrNotFound` | `internal/metastore/store.go:26-29`、`kv_pebble.go:167-215`、`internal/storage/storage.go:372-381` | 接口与抽象 |
| th-163 | engine | `metadata-and-compaction.md:57` | `BatchDeleteMapping` 返回 per-key 错误，整体存储错误经第二个返回值暴露 | `internal/metastore/store.go:36-38` | 错误处理 |
| th-164 | engine | `metadata-and-compaction.md:58` | `AllocateSegment` 返回的偏移恒 4K 对齐、单调不重叠，可并发调用 | `internal/metastore/store.go:43-45` | 并发、接口与抽象 |
| th-165 | engine | `metadata-and-compaction.md:59` | `AllocateSegmentReserve` 可动用预留缓冲段；用户路径不得使用 | `internal/metastore/store.go:50-52` | 接口与抽象、资源生命周期 |
| th-166 | engine | `metadata-and-compaction.md:60` | `MarkCompacting` 在段不存在或非 `Full` 时是空操作（返回 nil） | `internal/metastore/store.go:59-61` | 错误处理 |
| th-167 | engine | `metadata-and-compaction.md:61` | `MoveMapping` 是条件写：仅当当前 mapping 与 `old` 完全一致才原子切换并转移存活计数，否则返回 `ErrConflict` 且不改数据 | `internal/metastore/store.go:63-66`、`kv_pebble.go:504-533` | 并发、错误处理 |
| th-168 | engine | `metadata-and-compaction.md:62` | `ListSegments` 枚举内存引用表而不是 pebble 全扫 | `internal/metastore/store.go:68-69` | 性能 |
| th-169 | engine | `metadata-and-compaction.md:63` | `Cursor` 在尚无写入时必须返回 `(0, 0)` | `internal/metastore/store.go:70-71` | 接口与抽象 |
| th-170 | engine | `metadata-and-compaction.md:64` | `RefSegment`/`UnrefSegment` 必须在读开始前/结束后调用；仅在引用归零时才允许 Reclaiming 段回收复用 | `internal/metastore/store.go:73-76`、`segments.go:21-22` | 资源生命周期、并发 |
| th-171 | engine | `metadata-and-compaction.md:65` | `UsedBytes` 的 Full/Reclaiming 段计整段、Active 段计已写偏移 | `internal/metastore/store.go:81-83` | 接口与抽象 |
| th-172 | engine | `metadata-and-compaction.md:81-86` | 段状态迁移链固定 `Free → Active → Full → Reclaiming → Free` | `internal/metastore/segments.go:19-20`、`meta.go:76-82` | 接口与抽象 |
| th-173 | engine | `metadata-and-compaction.md:90` | `AliveCount` 必须与 mapping 写入同一 pebble WriteBatch 原子持久化，启动时全量扫描重建 | `internal/metastore/segments.go:15-17` | 并发、资源生命周期 |
| th-174 | engine | `metadata-and-compaction.md:92` | 空闲池 `free` 是 FIFO，供 allocator 在游标到顶时取段复用 | `internal/metastore/segments.go:23` | 资源生命周期 |
| th-175 | engine | `metadata-and-compaction.md:93` | 后台 GC goroutine 周期固定 `gcInterval = 1s` | `internal/metastore/segments.go:43`、`:135-151` | 性能、资源生命周期 |
| th-176 | engine | `metadata-and-compaction.md:99` | `Free`/`Reclaiming` 段被写入新对象必须重新激活；`Free` 还需先移出空闲池 | `internal/metastore/segments.go:184-190` | 资源生命周期 |
| th-177 | engine | `metadata-and-compaction.md:100` | `markCompacting` 仅 `Full` 段可进搬移 | `internal/metastore/segments.go:298-310` | 资源生命周期 |
| th-178 | engine | `metadata-and-compaction.md:101` | 重启自愈：`AliveCount>0` 的 `Compacting` 段回退 `Full`，`==0` 直接转 `Reclaiming` | `internal/metastore/segments.go:89-101` | 资源生命周期、错误处理 |
| th-179 | engine | `metadata-and-compaction.md:102` | 不一致自愈：段标记 `Free` 但 mapping 仍引用 → 以 mapping 为准回退 `Active` 并移出空闲池 | `internal/metastore/segments.go:116-120` | 错误处理、资源生命周期 |
| th-180 | engine | `metadata-and-compaction.md:103` | 重建存活计数时必须丢弃持久化的 `AliveCount`（置 0） | `internal/metastore/segments.go:71-72` | 错误处理 |
| th-181 | engine | `metadata-and-compaction.md:104-105` | `popFree`/`reclaimOnce` 在持久化失败时必须回滚或留待下轮 GC 重试 | `internal/metastore/segments.go:324-329`、`:365-368` | 错误处理、资源生命周期 |
| th-182 | engine | `metadata-and-compaction.md:106` | `Ref`/`Unref` 对未知 segmentID 必须静默忽略；`Unref` 还要求 `refCount > 0` | `internal/metastore/segments.go:246-261` | 错误处理、资源生命周期 |
| th-183 | engine | `metadata-and-compaction.md:118-126` | 段滚动策略：下一段空闲则顺序滚动（用户路径上限 `SegmentCount−reserveSegs`）；被占用或到顶则从空闲池取；池空返回 `ErrNoSpace` | `internal/metastore/kv_pebble.go:423-427` | 资源生命周期、错误处理 |
| th-184 | engine | `metadata-and-compaction.md:128` | 预留段 `reserveSegs = 2` 只给 compaction 用，不参与用户写路径顺序滚动 | `internal/metastore/kv_pebble.go:67-70` | 资源生命周期 |
| th-185 | engine | `metadata-and-compaction.md:129` | 切换段时必须把旧段标记 `Full`、新段激活 | `internal/metastore/kv_pebble.go:429`、`:389-411` | 资源生命周期 |
| th-186 | engine | `metadata-and-compaction.md:130` | `AllocateSegmentBatch` 一次锁定分配器、逐项 `allocOneLocked`，全程只做一次游标持久化 | `internal/metastore/kv_pebble.go:454-456`、`:465-478` | 性能、并发 |
| th-187 | engine | `metadata-and-compaction.md:131` | 普通路径每次分配都要单独持久化游标 | `internal/metastore/kv_pebble.go:443-445` | 资源生命周期 |
| th-188 | engine | `metadata-and-compaction.md:135` | `ierr.ErrConflict` 必须当控制信号：跳过该 key 下轮重扫，不得原地重试同一次 CAS | `internal/storage/compact.go:150-157` | 并发、错误处理 |
| th-189 | engine | `metadata-and-compaction.md:161` | `moveObject` 对 `ierr.ErrNotFound` 也返回「跳过」 | `internal/storage/compact.go:175-180` | 错误处理 |
| th-190 | engine | `metadata-and-compaction.md:162` | CAS 冲突时数据不得丢：新位置数据已落盘，孤儿块由 segment GC 兜底回收 | `internal/storage/compact.go:202-204` | 资源生命周期、错误处理 |
| th-191 | engine | `metadata-and-compaction.md:167` | Compaction 默认配置：60s 扫描、空洞率 ≥ 0.8 或水位 ≥ 0.8 触发、每轮 ≤ 512 对象 | `internal/storage/compact.go:26-34` | 性能 |
| th-192 | engine | `metadata-and-compaction.md:181-184` | 候选段按空洞率降序（同率按段号升序保证确定性）；`MaxMovePerRound` 跨段合并记账 | `internal/storage/compact.go:124-130`、`:132-136` | 性能 |
| th-193 | engine | `metadata-and-compaction.md:188` | 搬移顺序固定：读旧位置 → 分配新位置 → 写设备（先数据）→ CAS 切映射（后元数据） | `internal/storage/compact.go:170-172` | 资源生命周期、错误处理 |
| th-194 | engine | `metadata-and-compaction.md:189` | 搬移的新落点必须用 `AllocateSegmentReserve` | `internal/storage/compact.go:194-198` | 资源生命周期 |
| th-195 | engine | `metadata-and-compaction.md:190` | 读到的长度不足 `meta.Size` 必须报错返回，不得搬一个残缺对象 | `internal/storage/compact.go:190-192` | 错误处理 |
| th-196 | engine | `metadata-and-compaction.md:193` | `compactOnce` 可并发调用（同进程内唯一定时器驱动）；`Stop` 必须 `close(c.stop)` + `wg.Wait()` 后才返回 | `internal/storage/compact.go:81`、`:57-61` | 并发、资源生命周期 |

### platform 层（`platform/`，3 文件）

| 编号 | 所属层 | 所在文件 | 规则一句话陈述 | 锚定的真实文件:行号 | 管什么类型的决策 |
|---|---|---|---|---|---|
| th-197 | platform | `platform/file-splitting.md:7-26` | 平台命名只用 `_linux` / `_other` 成对，不得用 `_darwin` / `_windows` / `_unix` / `_bsd`；非 Linux 侧统一收敛到一个 `_other.go` | `internal/aio/`、`internal/device/`、`internal/rpcclient/`、`internal/transport/` 共 10 个 `_linux.go` + 7 个 `_other.go` | 命名、构建与平台 |
| th-198 | platform | `platform/file-splitting.md:28-45` | `_other.go` 必须显式写 `//go:build !linux`；`_linux.go` 多数只靠后缀 | `internal/aio/aio_other.go:1`；唯一冗余 `internal/rpcclient/putwriter_linux.go:1` | 构建与平台 |
| th-199 | platform | `platform/file-splitting.md:49-61` | `_other` 侧不是 stub，而是同语义降级实现（能给兜底就必须给） | `internal/aio/aio.go:13-17`、`aio_linux.go:56` vs `aio_other.go:31`、`device_linux.go:15-17` vs `device_other.go:9-11` | 接口与抽象、构建与平台 |
| th-200 | platform | `platform/file-splitting.md:63-96` | 做不到同语义的能力，`_other` 侧必须报错且报得能被上层运行期判掉 | `internal/aio/aio_other.go:30-44`、`probe_other.go:7-11`、`internal/transport/server_shm_other.go:14-20` | 错误处理、接口与抽象 |
| th-201 | platform | `platform/file-splitting.md:98-113` | 不同层平台桩的未导出 sentinel 各持一份是刻意的，不得为消重合并进 `ierr` | `internal/transport/server_shm_other.go:15`、`internal/rpcclient/dial_shm_other.go:12` | 错误处理、文件组织 |
| th-202 | platform | `platform/file-splitting.md:115-136` | 平台分文件不等于调用方要加 build tag：上层主文件必须保持无 tag，靠运行期布尔/哨兵降级 | `internal/transport/server_shm_other.go:3-4`、`cmd/taihu/cmd/server.go:210-228` | 构建与平台、接口与抽象 |
| th-203 | platform | `platform/file-splitting.md:138-140` | 确实无对应面的能力允许只有 `_linux.go`，判据是「非 Linux 上有没有必要存在同名符号」 | `internal/aio/aio_uring_linux.go`、`internal/transport/client_shm_linux.go`、`shm_frame_linux.go` | 文件组织 |
| th-204 | platform | `platform/file-splitting.md:142-161` | `_test.go` 后缀必须排在 GOOS 之后；GOOS 不紧邻 `_test` 时必须补显式 `//go:build linux` | `internal/aio/aio_linux_test.go:1`、`aio_linux_more_test.go:1`、`mode_test.go:1`、`transport_shm_test.go:1` | 测试、构建与平台 |
| th-205 | platform | `platform/file-splitting.md:163-174` | `third_party/` 免 gofmt，改它下任何文件都不要顺手 reformat | `Makefile:24-28` | 构建与平台、注释与文档 |
| th-206 | platform | `platform/file-splitting.md:176-188` | `third_party/{netpoll,shmipc-go}` 必须并进主模块，不得用嵌套 `go.mod` + `replace` | `go.mod:18-27`、`third_party/README.md:6-24` | 依赖方向、构建与平台 |
| th-207 | platform | `platform/build-verification.md:7-26` | `make check-linux` 就是四条命令（linux/amd64 vet、build、arm64 build、`test -c`），不得增删 | `Makefile:65-70` | 构建与平台 |
| th-208 | platform | `platform/build-verification.md:28-50` | 改任何平台相关代码后必须跑 `make check-linux`（darwin 上平台错误是隐形的） | `Makefile:60-64` | 构建与平台、元规则 |
| th-209 | platform | `platform/build-verification.md:52-90` | `CGO_ENABLED=0` 必须与 `GOOS=linux` 成对出现，不得单独在本机加 | `Makefile:66-69` | 构建与平台 |
| th-210 | platform | `platform/build-verification.md:92-108` | 本机 `go test ./...` 的 7 个平台性失败不得当成自己的破坏，也不得为「让本机全绿」去改它们 | `internal/device/info_other.go:14`、`cmd/taihu/cmd/bench_cli_test.go:249`、`:378` | 测试、元规则 |
| th-211 | platform | `platform/build-verification.md:110-126` | Linux 测不了的断言必须抽到无 tag 的契约测试里；平台门控只用在「提供后端清单」的文件上 | `internal/aio/aio_linux_test.go`、`aio_backends_linux_test.go`、`aio_backends_other_test.go:7-14` | 测试 |
| th-212 | platform | `platform/build-verification.md:128-132` | 改 fork 或升 Go 版本时，`check-linux` 是 vet 新问题的第一发现点 | `third_party/README.md:173-175`、`:139-153` | 构建与平台 |
| th-213 | platform | `platform/build-verification.md:134-142` | 动过 fork 后验收口径是 `make check && make check-linux && go test ./...`，且**不能只靠门禁**——shm 协议是否还通必须上 Linux 真机做一次真实 Put/Get 往返 | `third_party/README.md:182-194`、`:162-166` | 构建与平台、测试 |

### sdk 层（`sdk/`，2 文件）

| 编号 | 所属层 | 所在文件 | 规则一句话陈述 | 锚定的真实文件:行号 | 管什么类型的决策 |
|---|---|---|---|---|---|
| th-214 | sdk | `sdk/index.md:9` | `pkg/` 下只允许存在 `taihu-client` 这一个包 | `ls pkg/` 仅 `taihu-client/` | 文件组织、依赖方向 |
| th-215 | sdk | `sdk/index.md:10-21` | 目录名 `pkg/taihu-client` 与包名 `taihuclient` 的不一致必须保持，调用方只能用包名；改包名 = 改对外契约 | `pkg/taihu-client/config.go:7`、`examples/taihu-client/main.go:25` | 命名、接口与抽象 |
| th-216 | sdk | `sdk/index.md:27-38` | SDK 非测试代码只允许 import `internal/{cluster,rpcclient,version}` 三个内部包 | `pkg/taihu-client/storage.go:11-13`、`reexport.go:4-5` | 依赖方向 |
| th-217 | sdk | `sdk/index.md:40-57` | `make check-sdk-only` 是这条边界的机器强制，必须用 grep 而非 `go list` | `Makefile:52-58` | 构建与平台、依赖方向 |
| th-218 | sdk | `sdk/index.md:59-61` | `internal/` 不得依赖 `pkg/`（反向同守） | `Makefile:45-50` | 依赖方向 |
| th-219 | sdk | `sdk/index.md:63-65` | SDK 层不得有平台分裂文件；同机 shm / 跨节点 TCP 的差异靠运行期比较 hostname | `pkg/taihu-client/storage.go:149` | 构建与平台 |
| th-220 | sdk | `sdk/public-api.md:9-23` | `NewFromTiKV` 与 `NewCluster` 是仅有的两个构造入口，都返回同一个 `*Storage` | `pkg/taihu-client/tikv.go:39-44`、`storage.go:52-55` | 接口与抽象 |
| th-221 | sdk | `sdk/public-api.md:15` | `NewFromTiKV` 必须校验 `PDAddrs` 非空，否则返回错误 | `pkg/taihu-client/tikv.go:39-44` | 错误处理 |
| th-222 | sdk | `sdk/public-api.md:21-23` | 两个构造函数都返回必须 `Close()` 的对象，且 `NewCluster` 成功后不需要调用方再启动任何东西 | `pkg/taihu-client/storage.go:67-69`、`:339-360` | 资源生命周期 |
| th-223 | sdk | `sdk/index.md:88` | 新增后台 goroutine 时必须确认 `Close()` 能停掉它 | `pkg/taihu-client/storage.go:340-348` | 资源生命周期、并发 |
| th-224 | sdk | `sdk/public-api.md:27` | `Storage` 是唯一的公开客户端类型 | `pkg/taihu-client/storage.go:29-45` | 接口与抽象 |
| th-225 | sdk | `sdk/public-api.md:37-44` | `Storage` 的数据面五方法必须正好等于 `rpcclient.ObjectStore`，并有编译期断言钉住 | `pkg/taihu-client/storage.go:47-49` | 接口与抽象、测试 |
| th-226 | sdk | `sdk/public-api.md:58` | `Get` 返回的 `release` 必须调用；调用方统一 `defer release()`（两条路径都安全） | `pkg/taihu-client/storage.go:198-201`、`:294` | 资源生命周期 |
| th-227 | sdk | `sdk/public-api.md:59` | `key` 不可变、无覆盖写；更新语义 = `Delete` 后重建 | `pkg/taihu-client/storage.go:27`、`examples/taihu-client/main.go:74-75` | 接口与抽象 |
| th-228 | sdk | `sdk/public-api.md:60` | 索引是尽力而为（异步批量写、队列满即丢），读 miss 由回源兜底；没有配 `Source` 时索引丢过的 key 读不到 | `pkg/taihu-client/index.go:78-84`、`storage.go:278-280` | 错误处理、性能 |
| th-229 | sdk | `sdk/public-api.md:68` | `ClusterConfig` 与 `TiKVOptions` 字段一一对应，新增字段两边都要改（漏一个静默丢配置） | `pkg/taihu-client/config.go:41-76`、`tikv.go:51-64` | 接口与抽象、文件组织 |
| th-230 | sdk | `sdk/public-api.md:70-79` | 对外可写字符串的枚举常量集中声明，调用方应引用常量而非字面量 | `pkg/taihu-client/config.go:21-28`、`:31-38` | 命名、接口与抽象 |
| th-231 | sdk | `sdk/public-api.md:82-91` | re-export 只转出出现在导出签名/调用方解读路径上的 internal 类型与错误，用 type alias | `pkg/taihu-client/reexport.go:4-43` | 接口与抽象 |
| th-232 | sdk | `sdk/public-api.md:93-96` | 故意不转 `ObjectMeta`/`SegmentEntry`/`SegmentSummary`/`SegmentState`/段状态常量/`ErrConflict` | `pkg/taihu-client/storage.go:181-339`（签名只用 `[]byte`/`int64`/`error`） | 接口与抽象 |
| th-233 | sdk | `sdk/public-api.md:98-100` | 补 alias 的维护约定：签名里出现 internal 类型就补一条 | `pkg/taihu-client/reexport.go:8-17` | 接口与抽象 |
| th-234 | sdk | `sdk/public-api.md:104` | SDK 必须把底层错误原样透出、不做包装，使 `errors.Is` 直接可用 | `pkg/taihu-client/reexport.go:29-31`、`examples/taihu-client/main.go:109-113` | 错误处理 |
| th-235 | sdk | `sdk/public-api.md:114` | SDK 自有错误自己定义：`ErrNoInstances` 导出、`errSourceUnset` 未导出 | `pkg/taihu-client/storage.go:18`、`:20` | 错误处理、命名 |
| th-236 | sdk | `sdk/public-api.md:117-131` | `examples/taihu-client/` 不是文档而是编译期约束，改能力面必须同步接口与断言 | `examples/taihu-client/main.go:28-40` | 测试、接口与抽象 |
| th-237 | sdk | `sdk/public-api.md:137-148` | 共享 fake 用「嵌入接口 + 按需注入错误」而非完整实现，且只允许出现在 `_test.go` | `pkg/taihu-client/testutil_test.go:13-22`、`:24-57` | 测试 |
| th-238 | sdk | `sdk/public-api.md:150` | 构造 + 清理必须粘在 helper 里（`t.Cleanup(func() { _ = s.Close() })`），用例不收尾 | `pkg/taihu-client/testutil_test.go:59-72` | 测试、资源生命周期 |
| th-239 | sdk | `sdk/public-api.md:151` | 实例注册与快照刷新必须成对（写完 KV 立即 `refresh()`） | `pkg/taihu-client/testutil_test.go:74-88` | 测试 |
| th-240 | sdk | `sdk/public-api.md:152` | 外部后端必须走间接层变量而不是真连 | `pkg/taihu-client/tikv.go:34-37`、`tikv_test.go:22-23` | 测试、接口与抽象 |

### testing 层（`testing/`，3 文件）

| 编号 | 所属层 | 所在文件 | 规则一句话陈述 | 锚定的真实文件:行号 | 管什么类型的决策 |
|---|---|---|---|---|---|
| th-241 | testing | `testing/index.md:9` | 单测必须与实现同目录同包，不得建独立测试目录 | `internal/storage/storage.go:1` / `storage_test.go:1`、`internal/metastore/meta.go:1` / `meta_test.go:1` | 测试、文件组织 |
| th-242 | testing | `testing/unit-tests.md:11-20` | 只有为验证「对外名字可命名」才允许外置测试包，且必须在文件头写明破例理由 | `internal/rpcclient/api_test.go:1`、`:11-20` | 测试、注释与文档 |
| th-243 | testing | `testing/unit-tests.md:32-53` | 断言一律手写 `if got != want { t.Fatalf(...) }`，不得 import testify | `internal/layout/layout_test.go:21-26` | 测试、依赖方向 |
| th-244 | testing | `testing/unit-tests.md:55` | 失败消息必须带实际值与期望值两侧（`<what>: %v, want %v`） | `internal/transport/protocol/protocol_test.go:81-103` | 测试 |
| th-245 | testing | `testing/unit-tests.md:77-114` | 逻辑/纯函数测试必须写成表驱动 `cases := []struct{...}` + `t.Run(`，用例名是中文短句 | `internal/layout/layout_test.go:8-20`、`:49-63` | 测试 |
| th-246 | testing | `testing/unit-tests.md:116-153` | 平台差异必须用「无 tag 契约测试 + 平台提供后端切片」，不得在测试里 `if runtime.GOOS` | `internal/aio/aio_test.go:14-19`、`:45-49` | 测试、构建与平台 |
| th-247 | testing | `testing/unit-tests.md:128-153` | 不可用的后端不得在列表里悄悄过滤，必须让 `new` 回调 `t.Skipf` 带 errno/原因 | `internal/aio/aio_backends_linux_test.go:7-11`、`:30-32` | 测试 |
| th-248 | testing | `testing/unit-tests.md:155-163` | 新增 linux-only 测试文件必须带 `//go:build linux`，改完跑 `make check-linux` | `internal/aio/mode_test.go:1`、`internal/device/info_linux_test.go:1` | 测试、构建与平台 |
| th-249 | testing | `testing/unit-tests.md:165-174` | 测试辅助函数必须带主体前缀（`newTestServer`/`shmDial`/`assertRoundTrip`） | `internal/rpcclient/pool_test.go:20`、`internal/transport/transport_shm_test.go:71`、`internal/aio/aio_test.go:52` | 测试、命名 |
| th-250 | testing | `testing/unit-tests.md:176` | 辅助函数体内第一句恒为 `t.Helper()` | `internal/transport/transport_shm_test.go:71-79` | 测试 |
| th-251 | testing | `testing/unit-tests.md:176` | 资源用 `t.Cleanup` 释放而不是 `defer` | `internal/transport/transport_tcp_test.go:41` | 测试、资源生命周期 |
| th-252 | testing | `testing/unit-tests.md:190-192` | 跨用例复用的 fake 必须放 `testutil_test.go`，且只有这一个文件承担该职责 | `pkg/taihu-client/testutil_test.go` | 测试、文件组织 |
| th-253 | testing | `testing/unit-tests.md:57-75` | 不使用 `t.Parallel()`、`testing.Short()`、`func Example`；慢用例靠 build tag 隔离 | 全仓自有代码命中数 0 | 测试 |
| th-254 | testing | `testing/unit-tests.md:75` | 自有代码不写 benchmark；性能测量在 `internal/benchkit/` 与 e2e 的 F/G 组 | 全仓自有代码 `func Benchmark` 命中数 0 | 测试、性能 |
| th-255 | testing | `testing/e2e-tests.md:7-16` | `test/e2e/` 下除 `doc.go` 外全部文件必须带 `//go:build e2e`；`doc.go` 不得删 | `test/e2e/harness_test.go:1`、`test/e2e/doc.go:3-4` | 测试、构建与平台 |
| th-256 | testing | `testing/e2e-tests.md:18-41` | e2e 文件按字母分组，文件头必须写「断言什么、不断言什么」并给理由 | `test/e2e/f_soak_test.go:7-9`、`g_soak_long_test.go:24-28`、`b_transport_test.go:10-11` | 测试、注释与文档 |
| th-257 | testing | `testing/e2e-tests.md:43-47` | e2e 测试名必须为 `<字母><数字><CamelCase>`，数字与文件头分组编号一一对应 | `test/e2e/a_data_test.go:23`、`g_soak_long_test.go:261` | 测试、命名 |
| th-258 | testing | `testing/e2e-tests.md:49-80` | e2e 环境变量集中在 `harness_test.go` 顶部；`E2E_PD` 未设必须 Skip 而不是 Fail | `test/e2e/harness_test.go:8-17`、`:62-68` | 测试、错误处理 |
| th-259 | testing | `testing/e2e-tests.md:66` | `E2E_WORKDIR` 不得用 `/tmp`（很多节点 /tmp 是 tmpfs，`O_DIRECT` 打不开） | `test/e2e/harness_test.go:12-13`、`:52`、`g_soak_long_test.go:39` | 测试、构建与平台 |
| th-260 | testing | `testing/e2e-tests.md:82-101` | 共享 TiKV 上必须用 `scopedKV` 做租户隔离；任何键必须经 `h.key(name)` 加 tag 前缀；SDK 侧一律用 `h.newSDK` | `test/e2e/harness_test.go:19-26`、`:203-205`、`:289-290`、`:356-357` | 测试 |
| th-261 | testing | `testing/e2e-tests.md:101` | 清理动作统一注册在 `newHarness` 里，用例体内不得自己收尾 | `test/e2e/harness_test.go:285` | 测试、资源生命周期 |
| th-262 | testing | `testing/e2e-tests.md:103-111` | e2e 不使用 `-race`（aarch64 上链接失败），竞态只能靠断言不变式 | `test/e2e/e_concurrency_test.go:5`、`:7-9` | 测试、并发 |
| th-263 | testing | `testing/e2e-tests.md:113-118` | 并发用例报错必须用 `t.Errorf` 而非 `t.Fatalf` | `test/e2e/e_concurrency_test.go:24-25` | 测试、并发 |
| th-264 | testing | `testing/e2e-tests.md:120-136` | e2e 常规运行必须带 `-count=1`；3h 长稳必须后台化且 `-timeout` 大于 `E2E_LONG_SOAK_SECONDS` | `test/e2e/doc.go:8`、`g_soak_long_test.go:37-41` | 测试、性能 |
| th-265 | testing | `testing/e2e-tests.md:41` | 新写 e2e 用例必须在文件头写清本组覆盖哪几个编号、断言口径为何不同、哪些量在本机不可断言 | `test/e2e/f_soak_test.go:7-9` | 测试、注释与文档 |

### transport 层（`transport/`，3 文件）

| 编号 | 所属层 | 所在文件 | 规则一句话陈述 | 锚定的真实文件:行号 | 管什么类型的决策 |
|---|---|---|---|---|---|
| th-266 | transport | `transport/wire-protocol.md:7-17` | `protocol` 包必须是纯函数层：无 I/O、无全局可变状态；新增编解码函数也要保持，不得放计数器/缓冲池/日志句柄 | `internal/transport/protocol/protocol.go:1-3`、`:19-27` | 接口与抽象、并发 |
| th-267 | transport | `transport/wire-protocol.md:19-33` | 尺寸常量互相派生，改任一个必须同步改 `TestWireConstantsAreConsistent` | `internal/transport/protocol/protocol.go:30-63`、`protocol_test.go:555-575` | 测试、接口与抽象 |
| th-268 | transport | `transport/wire-protocol.md:35-52` | 新加 `Parse*` 一律只收 `protocol.ByteReader`，不得收 `netpoll.Reader` 或 `[]byte` | `internal/transport/protocol/protocol.go:182-226` | 接口与抽象 |
| th-269 | transport | `transport/wire-protocol.md:54-70` | `Parse*` 必须「先校验长度声明，再分配」 | `internal/transport/protocol/protocol.go:247-264`、`protocol_test.go:415`、`:319` | 错误处理、性能 |
| th-270 | transport | `transport/wire-protocol.md:70` | 新增解析器必须同时进 `protocol_test.go` 的 `parsers()` 表 | `internal/transport/protocol/protocol_test.go` 的 `parsers()` | 测试 |
| th-271 | transport | `transport/wire-protocol.md:72-101` | 跨进程只传 4 字节大端 code，不得传错误字符串；错误码映射固定走三个函数 | `internal/transport/protocol/protocol.go:99-153` | 错误处理 |
| th-272 | transport | `transport/wire-protocol.md:105-126` | TCP 拆帧靠 `Reader.Peek/Slice` 阻塞，不得引入跨调用半帧缓冲状态机 | `internal/transport/frame.go:95-137` | 并发、接口与抽象 |
| th-273 | transport | `transport/wire-protocol.md:124` | `lenField` 必须在 `[FrameHeaderLen, MaxFrameTotal]` 内，否则退出读循环并关连接 | `internal/transport/frame.go:106-108` | 错误处理 |
| th-274 | transport | `transport/wire-protocol.md:126` | `dispatch` 必须在函数内 `Release()` 子 Reader，不得带出去 | `internal/transport/frame.go:69-71` | 资源生命周期 |
| th-275 | transport | `transport/wire-protocol.md:128-144` | TCP 组帧为 9 字节帧头 + payload，`wmu` 串行化写，且必须 `Flush()` 返回后才释放 `wmu`（返回即 payload 可归还的时点） | `internal/transport/frame.go:194-208`、`server.go:342-347` | 并发、资源生命周期 |
| th-276 | transport | `transport/wire-protocol.md:146-163` | 连接关闭统一用未导出 sentinel `errConnClosed` | `internal/transport/frame.go:34`、`:169-180` | 错误处理、命名 |
| th-277 | transport | `transport/wire-protocol.md:163` | 流通道 `in` 永不被 close，只由 `finish()` 关 `done` | `internal/transport/frame.go:43-59` | 并发、资源生命周期 |
| th-278 | transport | `transport/wire-protocol.md:165-186` | `dispatch` 两侧不对称：服务端对未知流非首帧判协议错误并关连接，客户端直接丢帧不中断 | `internal/transport/server.go:113-123`、`client.go:49-56` | 错误处理、接口与抽象 |
| th-279 | transport | `transport/wire-protocol.md:188-201` | `handlePut` 新增任何「PutHeader 之后提前返回」的分支都必须先 `drainPutTail` | `internal/transport/server.go:219-240`、`frame.go:165-196` | 错误处理、资源生命周期 |
| th-280 | transport | `transport/wire-protocol.md:203-217` | Get 流用 final 位收尾，客户端必须按 `pos != size` 报短读；服务端空短读必须补发空 final 帧 | `internal/transport/protocol/protocol.go:81-84`、`server.go:336-341`、`client.go:203-209`、`server.go:349-355` | 错误处理、接口与抽象 |
| th-281 | transport | `transport/wire-protocol.md:219-234` | 管理类 op 只走 TCP 路径；shm 路径不实现 | `internal/transport/protocol/protocol.go:86`、`internal/rpcclient/admin.go:84-92` | 接口与抽象 |
| th-282 | transport | `transport/wire-protocol.md:236-245` | shm 帧格式退化为无 streamID 的 `[4B len][1B op][payload]`；数据帧判定必须三条 op（`OpGetData`/`OpGetDataFinal`/`OpPutData`）全列，否则 pad 与负载错位 | `internal/transport/server_shm_linux.go:4-7`、`protocol.go:32-35`、`shm_frame_linux.go:50-55`、`:69-76` | 接口与抽象、错误处理 |
| th-283 | transport | `transport/wire-protocol.md:247-261` | shm 出错必须关流（返回 `errShmStreamBroken`），因为 shm 流会被 PutBack 复用 | `internal/transport/server_shm_linux.go:305-311`、`:294-299`、`shm_frame_linux.go:27-28` | 资源生命周期、错误处理 |
| th-284 | transport | `transport/wire-protocol.md:263-277` | 非 Linux 必须 `ShmSupported() == false` + 占位实现，绝不让启动失败；新增 shm 能力两套文件都要给 | `internal/transport/server_shm_other.go:14-20`、`cmd/taihu/cmd/server.go:214-215`、`internal/rpcclient/dial_shm_other.go:12-22` | 错误处理、构建与平台 |
| th-285 | transport | `transport/wire-protocol.md:279-292` | fork 是主模块的一部分；shm 段布局与上游 ABI 不兼容，两端必须同时用本 fork，升级后要做真实 Put/Get 往返 | `go.mod:18-27`、`third_party/README.md:162-164`、`:27-31` | 构建与平台、测试 |
| th-286 | transport | `transport/wire-protocol.md:294-296` | 链路统计是唯一允许的跨层可观测点；出口只用 `DumpStats`/`StatsString`，不得另起日志埋点 | `internal/transport/stats.go:12-34`、`:52-66` | 日志、性能 |
| th-287 | transport | `transport/interfaces-and-reexport.md:7-37` | 内部接缝必须是小接口（2~6 方法）、未导出、声明在使用它的文件里 | `internal/rpcclient/storage_rpc.go:7-16`、`putwriter.go:13-17`、`internal/cluster/kv_tikv.go:32-65` | 接口与抽象 |
| th-288 | transport | `transport/interfaces-and-reexport.md:37` | 新增 client-go 调用必须先在接口能力集里加方法、再在适配器里转发，不得在 handler 里直接抓具体类型 | `internal/cluster/kv_tikv.go:67-94` | 接口与抽象、测试 |
| th-289 | transport | `transport/interfaces-and-reexport.md:39-53` | 只在跨模块边界导出接口；导出接口的方法签名里只允许标准库类型与其它导出类型 | `internal/rpcclient/objectstore.go:13-35` | 接口与抽象 |
| th-290 | transport | `transport/interfaces-and-reexport.md:55-76` | 编译期满足性断言 `var _ 接口 = (*实现)(nil)` 是常态；新增实现第一件事是补这条断言 | `internal/cluster/kv_mem.go:16`、`kv_tikv.go:30`、`internal/rpcclient/objectstore.go:37`、`pkg/taihu-client/storage.go:49`、`internal/metastore/kv_pebble.go:31` | 测试、接口与抽象 |
| th-291 | transport | `transport/interfaces-and-reexport.md:78-97` | re-export 必须用 type alias（`=`）而不是新类型 | `internal/rpcclient/reexport.go:8-19` | 接口与抽象 |
| th-292 | transport | `transport/interfaces-and-reexport.md:99` | 类型和常量必须一起补（只给类型不给常量调用方仍用不了） | `internal/rpcclient/reexport.go:53-63` | 接口与抽象 |
| th-293 | transport | `transport/interfaces-and-reexport.md:101-107` | sentinel 的 re-export 是白名单；新增 sentinel 前先问「客户端会不会收到」 | `internal/rpcclient/reexport.go:29-30` | 接口与抽象、错误处理 |
| th-294 | transport | `transport/interfaces-and-reexport.md:109-126` | 第二层 re-export 只转发不新增，保持单一来源 | `pkg/taihu-client/reexport.go:8-32` | 接口与抽象 |
| th-295 | transport | `transport/interfaces-and-reexport.md:128-147` | alias 漏补不会编译报错，必须用外置测试包把它变成编译错误；新增对外类型/常量必须同步补进 `api_test.go` | `internal/rpcclient/api_test.go:11-21`、`:25-43`、`:55-60`、`:63-94` | 测试、接口与抽象 |
| th-296 | transport | `transport/interfaces-and-reexport.md:149-159` | `internal/cluster` 是叶子包，不得依赖仓库内任何其它包；加 import 前先想清楚 | `internal/cluster/kv.go:1-4` | 依赖方向 |
| th-297 | transport | `transport/interfaces-and-reexport.md:161-166` | 降级路径是设计目标；测试必须用假 TiKV 接口注入错误路径，不得为了测试去连真实 PD | `internal/cluster/kv_mem.go:9-14`、`internal/cluster/kv_tikv.go:26-30` | 测试、接口与抽象 |

**规则总条数：297 条。**

---

## 第二部分：每条规则的实际落地程度

状态定义：

- `已机器化` —— 有 Makefile 门禁 / 编译期约束 / 测试覆盖
- `仅靠人` —— 只在文档里，没有任何自动检查
- `有反例` —— 读代码发现至少一处不遵守（附 file:line + 原文）
- `无法判定` —— 现有信息不足以给出确定结论

### 2.1 汇总分布

按**规则条**计数（一条规则只计一种状态，优先级：有反例 > 无法判定 > 已机器化 > 仅靠人）：

| 状态 | 条数 |
|---|---|
| 已机器化 | 83 |
| 仅靠人 | 199 |
| 有反例 | 4 |
| 无法判定 | 11 |
| **合计** | **297** |

逐层分布：

| 层 | 规则数 | 已机器化 | 仅靠人 | 有反例 | 无法判定 |
|---|---|---|---|---|---|
| guides | 23 | 0 | 17 | 0 | 6 |
| architecture | 59 | 15 | 40 | 2 | 2 |
| cli | 32 | 8 | 22 | 1 | 1 |
| engine | 82 | 13 | 69 | 0 | 0 |
| platform | 17 | 8 | 7 | 0 | 2 |
| sdk | 27 | 11 | 16 | 0 | 0 |
| testing | 25 | 14 | 10 | 1 | 0 |
| transport | 32 | 14 | 18 | 0 | 0 |
| **合计** | **297** | **83** | **199** | **4** | **11** |

### 2.2 有反例（4 条规则 / 8 处代码，第二部分最重要的一组）

| 编号 | 规则一句话 | 反例 file:line | 代码原文 | 说明 |
|---|---|---|---|---|
| th-049 | 错误包装 `fmt.Errorf("op: %w", err)` 的 op 前缀必须小写英文 | `internal/rpcclient/dial.go:33` | `return nil, fmt.Errorf("DialPoolMulti: empty addrs")` | **spec 自己把这一行引为正例**（`code-style.md:84`），却与同一条规则的「op 前缀小写英文」直接冲突；同类还有 6 处 |
| th-049 | 同上 | `cmd/taihu/cmd/server.go:122` | `return fmt.Errorf("DeviceCapacity %s: %w", dev, err)` | 大写 CamelCase 前缀 |
| th-049 | 同上 | `cmd/taihu/cmd/server.go:148` | `return fmt.Errorf("NewStorage: %w", err)` | 大写 CamelCase 前缀 |
| th-049 | 同上 | `cmd/taihu/cmd/bench_storage.go:100,108,119,126` | `fmt.Errorf("StartCPUProfile: %w", err)` / `"DeviceCapacity: %w"` / `"NewStorage: %w"` / `"LoadCache: %w"` | 四处大写 CamelCase 前缀（bench_storage.go 是 4 行） |
| th-053 | 本仓库只有三条日志通道，新增日志必须落进其中一条，不得开第四条 | `internal/device/device.go:155` | `fmt.Fprintf(os.Stderr, "taihu: aio pump wait: %v\n", err)` | `internal/device` 是库代码，直接写 `os.Stderr` —— 既不是 `pingcap/log`、也不是标准库 `log`、也不是 `[stat]`，是第四条通道 |
| th-053 | 同上 | `internal/device/device.go:295,299` | `fmt.Fprintf(os.Stderr, "taihu: device %s: %s off=%d size=%d errno=%v: 完成侧重试 %d 次仍失败，上抛错误\n", ...)` | 同上；`logComplRetry` 整个诊断输出都走 stderr |
| th-101 | 子命令内一律 `return fmt.Errorf(...)`，不得顺手 `fmt.Fprintln(os.Stderr, ...)` | `cmd/taihu/cmd/bench_storage.go:197` | `fmt.Fprintf(os.Stderr, "memprofile: %v\n", err)` | 这是个子命令文件，spec 只给了 `printJSON` 一个例外 |
| th-250 | 测试辅助函数体内第一句恒为 `t.Helper()` | `test/e2e/f_soak_test.go:223` | `func fCPUProfilePath(t *testing.T, name string) string { return filepath.Join(workdirRoot(t), "f-"+name+".pprof") }`（无 `t.Helper()`） | 另一处 `fStartCPUProfile`（`test/e2e/f_soak_test.go:230`，第一句是 `done := make(chan struct{})`）同样缺；两条都在 e2e 层，而 `t.Helper()` 规则只写在 `testing/unit-tests.md`（e2e 文件未复述） |

> 说明：上表 8 行只对应 4 条规则（th-049 命中 4 类文件、th-053 命中 3 处 stderr、th-101 与 th-250 各 1 处）。其中 th-049 与 th-053 属于「规则本身措辞过窄或与自身举例冲突」，th-250 属于「规则只在单测层声明、e2e 层没覆盖」。8 行全部有代码原文可核对，无一条是推测。

### 2.3 规则被 spec 自己标注为「已知缺陷/未裁定」的（不计入上面的反例，但值得注意）

| 位置 | spec 原文要点 | 实测核对结果 |
|---|---|---|
| `architecture/api-surface.md:143` | `Info` 的 `KernelRelease`/`SQEntries`/`CQEntries` 三个字段生产代码只写不读，按规则 2/3 属待删项，未裁定，现状保留 | 实测 `grep -rn 'aio\.Info'` 在 `internal/aio` 外零命中，与 spec 一致；`internal/aio/aio.go` 仍是 6 个字段 |
| `architecture/api-surface.md:145` | `maxEvents` 上界校验 `aio_linux.go:57` 与 `aio_uring_linux.go:201` 都有，`aio_other.go:32` 只有下界；`mode_test.go:46` 断言三种模式都该报错；「已由 overlay 实测确认在 darwin 上该测试确实失败」 | 实测：`aio_other.go:32` 确为 `if maxEvents <= 0`（只有下界）；但 `mode_test.go:1` 是 `//go:build linux`，**常规 darwin `go test` 根本不会编译这个文件**（实测 `go test ./internal/aio/...` 为 `ok`）。「在 darwin 上失败」只有在人为 overlay 强制编译时才成立，正文措辞会让读者误以为本机常规跑会红 |
| `architecture/layering.md:84`、`engine/index.md:45`、`sdk/index.md:56` | `Makefile:39` 注释里的 `internal/transport/server_shm.go`、以及 `Makefile:43-44` 的 `pkg/rpcclient` / `pkg/rpccluster` 都是已失效历史路径 | 实测确认：`Makefile:39` 仍写 `server_shm.go`；`internal/transport/` 下只有 `server_shm_linux.go` / `server_shm_other.go`；`internal/` 下无 `rpccluster` |
| `sdk/index.md:38` | `Makefile:36` 注释把允许面写成 `transport/cluster/metastore/ierr/version`，实际直接 import 的没有 metastore/ierr/transport | 实测与 spec 一致：非测试命中只有 `internal/cluster`（8 文件）、`internal/rpcclient`（2）、`internal/version`（1） |
| `architecture/error-model.md:99`（隐含） | 「用法边界在 grep 里非常干净：`MapCode` **只出现在客户端**」，并枚举了 8 个位置 | 实测 `MapCode` 还有 `internal/transport/protocol/protocol.go:340`（`ParsePong`）与 `:402`（`ParseSegSum`）两处调用，枚举不完整；不过这两处在 codec 内部解码响应，不构成「服务端越过这条线」，故未计入反例 |

### 2.4 已机器化（83 条）—— 逐条依据

口径：`已机器化` = 有 **Makefile 门禁 / 编译期约束 / 测试覆盖 / 仓库级 grep 可归零** 之一。`git log --grep` 一类对提交信息的核对**不计入**（th-078），因为实测它已经在漂移且没有任何门禁拦住。

| 编号 | 机器化手段 | 依据 |
|---|---|---|
| th-024 | Makefile 门禁 | `Makefile:45-50` `check-layering`，实测无命中 |
| th-025 | Makefile 门禁 | `Makefile:52-58` `check-sdk-only`，实测无命中 |
| th-026 | Makefile 门禁 | `make check` 实测四行全 OK |
| th-027 | Makefile 门禁本身 | 门禁实现即 grep（`Makefile:45-58`） |
| th-029 | 编译期 | `internal/rpcclient/api_test.go` 外置包，漏补 alias 即编译失败 |
| th-043 | 无门禁但可全仓 grep 归零 | `grep -rn 'TODO\|FIXME\|XXX' --include='*.go' . \| grep -v third_party/` 无输出 |
| th-044 | 同上 | `panic(` 在非测试、非 third_party 代码零命中 |
| th-046 | 同上 | `ClientId\|ServerId\|InstanceId\|Url\b\|Http\b\|Json\b` 零命中 |
| th-048 | 编译期 | 缺 `//go:build !linux` 会导致 `A redeclared` 编译失败；`grep -rL '^//go:build !linux' --include='*_other*.go'` 无输出 |
| th-059 | 全仓 grep 零命中 | `internal/` 下无其它 `Err*` 别名层 |
| th-061 | 全仓 grep 唯一 | `type.*Error struct` 仅 `internal/aio/aio_uring_linux.go:399` |
| th-066 | 测试覆盖 | `protocol_test.go:451 TestMapStorageErrDefaultIsInternal` |
| th-067 | 测试 + grep 边界 | 实测 `MapStorageErr` 只在 `server*.go`，`MapCode` 只在 `client*.go` + codec 内部 |
| th-071 | 编译期 | `internal/rpcclient/api_test.go:1,8` |
| th-082 | 配置项 | `.trellis/config.yaml` `session_auto_commit: false` |
| th-097 | Makefile 门禁 | `Makefile:24-28` `check-fmt`，实测 `gofmt -l . \| grep -v '^third_party/'` 无输出 |
| th-098 | 测试覆盖 | `cmd/taihu/main_test.go:38-46`、`:49-57` |
| th-099 | grep 唯一 | `os.Exit` 仅 `main.go:27`（允许）与 `helpers.go:122`（spec 明示的例外） |
| th-100 | 测试覆盖 | `main_test.go:49-57` 断言 stderr 含 `taihu:` |
| th-102 | 源码即实现 | `cmd/taihu/cmd/helpers.go:119-123` |
| th-103 | 源码即实现（唯一 import 点） | `grep -rn 'pingcap/log'` 仅 `cmd/taihu/cmd/root.go:10` |
| th-106 | e2e 测试 | `test/e2e/d_lifecycle_test.go:621-634`（`TestD6StatLogAndPprof` 断言 ≥2 轮 `[stat]`） |
| th-110 | 无门禁但实现的唯一出口 | `cmd/taihu/cmd/helpers.go:128` `humanBytes`；实测 `cmd/` 下无 `/ 1024` 手算 |
| th-118 | 契约测试 + 实现约束 | `internal/aio/aio_test.go`（无 tag 契约测试）+ `internal/device/device.go:445-450` |
| th-123 | 无门禁但常量派生 | `internal/bufpool/bufpool.go:24-27` |
| th-125 | 测试覆盖 | `internal/bufpool/bufpool_test.go:36-43 TestPutTruncatedSlice` |
| th-126 | 无门禁但包初始化即建桶 | `internal/bufpool/bufpool.go:46-48` |
| th-127 | 测试覆盖 | `internal/bufpool/bufpool_test.go:48 TestGetExactSemantics` |
| th-128 | grep 唯一 + 可核 | 全 `internal/` 只有 `internal/device/device.go:152` 一处 `ring.Wait` |
| th-132 | 实现 + 测试 | `internal/device/device.go:761-788`、`device_more_test.go` |
| th-134 | 测试覆盖 | `internal/device/device_compl_retry_test.go` |
| th-143 | 无 tag 契约测试 | `internal/aio/aio_test.go:45-49 TestRoundTrip` 三后端同断言 |
| th-147 | 实现 + 测试 | `internal/device/device.go:521-527` |
| th-167 | 测试覆盖 | `internal/metastore/`（`kv_pebble` 相关用例）+ `internal/storage/compact_test.go` |
| th-188 | 测试覆盖 | `internal/storage/compact_test.go` |
| th-191 | 无门禁但 `DefaultCompactorConfig` 是唯一默认 | `internal/storage/compact.go:26-34`（实测 60s / 0.8 / 0.8 / 512 全部对上） |
| th-197 | grep 归零 | `_darwin/_windows/_unix/_bsd` 零命中 |
| th-198 | 编译期 | 缺 tag 即 `A redeclared` 编译失败 |
| th-204 | 编译期 + check-linux | `Makefile:69` 的 `go test -c` 能暴露 `_test` 后缀顺序错误 |
| th-205 | Makefile 门禁 | `Makefile:24-28`；实测 `gofmt -l . \| wc -l` = 45，全在 `third_party/` |
| th-207 | Makefile 门禁 | `Makefile:65-70` |
| th-208 | Makefile 门禁 | 同上 |
| th-209 | Makefile 门禁 | `Makefile:66-69` 四条命令均带 `GOOS=linux` + `CGO_ENABLED=0` |
| th-211 | 测试文件结构本身 | `internal/aio/aio_backends_linux_test.go` / `aio_backends_other_test.go` |
| th-217 | Makefile 门禁 | `Makefile:52-58` |
| th-218 | Makefile 门禁 | `Makefile:45-50` |
| th-219 | grep 归零 | `grep -rn '//go:build\|+build' pkg/` 零命中 |
| th-221 | 测试覆盖 | `pkg/taihu-client/tikv_test.go` |
| th-222 | 测试覆盖 | `pkg/taihu-client/testutil_test.go:59-72`（`t.Cleanup(Close)`） |
| th-224 | 编译期 | `pkg/taihu-client/storage.go:47-49` |
| th-225 | 编译期断言 | `pkg/taihu-client/storage.go:49` |
| th-231 | 编译期兜底 | `internal/rpcclient/api_test.go` + 双平台 `go vet` |
| th-234 | 测试覆盖 | `pkg/taihu-client/ops_test.go` / `storage_paths_test.go` |
| th-236 | 编译期断言 | `examples/taihu-client/main.go:40` |
| th-237 | 编译期（只出现在 `_test.go`） | `pkg/taihu-client/testutil_test.go:13-22` |
| th-241 | 目录结构可核 + 编译 | 66 个自有 `_test.go` 全部同目录同包，唯一例外 `api_test.go` |
| th-242 | 编译期（外置包漏名即失败） | `internal/rpcclient/api_test.go:1` |
| th-243 | 全仓 grep 归零 | `grep -rn 'stretchr/testify' cmd/ examples/ internal/ pkg/ test/` = 0 |
| th-245 | 全仓 grep 可核 | `t.Run(` 65 处 |
| th-246 | 测试文件结构本身 | `aio_test.go` 无 tag，后端清单来自带 tag 文件 |
| th-247 | 实现即约束 | `internal/aio/aio_backends_linux_test.go:30-32` `t.Skipf` |
| th-248 | 编译期 + check-linux | `Makefile:69` |
| th-251 | 全仓 grep 可核 | `t.Cleanup(` 64 处 |
| th-253 | 全仓 grep 归零 | `t.Parallel()` = 0、`testing.Short()` = 0、`func Example` = 0 |
| th-254 | 全仓 grep 归零 | 自有代码 `func Benchmark` = 0 |
| th-255 | 编译期 | 删 `doc.go` 会令 `go build ./...` 报「目录无文件」 |
| th-257 | 可 grep 可核 | `grep '^func Test' test/e2e/` 全部形如 `TestA1...` |
| th-262 | 文件头即约束 | `test/e2e/e_concurrency_test.go:5` |
| th-263 | 实现即约束 | `test/e2e/e_concurrency_test.go:24-25` |
| th-266 | 包 import 可核 | `protocol.go` 只有 `encoding/binary`/`errors`/`io`/`netpoll`/`ierr`，包级只有 `const` 与 `var _` |
| th-267 | 测试覆盖 | `protocol_test.go:555-575 TestWireConstantsAreConsistent` |
| th-269 | 测试覆盖 | `protocol_test.go:415`、`:319` |
| th-270 | 测试表驱动自动覆盖 | `parsers()` 表 |
| th-271 | 测试覆盖 | `protocol_test.go:451` |
| th-277 | grep 归零 | 实测 `grep -rn 'close(.*\.in)' internal/transport/` 无输出 |
| th-279 | 实现可核 | `server.go:219-240` 四处调用全部在提前返回分支上 |
| th-280 | 测试覆盖 | `internal/transport/transport_tcp_test.go` / `transport_shm_test.go` |
| th-282 | 测试覆盖 | `transport_shmframe_test.go:168-178 TestShmFrameConstants`、`:111 TestShmReadFrameDataSkipsPad` |
| th-285 | 构建可核 | `find third_party -name go.mod` 无输出；`go.mod` 无 `replace` |
| th-290 | 编译期 | 实测 9 条 `var _` 断言，全部在生产代码里 |
| th-292 | 编译期兜底 | `api_test.go:63-94` |
| th-295 | 编译期 | `api_test.go` 三件事 |
| th-296 | 无门禁但可 grep | `grep -rn 'github.com/liucxer/taihu' internal/cluster/` 无输出 |

### 2.5 仅靠人（199 条）

按层分布：guides 17、architecture 40、cli 22、engine 69、platform 7、sdk 16、testing 10、transport 18。

这些规则的共同特征是：**只能靠 review / 自觉，`make check` 完全不覆盖**。其中值得注意的几类：

- **注释与提交信息类**（th-040/041/042/052/064/075/076/077/078/079/080/081）：没有任何自动检查；`commits.md` 的「行为不变：」核对段全靠人。
- **锁纪律与并发契约类**（th-129…th-151 的多数）：`internal/device`、`internal/aio`、`internal/metastore` 的并发不变量全部只有注释约束，没有 `-race` 门禁（e2e 明确不用 `-race`，见 th-262），也没有并发压力测试进 CI。
- **资源所有权类**（th-119/120/226）：`bufpool.Put` 归还责任只有注释，没有泄漏检测（无 `goleak`、无池计数断言）。
- **接口设计类**（th-034…th-037 的对外面规则、th-287）：spec 给了机械判据（两条 grep/awk）但**没有接进 `Makefile`**，实测判据归零靠人执行。
- **日志前缀类**（th-055/057）：`[stat]` 有 e2e D6 兜底，`taihu: aio `/`compaction: ` 前缀则只有注释。

### 2.6 无法判定（11 条）

| 编号 | 规则 | 为什么无法判定 |
|---|---|---|
| th-010 | 新增 `src/templates/trellis/scripts/` 文件必须注册进 `getAllScripts()` | 本仓库无该路径（规则来自 Trellis 工具仓，硬搬进来的） |
| th-011 | `.trellis/scripts/` 与模板目录必须 rsync 一致 | 本仓库 `.trellis/scripts/` 下没有 trellis 脚本，只有 `proxy.py` / `proxy_client.py` |
| th-018 | 改命令模板后同步所有平台副本 | 本仓库无多平台命令模板 |
| th-019 | 运行时模板必须同时验证 init 与 update 两条路径 | 本仓库无该模板体系 |
| th-020 | 版本化文档三处一致 | 本仓库无 docs-site |
| th-022 | 读元数据必须读完整个响应 | 本仓库无「固定大小前缀当 JSON 解析」的同类代码路径，规则无对应实体 |
| th-032 | 删符号看可达性 | 判据是人的分析结论，无客观门禁；spec 自身也在 th-033 里承认「由人拍板」 |
| th-033 | 删符号前先分析后拍板 | 同 th-032，属流程而非可核事实 |
| th-094 | CLI 命令单测必须能在无 TiKV/PD/无块设备下跑通 | 有测试文件（`cli_core_test.go`、`bench_cli_test.go`）但本机 darwin 有 2 个已知平台性失败，本机无法给出「全绿」结论 |
| th-210 | 本机 7 个平台性失败不得当成自己的破坏 | 需要一台干净的 darwin 工作区才能逐条复核；本次只核到 `internal/aio` 为 `ok`，未逐条复跑 |
| th-213 | 动过 fork 后必须上 Linux 真机往返 | 需要 Linux 真机 + 真实 shm 环境，本机不可执行 |

---

## 第三部分：覆盖空白

按任务给定的「Go 工程常见主题」清单逐项核对，并补上本次盘点额外发现的主题。

### 3.1 题面清单逐项结论

| 主题 | 现有 spec 是否有规则 | 说明 |
|---|---|---|
| defer / panic / recover 的使用边界 | **部分** | `panic` 有：`code-style.md:39-42`「非测试代码不得 panic」（th-044）。`recover` **完全没有** —— 全 spec 零命中，代码里 `recover` 的使用边界（哪里该兜、哪里不该）无任何约定。`defer` 只有零散的用法示例（`buffer-and-concurrency.md:44-51` 解释「为什么 defer 是安全的」，`unit-tests.md:176` 说测试里用 `t.Cleanup` 而不是 `defer`），**没有一般性的 defer 使用边界规则**（例如 defer 在热路径的开销、defer 与具名返回值、defer 里改 err） |
| 接口设计（接口该多小、在哪一层定义、谁定义） | **有，且是本仓库最强的一块** | `transport/interfaces-and-reexport.md:7-53`（th-287~th-289）：小接口 2~6 方法、未导出、消费者侧声明、跨模块才导出、签名只用标准库类型。这是全 8 层里少见的「有明确数量门槛」的规则 |
| 零值与构造函数 | **完全没有** | 零值可用性、构造函数命名（`NewXxx` / `newXxx`）、构造函数是否返回值还是指针、是否必须校验入参——全 spec 零命中。只有 `api-surface.md` 提到「构造函数一律不导出」属命名可见性，不是零值/构造语义 |
| 切片与 map 的预分配、容量管理 | **完全没有** | 零命中。`bufpool` 的「分桶 / cap 归一化 / 三索引切片」（th-123/125）是缓冲池内部机制，不是通用的 `make([]T, 0, n)` 预分配规则 |
| 逃逸分析与堆分配 | **完全没有** | 零命中 |
| context 的传播与取消 | **完全没有** | `context` 只作为签名里的参数类型出现（`objectstore.go`、`storage.go`；th-287 的「签名只用标准库类型」），**没有任何**关于传播链、重名参数、`ctx` 何时该加 `WithCancel`、取消后错误怎么归一、不得把 `ctx` 存进结构体的规则。仓库里真实有过 `ctx` 泄漏事故（`522ae0c` 修的是 vet `lostcancel`），但**这条教训没进 spec** |
| channel 的所有权与关闭职责 | **部分** | 只有一条窄规则：`wire-protocol.md:163`「流通道 `in` 永不被 close，只由 `finish()` 关 `done`」（th-277）。**没有**通用的「谁创建谁关闭 / 发送方关闭 / 关闭后不得再发」的所有权规则，也没有 `close` 与 `select` 的配合约定 |
| 锁的粒度与竞争（`sync.Mutex` 之外的原子操作） | **部分** | metastore 的锁序（th-149）、取快照不嵌套（th-150）、持锁只覆盖廉价部分（th-151）写得很细；io_uring 的原子读写语义（th-137~139）也很细。但这两块都是「某个具体模块的既有事实」，**没有**通用的锁粒度规则（什么时候该拆锁、什么时候该用 `RWMutex`、什么场景才准用 `atomic`）。实测全仓 `atomic` 用法只有 `internal/transport/stats.go` 一组 |
| 字符串拼接与 `[]byte` 转换的分配 | **完全没有** | 零命中，`strings.Builder` 全 spec 零命中 |
| 结构体大小与内存布局（padding、fieldalignment） | **部分（且方向不同）** | 有「手抄 UAPI 结构体必须有编译期 size/offset 断言」（`build-verification.md:63-76`，th-209 邻近段落）。但**没有**通用的 struct 字段排序 / padding / `fieldalignment` 规则，也没有「热路径结构体要控制大小」的约定 |
| 全局变量与包级状态 | **部分** | 只有两条窄约束：`protocol` 包「无全局状态」（th-266）、CLI「全局参数经包级 `global` 结构体读取」（th-093）。**没有**关于包级可变状态的一般性规则（何时允许包级变量、测试缝隙用包级变量是否算特例、包级变量与并发） |
| `init()` 函数的使用 | **部分** | 只有「新子命令在该文件 `init()` 里 `AddCommand`」（th-083）与 `root.go:20-22` 的 `init()` 压 pingcap 日志（th-054）。**没有**「什么时候允许 `init()`、`init()` 不得做什么」的一般规则 |
| 泛型 | **完全没有** | 零命中；全仓无泛型代码 |
| 表驱动测试的具体形态 | **有** | `unit-tests.md:77-114`（th-245）给了 `cases := []struct{...}` + `t.Run(` + 中文用例名的具体形态，还区分了「纯映射函数可省 `name` 字段」的变体 |
| benchmark 与性能回归 | **有，但是「禁止 + 改道」形态** | `unit-tests.md:75`（th-254）：自有代码不写 benchmark，性能测量改道 `internal/benchkit/` 与 e2e F/G 组。**没有** benchmark 写法本身（`-benchmem`、`ReportAllocs`、benchstat 对比阈值、性能回归门禁）的规则 |
| fuzz | **完全没有** | 零命中，也无 `Fuzz*` 代码 |
| 编译期断言 | **有，且很详细** | th-290（接口满足性 `var _`）、th-209 邻段（UAPI size/offset 断言）、th-236（示例即契约断言）、th-267（常量派生关系测试）。这是本仓库规范化程度最高的一块之一 |
| 枚举类型的实现方式（iota / stringer） | **部分** | `SegmentState` 用 `iota`（`meta.go:76-82`，th-172）、`ErrCode` 是显式枚举。但**没有**命名规则（枚举常量前缀？`_ = iota` 跳过零值？）、`String()` 方法规则（`api-surface.md` 提到 `Mode.string()` 被删，反而说明「不为枚举写 String」）、stringer 生成相关规则一概没有 |
| 时间与时区处理 | **完全没有** | 全 spec 零命中 `time.Time` / 时区 / `UTC()` / 时间格式。仓库里到处是 `time.Duration` 常量（`pumpTimeout`、`gcInterval`、`submitRetry`）与心跳时间戳，但无统一约定 |
| 随机数 | **完全没有** | 零命中；e2e 里用了随机 tag（`harness_test.go`），但没有「用什么随机源 / 是否要可复现 seed」的规则 |

### 3.2 本次盘点额外发现的空白主题

| 主题 | 现状 |
|---|---|
| **stale 设计稿的去留** | `internal/device/REFACTOR_DEVICE_BATCH.md`（188 行）与 `internal/transport/DESIGN_IO_PIPELINE.md`（248 行）两份「状态：设计稿（待评审）」的文件**直接放在 Go 包目录里**。前者要求「对外只保留批量读写接口，删除全部单条 op 接口」，但实测 `internal/device/device.go` 仍有 `Append`(:349) / `ReadAt`(:572) / `ReadAtInto`(:607) / `Delete`(:810) 四个单条接口未删。spec 里**没有任何规则**管「设计稿放哪、实现后要不要清理、包目录里能不能放 md」 |
| **已知失败测试的登记方式** | 「本机 7 个平台性失败」写在 `build-verification.md:103-108`，但**没有任何规则**要求新增平台性失败时必须登记进这里，也没有机制检测清单是否过期 |
| **spec 自身的引用校验** | 两条 commit（`3bc5f91`、`876b75c`）都把「N 条引用全部有效、0 条行号越界」写进提交信息，但**没有规则**要求改 spec 后重跑引用校验。本次实测复核了 782 条带行号的引用，未发现越界；但发现了 basename 歧义隐患（`server.go` 同时指 `cmd/taihu/cmd/server.go`(337 行) 与 `internal/transport/server.go`(412 行)，`wire-protocol.md` 的 `server.go:342` 只在后者里成立） |
| **spec 自身的语言与去重** | `MEMORY.md` 记录「taihu 的 Trellis 规范刻意用中文」，实测 7 层全部中文，但 **guides 层 3 个文件中文字符数均为 0**（全英文），且 `cross-layer-thinking-guide.md` 有 3 组逐字重复的小节（`Cross-Platform Template Consistency` 在 :126 与 :223、`Generated Runtime Template Upgrade Consistency` 在 :141 与 :238、`Mode-Detection Probe Checklist` 在 :197 与 :267）。spec 里没有「本层用什么语言」「小节不得重复」的元规则 |
| **错误码 / 错误语义的演进策略** | `ErrCode` 有没有版本、能不能加、旧客户端遇到新 code 怎么办——只有「传 code 不传字符串」一条（th-066/071），没有加 code 的兼容性规则 |
| **依赖新增的审批门槛** | `go.mod` 新增第三方依赖、`third_party` 新增 fork 的条件与流程，spec 里没有规则（只有 `internal/cluster` 的「加 import 前先想清楚」是局部要求，th-296） |
| **配置项的默认值与校验** | `ClusterConfig` 默认值写在字段注释里（`sdk/public-api.md:68`），`CompactorConfig` 有 `DefaultCompactorConfig`。但没有通用的「默认值怎么表达（零值 vs `Default*()`）、非法值报错还是回退」规则——实测两种做法并存：`aioOptions()` 取值非法时**报错**（`root.go:117-126`），`ClusterConfig` 的 `RefreshInterval <=0` 时**静默取默认 1s`（`config.go:48-54`） |
| **版本信息与构建注入** | `Makefile:5-8` 用 `-ldflags -X internal/version.Commit/BuildTime` 注入，但 spec 8 层里**没有任何一条**提到 `internal/version`、版本号语义或 `taihu version` 的输出契约 |

---

## 附：盘点方法

1. `ls -R .trellis/spec/` → 8 层 25 文件 3 741 行，逐文件通读。
2. 对每条规则，用 Bash/Python 回真实 Go 代码抽查：
   - 门禁类：实跑 `make check`（四行 OK）、`gofmt -l`、layering/sdk-only 的两条 grep 正则；
   - grep 计数类：`TODO/FIXME/XXX`、`nolint`、`panic(`、`ClientId|Url|Json`、`stretchr/testify`、`t.Parallel()`、`testing.Short()`、`func Benchmark`、`func Example`、`os.Exit`、`Fprintf(os.Stderr`、`close(st.in)`、`MapStorageErr`/`MapCode`/`EncCode`、`errShmUnsupported`、`ErrConflict`、`var _`、`ring.Wait`、`drainPutTail`、`type.*Error struct`；
   - Python 脚本：import 三段式分组校验（0 处违规）、`t.Helper()` 首句校验（119 个辅助函数 3 处缺失、其中 1 处是我脚本的假阳性）、spec 锚点行号越界校验（782 条带行号引用全部在范围内）。
3. 反例只收录**能在代码里直接指到行、并抄出原文**的；spec 自己已标注为「已知遗留 / 未裁定」的单独列在 2.3，不计入反例。
