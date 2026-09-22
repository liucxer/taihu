# 测试层（单测 + test/e2e）

> 本仓库的测试分两级：与实现同目录的单元测试（`go test ./...` 默认跑），以及需要真机（裸盘 + TiKV）的 `test/e2e/` 端到端套件（`//go:build e2e` 门控，不设 `E2E_PD` 全部 Skip）。

---

## 本层是什么

- **没有独立的测试目录**：单测与实现同目录同包（`internal/storage/storage_test.go` 紧挨 `internal/storage/storage.go`，两边都是 `package storage`）。全仓唯一的外置测试包是 `internal/rpcclient/api_test.go`（`package rpcclient_test`），破例理由见 `unit-tests.md`。
- **唯一的独立测试树是 `test/e2e/`**：8 个测试文件共 4925 行（另有 9 行不带 tag 的 `doc.go`，合计 4934），全部 `package e2e`，按字母分组（`a_data` … `g_soak_long` + `harness_test.go`）。
- **标准库 `testing` 一手到底**：`go.mod:12` 声明了 `github.com/stretchr/testify v1.9.0`，但仓库自有代码（`cmd/`、`examples/`、`internal/`、`pkg/`、`test/`）对它**零引用** —— 13 处引用全在 `third_party/shmipc-go/` 的上游 fork 测试里。断言一律手写 `if got != want { t.Fatalf(...) }`。

## 本层文件

| 文件 | 用途 |
|---|---|
| `index.md` | 本文件：测试分级的边界、两条命令口径、开发前检查与质量校验 |
| `unit-tests.md` | 同目录同包的约定、断言与表驱动形态、无 tag 契约测试模式、平台 build tag 清单、测试辅助函数命名、**按包统计的覆盖率下限 ≥80% 与 `internal/aio` 例外** |
| `e2e-tests.md` | `test/e2e/` 的分组宪章、环境变量与门控、租户隔离、禁用 `-race` 的理由、测试名约定、长稳运行方式 |

（两份分文件而非合并：单测的读者是改 `internal/`/`pkg/` 的开发者，e2e 的读者是要上真机的人，两边的「读前必知」完全不相交。）

## Pre-Development Checklist

- [ ] 新测试文件与实现**同目录同包名**，文件名 `<subject>_test.go`（`internal/storage/storage_test.go:1`、`internal/metastore/meta_test.go:1`）；只有为验证「对外名字可命名」才允许外置包，且需像 `internal/rpcclient/api_test.go:11-20` 那样在文件头写明破例理由
- [ ] 断言手写 `if got != want { t.Fatalf("%s: got %v want %v", ...) }`（`internal/layout/layout_test.go:21-26`），**不要** import testify
- [ ] 逻辑/纯函数测试写成表驱动 `cases := []struct{...}` + `for _, tc := range cases` + `t.Run(`（`internal/transport/protocol/protocol_test.go:81-103`）
- [ ] 需要平台差异的测试：断言体写成**不带 build tag** 的契约测试，后端清单由 `//go:build linux` / `//go:build !linux` 的文件提供；不可用的后端**不静默过滤**，而是让 `new` 回调 `t.Skipf` 带 errno（`internal/aio/aio_backends_linux_test.go:7-11`）
- [ ] 新增 linux-only 测试文件必须带 `//go:build linux`，且 `_test.go` 后缀排在 GOOS 之后；改完跑 `make check-linux`（`Makefile:60-64`）
- [ ] 测试辅助函数带主体前缀（`newTestServer`、`shmDial`、`tcpNewTestStorage`、`assertRoundTrip`），共享 fake 放 `testutil_test.go`（`pkg/taihu-client/testutil_test.go:14`）
- [ ] 新增 e2e 用例：文件按组分字母，文件头写「这组断言什么、不断言什么」宪章；测试名 `<字母><数字><CamelCase>`（`test/e2e/harness_test.go:1` 的 tag 不能漏）

## Quality Check

`Makefile` **没有** `test` target（`Makefile:10` 的 `.PHONY` 只有 `all build clean check check-fmt check-layering check-sdk-only check-linux`），测试命令写在文档与测试文件注释里。

```bash
# 单测：全仓。e2e 目录只有 doc.go 不带 tag，其余文件被 //go:build e2e 挡住，不会被误跑
go test ./...

# 覆盖率：按包统计，目标 ≥80%。两个排除项的理由与 internal/aio 的例外
# 见 unit-tests.md 规则 12
go test -cover $(go list ./... | grep -v third_party | grep -v test/e2e)

# 格式 + 分层不变量 + go vet
make check

# 跨平台编译核对：本机是 darwin，linux 专有代码的错误只有这一步能暴露
# （第三行就是 Makefile:69 的 go test -c，只编译测试二进制不执行）
make check-linux

# e2e：在集群节点上跑（命令原文见 test/e2e/doc.go:8）；不设 E2E_PD 则全部 Skip
E2E_PD=100.71.128.12:2379 go test -tags e2e -count=1 -timeout 30m ./test/e2e/...

# 3h 长稳：必须后台化，前台跑会被 /exec 的 1800s 超时掐断（命令原文见 test/e2e/g_soak_long_test.go:39-41）
setsid nohup env TMPDIR=/var/tmp E2E_PD=<pd> E2E_LONG_SOAK_SECONDS=10800 \
  go test -tags e2e -run TestG1LongSoakStability -timeout 4h -v ./test/e2e/ \
  > /var/tmp/g1-soak.log 2>&1 < /dev/null &
```

`make check` / `make check-linux` 的组成见 `Makefile:21-22` 与 `Makefile:65-70`。这三条本地命令（`go test ./...` / `make check` / `make check-linux`）也是本仓库历史重构的验收口径（`doc/设计文档/20260914_结构评审与优化建议.md:337`、`third_party/README.md:194`）。
