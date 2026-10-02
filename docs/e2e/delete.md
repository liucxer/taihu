# Delete —— 数据面 e2e 测试文档

## 语义

```go
func (s *Storage) Delete(ctx context.Context, key string) error
```

- 删除对象 + **同步尽力删索引**（保证删除语义，区别于 Put 的异步索引）；清路由缓存。
- 定位：路由缓存 → TiKV 索引；索引 miss 时对本地实例兜底删除（首写本地模型下对象必在本地或已删）。
- 对象不存在：直连路径 `ErrNotFound`（`a_data_test.go:182`）；SDK 路径索引 miss + 本地兜底也 miss → `ErrNotFound`。
- 段内垃圾由 Compactor 回收（`internal/storage`），Delete 本身只打删除标记（见 d_lifecycle 段回收场景）。

## e2e 用例

| 用例 | 前置 | 步骤 | 预期 |
|---|---|---|---|
| D1 直连删除 | 1 实例 | `TestA6Delete`（`a_data_test.go:153`）：Put → Delete → Get/Stat/Delete | 后三者均 `ErrNotFound` |
| D2 SDK 删除+索引收敛 | 1 实例 + SDK | `TestASDKPath`（`a_data_test.go:309-323`）：Delete → Get/Stat → `ListIndexKeys` | Get/Stat `ErrNotFound`；索引枚举收敛剔除该 key |
| D3 删一半存活校验 | 1 实例 | `TestA8MixedBatch`（`a_data_test.go:238`）：批量删一半 | 存活 half 读回正常、删除 half `ErrNotFound` |
| D4 并发读写删 | 1 实例 | `e_concurrency_test.go:365` writer Delete + reader Get | Get 要么完整内容要么 `ErrNotFound`，无撕裂 |
| D5 重启后删除可见 | 1 实例重启 | `d_lifecycle_test.go:485`：Delete → 重启 → Get | 仍 `ErrNotFound`（删除持久） |
| D6 churn 浸泡 | 1 实例 | `g_soak_long_test.go:446` 循环删写 | 删 `ErrNotFound` 可容忍，写后可读 |

## 覆盖位置

- `test/e2e/a_data_test.go:153,238,309`、`d_lifecycle_test.go:485`、`e_concurrency_test.go:365`、`g_soak_long_test.go:446`
- 单元层：`internal/rpcclient`、`pkg/taihu-client/storage.go:577`（Delete：索引同步删 + 本地兜底）

## 运行

```bash
go test -tags e2e ./test/e2e/ -v -run 'TestA6Delete|TestASDKPath|TestA8MixedBatch' -count=1
```

## 注意

- 「同步删索引」指 Delete 调用返回时索引已删（尽力）；Get 若恰在删除与索引落定间并发，可能仍读到旧值（最终一致窗口），D4/D6 断言已按此放宽。
- 删除后的空间释放（段回收）属 Compactor，覆盖在 d_lifecycle 场景，不在本 API 文档展开。
