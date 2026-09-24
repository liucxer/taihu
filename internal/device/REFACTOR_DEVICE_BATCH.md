# device 层重构：统一为批量读写接口

> 状态：设计稿（待评审）
> 范围：`internal/device`（底层裸设备 IO 抽象），涉及 `internal/aio`、`internal/storage` 适配
> 目标：**对外只保留批量读写接口，删除全部单条 op 接口**

## 1. 背景与动机

当前 device 层对外暴露了 4 个操作接口，粒度不一：

| 接口 | 类型 | 说明 |
|------|------|------|
| `Append` | 单条写 | 4K 对齐直写（零拷贝）+ 尾块补零，最多 2 次异步写 |
| `ReadAt` | 单条读 | 读入 bufpool 池化缓冲（所有权协议：调用方必须归还） |
| `ReadAtInto` | 单条读 | DMA 直读调用方 dst（零拷贝） |
| `ReadAtIntoBatch` | 批量读 | 一次 io_submit 多条读 |
| `Delete` | 单条删（占位） | append-only，物理擦除延迟到 segment GC |

真实业务只有两种远端访问形态：**shm（共享内存直读）** 与 **rpc（网络分块读写）**，二者在传输层天然按"一批对象/一批块"聚合（如 `storage.BatchRead`）。单条接口与其"语义 = batch 长度为 1 的特例"重复，且 `ReadAt`/`ReadAtInto` 并存制造了双所有权协议（返回池缓冲 vs 直读入参）——这些都是不必要的复杂度。

重构后 device 对外的操作接口收敛为 **2 个批量读写接口**：

1. `ReadAtIntoBatch`：批量读取
2. `AppendBatch`：批量写入

**不做删除接口**：device 层删除本就是占位（append-only，物理擦除延迟到 segment 级 GC），段的回收与释放完全由 `metastore` 段状态机承担（AliveCount==0 → `SegmentStateReclaiming` → 后台 GC 在 refCount==0 时回收为 `Free` 入池），device 无删除职责，`Delete` 占位接口一并移除、不提供 `DeleteBatch`。

配套原则：
- **单条 = batch(1)**：上层单次访问统一包装成长度为 1 的 batch 调用，不再有"单条专用接口"。
- **所有权归一**：所有读都"直读进调用方提供的 dst"，device 层不再接触 bufpool（消除"返池缓冲须归还"的耦合）。
- **对齐约束不变**：批量读写仍要求 off/size 4K 对齐、dst 首地址 4K 对齐（O_DIRECT 硬性前提，由上层 storage 吸收非对齐窗口）。

## 2. 目标接口

### 2.1 批量读取（保留并扩展 `ReadAtIntoBatch`）

```go
// ReadJob 批读项：读 segmentID 段内 off 处 size 字节到 Buf。
type ReadJob struct {
    SegmentID int64
    Off       int64
    Buf       []byte
    Size      int64
}

// ReadAtIntoBatch 一次 io_submit 批量提交多条段内直读（同一 ring 及其绑定的 fd），
// 全部完成后按 jobs 顺序返回各项读入字节数。批内项不满足直读快路径时单项回退，
// 整批提交失败（队列满/截断）时全量回退逐条。语义与逐条完全等价。
func (d *Device) ReadAtIntoBatch(ctx context.Context, jobs []ReadJob) ([]int64, error)
```

- **已存在**，无需改签名。
- 单条读 = `ReadAtIntoBatch([]ReadJob{...})`，由 `storage` 层包装，占用当前 `ReadAtInto` 的位置。

### 2.2 批量写入（新增 `AppendBatch`）

```go
// AppendJob 批写项：写 size 字节 data 到 segmentID 段内 off（4K 对齐）处。
// data 首地址 4K 对齐 → body 零拷贝直写，仅尾块取 4K 临时缓冲补零；
// 非对齐 → 整体拷入对齐缓冲后写出。
type AppendJob struct {
    SegmentID int64
    Off       int64
    Size      int64
    Data      []byte
}

// AppendBatch 一次 io_submit 尽量批量提交多条段内写；逐项语义与原来的 Append 一致
//（零拷贝直写 + 尾块补零 / 非对齐兜底），整批提交失败时逐条回退。
// 返回 error 语义同原 Append（任一失败即返回首个错误）。
func (d *Device) AppendBatch(ctx context.Context, jobs []AppendJob) error
```

