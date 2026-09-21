# 端到端测试（test/e2e/）

> L1 端到端套件：起真实 `taihu server`（losetup 裸块设备 + 真 TiKV），由直连数据面客户端与集群 SDK 两条路径驱动；全体被 `//go:build e2e` 门控，不设 `E2E_PD` 则全部 Skip。

---

## 规则 1：整体被 `//go:build e2e` 门控，`doc.go` 例外

`test/e2e/` 下 8 个文件全部以 `//go:build e2e` 开头（`test/e2e/harness_test.go:1`、`a_data_test.go:1` … `g_soak_long_test.go:1`）。唯一不带 tag 的是 `doc.go`，理由写在它自己里面（`test/e2e/doc.go:3-4`）：

```go
// 本目录下除本文件外的测试文件全部带 `//go:build e2e` 构建标签；本文件不带标签，仅用于
// 保证该目录在 `go build ./...` / `go test ./...`（无 e2e 标签）下仍有可编译文件。
```

新加 e2e 文件时**必须**带 tag，并且不能删掉 `doc.go` —— 否则 `go build ./...` / `go test ./...`（单测口径）会因为该目录无文件而报错。

## 规则 2：文件按字母分组，每个文件头必须写「断言什么、不断言什么」

| 文件 | 组 | 主题 |
|---|---|---|
| `a_data_test.go` | A | 数据正确性（最高优先级） |
| `b_transport_test.go` | B | 传输面（auto/rpc/shm 三路径、多 IP、多路复用、半包） |
| `c_registry_test.go` | C | 注册 / 选路面（心跳摘除、容量、优雅停机、写路由、KV 降级） |
| `d_lifecycle_test.go` | D | 生命周期 / 崩溃恢复 / 段水位 |
| `e_concurrency_test.go` | E | 并发/竞态（不撕裂） |
| `f_soak_test.go` | F | 长稳 + 性能回归（60s 口径） |
| `g_soak_long_test.go` | G | 长稳（3h 口径） |
| `harness_test.go` | — | 共享 harness（不带用例） |

文件头的中文分组宪章是硬要求，**最关键的写法是明确「不做哪种断言」并给出理由**。`test/e2e/f_soak_test.go:7-9`：

```go
// 重要前提（决定了本组能断言什么）：e2e 的块设备是 `losetup` 挂在 /var/tmp 稀疏文件上的
// loop 设备，性能远低于真实 NVMe 且抖动大，因此**不做绝对带宽断言**。本组只做可在 loop
// 上稳定复现的回归护栏：
```

同理 `test/e2e/g_soak_long_test.go:24` 再强调一次（`断言口径（loop 设备抖动大，不做绝对带宽断言）`），并把硬断言/趋势断言/软记录三档分开列（`:25-28`）。另一个「不断言什么」的例子是 B 组：`test/e2e/b_transport_test.go:10-11` 指出进程内帧统计是**进程级全局计数器**，故全部断言一律用「前后差值」，绝不假设绝对值。

新写用例时照这个格式：在文件头写清楚本组覆盖哪几个编号、为什么这组的断言口径与别组不同、哪些量在这台机器上不可断言。

## 规则 3：测试名 `<字母><数字><CamelCase>`

真实命名（`grep '^func Test' test/e2e/`）：`TestA1Block4MiB`、`TestA2SizeMatrix`、`TestB6PartialFrame`、`TestC4WriteRoutingLocalVsRoundRobin`、`TestD4SegmentWatermarkReclaim`、`TestE1ConcurrentSameKeyNoTear`、`TestE1MultiChunkNoTear`、`TestF3AIOBackends`、`TestG1LongSoakStability`（`test/e2e/a_data_test.go:23`、`:38`、`test/e2e/b_transport_test.go:637`、`test/e2e/c_registry_test.go:354`、`test/e2e/d_lifecycle_test.go:368`、`test/e2e/e_concurrency_test.go:67`、`:197`、`test/e2e/f_soak_test.go:671`、`test/e2e/g_soak_long_test.go:261`）。数字是组内序号，与文件头的分组宪章编号一一对应。

少数不走这条命名的是「SDK 路径整体复走」类用例：`TestASDKPath`、`TestASDKSourceRebuild`（`test/e2e/a_data_test.go:278`、`:338`）—— 它们没有组内序号，因为不新增断言口径，只是换客户端路径复跑 A 组关键项。

## 规则 4：环境变量与门控集中在 `harness_test.go` 顶部

全部环境变量只有 7 个，声明在文件头注释里（`test/e2e/harness_test.go:8-17`）：

```go
// 环境变量（在集群节点上跑；未设 E2E_PD 时全部用例 t.Skip，不会误报失败）：
//
//	E2E_PD        TiKV PD 地址列表（逗号分隔），如 100.71.128.12:2379。必需。
//	E2E_BIN       taihu 二进制路径；缺省在 E2E_WORKDIR 下 `go build ./cmd/taihu`（仅构建一次）。
//	E2E_WORKDIR   用例工作目录根；缺省 /var/tmp/e2e-taihu（必须是本机真实文件系统，
//	              勿用 /tmp —— 许多节点 /tmp 是 tmpfs，O_DIRECT 打不开）。
//	E2E_DEV       复用给定块设备（如 /dev/loop9）；缺省每个 server 用 losetup 把
//	              E2E_WORKDIR 下的稀疏文件挂成独立 loop 块设备（真块设备语义：
//	              BLKGETSIZE64 / O_DIRECT / O_EXCL 均可用）。
//	E2E_DEV_SIZE  稀疏镜像大小（字节），缺省 16GiB（= 2 个 8GiB segment）。
```

**`/tmp` 那条警告是硬约束**（`test/e2e/harness_test.go:12-13`）：`E2E_WORKDIR` 缺省值是 `/var/tmp/e2e-taihu`（`test/e2e/harness_test.go:52`），因为很多节点 `/tmp` 是 tmpfs，`O_DIRECT` 打不开。连 3h 长稳的手工命令也显式带 `TMPDIR=/var/tmp`（`test/e2e/g_soak_long_test.go:39`）。

门控实现：**`E2E_PD` 是唯一必填项，未设直接 Skip 而不是 Fail**（`test/e2e/harness_test.go:62-68`）：

```go
// pdAddrs 解析 E2E_PD；未设置直接跳过（e2e 依赖真实 TiKV，无则不该误报失败）。
func pdAddrs(t *testing.T) []string {
	t.Helper()
	raw := envStr("E2E_PD")
	if raw == "" {
		t.Skip("E2E_PD 未设置：e2e 需要真实 TiKV PD，例如 E2E_PD=100.71.128.12:2379")
	}
```

两个时长变量**相互独立**，各自控制 F1 与 G1：`E2E_SOAK_SECONDS`（`test/e2e/f_soak_test.go:410-413`，缺省 60s）与 `E2E_LONG_SOAK_SECONDS`（`test/e2e/g_soak_long_test.go:262`，缺省 60s，3h 用 `10800`；`test/e2e/g_soak_long_test.go:22` 明确写了二者独立）。两者都走同一个读取器 `fDuration`（`test/e2e/f_soak_test.go:43-53`，非法值 `t.Fatalf`）。

## 规则 5：共享 TiKV 上必须用 `scopedKV` 做租户隔离

e2e 连的是**集群里其它 taihu 实例共用的** TiKV，所以隔离策略写在 `test/e2e/harness_test.go:19-26`：

```go
// 隔离策略：TiKV 与本集群其它 taihu 实例共用，故
//   - 实例名 / 业务 key 前缀 / 工作目录全部带本次随机 tag；
//   - 客户端视角的 KV 用 scopedKV 包一层，把实例清单与索引区扫描限制在本 tag 内，
//     使 SDK 看不到（也不会写坏）同集群的其它实例；
//   - 用例结束（t.Cleanup）停 server、删注册/容量/索引记录、卸载 loop、删工作目录。
//
// 为什么必须有 scopedKV：客户端 instanceRegistry 的 Scan 覆盖整个 /taihu/instances/
// 前缀，不做隔离就会把集群里生产实例一并纳入选路（写路由/水位/跨节点断言全部失真）。
```

`scopedKV` 只包住客户端视角的 KV（`h.kv`），对注册区的直接断言走裸 KV（`h.raw`，`test/e2e/c_registry_test.go:7-8` 说明了这个分工）。它把 `Scan`/`DeleteRange` 与允许区间**求交**而非整体替换，以免吃掉调用方自己的二级前缀（`test/e2e/harness_test.go:203-205`）。

新写用例时：任何键都必须经 `h.key(name)` 加本次 tag 前缀（`test/e2e/harness_test.go:289-290`），SDK 侧一律用 `h.newSDK`（`test/e2e/harness_test.go:356-357`）而不是自己拼 `ClusterConfig`，否则会看到生产实例。

清理动作统一注册在 `newHarness` 里（`test/e2e/harness_test.go:285` 的 `t.Cleanup(h.cleanup)`），用例体内不需要自己收尾。

## 规则 6：e2e 不使用 `-race`

理由写在 E 组文件头（`test/e2e/e_concurrency_test.go:5`）：

```go
// E 组：并发/竞态（不使用 -race：aarch64 上 `go test -race` 链接失败）。
```

因此**竞态检测不能靠工具，只能靠断言不变式**。E 组的做法是把「不撕裂」写成可判定的断言：并发写同一 key 时读到的内容必须逐字节等于**某一次**写入的完整内容，为此各 writer 的内容长度刻意不同（`test/e2e/e_concurrency_test.go:7-9`）。

并发用例里报错用 `t.Errorf` 而非 `t.Fatalf`，并且这条规则被显式写进辅助函数注释（`test/e2e/e_concurrency_test.go:24-25`）：

```go
// eRunPool 以 workers 个并发执行 fn(0..n-1)。fn 内用 t.Errorf 报告（并发安全），
// 不用 t.Fatalf（非并发安全），以保证其余任务继续执行、尽可能多地暴露问题。
```

## 规则 7：运行方式

常规 e2e（命令原文见 `test/e2e/doc.go:8`）：

```bash
E2E_PD=100.71.128.12:2379 go test -tags e2e -count=1 -timeout 30m ./test/e2e/...
```

`-count=1` 是必须的（用例有状态，不吃缓存）。3h 长稳**必须后台化**，因为运行环境（9527 代理 `/exec`）有 1800s 超时，前台跑会被掐断（`test/e2e/g_soak_long_test.go:37-41`）：

```bash
setsid nohup env TMPDIR=/var/tmp E2E_PD=<pd> E2E_LONG_SOAK_SECONDS=10800 \
  go test -tags e2e -run TestG1LongSoakStability -timeout 4h -v ./test/e2e/ \
  > /var/tmp/g1-soak.log 2>&1 < /dev/null &
```

注意这里的 `-timeout 4h` 必须大于 `E2E_LONG_SOAK_SECONDS`（10800s = 3h），且 `TMPDIR` 要显式指到 `/var/tmp`。局部重跑用 `-run <测试名>`（如上），不要为了跑一条用例而削减 `-timeout`。
