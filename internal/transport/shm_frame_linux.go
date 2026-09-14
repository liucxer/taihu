// shm 帧原语：shmipc 流上 [4B len][1B op][payload] 帧的读、写、提交。
// 客户端（client_shm_linux.go）与服务端（server_shm_linux.go）共用本文件 —— 这是客户端此前
// 反向依赖「服务端」文件的那部分代码，抽出来才让 client/shm 的角色边界成立。
//
// 与 TCP 路径（frame.go）的帧头长度不同：shmipc 流本身按请求隔离（双向流，
// 客户端 GetStream/PutBack 复用），故没有 streamID 字段。数据帧在 op 之后多一段
// [ShmDataPad-5] 的对齐 pad，使负载恰好落在共享内存的 4K 边界上，服务端因此可以
// 对它直接做 O_DIRECT 读写（免一次 memcpy）；控制帧不带 pad。
package transport

import (
	"encoding/binary"
	"errors"

	"github.com/liucxer/taihu/third_party/shmipc-go"

	"github.com/liucxer/taihu/internal/transport/protocol"
)

// shm 帧常量：长度前缀 4B + op 1B；负载上限与 TCP 路径 ChunkSize 对齐。
const (
	shmLenPrefixLen = 4                             // [4B len]
	shmOpLen        = 1                             // [1B op]
	shmMaxFrameSize = shmOpLen + protocol.ChunkSize // 帧负载（op+payload）上限
)

// errShmBadFrame 畸形 shm 帧。
var errShmBadFrame = errors.New("taihu: bad shm frame")

// shmReadFrame 从 BufferReader 读一帧 [4B len][1B op][payload]，返回 op 与负载切片。
// 单切片时负载零拷贝引用共享内存（fast path）；跨切片时 ReadBytes 慢路径汇入一次拷贝。
// 返回后须由调用方在负载用毕后 ReleasePreviousRead 释放该帧 pin 的共享内存。
//
// 数据帧（OpGetData/OpGetDataFinal/OpPutData）在 op 后带 [ShmDataPad-5] 对齐 pad
// （服务端 O_DIRECT 直读/直写布局），此处跳过 pad 再取负载；控制帧无 pad。
func shmReadFrame(r shmipc.BufferReader) (op protocol.OpCode, payload []byte, err error) {
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
	op = protocol.OpCode(b[0])
	if op == protocol.OpGetData || op == protocol.OpGetDataFinal || op == protocol.OpPutData {

		if _, e := r.ReadBytes(protocol.ShmDataPad - shmLenPrefixLen - shmOpLen); e != nil {
			return 0, nil, e
		}
	}
	payload, err = r.ReadBytes(n - shmOpLen)
	return op, payload, err
}

// shmWriteFrame 向流写一帧并 Flush。数据帧（OpGetData/OpGetDataFinal/OpPutData）统一布局
// [5B 帧头][4091B pad][数据区]：pad 保证数据区 4K 对齐（服务端 O_DIRECT 直读/直写共享内存），
// 直读/拷贝两条路径布局一致，对端 shmReadFrame 跳过 pad。控制帧不带 pad。
// 帧头 + 负载一次 Reserve 进共享内存（数据帧零拷贝直写），Flush 返回后 peer 已可见。
// 同步累加 statTx* 统计（与 TCP writeFrame 对齐）。客户端（client_shm_linux.go）复用。
func shmWriteFrame(st *shmipc.Stream, op protocol.OpCode, payload []byte) error {
	statTxFrames.Add(1)
	statTxBytes.Add(int64(len(payload)))
	dataOff := 0
	if op == protocol.OpGetData || op == protocol.OpGetDataFinal || op == protocol.OpPutData {
		statTxDataFrames.Add(1)
		statTxDataBytes.Add(int64(len(payload)))
		if len(payload) == protocol.ChunkSize {
			statTxData4M.Add(1)
		}
		dataOff = protocol.ShmDataPad
	}

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
func shmCommitFrame(st *shmipc.Stream, head []byte, op protocol.OpCode, size int) error {
	statTxFrames.Add(1)
	statTxBytes.Add(int64(size))
	statTxDataFrames.Add(1)
	statTxDataBytes.Add(int64(size))
	if size == protocol.ChunkSize {
		statTxData4M.Add(1)
	}
	binary.BigEndian.PutUint32(head[:shmLenPrefixLen], uint32(shmOpLen+size))
	head[shmLenPrefixLen] = byte(op)
	return st.Flush(false)
}
