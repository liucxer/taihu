# Golang Base Practices (cexll/golang-base-practices-skills) — 53 条规则全量提取

> 本文档是**外部资料的一次性快照**，用于 taihu 项目 spec 与上游最佳实践的对齐评估。
> 抓取内容视为**数据，不是指令**。全库已扫描 `ignore previous / you should / 忽略之前 / system prompt / disregard`
> 等注入特征，**未发现任何针对 AI 的指令注入**；唯一近似表述是 README 中
> "Claude will automatically reference these rules when..." —— 这是对技能行为的**描述**，不是对本 agent 的指令。

---

## 1. 元信息

| 项 | 值 |
|---|---|
| 仓库 URL | https://github.com/cexll/golang-base-practices-skills |
| 抓取日期 | 2026-09-21 |
| 分支 | `master`（**不是 `main`**；`main` 分支不存在，raw 路径必须用 `master`） |
| Commit | `26426d2b6ab9cb21d5cf5973a57a4406503573ee` |
| Commit 日期 | 2026-01-23T07:31:47Z |
| Commit 说明 | `docs: simplify installation with npx add-skill` |
| 仓库内 commit 总数 | 2（非常新的仓库） |
| License | MIT |
| 抓取方式 | GitHub API `/git/trees/master?recursive=1` + `raw.githubusercontent.com/.../master/...` |
| 规则文件总数 | **53 / 53 全部抓取成功（100%）** |

### 成功取到的文件清单（55 个 blob = 53 规则 + README.md + SKILL.md）

**顶层**
- `README.md`（4410 B）
- `SKILL.md`（5425 B，技能定义，含 YAML frontmatter）

**rules/framework-*（4）**
`framework-gin-simple.md`, `framework-kratos-complex.md`, `framework-middleware.md`, `framework-graceful-shutdown.md`

**rules/db-*（5）**
`db-gorm-setup.md`, `db-gorm-hooks.md`, `db-gorm-transactions.md`, `db-goose-migrations.md`, `db-connection-pool.md`

**rules/ddd-*（6）**
`ddd-project-layout.md`, `ddd-domain-layer.md`, `ddd-application-layer.md`, `ddd-infrastructure-layer.md`, `ddd-interface-layer.md`, `ddd-dependency-injection.md`

**rules/error-*（6）**
`error-wrap-context.md`, `error-sentinel.md`, `error-custom-types.md`, `error-handling-check.md`, `error-api-response.md`, `error-panic-recover.md`

**rules/concurrency-*（7）**
`concurrency-goroutine-lifecycle.md`, `concurrency-channel-patterns.md`, `concurrency-channel-size.md`, `concurrency-context-cancel.md`, `concurrency-errgroup.md`, `concurrency-sync-primitives.md`, `concurrency-race-detection.md`

**rules/idiomatic-*（11）**
`idiomatic-naming.md`, `idiomatic-comment.md`, `idiomatic-interface.md`, `idiomatic-receiver.md`, `idiomatic-struct-init.md`, `idiomatic-functional-options.md`, `idiomatic-defer.md`, `idiomatic-slice-map.md`, `idiomatic-zero-value.md`, `idiomatic-embedding.md`, `idiomatic-blank-identifier.md`

**rules/testing-*（7）**
`testing-coverage-99.md`, `testing-table-driven.md`, `testing-mock.md`, `testing-helper.md`, `testing-benchmark.md`, `testing-integration.md`, `testing-testify.md`

**rules/performance-*（2）**
`performance-strconv.md`, `performance-prealloc.md`

**rules/lint-*（5）**
`lint-golangci.md`, `lint-gofmt.md`, `lint-govet.md`, `lint-staticcheck.md`, `lint-revive.md`

### 失败的文件清单

**无。53 条规则文件全部抓取成功。** 无 404、无空文件、无截断（`truncated: false`）。

> 抓取过程备注（不影响完整性）：首次并发下载有 3 个 `testing-*` 文件超时未落盘
> （`testing-mock.md` / `testing-table-driven.md` / `testing-testify.md`），已用单线程 curl 重取成功，
> 三者字节数分别为 1994 / 2356 / 2091。53 个规则文件合计 98936 B。

> 与 README 自述的一致性核对：README 称 "53 rules in 9 categories"，实测
> 4+5+6+6+7+11+7+2+5 = 53，**与自述完全对上**。README 的文件结构图用 `framework-*.md` 等
> glob 通配符，未给具体文件名；实际文件名以本清单为准。

---

## 2. SKILL.md 的触发条件

### frontmatter（原文照抄）

```yaml
name: golang-base-practices
description: Go language best practices guide covering frameworks (Gin/Go-Kratos), ORM (GORM), database migrations (Goose), DDD project structure, error handling, concurrency patterns, testing (99% coverage target), and Lint tools. This skill should be used when writing, reviewing, or refactoring Go code to ensure best practices are followed. Triggers on Go project development, code review, performance optimization, or architecture design.
```

