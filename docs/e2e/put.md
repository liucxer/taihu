# Put —— 数据面 e2e 测试文档

## 语义

```go
func (s *Storage) Put(ctx context.Context, key string, size int64, in []byte) error
```

- 写路由选实例（`local` 本地优先 / `round-robin` 水位跳过）→ 数据面写入 → 异步写索引（key→实例）+ 路由缓存。
- key 只写一次（无覆盖写语义保证；覆盖写见并发场景）；`in` 不足 `size` 字节报 `ErrShortWrite`；无在线实例报 `ErrNoInstances`。
- 内部按 ChunkSize(4MiB) 分帧零拷贝发送。

## e2e 用例

| 用例 | 前置 | 步骤 | 预期 |
|---|---|---|---|
| P1 尺寸矩阵（直连） | 1 实例 | `TestA2SizeMatrix`：4KiB~4MiB+ 各尺寸 `putExact` | Put/Stat/Get 全链路一致 |
| P2 尺寸矩阵（SDK） | 1 实例 + SDK | `TestASDKPath`：同矩阵走 SDK（索引异步收敛） | 内容一致；`ListIndexKeys` 收敛 |
| P3 不可压缩内容 | 1 实例 | `TestA3Incompressible`：randBytes 全尺寸写读 | sha256 + 逐字节一致 |
| P4 覆盖写 | 1 实例 | `TestA5Overwrite`：同 key 二次 Put | 新内容可读、尺寸更新 |
| P5 多块对象 | 1 实例 | `TestA7MultiBlock`：跨 4MiB 块边界尺寸 | 内容一致 |
| P6 混合批量（直连） | 1 实例 | `TestA8MixedBatch`：多尺寸写入+删一半 | 存活集合正确 |
| P7 轮询路由写 | 2 实例 | `c_registry_test.go:176` round-robin Put N key | 写请求在实例间均分 |
| P8 KV 不可达降级 | 无实例 SDK | `c_registry_test.go:415` | Put 报 `ErrNoInstances` 不 panic |
| P9 并发覆盖写 | 1 实例 | `e_concurrency_test.go` 多 writer 同 key | 最终读到任一 writer 的完整内容 |

## 覆盖位置

- `test/e2e/a_data_test.go:278`（TestASDKPath，SDK 全路径）、`a_data_test.go:38`（TestA2SizeMatrix）、`a_data_test.go:55/128/195/216`
- `test/e2e/c_registry_test.go:176,182,371,376`（路由算法下的写）、`c_registry_test.go:436`（ErrNoInstances）
- 并发：`test/e2e/e_concurrency_test.go`；浸泡：`f_soak_test.go:351`
- 单元层：`pkg/taihu-client`、`internal/rpcclient`、`internal/transport/client.go:98`（Put 分帧）

## 运行

```bash
go test -tags e2e ./test/e2e/ -v -run 'TestA2SizeMatrix|TestASDKPath' -count=1
```

## 注意

- Put 成功≠索引已可见：索引异步批量写（批 128 / flush 100ms / 失败丢弃），断言索引需 `waitFor` 收敛（`harness_test.go:672`）。
- SDK 层 GetBatch/GetFdBatch 批量读依赖**路由缓存**命中；Put 后缓存已热，这正是 H 场景批量用例能走批量路径的前提。
