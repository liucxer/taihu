//go:build linux

package aio

import "testing"

// 本文件是 Linux 5.10 验证机（openEuler 22.03 SP4，io_uring 原生支持）的入口。
// build tag 只能按 GOOS/GOARCH 分、分不出内核版本，所以本文件与 4.19 那份会同时
// 编译进同一个测试二进制，靠 requireKernel 在运行期只让匹配的那一份真正跑。

// TestRingContractLinux510 5.10 上的完整契约：libaio 与 io_uring 两个后端 ×
// 普通缓冲 IO 与 O_DIRECT 两条通道，Ring 的 6 个方法 + 各档 IO 尺寸 + 双向数据一致性。
func TestRingContractLinux510(t *testing.T) {
	requireKernel(t, "5.10.")
	for _, b := range linuxRingBackends(t) {
		for _, ch := range linuxTestChannels() {
			t.Run(b.name+"/"+ch.name, func(t *testing.T) { runRingContract(t, b, ch) })
		}
	}
}

// TestLinux510IOUringNative 5.10 是 io_uring 的原生内核（5.1 起有、5.10 已相当成熟）：
// 断言探测能拿到内核回填的队列深度与能力位、按该深度能建出环、auto 也落 io_uring，
// 同时 libaio 仍可作为回退路径使用。
func TestLinux510IOUringNative(t *testing.T) {
	requireKernel(t, "5.10.")
	assertIOUringUsable(t)
	assertAutoPicksIOUring(t)
	assertLibAIOUsable(t)
}
