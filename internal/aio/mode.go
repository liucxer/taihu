package aio

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// Mode 选择异步磁盘 IO 的后端实现。
type Mode int

const (
	// ModeAuto 自动探测：内核支持 io_uring 则用，否则回退 libaio 并记录原因。
	ModeAuto Mode = iota
	// ModeLibAIO 强制 Linux 原生 AIO（libaio 内核接口）。
	ModeLibAIO
	// ModeIOUring 强制 io_uring；内核不支持时返回错误（不静默降级）。
	ModeIOUring
)

// String 实现 fmt.Stringer，用于启动日志标识实际生效的后端。
func (m Mode) String() string {
	switch m {
	case ModeAuto:
		return "auto"
	case ModeLibAIO:
		return "libaio"
	case ModeIOUring:
		return "io_uring"
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

// ParseMode 解析命令行取值：auto（默认）|on|off。on 等价 ModeIOUring，off 等价 ModeLibAIO。
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return ModeAuto, nil
	case "on":
		return ModeIOUring, nil
	case "off":
		return ModeLibAIO, nil
	}
	return ModeAuto, fmt.Errorf("aio: invalid io-uring mode %q (want auto|on|off)", s)
}

// EnvMode 环境变量兜底开关（仅在 ModeAuto 下生效，便于线上紧急回退；命令行优先）。
const EnvMode = "TAIHU_AIO_URING"

// Options 创建异步 IO 队列的参数。
type Options struct {
	// Mode 后端选择，零值为 ModeAuto。
	Mode Mode
	// IOPoll 启用 IORING_SETUP_IOPOLL，仅 io_uring 后端有效。
	// 前置条件：目标块设备队列须开启轮询（/sys/class/block/<dev>/queue/io_poll=1），
	// 否则请求会永远停在 iopoll_list 上不完成（blk_poll 直接返回 0）。
	IOPoll bool
}

// NewWithOptions 按指定参数创建异步 IO 队列。
// Linux 上 maxEvents 为队列深度上限（1..65536）；非 Linux 平台忽略上限语义。
func NewWithOptions(maxEvents int, o Options) (Ring, error) {
	if o.Mode == ModeAuto {
		if v := os.Getenv(EnvMode); v != "" {
			m, err := ParseMode(v)
			if err != nil {
				return nil, fmt.Errorf("%s=%q: %w", EnvMode, v, err)
			}
			o.Mode = m
		}
	}

	switch o.Mode {
	case ModeIOUring:
		r, err := newIOUringRing(maxEvents, o.IOPoll)
		if err != nil {
			return nil, err
		}
		logBackend(r, "forced on"+iopollSuffix(o.IOPoll))
		return r, nil
	case ModeLibAIO:
		r, err := newLibAIORing(maxEvents)
		if err != nil {
			return nil, err
		}
		logBackend(r, "forced off")
		return r, nil
	}

	// ModeAuto：运行期探测（不比较版本号，见 Probe）。
	info := Probe()
	if info.Supported {
		r, err := newIOUringRing(maxEvents, o.IOPoll)
		if err != nil {
			return nil, err
		}
		logBackend(r, fmt.Sprintf("auto: probe ok, features=0x%x%s",
			info.Features, iopollSuffix(o.IOPoll)))
		return r, nil
	}
	r, err := newLibAIORing(maxEvents)
	if err != nil {
		return nil, err
	}
	logBackend(r, "auto: io_uring 不可用 — "+info.Reason)
	return r, nil
}

// iopollSuffix 把 IOPOLL 状态拼进启动日志（仅在启用时出现）。
func iopollSuffix(on bool) string {
	if on {
		return " iopoll=on"
	}
	return ""
}

// logBackend 打一行启动日志标明实际生效的后端与判定原因。双内核并存期，
// 这行是排障时唯一能确定「跑的是哪个后端」的证据，故队列深度取自**实际建出的
// ring**，而不是探测时的临时 ring（探测用 64 条，与真实深度无关）。
func logBackend(r Ring, why string) {
	if sq, cq, ok := ringQueueDepth(r); ok {
		log.Printf("taihu: aio backend=%s sq=%d cq=%d kernel=%s (%s)",
			backendName(r), sq, cq, kernelRelease(), why)
		return
	}
	log.Printf("taihu: aio backend=%s kernel=%s (%s)", backendName(r), kernelRelease(), why)
}

// NewWithMode 是 NewWithOptions 的简化形式（不启用 IOPOLL）。
func NewWithMode(maxEvents int, m Mode) (Ring, error) {
	return NewWithOptions(maxEvents, Options{Mode: m})
}
