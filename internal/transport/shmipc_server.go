//go:build linux

// 共享内存 IPC 服务端（shmipc-go）：数据面共享内存零拷贝，控制面 unix socket。
// 与 TCP 服务端（server.go）并存，TCP 路径零改动，本文件为纯增量。
//
// shmipc 流天然按请求隔离（双向流，客户端 GetStream/PutBack 复用），无 streamID，
// 帧格式退化为 [4B len][1B op][payload]：len = 1 + len(payload)（大端）。
// 请求处理逻辑镜像 TCP 路径 handlePut/handleGet/handleDelete/handleStat，
// 复用 parse* 纯函数（byteReader 解耦后 sliceReader 适配共享内存切片）与
// storage.Put/ReadAt/Delete/Stat；共享内存按帧 Reserve，Flush 后 peer 即可读。
package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"

	"github.com/cloudwego/shmipc-go"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/pkg/taihu"
)

// shm 帧常量：长度前缀 4B + op 1B；负载上限与 TCP 路径 chunkSize 对齐。
const (
	shmLenPrefixLen = 4               // [4B len]
	shmOpLen        = 1               // [1B op]
	shmMaxFrameSize = shmOpLen + chunkSize // 帧负载（op+payload）上限
)

// errShmBadFrame 畸形 shm 帧。
var errShmBadFrame = errors.New("taihu: bad shm frame")

// shmReadFrame 从 BufferReader 读一帧 [4B len][1B op][payload]，返回 op 与负载切片。
// 单切片时负载零拷贝引用共享内存（fast path）；跨切片时 ReadBytes 慢路径汇入一次拷贝。
// 返回后须由调用方在负载用毕后 ReleasePreviousRead 释放该帧 pin 的共享内存。
//
// 数据帧（opGetData/opGetDataFinal/opPutData）在 op 后带 [shmDataPad-5] 对齐 pad
// （服务端 O_DIRECT 直读/直写布局），此处跳过 pad 再取负载；控制帧无 pad。
func shmReadFrame(r shmipc.BufferReader) (op OpCode, payload []byte, err error) {
	lb, err := r.ReadBytes(shmLenPrefixLen)
	if err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(lb))
	if n < shmOpLen || n > shmMaxFrameSize {
		return 0, nil, errShmBadFrame
	}
	b, err := r.ReadBytes(shmOpLen)
	if err != nil {
		return 0, nil, err
	}
	op = OpCode(b[0])
	if op == opGetData || op == opGetDataFinal || op == opPutData {
		// 数据帧：跳过对齐 pad，payload 从数据区起始（4K 对齐）读 n-1 字节。
		if _, e := r.ReadBytes(shmDataPad - shmLenPrefixLen - shmOpLen); e != nil {
			return 0, nil, e
		}
	}
	payload, err = r.ReadBytes(n - shmOpLen)
	return op, payload, err
}

// shmWriteFrame 向流写一帧并 Flush。数据帧（opGetData/opGetDataFinal/opPutData）统一布局
// [5B 帧头][4091B pad][数据区]：pad 保证数据区 4K 对齐（服务端 O_DIRECT 直读/直写共享内存），
// 直读/拷贝两条路径布局一致，对端 shmReadFrame 跳过 pad。控制帧不带 pad。
// 帧头 + 负载一次 Reserve 进共享内存（数据帧零拷贝直写），Flush 返回后 peer 已可见。
// 同步累加 statTx* 统计（与 TCP writeFrame 对齐）。客户端（shmipc_client.go）复用。
func shmWriteFrame(st *shmipc.Stream, op OpCode, payload []byte) error {
	statTxFrames.Add(1)
	statTxBytes.Add(int64(len(payload)))
	dataOff := 0
	if op == opGetData || op == opGetDataFinal || op == opPutData {
		statTxDataFrames.Add(1)
		statTxDataBytes.Add(int64(len(payload)))
		if len(payload) == chunkSize {
			statTxData4M.Add(1)
		}
		dataOff = shmDataPad
	}
	// 数据帧 dataOff=shmDataPad 已含 [5B 帧头 + pad]（payload 从 shmDataPad 起）；
	// 控制帧 dataOff=0 需补 [4B len][1B op] 帧头。Reserve 长度必须与对端实际读取量
	// 一致（帧头+pad+payload），否则对端 size()>0 导致 ReleasePreviousRead 不归还切片。
	total := dataOff + len(payload)
	if dataOff == 0 {
		total += shmLenPrefixLen + shmOpLen
	}
	buf, err := st.BufferWriter().Reserve(total)
	if err != nil {
		return err
	}
	binary.BigEndian.PutUint32(buf[:shmLenPrefixLen], uint32(shmOpLen+len(payload)))
	buf[shmLenPrefixLen] = byte(op)
	// payload 起始：数据帧 = shmDataPad（[0:5] 帧头 + [5:4096] pad 均在前 4096B 内）；
	// 控制帧 = 帧头后（[5:]）。
	payloadOff := dataOff
	if dataOff == 0 {
		payloadOff = shmLenPrefixLen + shmOpLen
	}
	copy(buf[payloadOff:], payload)
	return st.Flush(false)
}

