# 远程访问层线路协议（wire protocol）

> TCP 与 shm 两条数据面共用同一套 op 与编解码纯函数，只有帧头与承载方式不同；跨进程只传错误码，不传错误字符串。

---

## 1. `protocol` 包是纯函数层：无 I/O、无全局状态

这不是"建议"，是包注释里写明的性质（`internal/transport/protocol/protocol.go:1-3`）：

```go
// Package protocol 实现 taihu 传输层的线路协议编解码（纯函数、无 I/O、无全局状态）。
//
// 帧格式: [4B len][4B streamID][1B op][payload...]
```

包内 import 只有 `encoding/binary`、`errors`、`io`、`third_party/netpoll`（仅用于接口断言）与 `internal/ierr`（`protocol.go:19-27`），没有任何连接、文件、全局可变状态或锁。因此两条数据面（netpoll TCP 与 shmipc 共享内存）与任意并发流可以同时调用 `Parse*`/`Encode*`，不存在共享状态竞争；新增编解码函数时也必须保持这一点（不要在这里放计数器、缓冲池或日志句柄）。

## 2. 尺寸常量互相派生，改一个必须改另一个

帧上限由三个常量链式派生（`protocol.go:30`、`:51`、`:54`、`:60`）：

```go
// ChunkSize 单条数据帧负载上限（4MiB），与旧 gRPC 方案一致。
const ChunkSize = 1 << 22 // 4MiB
...
// FrameHeaderLen = streamID(4) + op(1)。
const FrameHeaderLen = 5
// MaxFrameTotal 帧负载上限 = FrameHeaderLen + ChunkSize。
const MaxFrameTotal = FrameHeaderLen + ChunkSize
```

`protocol_test.go:555-575` 的 `TestWireConstantsAreConsistent` 专门钉住这组关系（`MaxFrameTotal == FrameHeaderLen+ChunkSize`、`InputNodeSize == 4+MaxFrameTotal`、`ShmSliceSize` 4K 对齐且不小于 `ChunkSize+8*1024`、`OpGetDataFinal == OpGetData|0x80`）。**改任一派生常量时必须同步改这个测试**，否则门禁会红——这是本仓库对"协议常量改一处漏一处"的唯一防线。

## 3. `ByteReader` 是消费者侧接口，不是生产者侧抽象

接口定义在 `protocol.go:182-192`，注释直接说明它是为了让同一批 `Parse*` 跑在两种 Reader 上：

```go
// ByteReader 帧 payload 读取的最小接口。netpoll.Reader 天然满足（TCP 路径）；
// SliceReader 适配共享内存切片（shmipc 路径），使 Parse* 纯函数在两传输下复用。
type ByteReader interface {
	// Next 返回后续 size 字节（并消费），不足时返回错误。
	Next(size int) ([]byte, error)
	// ReadString 读取 size 字节并转为 string（并消费）。
	ReadString(size int) (string, error)
}
```

`SliceReader`（`protocol.go:194-226`）是 shm 侧的适配器，语义与 `netpoll.Reader.Next` 一致：pos 前进、返回零拷贝引用、越界报 `io.ErrUnexpectedEOF`（`protocol.go:210-217`）。shm 路径的控制帧就靠它解析：`client_shm_linux.go:116`、`:224`、`:343`、`:373` 与 `server_shm_linux.go:321`。

**约定**：新加 `Parse*` 一律只收 `ByteReader`，不要收具体的 `netpoll.Reader` 或 `[]byte`——否则 shm 路径要么重写一份、要么先拷一份。

## 4. `Parse*` 的防御顺序：先校验长度声明，再分配

所有带长度字段的解析器都是"读长度 → 比 `MaxKeyLen` → 才 `ReadString`"（`protocol.go:247-264`）：

```go
func ParsePutHeader(r ByteReader) (key string, size int64, err error) {
	kl, err := ReadU32(r)
	if err != nil {
		return "", 0, err
	}
	if kl > MaxKeyLen {
		return "", 0, errors.New("taihu: key too long")
	}
	key, err = r.ReadString(int(kl))
```

`protocol_test.go:415`（`TestParseHugeDeclaredKeyLenDoesNotAllocate`）证明畸形长度不会放大内存，`protocol_test.go:319`（`TestParseTruncatedInputNeverPanics`）逐字节截断喂给全部解析器并要求不 panic。**新增解析器必须同时进这两个测试的解析器表**（表在 `protocol_test.go` 的 `parsers()`）。

## 5. 跨进程只传 code，不传字符串

线上错误码是 `ErrCode`（4 字节大端，`protocol.go:99-110`），三个方向由三个函数承担：

- 库错误 → code：`MapStorageErr`（`protocol.go:112-126`），用 `errors.Is` 对齐 `ierr` sentinel：

