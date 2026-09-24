# aio：设备 fd 下沉到 ring 构造绑定

## Context

当前 `Ring` 接口的 4 个提交方法都要求调用方**每次 Submit 传入 fd**（`SubmitRead(fd, buf, off)` 等），device 层每次提交都要取一次 `fd := int(d.f.Fd())`。但确认的拓扑是：**一个设备 1 个 fd、ring 与设备一对一**，fd 在 ring 存活期内恒定。用户决策：**fd 下沉到 aio 层，构造 ring 时绑定；aio 明确作为内部组件，不计划开源**（故不追求与内核 per-op fd API 的通用性对齐）。

重构目标：`Ring` 提交方法签名去掉 fd 参数，fd 在构造时绑定进 ring 结构体。构造不做 fd 有效性校验（提交期由内核返回 errno，与现状语义一致）。

## 设计决策

1. **fd 放进 `aio.Options` 新增字段 `FD int`**，`NewWithOptions(o Options, devPath string)` 签名不变。
   - 好处：30+ 测试里不真正提交 IO 的选路用例（env/auto/IOPoll/maxEvents）零改动；`NewWithOptions` 签名行不上 spec 引用的雷区。
   - 语义：`FD=0` 或不合法时，仅在提交时报内核错误（与现状一致）。
2. **结构体字段命名**：
   - libaio `ring` 与 fallback `ring`：新增 `fd int`（无既有 fd 字段，直接命名 fd）。
   - io_uring `uringRing`：**新增 `devFD int`**——现有 `fd` 字段是 io_uring_setup 返回的 ring fd（`TestUringEnterFailureKeepsRingUsable`/`TestUringWaitNoIO` 故意污染它），命名区分防止混淆，且不触碰这些测试语义。
   - `submit` 内部：libaio `Fildes: uint32(r.fd)`、uring `FD: int32(r.devFD)`、fallback `unix.Pread(r.fd, ...)`。
3. **构造签名**：`newLibAIORing(fd int, maxEvents int)`、`newIOUringRing(fd int, maxEvents int, iopoll bool)`（fallback 的 `newIOUringRing` 桩同步改签名；两平台签名必须一致，aio.go 统一调）。
4. **契约 harness**：`backend.new` 签名改为 `new func(t *testing.T, fd int, maxEvents int) Ring`；各契约用例把 `ch.open` 提前、先拿 fd 再建 ring。`ringWrite`/`ringRead` helper 去掉 fd 参数。

## 改动清单

### aio.go
- `Ring` 接口 6 方法：4 个提交方法去 fd（`SubmitRead(buf, off)`、`SubmitReadBatch(specs)`、`SubmitWrite(buf, off)`、`SubmitWriteBatch(specs)`）；Wait/Close 不变。
- 接口注释重写：方法注释删 fd；`ReadSpec`/`WriteSpec` 的「同一批共享同一 fd」改为「提交用 ring 构造时绑定的 fd」；`Submit*Batch` 的「（同一 fd，摊薄 syscall）」去掉 fd 表述。
- `Options` 新增 `FD int` 字段 + 注释（ring 存活期内 fd 必须有效；ring 不关闭 fd；非法 fd 提交时返回内核错误）。
- `NewWithOptions`：3 处 `newIOUringRing(o.MaxEvents, o.IOPoll)` → `newIOUringRing(o.FD, o.MaxEvents, o.IOPoll)`；2 处 `newLibAIORing(o.MaxEvents)` → `newLibAIORing(o.FD, o.MaxEvents)`。

### 三个后端
- `aio_libaio_linux.go`：`ring` 结构体加 `fd int`；4 个 Submit 方法去 fd；`submit`/`submitBatch` 去 fd 参数改用 `r.fd`（:114/:152 的 `Fildes` 赋值处）；`newLibAIORing(fd int, maxEvents int)`。
- `aio_uring_linux.go`：`uringRing` 加 `devFD int`；4 个 Submit 方法去 fd；`submit(fd, specs, op)` → `submit(specs, op)`，SQE `FD: int32(r.devFD)`（:331 附近）；`newIOUringRing(fd int, maxEvents int, iopoll bool)`；其余（reap/Wait/Close）不动——`TestUringReapFakes`/`TestUringWaitNoIO` 的零值/伪造结构体字面量不受新字段影响。
- `aio_fallback_other.go`：`ring` 加 `fd int`；4 个 Submit 方法去 fd；`submit` 用 `r.fd`；`newLibAIORing(fd int, maxEvents int)`；`newIOUringRing` 桩改 `(fd int, maxEvents int, iopoll bool)`。

