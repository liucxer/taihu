# metastore 层梳理（层意义 + 外部使用面）

## 层定位

metastore = **对象/段元数据的唯一持久层**，backend 为纯 Go Pebble LSM（键前缀隔离：
`m00<key>` mapping、`s00<inner>` 段与状态），storage/ 不直接持有元数据状态。

职责：
- **对象映射**：key → `ObjectMeta{SegmentID, Offset, Size}`（Put 写、Get/Meta 读、Iter 遍历）
- **写游标分配**：`AllocateSegment` 在 Put 路径原子申请段内写位置（storage 不再自持游标）
- **段状态机 + 存活计数**：Free/Active/Full/Reclaiming/Compacting + Ref/Unref 计数
  （读引用防 GC 回收段；80% 阈值驱动 compaction 选择）
- **compaction 事务边界**：`MoveMapping` 用 WriteBatch + CAS 实现跨段搬移原子性

## 对外 API 形状

对外导出仅有 `metastore.Open(dir, layout)` → `Store` 接口 + 类型/常量。
**唯一业务调用方 = internal/storage**（构造于 storage.NewStorage）；经
`internal/rpcclient/reexport.go` 与 `storage.admin.go` 再透出给 SDK/运维面。

## 外部业务使用的函数（按调用方）

**storage/（主）**——`s.db.*` 实际调用经 grep 确认：
| 函数 | 用途 | 调用点 |
|---|---|---|
| `LoadCache` | 启动预热缓存 | storage.go:62 |
| `GetMapping` / `PutMapping` | 对象映射读写（Put/ReadAt/Meta/SegmentStats 计数） | storage.go、admin.go:68 |
| `AllocateSegment` | Put 申请写位置（游标下沉，见 storage.go:17 注释） | Put 路径 |
| `Cursor` | 管理面读游标 | admin.go:25 |
| `ListSegments` | 管理面段清单（Segments） | admin.go:27 |
| `IterMapping` | 对象遍历/统计/compaction 全量扫描 | admin.go:45/57、compact.go:89 |
| `MoveMapping` | compaction 段间搬移（WriteBatch+CAS） | compact.go:202 |
| `RefSegment` / `UnrefSegment` | 读期间段存活计数 | admin_extra_test、storage.ReadAt |
| `LoadCache` | (见上) | — |

**透出面（只读类型与统计，非函数调用）**：
- `rpcclient/reexport.go`：`ObjectMeta`、`SegmentEntry`、`SegmentSummary`、`SegmentState` + 5 个状态常量（SDK 类型面）
- `cmd/taihu/cmd/server.go:247-250`：`SegmentStats()[SegmentState*]` 读盘状态统计

## 观察（后续小步重构候选）
1. 测试 5 文件同包共存 → 合并为 metastore_test.go（本次执行）。
2. `PutMapping`/`AllocateSegment` 未在本次 grep 的调用明细中直接展开（Put 路径内部使用），
   梳理结论：storage 是唯一调用方，无外部业务函数直接触达 metastore——"外部使用函数"的
   准确答案是：不是业务直连，而是 storage 内经 `s.db.*` 调用上述 10 个方法。