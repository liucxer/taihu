# 迁移 bufpool：internal/bufpool → pkg/bufpool

## 目标

把 4K 对齐分桶缓冲池（含精确尺寸池 GetExact/PutExact）从 `internal/bufpool` 迁至
对外目录 `pkg/bufpool`，供 SDK/外部复用同一缓冲约定（对齐 + bufpool.Put 归还契约）。

## 范围

- `git mv internal/bufpool pkg/bufpool`
- 全仓 import：`taihu/internal/bufpool` → `taihu/pkg/bufpool`
- 包文档注释同步（对齐 ierr 迁移先例）

## 验收

- `go build ./...` 通过；darwin/linux vet 通过
- bufpool 及相关（device/storage/transport/rpcclient/taihu-client）单测通过
- 无 `internal/bufpool` 残留引用（历史文档除外）

## 说明

迁移为机械操作，等同于 09-24-error-layout-converge 中的 ierr 迁移先例；不影响 API 形状。