### device 层
- `device.go` `NewDevice`（:87-88）：`aio.Options{...}` 加 `FD: int(f.Fd())`。
- `submitOpN`（:190 删 `fd := int(d.f.Fd())`；:204/:206 去 fd）、Append 路径（:497 / :514）、ReadAt 批路径（:671 / :680）同样去 fd。
- `device_compl_retry_test.go` `fakeRing`：4 个 Submit 方法签名去 fd（它本就不使用 fd 值）。

### aio 测试（关键：行号为改前，实施时以编译错误为准逐个修）
- 契约 harness：`linuxRingBackends`（aio_linux_test.go:60-93）、`fallbackBackend`（aio_darwin_test.go:52-65）的 `new` 回调收 fd，`Options` 加 `FD: fd`。
- 契约用例（aio_test.go）：`contractRoundTrip/Batch/InflightMixed/WaitSemantics/ReadBeyondEOF/SubmitAfterClose/FdSurvivesGC/QueueFull` 全部改为「先 open 拿 fd → `b.new(t, int(f.Fd()), depth)`」，去掉各处 `fd := int(f.Fd())` 与 Submit 的 fd 实参。
- 直接调构造的后端细节用例：`newLibAIORing(...)`（:294/:379/:425/:454/:1197）与 `newIOUringRing(...)`（:493/:506/:519/:578/:616/:704/:760/:1209）补 fd 实参；越界校验用例（:493/:506）可传 0（校验先于 fd 使用）。
- `TestLibAIOContextErrors`（:377-419）：`l.ctx = 0` 的 EINVAL 断言语义不变（fd 已绑定在结构体里）。
- `TestUringSubmitEmptyBatchAndAfterClose`（:577-611）：空批 `SubmitReadBatch(nil)`/`SubmitWriteBatch(nil)`；文件创建提前到建 ring 前，绑定 fd。
- `TestUringEnterFailureKeepsRingUsable`（:615-650）：`u.submit(fd, specs, op)` → `u.submit(specs, op)`；`u.fd = -1` 污染仍指 setup fd，不动。
- `ringWrite`/`ringRead`（aio_test.go:644/:661）去掉 fd 参数。

### doc / spec 引用同步（改后逐条重测行号，规则见 `.trellis/spec/index.md`）
- `internal/aio/aio.go` 自身注释（文件头、Options、Ring 接口）。
- `.trellis/spec/`：`layout/index.md`（:258-268 方法表与行号）、`concurrency/index.md`（:78、:106）、`idiomatic/index.md`（:36、:103、:115-116、:166）、`index.md`（:74-83 引用格式示例）、`error/index.md:62`、`testing/index.md`（:58/:101/:131）——行号位移，逐条重核。
- `.trellis/tasks/09-22-aio-spec-conformance/design.md:135`「签名与语义全部不动」与现状矛盾：加括注说明「后续 fd 下沉重构已改动接口」，不改历史结论文字。
- `doc/设计文档/20260914_io_uring可行性分析与改造方案.md`（:59/:65/:204/:208/:212/:220/:234/:445/:446）、`REFACTOR_DEVICE_BATCH.md`（:46/:74/:164-167）、`DESIGN_IO_PIPELINE.md:208`、`.trae/documents/aio-device-integration.md`（:11/:14）——签名/措辞改写 + 行号重指。
- `project_memory.md` Engineering Conventions：补「aio Ring 构造时绑定设备 fd（Options.FD），Submit*/Batch 不再带 fd」条目。

## 验证

1. `gofmt -l internal/aio internal/device` 为空。
2. darwin：`go build ./...` && `go vet ./internal/aio ./internal/device` && `go test ./internal/aio/ ./internal/device/ -count=1` && `go test ./internal/... -count=1`（全 internal 回归）。
3. 交叉：`GOOS=linux GOARCH=amd64 go build ./...` + `go vet` + 两包 `go test -c`。
4. spec 引用核查：.trellis/spec 里所有 aio/device 引用 token 逐条打印目标行核对；全仓 grep 旧签名 `SubmitRead(fd` 与 `SubmitWrite(fd` 零残留（排除 .git 等）。
5. 用户终端在 4.19（192.168.210.181）/ 5.10（192.168.210.180）跑两后端 × 两通道契约，确认无回归。

## 提交

单一提交：`refactor(aio): 设备 fd 下沉到 ring 构造绑定`（实现 + 测试 + doc/spec 引用同步一并提交，引用不可拆分）。用户未要求 push，不 push。