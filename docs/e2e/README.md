# taihu SDK 数据面 API 端到端测试文档

本目录为 `pkg/taihu-client` 每个**数据面 API** 各提供一份 e2e 测试文档：语义、用例表、覆盖位置、运行方法。总览见本文件。

| API | 文档 | e2e 主覆盖 | 备注 |
|---|---|---|---|
| Put | [put.md](put.md) | `a_data_test.go:278` TestASDKPath 等 | 直连+SDK 双路径 |
| Get | [get.md](get.md) | `a_data_test.go:69` TestA4RangeRead 等 | 含 size=-1、回源 |
| GetFd | [getfd.md](getfd.md) | `h_sdk_batch_fd_test.go:168` TestH4GetFd | 零拷贝 + TCP 回退 |
| GetBatch | [getbatch.md](getbatch.md) | `h_sdk_batch_fd_test.go:112` TestH3GetBatch | SDK 不接受 size<0 |
| GetFdBatch | [getfdbatch.md](getfdbatch.md) | `h_sdk_batch_fd_test.go:214` TestH5GetFdBatch | 逐 buf Release |
| PutBatch | [putbatch.md](putbatch.md) | `h_sdk_batch_fd_test.go:47` TestH1PutBatch | 同内容多 key |
| PutBatchKeys | [putbatchkeys.md](putbatchkeys.md) | `h_sdk_batch_fd_test.go:80` TestH2PutBatchKeys | 各 key 各内容 |
| Delete | [delete.md](delete.md) | `a_data_test.go:153` TestA6Delete 等 | 含索引同步删 |
| Stat | [stat.md](stat.md) | `a_data_test.go:278` TestASDKPath 等 | 尺寸一致性 |
| HasLive / CheckPoolIsValid / UsageGet / ListIndexKeys | — | `h_sdk_batch_fd_test.go:269` TestH6Ops + `a_data_test.go:286,304` | 运维观测面，非数据面 |

## e2e 套件结构（test/e2e/）

| 场景 | 文件 | 内容 |
|---|---|---|
| A | `a_data_test.go` | 数据正确性：尺寸矩阵、区间读、不可压缩内容、覆盖写、删除、多块、混合批量、SDK 路径、回源重建 |
| B | `b_transport_test.go` | 传输层：TCP 协议、并发流、shm 不可用降级 |
| C | `c_registry_test.go` | 注册/发现：心跳、轮询路由、本地路由、KV 不可达降级 |
| D | `d_lifecycle_test.go` | 生命周期：重启、容量用尽、段回收、优雅停机 |
| E | `e_concurrency_test.go` | 并发读写删竞态 |
| F/G | `f_soak_test.go` `g_soak_long_test.go` | 浸泡/长时浸泡 |
| H | `h_sdk_batch_fd_test.go` | **SDK 批量 + 零拷贝 API**（PutBatch/PutBatchKeys/GetBatch/GetFd/GetFdBatch/ops） |

## 运行方法

e2e 需要真实环境：**Linux 节点 + 真实 TiKV**（起真 `taihu server` 进程、losetup loop 块设备、O_DIRECT）。所有用例带 `//go:build e2e` tag；未设 `E2E_PD` 时整包 `t.Skip`，不误报。

```bash
# 在 Linux 集群节点上：
go test -tags e2e ./test/e2e/ -v -count=1                # 全套
go test -tags e2e ./test/e2e/ -v -run 'TestH' -count=1   # 仅 H 场景（批量+零拷贝）
go test -tags e2e ./test/e2e/ -v -run 'TestASDKPath' -count=1
```

环境变量（`harness_test.go:8`）：

| 变量 | 缺省 | 说明 |
|---|---|---|
| `E2E_PD` | 必填 | TiKV PD 地址（逗号分隔），未设则全部跳过 |
| `E2E_BIN` | 现场构建 | taihu 二进制路径；缺省 `go build ./cmd/taihu` 一次 |
| `E2E_WORKDIR` | `/var/tmp/e2e-taihu` | 工作目录根（勿用 tmpfs，O_DIRECT 打不开） |
| `E2E_DEV` | 自动 losetup | 复用既有块设备（如 `/dev/loop9`） |
| `E2E_DEV_SIZE` | 16GiB | 稀疏镜像大小（= 2 个 8GiB segment） |

隔离与清理（`harness_test.go:19-26`）：TiKV 与生产集群共用时，实例名 / key 前缀 / 工作目录均带随机 tag；客户端 KV 经 `scopedKV` 收敛扫描区间；`t.Cleanup` 停进程、清注册/容量/索引记录、卸 loop、删目录。

## 零拷贝用例的特殊断言

H 场景的 GetFd / GetFdBatch 用例（[getfd.md](getfd.md)、[getfdbatch.md](getfdbatch.md)）不止校验返回值形状：SDK 与 server 同机（auto → shm 通道）时**必须交付 fd>0 的 splice 源**，并用 `syscall.Pread(fd, foff)` 直接读出对象内容比对——验证 `(fd, foff)` 真实指向设备内对象数据（模拟 FUSE splice 的前置校验）。`Transport=rpc` 强制 TCP 时断言回退拷贝路径（fd==0、data 正确）。

## 新增用例指引

1. 每个场景一个文件（下一字母 `i_`），文件头 `//go:build e2e`，中文注释说明覆盖目标。
2. 复用 harness：`newHarness` → `startServer(serverOpts{})` → `newSDK(nil 或 mut)`；断言用 `getExact/getRange/putExact/waitFor/pattern/randBytes`（`harness_test.go:604-684`）。
3. key 一律经 `h.key(...)` 加前缀（索引清理依赖整段 DeleteRange）。
4. 数据用 `randBytes`（不可压缩）或 `pattern`（区间读可重算期望值），勿用零填充。
5. 本机（darwin/无 E2E_PD）只能验证编译与 skip 路径：`go vet -tags e2e ./test/e2e/`。
