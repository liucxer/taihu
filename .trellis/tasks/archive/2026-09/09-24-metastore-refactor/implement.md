# 执行计划

1. **测试合并**：将 store_test.go、compact_test.go、segments_test.go、meta_test.go、
   cache_test.go 的 package/imports 剥离、正文拼接为 `metastore_test.go`（总入口注释
   说明"本层全部测试单文件"，同 aio/device 先例——但此处非同平台拆分，是同层合并）；
   goimports 收敛 import；删除原 5 文件。
2. **导出面收敛（prd 需求 3+4）**：
   a. 新建 `metastore.go`，把全部导出顶层声明（`Store`/`Open`/`ObjectMeta`/`SegmentMeta`/
      `SegmentState`+常量/`PutMappingItem`/`AllocResult`/…）**集中迁入**（store.go 随之还原为
      仅接口文件或并入——目标是非 metastore.go 无导出声明）；
   b. `go doc`/grep 枚举 metastore 导出符号 → 全仓 grep 外部引用审计；
   c. 无外部引用的导出符号降级为私有（含 `PutMappingItem`/`AllocResult` 等）；
   d. `Store` 接口方法、`Open`、透出类型面保持导出；
   e. 编译回归逐包确认无外部包引用被降级符号。
3. **校对**：`gofmt -l`；`go vet ./internal/metastore/ ./internal/storage/ ./internal/rpcclient/ ./pkg/taihu-client/ ./cmd/...`；
   `go test ./internal/metastore/ ./internal/storage/ ./internal/rpcclient/ ./pkg/taihu-client/ -count=1`。
4. **提交推送**：`refactor(metastore): 测试收敛为单文件 + 导出面按外部调用收敛`；
   design.md 梳理随任务 bookkeeping 归档（/trellis-finish-work）。

## 验证命令
- `go build ./...`
- `go test ./internal/metastore/ -count=1`
- `go test ./internal/storage/ -count=1`（消费方回归）
- 导出面 grep 断言：被降级符号无外部引用