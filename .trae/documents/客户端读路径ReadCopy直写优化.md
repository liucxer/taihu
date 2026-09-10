# 客户端读路径 ReadCopy 直写优化（降低客户端 CPU 消耗）

## Context

3 客户端 / 6 客户端并行读的 pprof 显示客户端 CPU 消耗构成（两轮形态完全一致）：

- **memmove ~50%**：其中 `Conn.Get` 汇入拷贝（帧数据拷进 4K 对齐 bufpool 缓冲）占 **69%**，`UnsafeLinkBuffer.Next` 多节点帧搬移占 **31%**；
- Syscall6（recv）~44%。

`Next(n)` 在**单节点连续空间足够**时零拷贝返回切片，但 4MiB 帧经多次 readv 读入为多个 linkBuffer 节点时，`Next` 会 `malloc` + 逐节点 `copy`（1 次多余搬移），随后 `Get` 再 `copy` 汇入对齐缓冲（第 2 次）。多节点场景每帧 2 次 memmove，其中 Next 搬移是纯多余开销。

netpoll 收侧已有现成原语 `UnsafeLinkBuffer.readCopy(p []byte)`（[nocopy_linkbuffer.go#L89-L145](file:///d:/workspace/taihu/third_party/netpoll/nocopy_linkbuffer.go#L89-L145)）：**直接把节点链数据拷进用户缓冲 p，一次拷贝、无中间 malloc/搬移**，并自动释放已消费的非暴露节点。但该方法是未导出的，且不在 `Reader` 接口上。`frameMsg.r` 实际类型为 `*UnsafeLinkBuffer`（非 race 构建，`LinkBuffer = UnsafeLinkBuffer`），天然具备该方法。

**目标**：导出 `ReadCopy`，`Conn.Get` 收帧改为直拷调用方对齐缓冲，多节点场景 2 次 memmove → 1 次，单节点场景保持 1 次不变。预期客户端 memmove ~50% → ~35%，客户端进程 CPU 降 ~15%。

## 改动

### 1. third_party/netpoll/nocopy_linkbuffer.go

把 `readCopy(p []byte) (n int)` 导出为：

```go
// ReadCopy copies up to len(p) bytes directly from the link buffer into p,
// without exposing the underlying nodes to user code and without any
// intermediate allocation/copy (the only copy is into p itself).
// Consumed non-exposed nodes are released; exposed nodes stay for Release().
// Returns the number of bytes copied (0 if the buffer is empty).
func (b *UnsafeLinkBuffer) ReadCopy(p []byte) (n int, err error)
```

内部逻辑与现有 `readCopy` 一致，仅签名变化（`err` 恒为 nil，保留 error 以符合 netpoll Reader 风格）。

### 2. third_party/netpoll/nocopy_linkbuffer_race.go

`SafeLinkBuffer` 的锁包装 `readCopy` 同步改名导出，保持 -race 下并发安全：

```go
func (b *SafeLinkBuffer) ReadCopy(p []byte) (n int, err error) {
	b.Lock()
	defer b.Unlock()
	return b.UnsafeLinkBuffer.ReadCopy(p)
}
```

### 3. internal/transport/conn.go — `Conn.Get` 收帧直拷

[conn.go#L336-L354](file:///d:/workspace/taihu/internal/transport/conn.go#L336-L354) `opGetData` 分支：用类型断言调用 `ReadCopy` 直拷进对齐缓冲，断言失败（如 future/race 变体未实现）回退现有 `Next + copy` 路径：

```go
case opGetData:
	rem := int64(msg.r.Len())
	if rem > size-pos {
		msg.r.Release()
		dispose()
		return nil, nil, fmt.Errorf("taihu: get stream exceeds requested size")
	}
	if buf == nil {
		buf = bufpool.Get(int(size))
		out = buf[:size]
	}
	// 直写调用方缓冲：多节点帧由 readCopy 一次拷入，消除 Next 的中间搬移。
	if rc, ok := msg.r.(interface{ ReadCopy([]byte) (int, error) }); ok {
		n, err := rc.ReadCopy(out[pos : pos+int(rem)])
		if err != nil {
			msg.r.Release()
			dispose()
			return nil, nil, err
		}
		pos += int64(n)
	} else {
		p, err := msg.r.Next(int(rem))
		if err != nil {
			msg.r.Release()
			dispose()
			return nil, nil, err
		}
		pos += int64(copy(out[pos:], p))
	}
	msg.r.Release()
```

注意：`ReadCopy` 后仍须 `msg.r.Release()`——Slice 出的子 Reader 节点带 `flagReadExposed`（`Refer` 引用），`ReadCopy` 只释放非暴露节点，暴露节点由 `Release` 归还，生命周期与现状一致，不破坏 netpoll 节点复用。

## 不纳入本次

- **GetRaw 帧式 API 重引入**（消除汇入拷贝，memmove 再 -69%）：需要新增 API + bench rawread 模式，且 gRPC 时代实测带宽仅 +1.3%、风险高（节点生命周期曾回退），列为后续单独评估。
- **减少 recv 次数**（SO_RCVBUF/帧合并）：`ReadCopy` 后节点数不再影响 memmove 次数，收益有限。

## 验证

1. `go build ./...` + `go vet ./...`；
2. 单测：`go test ./pkg/rpcclient/ -run TestDialPoolRoundTrip -v`；`BIG4M=1` 再跑一遍覆盖 4MiB 跨节点帧；其余包 `go test ./...`；
3. **`go test -race ./pkg/rpcclient/`**（关键：上次零拷贝移交回退即 -race 实证的生命周期错乱，ReadCopy 不转移节点所有权，应保持干净）；
4. 编译部署 128.12（编译部署skill），3 客户端并行读复测（同 8abdf4d 主测参数：32T/16C/4MiB×20000），拉回客户端 pprof 对比：
   - memmove 占比（预期 50% → ~35%）；
   - 客户端进程 CPU / 聚合带宽 / 延迟；
   - 整机 busy 是否下降。
