# 客户端多 IP 连接均分实现计划

## Context

当前 taihu-server 支持 `-listen` 逗号分隔绑定多个 IP（`listenMultiPort` 将所有 IP 绑到同一端口），但注册到 TiKV 的通告地址 **只取第一个 IP**（`advAddr = ips[0]:port`）。客户端从 TiKV 拿到 `InstanceInfo.Addr` 后只与第一个 IP 建立连接，其余绑定的 IP 上的监听虽存在但从未被使用——多 IP 绑定形同虚设，25G 分流也无法利用（146 集群测试已证明 bond1 饱和而 bond2 空闲的 2:1 分流瓶颈）。

目标：**当服务端绑定多个 IP 时，客户端与每个 IP 都建立 TCP 连接，读写请求在全部 IP 连接上 round-robin 均分**。利用 `rpcclient.Storage` 现有的 `pick()` round-robin（`atomic.AddUint64(&s.rr,1) % len(conns)`），只要把多个 IP 的连接合并进同一个 `Storage.conns`，即可零新增轮询代码实现"按 IP 均分"。

核心洞察（已核实）：`rpcclient.Storage` 的 round-robin 与 IP 无关——`conns []rpcConn` 里的底层连接若覆盖多个 IP，`pick()` 自动在所有连接上均分。缺口仅在：(1) 服务端只通告 `ips[0]`；(2) 建连函数只接受单地址。

## 改动清单（全部为修改，无新增文件）

### 1. `internal/cluster/instance.go` —— 数据模型加字段

`InstanceInfo` 增加：

```go
Addr  string   `json:"addr"`   // 保持：首个 IP:port（兼容旧客户端 & connKey）
Addrs []string `json:"addrs"`  // 新增：完整 ip:port 列表；旧 server 无此字段时为空
```

### 2. `pkg/rpcclient/dial.go` —— 新增多地址建连

`DialPool` 签名**不变**，改为委托；新增 `DialPoolMulti`：

```go
func DialPool(ctx context.Context, addr string, n int) (*Storage, error) {
    return DialPoolMulti(ctx, []string{addr}, n)
}

// DialPoolMulti 每个地址建 perAddr 条连接，全部塞入同一 Storage.conns，
// 现有 pick() 自动在「地址数 × perAddr」全部连接上 round-robin。
// addrs 为空 / perAddr<1 按 DialPool 语义回退；任一地址拨号失败则整体报错并
// 关闭已建连接（与 DialPool 全失败语义一致，避免静默降级隐蔽故障）。
func DialPoolMulti(ctx context.Context, addrs []string, perAddr int) (*Storage, error) {
    if perAddr < 1 { perAddr = 1 }
    s := &Storage{conns: make([]rpcConn, 0, len(addrs)*perAddr)}
    for _, addr := range addrs {
        for i := 0; i < perAddr; i++ {
            conn, err := transport.DialClient("tcp", addr)
            if err != nil {
                _ = s.Close()
                return nil, fmt.Errorf("dial %s: %w", addr, err)
            }
            s.conns = append(s.conns, conn)
        }
    }
    return s, nil
}
```

所有现有 `DialPool` 调用点（`dial.go:14`、`pool_test.go:54,101`、`storage.go:131,135`、`cmd/taihu-cli/cmd/key.go:31`、`common.go:96`）零改动。

### 3. `cmd/taihu-server/main.go` —— 注册时填充 Addrs

`info` 构造处（约 L151-158）新增：

```go
info := &cluster.InstanceInfo{
    Name:      *serverName,
    Node:      hostname,
    Hostname:  hostname,
    Addr:      advAddr,        // 仍以首个 IP 为 Addr（不破坏 connKey / 旧客户端）
    ShmAddr:   shmPath,
    StartTime: time.Now().Unix(),
}
for _, ip := range ips {
    info.Addrs = append(info.Addrs, net.JoinHostPort(ip, strconv.Itoa(rpcPort)))
}
```

`refresh()`（L160-169）无需改：`n := *info` 值拷贝自动保留 `Addrs` 字段，`cluster.Register`/`RunHeartbeat` 同样值拷贝透传。

### 4. `pkg/rpccluster/storage.go` —— clientFor 跨节点分支

仅改 else（跨节点 TCP）分支（L133-136）；shm 分支（L126-132）与其内部回退不动：