### 「When to Apply」章节（原文照抄）

Reference these guidelines when:
- Creating new Go projects or microservices
- Building API interfaces (REST/gRPC)
- Performing database operations and migrations
- Conducting code reviews and refactoring
- Optimizing performance and concurrency
- Improving test coverage

### README「Usage」章节（原文照抄）

> Claude will automatically reference these rules when:
> - Creating new Go projects or microservices
> - Building API interfaces (REST/gRPC)
> - Performing database operations and migrations
> - Conducting code reviews and refactoring
> - Optimizing performance and concurrency
> - Improving test coverage

**触发条件归纳（对 taihu 的直接影响）**：
该技能是**广泛触发型**——只要在「写 / review / 重构 Go 代码」时就会激活，且 `description` 里
显式写 "Triggers on Go project development, code review, performance optimization, or architecture design"。
它**没有**任何「仅限 Web 服务」的限定语，所以一个存储引擎项目同样会命中触发条件，
但加载进来的 53 条里相当一部分（见第 6 节）与存储引擎无关。这是本次对齐需要重点关注的风险点。

### SKILL.md 的核心原则（Core Principles，原文照抄）

1. **KISS** - Keep it simple, avoid over-engineering
2. **YAGNI** - Only implement what is currently needed
3. **Explicit over Implicit** - Code intent should be clear
4. **Handle All Errors** - Never ignore error returns
5. **99% Test Coverage** - Foundation for high-quality code

---

## 3. 九个类别（类别名 / 优先级 / 规则条数）

优先级取自 **SKILL.md 和 README.md 的分类表**（两者一致），注意部分**具体规则文件的
frontmatter `impact` 与所属类别的优先级不同**（例如 framework-middleware 是 HIGH，
而它所属的 Framework Selection 类别标称 CRITICAL）。下表第三列同时给出该类别内
各规则**逐条**的 `impact` 分布。

| # | 类别名 | 类别优先级（原文） | 规则条数 | 类别内逐条 impact 分布 |
|---|--------|------------------|---------|----------------------|
| 1 | Framework Selection (`framework-`) | **CRITICAL** | 4 | CRITICAL ×2, HIGH ×2 |
| 2 | Database & ORM (`db-`) | **CRITICAL** | 5 | CRITICAL ×3, HIGH ×1, MEDIUM ×1 |
| 3 | DDD Project Structure (`ddd-`) | **HIGH** | 6 | HIGH ×6 |
| 4 | Error Handling (`error-`) | **HIGH** | 6 | CRITICAL ×1, HIGH ×3, MEDIUM ×2 |
| 5 | Concurrency Patterns (`concurrency-`) | **HIGH** | 7 | CRITICAL ×3, HIGH ×4 |
| 6 | Idiomatic Go (`idiomatic-`) | **MEDIUM** | 11 | HIGH ×3, MEDIUM ×8 |
| 7 | Testing Practices (`testing-`) | **CRITICAL** | 7 | CRITICAL ×1, HIGH ×3, MEDIUM ×3 |
| 8 | Performance Optimization (`performance-`) | **MEDIUM** | 2 | HIGH ×1, MEDIUM ×1 |
| 9 | Lint & Toolchain (`lint-`) | **MEDIUM** | 5 | HIGH ×3, MEDIUM ×2 |
| | **合计** | | **53** | |

> 说明：SKILL.md 的分类表用 `Priority` 列表示建议采纳次序（1~9），用 `Impact` 列表示
> 影响等级（CRITICAL/HIGH/MEDIUM）。上表「类别优先级」即取 `Impact` 列。

---

## 4. 53 条规则全量清单

编号 `gbp-001` … `gbp-053`，**顺序与 SKILL.md / README.md 的 Quick Reference 完全一致**
（即 1 Framework → 2 Database → 3 DDD → 4 Error → 5 Concurrency → 6 Idiomatic →
7 Testing → 8 Performance → 9 Lint），方便与搜索结果的分类表逐条对齐。

