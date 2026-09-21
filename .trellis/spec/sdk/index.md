# 对外 SDK 层（pkg/taihu-client）

> 外部业务访问 taihu 集群的唯一入口：一个目录、一个包、一个 `*Storage`；这里写的是这条边界的实际形态与机器强制手段。

---

## 本层是什么

- `pkg/` 下**只有**这一个包（`ls pkg/` 仅 `taihu-client/`），所以「SDK 层」= `pkg/taihu-client/`。
- **目录名与包名不一致**：目录 `pkg/taihu-client`，包名 `taihuclient`（`pkg/taihu-client/config.go:7`）。import 路径末段 `taihu-client` 含连字符、不是合法 Go 标识符，调用方**只能**用包名引用：

  ```go
  	"github.com/liucxer/taihu/pkg/taihu-client"
  )

  // 编译期断言：真实 SDK 客户端必须满足本示例使用的能力集。
  var _ clusterClient = (*taihuclient.Storage)(nil)
  ```
  （`examples/taihu-client/main.go:25`、`examples/taihu-client/main.go:40`）

  即：外部代码里出现的是 `taihuclient.`，不是 `taihu-client.`。改包名 = 改对外契约。
- 包注释把职责说死为「集群客户端（缓存场景：首写本地 + 索引锚定 + 回源兜底）」，并明确定位层不进数据面热路径（`pkg/taihu-client/config.go:1-7`）。

## 分层边界：SDK 不碰引擎

### 规则 1：非测试代码只 import 三个 internal 包

`grep -rn "liucxer/taihu/internal" --include='*.go' pkg/ | grep -v '_test.go'` 的全部命中如下（去掉 `_test.go` 后）：

| internal 包 | 引用位置 |
|---|---|
| `internal/cluster` | `config.go:13`、`index.go:7`、`ops.go:7`、`picker.go:7`、`registry.go:8`、`reexport.go:4`、`storage.go:11`、`tikv.go:8` |
| `internal/rpcclient` | `reexport.go:5`、`storage.go:12` |
| `internal/version` | `storage.go:13` |

数据面是**委托**给 `internal/rpcclient`，SDK 自己不解包、不碰引擎：`clientFor` 只做「选实例 → 拨号 → 缓存连接」（`pkg/taihu-client/storage.go:117-177`），落到 `rpcclient.DialPool` / `DialPoolMulti` / `DialShmPool`（`storage.go:136-162`）。`Storage` 的类型注释同口径：「数据面复用 rpcclient.Storage（netpoll 零拷贝），定位层不进数据面热路径」（`storage.go:25`）。

注意 `Makefile:36` 的注释把允许面写成 `transport/cluster/metastore/ierr/version`，但**实际直接 import 的没有 metastore/ierr/transport** —— 它们只经 `rpcclient` 的 re-export 间接透出。以 grep 结果为准，别照抄注释。

### 规则 2：`make check-sdk-only` 是这条边界的机器强制

`Makefile:52-58`：

```make
check-sdk-only:
	@viol=$$(grep -rn '"github.com/liucxer/taihu/internal/\(storage\|device\|aio\|bufpool\|layout\)"' \
	  --include='*.go' pkg/ | grep -v '_test.go' || true); \
	if [ -n "$$viol" ]; then \
	  echo "违规：pkg/ 直接依赖存储引擎（SDK 不得依赖引擎内部，测试除外）："; echo "$$viol"; exit 1; \
	fi; \
```

要点（都写在 `Makefile:31-44` 的注释里）：

- 禁的是 `internal/{storage,device,aio,bufpool,layout}` 五个包，用正则字面量匹配 import 路径，排除 `_test.go`（`Makefile:54`）。
- 排除测试的理由：同模块测试里需要起真实引擎 + server 做端到端接线，那是合法的，这条只约束库代码（`Makefile:43-44`）。注意该注释举的例子 `pkg/rpcclient`、`pkg/rpccluster` 都是历史路径：前者现在在 `internal/rpcclient`，后者已不存在（集群客户端就是本包 `taihu-client`，`internal/` 下无 `rpccluster`）。注释未随搬迁更新，读时以实际目录为准。
- 用 grep 而**不用** `go list`：`go list` 在 darwin 上看不见 `//go:build linux` 的文件，会本机通过、Linux 上炸；grep 对 build tag 无感，代价是注释/字符串里的路径字面量会误报（`Makefile:38-42`）。（该注释举的 `internal/transport/server_shm.go` 同样是历史路径，现为 `server_shm_linux.go` / `server_shm_other.go` 一对。）

