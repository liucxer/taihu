package device

import "github.com/liucxer/taihu/internal/aio"

// Option 是 NewDevice 的可选参数（变参选项）。新增配置项时在此扩展，
// 既有调用点无需改动签名。
type Option func(*options)

// options NewDevice 的可选配置。
type options struct {
	aioMode   aio.Mode
	aioIOPoll bool
	// ring 仅测试注入：非 nil 时替代真实 aio ring（用于构造指定完成事件序列，
	// 如瞬时 EAGAIN）。生产路径恒为 nil。
	ring aio.Ring
}

// defaultOptions 默认走 auto：内核支持 io_uring 就用，否则回退 libaio 并记录原因。
// 实际生效的后端在 aio 层打启动日志（backend=... kernel=...）。
func defaultOptions() options {
	return options{aioMode: aio.ModeAuto}
}

// WithAIOMode 指定异步磁盘 IO 后端：aio.ModeAuto / ModeLibAIO / ModeIOUring。
// ModeIOUring 在内核不支持时返回错误（不静默降级）。
func WithAIOMode(m aio.Mode) Option {
	return func(o *options) { o.aioMode = m }
}

// WithAIOIOPoll 启用 io_uring 的 IORING_SETUP_IOPOLL（仅 io_uring 后端生效）。
// 前置条件：目标块设备队列须开启轮询（/sys/class/block/<dev>/queue/io_poll=1），
// 否则请求会永远停在 iopoll_list 上不完成。
func WithAIOIOPoll(on bool) Option {
	return func(o *options) { o.aioIOPoll = on }
}
