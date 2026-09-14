# third_party —— 两份 fork 的上游基线、本地改动与运维约束

本目录放的是两份被本地打过 patch 的上游库。它们**不是独立子模块**，而是主模块的一部分
（目录下没有 `go.mod`），import 路径为 `github.com/liucxer/taihu/third_party/<lib>`。

## 为什么并进主模块，而不是用 replace 挂进来

早期做法是给两份 fork 各留一个嵌套 `go.mod`，再用 `go.mod` 里的**相对路径 `replace`** 挂进来。
这个做法有个对外致命的性质：

> **Go 的 `replace` 只在主模块生效，不会传递给消费者。**

于是任何外部模块 import 本仓库的 `pkg/`，解析到的都是**上游** netpoll / shmipc-go，因缺少
fork 新增的 API 而在构建期失败。实测（外部模块只加一条 `replace github.com/liucxer/taihu => <本仓库>`，
不给 fork 加任何 replace）：

```
# github.com/liucxer/taihu/internal/transport
conn.go:28:10: undefined: netpoll.SetAlignedAllocator
conn.go:29:10: undefined: netpoll.SetInputAlignedAllocator
conn.go:30:10: undefined: netpoll.SetInputNodeSize
```

也就是说，`pkg/` 承诺的公开 API 在当时是虚构的。并进主模块后这个阻碍被彻底消除，
同时构建也不再依赖本地路径与网络。

**注意：** 这里的关键在于 `internal/transport` 是通过**匿名接口类型断言**使用 fork 新增 API 的：

- `internal/transport/client.go:187`：`msg.r.(interface{ TakeTry() ([]byte, []byte, bool) })`
- `internal/transport/client.go:206`：`msg.r.(interface{ ReadCopy([]byte) (int, error) })`

（写这份文档时这两处在 `conn.go`，该文件此后拆成了 `frame.go` + `client.go`，故路径已更新；
上面那段消费者报错里的 `conn.go:28` 是当时编译器的原始输出，保留原样。）

断言是**结构性**的——上游 netpoll 的 `*UnsafeLinkBuffer` 没有这两个方法，断言恒为 false，
零拷贝路径会静默退化成拷贝路径或直接失败。所以「消费者拿到上游版本」不是编译报错那么简单，
**它是会静默降级的**。这也是并模块必须做、且不能靠「消费者自己也加 replace」绕过的原因。

## 上游基线与许可证

| fork | 上游模块 | 基线版本 | 许可证 | NOTICE |
|---|---|---|---|---|
| `third_party/netpoll` | `github.com/cloudwego/netpoll` | **v0.7.5** | Apache-2.0（`LICENSE` 随附） | ✅ 已补回（82 B） |
| `third_party/shmipc-go` | `github.com/cloudwego/shmipc-go` | **v0.2.0** | Apache-2.0（`LICENSE` 随附） | 上游无 NOTICE 文件 |

Apache-2.0 §4(d)：分发衍生作品时须随附上游 NOTICE。netpoll 上游 v0.7.5 有一份 82 字节的
`NOTICE`，fork 化时被丢掉了，现已补回 `third_party/netpoll/NOTICE`（逐字节与上游一致）。
shmipc-go 上游本身没有 `NOTICE`（只有 `LICENSE` 和 `.licenserc.yaml`），故无需补。

上游 netpoll 另有一份 `CREDITS`（贡献者名单）未随 fork 携带。它是 Go 生态惯例性的
说明文件，不属于 §4 的许可证义务，故不补；如需追溯贡献者请查上游仓库。

## 本地改动清单（相对上游基线逐一核对）

以下清单由 `diff -rq` / `diff -u` 对本机 module cache 里的上游版本实测得出。改动分三类：
**[功能]** 是 fork 存在的原因；**[并模块]** 是把 fork 挂进主模块产生的；**[门禁]** 是并模块后
语言版本升至主模块的 `go1.25`、`go vet` 新报出的问题所需的修复。

### netpoll（基线 v0.7.5）