// shmCommitFrame 写数据帧帧头 [4B len][1B op] 并 Flush（数据已由调用方直写进
// reserve 区，不再 copy）。head 为帧首区域 [0:5] 的共享内存引用（客户端零拷贝写
// 时由 Reserve 返回的整区前 5B 提供）。同步累加 statTx* 统计（与 shmWriteFrame 对齐）。
func shmCommitFrame(st *shmipc.Stream, head []byte, op OpCode, size int) error {
	statTxFrames.Add(1)
	statTxBytes.Add(int64(size))
	statTxDataFrames.Add(1)
	statTxDataBytes.Add(int64(size))
	if size == chunkSize {
		statTxData4M.Add(1)
	}
	binary.BigEndian.PutUint32(head[:shmLenPrefixLen], uint32(shmOpLen+size))
	head[shmLenPrefixLen] = byte(op)
	return st.Flush(false)
}

// shmServer 共享内存 IPC 服务端。
type shmServer struct {
	storage *taihu.Storage
	ln      *net.UnixListener
	conf    *shmipc.Config

	wg        sync.WaitGroup
	closed    chan struct{}
	closeOnce sync.Once
}

// ServeShm 在 unix socket 路径 uds 上提供 shmipc 服务，返回 io.Closer 关闭服务。
// 与 TCP 监听（Server.Serve）互不干扰，可同时启用。共享内存由客户端创建并传入
// （MemFd），服务端仅映射，因此本处配置只须通过 shmipc.VerifyConfig。
func ServeShm(storage *taihu.Storage, uds string) (io.Closer, error) {
	conf := shmipc.DefaultConfig()
	_ = os.Remove(uds)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: uds, Net: "unix"})
	if err != nil {
		return nil, err
	}
	s := &shmServer{
		storage: storage,
		ln:      ln,
		conf:    conf,
		closed:  make(chan struct{}),
	}
	go s.acceptLoop()
	return s, nil
}

// Close 关闭 unix listener 并等待全部连接/流处理 goroutine 退出。
func (s *shmServer) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	_ = s.ln.Close()
	s.wg.Wait()
	return nil
}

// acceptLoop 接受 unix socket 连接，每连接一个 shmipc Session。
func (s *shmServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // 监听关闭或异常
		}
		select {
		case <-s.closed:
			_ = conn.Close()
			return
		default:
		}
		s.wg.Add(1)
		go s.serveConn(conn)
	}
}

// serveConn 单连接服务循环：接受流并逐流起 goroutine 处理。
func (s *shmServer) serveConn(conn net.Conn) {
	defer s.wg.Done()
	session, err := shmipc.Server(conn, s.conf)
	if err != nil {
		return
	}
	defer session.Close()
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func(st *shmipc.Stream) {
			defer s.wg.Done()
			s.handleStream(st)
		}(stream)
	}
}

