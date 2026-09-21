# 存储引擎（engine）

> 覆盖 `internal/{aio,bufpool,device,layout,metastore,storage}` 六个包：从异步磁盘 IO、对齐缓冲池、裸设备访问，到段物理布局、pebble 元数据与对象读写 / 段级压缩。

---

## 本层职责与包边界

| 包 | 职责 | 入口文件 |
|---|---|---|
| `internal/layout` | 裸设备物理布局参数（段大小 / 段数）与 4K 对齐工具 | `internal/layout/layout.go` |
| `internal/aio` | 异步磁盘 IO 的纯 Go 实现，三后端（io_uring / libaio / 非 Linux goroutine 兜底） | `internal/aio/aio.go` |
| `internal/bufpool` | 2 的幂分桶的 4K 对齐缓冲池 + 精确尺寸池 | `internal/bufpool/bufpool.go` |
| `internal/device` | 裸设备按 segment 的 append / read，单一完成泵 | `internal/device/device.go` |
| `internal/metastore` | pebble 持久化的映射、段状态、顺序写游标分配器 | `internal/metastore/store.go` |
| `internal/storage` | 对外 object 存储：Put/ReadAt/Batch*/Compactor | `internal/storage/storage.go` |

### 本层必须遵守的三条结构性约定

1. **`layout` 独立成包的唯一理由是打断循环依赖**：`device`（IO 层）与 `metastore`（分配层）共同依赖它 —— `internal/layout/layout.go:1-2`。
2. **异步 IO 的唯一出口是 `internal/aio`**，两个 Linux 后端实现同一个 `Ring` 接口，调用方无感 —— `internal/aio/aio.go:10-11`。任何新代码不应绕过它直接 syscall。
3. **整盘段数不再硬编码**：v1 曾固定 2048 段 × 8GB，现由启动时读到的真实设备容量经 `ComputeLayout` 计算后注入 device 与 metastore —— `internal/layout/layout.go:6-7`、`internal/layout/layout.go:22-23`。

## 边界由 Makefile 守卫（不是靠自觉）

`make check` 里有两条反向 grep，两条方向都要守，少一条边界就会被绕回来 —— `Makefile:35-36`：

- `internal/` 不得反向依赖 `pkg/`（违反即成环，也说明引擎又爬回了对外目录）—— `Makefile:45-50`。
- `pkg/` 不得直接依赖存储引擎 —— `Makefile:52-58`：

```make
check-sdk-only:
	@viol=$$(grep -rn '"github.com/liucxer/taihu/internal/\(storage\|device\|aio\|bufpool\|layout\)"' \
	  --include='*.go' pkg/ | grep -v '_test.go' || true); \
	if [ -n "$$viol" ]; then \
	  echo "违规：pkg/ 直接依赖存储引擎（SDK 不得依赖引擎内部，测试除外）："; echo "$$viol"; exit 1; \
	fi; \
	echo "check-sdk-only: OK"
```

注意第二条**排除 `_test.go`**：同模块测试里 `pkg/rpcclient`、`pkg/rpccluster` 需要起真实引擎 + server 做端到端接线，那是合法的 —— `Makefile:43-44`。

两条都用 grep 源码而**不用 `go list`**：`go list` 在本机（darwin）看不见 `//go:build linux` 的文件，而平台分裂的 shm 服务端恰恰是最容易漏改的那个 —— 用 `go list` 会本机通过、Linux 上炸。grep 对 build tag 无感 —— `Makefile:38-41`。

> 注意 `Makefile:39` 在这句话里举的例子是 `internal/transport/server_shm.go`，**该路径已不存在**：它在「平台文件统一 `_linux`/`_other` 后缀」那次重构里拆成了 `internal/transport/server_shm_linux.go` 与 `internal/transport/server_shm_other.go`。Makefile 注释没跟着改，照抄它会得到一个死引用 —— 以 `ls internal/transport/` 的结果为准。

## 错误体系不在本层重复定义

`ierr` 的定义与分层规则见 `.trellis/spec/architecture/error-model.md`。本层只出现一处需要记住的反例：`ierr.ErrConflict` 是 compaction 内部的 CAS 控制信号，**不暴露给客户端**，`internal/rpcclient/reexport.go:30` 明确把它排除在 re-export 之外。

## 本层其它文件

