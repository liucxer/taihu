# 分层与依赖方向

> 两条方向不变量由 `make` 守住：`internal/` 不得反向依赖 `pkg/`，`pkg/` 不得直接依赖存储引擎 —— 少守一条，边界就会被绕回来。

---

## 为什么要有这两条门禁

`Makefile:30-44` 的注释块把规则和理由一起写死了：

```make
# ── 分层不变量 ────────────────────────────────────────────────────────────────
# pkg/ 只放**客户端 SDK**（外部调用方唯一入口），非客户端面一律下沉 internal/。
# 引擎（internal/storage）、传输层、元数据层都是服务端与客户端共用的下层实现。
#
# 两条方向都要守，少一条边界就会被绕回来：
#   check-layering  internal/ 不得反向依赖 pkg/ —— 违反即成环，也说明引擎又爬回了对外目录
#   check-sdk-only  pkg/ 不得直接依赖存储引擎 —— SDK 只应依赖 transport/cluster/metastore/ierr/version
```

`README.md:96` 是同一契约的文字版：`pkg/` 只放对外唯一 SDK 包 `taihu-client`；业务访问**必须**走 `taihu-client`（TiKV 集群路由），直连客户端 `internal/rpcclient` 仅供服务端与命令行内部使用。

---

## 规则

### 1. `internal/` 不得 import `pkg/`

实现体在 `Makefile:45-50`，用 grep 扫源码：

```make
check-layering:
	@viol=$$(grep -rn '"github.com/liucxer/taihu/pkg/' --include='*.go' internal/ || true); \
	if [ -n "$$viol" ]; then \
	  echo "违规：internal/ 不得依赖 pkg/（pkg/ 只放客户端 SDK）："; echo "$$viol"; exit 1; \
	fi; \
	echo "check-layering: OK"
```

当前实测无命中（`grep -rn '"github.com/liucxer/taihu/pkg/' --include='*.go' internal/` 无输出）。

### 2. `pkg/` 非测试代码不得直接 import 存储引擎

实现体在 `Makefile:52-58`，被禁的五个包写死在正则里：

```make
check-sdk-only:
	@viol=$$(grep -rn '"github.com/liucxer/taihu/internal/\(storage\|device\|aio\|bufpool\|layout\)"' \
	  --include='*.go' pkg/ | grep -v '_test.go' || true); \
	if [ -n "$$viol" ]; then \
	  echo "违规：pkg/ 直接依赖存储引擎（SDK 不得依赖引擎内部，测试除外）："; echo "$$viol"; exit 1; \
	fi; \
	echo "check-sdk-only: OK"
```

**`_test.go` 是明确豁免的**，理由写在 `Makefile:43-44`：同模块测试里 `pkg/rpcclient`、`pkg/rpccluster` 需要起一个真实引擎 + server 做端到端接线，那是合法的；这条只约束库代码。实测豁免点：`pkg/taihu-client/storage_multiaddr_test.go:13-14` 引了 `internal/layout` 与 `internal/storage`。

### 3. 门禁汇总入口是 `make check`

`Makefile:21-22`：

```make
check: check-fmt check-layering check-sdk-only
	go vet ./...
```

即 `make check` 是提交前的唯一必跑命令，分层两条只是其中两项。

---

## 为什么用 grep 而不是 `go list`

`Makefile:38-41` 给了理由，这条决定了门禁的写法不能"优化"：

