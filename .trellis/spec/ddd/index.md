# ddd —— DDD 项目结构（gbp 类 3）

> 来源：`cexll/golang-base-practices-skills` 的 `rules/ddd-*.md`，6 条。
> **本层 6 条对 taihu 全部不适用。** 保留这一层是为了记住否决理由——**这是三层里最容易被人重新提议的一层**。

## 为什么整层不适用

gbp 这一类的前提是「你有一个业务领域要建模」。taihu 是存储引擎，没有业务域实体。

**决定性的一条是 gbp-010。** 它要求按 `cmd/ internal/{domain,application,infrastructure,interfaces} pkg/` 分层，而 taihu 的首层目录是：

```
internal/{aio,benchkit,bufpool,cluster,device,ierr,layout,metastore,rpcclient,storage,transport,version}
```

**这是按「资源与机制」分层，不是按「业务域职责」分层。** `internal/aio` 是异步 IO 后端，`internal/device` 是块设备，`internal/layout` 是磁盘格式——每个名字都对应一个**物理对象或机制**，不是一层职责。

## gbp-010 与现有门禁直接冲突

这不是「风格不同」，是**会打起来**。仓库当前有两道 Makefile 门禁：

```make
check-layering:    # internal/ 不得依赖 pkg/（pkg/ 只放客户端 SDK）
check-sdk-only:    # pkg/ 不得直接依赖引擎内部（storage/device/aio/bufpool/layout），测试除外
```

它们编码的是 taihu 真实的依赖方向：**引擎在内，SDK 在外，方向单一**。

若改成 gbp-010 的 `domain / application / infrastructure / interfaces`：

- `internal/device`、`internal/aio` 这类**贴着内核与设备**的包，在 DDD 四层里没有位置——它们既不是 domain（不含业务规则），也不是 infrastructure（不是"实现 domain 接口的适配器"，它们是自己定义契约的一方）；
- `check-sdk-only` 的语义会失去依据——它保护的「SDK 不得反向依赖引擎内部」在 DDD 分层里没有对应表述；
- 两道门禁要么被改掉，要么变成无人理解的遗留物。**两道门禁都是能跑的命令，不是文档。**

所以本条否决的理由是可检验的：**采纳它必须同时删除两道门禁**，而门禁保护的是一个真实存在的分层不变量。

## 逐条裁决

### gbp-010 · Standard Project Directory Structure（HIGH）

**规则**：按 `cmd/ internal/{domain,application,infrastructure,interfaces} pkg/` 分层；依赖方向 interfaces → application → domain，infrastructure → domain，domain 无外部依赖。

**对 taihu：不适用，且与门禁冲突。** 理由见上。**注意本条与本 spec 的 [layout/](../layout/index.md) 层不冲突**：layout 层裁决的是 project-layout 的目录**用途**，不是 DDD 的四层职责划分。

### gbp-011 · Domain Layer Design（HIGH）

**规则**：领域层放纯业务逻辑，不依赖任何外部框架；正例是 `User` 实体、`Email` 值对象、领域层定义的 `Repository` 接口。

**对 taihu：不适用。** 本仓库没有业务域模型——没有 `User` 这类实体。它的「领域」是**段 / 对象 / 映射**，且**必须贴着设备与内核**：`internal/device` 直接和文件描述符、`O_DIRECT` 打交道，`internal/aio` 直接发 `io_uring` / `libaio` 系统调用。「不依赖外部」在这里不成立，也不应该成立。

### gbp-012 · Application Layer Design（HIGH）

**规则**：应用层编排用例，按 CQRS 分离 Command 与 Query。

**对 taihu：不适用。** 无 CQRS、无用例 Handler。编排面是 RPC 服务端 `internal/transport/server.go` 的 `handlePut` / `handleGet` 一族，与 Command/Query 分离是两套建模。

### gbp-013 · Infrastructure Layer Design（HIGH）

**规则**：基础设施层实现领域接口，并做 model ↔ entity 转换；正例是 GORM 实现 `UserRepository` + `toDomain` / `toModel` 互转。

**对 taihu：不适用。** 无 ORM（`grep -rlE 'gorm|database/sql'` → 0），无 entity / model 双层映射。本仓库确实有双表示——`internal/layout` 的磁盘格式与 `internal/storage` 的内存语义——但那是**同一个对象的两种物理表示**，转换是为了对齐与序列化，不是为了「领域模型 ↔ 持久化模型」的解耦。

### gbp-014 · Interface Layer Design（HIGH）

**规则**：接口层把外部请求转成应用层的 command/query；正例是 Gin handler + DTO + `ShouldBindJSON`。

**对 taihu：不适用。** 无 HTTP 接口层。外部请求的入口是**二进制帧解析**（`internal/transport/protocol`），取参方式是按偏移读字节，不是 JSON 绑定。

### gbp-015 · Dependency Injection Patterns（HIGH）

**规则**：用依赖注入解耦，推荐 Google Wire。

**对 taihu：不适用。** 无 wire（`grep -rl 'google/wire'` → 0，全仓 `wire.Build` 零命中）。

本仓库的装配形态是**显式构造函数参数**（如 `transport.NewServer(storage)`）加少量变参选项（`internal/storage/options.go`）。这是刻意的：**装配点少且集中在 CLI 层**，为此引入代码生成不划算。这一条属于「已知不适用」，不是「还没做」。
