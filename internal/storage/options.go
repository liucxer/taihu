package storage

import "github.com/liucxer/taihu/internal/aio"

// NewStorage 的可选配置私有实现（Option 类型与 WithAIOMode/WithAIOIOPoll 构造
// 集中在 storage.go 导出面；本文件只保留选项结构与默认值）。

// options NewStorage 的可选配置。
type options struct {
	aioMode   aio.Mode
	aioIOPoll bool
}

// defaultOptions 默认走 auto：内核支持 io_uring 就用，否则回退 libaio。
func defaultOptions() options {
	return options{aioMode: aio.ModeAuto}
}
