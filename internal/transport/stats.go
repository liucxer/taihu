package transport

import (
	"fmt"
	"io"
	"strings"
	"sync/atomic"
)

// 链路帧尺寸统计（压测/验证用，原子计数，热路径仅一次 Add，开销可忽略）。
// 用于证明三类 4MiB 断言：服务端回数据帧、客户端收数据帧、磁盘 IO（后者在 device 层）。
var (
	// 服务端发送（conn.writeFrame 全量统计）。
	statTxFrames     atomic.Int64 // 已发送帧总次数
	statTxBytes      atomic.Int64 // 已发送帧负载总字节
	statTxDataFrames atomic.Int64 // 其中数据帧（opGetData）次数
	statTxDataBytes  atomic.Int64 // 其中数据帧负载总字节
	statTxData4M     atomic.Int64 // 其中负载恰为 4MiB 的整帧数

	// 客户端接收（conn.Get 数据帧统计）。
	statRxFrames atomic.Int64 // 收到数据帧次数
	statRxBytes  atomic.Int64 // 收到数据帧负载总字节
	statRxData4M atomic.Int64 // 其中负载恰为 4MiB 的帧数
	statRxTake   atomic.Int64 // TakeTry 零拷贝移交命中帧数
	statRxCopy   atomic.Int64 // ReadCopy/Next 回退拷贝帧数
)

// DumpStats 打印链路帧尺寸统计到 w。
func DumpStats(w io.Writer) {
	fmt.Fprintf(w, "transport-tx frames=%d bytes=%d dataFrames=%d dataBytes=%d data4MiB=%d\n",
		statTxFrames.Load(), statTxBytes.Load(), statTxDataFrames.Load(), statTxDataBytes.Load(), statTxData4M.Load())
	fmt.Fprintf(w, "transport-rx dataFrames=%d dataBytes=%d data4MiB=%d take=%d copy=%d\n",
		statRxFrames.Load(), statRxBytes.Load(), statRxData4M.Load(), statRxTake.Load(), statRxCopy.Load())
}

// StatsString 返回链路帧尺寸统计的多行文本（供日志单行拼接）。
func StatsString() string {
	var b strings.Builder
	DumpStats(&b)
	return b.String()
}