// handleStream 单流请求循环：流被客户端 PutBack 复用（不关闭），服务端持续读下一
// 请求。每请求帧处理镜像 TCP 路径对应 handler；响应写毕后释放本请求帧 pin 的
// 共享内存（读缓冲与写缓冲相互独立，无冲突）。任何错误（协议畸形/写失败）都
// st.Close() 通知对端流关闭，客户端池将丢弃该流（下次 GetStream 自动重开），
// 避免残留未消费帧污染可复用流导致对端阻塞。
func (s *shmServer) handleStream(st *shmipc.Stream) {
	r := st.BufferReader()
	var err error
	for {
		op, payload, rerr := shmReadFrame(r)
		if rerr != nil {
			err = rerr
			break
		}
		switch op {
		case opPutHeader:
			err = s.handleShmPut(st, r, payload)
		case opGetReq:
			err = s.handleShmGet(st, payload)
		case opDelReq:
			err = s.handleShmDelete(st, payload)
		case opStatReq:
			err = s.handleShmStat(st, payload)
		default:
			err = errShmStreamBroken
		}
		// 释放本请求帧（及 Put 逐帧消费后遗留）pin 的共享内存。
		r.ReleasePreviousRead()
		if err != nil {
			break
		}
	}
	_ = st.Close()
}

// errShmStreamBroken 哨兵错误：流上残留未消费请求帧，无法继续复用，须关闭通知对端。
var errShmStreamBroken = errors.New("taihu: shm stream broken")

// shmRespErr 写错误响应并返回哨兵错误（handleStream 据此关闭流）。
// 与 TCP 不同（TCP 流用完即关、迟到帧被丢弃），shm 流被客户端 PutBack 复用，
// 请求未完整消费（超限/畸形）时残留帧会污染流，故错误响应后必须关闭。
func (s *shmServer) shmRespErr(st *shmipc.Stream, op OpCode, code errCode) error {
	_ = shmWriteFrame(st, op, encCode(code))
	return errShmStreamBroken
}

// handleShmPut 处理 Put 请求流：PutHeader → PutData* → PutEnd，语义镜像 TCP handlePut。
// 每个 PutData 帧消费后即 ReleasePreviousRead（共享内存环形复用，避免长对象堆积 pin）。
func (s *shmServer) handleShmPut(st *shmipc.Stream, r shmipc.BufferReader, payload []byte) error {
	key, size, err := parsePutHeader(newSliceReader(payload))
	if err != nil {
		return s.shmRespErr(st, opResp, codeInvalidArgument)
	}
	if size < 0 {
		return s.shmRespErr(st, opResp, codeInvalidArgument)
	}
	if size > s.storage.MaxObjectSize() {
		return s.shmRespErr(st, opResp, codeTooLarge)
	}
	if size == 0 {
		// 消费 PutEnd（客户端恒发），保持流干净可复用。
		op, _, err := shmReadFrame(r)
		if err != nil {
			return err
		}
		if op != opPutEnd {
			return s.shmRespErr(st, opResp, codeInvalidArgument)
		}
		seg, off, err := s.storage.PutBegin(context.Background(), key, 0)
		if err != nil {
			return s.shmRespErr(st, opResp, mapStorageErr(err))
		}
		if err := s.storage.PutCommit(context.Background(), key, seg, off, 0); err != nil {
			return s.shmRespErr(st, opResp, mapStorageErr(err))
		}
		return shmWriteFrame(st, opResp, encCode(codeOK))
	}

	// 分段直写：PutHeader 分配段游标，每个 PutData 帧（payload 4K 对齐）直接
	// O_DIRECT 直写共享内存切片（免 bufpool 汇集拷贝），IO 完成后即时归还本帧切片。
	seg, off, err := s.storage.PutBegin(context.Background(), key, size)
	if err != nil {
		return s.shmRespErr(st, opResp, mapStorageErr(err))
	}
	var pos int64
	for {
		op, p, err := shmReadFrame(r)
		if err != nil {
			return err
		}
		switch op {
		case opPutData:
			if pos+int64(len(p)) > size {
				return s.shmRespErr(st, opResp, codeInvalidArgument)
			}
			if err := s.storage.PutAppend(context.Background(), seg, off+pos, int64(len(p)), p); err != nil {
				return s.shmRespErr(st, opResp, mapStorageErr(err))
			}
			pos += int64(len(p))
			// 释放本帧 pin；当前帧为 front 切片，在下次 shmReadFrame 时才移入 pinnedList。
			r.ReleasePreviousRead()
		case opPutEnd:
			if pos != size {
				return s.shmRespErr(st, opResp, codeInvalidArgument)
			}
			if err := s.storage.PutCommit(context.Background(), key, seg, off, size); err != nil {
				return s.shmRespErr(st, opResp, mapStorageErr(err))
			}
			return shmWriteFrame(st, opResp, encCode(codeOK))
		default:
			return s.shmRespErr(st, opResp, codeInvalidArgument)
		}
	}
}