```make
# 两条都用 grep 源码，**不用 go list**：go list 在本机（darwin）看不见
# `//go:build linux` 的文件，而 internal/transport/server_shm.go 恰恰是最容易漏改的那个 ——
# 用 go list 会本机通过、Linux 上炸。grep 对 build tag 无感。代价是注释/字符串里的
# 路径字面量会误报，这里可接受（宁可误报）。
```

即：**宁可误报也不漏报**。不要为了"减少误报"把 grep 换成 AST/`go list` 方案。

引用的是原文，其中 `internal/transport/server_shm.go` 是**已失效的历史路径** —— 该文件在平台文件统一 `_linux`/`_other` 后缀后拆成了 `internal/transport/server_shm_linux.go` 与 `internal/transport/server_shm_other.go`，Makefile 注释未同步。别把这个名字抄进新代码或新文档。

---

## 依赖全貌（非测试代码）

逐条 `grep -rn '"github.com/liucxer/taihu/' --include='*.go' <包> | grep -v '_test.go:'` 实测所得：

| 包 | 依赖的本项目包 |
|----|----------------|
| `cmd/taihu` | `cmd/taihu/cmd` |
| `cmd/taihu/cmd` | `internal/{aio,benchkit,bufpool,cluster,device,layout,metastore,rpcclient,storage,transport,version}`、`pkg/taihu-client` |
| `internal/transport` | `internal/{bufpool,device,ierr,layout,metastore,storage,transport/protocol}`、`third_party/{netpoll,shmipc-go}` |
| `internal/storage` | `internal/{aio,bufpool,device,ierr,layout,metastore}` |
| `internal/rpcclient` | `internal/{ierr,metastore,transport}` |
| `internal/transport/protocol` | `internal/ierr`、`third_party/netpoll` |
| `internal/device` | `internal/{aio,bufpool,ierr,layout}` |
| `internal/metastore` | `internal/{ierr,layout}` |
| `internal/cluster` | 无（叶子包） |
| `internal/{aio,benchkit,bufpool,ierr,layout,version}` | 无（叶子包） |
| `pkg/taihu-client` | `internal/cluster`、`internal/rpcclient`、`internal/version` |

两条关键事实逐条核过：

- **`internal/cluster` 是叶子包** —— `grep -rn '"github.com/liucxer/taihu/' --include='*.go' internal/cluster/` 无输出。
- **`pkg/taihu-client` 非测试代码只 import 三个内部包** —— `grep -rn '"github.com/liucxer/taihu/' --include='*.go' pkg/ | grep -v '_test.go'` 的全部命中是 `internal/cluster`（config.go:13、index.go:7、ops.go:7、picker.go:7、reexport.go:4、registry.go:8、storage.go:11、tikv.go:8）、`internal/rpcclient`（reexport.go:5、storage.go:12）、`internal/version`（storage.go:13）。

---

## 3. 反向依赖被刻意断开时，要写清理由

不是所有"能连上"的边都该连。`internal/metastore/meta.go:112-122` 是一个范例：wire 结构与领域态结构字段平齐，但**刻意不让 `internal/transport/protocol` import `internal/metastore`**。

```go
// 编解码另有一份 wire 结构：internal/transport/protocol.SegmentEntry，字段相同
// 但 State 是 uint8。**刻意不让 protocol import 本包** —— 本包依赖
// github.com/cockroachdb/pebble，而 protocol 是一个只依赖 encoding/binary 的
// 纯 codec（带表驱动单测），把持久化模型拖进编解码层不划算；领域态与 wire 态
// 本就是两个东西，转换发生在服务端边界（internal/transport/server_admin.go）
// 是合理的。两份结构的字段平齐性由 protocol_test.go 的 TestSegmentWireParity 守着。
```

实测印证：`internal/transport/protocol/` 非测试只有一个文件 `protocol.go`，它只 import `internal/ierr`（`protocol.go:26`）与 `third_party/netpoll`（`protocol.go:24`），`metastore` 只出现在 `_test.go` 里。平齐性由 `internal/transport/protocol/protocol_test.go:28` 的 `TestSegmentWireParity` 守着。

**这是本仓库处理"该不该加一条依赖边"的定式**：写清为什么**不**连，以及拿什么替代（边界转换 + 一致性测试）。

---

## 4. 跨包类型可见性由外部测试包兜底

`internal/` 下的类型外部模块无法命名，所以 `internal/rpcclient/reexport.go:24-25` 定了一条维护约定：签名里出现新 internal 类型而忘了补 alias 时**编译不会报错**，只是包对外悄悄不可用 —— 因此用外部测试包逐个命名这些类型作为编译期兜底。

实测：`internal/rpcclient/api_test.go:8` 以 `package rpcclient_test`（外部测试包）import `github.com/liucxer/taihu/internal/rpcclient` 自身。补 alias 的具体规则见 [error-model.md](./error-model.md)。
