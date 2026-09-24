// Package ierr 定义 taihu 存储的公共错误 —— **唯一事实源**。
// 内部各层（aio/device/metastore/storage/transport/protocol）与对外 SDK
// （pkg/taihu-client）直接使用本包错误；包位于 pkg/ 下，外部调用方可用
// errors.Is 判定同一批哨兵。internal/ 下的包不得自建错误别名层。
package ierr

import "errors"

var (
	// ErrNotFound 表示 key 不存在。
	ErrNotFound = errors.New("taihu: key not found")
	// ErrInvalidRange 表示 Get 的读写范围非法（off<0 或 size<0）。
	ErrInvalidRange = errors.New("taihu: invalid range")
	// ErrTooLarge 表示对象超过单 segment 上限（不允许跨段）。
	ErrTooLarge = errors.New("taihu: object too large, exceeds segment size")
	// ErrNoSpace 表示无空闲 segment 可写。
	ErrNoSpace = errors.New("taihu: no free segment")
	// ErrShortWrite 表示设备实际写入字节数少于期望。
	ErrShortWrite = errors.New("taihu: short write")
	// ErrConflict 表示条件写（CAS）失败：当前映射与期望不符（并发 Put/Delete 竞态），
	// 调用方应跳过本次操作并重试。compaction 搬移使用。
	ErrConflict = errors.New("taihu: mapping conflict")
	// ErrFull 表示提交队列已满（io_submit 返回 EAGAIN），应先 Wait 取回完成事件后重试。
	ErrFull = errors.New("taihu: submission queue full")
	// ErrTimeout 表示 Wait 在超时时间内未取够 min 个事件。
	ErrTimeout = errors.New("taihu: wait timeout")
	// ErrInvalidMaxEvents 表示 aio 队列深度 Options.MaxEvents 超出合法区间 [1, 65536]。
	ErrInvalidMaxEvents = errors.New("aio: maxEvents must be in [1, 65536]")
	// ErrUringLinuxOnly 表示 io_uring 后端仅在 Linux 上可用。
	ErrUringLinuxOnly = errors.New("aio: io_uring 仅 Linux 支持")
	// ErrIOPOLLLinuxOnly 表示 IOPOLL 仅在 Linux 上可用。
	ErrIOPOLLLinuxOnly = errors.New("aio: IOPOLL 仅 Linux 支持")

	// ErrInvalidArgument 表示协议参数非法。
	ErrInvalidArgument = errors.New("taihu: invalid argument")
	// ErrRPCError 表示 RPC 调用方错误（对端返回错误码）。
	ErrRPCError = errors.New("taihu: rpc error")
	// ErrKeyTooLong 表示 key 超过协议允许长度。
	ErrKeyTooLong = errors.New("taihu: key too long")
	// ErrConnClosed 表示连接已关闭后仍尝试收发。
	ErrConnClosed = errors.New("taihu: connection closed")
	// ErrShmBadFrame 表示畸形 shm 帧。
	ErrShmBadFrame = errors.New("taihu: bad shm frame")
	// ErrShmStreamBroken 表示 shm 流上残留未消费请求帧，无法复用，须关闭通知对端。
	ErrShmStreamBroken = errors.New("taihu: shm stream broken")
	// ErrShmUnsupported 表示 shmipc 仅 Linux 支持（server/dial 两侧共用同一哨兵）。
	ErrShmUnsupported = errors.New("taihu: shmipc only supported on linux")
	// ErrShmOnly 表示零拷贝写（NewPut）仅共享内存连接支持。
	ErrShmOnly = errors.New("taihu: zero-copy write requires shm connection")
	// ErrAdminShmOnly 表示管理 RPC 仅共享内存传输支持。
	ErrAdminShmOnly = errors.New("taihu: admin RPC not supported on this transport (shm only)")
	// ErrStorageClosed 表示对象存储已关闭后仍尝试写入。
	ErrStorageClosed = errors.New("taihu: storage closed")
	// ErrDeviceClosed 表示设备已关闭后仍尝试提交。
	ErrDeviceClosed = errors.New("taihu: device closed")
	// ErrNoInstances 表示集群无在线实例可写。
	ErrNoInstances = errors.New("taihu-cluster: no online instance")
	// ErrSourceUnset 表示集群全 miss 且未配置回源。
	ErrSourceUnset = errors.New("taihu-cluster: no source getter configured")
	// ErrKVRequired 表示配置集群模式却未提供 KV（TiKV）客户端。
	ErrKVRequired = errors.New("taihuclient: KV is required")
	// ErrNoShmAddr 表示 shm 传输要求候选实例与本进程同主机，但实例无共享内存地址。
	ErrNoShmAddr = errors.New("taihuclient: instance has no shm addr (transport=shm requires same-host instance)")
)
