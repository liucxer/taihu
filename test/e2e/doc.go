// Package e2e 是 taihu 的 L1 端到端测试：taihu CLI/SDK ↔ taihu server ↔ 裸盘 ↔ 真实 TiKV。
//
// 本目录下除本文件外的测试文件全部带 `//go:build e2e` 构建标签；本文件不带标签，仅用于
// 保证该目录在 `go build ./...` / `go test ./...`（无 e2e 标签）下仍有可编译文件。
//
// 运行方式（在集群节点上，见 harness_test.go 顶部的环境变量说明）：
//
//	E2E_PD=100.71.128.12:2379 go test -tags e2e -count=1 -timeout 30m ./test/e2e/...
package e2e