- **aio 层配套**：`Ring` 需新增 `SubmitWriteBatch(specs []WriteSpec) (firstSeq uint64, submitted int, err error)`　（*归档注：fd 已随「fd 下沉」重构移出，绑定在 `Options.FD`*），
  复用现有 `submitBatch`（其已按 `op uint16` 泛化，传 `opcodePwrite` 即可）；`WriteSpec` 与 `ReadSpec` 同构（Buf+Off）。
- 写完成事件校验沿用 `checkWrite`（Res < 0 → -errno；Res != want → short write）。
- 数据一致性保证沿用现状：batch 内各项独立落定；全部完成才返回（对外保持同步语义）。

### 2.3 删除单条接口

| 删除 | 替代 |
|------|------|
| `Append` | `AppendBatch([]AppendJob{...})`（storage.PutAppend 内包装） |
| `ReadAt` | `storage.ReadAt` 内部 = `bufpool.Get` + `ReadAtIntoBatch` + 平移（见 §3） |
| `ReadAtInto` | `ReadAtIntoBatch`（batch(1)） |
| `Delete` | **移除**（占位无用；段回收归 metastore 状态机，不提供 `DeleteBatch`） |

### 2.4 最终公共接口清单（重构后 device 包全部导出接口）

**构造与生命周期**

```go
func NewDevice(ctx context.Context, nvmePath string, segSize int64, opts ...Option) (*Device, error)
func (d *Device) Close() error
```

**数据面（仅批量读写）**

```go
// 批量读：一次 io_submit 多条段内直读，按 jobs 顺序返回各条读入字节数
func (d *Device) ReadAtIntoBatch(ctx context.Context, jobs []ReadJob) ([]int64, error)
// 批量写：一次 io_submit 尽量提交多条段内 append（对齐直写 + 尾块补零 / 非对齐兜底）
func (d *Device) AppendBatch(ctx context.Context, jobs []AppendJob) error
```

**类型**

```go
type ReadJob struct {           // 批读项（现有）
    SegmentID, Off, Size int64
    Buf                  []byte // 直读入参，须 4K 对齐且 cap ≥ Size
}
type AppendJob struct {         // 批写项（新增）
    SegmentID, Off, Size int64
    Data                 []byte // 写源；首地址 4K 对齐走零拷贝直写
}
type Option func(...)           // 变参选项（现有，AIO 后端 / IOPOLL）
```

**辅助 / 配置**

```go
func (d *Device) Stats() (io4M, ioOther, bytes4M, bytesOther int64)   // 4MiB vs 其他 IO 分档统计
func DeviceCapacity(path string) (int64, error)                        // BLKGETSIZE64 ioctl 裸盘容量
func WithAIOMode(m aio.Mode) Option                                    // auto / libaio / io_uring
func WithAIOIOPoll(on bool) Option                                     // IOPOLL 开关
```

**移除（不再导出）**：`Append`、`ReadAt`、`ReadAtInto`、`Delete`，以及既定批量写/删新增项 `DeleteBatch`（不提供）。

## 3. storage 层适配

### 3.1 `storage.ReadAt` 反耦合改造

现在 `storage.ReadAt` 直接调 `dev.ReadAt` 拿池缓冲。改后由 storage 自持缓冲管理：

```go
func (s *Storage) ReadAt(ctx context.Context, key string, off, size int64) ([]byte, error) {
    // meta 查询 / 边界校验 / 4K 对齐区间计算 不变...
    buf := bufpool.Get(int(dlen))                       // device 不再隐含拿池
    ns, err := s.dev.ReadAtIntoBatch(ctx, []device.ReadJob{
        {SegmentID: meta.SegmentID, Off: dstart, Size: dlen, Buf: buf[:dlen]},
    })
    // 短读截断 + skip 平移 + 返回 buf[:n]  → 原逻辑保留
}
```