```go
func MapStorageErr(err error) ErrCode {
	switch {
	case errors.Is(err, ierr.ErrNotFound):
		return CodeNotFound
	case errors.Is(err, ierr.ErrInvalidRange):
		return CodeInvalidRange
	case errors.Is(err, ierr.ErrTooLarge):
		return CodeTooLarge
	case errors.Is(err, ierr.ErrNoSpace):
		return CodeNoSpace
	default:
		return CodeInternal
	}
}
```

- code → 库错误：`MapCode`（`protocol.go:128-146`）。
- code → wire payload：`EncCode`（`protocol.go:148-153`），4 字节大端。

两个必须知道的实际性质（不是"最佳实践"，是当前行为）：

1. `MapStorageErr` 只映射 4 个 sentinel，`ierr.ErrShortWrite` 落到 `CodeInternal`（`protocol.go:123-124` 的 `default`）。
2. `MapCode` 的 switch **没有** `CodeInternal` 分支，它走 `default` 返回一个**新造的** `errors.New("taihu: rpc error")`（`protocol.go:143-145`），因此服务端未识别的错误在客户端**无法**用 `errors.Is` 对齐任何 sentinel。`protocol_test.go:451`（`TestMapStorageErrDefaultIsInternal`）固定了第 1 点。

错误 sentinel 的全局体系（`internal/ierr` 是唯一事实源、re-export 只有一处）见 `.trellis/spec/architecture/error-model.md`，本层不重复。

## 6. TCP 拆帧：靠 Reader 阻塞，不靠跨调用状态

`internal/transport/frame.go:95-137` 的读循环是连接 Reader 的**唯一**消费者，帧不完整时不存在"半帧缓冲"状态机：

```go
p4, err := c.c.Reader().Peek(4)
if err != nil {
	return
}
lenField := binary.BigEndian.Uint32(p4)
if lenField < protocol.FrameHeaderLen || lenField > protocol.MaxFrameTotal {
	return
}
sub, err := c.c.Reader().Slice(4 + int(lenField))
```

不变量：

- `Peek(4)` 阻塞到长度字段就绪，`Slice(4+len)` 阻塞到**整帧**就绪——部分帧到达就是等，不需要上层拼缓冲。
- `lenField` 必须在 `[FrameHeaderLen, MaxFrameTotal]` 内，否则直接退出读循环并关连接（`frame.go:106-108`）；这是防畸形长度放大分配的第一道闸。
- 主 Reader 在 `Slice` 后立即 `Release()`，再在子 Reader 上 `Skip(4)`、`Next(4)`、`ReadByte()` 取 sid/op（`frame.go:109-132`）。
- 子 Reader 是零拷贝引用，同时推进并释放主 Reader，所以 `dispatch` 必须在函数内 `Release()`（见 `frame.go:69-71` 的 `dispatch` 注释）。

## 7. TCP 组帧：9 字节帧头 + payload，`wmu` 串行化写

`frame.go:194-208`：

```go
c.wmu.Lock()
defer c.wmu.Unlock()
var hdr [4 + protocol.FrameHeaderLen]byte // len(4)+sid(4)+op(1)
binary.BigEndian.PutUint32(hdr[0:4], uint32(protocol.FrameHeaderLen+len(payload)))
binary.BigEndian.PutUint32(hdr[4:8], sid)
hdr[8] = byte(op)
if _, err := c.c.Writer().WriteBinary(hdr[:]); err != nil {
	return err
}
```

**`wmu` 必须在 `Flush()` 返回后才释放**（`frame.go:76` 的 `Conn` 注释：netpoll `Flush` 并发调用返回 `ErrConcurrentAccess`，且 `WriteBinary` 在 Flush 排空前引用调用方缓冲）。因此 `writeFrame` 返回即为"payload 可安全复用/归还"的时点——服务端 `handleGet` 正是在 `writeFrame` 返回后才 `bufpool.Put(data)`（`server.go:342-347`）。

## 8. 连接关闭用未导出 sentinel `errConnClosed`

`frame.go:34` 定义、`frame.go:169-180` 在 `await` 中作为流结束/连接结束的统一返回：

```go
select {
case msg := <-st.in:
	return msg, nil
case <-st.done:
	return frameMsg{}, errConnClosed
case <-c.closed:
	return frameMsg{}, errConnClosed
case <-ctx.Done():
	return frameMsg{}, ctx.Err()
}
```

流通道 `in` **永不被 close**（发送方与关闭方会竞争），只由 `finish()` 关 `done` 解除阻塞投递（`frame.go:43-59`）。新增流处理器时不要自作主张 `close(st.in)`。

## 9. `dispatch` 的两侧不对称：服务端致命、客户端丢弃

`dispatch` 的所有权约定写在签名注释里（`frame.go:69-71`）：返回 `deliverFatal` 时读循环退出，`sub` 在此函数内 `Release()`。