**[功能]**

- `nocopy.go` —— 新增两个可注入分配器及其开关，patch 的核心：
  - `SetAlignedAllocator(get, put)`：让 netpoll 的节点缓冲来自外部 4K 对齐池（`internal/bufpool`），
    收流缓冲因此 4K 对齐且可池化复用，而非走 netpoll 内部 mcache。新增 `flagAligned` 标记，
    释放时归回该池。
  - `SetInputAlignedAllocator(get, put)`：**仅**用于连接 inputBuffer 节点的**精确尺寸**（== 线上帧长）
    分配器，配合新增 `flagInputAligned`。一帧一节点、ack 后不再写入，这是零拷贝移交安全的前提。
  - `SetInputNodeSize(n)`：给 inputBuffer 节点的自适应容量设上限，使单个节点不超一帧。
  - 三个开关均为「仅影响安装之后创建的节点」，故只应在启动时调用一次；未设置时保持上游行为。
- `nocopy_linkbuffer.go` ——
  - `readCopy` → 导出为 `ReadCopy`，并补 `error` 返回值。原有的 `readCopy` 只拷不报错，
    调用方无法区分「拷满」与「对端关闭」；导出后 `internal/transport` 才能经断言调用它。
  - 新增 `TakeTry() (buf, full []byte, ok bool)`：零拷贝整块移交。仅当剩余数据在单一节点内、
    该节点是收流精确对齐节点、且已被本帧完整消费、且已写满（`malloc == cap`，否则 `book` 还会复用它）
    时才移交；否则返回 `ok == false` 让调用方回退拷贝路径。移交后缓冲归还的唯一路径是调用方的
    `put(full)`（注意**不能**归还 `buf`，它是 `cap` 已被截断的子切片），且不得再 `Release` 该 Reader
    （避免双归还）；另有 `flagUnmanaged` 作防御。
- `nocopy_linkbuffer_race.go` —— 同上两者在 `SafeLinkBuffer`（`-race` 构建）下的加锁转发。
  注意该文件里 `// TakeTry implements Reader.` 的注释是从邻近方法抄来的，**`TakeTry` 并不在
  `Reader` 接口里**（上游及本 fork 的 `Reader` 接口都无此方法），它是具体类型上的方法，
  由调用方用匿名接口断言取用。

**[并模块]**（仅两行 import，`github.com/cloudwego/netpoll/internal/runner` → 本仓库新路径）

- `connection_onevent.go:23`、`netpoll_unix.go:28`

**[测试]** 新增 `nocopy_taketry_test.go`（`package netpoll`，只用标准库 + 自带 testPool mock），
是 fork 树里唯一的测试。随主模块 `go test ./...` 一起跑。

### shmipc-go（基线 v0.2.0）

**[功能]**

- `buffer_manager.go` —— 三处改动：
  1. **4K 对齐 stride**：新增 `alignSize = 4096` / `align4K()` / `alignListOffset()`。buffer 数据区
     （起始 + 20B header）按 4096 步进并对齐，使其可直接作为 `O_DIRECT` 读缓冲，免去一次 memcpy。
     凡涉及 stride 的地方（`bufferRegionCap`、`bufferNum`、`countBufferListMemSize`、`*b.tail`、
     `next = current + stride`）都同步改了；`sizeof` 类算术同时改用显式 `uint64`，避免溢出。
  2. **`counter` 偏移 bug 修复**：`mappingFreeBufferList` 读 `counter` 从 `mem[offset+24]`
     改为 `mem[offset+20]`。上游自身不一致——创建侧 `createFreeBufferList`（`buffer_manager.go:392`）
     一直写 `+20`，映射侧却读 `+24`；字段布局是 `head@8, tail@12, capPerBuffer@16, counter@20`，
     `+24` 落到了下一个字段上。后果是**attach 到已有共享内存的那一端**（客户端）把 counter 当垃圾值，
     空链表占用判断失真，可能发出仍在使用的 buffer。
  3. **`BufferList.push` 防御性越界检查**：`newTail` 越界或 `newTail + bufferHeaderSize` 越界时
     打日志、`putBackBufferSlice` 后丢弃，而不是继续破坏链表。

