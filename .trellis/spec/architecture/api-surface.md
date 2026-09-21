# 包对外面：可见性、删留与摆放

> 每个包都有一张对外的脸。这张脸要**窄**（默认私有）、要**真被用**（只被测试用的删掉）、要**能一眼看全**（集中到一个文件）。而判断一个符号该不该留，看的是**可达性**不是**引用计数** —— 「外部没写这个名字」不等于「外部没用它」。

---

## 规则 1：包内标识符默认私有，确有外部调用才导出

`internal/` 下的包，外部模块**本来 import 不了**（Go 语言规则：`github.com/liucxer/taihu/internal/...` 只能被以 `github.com/liucxer/taihu` 为根的包 import；实测 `pkg/` 下 `grep -rn 'internal/aio'` 零命中）。所以这里的「导出」从一开始就不是为了模块外的消费方，只是为了同模块的兄弟包 —— **该导出的集合天然很窄，默认值应当是不导出**。

落到本仓库：后端构造函数一律不导出 —— `newLibAIORing`（`internal/aio/aio_linux.go:56`、`internal/aio/aio_other.go:31`）、`newIOUringRing`（`internal/aio/aio_uring_linux.go:200`、`internal/aio/aio_other.go:42`）；环境变量名 `envMode` 也只是包内常量（`internal/aio/aio.go:129`），运维侧读到的仍是字符串值 `TAIHU_AIO_URING`，导出与否对外无感。

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

**（a）配置可达 —— `ModeLibAIO`。** `grep -rn 'aio\.ModeLibAIO'` 在 `internal/aio` 之外无命中，外部没人写过这个名字。但它由 `ParseMode("off")` 返回（`internal/aio/aio.go:122-123`），而 `cmd/taihu/cmd/root.go:118` 调 `aio.ParseMode(global.ioUring)` —— `-io-uring=off` 是 CLI 上公开的取值。名字没被写，**取值被配置到达**。

**（b）返回值可达 —— `Info`。** 同样零引用，但它是导出函数 `Probe()` 的返回类型（`internal/aio/aio.go:238`）。`internal/device/device.go:82-83` 写着：

```go
if o.aioIOPoll && (o.aioMode == aio.ModeIOUring ||
	(o.aioMode == aio.ModeAuto && aio.Probe().Supported)) {
```

`aio.Info` 这个名字一次都没出现，依赖却真实存在 —— 收起来会让 `aio.Probe().Supported` 变成对不可命名类型的字段访问。

**判据**：删之前逐条问「外部能不能通过**配置取值**、通过**返回值/参数类型**、通过**接口满足**到达它」。三条都不成立才是真死代码。作为对照，其余 14 个导出名都有实打实的生产引用点（`internal/device`、`internal/storage`、`cmd/taihu/cmd/root.go`）。

## 规则 4：一个包的对外面集中在一个文件

想看这个包对外提供什么，应当**只开一个文件**。`internal/aio` 的全部 16 个导出名（`ErrFull`/`ErrTimeout`、`ReadSpec`/`WriteSpec`/`Event`/`Ring`、`Mode`+三个常量/`ParseMode`、`Options`/`NewWithOptions`、`Info`/`Probe`/`CheckIOPoll`）都定义在 `internal/aio/aio.go`，包文档末尾也写明了这一点（`internal/aio/aio.go:23-25`）。

实现文件则按**实现 / 平台**分，不按「导出 / 未导出」分：

| 文件 | 职责 |
|------|------|
| `aio.go` | 全部对外 API + 后端选型分派 + 启动日志 |
| `aio_linux.go` | libaio 后端（linux） |
| `aio_uring_linux.go` | io_uring 后端（linux） |
| `aio_other.go` | 非 Linux 兜底后端（`!linux`） |
| `probe_linux.go` / `probe_other.go` | 平台相关的探测与校验**实现**（不是 API） |

> **这是新立的约定，目前只有 `internal/aio` 一个实例，别把它说成既有全仓惯例。** 实测其余包的对外面仍是散着的：`internal/device` 散在 4 个文件、`internal/transport` 散在 9 个、`internal/cluster` 散在 7 个。**新增包照这条来；已有包不要求追溯改造**，遇到了再收。

## 规则 5：平台分裂的符号，靠「缓存上移 + 薄转发」进那个文件