### 3.2 `storage.ReadAtInto` / `BatchRead` 换皮

- `ReadAtInto`：直接调 `ReadAtIntoBatch(batch(1))`。
- `BatchRead`：已有聚合逻辑，**直接透传 `ReadAtIntoBatch`**（当前实现已经这么走，仅需剔除对单条接口的回退引用）。

### 3.3 `PutAppend` / 分段写 / compact

- `PutAppend`（分段直写）→ `AppendBatch(batch(1))`。
- **选型**：若单 Put 本身就是多次 `PutAppend` 拼接（大对象分段），可顺势在 storage 层聚合各段为
  一次 `AppendBatch`，减少 io_submit 次数——作为可选项，不在本次强制范围内。
- `compact.moveObject` 的 `dev.Append` → `AppendBatch(batch(1))`。

## 4. aio 层改动清单

| 文件 | 改动 |
|------|------|
| `aio/aio.go` | 新增 `WriteSpec`（Buf+Off 同构 ReadSpec）；`Ring` 接口新增 `SubmitWriteBatch`　（*归档注：后续「fd 下沉」重构后 `SubmitWriteBatch(specs)` 不再带 fd 参数*） |
| `aio/aio_libaio_linux.go` | `SubmitWriteBatch` = `submitBatch(specs, opcodePwrite)`（复用泛化实现） |
| `aio/aio_uring_linux.go` | `SubmitWriteBatch` 走 `submit(specs, ioringOpWrite)`（复用已在的 write 提交） |
| `aio/aio_fallback_other.go` | `SubmitWriteBatch` = goroutine + pwrite 逐条兜底 |

## 5. 边界与风险

1. **O_DIRECT 对齐不变**：batch 项沿用逐条的 4K 对齐硬校验；不满足项 **单项回退**（读）或**整体兜底拷贝**（写），不破坏整批。
2. **尾块补零**：`AppendBatch` 每项沿用"对齐主体直写 + 尾块 4K 临时缓冲补零"，临时缓冲只能逐项申请，批量本身不消除该项成本（4K 缓冲复用可后续优化）。
3. **完成泵一致性**：batch 提交/消费沿用 `submitOp` 的 `pending/m` 语义；`inSubmit` 需按 **实际成功排队条数** 自增减（`SubmitReadBatch`/`SubmitWriteBatch` 返回 `submitted`），避免泵提前退出。
4. **所有权归一副作用**：`storage.ReadAt` 从"device 返回池缓冲"改为"storage 自有 bufpool.Get"，**归还协议不变**（storage 对上层仍是返池缓冲），仅 device 层解除 bufpool 依赖。
5. **删除语义归属**：段回收完全由 `metastore` 状态机处理，device 层无删除接口；`storage.Delete`（key 级删映射）继续由 storage 层调用 metastore，不经过 device。
6. **兼容期**：为避免一次大爆炸，可先加 `AppendBatch` 并让旧调用方切到 batch 包装，验证通过后删除单条 `Append`/`ReadAt`/`ReadAtInto`/`Delete`。

## 6. 测试计划

- `device_test.go`：读/写路径由单条用例改为 `batch(1)` 与多批混合用例；覆盖批内单项回退、整批截断回退、队列满重试。
- 新增 `AppendBatch` 用例：对齐主体零拷贝、尾块补零、非对齐兜底、跨多段批量。
- `storage` 层回归：`TestReadAt*`、`BatchRead`、compact、覆盖写（同 key 覆盖计数归零转 Reclaiming）。
- 性能冒烟：Compare 批量 vs 逐条 io_submit 次数（syscall 摊薄收益）、带宽/延迟不回退。

## 7. 结论

- device 对外操作接口收敛为 `ReadAtIntoBatch` / `AppendBatch` 两个批量读写接口，单条访问统一由上层包装为 batch(1)。
- 读侧所有权协议归一为"直读调用方 dst"，device 解除对 bufpool 的依赖。
- 写侧补齐批量形态（依赖 aio `SubmitWriteBatch`）；**不做删除接口**，段回收语义归属 `metastore` 状态机，`Delete` 占位一并移除。