// handleShmGet 处理 Get 请求：按 chunkSize 分块读下发 opGetData，末帧置 final 位
// （opGetDataFinal）收尾。语义镜像 TCP handleGet。
//
// 两条数据帧路径（布局一致，客户端 shmReadFrame 统一跳 pad）：
//   - 直读快路径（off 4K 对齐，skip==0）：O_DIRECT 直读共享内存切片数据区，
//     免 bufpool→共享内存 memcpy（读路径零拷贝）。
//   - 回退路径（off 非对齐）：Storage.ReadAt 读入 bufpool 对齐缓冲，shmWriteFrame 拷贝。
func (s *shmServer) handleShmGet(st *shmipc.Stream, payload []byte) error {
	key, off, size, err := parseGetReq(newSliceReader(payload))
	if err != nil {
		return shmWriteFrame(st, opGetErr, encCode(codeInvalidArgument))
	}
	if size == -1 {
		total, err := s.storage.Stat(context.Background(), key)
		if err != nil {
			return shmWriteFrame(st, opGetErr, encCode(mapStorageErr(err)))
		}
		size = total - off
	}
	if size < 0 {
		return shmWriteFrame(st, opGetErr, encCode(codeInvalidRange))
	}
	pos, end := off, off+size
	for pos < end {
		want := end - pos
		if want > chunkSize {
			want = chunkSize
		}
		// 直读快路径：pos 与 want 均 4K 对齐（帧 Reserve 长度 = pad+dlen = pad+want，
		// 对端读满后 size()==0 归还切片）。尾帧（want 非 4K 对齐）走回退路径（精确
		// Reserve + 拷贝，dlen 对齐余量会导致对端读不满泄漏切片）。
		if pos%layout.BlockSize == 0 && want%layout.BlockSize == 0 {
			n, rerr := shmWriteDataFrameDirect(st, s.storage, key, pos, want, end)
			if rerr != nil {
				if rerr == io.EOF {
					return nil // final 帧（含空短读兜底）已发，读完毕
				}
				return shmWriteFrame(st, opGetErr, encCode(mapStorageErr(rerr)))
			}
			pos += n
			continue
		}
		// 回退路径：非对齐 off，bufpool 读 + 拷贝写。
		data, rerr := s.storage.ReadAt(context.Background(), key, pos, want)
		if len(data) > 0 {
			op := OpCode(opGetData)
			if pos+int64(len(data)) >= end || rerr == io.EOF {
				op = opGetDataFinal
			}
			if serr := shmWriteFrame(st, op, data); serr != nil {
				bufpool.Put(data)
				return serr
			}
			bufpool.Put(data)
			pos += int64(len(data))
		}
		if rerr == io.EOF {
			if len(data) == 0 {
				// 空短读兜底：发空 final 帧让客户端报 short read，避免客户端挂死。
				_ = shmWriteFrame(st, opGetDataFinal, nil)
			}
			return nil
		}
		if rerr != nil {
			return shmWriteFrame(st, opGetErr, encCode(mapStorageErr(rerr)))
		}
	}
	return nil
}