**[门禁]**（并模块后语言版本 1.15 → 1.25，`go vet` 新报出；均不改变行为）

- `util.go` —— `string2bytesZeroCopy` 原先手搓 `reflect.SliceHeader` 再整体强转切片头，
  vet 报 `possible misuse of reflect.SliceHeader`（`Data` 是 `uintptr`，GC 看不到这层引用）。
  改用 `unsafe.Slice(unsafe.StringData(s), len(s))`：语义等价（同为 `len == cap` 的只读零拷贝视图），
  `len(s) == 0` 时返回 `nil`——唯一调用方 `WriteBytes` 对空输入提前 return，不解引用。同时删除
  已不再使用的 `reflect` import。
- `session.go` —— 两处 `fmt.Errorf("..." + err.Error())` 改为 `%,  %s` 占位（`VerifyConfig` 失败、
  `getConnDupFd` 失败）。printf 检查在 go ≥ 1.24 语言版本下把非常量格式串判为错误；
  消息文本与原来逐字节一致（冒号后不补空格）。
- `event_dispatcher_linux.go` —— 删除 `runLoop` 内 goroutine 末尾的 `runtime.KeepAlive(d)`。
  它位于 `for { ... }` 死循环之后（循环唯一出口是 `epollWait` 出错时的 `return`），永远执行不到，
  vet 报 `unreachable code`。它本来也无效果：`d` 被 goroutine 闭包捕获且循环体内持续使用
  （`d.epollFd` / `d.runLambda` 等），生命周期本就有保障。

## 运维约束（务必遵守）

1. **shmipc 的段布局与上游 ABI 不兼容。** `buffer_manager.go` 改了 stride 与各列表起始偏移
   （见上），所以共享内存段的**字节布局与上游 v0.2.0 完全不同**。混版部署会**静默错位**而非报错。
   **两端必须同时使用本 fork。** 升级 shmipc fork 时须两端一起升，并做一次实际 Put/Get 往返验证。
2. **两处独立的往返验证，不要互相替代。** shm 路径依赖 segment 布局一致；netpoll 路径依赖
   fork 新增 API 存在。只测其中一条不能证明另一条完好。
3. **fork 未携带上游测试。** 两份 fork 都**丢弃了上游所有 `_test.go`**（netpoll 仅存 fork 自带的
   `nocopy_taketry_test.go`；shmipc-go 一个都没有）。因此 `make check` / `go test ./...` 全绿
   **不代表 fork 的未改动部分被上游测试覆盖过**。对 fork 做非平凡改动时，请在临时副本里并入
   上游测试跑一遍，不要把门禁绿当成回归保证。
4. **升语言版本会解锁新的 vet 检查。** 并模块让两份 fork 的有效语言版本从 `go1.15` / `go1.20`
   升到主模块的 `go1.25`，vet 因此新报出上述 3 处问题（还有一处 printf 命中）。同时 go1.22 的
   **逐迭代循环变量作用域**也随之生效。后者已审计：全仓库（含两份 fork）不存在「闭包内写入循环变量」
   这一唯一会因该变更而改变行为的模式，只读捕获只会变得更正确。

## 如何更新一份 fork

```bash
MC=$(go env GOMODCACHE)
# 1) 取上游目标版本到临时目录，与当前 fork 做 diff，逐个确认本文件里列出的改动仍在
diff -rq "$MC/github.com/cloudwego/netpoll@<新版本>" third_party/netpoll
# 2) 确认上游模块路径是否变化（本 fork 的两行内部 import 依赖它）
# 3) 更新本文件顶部的基线版本表，并重跑：
make check && make check-linux && go test ./...
```

改动必须保持「只增不改语义」的最小面：本 fork 的 patch 是**承重**的（`internal/transport`
的零拷贝路径与 4K 对齐收流都依赖它），不是可选优化。
