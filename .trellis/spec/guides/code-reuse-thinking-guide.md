# 复用与单一事实源

> 本指南**不是规则清单** —— 规则在其余 7 层。它回答一个动作问题：**动手写新东西之前，先搜什么、搜到什么深度算够**。
>
> 判据来自本仓库的真实漏改（§2）与真实「刻意的重复」（§4），不是通用建议。

---

## 1. 本仓库已有的单一事实源

这几样东西被刻意收在一处。动到它们之前先知道消费方在哪 —— 否则就是「改了一半」。

| 事实源 | 唯一所在 | 消费方 |
|---|---|---|
| 库错误 sentinel | `internal/ierr/ierr.go:1-3`（包文档自称**唯一事实源**） | `device` / `metastore` / `storage` / `transport` 全层；对外经 `internal/rpcclient/reexport.go:31-42` 转出一份 |
| 线上帧布局 | `internal/transport/protocol/protocol.go:51` `FrameHeaderLen = 5` | `internal/transport/frame.go:106`（校验下界）、`:196-197`（写帧头）、`internal/transport/server.go:7`（文档注释） |
| 段物理布局 | `internal/layout/layout.go` | `device` 与 `metastore` 共同依赖 —— `internal/layout/layout.go:1-2` 写明「本包独立存在的唯一理由是打断循环依赖」 |
| 库错误 ↔ 线上错误码 | `internal/transport/protocol/protocol.go:113` `MapStorageErr` / `:129` `MapCode` | 服务端只调前者（`internal/transport/server.go:226` 等 8 处）、客户端只调后者（`internal/transport/client.go:106` 等 4 处） |

**派生优于复写。** 最短的正面示例是 `internal/transport/protocol/protocol.go:54` —— 新容量由既有常量算出来，而不是再写一个手算结果：

```go
// MaxFrameTotal 帧负载上限 = FrameHeaderLen + ChunkSize。
const MaxFrameTotal = FrameHeaderLen + ChunkSize
```

改了 `FrameHeaderLen` 或 `ChunkSize`，`MaxFrameTotal` 自动跟上；若当初写成 `= 37`，这次改动就会漏掉它。

## 2. 一个真实的漏改：`64eaec3`

这条不是假设，是本仓库发生过的：

```
feat(bench): bench_cluster 补齐 --pipeline 参数

benchkit 已支持 Pipeline（每个 worker 保持的在途 op 数，>1 时走
runPipelinedWorker 并发下发），bench_single 也已读取该 flag，但
bench_cluster 既没注册也没读取，集群压测无法开流水线，恒为串行。
```

形态值得记住：**共享逻辑（`internal/benchkit/run.go:33` 的 `Config.Pipeline`）已经支持，两个调用点里只有一个接上了线**。`cmd/taihu/cmd/bench_single.go:123` 注册了 `f.Int("pipeline", ...)` 并 `:55` 读取，`cmd/taihu/cmd/bench_cluster.go` 两样都没有 —— 于是同一个 benchkit 在两个命令下行为不同，且不报错。

修法只有 2 行（`git show --stat 64eaec3` 的 `2 ++`），但发现它花了很久。**这类「一半接上线」的形态靠编译器查不出来**，只能靠写完一个调用点后去搜另一个：

```bash
# 我改/加了 X，还有谁读它？
grep -rn 'Pipeline' --include='*.go' cmd/ internal/benchkit/ | grep -v _test.go
```

## 3. 动手前的三问

| 问 | 若「是」 |
|---|---|
| 同样的值/常量在别处定义过吗？ | 用那一个；要改就改那一个（§1 的表先过一遍） |
| 我是在复制一段已有的**逻辑**吗？ | **停** —— 先判断它是不是该抽到共享位置（§5 说了什么时候**不**该抽） |
| 我加的参数/字段有**多个消费方**吗？ | 那就把每一个消费方都接上线，别只接手上这个（§2） |

批量改动之后回搜一遍，本仓库有现成的机械手段：

```bash
make check            # check-fmt + check-layering + check-sdk-only + go vet
```

`check-layering` 与 `check-sdk-only` 就是两条**用 grep 实现的越界检测** —— 它们不查重复代码，但查「同一件事有没有在错误的层里被做第二遍」（规则与理由见 `Makefile:35-36` 的注释与 [layering.md](../architecture/layering.md)）。改完先跑它们，比人眼过一遍可靠。

## 4. 重复**不是**问题：两处可以并存

本仓库最容易被误判成「duplicate」的地方，是 `internal/transport` 的两个平台桩：

```go
// errShmUnsupported shmipc 仅支持 Linux。
var errShmUnsupported = errors.New("taihu: shmipc only supported on linux")
```

`internal/transport/server_shm_other.go:14` 与 `internal/rpcclient/dial_shm_other.go:12` 各持一份，**消息相同、互不复用**。这是刻意的：它们是不同层的平台桩，行为与生存期独立。合并到 `internal/ierr` 会把「一个平台的降级实现」提升成「全仓库公共错误」，语义反而错了。

判据：**重复的是「值的巧合」还是「契约的同一件事」？** 前者各留一份，后者收一处。

## 5. 什么时候**不**抽象

「重复三次就抽」在本仓库不成立 —— 有两条刻意的未抽象，都写明了理由：

| 未抽象 | 理由 |
|---|---|
| `internal/transport/batch.go:15` | 注释写明「不做主动关闭」—— 不为没有需求的调用方预先建一套生命周期 API |
| `internal/cluster/kv.go:8` | 接口只列实际用到的几个方法 —— 最小接口，不预建抽象 |

**只在同时满足下面两条时才抽**：

1. 它已经在 2 个以上地方**真的**重复（不是「将来可能」）；
2. 抽象之后，读的人不用跳转就能知道它做什么。

任一条不满足，就地写清楚，比抽出去更好读。

## 6. 唯一的「必须先搜」硬约束

改**任何**跨进程/跨版本的常量之前，先搜它的解析侧：

```bash
grep -rn 'FrameHeaderLen\|MaxFrameTotal\|metaVersion' --include='*.go' internal/ | grep -v _test.go
```

原因见 `cross-layer-thinking-guide.md` §4 —— 本仓库有一个**写了但没人读**的版本字节（`internal/metastore/meta.go:14` 的 `metaVersion`，三处 encode 写入 `:30`/`:57`/`:94`，解码侧零使用）。它证明了「我写的时候两边都改了」这个直觉并不可靠。

---

## 相关

- 规则本体：[api-surface.md](../architecture/api-surface.md) 规则 7（依赖从构造函数进）、[code-style.md](../architecture/code-style.md) 规则 19（魔法数字改具名常量）
- 跨层传播：`cross-layer-thinking-guide.md`
- 平台文件成对的复用形态：[file-splitting.md](../platform/file-splitting.md)
