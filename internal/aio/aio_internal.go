package aio

import (
	"errors"
	"log"
)

// 本文件是「平台无关的包内私有实现」：对外面文件 aio.go 之外的常量、变量与辅助函数。
//
// 判据：aio.go 里只应有导出名，任何未导出的顶层声明都挪到这里 —— 这样 aio.go 可以
// 当作本包的对外契约整篇读，不必在实现细节里挑出可导出的部分（平台专有的那部分见
// aio_linux.go / aio_other.go / aio_uring_linux.go 与 probe_linux.go / probe_other.go）。

// envMode 环境变量兜底开关（仅在 ModeAuto 下生效，便于线上紧急回退；命令行优先）。
const envMode = "TAIHU_AIO_URING"

// errInvalidMaxEvents 表示 Options.MaxEvents 超出内核允许范围。
var errInvalidMaxEvents = errors.New("aio: maxEvents must be in [1, 65536]")

// info 描述 io_uring 可用性探测结果。是 probe() 的产出形状，同时被结论缓存
// （probe_cache.go 的 probeInfo）与选路方（NewWithOptions）消费；
// 包外只经由 NewWithOptions 的选路间接依赖该结论，故不导出。
type info struct {
	Supported     bool   // 当前内核是否可用 io_uring
	Reason        string // 人类可读原因（供日志与错误信息）
	KernelRelease string // 内核版本字符串，仅供日志
	SQEntries     uint32 // 内核回填的提交队列深度
	CQEntries     uint32 // 内核回填的完成队列深度
	Features      uint32 // 内核能力位（IORING_FEAT_*）
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