| 文件 | 用途 |
|---|---|
| [buffer-and-concurrency.md](./buffer-and-concurrency.md) | 缓冲所有权协议、`bufpool` 为何否决 `sync.Pool`、单一完成泵模型、Ring 并发契约、metastore 锁纪律 |
| [metadata-and-compaction.md](./metadata-and-compaction.md) | pebble 命名空间与编码、`Store` 接口契约、段状态机与 GC、分配器、CAS 搬移与 compaction |

## 已知缺口（对后续 agent 是真信息）

- **`internal/storage` 没有包文档注释**。包内 4 个非测试文件（`storage.go`、`compact.go`、`admin.go`、`options.go`）首行均为 `package storage`，无 `// Package storage ...`。包级设计意图散落在各导出方法注释里，例如「Storage 不持有写游标状态」写在 `internal/storage/storage.go:14-18` 的 `Storage` 类型注释上。
- **`internal/metastore` 的包文档只在 `store.go`** —— `internal/metastore/store.go:1-2`。`kv_pebble.go`、`segments.go`、`meta.go`、`cache.go` 都没有包文档；实现约定写在类型注释上，例如段管理的全部机制（存活计数、状态机、读引用、空闲池、后台 GC）写在 `internal/metastore/segments.go:13-26` 的 `segmentManager` 注释里。
- **`internal/aio` 的包文档只在 `aio.go`** —— `internal/aio/aio.go:1-25`（并明确说明「本文件集中该包的全部对外 API」）。`aio_linux.go`、`aio_uring_linux.go`、`probe_linux.go` 只有文件内注释。
- **`internal/device` 同理**：包文档在 `internal/device/device.go:1-11`，平台专有文件（`device_linux.go`、`info_linux.go`、`options.go`）无包文档。
- **本层没有 `internal/storage` 级别的「设计文档」落点**：`CompactorConfig` 的注释指向一份仓库外文档《segment 级 Compaction（数据迁移）设计方案》—— `internal/storage/compact.go:17-18`。该文档不在仓库内，读注释是唯一途径。

## Pre-Development Checklist

- [ ] 改动是否会撞上两条方向检查？先跑 `make check`（`internal/` 不得依赖 `pkg/`；`pkg/` 不得依赖 `storage|device|aio|bufpool|layout`）。
- [ ] 新申请 / 归还的缓冲是否走 `bufpool`？读路径的返回值是否由调用方 `bufpool.Put` 归还？—— 见 [buffer-and-concurrency.md](./buffer-and-concurrency.md) 第一节。
- [ ] 是否新增了 `ring.Wait` 调用点？本仓库的完成泵是它唯一的持有者 —— `internal/device/device.go:145`。
- [ ] 新增的 metastore 方法是否应在锁内（按惯例用 `*Locked` 后缀命名）？锁序是否仍是 `allocator.mu` → `segmentManager.mu` —— `internal/metastore/segments.go:26`。
- [ ] 是否引入了新的段状态迁移？重启自愈分支（`Compacting` → `Full`/`Reclaiming`）是否也要跟着改 —— `internal/metastore/segments.go:89-101`。
- [ ] 平台专有代码（`//go:build linux`）改动后是否跑了 `make check-linux`。
- [ ] 改动是否触碰 `reserveSegs` 语义（预留缓冲段只给 compaction 用）—— `internal/metastore/kv_pebble.go:67-70`。

## Quality Check

```bash
make check          # check-fmt + check-layering + check-sdk-only + go vet ./...
go test ./internal/...    # 本层单测；aio/device 的 Linux 后端与 O_DIRECT 相关用例需在 Linux 上跑
make check-linux    # 跨平台编译核对：GOOS=linux 的 go vet / go build / go test -c
```

- `make check` **不执行测试**，它只做格式化、两条方向检查与 `go vet` —— `Makefile:21-22`。单测须显式 `go test`。
- 改了 `aio` / `device` 的 `_linux.go` 后必须跑 `make check-linux`：这些错误在 macOS 上编得过、只在 Linux 上暴露 —— `Makefile:60-70`。注意它用 `go test -c -o /dev/null ./...`，**只编译测试二进制不执行**，本机是 macOS 跑不了 linux 测试，但能编过 —— `Makefile:64` 注释。
- `check-fmt` 会跳过 `third_party/`（上游 fork 保持与上游一致的格式）—— `Makefile:24-28`。
