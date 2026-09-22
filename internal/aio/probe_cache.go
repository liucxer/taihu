package aio

import "sync"

// 本文件是「平台无关的包内私有实现」：Probe 的结论缓存。
//
// 为什么单独成文件：未导出符号不进对外面文件（aio.go），否则 aio.go 就不再能当作
// 本包的对外契约整篇读；平台无关的缓存逻辑也不进 probe_linux.go / probe_other.go ——
// 那两个文件只留真正的平台差异。对外入口 Probe 在 aio.go，是一层薄转发。

var (
	probeMu   sync.Mutex
	probeInfo Info
	probeDone bool
)

// probeCached 是 Probe 的实现体：缓存探测结论，并把平台差异转发给 probe()。
//
// 只缓存确定性结论 —— 第二个返回值 deterministic 为真才落缓存。资源类瞬时错误
// （ENOMEM/EMFILE 等）不缓存，否则一次偶发失败会把进程永久钉死在 libaio 上。
// deterministic 由平台实现给出（见 probe_linux.go / probe_other.go）。
func probeCached() Info {
	probeMu.Lock()
	defer probeMu.Unlock()
	if probeDone {
		return probeInfo
	}
	info, deterministic := probe()
	if info.Supported || deterministic {
		probeInfo, probeDone = info, true
	}
	return info
}
