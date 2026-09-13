# 整盘容量动态化：启动读取 nvme 容量 + TiKV 记录比较

## Context

当前工程将整盘容量硬编码为 16TB：`internal/layout/layout.go` 的 `SegmentSizeBytes=8GB` × `SegmentCount=2048` 隐含推导，且**没有任何裸设备容量查询**（`GetDiskCapacity` 实际是 statfs 查 pebble 元数据目录所在文件系统，与 nvme 裸盘无关）。

本次改造（**不做独立初始化步骤**）：
1. **每次启动读取一次 nvme 真实容量**，据此计算 segment 布局（不再写死 2048 段）。
2. **TiKV 记录与比较**：按实例唯一名 `-name`（必填，如 `TAIHU-0`）在 TiKV 记录容量；后续启动再次读取 nvme 容量并与 TiKV 记录比较，**不一致则报错拒绝启动**（防止容量变化导致布局错乱）。
3. `-tikv-pd` 必填（容量记录/比较是硬依赖）；`-addr` 必填（严格，不隐式复用任何记录值）。

已确认决策：容量不一致报错拒绝启动；name 作为实例唯一标识（命令行传入，从 `TAIHU-0` 命名）；无 TiKV 报错；不考虑换盘检测（无序列号逻辑）。

## 设计

### 1. 设备容量查询（internal/device 新增）

- `info_linux.go`（新增）：
  ```go
  // DeviceCapacity 查询裸设备容量（字节）。
  func DeviceCapacity(path string) (int64, error)
  ```
  实现：`os.OpenFile(path, os.O_RDONLY, 0)` → `syscall.Syscall(SYS_IOCTL, fd, BLKGETSIZE64, &sz uint64)`。
- `info_other.go`（新增）：非 Linux 开发机用 `os.Stat(path).Size()`（保证 macOS 普通文件模拟设备可跑通）。

### 2. 布局动态化（internal/layout + 运行时注入）

- `internal/layout/layout.go`：
  - 删除常量 `SegmentSizeBytes`/`SegmentCount`；保留 `BlockSize`/`Align4k`。
  - 新增：
    ```go
    var DefaultSegmentSizeBytes int64 = 8 * 1024 * 1024 * 1024 // 默认 8GB/段（-seg-size 可覆盖）

    type Layout struct {
        SegmentSizeBytes int64
        SegmentCount     int64
    }
    // ComputeLayout 按设备容量与段大小计算布局：SegmentCount = capacity / SegmentSizeBytes（向下取整）。
    func ComputeLayout(capacity, segmentSizeBytes int64) Layout
    ```
- `internal/device/device.go`：Device 增加 `segSize int64` 字段；`NewDevice(ctx, path, segSize)`（签名变更，调用点：`pkg/taihu/storage.go:30`、`internal/device/device_test.go` 4 处）；`segmentBase`（L113）及 Append 越界检查改用 `d.segSize`。
- `internal/metastore/kv_pebble.go`：allocator 增加 `segSize/segCount` 字段；`Open(dir string, l layout.Layout)` 注入 allocator（签名变更）；`AllocateSegment` 的 L247（段尾判断）和 L252（滚动判断）改用字段。
- `pkg/taihu/layout.go`：删除两个常量 re-export（调用方改为经 storage 的 `MaxObjectSize()`）。

### 3. TiKV 容量记录（internal/cluster 新增）

- `internal/cluster/capacity.go`（新增）：
  - key 前缀 `CapacityKeyPrefix = "/taihu/capacity/"`，`CapacityKey(name) = prefix + name`。
  - 记录内容（JSON）：
    ```go
    type CapacityRecord struct {
        CapacityBytes    int64  // 本次读取的 nvme 容量
        SegmentSizeBytes int64  // 布局段大小
        SegmentCount     int64  // 布局段数
        ListenAddr       string // 实例监听地址（审计/查询用）
        UpdateTime       int64  // unix 秒
    }
    ```
  - 函数：`GetCapacity(kv KV, name string) (CapacityRecord, bool, error)`、`PutCapacity(kv KV, name string, c CapacityRecord) error`（复用现有 KV 接口）。

### 4. taihu 包 API（pkg/taihu/storage.go）

