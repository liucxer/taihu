# GetFdBatch —— 数据面 e2e 测试文档

## 语义

```go
func (s *Storage) GetFdBatch(ctx context.Context, keys []string, off, size int64) ([]*FdBuf, func(), error)
```

- 批量读并**逐 key 交付共享内存 fd 零拷贝源**（FUSE 批量 splice）；定位与分组语义同 GetBatch（路由缓存命中分组 → 底层批量；miss/非批量连接逐 key `GetFd` 回退）。
- 返回 `out[i]` 对应 `keys[i]`；`FdBuf`（`internal/transport/client.go:27`）导出字段 `Fd int / Foff uint64 / Data []byte`：
  - `Fd>0`：Data 零拷贝引用共享内存，(Fd,Foff) 为 splice 源；
  - `Fd==0`：Data 为池化拷贝缓冲（TCP 回退/逐 key 回退路径）。
- 释放：批内各 FdBuf **独立引用计数，逐个 `Release()`**（幂等）；返回的整批 `release` 为兜底（幂等，调用后不得再使用/释放批内 FdBuf）。
- SDK 层约束同 GetBatch：`off<0 || size<0` → `ErrInvalidRange`；空 keys/`size==0` no-op；任一 key 出错整批报错。
- **此前无 e2e 覆盖**（仅 `internal/transport/transport_shm_test.go:1609-1825` 单元层），H 场景补齐。

## e2e 用例（TestH5GetFdBatch，`h_sdk_batch_fd_test.go:214`）

| 用例 | 前置 | 步骤 | 预期 |
|---|---|---|---|
| Z1 批量零拷贝交付 | 1 实例 + SDK；`PutBatchKeys` 4 个 256KiB pattern 对象 | `GetFdBatch(keys, 0, size)` | 4 个 buf；每个 `Fd>0`；`Data` 与写入一致；**`Pread(Fd, Foff)` 读出与对象一致** |
| Z2 释放语义 | Z1 之后 | 逐 buf `Release()` ×2（幂等）→ 整批 `release()` ×2 | 不 panic；无泄漏 |
| Z3 流复用 | Z2 之后 | 普通 `Get(keys[0])` | 内容一致（全部归还后流 PutBack 复用） |
| Z4 size<0 拒绝 | —— | `GetFdBatch(keys, 0, -1)` | `ErrInvalidRange` |
| Z5 缺失 key | —— | keys 含不存在 key | `ErrNotFound`（整批） |

## 覆盖位置

- `test/e2e/h_sdk_batch_fd_test.go:214`（TestH5GetFdBatch：Z1–Z5）
- 单元层：`internal/transport/transport_shm_test.go:1609`（PutBatchKeys+GetFdBatch 内容与 fd）、`:1708`（部分/重复释放幂等、整批后连接复用）、`:1753`（缺 key/协议异常/参数校验）

## 运行

```bash
go test -tags e2e ./test/e2e/ -v -run 'TestH5GetFdBatch' -count=1
```

## 注意

- Z1 的 `Pread` 校验是核心：批量交付最易出 foff 错位类回归，仅验 Data 内容会漏（Data 与 fd/foff 指向同段内存时二者一致才健康，错位时二者都错但可能相互掩盖——Pread 从 fd 独立读出作交叉验证）。
- 同机 + auto 传输才有 `Fd>0`；跨节点断言需调整（见 getfd.md 注意）。
- 整批兜底 release 与逐 buf Release 混用时序：先逐 buf 后整批（或只用其一），不可整批后再逐 buf。
