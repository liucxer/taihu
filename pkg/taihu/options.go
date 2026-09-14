package taihu

import "github.com/liucxer/taihu/internal/aio"

// Option 是 NewStorage 的可选参数（变参选项）。新增配置项时在此扩展，
// 既有调用点无需改动签名。
type Option func(*options)

// options NewStorage 的可选配置。
type options struct {
	aioMode   aio.Mode
	aioIOPoll bool
}

// defaultOptions 默认走 auto：内核支持 io_uring 就用，否则回退 libaio。
func defaultOptions() options {
	return options{aioMode: aio.ModeAuto}
}

// WithAIOMode 指定底层磁盘异步 IO 后端：aio.ModeAuto / ModeLibAIO / ModeIOUring。
// 由命令行 -io-uring=auto|on|off 映射而来（on→ModeIOUring，off→ModeLibAIO）。
// ModeIOUring 在内核不支持时启动失败（不静默降级）。
func WithAIOMode(m aio.Mode) Option {
	return func(o *options) { o.aioMode = m }
}

// WithAIOIOPoll 启用 io_uring 的 IOPOLL 模式（仅 io_uring 后端生效）。默认关闭：
// 它需要块设备队列开启轮询，且完成靠内核忙等推进（占一个核），收益需实测。
func WithAIOIOPoll(on bool) Option {
	return func(o *options) { o.aioIOPoll = on }
}