```go
func NewStorage(ctx, dbDir, devPath string, l layout.Layout) (*Storage, error)
// 流程：metastore.Open(dbDir, l) → device.NewDevice(ctx, devPath, l.SegmentSizeBytes)

func (s *Storage) MaxObjectSize() int64        // layout.SegmentSizeBytes
func (s *Storage) Layout() layout.Layout       // 供心跳/审计
```
- `Put` 大小上限（原 L71 `layout.SegmentSizeBytes`）改用 `s.MaxObjectSize()`。
- `GetDiskCapacity` 语义修正：`Capacity = l.CapacityBytes`（来自 Config/记录值）、`Available = Capacity - Used`、`Used = db.UsedBytes()`（替代 statfs；`diskcapacity_linux.go/_other.go` 删除）。
  - `UsedBytes()`：`internal/metastore/segments.go` 的 segmentManager 遍历 `segs`：Full 段计 `segSize`、Active 段计 allocator 当前 `curOff`（读锁内取快照；段数 ≤2048，心跳 1s 一次开销可忽略）。
- **注意**：Storage 不再自行读容量/连 TiKV；读容量、TiKV 比较、布局计算编排在 `cmd/taihu server/main.go`。

### 5. 入口（cmd/taihu server/main.go）

启动编排顺序：
1. flag 校验：`-name`、`-tikv-pd`、`-db`、`-dev`、`-addr` 全部必填（`-addr` 必须具体 ip:port，拒绝通配）；`-seg-size` 可选（0=默认 8GB）。
2. `kv := cluster.NewTiKVKV(ctx, pdAddrs)`。
3. `capacity := device.DeviceCapacity(*dev)`。
4. TiKV 比较：
   - `GetCapacity(kv, *name)` 无记录 → `PutCapacity` 写入 `{capacity, segSize, segCount, *addr, now}`；
   - 有记录且 `CapacityBytes != capacity` → `log.Fatalf` 报错退出（提示实例容量变化）。
5. `l := layout.ComputeLayout(capacity, segSize)`。
6. `storage := taihu.NewStorage(ctx, *db, *dev, l)`。
7. 后续：集群注册（现有 `-name/-node/-reg-addr` 齐备才注册的逻辑不变）、`net.Listen(*addr)`、rpcserver/pprof/shmipc 逻辑不变。

### 6. 测试适配

- `internal/metastore/segments_test.go:176,453,454`：allocator 字面量构造补 `segSize/segCount` 字段；`Open` 调用点传 Layout。
- `internal/device/device_test.go`：4 处 `NewDevice` 传 segSize。
- 新增单测：`layout.ComputeLayout`（整除/非整除/容量为 0）；`cluster` 容量记录写读往返（MemoryKV）；`UsedBytes` 统计（构造 Full/Active 段状态断言）。

## 实施顺序

1. `internal/layout/layout.go`（删常量 + Layout/ComputeLayout）+ `pkg/taihu/layout.go`（删 re-export）
2. `internal/device/info_linux.go` + `info_other.go`（新增）+ `device.go`（NewDevice 签名/segmentBase）+ `device_test.go`
3. `internal/metastore/kv_pebble.go`（allocator 字段/Open 签名）+ `segments.go`（UsedBytes）+ `segments_test.go`
4. `internal/cluster/capacity.go`（新增：key/记录/Get/Put）+ 单测
5. `pkg/taihu/storage.go`（NewStorage 签名/MaxObjectSize/Layout/GetDiskCapacity）+ 删 `diskcapacity_*.go`
6. `internal/transport/server.go:145`、`shmipc_server.go:215` 上限改 `storage.MaxObjectSize()`
7. `cmd/taihu server/main.go`（编排读容量/TiKV 比较/布局注入）
8. `go test ./...` 全量

## 验证

- 开发机端到端（需本地起一个 TiKV 或先用 MemoryKV 通路验证逻辑，`-tikv-pd` 指向可用 PD）：
  1. `go build ./cmd/taihu server`；
  2. 首次启动 `./taihu server -name TAIHU-0 -tikv-pd <pd> -db /tmp/meta -dev /tmp/dev.img -addr 127.0.0.1:50051` → 成功，TiKV 写入容量记录；
  3. 再次启动（同参数）→ 读容量与记录一致 → 正常启动；
  4. 换 `dev.img`（不同大小）再启动 → 报"容量不一致"拒绝启动；
  5. 缺 `-name`/`-tikv-pd`/`-addr` 之一 → 启动前报错退出。
- `go test ./...` 全量通过（含新增单测）。
