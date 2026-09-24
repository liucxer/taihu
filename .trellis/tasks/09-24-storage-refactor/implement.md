# 执行计划（重构范围 A+B+C + 用户三条补充）

## 执行顺序

1. **BatchRead 泄漏回归单测（先红）**：构造「block 0 合法 + block 1 的 Dst 不足」inputs，
   断言执行返回 error 后目标段引用计数归零（复用 metastore 的 segState 式访问或
   through Ref 后回收能力验证）。
2. **修 BatchRead（两遍法）**：第一遍全量校验+计算 devJob/ref 计划（不 Ref），第二遍
   统一 Ref → `ReadAtIntoBatch` 提交 → `defer` 统一 Unref；早退零泄漏。单测转绿。
3. **抽 readWindow 助手**（私有）：`(meta, off, size, alignedOff) → (relStart, dlen, want, skip, err)`，
   统一「越界 ErrInvalidRange / 空窗口 io.EOF / 剩余截断 / 4K 对齐」；ReadAtMeta、
   ReadAtIntoMeta、BatchRead 三处全部改用它，语义逐字等价（现有读路径测试兜底）。
4. **Delete 去冗余**：直接 `s.db.DeleteMapping`（缺失仍返回 ErrNotFound），删外层 GetMapping。
5. **storage.go 拆文件**：`storage.go` 收敛为**导出面 hub**（Storage 类型 + 全部 public 顶层声明
   迁入：BatchReadBlock/BatchedReadResult/Compactor/CompactorConfig/Option/NewStorage/
   NewCompactor/DefaultCompactorConfig/WithAIOMode/WithAIOIOPoll + 生命周期/管理透传）；
   `write.go`（写路径方法）、`read.go`（读路径方法 + readWindow）；compact.go/admin.go/options.go
   只保留私有实现。
6. **导出面降级**：`BatchPut`/`PutItem`/`ReadAtInto` → 私有 `batchPut`/`putItem`/`readAtInto`
   （测试同包继续使用，无外部引用）。
7. **测试合并为 1 个 `storage_test.go`**：合并 storage_test/storage_extra_test/compact_test/
   compact_extra_test/admin_extra_test 五文件；函数名冲突用描述性改名；删除原 5 文件。
8. **断言 + 校验 + 提交**：grep「storage.go 之外无导出顶层声明」；gofmt -l、go vet、
   全量相关包测试；提交 `refactor(storage): ...` 并推送。

## 验证命令

- `go build ./...`
- `go vet ./internal/storage/ ./internal/transport/ ./internal/cluster/ ./cmd/...`
- `go test ./internal/storage/ ./internal/transport/ ./internal/cluster/ ./cmd/taihu/... -count=1`
  （cmd 两个已知环境性失败：plain-file capacity、shm 仅 Linux——与本次改动无关，需排除比对）
- 泄漏回归：`go test ./internal/storage/ -run TestBatchReadRefLeak -count=1 -race`
- 导出面断言：`grep -rnE '^func [A-Z]|^type [A-Z]|^var [A-Z]' internal/storage/*.go | grep -v storage.go | grep -v _test.go`（应空）

## 风险与回滚点

- 读路径重构是主要风险（对齐/skip/io.EOF 边界），现有读路径测试全覆盖 → 重构前确认无缺口；
  若某一用例暴露语义分歧，回退该 commit 并先补测试。
- 导出降级风险：BatchPut/PutItem/ReadAtInto 若出现漏网外部引用，go build 直接报错即发现。
- 测试合并后函数名冲突由 go build 暴露，逐个改名。
- 提交粒度：P1 修复、readWindow、文件拆分、导出降级、测试合并各一 commit，便于 review 与回滚。

## 质量门（task.py start 前）

- prd.md 验收项可测、无阻塞开放问题（用户三条补充已落入 R4/R5/R6）。
- 规划收口，等待用户批准最终总结（已更新）后 `task.py start`。