# 错误与包布局收敛：ierr 迁 pkg、错误集中化、aio/device 重构

## 背景 / 性质

回溯式收口任务：主体实现已完成于本会话（对话中直接执行，未走 task 流程），本任务用于
补齐 Trellis 记录、同步 spec、提交与归档。**不再新写功能代码。**

## 已完成范围（验收即现状）

1. **ierr 迁移**：`internal/ierr` → `pkg/ierr`（公开错误事实源）；全仓 29 处 import 改写；
   re-export 链注释与包文档更新。
2. **错误集中化**：全仓非测试生产代码仅剩 1 处 `errors.New`（examples 一次性提示，不入库）；
   `pkg/ierr` 现为 26 个哨兵：
   - 新增合并：`ErrInvalidArgument/ErrRPCError/ErrKeyTooLong/ErrConnClosed/ErrShmBadFrame/
     ErrShmStreamBroken/ErrShmUnsupported(双平台合一)/ErrShmOnly/ErrAdminShmOnly/
     ErrStorageClosed/ErrDeviceClosed/ErrNoInstances/ErrSourceUnset/ErrKVRequired/ErrNoShmAddr`
   - 各包本地 `var … = errors.New(...)` 定义删除，引用点改 `ierr.*`；`ErrShmOnly`、
     `ErrNoInstances` 保留导出名 re-export。
3. **aio 层**：io_uring 三文件合一为 `ring_uring_linux.go`；测试并入平台两文件；
   契约改单一共享设备文件（Truncate 复位），O_DIRECT 目录探测（TAIHU_AIO_TEST_DIR）。
4. **device 层**：非测试收为 3 文件（options 并入 device.go）；测试收为
   `device_linux_test.go`/`device_other_test.go`；O_DIRECT 目录探测（TAIHU_DEVICE_TEST_DIR，
   修复 /tmp(tmpfs) EINVAL）；`TestDeviceIOErrorPaths` 按 fd 下沉改为构造期注入错误方向 fd。

## 收尾步骤（本任务实际执行面）

- [ ] 同步 spec：`.trellis/spec/error/index.md` 及其余引用旧路径 `internal/ierr` 的 spec 正文改 `pkg/ierr`
- [ ] 提交（本会话未提交量）：ierr 迁移 + 错误集中化 + 此前 device 分批已推外的部分
- [ ] 归档 task + 记录 session

## 验收标准

- `go build ./...`、darwin/linux vet 通过；aio/device/storage/transport/rpcclient/taihu-client/ierr 测试通过
- spec 中"唯一事实源"路径为 `pkg/ierr`
- 工作区干净，commit 已推（按用户要求）