# 命令输出与退出码

> CLI 的对外契约：退出码只有 0/1、错误经唯一出口打到 stderr、正常输出分「人类可读」与 `--json` 两条通道，压测报告格式由 `internal/benchkit` 统一。

---

## 退出码与错误出口

规则 1：**退出码只有 0（成功）与 1（命令返回 error）两种，由 `run()` 一处决定。** `main.go:15-24`：

```go
func run(args []string) int {
	if args == nil {
		args = os.Args[1:]
	}
	os.Args = append([]string{os.Args[0]}, args...)
	if err := cmd.Execute(); err != nil {
		return 1
	}
	return 0
}
```

`main` 只做 `os.Exit(run(os.Args[1:]))`（`main.go:26-28`）。把它抽成函数的理由写在 `main.go:12-14`：让"参数装配 + 退出码"可被单测覆盖；用例见 `main_test.go:38-46`（成功 → 0）与 `main_test.go:49-57`（未知 flag → 1 且 stderr 含 `taihu:`）。**不要新增其它退出码，也不要在子命令里调 `os.Exit`。**

规则 2：**错误只在 `Execute` 打印一次，格式 `taihu: <err>`，落 stderr。** `root.go:76-82`：

```go
func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "taihu:", err)
		return err
	}
	return nil
}
```

配套 `root.go:71-72` 的 `SilenceUsage: true` + `SilenceErrors: true`：cobra 不再自己打印错误与整页 usage，一次失败只出一行。因此**子命令内一律 `return fmt.Errorf(...)`，不要顺手 `fmt.Fprintln(os.Stderr, ...)`**——那会变成第二份错误输出（现有命令的写法见 `cluster.go:48-51`、`key.go:37-41`、`bench_storage.go:105-109`）。

规则 3：**唯一的例外是 `printJSON` 的序列化失败**：它直接写 stderr 并 `os.Exit(1)`（`helpers.go:119-123`），跳过 `run()` 返回路径，但退出码与规则 1 一致。属于既有实现，新增输出路径不要效仿，改为返回 error。

---

## 输出纯净：压掉第三方日志

规则 4：**tikv client-go 的 `pingcap/log` 被压到 ErrorLevel，CLI 输出保持纯净。** `root.go:18-22`：

```go
// silenceTiKVLog 抑制 tikv client-go（pingcap/log）的 INFO/WARN 刷屏：CLI 输出
// 保持纯净，连通性失败由命令自身报错。
func init() {
	log.SetLevel(zapcore.ErrorLevel)
}
```

（import 见 `root.go:10` 的 `"github.com/pingcap/log"` 与 `root.go:12` 的 zapcore。）理由是连通性失败由命令自身报错（见规则 2），日志再刷一遍只是噪音。**日志的全局三通道约定见 [`.trellis/spec/architecture/code-style.md`](../architecture/code-style.md) 的「日志三通道」（该文件 `:103`）；本层在此之上只加一条：一次性命令不要新增 `log.Printf` 诊断输出，需要报错就 `return error`，stdout 只留给命令结果。**

规则 5：**`server` 是例外——它用标准库 `log`（`server.go:11`），不是 pingcap shim。** server 是长跑 daemon，启动信息与 `[stat]` 统计照打且带标准库时间前缀（如 `2026/09/21 16:58:53 taihu: ...`）。规则 4 的"纯净"约束的是一次性命令，不要据此去改 server 的日志。

---

## server 的 `[stat]` 周期统计

规则 6：**server 每 5 秒打一组 `[stat] ` 前缀的统计行，供 grep 过滤；新增周期诊断必须沿用该前缀。** `server.go:239-253`：

```go
go func() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		io4M, ioOther, b4M, bOther := st.IOStats()
		log.Printf("[stat] disk-io 4MiB=%d other=%d bytes4MiB=%d bytesOther=%d", io4M, ioOther, b4M, bOther)
```

三行格式串分别是 `[stat] disk-io 4MiB=%d other=%d bytes4MiB=%d bytesOther=%d`（`server.go:245`）、`[stat] segments free=%d active=%d full=%d reclaiming=%d`（`server.go:246-250`）、`[stat] %s`（`server.go:251`，内容来自 `transport.StatsString()`）。用途在 `server.go:239` 的注释：验证磁盘/回帧是否整块 4MiB。pprof 同样自动分配端口、由另一个 goroutine `Serve`（`server.go:231-238`），且与 RPC 端口互斥（`server.go:102` 用 `pickPort` 排除已占的 rpcPort）。

---

## 机器可读输出（`--json`）

规则 7：**`--json` 是根级全局开关，由 `printJSON` 一处实现。** 开关在 `root.go:92`（`pf.BoolVar(&global.json, "json", false, "机器可读 JSON 输出")`）；`helpers.go:117-125` 用 `json.MarshalIndent` 缩进 2 空格打到 stdout。

