# 重构 internal/storage：梳理现状并落地重构

## Goal

作为 golang 专家先梳理 internal/storage 的职责、耦合与可优化点，产出重构建议（design.md），
再按建议小步重构。目标：职责清晰、热点路径可读性提升、修复已发现的引用泄漏 bug，行为不变。

## Confirmed Facts（代码勘察结论）

- **层次**：storage 是对象存储核心，编排 `metastore.Store`（元数据）+ `device.Device`（裸盘 O_DIRECT）+ `layout`；消费方仅 cmd/taihu/cmd、internal/transport、internal/cluster。
- **文件**（非测试 5 个，共 ~947 行；测试 5 个，共 ~1302 行）：
  - `storage.go`(484)：Storage 类型 + 生命周期 + 写路径 + 读路径 + 批量 + 管理透传，职责最杂；
  - `options.go`(31)：functional options（aio 后端选择），模式良好不变；
  - `compact.go`(206)：后台 Compactor（60s 扫描、80% 空洞/水位阈值、每轮 ≤512 对象）；
  - `admin.go`(74)：Segments/ListKeys/ObjectMeta/Ping。
- **测试文件命名不一致**：`storage_test.go` + `storage_extra_test.go`、`compact_test.go` + `compact_extra_test.go`、`admin_extra_test.go`（"extra" 后缀是历史遗留，非平台拆分）。
- **公开 API 面**：写 `Put/PutBegin/PutAppend/PutCommit/BatchPut/BatchAppend/BatchPutCommit/BatchDelete`；读 `Meta/ReadAt/ReadAtMeta/ReadAtInto/ReadAtIntoMeta/BatchRead`；管理/统计透传 + `Close/LoadCache/MaxObjectSize/IOStats/GetDiskCapacity/SegmentStats/RefSegment/UnrefSegment`。
- **发现 bug（勘察阶段）**：`BatchRead` Phase 1 校验失败早退时，已 RefSegment 的段未 Unref——引用泄漏 → 段永不可回收（空间泄漏）；Phase 2 错误路径有清理，Phase 1 早期 return 没有（storage.go L382-397）。
- **冗余**：`Storage.Delete` 先 `GetMapping` 再 `DeleteMapping`，而 metastore.DeleteMapping 内部已做 GetMapping（缺失返回 ErrNotFound）——外层预检冗余。
- **重复实现**：`ReadAtMeta`/`ReadAtIntoMeta`/`BatchRead` 各自重复「校验 off/size → 截断 want → 计算 4K 对齐物理区间 dstart/dlen/skip → Ref/Unref」逻辑。

## Requirements

- R1: 修复 BatchRead 引用泄漏（Phase 1 早退路径补 Unref；推荐改为「先全量校验构建 job、再统一 Ref + 提交通信」两遍法）。
- R2: 读路径抽取公共窗口助手（readWindow），消除 ReadAtMeta/ReadAtIntoMeta/BatchRead 三份重复，行为不变（含 io.EOF 语义）。
- R3: `Storage.Delete` 去掉冗余 GetMapping 预检（行为不变，缺失仍返回 ErrNotFound）。
- R4: storage.go 按职责拆文件（storage.go 仅含导出面 + 生命周期；写路径/读路径分别落 write.go/read.go），降低单文件认知负担。
- R5: **全部测试合并为 1 个 `storage_test.go`**（现 5 个：storage_test/storage_extra_test/compact_test/compact_extra_test/admin_extra_test，metastore 同层合并先例）。
- R6: **导出面收敛（默认私有）**：只有外部调用（transport / cmd 生产代码）用到的方法与对象保持 public，其余默认降级 private；**全部 public 顶层声明集中到 `storage.go`** —— 非 storage.go 出现导出顶层声明即违规。
  - 审计结论（生产代码 grep）：`BatchPut` / `PutItem` / `ReadAtInto` 无外部调用 → 降级私有；
    其余（Put/PutBegin/PutAppend/PutCommit/BatchAppend/BatchPutCommit/BatchDelete/BatchRead/Delete/Stat/Meta/ReadAt/ReadAtMeta/ReadAtIntoMeta/MaxObjectSize/LoadCache/Close/RefSegment/UnrefSegment/Segments/ListKeys/ObjectMeta/Ping/IOStats/GetDiskCapacity/SegmentStats + Storage/BatchReadBlock/BatchedReadResult/Compactor/CompactorConfig/Option/NewStorage/NewCompactor/DefaultCompactorConfig/WithAIOMode/WithAIOIOPoll）保持 public。

## Acceptance Criteria

- [ ] `internal/storage/` 仅剩 1 个 `*_test.go`（storage_test.go），收纳全部用例
- [ ] **导出面断言**：全仓 grep「storage.go 之外无导出顶层声明」通过；被降级符号（batchPut/putItem/readAtInto）经 transport/cmd 编译回归确认无外部引用
- [ ] BatchRead 泄漏修复有回归单测（构造某块 dst 不足 early-return，断言已 Ref 段数 0）
- [ ] 读路径重构后 ReadAt/ReadAtInto/BatchRead 逐块语义等价（现有测试全覆盖验证）
- [ ] `go test ./internal/storage/ ./internal/transport/ ./internal/cluster/ ./cmd/taihu/... -count=1` 全绿（cmd 两个已知环境性失败排除比对）
- [ ] `gofmt -l` 空、`go vet` 无新增告警
- [ ] 对外 API 签名不变（Storage 方法签名不变；transport/cluster/cmd 生产调用点零改动）

## Out of Scope

- 公开 API 签名改动（IOStats/GetDiskCapacity 返回对象化、Layout 等）——已评审排除（档位 D），牵连 cluster/kv.go 与 cmd 多个调用点
- 缓存/allocator/Ref 计数上移 storage（上轮 metastore 分层评审已定「保持现状」）
- device/metastore 内部改动

## Key Decisions

- **重构范围 = A+B+C + 用户三条 PRD 补充（2026-09-24）**：
  1. 全部测试合并为 1 个文件（R5，metastore 先例）；
  2. 导出面默认私有、仅外部调用者 public，`BatchPut`/`PutItem`/`ReadAtInto` 降级（R6）；
  3. 全部 public 顶层声明集中到 storage.go，非 storage.go 无导出（R6）。
- 档位 D（公开 API 返回对象化）明确不做。