服务端对"未知流的非首帧"判协议错误并**关整条连接**（`server.go:113-123`）：

```go
if st == nil {
	switch op {
	case protocol.OpPutHeader, protocol.OpGetReq, protocol.OpDelReq, protocol.OpStatReq, protocol.OpPing, protocol.OpMetaReq, protocol.OpSegReq, protocol.OpKeysReq:
		st = newStream(sid)
		c.streams[sid] = st
		started = true
	default:
		c.mu.Unlock()
		_ = sub.Release()
		return deliverFatal // 未知流首帧：协议错误
	}
}
```

客户端对未知/已结束流则**直接丢帧、不中断连接**（`client.go:49-56`）。理由是不对称的：客户端侧流结束是常态（RPC 完成或 ctx 取消），服务端侧未知流意味着对端违约。

## 10. Put 流"全量先行发送"，服务端提前失败必须排空尾帧

客户端 `Put` 是 PutHeader → 全部 PutData → PutEnd **一口气发完**，最后才等响应（`client.go:77-94`）。因此服务端在 PutHeader 之后就失败（超限/零长/PutBegin 失败）时，客户端尾帧仍在途。`drainPutTail`（`server.go:165-196`）专门消费它们，注释写明了不排空的后果（`server.go:167-172`）：

```go
// 客户端 Put 是「全量先行发送」：PutHeader 之后立刻把所有 PutData 与 OpPutEnd 发完，
// 最后才等响应。故服务端在 PutHeader 之后提前失败（对象超限 / 零长 / PutBegin 失败）时，
// 客户端仍有尾帧在途；这些帧必须由本流消费掉——否则它们到达时流已被 endStream 注销、
// op 又不是首帧类型，会被 dispatch 判为「未知流非首帧」协议错误 → deliverFatal → 关闭
// 整条连接（连接一死，其上后续所有 RPC 全部失败；客户端仅轮转选连接，无剔除重建，
// 于是死连接被永久复用，错误随时间线性累积）。
```

**规则**：`handlePut` 新增任何"PutHeader 之后提前返回"的分支，都必须先 `c.drainPutTail(st, size)`（`server.go:219-240` 是现有四处调用）。

## 11. Get 流用 final 位收尾，客户端按 `pos != size` 报短读

`OpGetDataFinal` 是 `OpGetData | 0x80`（`protocol.go:81-84`），服务端在"读满请求窗口或 EOF 短读"时置位（`server.go:336-341`）。客户端收到 final 后校验实际字节数（`client.go:203-209`）：

```go
if final {
	if pos != size {
		dispose()
		return nil, nil, fmt.Errorf("taihu: get short read: got %d want %d", pos, size)
	}
	return out, dispose, nil
}
```

`OpGetEnd` 旧空帧常量保留但不再发送（`protocol.go:75` 注释："已弃用不再发送，保留常量兼容解析"），客户端仍兼容解析（`client.go:210-216`）。空短读时服务端补发空 final 帧（`server.go:349-355`），目的就是让客户端报短读而不是挂死——新增收尾路径时要保持这一性质。

## 12. 管理类 op 只走 TCP 路径

`protocol.go:86` 明确写了范围："管理类 op（admin RPC，首版仅 TCP 路径；shmipc 路径不实现，见 taihu-cli 设计文档 §4）"。管理 op 与数据面**同连接、同帧协议**（`client_admin.go:10`），服务端处理见 `server_admin.go:10-11`。客户端只在连接池里找第一条 `*transport.Conn`（`internal/rpcclient/admin.go:84-92`）：

```go
func (s *Storage) adminConn() (*transport.Conn, error) {
	for _, c := range s.conns {
		if t, ok := c.(*transport.Conn); ok {
			return t, nil
		}
	}
	return nil, errors.New("taihu: admin RPC not supported on this transport (shm only)")
}
```

流式管理响应（Segments / ListKeys）的分帧规则：`OpSegSum → OpSegData* → OpSegEnd`（`server_admin.go:49-95`，明细按 `(ChunkSize-4)/SegItemLen` 每帧上限切分），`OpKeysReq → OpKeysData* → OpResp{code}`（`server_admin.go:97-129`，按 `ChunkSize` 贪心装帧）。

## 13. shm 帧格式不同：无 streamID，数据帧带 4K pad

shmipc 流本身按请求隔离（双向流，客户端 GetStream/PutBack 复用），所以帧头少了 sid 字段（`server_shm_linux.go:4-7`）：

```go
// shmipc 流天然按请求隔离（双向流，客户端 GetStream/PutBack 复用），无 streamID，
// 帧格式退化为 [4B len][1B op][payload]：len = 1 + len(payload)（大端）；
```

