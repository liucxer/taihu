# 平台分裂与构建（platform）

> 本仓库是「Linux 生产 + macOS 开发」的双平台工程：平台专有代码用 `_linux.go` / `_other.go` 成对切分，`_other` 侧多数是同语义降级实现、少数是明确不支持的桩；构建门禁靠 `make check-linux` 在 macOS 上模拟 Linux 编译。

---

## 本层边界

本层是**跨包**的一层，管的是「同一份逻辑在不同 GOOS 上怎么落地」与「怎么在 macOS 上证明 Linux 编得过」，不重复各包自己的业务规则。

| 归属 | 内容 |
|------|------|
| 本层管 | `_linux.go` / `_other.go` 的命名与配对约定、build tag 的写法、平台桩的两种语义（降级 vs 明确不支持）、测试文件后缀顺序、`third_party/` 的免检与并模块、`make check-linux` 的四条命令与适用范围 |
| 不归本层管 | 各包内部实现（见 `.trellis/spec/engine/`、`.trellis/spec/transport/`）；测试分层的组织方式（见 `.trellis/spec/testing/`）；分层依赖门禁 `check-layering` / `check-sdk-only`（见 `.trellis/spec/architecture/layering.md`） |

平台分裂只发生在 `internal/` 的四个包里（`aio`、`device`、`rpcclient`、`transport`）。`pkg/` 下**没有任何** `//go:build` 文件 —— SDK 层是完全平台中立的，同机 shm / 跨节点 TCP 的差异靠运行期比较 hostname 决定，不是编译期分裂（`.trellis/spec/sdk/index.md:63-65`）。

## 本层文件

| 文件 | 用途 |
|------|------|
| [file-splitting.md](./file-splitting.md) | 平台专有代码怎么切：`_linux` / `_other` 成对清单（7 对）、`_other` 侧必须显式写 `//go:build !linux` 的原因、降级实现与「明确不支持」的分界、未导出 sentinel、测试文件后缀顺序、`third_party/` 免 gofmt 与并进主模块 |
| [build-verification.md](./build-verification.md) | 怎么在 macOS 上验证 Linux 可构建：`make check-linux` 四条命令逐条拆解、为什么必须跑、`CGO_ENABLED=0` 在本项目的确切含义、本机能做与必须上真机的分工、改 fork 后的验收口径 |

## Pre-Development Checklist

动手前逐条确认：

- [ ] 新增平台专有实现时，**成对给**：`x_linux.go` 真实现 + `x_other.go` 降级实现，且 `_other` 侧文件首行写 `//go:build !linux`（`file-splitting.md` §2）。
- [ ] 确认你要给的是**降级实现**还是**明确不支持**：能给出同语义兜底的（如 `internal/aio` 的 goroutine + 同步 pread/pwrite）就给兜底；做不到的（io_uring、shmipc）返回错误/sentinel，且让上层能运行期判掉（`file-splitting.md` §4）。
- [ ] 上层调用点不要跟着加 build tag：`cmd/taihu/cmd/server.go` 主文件无 tag，靠 `transport.ShmSupported()` 在运行期跳过 shm 数据面（`file-splitting.md` §6）。
- [ ] 新增 Linux 专有测试文件：文件名写成 `x_linux_test.go`，`_test` 必须排在 GOOS 后缀**之后**；若 GOOS 不紧邻 `_test`（如 `x_linux_more_test.go`），必须补显式 `//go:build linux`（`file-splitting.md` §8）。
- [ ] 改动任何平台相关文件后跑 `make check-linux`（本节 Quality Check）。
- [ ] 动了 `third_party/{netpoll,shmipc-go}`？不要顺手 gofmt，也不要在目录下新建 `go.mod`（`file-splitting.md` §9、§10）。

## Quality Check

```bash
# 本层第一道门：跨平台编译核对（linux/amd64 + linux/arm64，含测试二进制编译）
make check-linux

# 回本机（darwin）跑一遍实际测试。注意本机 go test ./... 不是全绿，
# 有 7 个平台性失败（cmd/taihu/cmd 2 个 + third_party/shmipc-go 5 个），
# 详见 build-verification.md §4。判定以 Linux 结果为准。
go test ./...

# 格式 + 分层不变量 + go vet（本机可跑）
make check
```

`make check-linux` 是本层的**唯一硬门禁**，它对本层不是可选项：本机是 macOS，Linux 专有文件（`aio_uring_linux.go`、`probe_linux.go`、`client_shm_linux.go`、`server_shm_linux.go`、`shm_frame_linux.go`）在本地根本不进编译，`unix.POLLIN`、`Syscall6` 参数个数、UAPI 结构体 size/offset 断言、测试名后缀顺序这些错误只有它能把住（`Makefile:60-64`）。门禁上线即抓到过真实问题：残留的 rpcserver 引用、以及 vet 的 `lostcancel` 告警（`doc/设计文档/20260914_结构评审与优化建议.md:323`；该处提到的 `pkg/rpcclient/pool_test.go` 是评审当时的路径，`pkg/rpcclient` 现已迁到 `internal/rpcclient`，`pkg/` 下只剩 `taihu-client`）。

## 两份实现的分界（速查）

| 包 | Linux | 非 Linux | 语义 |
|----|-------|----------|------|
| `internal/aio` | libaio（`aio_linux.go:56`）、io_uring（`aio_uring_linux.go:200`） | goroutine + 同步 pread/pwrite 兜底（`aio_other.go:31`） | 同语义降级，接口一致 |
| `internal/aio` io_uring 后端 | `newIOUringRing` 真实现 | 直接返回 `errors.New("aio: io_uring 仅 Linux 支持")`（`aio_other.go:42-44`） | 明确不支持 |
| `internal/aio` 探测 | 真跑一次 io_uring_setup（`probe_linux.go:19-42`） | 恒 `Supported=false`（`probe_other.go:9-11`） | 降级：auto 模式自动回退 |
| `internal/device` 打开 | `O_RDWR\|O_DIRECT\|O_EXCL`（`device_linux.go:16`） | `O_RDWR\|O_SYNC` 普通打开（`device_other.go:10`） | 降级，仅本机联调 |
| `internal/device` 容量 | `BLKGETSIZE64` ioctl（`info_linux.go:22`） | 普通文件 `Stat().Size()` 近似（`info_other.go:14`） | 降级，语义不同但够本机用 |
| `internal/rpcclient` shm | 真连 shmipc（`dial_shm_linux.go:12-28`） | `errShmUnsupported`（`dial_shm_other.go:12-22`） | 明确不支持 |
| `internal/transport` shm 服务端 | `ShmSupported() = true`（`server_shm_linux.go:38`） | `ShmSupported() = false`（`server_shm_other.go:20`） | 明确不支持，但调用方据布尔降级 |