// shmWriteDataFrameDirect O_DIRECT 直读共享内存的数据帧写（免 memcpy 快路径）：
// Reserve 对齐切片后先写 opGetErr 占位帧头，数据区 [shmDataPad:shmDataPad+dlen] 作为
// O_DIRECT 目标缓冲直接 DMA 进共享内存（storage.ReadAtInto 同步等待完成）；成功则
// 更新帧头为数据帧（len 按实际读入 n 更新，短读/EOF 时 n < want）。返回实际 payload
// 字节数 n。
//
// 空短读（n==0 && EOF）与直读失败（错误码帧已发）均返回 (0, io.EOF)：前者发空 final
// 帧（对端报 short read），后者已发 opGetErr 错误帧；调用方按 EOF 收尾即可，不再追加
// 错误帧（避免未写帧头的直读切片污染流）。
func shmWriteDataFrameDirect(st *shmipc.Stream, storage *taihu.Storage, key string, pos, want, end int64) (int64, error) {
	dlen := layout.Align4k(want)
	buf, err := st.BufferWriter().Reserve(shmDataPad + int(dlen))
	if err != nil {
		return 0, err
	}
	// 占位错误帧头 [4B len=5][1B op=opGetErr][4B 错误码]：直读失败时客户端直接收错误帧，
	// 不会读到未写帧头的垃圾切片。
	binary.BigEndian.PutUint32(buf[:shmLenPrefixLen], uint32(shmOpLen+4))
	buf[shmLenPrefixLen] = byte(opGetErr)
	copy(buf[shmLenPrefixLen+shmOpLen:], encCode(codeInternal))
	ctx := context.Background()
	n, rerr := storage.ReadAtInto(ctx, key, pos, want, buf[shmDataPad:shmDataPad+dlen])
	if rerr != nil && rerr != io.EOF {
		// 直读失败：更新错误码后 Flush 错误帧，复用 EOF 收尾语义（错误帧已发）。
		copy(buf[shmLenPrefixLen+shmOpLen:], encCode(mapStorageErr(rerr)))
		statTxFrames.Add(1)
		statTxBytes.Add(4)
		if err := st.Flush(false); err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	// 成功：更新帧头为数据帧，len = op(1) + payload 字节数；final 帧按读完毕判定。
	op := byte(opGetData)
	if pos+n >= end || rerr == io.EOF {
		op = byte(opGetDataFinal)
	}
	binary.BigEndian.PutUint32(buf[:shmLenPrefixLen], uint32(shmOpLen+n))
	buf[shmLenPrefixLen] = op
	// 同步累加 statTx* 统计（与 shmWriteFrame 对齐）。
	statTxFrames.Add(1)
	statTxBytes.Add(n)
	statTxDataFrames.Add(1)
	statTxDataBytes.Add(n)
	if n == chunkSize {
		statTxData4M.Add(1)
	}
	if err := st.Flush(false); err != nil {
		return 0, err
	}
	return n, rerr
}

// handleShmDelete 处理 Delete 请求（一元），语义镜像 TCP handleDelete。
func (s *shmServer) handleShmDelete(st *shmipc.Stream, payload []byte) error {
	key, err := parseKeyReq(newSliceReader(payload))
	if err != nil {
		return shmWriteFrame(st, opResp, encCode(codeInvalidArgument))
	}
	if err := s.storage.Delete(context.Background(), key); err != nil {
		return shmWriteFrame(st, opResp, encCode(mapStorageErr(err)))
	}
	return shmWriteFrame(st, opResp, encCode(codeOK))
}

// handleShmStat 处理 Stat 请求（一元）：成功回 opStatResp{size}，失败回 opResp{code}。
func (s *shmServer) handleShmStat(st *shmipc.Stream, payload []byte) error {
	key, err := parseKeyReq(newSliceReader(payload))
	if err != nil {
		return shmWriteFrame(st, opResp, encCode(codeInvalidArgument))
	}
	size, err := s.storage.Stat(context.Background(), key)
	if err != nil {
		return shmWriteFrame(st, opResp, encCode(mapStorageErr(err)))
	}
	p := make([]byte, 8)
	binary.BigEndian.PutUint64(p, uint64(size))
	return shmWriteFrame(st, opStatResp, p)
}