### 规则 3：反向也守 —— `internal/` 不得依赖 `pkg/`

`Makefile:45-50` 的 `check-layering` 用同一手法反向 grep `"github.com/liucxer/taihu/pkg/`，理由是「违反即成环，也说明引擎又爬回了对外目录」（`Makefile:35`）。加新底层包时两条都要过。

### 规则 4：本层没有平台分裂文件

`grep -rn "//go:build\|+build" pkg/` 无命中 —— `pkg/taihu-client` 下**没有任何 `//go:build` 文件**，本层不是平台相关层，不存在 darwin/linux 双实现。平台分裂只发生在下层（典型是 `internal/transport/` 的 `server_shm_linux.go` + `server_shm_other.go` 这一对）。SDK 里出现同机 shm / 跨节点 TCP 的差异是靠运行时比较 hostname 做的（`storage.go:149`），不是编译期分裂。

## 与其它层的关系

re-export 的**跨模块完整论证**（为什么必须 alias、为什么 alias 而非新类型、漏补为何不报错）在 `.trellis/spec/transport/interfaces-and-reexport.md`；本层只写 SDK 侧可见面与外部调用约定，见 `public-api.md`。

## 本层文件

| 文件 | 用途 |
|---|---|
| `index.md` | 本文件：边界、机器强制手段、开发前检查与质量校验 |
| `public-api.md` | 对外可见的名字、`Storage` 能力面、配置项、re-export 契约、错误与示例约定 |

（两个文件不合并：`index.md` 规定必须有一张「本层其它文件」表，单文件时该表为空；且边界约束属流程性内容、公开 API 属参考性内容，混排后每次改 API 都要动检查单。）

## Pre-Development Checklist

- [ ] 新增的 import 属于 `internal/{cluster,rpcclient,version}` 之一，或先想清楚为什么必须新增（`Makefile:52-58` 会拦掉引擎五个包）
- [ ] 新增导出签名里若出现 `internal/` 类型，已在 `pkg/taihu-client/reexport.go` 补齐 `type X = ...`（维护约定 `pkg/taihu-client/reexport.go:17`）
- [ ] 若新增了调用方能观察到的库错误，已在 `reexport.go:32-43` 的 `var` 块里给出可命名的别名，并确认它确实会到达客户端
- [ ] 改动 `Storage` 方法集后确认 `var _ rpcclient.ObjectStore = (*Storage)(nil)` 仍成立（`pkg/taihu-client/storage.go:49`）
- [ ] 改动 `Storage` 能力面后同步 `examples/taihu-client/main.go:28-37` 的 `clusterClient` 接口与 `main.go:40` 的编译期断言
- [ ] 调过 `ClusterConfig` / `TiKVOptions` 字段后，同步另一侧（`tikv.go:51-64` 是逐字段搬运，漏一个就静默丢配置）
- [ ] 新增后台 goroutine 时确认 `Close()` 能停掉它（现有：registry、index、client keepalive，`storage.go:340-348`）

## Quality Check

```bash
# 边界与格式：check-fmt + check-layering + check-sdk-only + go vet
make check

# 只跑本层单测（SDK 测试不依赖真实 TiKV，用 testutil_test.go 的内存 KV）
go test ./pkg/taihu-client/...

# 外部调用方视角的接线验证（示例即契约）
go test ./examples/taihu-client/...

# 跨平台核对：本机是 darwin，Linux 专有代码的错误只有这一步能暴露
make check-linux
```

`make check` 的组成见 `Makefile:21`；`make check-linux` 的用途说明见 `Makefile:60-64`。
