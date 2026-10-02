# Stat —— 数据面 e2e 测试文档

## 语义

```go
func (s *Storage) Get(ctx) // 对照：Stat 返回对象字节数
func (s *Storage) Stat(ctx context.Context, key string) (int64, error)
```

- 返回对象**逻辑尺寸**（Put 时声明的 `size`，非磁盘占用）；不存在 → `ErrNotFound`。
- 是 Get 的 `size=-1` 的前置事实源（Get 内部先 Stat 再按需读）；区间读的合法性判断依赖它。
- SDK 路径定位：路由缓存 → TiKV 索引 → 本地兜底；miss 一律 `ErrNotFound`。

## e2e 用例

| 用例 | 前置 | 步骤 | 预期 |
|---|---|---|---|
| S1 尺寸矩阵 | 1 实例 | `TestA2SizeMatrix` / `TestASDKPath`：各尺寸 Put 后 Stat | Stat == 写入 size（4KiB~4MiB+ 全矩阵，直连与 SDK 双路径） |
| S2 超长 data 截断 | 1 实例 + SDK | `TestH2PutBatchKeys` K2（`h_sdk_batch_fd_test.go:105`） | `Stat == size`（非 len(datas[i])） |
| S3 删除后 | 1 实例 | `TestA6Delete`（`a_data_test.go:175`）/ `TestASDKPath:318` | `ErrNotFound` |
| S4 Get 一致性 | 1 实例 | `getExact` helper（`harness_test.go:614`）内嵌断言 | 每次 Get(0,-1) 前先 Stat，两值必须一致 |
| S5 重启持久 | 1 实例重启 | `d_lifecycle_test.go:437`：重启后逐 key Stat | 尺寸与重启前一致 |
| S6 并发覆盖写 | 1 实例 | `e_concurrency_test.go:439-445`：writer 覆盖写 + reader Stat+Get | Stat 与 Get 实际读到的是同一代内容（读到旧 Stat 值时 Get 按显式 size 限幅） |

## 覆盖位置

- `test/e2e/a_data_test.go:38,278`（S1）、`a_data_test.go:175,318`（S3）、`d_lifecycle_test.go:437`（S5）、`e_concurrency_test.go:439`（S6）、`h_sdk_batch_fd_test.go:105`（S2）
- helper：`harness_test.go:614`（getExact 把 Stat==len(want) 作为每个读回校验的隐含断言——**全套 e2e 每个读路径都在持续回归 Stat**）
- 单元层：`internal/rpcclient`、`internal/transport`（OpStatReq/Resp）

## 运行

```bash
go test -tags e2e ./test/e2e/ -v -run 'TestA2SizeMatrix|TestASDKPath|TestH2PutBatchKeys' -count=1
```

## 注意

- S6 揭示的并发语义：Stat 与后续 Get 非原子——覆盖写竞态下 Stat 值可能过期，调用方用「显式 size」路径时须自行容忍（或接受 `ErrInvalidRange`）。
- Stat 返回的是逻辑 size；需要磁盘占用量请走 `UsageGet`（集群水位，`h_sdk_batch_fd_test.go:269` TestH6Ops）。
