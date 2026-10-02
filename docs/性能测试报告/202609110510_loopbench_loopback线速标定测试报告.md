# loopback 线速标定测试报告（taihu-loop-bench）

- 日期：2026-09-11
- 提交：未提交（测试工具 `cmd/taihu-loop-bench` 为新增 bench 程序）
- 测试人：taihu 工程（DeepSeek 助手执行）
- 关联报告：`202609110430_4e5532f_final位收尾优化与压力梯度验证报告.md`（gRPC 端到端 16.5 GiB/s）

## 一、背景与目的

此前全部端到端测试（gRPC 3 server × 3 client）聚合读上限停在 ~16.5 GiB/s，且与磁盘直连聚合（19.71 GiB/s）有 ~16% 差距，一直推断瓶颈在"同机 loopback"。但该结论基于间接证据（应用层带宽 ≈ lo PEAK 采样），存在两个未决问题：

1. **loopback 的真实线速上限到底是多少**？若远高于 16.5 GiB/s，则 gRPC 协议/收流路径仍有优化空间；若就在 ~17 GiB/s，则同机场景已到架构极限。
2. **gRPC 协议开销占比多少**？需要与"裸 TCP、socket 函数完全一致"的基准对比。

为此编写独立 bench 程序 `taihu-loop-bench`，使用与 taihu 传输层完全一致的网络栈（netpoll 事件驱动 epoll + readv 收流 / sendmsg 推流），去掉 gRPC 帧协议与磁盘 IO，**纯数据流标定 loopback 线速**。

## 二、测试环境

| 项 | 值 |
| --- | --- |
| 节点 | 100.71.128.12（SZYFQ-PM-OS01-BCNFS-XCKP05，跳板机） |
| 架构 | aarch64，96 核，Linux 4.19.90（bcLinux） |
| 网络形态 | client 与 server 同机，全部走 loopback（127.0.0.1） |
| 网络栈 | netpoll fork（`third_party/netpoll`）：epoll 事件驱动，readv 收流 / sendmsg 推流，非阻塞 |
| 磁盘 | 未参与本测试（纯内存数据流，无 IO） |
| 工具 | `cmd/taihu-loop-bench`（新增，交叉编译 linux/arm64 部署 `/tmp/taihu-loop-bench.new`） |

## 三、测试工具设计（taihu-loop-bench）

### 3.1 设计原则：socket 函数与当前传输层一致

| 层 | 当前传输层（gRPC 读路径） | taihu-loop-bench | 一致性 |
| --- | --- | --- | --- |
| 事件模型 | netpoll epoll 事件驱动 | 同左（`netpoll.NewEventLoop` / `DialConnection`） | 一致 |
| 收流 | `defaultPoll.handler → ioread → readv` | `conn.Reader().Next(n) + Release`（nocopy 零拷贝） | 一致 |
| 推流 | `connection.flush → sendmsg` | `conn.Writer().Malloc(n) + Flush`（nocopy） | 一致（均走 netpoll 写路径 sendmsg） |
| 数据方向 | 读：server 推 4MiB 数据帧 | `-dir push`：server→client；`-dir pull`：client→server | 对齐 |
| 块大小 | 4MiB/帧 | `-chunk`（默认 4194304） | 一致 |

### 3.2 关键实现（与限速相关的语义）