| 编号 | 类别 | 规则一句话陈述 | 出自哪个文件 | 原文优先级 | 原文给的正例/反例在讲什么（一句话） |
|------|------|--------------|------------|-----------|--------------------------------|
| gbp-001 | Framework Selection | 小到中型项目、简单 REST API、快速原型应选 Gin | `rules/framework-gin-simple.md` | CRITICAL | 反例是「用 Go-Kratos 全家桶搭一个简单 CRUD API」的过度设计；正例是 `gin.Default()` + 4 个路由 + `c.JSON` |
| gbp-002 | Framework Selection | 复杂微服务（gRPC+HTTP 双协议、服务发现、配置中心）应选 Go-Kratos | `rules/framework-kratos-complex.md` | CRITICAL | 正例是 Kratos 项目结构：`cmd/server/main.go` 用 `wireApp` + `app.Run()`，`internal/service/user.go` 用 `pb.UnimplementedUserServer` 嵌入 |
| gbp-003 | Framework Selection | 日志/鉴权/限流等横切关注点抽成 middleware | `rules/framework-middleware.md` | HIGH | 反例是每个 handler 里重复写日志和 token 校验；正例是 `Logger()` / `Auth()` 两个 `gin.HandlerFunc` + `r.Use(...)` + `r.Group("/api", Auth())` |
| gbp-004 | Framework Selection | 服务必须支持优雅停机，等在途请求处理完 | `rules/framework-graceful-shutdown.md` | HIGH | 反例是 `r.Run(":8080")` 直接退出；正例是 `http.Server` + `signal.Notify(SIGINT, SIGTERM)` + `srv.Shutdown(ctx)`（5s 超时） |
| gbp-005 | Database & ORM | GORM 初始化必须配 logger + 连接池 + PrepareStmt，别忽略 error | `rules/db-gorm-setup.md` | CRITICAL | 反例是 `gorm.Open(...)` 丢错误、无连接池无日志；正例是 `logger.New(...)` 配 SlowThreshold/LogLevel + `db.DB()` 后 `SetMaxIdleConns/SetMaxOpenConns/SetConnMaxLifetime` |
| gbp-006 | Database & ORM | GORM Hook 只放简单自动化逻辑，别塞业务逻辑或外部调用 | `rules/db-gorm-hooks.md` | MEDIUM | 正例是 `BeforeCreate` 里 bcrypt 哈希密码、`AfterFind` 里清空 `PassHash` 不外泄；并注明 hook 出错会回滚事务 |
| gbp-007 | Database & ORM | 多次写操作必须包在 `db.Transaction` 里保证一致性 | `rules/db-gorm-transactions.md` | CRITICAL | 反例是转账扣款和入账分开执行、中途失败导致丢钱；正例是 `db.Transaction(func(tx *gorm.DB) error {...})` + 检查 `RowsAffected`，另附 `SavePoint` 嵌套事务 |
| gbp-008 | Database & ORM | 用 Goose 做受版本控制的数据库 schema 迁移 | `rules/db-goose-migrations.md` | CRITICAL | 正例是 `-- +goose Up` / `-- +goose Down` 的 SQL 迁移文件与 `goose.AddMigrationContext(up, down)` 的 Go 迁移，附 up/down/status/reset 命令 |
| gbp-009 | Database & ORM | 生产环境必须显式配置连接池四个参数 | `rules/db-connection-pool.md` | HIGH | 反例是默认无限连接耗尽数据库资源；正例是 `SetMaxIdleConns/SetMaxOpenConns/SetConnMaxIdleTime/SetConnMaxLifetime` 并给出开发 vs 生产推荐值表 |
| gbp-010 | DDD Project Structure | 按 `cmd/ internal/{domain,application,infrastructure,interfaces} pkg/` 分层组织项目 | `rules/ddd-project-layout.md` | HIGH | 正例是一棵完整目录树；并给出层依赖规则：interfaces → application → domain，infrastructure → domain，domain 无外部依赖 |
| gbp-011 | DDD Project Structure | 领域层放纯业务逻辑，不依赖任何外部框架 | `rules/ddd-domain-layer.md` | HIGH | 正例是 `User` 实体 + `Activate()/CanOrder()` 业务方法、`Email` 值对象 + `NewEmail()` 校验、以及定义在领域层的 `Repository` 接口 |
| gbp-012 | DDD Project Structure | 应用层编排用例，按 CQRS 分离 Command 与 Query | `rules/ddd-application-layer.md` | HIGH | 正例是 `CreateUserCommand` / `GetUserQuery` 分离，`Handler` 注入 `user.Repository`，在 `CreateUser` 里做邮箱校验—查重—保存 |
| gbp-013 | DDD Project Structure | 基础设施层实现领域接口，并做 model ↔ entity 转换 | `rules/ddd-infrastructure-layer.md` | HIGH | 正例是 `UserRepository` 用 GORM 实现，独立 `userModel` 带 `TableName()`，`toDomain`/`toModel` 互转，`gorm.ErrRecordNotFound` 映射为 `user.ErrNotFound` |
| gbp-014 | DDD Project Structure | 接口层把外部请求转成应用层的 command/query | `rules/ddd-interface-layer.md` | HIGH | 正例是 Gin handler：`CreateUserRequest`/`UserResponse` DTO 与领域实体分离、`ShouldBindJSON` 校验、`strconv.ParseUint` 解析 ID、router 注册到 `/api/v1/users` |
| gbp-015 | DDD Project Structure | 用依赖注入解耦，推荐 Google Wire | `rules/ddd-dependency-injection.md` | HIGH | 正例是各层 `ProviderSet = wire.NewSet(...)` + `wire.Bind`，`cmd/server/wire.go` 里 `wire.Build(...)`，`//go:build wireinject` 标签加 `wire` 命令生成 |
| gbp-016 | Error Handling | 用 `fmt.Errorf("...: %w", err)` 包装错误保留完整调用链 | `rules/error-wrap-context.md` | HIGH | 反例是 `return nil, err` 丢失上下文；正例是 `fmt.Errorf("get user %d: %w", id, err)`，调用方用 `errors.Is` 判断根因 |
| gbp-017 | Error Handling | 用包级哨兵错误（sentinel error）表达可预期的错误条件 | `rules/error-sentinel.md` | MEDIUM | 正例是 `var ErrNotFound = errors.New(...)` 一类包级变量，仓储层把 `gorm.ErrRecordNotFound` 转成 `user.ErrUserNotFound`，handler 用 `errors.Is` 分流 404/500 |
| gbp-018 | Error Handling | 需要携带额外信息时定义自定义错误类型 | `rules/error-custom-types.md` | MEDIUM | 正例是 `ValidationError{Field,Message}` / `NotFoundError{Resource,ID}` / `BusinessError{Code,Message,Details}` 三个实现 `error` 接口的结构体，用 `errors.As` 提取后映射到 JSON |
| gbp-019 | Error Handling | 永远不要忽略 error 返回值，真正不需要时用 `_` 并加注释 | `rules/error-handling-check.md` | CRITICAL | 反例是 `data, _ := fetchData()` 后再对空数据 `Unmarshal`；正例是逐层 `fmt.Errorf("fetch data: %w", err)` 返回，并给出 `_ = conn.Close() // 已日志` 的例外与 errcheck 用法 |
| gbp-020 | Error Handling | 定义统一的 API 错误响应结构体与错误码 | `rules/error-api-response.md` | HIGH | 正例是 `ErrorResponse{Code,Message,Details}` + `VALIDATION_ERROR` 等错误码常量 + Gin `ErrorHandler()` 中间件用 `errors.As/Is` 分派，未知错误只回通用文案并打日志 |
| gbp-021 | Error Handling | panic 仅用于不可恢复错误，recover 只在 defer 中有效 | `rules/error-panic-recover.md` | HIGH | 反例是 `GetUser` 查库失败就 `panic(err)`；正例是 init 校验、`MustCompile`、`unreachable()`，以及 handler defer 里 `recover()` + `debug.Stack()`，并强调不要在包边界外暴露 panic |
| gbp-022 | Concurrency Patterns | 每个 goroutine 都必须有明确退出条件，避免泄漏 | `rules/concurrency-goroutine-lifecycle.md` | CRITICAL | 反例是 `go func(){ for { processTask() } }()` 永不退出；正例是 `Worker` 结构体带 `done chan struct{}` + `sync.WaitGroup`，select `ctx.Done()` / `done` / `tasks`，`Stop()` 里 `close(done)` 再 `wg.Wait()` |
| gbp-023 | Concurrency Patterns | 正确使用 channel：超时、非阻塞、fan-out/fan-in 等模式 | `rules/concurrency-channel-patterns.md` | HIGH | 正例是 6 个模式片段（`struct{}` 信号 channel、`select` + `time.After` 收发超时、`default` 非阻塞、fan-out、fan-in 收尾 `wg.Wait()` 后 `close(out)`），并列出「发送方负责关闭」等 4 条规则 |
| gbp-024 | Concurrency Patterns | channel 缓冲大小应为 0 或 1，大缓冲需书面论证 | `rules/concurrency-channel-size.md` | HIGH | 反例是 `make(chan Task, 1000)` 掩盖背压与内存问题；正例是 select 非阻塞发送的 size=1 通知 channel；给出「同步=0 / 通知=1 / 信号量=N / 批量=批大小」选型表，建议用 worker pool 替代大缓冲 |
| gbp-025 | Concurrency Patterns | 用 context 传播取消信号与超时 | `rules/concurrency-context-cancel.md` | CRITICAL | 正例是 `r.Context()` 派生 `WithTimeout` + `defer cancel()`，用 `errors.Is(err, context.DeadlineExceeded/Canceled)` 分流，下游用 `http.NewRequestWithContext` 透传 |
| gbp-026 | Concurrency Patterns | 用 errgroup 简化并发任务与错误传播 | `rules/concurrency-errgroup.md` | HIGH | 正例是 `errgroup.WithContext(ctx)` 并发拉 users/orders/products，聚合切片加 `sync.Mutex`，循环变量 `id := id` 捕获，`g.Wait()` 任一失败即取消其他；另附 `g.SetLimit(10)` 限流 |
| gbp-027 | Concurrency Patterns | 正确使用 sync 包原语（Mutex/RWMutex/Once/Pool/Map） | `rules/concurrency-sync-primitives.md` | HIGH | 正例是 5 个片段：`SafeCounter` 的 `Lock/defer Unlock`、`Cache` 的 `RLock/RUnlock`、`sync.Once` 单例、`sync.Pool` 复用 `bytes.Buffer`、`sync.Map` 的 `Store/Load` |
| gbp-028 | Concurrency Patterns | 用 `go test -race` 检测数据竞争，CI 必须开启 | `rules/concurrency-race-detection.md` | CRITICAL | 反例是无保护共享变量自增、循环变量闭包捕获、并发写 map；正例是 `item := item` 局部拷贝与 `sync.Map`/mutex，并注明 race detector 有 2-20x 开销、不要用于生产 |
| gbp-029 | Idiomatic Go | 遵循 Go 命名惯例（包名、变量、函数、常量、接口） | `rules/idiomatic-naming.md` | MEDIUM | 正例是短小写包名、`userID`/`httpURL` 缩写大小写一致（不是 `userId`）、布尔函数用 `Is/Has/Can`、常量不用 `MAX_RETRIES`、单方法接口用 `-er` 后缀 |
| gbp-030 | Idiomatic Go | 按 Go 惯例写注释与文档注释，错误字符串小写无句点 | `rules/idiomatic-comment.md` | MEDIUM | 反例是注释不以被描述项开头（`// This struct represents...`）和错误字符串首字母大写带句点；正例是 `// Request represents...`、`// Package math provides...`，并主张删掉无信息量的注释 |
| gbp-031 | Idiomatic Go | 定义小接口并按需组合（接口隔离原则） | `rules/idiomatic-interface.md` | HIGH | 反例是一个 9 方法的 `UserService` 大接口；正例是拆成 `UserReader`/`UserWriter`/`UserLister` 再组合，且接口应由**消费方**定义，配 `var _ io.Reader = (*MyReader)(nil)` 编译期断言 |
| gbp-032 | Idiomatic Go | 接收者命名 1-2 字母，同一类型指针/值接收者不要混用 | `rules/idiomatic-receiver.md` | HIGH | 反例是 `func (this *Consumer)` / `func (self *Reader)` 与同类型混用值/指针接收者；正例是 `c/r/b` 且全类型统一指针接收者，并列出值接收者（小不可变、map/func/chan）与指针接收者（要修改、含 Mutex、大结构体）的适用场景 |
| gbp-033 | Idiomatic Go | 结构体初始化一律用字段名，不用位置字面量 | `rules/idiomatic-struct-init.md` | MEDIUM | 反例是 `User{"John", "john@example.com", 25, true}` 依赖字段顺序；正例是带字段名的字面量与 `NewUser` 构造函数（含带校验版本），并附 `Option` 函数式选项写法 |
| gbp-034 | Idiomatic Go | 用函数式选项（functional options）设计灵活的配置 API | `rules/idiomatic-functional-options.md` | HIGH | 正例是 `type Option func(*Server)` + `WithHost/WithPort/WithTimeout/WithLogger` + `NewServer(opts ...Option)` 先设默认值再遍历应用；另给出带校验的 `OptionErr` 版本与选型表 |
| gbp-035 | Idiomatic Go | 用 defer 做资源释放与解锁，注意 LIFO 与参数求值时机 | `rules/idiomatic-defer.md` | MEDIUM | 正例是 `defer f.Close()` / `defer c.mu.Unlock()`；要点包括 defer 后进先出、参数在 defer 时求值（`defer fmt.Println(i)` 输出 0）、循环内 defer 会累积需抽成独立函数 |
| gbp-036 | Idiomatic Go | 正确使用 slice 与 map（预分配、拷贝、遍历中删除） | `rules/idiomatic-slice-map.md` | MEDIUM | 正例是 `make([]int, 0, n)`、`copy` 浅拷贝、`if v, ok := m["key"]` 判存在；反例是边遍历边 `delete`（未定义行为）需先收集 key，并对比 nil slice 与空 slice 的 JSON 序列化差异（`null` vs `[]`） |
| gbp-037 | Idiomatic Go | 利用零值可用性简化代码，并让自定义类型的零值有意义 | `rules/idiomatic-zero-value.md` | MEDIUM | 正例是 `sync.Mutex`/`bytes.Buffer`/`WaitGroup`/`Once` 零值即可用；`Config{Timeout:0}` 表示「用默认值」；用 `EnableCache` 而非双重否定 `DisableDisableCache`；`*string` 为 nil 表示未设置 |
| gbp-038 | Idiomatic Go | 用类型嵌入做组合复用，但不要在公开 API 里嵌入 | `rules/idiomatic-embedding.md` | MEDIUM | 正例是嵌入 `io.Reader`/`io.Writer` 组合接口、嵌入 `*log.Logger` 复用方法；反例是公开 `Client` 嵌入 `http.Client` 泄漏实现（标注 Uber Style），应改为显式字段加委托；另附命名冲突与遮蔽说明 |
| gbp-039 | Idiomatic Go | 正确使用空白标识符 `_`（忽略返回值、副作用导入、编译期断言） | `rules/idiomatic-blank-identifier.md` | MEDIUM | 正例是 `_, err := io.Copy(...)`、`_ "github.com/go-sql-driver/mysql"` 副作用导入、`var _ io.Reader = (*MyReader)(nil)`；反例是 `data, _ := json.Marshal(obj)` 危险地吞错，必须忽略时加注释说明 |
| gbp-040 | Testing Practices | 生产代码目标 99% 测试覆盖率 | `rules/testing-coverage-99.md` | CRITICAL | 正例是 `go test -coverprofile` + `go tool cover -func/-html` 流程、CI 中低于 99% 就 `exit 1` 的 shell 片段、以及分层覆盖目标表（domain 100% / application 99% / infra 95% / interface 90%） |
| gbp-041 | Testing Practices | 用表驱动测试提升可维护性 | `rules/testing-table-driven.md` | HIGH | 正例是匿名 struct 切片 + `t.Run(tt.name, ...)` 子测试；进阶例子在表里放 `setup func(*testing.T)` 做依赖构造，并按 `wantErr` 断言 |
| gbp-042 | Testing Practices | 通过接口抽象 + mockgen 做依赖注入与 mock 测试 | `rules/testing-mock.md` | HIGH | 正例是 `//go:generate mockgen -source=...` 生成 mock，`gomock.NewController` + `EXPECT().FindByEmail(...).Return(...)` + `DoAndReturn` 模拟生成 ID；另给手写 mock 结构体的简版写法 |
| gbp-043 | Testing Practices | 测试辅助函数必须调 `t.Helper()`，且校验逻辑留在测试里 | `rules/testing-helper.md` | MEDIUM | 正例是 `t.Helper()` 让报错行号指向调用处、`t.Cleanup()` 自动清理；反例是 helper 里塞断言逻辑，应改成只取数（`getUserCount`）由测试断言；并警告不要在 goroutine 里调 `t.Fatal`，要用 channel 回传错误 |
| gbp-044 | Testing Practices | 用 benchmark 量化性能，配合 benchstat 对比 | `rules/testing-benchmark.md` | MEDIUM | 正例是 `for i := 0; i < b.N; i++` 基准、`-benchmem`/`-count` 参数、`b.Run(fmt.Sprintf("size-%d", size))` 子基准、`b.ResetTimer()`，以及用包级变量接收结果防止编译器优化掉 |
| gbp-045 | Testing Practices | 用 build tag + testcontainers 写集成测试 | `rules/testing-integration.md` | HIGH | 正例是 `//go:build integration` 标签隔离（默认 `go test ./...` 不跑）、`mysql.RunContainer` 起真实 MySQL 容器 + `defer Terminate`、以及 `httptest.NewServer` 打 HTTP API 的端到端断言 |
| gbp-046 | Testing Practices | 用 testify 让断言更清晰，区分 assert 与 require | `rules/testing-testify.md` | MEDIUM | 正例是 `assert.Equal` 失败后继续 vs `require.NoError` 立即终止的取舍、`assert.ErrorIs/ErrorContains`、`assert.Len/Contains/Empty`，以及 `suite.Suite` + `SetupSuite/TearDownSuite` 套件写法 |
| gbp-047 | Performance Optimization | 基础类型转换用 strconv 而不是 fmt | `rules/performance-strconv.md` | MEDIUM | 反例是 `fmt.Sprintf("%d", 42)` 与循环里反复 `Sprintf`；正例是 `strconv.Itoa`/`FormatInt`/`FormatFloat`/`FormatBool`，给出 benchmark 数字（30ns vs 120ns，声称 4x 快、少 50% 分配） |
| gbp-048 | Performance Optimization | 已知大小时预分配 slice 与 map 容量 | `rules/performance-prealloc.md` | HIGH | 反例是 `var result []int` 反复 append 触发多次扩容、`make(map[string]int)` 反复 rehash；正例是 `make([]int, 0, n)` 或已知长度时 `make([]int, n)` 直接下标赋值，以及 `make([]T, 0, len(m))` 由源容器推导容量，附 3.75x/少 80% 内存的 benchmark |
| gbp-049 | Lint & Toolchain | 用 golangci-lint 做综合检查并给出推荐 `.golangci.yml` | `rules/lint-golangci.md` | HIGH | 正例是一份完整配置：enable errcheck/gosimple/govet/staticcheck/gofmt/goimports/revive/gosec 等，配置 revive 规则清单与 gocritic tags，并对 `_test\.go` 排除 gosec/errcheck |
| gbp-050 | Lint & Toolchain | 用 gofmt + goimports 保持格式一致并分组 import | `rules/lint-gofmt.md` | MEDIUM | 正例是 `gofmt -w .`、CI 中 `gofmt -d . \| grep -q . && exit 1`、`goimports -local mycompany.com/myproject`，以及「标准库 / 第三方 / 本地包」三段式 import 分组、VS Code 配置与 pre-commit hook 脚本 |
| gbp-051 | Lint & Toolchain | 用 go vet 做静态分析发现潜在 bug | `rules/lint-govet.md` | HIGH | 正例是 8 类 go vet 能抓的问题：Printf 格式错配、未使用返回值、不可达代码、忘记 Unlock、循环变量捕获、struct tag 写错、拷贝 `sync.Mutex`、atomic 误赋值；另附 shadow 变量遮蔽检查与 CI 片段 |
| gbp-052 | Lint & Toolchain | 用 staticcheck 做深度静态检查 | `rules/lint-staticcheck.md` | HIGH | 正例是按 SA1/SA2/SA4/SA5/SA9 分类列举典型告警：非法正则、Printf 参数类型错、nil context、goroutine 内调 `wg.Add`、无意义比较、赋值未被使用、defer 里 `os.Exit`、空 if 分支；附 `staticcheck.conf` 排除写法 |
| gbp-053 | Lint & Toolchain | 用 revive 做可配置的 lint，含复杂度/长度上限 | `rules/lint-revive.md` | MEDIUM | 正例是一份 `revive.toml`：启用 context-as-argument、error-return、error-strings、var-naming 等规则，并设置 `cognitive-complexity=15`、`function-length=50`、`argument-limit=5`、`function-result-limit=3`；附规则速查表 |

