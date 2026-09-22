// Package ierr 定义 taihu 存储的公共错误 —— **唯一事实源**。
// 内部各层（aio/device/metastore/storage/transport/protocol）直接使用本包错误；
// 面向客户端的 re-export 只有一处：internal/rpcclient/reexport.go（pkg/taihu-client/reexport.go
// 再转指一层）。internal/ 下的包不得自建错误别名层。
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
)
