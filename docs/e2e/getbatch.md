# GetBatch —— 数据面 e2e 测试文档

## 语义

```go
func (s *Storage) GetBatch(ctx context.Context, keys []string, off, size int64) ([][]byte, func(), error)
```

- 批量读多个对象同一 `[off, off+size)` 子区间；`out[i]` 对应 `keys[i]`；用毕调用一次 `release()`（幂等）归还全部缓冲。
- **SDK 层约束**（`pkg/taihu-client/storage.go:234`）：`off<0 || size<0` → `ErrInvalidRange`（不接受 `size=-1`，整读需传确切 size 或逐 key Get）；空 keys / `size==0` → no-op `(nil, func(){}, nil)`。
- 分组语义：路由缓存命中的 key 按实例分组走底层批量（TCP/shm 均实现 `batchGetter`），组失败逐 key Get 回退；缓存 miss 的 key 逐 key 原 Get（含索引定位 + 回源）并预热缓存。
- 任一 key 出错：已取缓冲全部释放后整体报错。
- **此前无 e2e 覆盖**，H 场景补齐。

## e2e 用例（TestH3GetBatch，`h_sdk_batch_fd_test.go:112`）

| 用例 | 前置 | 步骤 | 预期 |
|---|---|---|---|
| B1 全量批量读 | 1 实例 + SDK；4 个 128KiB pattern 对象 | `GetBatch(keys, 0, size)` | `out[i]` 与 `wants[i]` 逐字节一致；release 后连接可复用 |
| B2 区间批量读 | 同上 | `GetBatch(keys[:2], 4096, 8192)` | 每个 out 与 pattern 对应子区间一致 |
| B3 size<0 拒绝 | 同上 | `GetBatch(keys, 0, -1)` | `errors.Is(err, ErrInvalidRange)` |
| B4 缺失 key | 同上 | keys 含不存在 key | `ErrNotFound`（整批失败） |
| B5 空 keys no-op | 同上 | `GetBatch(nil, 0, size)` | `(nil, non-nil rel, nil)`，无错误 |

## 覆盖位置

- `test/e2e/h_sdk_batch_fd_test.go:112`（TestH3GetBatch：B1–B5）
- 单元层：`internal/rpcclient/storage_rpc.go:85-148`（batchGetter 分发与逐 key 回退）、`internal/transport` 单流多请求 pipeline

## 运行

```bash
go test -tags e2e ./test/e2e/ -v -run 'TestH3GetBatch' -count=1
```

## 注意

- B1 能走「批量路径」依赖 Put 已预热路由缓存（PutBatchKeys/Put 写后缓存必热）；冷缓存场景由 miss 分支逐 key Get 兜底，二者最终一致。
- 与 Get 不同，SDK 批量读不接受 `size=-1`——这是文档化约束而非 bug（`storage.go:238`）。
