# internal/storage 重构建议（golang 专家视角）

> 勘察范围：storage.go / options.go / compact.go / admin.go + 5 个测试文件 + 调用方
> （transport/server.go、transport/batch.go、transport/server_shm_{linux,other}.go、
> cluster/kv.go、cmd/taihu/cmd/server.go、cmd/taihu/cmd/bench_storage.go）。
> 结论先行：**层次本身是健康的**（storage 编排、device 裸盘、metastore 元数据、layout 布局，
> 无循环依赖），不需要推倒重来；值得做的是「一处 bug + 三处重复 + 文件/测试收敛」。

## 1. 现状评估

### 1.1 分层与依赖（健康）
```
transport / cluster / cmd
        │  调用 Storage 公开方法
        ▼
internal/storage (Storage)
        │  读路径 Ref/Unref、对齐吸收、批读组装
        ▼
metastore.Store ＋ device.Device ＋ layout.Layout
```
- 职责：Storage 吸收 O_DIRECT 对齐细节与段快照语义，transport 只管帧编解码——边界正确。
- 无循环依赖；`options.go` 的 functional options 模式无需改。

### 1.2 问题清单（按严重度排序）

| # | 严重度 | 问题 | 位置 |
|---|---|---|---|
| P1 | 高（空间泄漏） | **BatchRead Phase 1 早退不释放已 Ref 段**：block 0 通过校验并 Ref，block 1 校验失败 `return err` 时，block 0 的 Ref 永久泄漏 → 段 AliveCount 归零也无法 Reclaiming→Free 复用 | storage.go L382-397 |
| P2 | 中（可读性） | 读路径三份重复：ReadAtMeta / ReadAtIntoMeta / BatchRead 各自实现「校验 → 截断 want → 对齐 dstart/dlen/skip → Ref/Unref」 | storage.go L257-283 / L318-341 / L379-403 |
| P3 | 低（冗余调用） | Storage.Delete 先 GetMapping 再 DeleteMapping，而 metastore.DeleteMapping 内部已查一遍映射 | storage.go L449-454 |
| P4 | 低（单文件过大） | storage.go 484 行混装 5 类职责（生命周期/写/读/批量/管理透传） | storage.go |
| P5 | 低（测试组织） | 5 个测试文件，"extra" 后缀历史遗留、主题重叠（storage_test+storage_extra_test 等） | *_test.go |

### 1.3 观察（不建议动的）

- `PutBegin/PutAppend/PutCommit`、`BatchAppend/BatchPutCommit` 对 transport 暴露分段原语——shm 零拷贝流水线依赖，保留。
- `moveObject` 的 orphan 块靠 GC 回收——设计内行为，保留。
- 公开 API 签名（IOStats 返回 4 值、GetDiskCapacity 返回 3 值）对象化收益低、牵连 cluster/cmd 调用点——本轮不做。

## 2. 重构建议（按档位）

### 档位 A：最小正确性（修复 P1，必做）
- **BatchRead 两遍法**：第一遍全量校验 + 计算全部 devJob/ref 计划（不 Ref）；第二遍统一 Ref → 提交 → 统一 Unref（defer 保护）。早退路径天然零泄漏。
- 回归单测：构造「第 1 块合法、第 2 块 Dst 不足」的 blocks，断言执行后段引用计数为 0（用 metastore Segments 的 AliveCount/引用表或可控的 Reclaim 验证）。

### 档位 B：读路径去重（P2）
- 私有助手统一「对象窗口 → 物理区间」：
  ```go
  // readWindow 校验并计算物理读区间（含剩余截断）。
  // 返回 relStart/dlen/want/skip；off 越界返回 ErrInvalidRange，空窗口返回 io.EOF。
  func readWindow(meta metastore.ObjectMeta, off, size int64, alignedOff bool) (relStart, dlen, want, skip int64, err error)
  ```
