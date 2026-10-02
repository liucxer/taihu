# GetFd —— 数据面 e2e 测试文档

## 语义

```go
func (s *Storage) GetFd(ctx context.Context, key string, off, size int64) (int, uint64, []byte, func(), error)
```

- 读 `[off, off+size)` 并**尽力交付共享内存 (fd, offset) 零拷贝源**（供 FUSE 读路径 splice）。
- 返回值语义（`internal/rpcclient/storage_rpc.go:74`）：
  - `fd>0`：shm 通道交付；`splice(fd, foff, size)` 完成后调 `release()`；`data` 与 fd/foff 指向同一段内存；
  - `fd==0`：连接不支持 fd 交付（TCP/多帧）→ 回退 Get 拷贝路径，`data` 为池化拷贝缓冲。
- 定位与 Get 相同（路由缓存 → 索引 → 回源；回源仅走拷贝路径）；miss `ErrNotFound`。
- **此前无 e2e 覆盖**（仅 `internal/transport/transport_shm_test.go:1609` 单元层），H 场景补齐。

## e2e 用例（TestH4GetFd，`h_sdk_batch_fd_test.go:168`）

| 用例 | 前置 | 步骤 | 预期 |
|---|---|---|---|
| F1 shm 零拷贝交付 | 同机实例 + SDK(auto→shm) | Put 1MiB pattern → `GetFd(key, 0, -1)` | `fd>0`；`data` 内容一致；**`syscall.Pread(fd, foff, len)` 读出与对象一致**（验证 (fd,foff) 真实指向设备内数据） |
| F2 释放后流复用 | F1 之后 | `release()` → 普通 `Get` 同 key | 内容一致（连接未损坏） |
| F3 TCP 回退拷贝 | 同实例 + SDK(`Transport=rpc`) | `GetFd(key, 0, -1)` | `fd==0`（回退）；`data` 内容一致 |
| F4 miss | —— | GetFd 不存在 key | `ErrNotFound`（SDK 内部转回源，无 Source 时仍 ErrNotFound） |

## 覆盖位置

- `test/e2e/h_sdk_batch_fd_test.go:168`（TestH4GetFd：F1–F3）
- 单元层：`internal/transport/transport_shm_test.go:1609-1706`（shm fd 交付）、`internal/rpcclient/storage_rpc.go:57-82`（fdGetter 接口与回退）

## 运行

```bash
go test -tags e2e ./test/e2e/ -v -run 'TestH4GetFd' -count=1
```

## 注意

- F1 的 `fd>0` 断言依赖 e2e 环境**同机 + auto 传输**：SDK 与 server 同节点（127.0.0.1），hostname 一致 → shm 通道。跨节点跑该用例需改 `Transport=shm` 或接受回退。
- `Pread` 校验是本用例的核心价值：只验「fd>0」无法发现 foff 错位/短交付类回归。
- release() 必须在 splice 完成后调用；幂等可重复调。
