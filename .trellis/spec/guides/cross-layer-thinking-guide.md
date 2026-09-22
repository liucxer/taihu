# 跨层与跨边界

> 本指南管一件事：**一次改动要穿过几层**。层内怎么写由各层规则负责，这里只回答「我改了这头，另一头在哪」。
>
> 下面每条都指回本仓库真实发生过的漏改或真实存在的链，不是通用建议。

---

## 1. 本仓库有哪几层，方向不变量是什么

不重述 —— 规则与门禁见 [layering.md](../architecture/layering.md)。这里只留最短的辨认法：

```
cmd/taihu  →  internal/{transport,rpcclient,benchkit}  →  internal/{storage,cluster,device,aio,...}  →  internal/{layout,ierr}
                pkg/taihu-client  ↗（只能走 rpcclient/cluster/metastore/ierr/version）
```

**判据**：改完之后 `make check` 的 `check-layering` 与 `check-sdk-only` 都还要是 `OK`（`Makefile:35-36` 写明了两条各自防的是什么）。

## 2. 一条 flag 的全链：五段，少接一段不报错

`-batch` 这个 flag 从命令行到真正干活的 goroutine 要穿五段：

| # | 段 | 位置 |
|---|---|---|
| 1 | 注册 | `cmd/taihu/cmd/server.go:291-292` `f.Int("batch", 0, ...)` |
| 2 | 读取 | `cmd/taihu/cmd/server.go:212-213` `cmd.Flags().GetInt("batch")` |
| 3 | 装进配置 | `cmd/taihu/cmd/server.go:215-216` `transport.PipelineConfig{ReadBatch: batchTarget}` |
| 4 | 构造 | `internal/transport/batch.go:48` `newPipeline` → `:78` `newBatchWriter` |
| 5 | 起 goroutine | `internal/transport/batch.go:90-93` |

**为什么值得记**：第 1、2 段的失败方式是**静默**的。flag 注册了但没人读（第 2 段断）→ 用户设了没反应；flag 没注册但有人读（第 1 段断）→ `GetInt` 取回默认值 0，同样没反应。两种都不编译报错、不运行报错。

同形的一次真实事故记在 `code-reuse-thinking-guide.md` §2（`64eaec3`，两个 bench 命令里只有一个接上了 `--pipeline`）。

**结论**：新增一个可配参数时，**五段都要点名确认**，而不是「肯定传下去了」。

## 3. 一条 error 跨进程要穿过的 5 层

这是本仓库最完整的一条跨层数据流，改动错误体系前必须整条看一遍：

```
internal/ierr/ierr.go:9-23          唯一事实源：ErrNotFound / ErrInvalidRange / ...
      ↓ 库内直接返回
internal/storage/…                  返回 ierr.ErrXxx
      ↓ 服务端出口，只此一次转换
internal/transport/protocol/protocol.go:113   MapStorageErr(err) → ErrCode
      ↓ 线上只传 code，不传字符串
      帧（OpResp / OpGetErr + EncCode）
      ↓ 客户端入口，只此一次转换
internal/transport/protocol/protocol.go:129   MapCode(code) → error
      ↓ 转出给 SDK
internal/rpcclient/reexport.go:31-42         re-export 成 rpcclient.ErrXxx
      ↓
pkg/taihu-client/reexport.go                 再转指一层
      ↓
外部调用方 errors.Is(err, taihuclient.ErrNotFound)
```

三条要点：

- **服务端只调 `MapStorageErr`、客户端只调 `MapCode`**，不得反向。调用点实测：`MapStorageErr` 在 `internal/transport/server.go:226` 等 8 处与 `server_admin.go`、`server_shm_linux.go`；`MapCode` 只在 `internal/transport/client.go:106` 等 4 处与 `client_admin.go`、`client_shm_linux.go`。
- **跨进程只传 code**，所以新增一个库错误时，只改 `ierr` 是不够的 —— 必须同时在 `protocol.go` 的映射里给出它的 code，否则它会退化成「未知错误」。
- **`ierr.ErrConflict` 刻意不在链上**：它是 compaction 内部的 CAS 控制信号，`internal/rpcclient/reexport.go:30` 明确排除。加错误时要判断「这是客户端会收到的，还是内部信号」。

## 4. 一个枚举值的全链：在磁盘上有位置

`SegmentState` 不只是一个 Go 常量 —— 它在磁盘上占一个字节：

| # | 层 | 位置 |
|---|---|---|
| 1 | 类型定义 | `internal/metastore/meta.go:74` `type SegmentState uint8` |
| 2 | 持久化 | `internal/metastore/meta.go:94` `b[0] = metaVersion`，`:106` `SegmentState(b[1])` 按**固定字节偏移**解出 |
| 3 | 状态机 | `internal/metastore/segments.go:93-99`（`Compacting` → `Full` / `Reclaiming` 的重启自愈分支） |
| 4 | 对外 | `internal/rpcclient/reexport.go:54` `type SegmentState = metastore.SegmentState`、`:58` 起的常量 |

