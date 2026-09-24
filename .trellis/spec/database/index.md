# database —— 数据库与 ORM（gbp 类 2）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/db-*.md`，5 条。
> **本层 5 条对 taihu 全部不适用。** 保留这一层是为了记住否决理由。

## 为什么整层不适用

gbp 这一类的前提是「你的程序**使用**一个数据库」。taihu **本身就是**存储引擎：它自己就是被使用的那一层。

- 持久化是自己实现的：`internal/layout`（磁盘格式）+ `internal/metastore`（Pebble 封装）+ `internal/storage`（对外语义）。
- `cockroachdb/pebble v1.1.5` 是**被封装的对象**，不是「被使用的数据库驱动」——Pebble 是 KV 引擎，没有 SQL、没有连接池、没有 ORM 生命周期。

核实：`grep -rlE 'gorm|database/sql|sqlx' --include='*.go' internal cmd pkg examples` → 0；`grep -rl 'goose'` → 0。

**这一层最值得记住的判据**：要求一个存储引擎「用 Goose 做 schema 迁移」「配 GORM 连接池」，是把**被实现物当成了依赖**。方向反了。

## 逐条裁决

### gbp-005 · GORM Initialization and Configuration（CRITICAL）

**规则**：`gorm.Open` 必须配 logger + 连接池 + `PrepareStmt`，且不能忽略返回的 error。

**对 taihu：不适用。** 无 GORM，无可初始化的 ORM 句柄。

### gbp-006 · GORM Hook Usage Guidelines（MEDIUM）

**规则**：GORM Hook 只放简单自动化逻辑（如 `BeforeCreate` 里哈希密码），别塞业务逻辑或外部调用。

**对 taihu：不适用。** 无 GORM，无 `BeforeCreate` / `AfterFind` 生命周期回调。

### gbp-007 · Transaction Handling Patterns（CRITICAL）

**规则**：多次写操作包在 `db.Transaction` 里保证原子性，并检查 `RowsAffected`。

**对 taihu：不适用（语义不对应）。** 本仓库的原子性靠 Pebble 的 `Batch` 与 `internal/metastore` 的批提交，是 LSM 的写入批次语义，与 SQL 事务不是一回事——没有隔离级别、没有回滚点、没有 `RowsAffected` 这类行计数。

### gbp-008 · Database Migrations with Goose（CRITICAL）

**规则**：用 Goose 做受版本控制的 schema 迁移（`-- +goose Up` / `Down`）。

**对 taihu：不适用，且方向反了。** 本仓库无关系型 schema，也没引入迁移工具。**磁盘格式的版本兼容是 taihu 自己要实现的机制**，不是它能拿来用的依赖。核对时若看到有人提「引入 Goose」，先确认他是不是把 taihu 当成了一个应用。

### gbp-009 · Connection Pool Configuration（HIGH）

**规则**：生产环境显式配置连接池四个参数（`SetMaxIdleConns` / `SetMaxOpenConns` / `SetMaxConnIdleTime` / `SetConnMaxLifetime`）。

**对 taihu：不适用。** 无数据库连接池。形态上最接近的是 `pkg/bufpool`，但那是 4K 对齐的**内存缓冲桶**，不是连接池：

- 它管的是 `[]byte` 的复用与对齐，没有「连接」这个对象；
- 它**刻意否决了 `sync.Pool`** —— `pkg/bufpool/bufpool.go:10-14` 写明了实测依据（`sync.Pool` 会被 GC 清空，导致 4M/8M 大缓冲每轮重分配）。

四个 `Set*` 参数在这里没有任何对应物。
