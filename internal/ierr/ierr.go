// Package ierr 定义 taihu 存储的公共错误。
// 内部各层（device/metastore）直接使用本包错误，对外根包 re-export（见 taihu/error.go），
// 保证调用方感知的 API 不变。
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
)