数据帧在 op 之后多一段对齐 pad，使负载落在 4K 边界上供服务端 `O_DIRECT` 直读直写（`protocol.go:32-35` 定义 `ShmDataPad = 4096`）。读侧 `shmReadFrame` 只对 `OpGetData/OpGetDataFinal/OpPutData` 跳过 pad（`shm_frame_linux.go:50-55`），写侧 `shmWriteFrame` 对应地把 `dataOff` 置为 `ShmDataPad`（`shm_frame_linux.go:69-76`）。**数据帧判定必须三条 op 全列**，否则 pad 与负载错位——`transport_shmframe_test.go:168-178`（`TestShmFrameConstants`）与 `:111`（`TestShmReadFrameDataSkipsPad`）是这条不变量的门禁。

## 14. shm 出错必须关流：`errShmStreamBroken`

TCP 流用完即关、迟到帧被丢弃；shm 流被客户端 PutBack 复用，残留帧会污染下个请求。所以服务端响应错误后**必须关流**（`server_shm_linux.go:305-311`）：

```go
// shmRespErr 写错误响应并返回哨兵错误（handleStream 据此关闭流）。
// 与 TCP 不同（TCP 流用完即关、迟到帧被丢弃），shm 流被客户端 PutBack 复用，
// 请求未完整消费（超限/畸形）时残留帧会污染流，故错误响应后必须关闭。
func (s *shmServer) shmRespErr(st *shmipc.Stream, op protocol.OpCode, code protocol.ErrCode) error {
	_ = shmWriteFrame(st, op, protocol.EncCode(code))
	return errShmStreamBroken
}
```

`handleStream` 的循环在错误时 `break` 并 `st.Close()`（`server_shm_linux.go:294-299`），未知 op 同样返回该 sentinel（`:290-292`）。另外两个未导出 sentinel：`errShmBadFrame`（`shm_frame_linux.go:27-28`，帧长越界时返回，`:42-44`）、`errConnClosed`（TCP 侧，见 §8）。

## 15. 非 Linux：`ShmSupported()==false` + 占位实现，绝不让启动失败

`server_shm_other.go:14-20`：

```go
// errShmUnsupported shmipc 仅支持 Linux。
var errShmUnsupported = errors.New("taihu: shmipc only supported on linux")

// ShmSupported 报告本平台是否支持 shmipc 共享内存 IPC：非 Linux 恒为 false。
// 调用方（server 启动）据此跳过 shm 服务而非启动失败 —— 本平台 TCP 数据面
// 仍然完整可用，只是少了同机零拷贝那条路径。
func ShmSupported() bool { return false }
```

调用点 `cmd/taihu/cmd/server.go:214-215` 用 `if transport.ShmSupported()` 包住 `ServeShmWithConfig`。客户端侧的对应占位：`internal/rpcclient/dial_shm_other.go:12-22`（`DialShm`/`DialShmPool` 返回 `errShmUnsupported`）、`internal/rpcclient/putwriter_other.go:8-10`（`newShmPut` 返回 `ErrShmOnly`，`ErrShmOnly` 定义在跨平台的 `putwriter.go:10-11`）。**新增 shm 能力时两套文件都要给**：`_linux.go` 真实现 + `_other.go` 返回 sentinel，并跑 `make check-linux`。

## 16. fork 是主模块的一部分，不是 replace 进来的

`go.mod:18-27` 的注释记录了原因，import 路径就是本仓库路径：

```go
// 现在把 fork 直接并入主模块（目录下不再有 go.mod，import 路径为
// github.com/liucxer/taihu/third_party/...），既彻底消除这个对外阻碍，
// 也让构建不再依赖本地路径与网络。
```

对本层的实际约束有两条：

1. **shmipc 的段布局与上游 ABI 不兼容**，`third_party/README.md:162-164` 要求"两端必须同时使用本 fork"——升级 fork 必须客户端与服务端一起升，并做一次真实 Put/Get 往返验证。
2. `internal/transport` 对 fork 新增 API 的依赖有两种形态（`third_party/README.md:27-31`）：直接调用是编译期报错；匿名接口断言（`client.go:183` 的 `ReadCopy`）在上游库上恒为 false，会**静默退化成拷贝路径**。改动这条断言附近代码时，`make check-linux` 不能替代真机往返验证。

## 17. 链路统计是唯一允许的跨层可观测点

`stats.go:12-34` 是一组包级 `atomic.Int64`，`writeFrame` / `shmWriteFrame` / `shmCommitFrame` / `conn.Get` 在热路径只做一次 `Add`；出口是 `DumpStats(w io.Writer)` 与 `StatsString()`（`stats.go:52-66`）。要做到"服务端发送帧尺寸"与"客户端接收帧尺寸"两套计数对得上时，用这两个函数，不要另起日志埋点（`stats.go:14-20` 的注释说明它们就是为证明三类 4MiB 断言而存在）。