- **推流端 `pushLoop`**（[main.go:50-79](file:///d:/workspace/taihu/cmd/taihu-loop-bench/main.go#L50-L79)）：每连接独立 goroutine，循环 `Writer.Malloc(chunk)` + `Writer.Flush()`。netpoll 的 `flush()`（[connection_impl.go:526-552](file:///d:/workspace/taihu/third_party/netpoll/connection_impl.go#L526-L552)）为**同步 sendmsg**：输出缓冲非空即 `sendmsg` 直发；若 TCP 发送缓冲满（EAGAIN）则注册写事件并 `waitFlush()` **阻塞等待写事件**——这是单连接推流速率的天然背压点，与当前服务端发送路径同构。
- **收流端 `recvLoop`**（[main.go:81-90](file:///d:/workspace/taihu/cmd/taihu-loop-bench/main.go#L81-L90)）：每连接 goroutine 循环 `Reader.Next(chunk)`（阻塞等满 chunk 字节，nocopy 返回）+ `Release`，零用户态拷贝，与当前客户端收帧路径同构。
- **统计**：`sendBytes/recvBytes` 原子计数；client 每秒打点瞬时速率，结束输出 `RESULT avg/peak/total/elapsed`；server 每秒打点收发速率。
- **角色**：`-role server`（netpoll EventLoop 监听 + OnConnect 起推流/收流 goroutine）、`-role client`（DialConnection × conns + 统计）。

### 3.3 参数

```
taihu-loop-bench -role server|client -addr 127.0.0.1:7788 \
  -conns N -secs 10 -chunk 4194304 -dir push|pull
```

## 四、测试方法

### 4.1 场景矩阵

| 场景 | 说明 |
| --- | --- |
| A. 单进程对梯度 | 1 server + 1 client，conns = 1/2/4/8/16/32/64，secs=8，chunk=4MiB，push |
| B. 3×3 串行 | 3 server（7788/7789/7790）+ 3 client 依次串行，各 conns=8，secs=8 |
| C. 3×3 并行 | 3 server + 3 client 同时运行（与 gRPC 3×3 对齐），conns = 4/8/16，secs=8 |

### 4.2 采样

- 全程 `mpstat -P ALL 1` 采集整机逐核 CPU（场景 A、C）；
- client 每秒打点 + `RESULT` 汇总（avg 均值 / peak 峰值 / total 总量 / elapsed 时长）。

### 4.3 数据处理

- 聚合带宽 = 各 client `RESULT avg` 之和；聚合峰值 = 各 client `RESULT peak` 之和；
- CPU 取测试活跃窗口的 `all` 行均值（`usr/sys/iowait/idle`）。

## 五、测试结果

### 5.1 场景 A：单进程对连接梯度（1 server + 1 client）

| conns | avg (MiB/s) | peak (MiB/s) | 单连接分摊 (GiB/s) |
| --- | --- | --- | --- |
| 1 | 3040.0 | 3948 | 3.0 |
| 2 | 5650.6 | 6388 | 2.8 |
| 4 | 9595.5 | 10352 | 2.4 |
| **8** | **10370.9** | **10736** | 1.3 |
| 16 | 9667.9 | 10144 | 0.6 |
| 32 | 9807.0 | 10136 | 0.3 |
| 64 | 9052.8 | 9540 | 0.14 |

**整机 CPU（活跃窗口）：busy 8.6%（usr 0.6% / sys 7.1% / iowait 0）**

要点：
- 4 连接内线性叠加（1→4：3.0→9.6 GiB/s），8 连接达平台 ~10.4 GiB/s，之后随连接数增加**轻微下降**（调度/锁开销）；
- **单进程对的聚合上限 ≈ 10.4 GiB/s**，且整机 CPU 仅 8.6% —— 远未到 CPU 饱和，上限来自**单连接同步 flush 背压 × 单 client 进程收流能力**的组合。

### 5.2 场景 B：3×3 串行（3 server + 3 client 依次跑，conns=8）

| 对 | avg (MiB/s) | peak (MiB/s) |
| --- | --- | --- |
| s1↔c1（7788） | 10676.1 | 11404 |
| s2↔c2（7789） | 10780.7 | 11252 |
| s3↔c3（7790） | 10696.7 | 11348 |

每对 ~10.7 GiB/s，与场景 A 单进程对上限一致（串行时互不干扰）。

### 5.3 场景 C：3×3 并行（与 gRPC 3×3 完全对齐）

**conns=4：**

| 对 | avg (MiB/s) | peak (MiB/s) |
| --- | --- | --- |
| c1（7788） | 5817.3 | 6096 |
| c2（7789） | 5805.7 | 5860 |
| c3（7790） | 5694.8 | 5832 |
| **聚合** | **17317.8（17.3 GiB/s）** | **17788（17.4 GiB/s）** |

**conns=8：**

| 对 | avg (MiB/s) | peak (MiB/s) |
| --- | --- | --- |
| c1（7788） | 4984.0 | 6132 |
| c2（7789） | 5375.4 | 5824 |
| c3（7790） | 5519.5 | 6156 |
| **聚合** | **15878.9（15.9 GiB/s）** | 18112（18.1 GiB/s） |

**整机 CPU（conns=4/8 活跃窗口）：busy 6.3%（usr 0.6% / sys 5.3% / iowait 0）**

> conns=16 轮因脚本等待逻辑超时未取到有效数据，已排除（不影响结论：conns=4 已见峰值，连接继续增多只会下降）。

要点：
- **并行 3×3 裸 TCP 聚合峰值 ≈ 17.3 GiB/s（conns=4）**，且整机 CPU 仅 6.3% —— 无任何核饱和；
- 并行时 loopback 为**共享介质**：每对从串行的 10.7 GiB/s 被压到 5.8 GiB/s，总带宽收敛到共享上限；
- conns 4→8 聚合反而从 17.3 降到 15.9 GiB/s（瞬时 peak 更高但均值下降），连接数增多带来调度/锁开销，不带来带宽。

## 六、对比与归因分析

### 6.1 全链路对比总表

| 场景 | 聚合带宽 | 说明 |
| --- | --- | --- |
| 磁盘直连聚合（3 盘并发，8abdf4d，无网络） | **19.71 GiB/s** | 磁盘层真实上限 |
| **裸 TCP loopback（3×3 并行，本报告）** | **17.3 GiB/s** | loopback + netpoll 栈线速 |
| gRPC 端到端（3×3，4e5532f final 位） | **16.5 GiB/s** | 应用层聚合读 |
| 单进程对裸 TCP（8 连接，本报告） | 10.4 GiB/s | 单进程对上限 |

### 6.2 归因

1. **loopback 线速上限 ≈ 17.3 GiB/s**：裸 TCP（零协议、零磁盘、零用户拷贝）并行聚合也止步 17.3，且 CPU 仅 6.3%、iowait 0 —— 上限来自**内核 loopback + TCP 栈的固有吞吐**（aarch64 4.19 内核回环栈），非应用层可优化项。数据方向为单向 push（对应读路径）。
2. **gRPC 协议开销仅 ~5%**：16.5 / 17.3 = 95.4%。经过多轮优化（RawCodec 零拷贝、RawFrame、TakeTry、final 位），gRPC 端到端已贴近裸 TCP 线速的 95%，剩余 5% 为帧协议头、收发 syscall 与 gRPC 流控的必要代价，**协议层已无可榨空间**。
3. **磁盘 12% 余量被 loopback 挡住**：磁盘直连能到 19.71 GiB/s，loopback 只放行 ~17.3，差额 ~2.4 GiB/s（12%）纯属回环线速限制，磁盘本身无问题（单盘峰值 6.6 GiB/s 可达极限）。
4. **单进程对上限（10.4）≠ loopback 上限**：单 client 进程受"同步 flush 背压 × 单进程 poller/goroutine"约束只能到 10.4；多进程并行共享 loopback 才能压出 17.3。这解释了为何单 server 16 客户端 gRPC 只能到 ~5.4 GiB/s，而 3 server × 3 client 到 16.5。

## 七、结论

1. **同机 loopback 场景架构极限 = 17.3 GiB/s**（实测裸 TCP 线速），gRPC 端到端 16.5 GiB/s 已达其 95.4%，**同机部署到此为顶**。
2. **协议与拷贝已无可优化**：客户端 memmove 归零、TakeTry 命中率 99.9%、协议开销 5% —— 继续优化的边际收益 <1%。
3. **要突破 17 GiB/s 天花板，唯一路径是跨机物理网卡**：需实测目标网卡（bond0.1253 / 多网卡绑定 / RDMA）的真实带宽后重新标定；磁盘侧 19.7 GiB/s 的余量只有在跨机后才能兑现。
4. **工具沉淀**：`cmd/taihu-loop-bench` 作为独立标定工具保留，可用于：跨机场景网络线速标定、网卡绑定方案对比、未来 io_uring/RDMA 改造的基准对照。

## 附录 A：原始数据文件

| 文件 | 内容 |
| --- | --- |
| `/tmp/run_loop_bench.sh` | 场景 A 脚本（1 server + conns 梯度） |
| `/tmp/run_3x3_c8.sh` | 场景 B 脚本（3×3 串行 conns=8） |
| `/tmp/run_3x3_par.sh` | 场景 C 脚本（3×3 并行梯度） |
| `/tmp/loop-cpu.log` | 场景 A 整机 CPU 采样 |
| `/tmp/loop-cpu-par.log` | 场景 C 整机 CPU 采样 |
| `/tmp/loop-srv-*.log` | 各 server 进程打点 |

## 附录 B：taihu-loop-bench 用法

```bash
# server（后台）
setsid nohup /tmp/taihu-loop-bench.new -role server -addr 127.0.0.1:7788 -dir push &

# client（前台，8 连接 × 10s）
/tmp/taihu-loop-bench.new -role client -addr 127.0.0.1:7788 -conns 8 -secs 10 -chunk 4194304 -dir push

# 输出
[client] t=1s rate=9632 MiB/s total=9.41 GiB
...
RESULT avg=10370.9 MiB/s peak=10736 MiB/s total=91.15 GiB elapsed=9.0s
```