**所以「加一个段状态」不是加一行常量**：第 2 段决定它能不能被解出来，第 3 段决定它重启后自不自愈，第 4 段决定 SDK 用户能不能命名它。漏掉第 4 段时**编译不会报错**，只是包对外悄悄不可用 —— 这正是 `internal/rpcclient/api_test.go` 用外置测试包逐个命名这些类型的原因。

## 5. 磁盘格式的版本字节：一个写了但没人读的契约

真实缺陷，本轮只记录不改码。

`internal/metastore/meta.go:14` 定义了 `metaVersion = byte(0)`，三处 encode 都把它写进去了（`:30` ObjectMeta、`:57` WriteCursor、`:94` SegmentMeta）。但解码侧**从不读它**：

```go
func decodeObjectMeta(b []byte) (ObjectMeta, error) {
	if len(b) != objectMetaLen {
		return ObjectMeta{}, fmt.Errorf("taihu: bad ObjectMeta length %d", len(b))
	}
	return ObjectMeta{
		SegmentID: int64(binary.LittleEndian.Uint64(b[1:9])),   // b[0] 是版本位，直接跳过了
		...
```
（`internal/metastore/meta.go:37-46`）

`grep -n metaVersion internal/metastore/meta.go` 只有 4 行命中：1 处定义 + 3 处写入，解码路径**零使用**。

**后果**：长度校验（`:38-41`）能在字段增删时拦住；但**同长度的字段重排或语义变更拦不住** —— 旧数据会被静默按新布局解读，读出错值还不报错。`meta_test.go` 的 round-trip 测试兜不住它：两侧用同一份代码、同一个版本，永远自洽。

**为什么放进本指南**：这是「我以为两边都改了」这类判断失灵的实证。改任何磁盘格式或线上格式之前，**先去解码侧搜一遍**，不要相信写入侧改了就等于契约成立。

## 6. 缓冲的所有权是跨层的

`bufpool` 借出的缓冲要穿过多层才被归还，归还责任在调用方：

- 契约写在包文档里 —— `internal/bufpool/bufpool.go:5-8`（`Get` 返回 4K 对齐、len 为 2 的幂；`Put` 按 cap 归一化回整桶）。
- 存活期约束写在 `internal/aio/aio.go:20-21`：「buf 在 Submit 后、对应完成事件被 Wait 取回前必须保持存活且不被改写」。
- 实测调用方分布：`internal/device/device.go` 12 处、`internal/transport/{frame,client,server}.go` 各 3 处、`internal/storage/` 2 处、`internal/rpcclient/storage_rpc.go` 1 处。

**改动读路径时，跨层的不是函数调用而是所有权** —— 谁 Get 的、在哪一层归还、归还前有没有人还持有引用。详见 [buffer-and-concurrency.md](../engine/buffer-and-concurrency.md) 第一节。

## 7. 平台分裂：同语义承诺是跨层的

`_linux.go` / `_other.go` 不是「一份实现 + 一个桩」，而是**同一个契约的两份实现**。调用方（`cmd/`）因此不需要 build tag，只做一次运行期判断。

规则与判据见 [file-splitting.md](../platform/file-splitting.md)。这里只提醒跨层的部分：**在 `_linux.go` 里加一个导出符号时，先问 `_other.go` 要不要同名的** —— 判断标准是「非 Linux 上有没有调用方」，不是「这个平台支不支持」。

## 8. 改一层之前，先找契约的另一半

一张查表：

| 我改了 | 另一半在哪 |
|---|---|
| `internal/ierr` 的错误 | `internal/transport/protocol/protocol.go` 的 code 映射 + `internal/rpcclient/reexport.go` |
| 导出签名里新增了 `internal/` 类型 | `internal/rpcclient/reexport.go` 补 alias（**漏了不报错**，见 `api_test.go` 的编译期兜底） |
| 线上帧的布局 | `internal/transport/frame.go` 的读写两侧 + `internal/transport/protocol/protocol.go` 的常量 |
| 磁盘格式 | 同文件的 encode **与** decode 两侧；先看 §5 |
| 段状态机 | `internal/metastore/segments.go` 的重启自愈分支（§4 第 3 段） |
| 平台专有实现 | 成对的 `_other.go`（§7） |
| `bufpool` 的契约 | 所有 Get/Put 调用层的归还路径（§6） |

---

## 相关

- 规则本体：[layering.md](../architecture/layering.md)（方向不变量）、[error-model.md](../architecture/error-model.md)（错误体系）、[wire-protocol.md](../transport/wire-protocol.md)（线上格式）
- 重复与复用：`code-reuse-thinking-guide.md`
- 平台分裂：[file-splitting.md](../platform/file-splitting.md)