- ReadAtMeta / ReadAtIntoMeta / BatchRead 全部改用它；io.EOF、短读截断、非对齐窗口原址平移的语义保持逐字等价（现有测试全覆盖兜底）。

### 档位 C：文件与测试收敛（P3-P5）+ 用户三条补充
- **storage.go 拆文件**（同包同文件族，行为零变）：
  - `storage.go`：**导出面 hub**（Storage 类型 + NewStorage/Close/MaxObjectSize/LoadCache +
    全部 public 顶层声明：BatchReadBlock/BatchedReadResult/Compactor/CompactorConfig/Option/
    NewCompactor/DefaultCompactorConfig/WithAIOMode/WithAIOIOPoll）+ 管理透传方法
  - `write.go`：写路径 Storage 方法（Put 族/Batch*/Delete，均非顶层、可留在本文件）
  - `read.go`：读路径 Storage 方法（Meta/ReadAt 族/BatchRead）+ 私有 readWindow 助手
  - `compact.go` / `admin.go` / `options.go`：只留私有实现（方法 + 私有结构），**无导出顶层声明**
- **Delete 去冗余**：`Delete` 直接调 `s.db.DeleteMapping`（缺失已返回 ErrNotFound）。
- **导出面收敛（默认私有）**：审计 = transport/server.go、server_admin.go、server_shm_linux.go、
  batch.go + cmd/server.go、root.go、bench_storage.go 生产代码 grep 结果：
  - 降级私有：`BatchPut` / `PutItem` / `ReadAtInto`（无外部调用；测试同包可继续使用）；
  - 保持 public：上述 R6 清单；对外桶型 `BatchReadBlock` / `BatchedReadResult` 因
    BatchRead（transport 用）与 server_shm_linux.go:136 显式 `storage.BatchReadBlock{...}` 而保留。
- **测试收敛**：5 个测试文件（含历史遗留 `_extra` 后缀）**全部合并为 1 个 `storage_test.go`**
  （metastore 同层合并先例）；函数名冲突以描述性改名消解。
- 名字不改：包与类型 `storage.Storage` 的 stutter 属 Go 惯例，改包名收益 < 成本。

### 档位 D（可选，需用户点头才做）
- 公开 API 返回对象化（IOStatsResult / DiskCapacity 结构体）——牵连 cluster/kv.go 注册与 cmd，收益主要是可读性。
- 基准/压测相关（bench_storage.go）函数复用 storage 分解原语——不属本任务。

## 3. 兼容性与风险

- 磁盘格式、元数据结构、对外 API 签名全部不变 → 无数据迁移、无传输层改动。
- 风险集中在「读路径重构改变边界语义」（对齐/skip/EOF）——B 档依赖现有 365+516 行读路径测试做行为等价兜底；若测试覆盖有缺口，先补齐再重构。
- BatchRead 泄漏修复属于净修复，无兼容影响。

## 4. 建议落地顺序（给 implement.md 用）

1. 写 BatchRead 泄漏回归单测（红）
2. 修 BatchRead（两遍法，去 leak，单测转绿）
3. 抽 readWindow，重构三个读方法的公共段（现有测试全绿）
4. Delete 去冗余
5. storage.go 拆文件（write.go/read.go；storage.go 收敛为导出面 hub）
6. 导出面降级：`BatchPut`/`PutItem`/`ReadAtInto` → 私有；Option/Compactor 等顶层导出迁入 storage.go
7. 测试合并：5 个测试文件 → 1 个 storage_test.go（_extra 后缀文件合并，函数名冲突改名）
8. 导出面断言 grep；gofmt / go vet / 全量相关包测试

## 5. 决策确认（2026-09-24）

- 范围 = **A+B+C**（P1 bug + 读路径去重 + 文件/测试收敛），且按用户 PRD 补充：
  测试单文件、导出面默认私有、public 全集中 storage.go。
- 档位 D（公开 API 返回对象化）不做。