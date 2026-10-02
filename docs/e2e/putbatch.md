# PutBatch —— 数据面 e2e 测试文档

## 语义

```go
func (s *Storage) PutBatch(ctx context.Context, keys []string, size int64, in []byte) error
```

- **同内容批量写**（bench 语义）：一次选实例（本地优先），把 `in` 前 `size` 字节写到每个 key；成功后批量写索引与路由缓存。
- 实例连接不支持完整批量（TCP 仅批量读）时**逐 key Put 回退**（同一实例）——两条路径最终语义一致。
- 空 keys / `size==0` → no-op `nil`；无在线实例 → `ErrNoInstances`；`in` 不足 `size` → `ErrShortWrite`。
- **此前无 e2e 覆盖**，H 场景补齐。

## e2e 用例（TestH1PutBatch，`h_sdk_batch_fd_test.go:47`）

| 用例 | 前置 | 步骤 | 预期 |
|---|---|---|---|
| W1 同内容多 key | 1 实例 + SDK | `PutBatch(4 keys, 1MiB, data)` | 无错；逐 key `getExact` 全部等于 data；`Stat==1MiB` |
| W2 索引收敛 | W1 之后 | `waitFor` → `ListIndexKeys(prefix)` | 4 个 key 全部可见（异步索引） |
| W3 空 keys no-op | —— | `PutBatch(nil, size, data)` | `nil`，不写索引 |
| W4 size=0 no-op | —— | `PutBatch(keys[:1], 0, data)` | `nil` |

## 覆盖位置

- `test/e2e/h_sdk_batch_fd_test.go:47`（TestH1PutBatch：W1–W4）
- 单元层：`internal/rpcclient/storage_rpc.go`（batchConn 完整批量 / 逐 key 回退）、`internal/transport`（shm 批量帧）

## 运行

```bash
go test -tags e2e ./test/e2e/ -v -run 'TestH1PutBatch' -count=1
```

## 注意

- PutBatch 是「一份数据 × N key」，**不是** N 份不同数据（那是 PutBatchKeys，见 [putbatchkeys.md](putbatchkeys.md)）。
- W1 在 e2e 走 shm 完整批量路径（同机）；TCP 节点上回退为逐 key Put——语义等价但性能特征不同，压测结论注意区分（`taihu bench` 的 batch 模式即此 API）。