规则 8：**每个命令先写 `if global.json { printJSON(...); return nil }`，再写人类可读分支。** JSON 结构体用带 tag 的匿名 struct（`cluster.go:54-66`、`instance.go:23-29`、`client.go:57-69`），保证字段名稳定；不要用 map 拼。现有 11 处调用点见 `cluster.go:81`、`cluster.go:182`、`cluster.go:246`、`key.go:160`、`key.go:252`、`key.go:284`、`key.go:319`、`key.go:361`、`instance.go:88`、`client.go:88`、`client.go:187`；另有 2 处是分流判断而非调用（`key.go:186` 的互斥校验、`key.go:221` 的 `file != "" || global.json`）。**新增命令漏写 JSON 分支属于不完整实现。**

规则 9：**二进制数据与 JSON 元信息不能共用 stdout。** `key get` 直接拒绝 `-json` 不带 `-file` 的组合（`key.go:186-188`：`get -json 需配合 -file（JSON 元信息与二进制数据不能混用 stdout）`）。落地方式见 `key.go:206-225`：对象数据写 `-file` 或 stdout；元信息在有 `-file`（或 `-json`）时走 `printJSON`，否则退到 stderr（`key.go:224` 的 `fmt.Fprintf(os.Stderr, "get %q: %d bytes\n", ...)`）。

---

## 人类可读输出

规则 10：**表格用定宽 `%-Ns` 格式串 + 一行表头；字节数一律过 `humanBytes`。** 表头+数据行见 `cluster.go:89-95`，`instance.go:98-110`、`client.go:96-101` 同形。`humanBytes` 在 `helpers.go:128-139`（GB/MB/KB 两位小数，默认 `%d B`），`key.go:288` 与 `cluster.go:94` 都用它——**不要自己除 1024**。空结果打一行说明而不是空表（`cluster.go:85-88` 的 `no instances registered`、`client.go:92-95`）。

---

## 压测报告输出（benchkit）

规则 11：**`benchkit.Store` 是压测数据面的唯一抽象，执行循环、进度、汇总报告都在 `internal/benchkit`，新压测工具不要另抄一份。** 接口定义 `internal/benchkit/run.go:36-43`（`Put`/`Get`/`Delete`/`Close`），公共参数 `Config` 与校验 `run.go:16-34`、`run.go:46-63`，区间切分 `run.go:70-80`（"每 key 恰好被一个 worker 处理一次"），执行入口 `benchkit.Run`（`run.go:84-112`），`Pipeline>1` 时改走 `runPipelinedWorker`（`run.go:117-118`）。

规则 12：**报告标题与进度行的格式由 benchkit 统一，工具名与端点由调用方传入。** 标题 `==== <toolName> <mode> ====`（`run.go:74`），调用侧传 `"taihu bench single"`（`bench_single.go:88`）与 `"taihu bench cluster"`（`bench_cluster.go:122`）；`endpoint` 标明被测端点，构造方式见 `bench_single.go:82-87`（`single(rpc:<addr>)` / `single(shm:<path>)`）与 `bench_cluster.go:123`（`cluster(client=...,transport=...)`）。进度行 `  %s: %d/%d (%.1f ops/s)`（`report.go:42`），汇总依次打 `==== 标题 ====`、`endpoint=... size=... threads=... count=...`、`objects=... bytes=... elapsed=...`、`throughput: ... ops/s ... MiB/s` 四行（`report.go:74-77`），再按需打 `latency: p50=... p90=... p99=...`（`report.go:86-88`）。压测结束额外打印客户端收帧统计 `==== frame stats ====`（`bench_single.go:95`、`bench_cluster.go:130`）。

规则 13：**`bench storage` 是唯一不走 benchkit 的压测命令，它自带同构实现；这是签名不匹配导致的既有重复，不要产出第三份副本。** `bench_storage.go:283-359` 的 `progress`/`latencyCollector`/`report` 与 `report.go:13-90` 同构。从签名看原因是数据面不匹配：它直连 `storage.Storage`（`internal/storage/storage.go:83` 的 `Put`、`:232` 的 `ReadAt` 返回 `([]byte, error)`），而 `benchkit.Store.Get` 要求 `([]byte, func(), error)`（`run.go:39`，`rpcclient.Storage` 的实现见 `internal/rpcclient/storage_rpc.go:57`）。新增压测工具前先判断数据面能否适配 `Store`：能就复用 `benchkit.Run`，不能就先讨论适配方案。

规则 14：**压测结论落到 `doc/性能测试报告/`，文件名 `<YYYYMMDDHHMM>_<commit7>_<标题>.md`**（例：`doc/性能测试报告/202609130703_c8336a2_taihu-storage-bench_4M_IO_读写性能测试报告.md`）。`doc/README.md:48` 明确"**报告文件不移动、不重命名**"——移动会打断报告之间与索引之间的相对链接；新报告按同一命名规则追加，并同步 `doc/README.md` 的系列索引（`doc/README.md:45-47`）。
