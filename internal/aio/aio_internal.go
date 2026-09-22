package aio

import (
	"errors"
	"log"
)

// 本文件是「平台无关的包内私有实现」：对外面文件 aio.go 之外的常量、变量与辅助函数。
//
// 判据见 .trellis/spec/architecture/api-surface.md 规则 4 —— aio.go 里只应有导出名，
// 任何未导出的顶层声明都挪到这里（平台专有的那部分见 aio_linux.go / aio_other.go /
// aio_uring_linux.go 与 probe_linux.go / probe_other.go）。

// envMode 环境变量兜底开关（仅在 ModeAuto 下生效，便于线上紧急回退；命令行优先）。
const envMode = "TAIHU_AIO_URING"

// errInvalidMaxEvents 表示 NewWithOptions 的 maxEvents 超出内核允许范围。
var errInvalidMaxEvents = errors.New("aio: maxEvents must be in [1, 65536]")

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
