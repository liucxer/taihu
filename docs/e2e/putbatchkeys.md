# PutBatchKeys —— 数据面 e2e 测试文档

## 语义

```go
func (s *Storage) PutBatchKeys(ctx context.Context, keys []string, size int64, datas [][]byte) error
```

- **每 key 不同内容**的批量写（FUSE 数据面用）：每 key 写 `datas[i]` 前 `size` 字节；其余语义与 PutBatch 一致（一次选实例、批量索引+路由缓存、非批量连接逐 key 回退）。
- 参数校验：`len(datas) != len(keys)` → `ErrInvalidRange`；空 keys / `size==0` → no-op `nil`；无实例 → `ErrNoInstances`。
- `datas[i]` 可比 `size` 长——超出部分**不会**写入（对象尺寸 == size）。
- **此前无 e2e 覆盖**，H 场景补齐。

## e2e 用例（TestH2PutBatchKeys，`h_sdk_batch_fd_test.go:80`；Z1 前置复用见 getfdbatch.md）

| 用例 | 前置 | 步骤 | 预期 |
|---|---|---|---|
| K1 各 key 各内容 | 1 实例 + SDK | `PutBatchKeys(4 keys, 1MiB, 4 份不同 randBytes)` | 无错；逐 key `getExact` 等于各自内容 |
| K2 只取前 size 字节 | 同上 | `datas[3]` 比 size 长 16B | `Stat(keys[3])==size`；读回无多余字节 |
| K3 数量不一致 | —— | `PutBatchKeys(4 keys, size, 3 datas)` | `ErrInvalidRange` |
| K4 索引收敛 | K1 之后 | `ListIndexKeys` | 4 key 可见（H5 用其作前置亦验证此点） |

## 覆盖位置

- `test/e2e/h_sdk_batch_fd_test.go:80`（TestH2PutBatchKeys：K1–K3）
- `test/e2e/h_sdk_batch_fd_test.go:214`（TestH5GetFdBatch 以 PutBatchKeys 为写入前置，间接再验 K4）
- 单元层：`internal/transport/transport_shm_test.go:1609`（shm 批量写帧内容校验）

## 运行

```bash
go test -tags e2e ./test/e2e/ -v -run 'TestH2PutBatchKeys|TestH5GetFdBatch' -count=1
```

## 注意

- K2 的「超长 data 截断到 size」是刻词语义（FUSE 页对齐缓冲常大于有效数据）；若未来改为报错需同步改本用例与文档。
- 批内全部 key 落在**同一实例**（一次 pick）；跨实例批量写不存在（也不同需求）。
