# Get —— 数据面 e2e 测试文档

## 语义

```go
func (s *Storage) Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error)
```

- 读 `[off, off+size)` 子区间；`size=-1` 读至结尾；返回 `(data, release, err)`，**用毕必须 release()**（幂等，归还池化缓冲）。
- 定位顺序：路由缓存 → TiKV 索引 → `Source` 回源重建（回写缓存）；全部 miss 报 `ErrNotFound`；`off+size` 越界报 `ErrInvalidRange`。
- 底层：`internal/transport/client.go:138`（TCP 多帧汇入）。

## e2e 用例

| 用例 | 前置 | 步骤 | 预期 |
|---|---|---|---|
| G1 区间读矩阵（直连） | 1 实例 | `TestA4RangeRead`：[0,1)、尾部 1B、跨块 [4096,8192)、`size=-1` 全读、越界 | 每子区间与期望逐字节一致 |
| G2 区间读（SDK） | 1 实例 + SDK | `TestASDKPath`：SDK 路径同矩阵（`a_data_test.go:353,370`） | 一致；索引定位生效 |
| G3 回源重建 | SDK + `Source` | `TestASDKSourceRebuild`（`a_data_test.go:338`）：索引清空后 Get | 从 Source 拉回并回写 |
| G4 miss 错误 | 1 实例 | `TestASDKPath`：删后 Get（`a_data_test.go:312`） | `errors.Is(err, ErrNotFound)` |
| G5 范围越界 | 1 实例 | `TestA4RangeRead` 越界分支 | `ErrInvalidRange` |
| G6 并发读 | 1 实例 | `e_concurrency_test.go` 多 reader 同 key | 内容始终完整一致 |
| G7 浸泡区间读 | 1 实例 | `f_soak_test.go:486` 随机区间循环读 | 无错、内容一致 |

## 覆盖位置

- `test/e2e/a_data_test.go:69`（TestA4RangeRead，含 size=-1 与越界）、`a_data_test.go:338`（TestASDKSourceRebuild）
- `test/e2e/e_concurrency_test.go:116,241,342`、`f_soak_test.go:385,468,486`、`g_soak_long_test.go:351,370,419`
- 单元层：`internal/rpcclient/storage_rpc.go:66`（Get 透传）、`internal/transport/client.go:138`（-1/范围/多帧）

## 运行

```bash
go test -tags e2e ./test/e2e/ -v -run 'TestA4RangeRead|TestASDKSourceRebuild' -count=1
```

## 注意

- release() 前不得复用 data 引用（单帧零拷贝路径 data 直接引用接收缓冲）。
- 范围读不校验 off<size>对象尺寸的组合细节见 `ErrInvalidRange` 语义（`pkg/ierr`）。
