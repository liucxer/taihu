package aio

import "sync"

// 本文件是「平台无关的包内私有实现」：io_uring 可用性探测的结论缓存。
//
// 为什么单独成文件：未导出符号不进对外面文件（aio.go），否则 aio.go 就不再能当作
// 本包的对外契约整篇读；平台无关的缓存逻辑也不进 probe_linux.go / probe_other.go ——
// 那两个文件只留真正的平台差异（见 probe()）。

var (
	probeMu   sync.Mutex // 保护 probeInfo / probeDone
	probeInfo info
	probeDone bool
)

// probeCached 探测当前内核是否可用 io_uring（包内唯一入口，不导出：包外只经
// NewWithOptions 的选路间接依赖结论）。
//
// 判定完全基于 io_uring_setup 的 errno，不比较内核版本号 —— 版本号反映不了三类
// 误判：RHEL 系的 io_uring_disabled sysctl、容器 seccomp 拦截、以及发行版把 io_uring
// 反向移植进老内核（例如 openEuler/BCLinux 4.19.90 就带完整 backport，能力集约等于 5.8）。
//
// 只缓存确定性结论 —— 第二个返回值 deterministic 为真才落缓存。资源类瞬时错误
// （ENOMEM/EMFILE 等）不缓存，否则一次偶发失败会把进程永久钉死在 libaio 上。
// deterministic 由平台实现给出（见 probe_linux.go / probe_other.go）。
func probeCached() info {
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