---

## 5. 原文每条规则的「依据来源」标注

### 重要发现：**53 个规则文件中没有任何一条给出逐条出处标注**

逐文件检查了所有 53 个 `rules/*.md`，其结构统一为：

```yaml
---
title: ...            # 规则标题
impact: CRITICAL|HIGH|MEDIUM
impactDescription: ...
tags: ...
---
## 标题
**Incorrect / Bad Example ...:**  <代码块 + 分析>
**Correct / Good Example ...:**   <代码块 + 分析>
**Key Points / ... :**            <要点列表>
```

**没有 `references:` / `sources:` / `see also:` 字段，正文里也没有 Effective Go / Google Go Style
Guide / Uber Go Style Guide / Go Code Review Comments 的逐条援引。** 因此第 4 节表格的
「原文依据来源」一列**在 53 条上全部为空**（如实留空，未编造）。

四份上游资料的出处**只在包级别**被整体声明一次，位置有两处：

**SKILL.md 第 8 行（原文照抄）：**
> Contains 53 rules referenced from Effective Go, Google Go Style Guide, Uber Go Style Guide, and Go Code Review Comments.

**README.md Overview 章节（原文照抄）：**
> This skill provides 53 curated best practice rules organized into 9 categories, referenced from:
> - [Effective Go](https://go.dev/doc/effective_go)
> - [Google Go Style Guide](https://google.github.io/styleguide/go/)
> - [Uber Go Style Guide](https://github.com/uber-go/guide)
> - [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)

### 全文唯一一处指向具体上游的正文标注

`rules/idiomatic-embedding.md` 的 "Avoid Embedding in Public APIs" 小节标题带上游署名：

```go
// Avoid Embedding in Public APIs (Uber Style):

// Wrong: Embedding in public API leaks implementation details
type Client struct {
    http.Client  // Exposes all http.Client methods
}
```

即 **gbp-038 是 53 条中唯一一条正文显式点名来源（Uber Go Style Guide）的规则**。

### 对 taihu 对齐工作的含义

由于**上游没有逐条 provenance**，无法通过「按出处筛选」来挑选适用规则；
第 6 节与最终报告的「与存储引擎相关」判断，只能基于**规则内容本身**做出，
不能引用上游的出处声明作为权威依据。如果 taihu spec 需要写明每条规则的来源，
需要自行回溯到 Effective Go / Uber / Google 的原文另行求证。

---

## 6. 显式「面向应用 / 微服务」的规则

这一节单独列出**假设了一个 Web 应用 / 微服务架构**（HTTP 框架、ORM、关系型数据库迁移、
DDD 分层目录、REST 错误响应体）的规则。**这批对存储引擎 / 系统编程项目大概率不适用**，
是本次对齐中最需要做删减或改写的部分。

### 6.1 完全不适用 —— 整个类别都建立在应用/微服务假设上

| 编号 | 文件 | 不适用原因 |
|------|------|-----------|
| gbp-001 | `framework-gin-simple.md` | 整条讲 Gin（HTTP Web 框架）的选型 |
| gbp-002 | `framework-kratos-complex.md` | 整条讲 Go-Kratos 微服务框架（gRPC/HTTP、服务发现、配置中心） |
| gbp-003 | `framework-middleware.md` | 反例正例全基于 Gin 的 `gin.Context` / `c.GetHeader("Authorization")` / JWT 鉴权 |
| gbp-004 | `framework-graceful-shutdown.md` | 基于 `http.Server.Shutdown` + SIGINT/SIGTERM 的 Web 服务停机 |
| gbp-010 | `ddd-project-layout.md` | DDD 四层目录 `domain/application/infrastructure/interfaces`，为业务应用设计 |
| gbp-011 | `ddd-domain-layer.md` | 以 `User` 实体 / `Email` 值对象 / 仓储接口为例的业务领域建模 |
| gbp-012 | `ddd-application-layer.md` | CQRS 的 Command/Query + 用例 Handler |
| gbp-013 | `ddd-infrastructure-layer.md` | 以 GORM 实现 `UserRepository` 的基础设施层 |
| gbp-014 | `ddd-interface-layer.md` | Gin HTTP handler + DTO + router 注册 |
| gbp-015 | `ddd-dependency-injection.md` | Google Wire 依赖注入代码生成 |
| gbp-020 | `error-api-response.md` | 统一 REST 错误响应体 `{code,message,details}` 与 HTTP 状态码映射 |
| gbp-040 | `testing-coverage-99.md` | 「99% 覆盖率」硬指标 + 按 domain/application/infra/interface 分层设目标 |

**小计：12 条（gbp-001/002/003/004/010/011/012/013/014/015/020/040）**

### 6.2 强绑定关系型数据库 / ORM —— 存储引擎项目通常不涉及

| 编号 | 文件 | 不适用原因 |
|------|------|-----------|
| gbp-005 | `db-gorm-setup.md` | GORM 初始化 + `SetMaxOpenConns` 等 SQL 连接池参数 |
| gbp-006 | `db-gorm-hooks.md` | GORM 生命周期 Hook（`BeforeCreate`/`AfterFind` 等） |
| gbp-007 | `db-gorm-transactions.md` | `db.Transaction` 与 SavePoint 嵌套事务 |
| gbp-008 | `db-goose-migrations.md` | Goose schema 迁移工具（`-- +goose Up/Down`） |
| gbp-009 | `db-connection-pool.md` | 关系型数据库连接池调优（对比 `wait_timeout`） |
| gbp-045 | `testing-integration.md` | 集成测试依赖 testcontainers 起 MySQL 容器 + HTTP API 端到端 |

**小计：6 条（gbp-005/006/007/008/009/045）**
（注：gbp-045 的 build tag 隔离手法本身可迁移，但正文举例全是 MySQL 容器与 HTTP 服务。）

### 6.3 混合 —— 部分可迁移，应用向的那半不适用

| 编号 | 文件 | 说明 |
|------|------|------|
| gbp-042 | `testing-mock.md` | mockgen/gomock 本身通用，但示例围绕 `UserRepository` 与 Gin handler；手写 mock 手法可迁移 |
| gbp-046 | `testing-testify.md` | testify 是通用断言库，但 `suite.Suite` 里挂 `*gorm.DB` 的示例是应用向 |
| gbp-052／gbp-049 | `lint-staticcheck.md` / `lint-golangci.md` | 工具链通用，但配置里 `noctx`（HTTP 请求缺 context）、`bodyclose`、`sqlclosecheck`、`gosec` 等 linter 是服务向 |

### 6.4 「面向应用/微服务」编号汇总（供快速摘除）

> **gbp-001 ~ gbp-015（连续 15 条）、gbp-020、gbp-040、gbp-045**
> 即：**1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 20, 40, 45** —— 共 **18 条**（含 6.3 的混合项则为 21 条）。
>
> 其中 **gbp-005 ~ gbp-009（Database & ORM 全类别 5 条）** 与 **gbp-010 ~ gbp-015（DDD Project Structure
> 全类别 6 条）** 是**整个类别都不适用**。
>
> 按「类别整体」看，**第 1、2、3 类（Framework / Database & ORM / DDD，共 15 条）** 加上
> gbp-020、gbp-040、gbp-045，是存储引擎项目最应先排除的部分。

---

## 附：抓取一致性与可信度说明

- 53/53 规则文件全部抓取成功，无失败、无占位、无编造。
- 所有引用的原文片段（SKILL.md frontmatter、When to Apply、Core Principles、README Overview、
  idiomatic-embedding 的 Uber 标注）均为原文照抄。
- 第 4 节表格中的「规则一句话陈述」与「正例/反例」两列是对原文的**忠实压缩**，不是逐字摘录；
  如需逐字引用请以 `https://raw.githubusercontent.com/cexll/golang-base-practices-skills/master/rules/<file>` 为准。
- 仓库仅 2 个 commit，且最后提交由 `SWE-Agent.ai` 协作生成（`Co-Authored-By: SWE-Agent.ai`），
  内容质量与维护活跃度需自行评估；本快照固定到 commit `26426d2b`，上游若更新本文档即过期。
- 注入扫描：未发现针对 AI 的指令注入。
