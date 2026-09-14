package storage

import "github.com/liucxer/taihu/internal/ierr"

// 公共错误定义（re-export internal/ierr，保证对外 API 不变）。
// 内部各层的原始定义见 internal/ierr。
var (
	// ErrNotFound 表示 key 不存在。
	ErrNotFound = ierr.ErrNotFound
	// ErrInvalidRange 表示 Get 的读写范围非法（off<0 或 size<0）。
	ErrInvalidRange = ierr.ErrInvalidRange
	// ErrTooLarge 表示对象超过单 segment 上限（不允许跨段）。
	ErrTooLarge = ierr.ErrTooLarge
	// ErrNoSpace 表示无空闲 segment 可写。
	ErrNoSpace = ierr.ErrNoSpace
	// ErrShortWrite 表示设备实际写入字节数少于期望。
	ErrShortWrite = ierr.ErrShortWrite
)
