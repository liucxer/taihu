//go:build linux

package aio

import "testing"

// 本文件是 Linux 4.19 验证机（openEuler/BCLinux 4.19.90，io_uring 由发行版反向移植回来）
// 的入口。build tag 只能按 GOOS/GOARCH 分、分不出内核版本，所以本文件与 5.10 那份会
// 同时编译进同一个测试二进制，靠 requireKernel 在运行期只让匹配的那一份真正跑。

// iouringSkip419 io_uring 在 4.19 验证机上不跑契约的原因。
//
// 实测（BCLinux 4.19.90 反向移植，见 TestRingContractLinux419/io_uring/odirect 的历史失败）：
// io_uring 对 O_DIRECT 文件做重复写会以 -EIO(-5) 完成 —— 契约里唯一做重复写的用例
// fd_survives_gc 因此失败。该内核 backport 的行为与本实现要求的语义不匹配，按
// 「内核不匹配就跳过，而不是报错」处理。io_uring 的探测与建环能力仍由
// TestLinux419IOUringBackport 覆盖。普通缓冲 IO 通道下 io_uring 实测可用；若要保留
// 那部分覆盖，把这里的整后端跳过收窄到 odirect 通道即可。
const iouringSkip419 = "io_uring 在本机 4.19 内核（反向移植）上做 O_DIRECT 写会返回 -EIO(-5)，" +
	"该组合不跑契约；探测/建环能力见 TestLinux419IOUringBackport"

// TestRingContractLinux419 4.19 上的契约：libaio 后端 × 普通缓冲 IO 与 O_DIRECT 两条通道
// （见 iouringSkip419），Ring 的 6 个方法 + 各档 IO 尺寸 + 双向数据一致性。
func TestRingContractLinux419(t *testing.T) {
	requireKernel(t, "4.19.")
	for _, b := range linuxRingBackends(t) {
		if b.name == "io_uring" {
			t.Run(b.name, func(t *testing.T) { t.Skip(iouringSkip419) })
			continue
		}
		for _, ch := range linuxTestChannels() {
			t.Run(b.name+"/"+ch.name, func(t *testing.T) { runRingContract(t, b, ch) })
		}
	}
}

// TestLinux419IOUringBackport 4.19.90 上的 io_uring 是发行版反向移植（上游 5.1 才引入），
// 版本号远低于 5.1 —— 断言它确实可用，正是「探测只看 io_uring_setup 的 errno、
// 不比较版本号」的现场证据（见 probe_cache.go 的说明：版本号反映不了 backport、
// io_uring_disabled sysctl 与容器 seccomp 这三类误判）。
func TestLinux419IOUringBackport(t *testing.T) {
	requireKernel(t, "4.19.")
	if pi := probeCached(); !pi.Supported {
		// 未带 backport 的 4.19 内核：本用例的前提不成立，跳过而不是报错。
		t.Skipf("本机 4.19 内核未带 io_uring 反向移植（%s）", pi.Reason)
	}
	assertIOUringUsable(t)
	assertAutoPicksIOUring(t)
	// 4.19 的 libaio 是原生能力：双后端并存期两者都要能跑，回退路径不能是纸面上的。
	assertLibAIOUsable(t)
}
