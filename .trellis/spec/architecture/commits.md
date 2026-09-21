# 提交信息

> 标题恒为中文的一句话，正文讲"为什么改"而不是"改了什么"，纯重构必须补一段「行为不变：」核对 —— 让 review 的人不必读 diff 就能判断该不该放行。

---

## 1. 标题格式：`type(scope): <中文标题>`

`git log` 现有 143 条提交里，除一条 `Merge pull request #...` 和一条早期的英文提交外，**标题全部是中文**（实测 `git log --format=%s | grep -vP '[\x{4e00}-\x{9fff}]'` 只命中 2 条）。

已用过的 type：`refactor`(11)、`fix`(7)、`perf`(6)、`docs`(6)、`feat`(5)、`chore`(4)、`style`(1)。

已用过的 scope（40 条带 scope 的提交里统计）：`transport`(7)、`perf`(3)、`bench`(3)、`aio`(3)、`storage`(2)、`repo`(2)、`pkg`(2)，以及各 1 次的 `server`、`sdk`、`rename`、`naming`、`device`、`cmd`、`bufpool`、`batch`、`doc`、`design`、`review`、`fmt`、`build`、`layout`、`trae`、`third_party`。

**scope 用包名或子系统名**（`internal/aio` → `aio`，`internal/transport` → `transport`），一次改多个模块就并写：`refactor(cli,sdk):`、`fix(storage,transport):`。

### 2. 仓库级改动省略 scope

最近 45 条里只有 3 条不带 scope，全是跨包/无归属的改动：

```
fed67b4 test: 补齐 82 个单测文件与 e2e 套件，third_party fork 回迁上游测试
8e2e67b chore: 补齐 go.sum 并忽略本地构建产物与备份包
d65adcc style: gofmt 规范化（补文件末尾换行 + const 块对齐），无逻辑变更
```

**即 scope 是"这次改动落在哪个包"，不是必填字段**；改动没有单一归属时（补测试、动 go.sum、全仓库 gofmt）就不写。

---

## 3. 正文：讲"为什么"，不是复述 diff

小的改动一段散文说完动机即可。`64eaec3`：

```
feat(bench): bench_cluster 补齐 --pipeline 参数

benchkit 已支持 Pipeline（每个 worker 保持的在途 op 数，>1 时走
runPipelinedWorker 并发下发），bench_single 也已读取该 flag，但
bench_cluster 既没注册也没读取，集群压测无法开流水线，恒为串行。

此处与 bench_single 对齐：注册 f.Int("pipeline", 1, ...)，默认 1 即
退化为旧的串行行为，不影响既有压测数据可比性。
```

注意"默认 1 即退化为旧的串行行为，**不影响既有压测数据可比性**" —— 说的是对既有结论的影响，不是"我加了个 flag"。

### 4. 大改动用 `-` 开头的中文 bullet + `##` 小节

跨多个文件/多个子问题的改动，正文先用 `##` 分小节，每节再列 bullet。`9bcc797` 的骨架：

```
fix(device): 完成侧瞬时错误重试、设备 O_EXCL 独占打开；shmipc 段启用 THP

## io_uring 完成侧瞬时错误重试

实测 nvme 上 4 MiB O_DIRECT 写经 io_uring 偶发以 -EAGAIN 完成（同负载 libaio
不复现），不重试就被当永久错误上抛，一次本可成功的 Put/Read 无谓失败。

- internal/device/device.go:39 新增 complRetryMax=8 / complRetryCap=2ms；
  :195 submitOp 拆出 submitOpN(buf,off,read,count)，count=false 避免重提重复
  计入 IO 尺寸统计；:265 retriableErrno 判定 EAGAIN/EINTR；...
```

`-` 的语义是**粗粒度清单**（一处配置的诞生 + 理由），不是逐行 diff 摘要。

### 5. bullet 里带 `文件:行号` 是可接受的

上面那条 `internal/device/device.go:39` 的引用方式是本仓库的既有做法，另有 `4ebf370`（`internal/storage/storage.go:211`、`internal/transport/server.go:297`）等。**行号是给 reviewer 定位用的锚点**，不要求长期有效 —— 后续改动导致行号漂移不必回头修提交信息。

---

## 6. 纯重构必须补「行为不变：」核对段

这是本仓库最硬的一条正文约定。它只用于**声称"只搬不改"**的提交 —— 全仓库 143 条里只有 2 条带它（`git log` 逐条比对所得：`c50473a`、`522ae0c`），**恰恰因为它只在真重构时才写**。

`c50473a` 的核对段（放在正文末尾，独立成段）：

```
行为不变：ring 选型三分支与启动日志逐字未改，搬移的函数体逐字节
比对一致（差异仅上述改名）。四个消失的导出名即上面收起私有与删死
代码的结果，本轮集中本身未增删。go vet 在 darwin/linux 双平台通过，
内部三包测试全绿。
```

它必须回答两件事：

1. **哪些行为被声称未变** —— 逐项点名（「ring 选型三分支」「启动日志」「搬移的函数体逐字节比对」），不是一句笼统的"无逻辑变更"。
2. **拿什么核对的** —— 比对方式（逐字节 diff）、消失的导出名的来由、以及验证命令与结果（`go vet` 双平台、内部三包测试全绿）。

`522ae0c` 用的是 bullet 变体，把该段列为最后一条 bullet：

```
- 行为不变：正常关机路径照旧取消心跳并注销；新增的只是错误退出路径不再泄漏
```

**写不出第 2 类内容（说不清拿什么核对的）就说明这次不是纯重构** —— 那就不该加这段，改成如实描述行为变化。

### 7. 不要用「行为不变」掩盖行为变化

反例方向明确：`9bcc797` 标题写的是「完成侧瞬时错误重试」，这是**新增重试行为**，所以它没有「行为不变」段，正文里把"实测 … 偶发以 -EAGAIN 完成"这类**触发条件与证据**写清楚。标题里的动词（"修"/"补齐"/"启用"）与是否该带核对段要一致。

---

## 8. 提交信息里的验证证据

正文可以引 gate 与测试结果作为放行依据。`c50473a` 引的是 `go vet` 双平台 + 三包测试；`522ae0c` 则解释了为什么搭便车修一个旧 bug：

```
此前一直挂着未修是当作独立单子。这次一并处理的原因：本次新增的 `make check` /
`make check-linux` 会跑 go vet，不修则两个新目标一上线就是红的 —— 一个永远红的
检查目标没人会用，等于白加。
```

**即：顺手修的东西要说明为什么必须在这笔修**，否则应该拆出去。

---

## 9. 本仓库不做自动提交

`.trellis/config.yaml` 里 `session_auto_commit: false`，注释给了理由：

```
# taihu：关闭自动提交。本仓库的提交信息有既定格式（type(scope): 中文标题 +
# 中文正文 + 「行为不变：」核对段），journal / 归档提交由人显式发起，
# 避免脚本插入的 chore 提交打断 git log。
```

**不要让脚本插入提交** —— 提交信息是人工产物，格式由人保证。