```go
} else {
    // 跨节点 TCP：多 IP 则每个地址建 cfg.Conns 条（均分）；单地址回退单地址路径（兼容旧 server）。
    if len(inst.Addrs) > 0 {
        c, err = rpcclient.DialPoolMulti(context.Background(), inst.Addrs, s.cfg.Conns)
    } else {
        c, err = rpcclient.DialPool(context.Background(), inst.Addr, s.cfg.Conns)
    }
}
```

`connKey`（L100-105）保持单 key（`"tcp://"+Addr`）：一个 key 对应一个含全部 IP×连接的 `Storage`，cache 命中/双检/copy-on-write（L113-147）零改动。

### 5. 注释/帮助文本更新

- `pkg/rpccluster/config.go` L35-37：`Conns` 语义 "每实例数据面连接数" → "**每地址**数据面连接数：本地实例 shm 会话数、跨节点每 IP TCP 连接数（DialPoolMulti perAddr）；多 IP 时总连接数 = IP 数 × Conns"。
- `cmd/taihu-rpc-bench/main.go` L56 `-conns` 帮助文本 + L103 内联注释同步措辞。

## 测试计划

### A. `pkg/rpcclient/pool_test.go`（包内）

- `TestDialPoolMultiRoundTrip`：`newTestServer(t)` 起 **2 个** server（不同端口天然区分，无需改 helper），`DialPoolMulti(ctx, []string{addrA, addrB}, 3)` → 断言 `len(s.conns) == 6`；交错 Put/Get 多对象确保落到两端均读写成功（若全落同一 server，另一 server 上 key 会 miss）。
- `TestDialPoolMultiNoAddrs`：`DialPoolMulti(ctx, nil, 2)` → 断言 err != nil。
- 既有 `TestDialPoolRoundTrip` 锁定 `DialPool` 兼容，零改动。

### B. `pkg/rpccluster` 包内测试（新文件 `storage_multiaddr_test.go`）

- `TestClientForMultiAddrConns`：`NewMemoryKV()+Register` 构造 `InstanceInfo{Addrs: []string{a, b}}`，`NewCluster` 后包内直调 `s.clientFor(inst)`，用 `reflect` 读未导出 `Storage.conns`，断言长度 = `len(Addrs)×cfg.Conns`，且各连接远端 IP 覆盖两地址。
- `TestClientForFallbackSingleAddr`：`Addrs=nil` → 断言走 `DialPool(Addr, Conns)`（conns 长度 = Conns）——兼容性路径。
- `TestClientForConnectionCaching`：两次 `clientFor` 同一实例 → 返回同一 `*Storage`（`==` 相等），仅建一批连接。

## 风险与兼容

1. **部分地址失败**：`DialPoolMulti` 整体报错 + 关闭已建连接；`clientFor` 持锁重试路径不变，失败后下一请求重试拨号，无竞态窗口。
2. **key 唯一性**：多 IP 用首地址 key；实例按 Name 单注册，key 唯一。IP 集合变化时旧连接暂留（pre-existing 行为，未恶化）。
3. **双向兼容**：新客户端读旧 server（Addrs=nil）→ 回退 `DialPool(Addr)`；旧客户端读新 server → 忽略 Addrs（首 Addr 不变）。
4. **picker/索引/缓存**：只经 `clientFor` 拿连接，多 IP 包在单个 `*Storage` 内，上层零感知。
5. **PutWriter**（putwriter_linux.go:14，`//go:build linux`）：仅 shm 会话路径用 `conns[0]` 类型断言；多 IP 走 TCP 时断言失败走通用路径，不影响正确性。
6. **连接总量**：多 IP × Conns 成倍增加连接数，属预期（conns=每 IP 连接数）。

## 验证

1. `go build ./...` 通过；`go test ./pkg/rpcclient/... ./pkg/rpccluster/...` 全绿。
2. 本地 linux/arm64 交叉编译 `taihu-server.50ebbe4` 与 `taihu-rpc-bench.50ebbe4`。
3. 146 集群验证（部署 skill `taihu146-perf`）：
   - 重部署：每节点变更为 **TAIHU-N/N+1 绑定 bond1+bond2 双 IP**（`-listen <bond1>,<bond2>`）等拓扑，使数据面跨节点能同时走两 bond。
   - 跨节点读：确认 bond1.2175 与 bond2.2372 的 rx 差分**均衡**（不再 2:1），单扇入节点带宽突破当前 8.0-8.6 GiB/s 上限（预期接近 2×bond 总和）。
   - 单测探针 `probe_max146.sh NCLI CONNS THREADS COUNT TAG` 复测聚合上限。