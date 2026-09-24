# metastore 层重构：测试收敛 + 层职责梳理 + 导出面收敛

## 需求
1. metastore 全部测试文件合并为 **1 个测试文件**（现 5 个：store/compact/segments/meta/cache_test.go，均平台中立、同包，可安全合并且无符号冲突——它们当前已共存于同一包）。
2. 梳理 metastore 层意义与外部业务使用函数，产出落到 `design.md`，作为后续小幅重构的依据。
3. **导出面收敛（默认私有）**：metastore 包内所有对象与函数**默认未导出**，仅对确有外部调用的符号保持导出。外部调用方清单（以 design.md 的 grep 结论为准）：
   - `internal/storage`：`Store` 接口及其方法（`s.db.*` 十个方法）+ `Open`
   - `internal/rpcclient/reexport.go`：透出的类型面 `ObjectMeta` / `SegmentEntry` / `SegmentSummary` / `SegmentState` 及 5 个状态常量
   - `cmd/taihu/cmd/server.go`：`SegmentStats()` 返回的 `SegmentState` 常量
   - 其余当前导出但无外部调用的符号（如 `PutMappingItem` / `AllocResult` 等）按需降级为私有。
4. **导出集中到 `metastore.go`**：新增 `internal/metastore/metastore.go`，作为**唯一导出文件**
   —— 全部 public 方法/对象/常量/类型（含 `Store` 接口、`Open`、`ObjectMeta`、
   `SegmentMeta`、`SegmentState` 与状态常量、`PutMappingItem`、`AllocResult` 等）集中于此，
   与 aio.go 收敛导出面的先例一致；原文件（store.go/meta.go/segments.go/…）只保留私有实现
   与内部辅助。需求 3 的审计据此落地：metastore.go 以外的任何导出符号即违规。
5. **KV 模型对象化（新增 `model.go`，私有文件）**：把对底层存储（CockroachDB 的 Pebble
   内核）的**全部操作对象化** —— 每个记录类型对应一个结构体（如 mapping / segment /
   cursor 三类），各自封装键编解码（`m00`/`s00`/`seg/` 前缀）与所需方法（`set` / `get` /
   `delete`，以及批量、CAS 等实际用到的变体），呈面向对象形态；
   `kv_pebble.go` 中的「前缀 + 裸 key 拼接 + Set/Get/NewIter」散落写法收敛到 model 层。
   **磁盘键格式保持兼容，不引入格式变更**。

## 验收
- `internal/metastore/` 仅剩 1 个 `*_test.go`（如 metastore_test.go），收纳全部用例
- `design.md` 含：层定位、外部使用函数清单、可优化点
- **导出面：metastore 导出符号 == 外部调用必需集**（以全仓 grep 为准，无多余导出；被降级符号经 storage/rpcclient/taihu-client/cmd 编译回归确认无外部引用）
- `metastore.go` 为唯一导出文件：全仓 grep 断言「metastore.go 之外无导出顶层声明」
- `model.go` 对象化落地：mapping/segment/cursor 等每类一个结构体，方法覆盖实际用到的
  set/get/delete（含批量、CAS 变体）；`kv_pebble.go` 不再直接做前缀拼接 + 裸 Set/Get；
  磁盘键格式不变（旧数据可继续读）
- `go test ./internal/metastore/` 与 storage 相关测试通过
- 涉及导出降级时对外行为不变（`s.db.*` 签名不变；rpcclient reexport 的类型面不变）

## 非目标
- **磁盘键格式/值编码不变**：model.go 只是把操作收拢成对象，不重排 `m00`/`s00`/`seg/` 布局
- 不把事务/原子性（WriteBatch、CAS 提交）下沉进单个 model 结构体；批与 CAS 编排仍由层负责
- 不新增功能、不改 pebble 实例配置