规则 4 落地时会撞上一个硬约束：有些符号天然是**平台分裂**的 —— `Probe` / `Info` / `CheckIOPoll` 在 `probe_linux.go` 与 `probe_other.go` 各有一份实现或声明。这不是冗余，是编译期约束：`Probe` 的 Linux 实现要调 `io_uring_setup`，非 Linux 没有那个系统调用号，**同一份代码放不进一个文件**。

两种解法，按「有没有平台无关的部分」区分：

**（a）缓存上移** —— 平台无关的逻辑搬进对外面文件，平台文件只留真正的差异。`Probe()` 的结论缓存（`probeMu` / `probeInfo` / `probeDone`，`internal/aio/aio.go:222-226`）与操作系统无关，只是当年被复制了两份；上移后平台文件只剩 `probe() (Info, bool)`（`internal/aio/probe_linux.go:19`、`internal/aio/probe_other.go:9`）。**顺带消掉了 `Info` 与 `Probe` 在两个平台文件里的重复声明**。

**（b）薄转发** —— 整个函数体都是平台相关的，对外面文件只放声明并转发：

```go
// CheckIOPoll 校验目标块设备是否开启了队列级轮询 —— IOPOLL 的前置条件。
// ...
// 实现按平台分文件：Linux 读 sysfs 校验，非 Linux 恒报不支持（见 probe_other.go）。
func CheckIOPoll(devPath string) error {
	return checkIOPoll(devPath)
}
```
（`internal/aio/aio.go:257-259`；实现见 `internal/aio/aio_uring_linux.go:20` 与 `internal/aio/probe_other.go:17`，**报错文案一字未改**）

**取舍**：也曾考虑「直接用 sysfs 实现、删掉非 Linux 存根」—— 代码更少、没有转发层。否掉的理由是错误信息：那样 macOS 上跑 `-io-uring=on -io-uring-iopoll` 会从 `aio: IOPOLL 仅 Linux 支持` 退化成「读取 `/sys/class/block/...` 失败」。**这一层转发换来的是平台正确的报错。**

---

## 附：`internal/aio` —— 本组规则的参考实现

按四条要求依次收紧的完整过程（2026-09-21）：

| 要求 | 动作 | 结果 |
|------|------|------|
| 默认私有，除非有外部调用 | `New`→`newRing`、`NewWithMode`→`newWithMode`、`EnvMode`→`envMode`、`Mode.String()`→`Mode.string()` | 4 个名字收起 |
| 外部不使用的都删除（先分析后拍板） | 逐符号清点「从包外是否可达」 | 产出规则 3 的两条反例判据 |
| 只被测试代码在用的才删 | 删 `newRing`/`newWithMode`/`Mode.string()` | `TestNewRing`/`TestModeString` 一并删除，`TestNewWithModeInvalidMaxEvents` 改写保留 |
| 对外面集中到一个文件 | 16 个导出名并入 `aio.go`，删 `mode.go` | `aio.go` 99 → 259 行 |

**过程中唯一一次差点删错**：分析时把 io_uring 整块（`aio_uring_linux.go` 一个文件 662 行，占包内 1497 行非测试代码的 44%）摆成「待判断是否可删」的大项，被人一句「io_uring 属于业务在用的嘛」纠正。核实链路后确认它在**生产默认路径**上：`cmd/taihu/cmd/server.go` → `aioOptions()`（`cmd/taihu/cmd/root.go:117`）→ `storage.WithAIOMode` → `device.WithAIOMode` → `aio.NewWithOptions`（`internal/device/device.go:95`），且 `-io-uring` 默认值就是 `auto`（`cmd/taihu/cmd/root.go:37`、`:94`），探测通过即建 io_uring。它在 BCLinux 4.19.90 上回退 libaio 是**那台机器**没走进去，不是链路没用它。

**已知遗留**：`Info` 的 `KernelRelease` / `SQEntries` / `CQEntries` 三个字段，生产代码**只写不读**（启动日志实际调的是 `kernelRelease()` 函数与 `ringQueueDepth(r)`，不是这几个字段），同结构的 `Supported` / `Reason` / `Features` 都有真实读取点。按规则 2、3 的判据它们属于待删项，但当时未作裁定，现状是保留（`internal/aio/aio.go:213-220` 仍是 6 个字段）。
