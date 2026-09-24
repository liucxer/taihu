# device 层接入 aio.Ring（libaio）替代同步 WriteAt/ReadAt

## Context

已完成 internal/aio 纯 Go libaio 封装（Linux: io_setup/submit/getevents/destroy；非 Linux: goroutine+pread/pwrite 兜底，接口语义一致）。本计划将其接入 device 层，替换同步 `f.WriteAt`/`f.ReadAt`，使 Linux 上磁盘 IO 真正走 libaio 异步提交。

**约束**：device 对外同步 API（`Append`/`ReadAt`）保持不变，storage 与现有测试无需改动；aio 包约束「Wait 必须串行调用」由单一完成泵 goroutine 满足；buf 在 Submit 后到事件取回前必须存活。

## 设计

Device 内部持有一个 `aio.Ring` + 一个完成泵 goroutine：（*归档注：本文记录接入初期的设计；`aio.New` 已被 `NewWithOptions` 取代，且「fd 下沉」重构后 fd 绑定在 `Options.FD`、`ring.Submit*` 不再带 fd 参数*）

- **泵**（唯一调用 `ring.Wait` 者）：`Wait(1, 64, &200ms)` 轮询取回事件，锁内按 `Event.Data`（seq）查 m/pending 分发到提交方通道，锁外发送（chan cap=1 不阻塞）。用 200ms 超时避免「泵阻塞在 Wait 时 Close 被调用、无在途事件导致永不唤醒」的挂起。
- **提交方**：锁外 `ring.Submit*`（ErrFull 时 100µs 短睡重试、检查 ctx/closed）→ 锁内原子完成「closed 检查 + pending 消费 + 注册 m」，再 `<-ch` 阻塞等事件。`pending` 表解决「io_submit 已返回、泵已取回事件、提交方尚未注册通道」的竞态（事件不丢不重）。
- **Close**：锁内置 `closed=true` → 等泵退出（`closed && len(m)==0`，pending 为瞬态交接不阻塞任何人）→ `ring.Close()` → `f.Close()`。泵遇 Wait 硬错误时 log+continue，不退出。

**缓冲生命周期**：各路径均在等到完成事件后才 `bufpool.Put`（含错误路径 res<0）；Linux O_DIRECT 对齐约束不变（bulk 直写调用方对齐缓冲，尾块与路径 2 用 bufpool 对齐缓冲，偏移/长度 4K）。

## 实施步骤（核心文件 internal/device/device.go）

1. **Device 结构体**新增字段：
   `ring aio.Ring`、`mu sync.Mutex`、`m map[uint64]chan aio.Event`、`pending map[uint64]aio.Event`、`closed bool`、`pumpDone chan struct{}`。

2. **NewDevice**：openDevice 成功后 `aio.New(256)`，失败关 f 返回错误；启动 `go d.pump()`。

3. **新增 `pump()`**：
   - 循环 `evs, err := d.ring.Wait(1, 64, &200ms)`（ErrTimeout 照常处理 evs）；
   - 锁内查 m 分发 / 未命中入 pending，锁外 `ch <- ev`；
   - 退出条件 `closed && len(m)==0` 时 `close(d.pumpDone)` 返回。

4. **新增提交辅助** `submitWrite(buf []byte, off int64) (aio.Event, error)` 与 `submitRead(buf []byte, off int64) (aio.Event, error)`：
   - 锁外 submit；ErrFull → 短睡重试（检查 ctx/closed）；
   - 锁内：closed → 删除自身 pending 项、返回错误；pending 命中 → 删除返回；否则注册 m、解锁、`<-ch`。

5. **Append 改造**：保留全部校验（offset 4K 对齐、越界、len(data)<size、size==0 短路）。路径 1 主体 `submitWrite(data[:bulkEnd])`（调用方持有），尾块 tmp=bufpool.Get(4K) submit 写、完成后 Put；路径 2 整体拷入 bufpool.Get(aligned)、完成后 Put。res<0 时按 -errno 返回错误。

6. **ReadAt 改造**：`buf := bufpool.Get(size)`，`submitRead(buf[:size])`，按 res 映射（与现状一致）：
   - res==0 → Put + io.EOF；res<0 → Put + errno；
   - res<size → 返回 `buf[:res]` + io.EOF；否则返回 `buf[:size]`。

7. **Close 改造**：锁内置 closed → `<-pumpDone` → `ring.Close()` → `f.Close()`。

8. **测试**（internal/device/device_test.go 追加）：
   - `TestDeviceConcurrent`：多 goroutine 异偏移并发 Append/ReadAt，`-race` 下验证正确性；
   - `TestDeviceCloseInflight`：有在途请求时 Close 正常排空不挂起。

## 验证

- `go build ./... && go vet ./internal/device/ ./internal/aio/`
- `go test -race ./internal/device/ ./internal/aio/ -count=1`（macOS 走兜底 ring，语义一致）
- `GOOS=linux GOARCH=amd64 go build ./internal/device/` 交叉编译
- `go test -race ./pkg/taihu/ -count=1` 确认 storage 调用链不受影响

## 不改动的文件

- internal/aio/*（接口与实现已定稿，device 只依赖其公开 API）
- internal/device/device_linux.go / device_other.go（openDevice 不变，aio 跨平台）
- pkg/taihu/storage.go（对外 API 未变）
