//go:build darwin

package aio

import "testing"

// 本文件是 macOS（开发/自测环境）的入口：本平台只有 goroutine + 同步 pread/pwrite 的
// 兜底实现（aio_other.go），没有 libaio、没有 io_uring，也没有 O_DIRECT（darwin 的
// x/sys/unix 里根本没有 O_DIRECT 符号），故只跑普通缓冲 IO 这一条通道。
//
// 契约本体在 aio_test.go 的 runRingContract 里跨平台共享。

// TestRingContractDarwin macOS 上的完整契约：Ring 的 6 个方法 + 各档 IO 尺寸
// （4K…4M）+ 双向数据一致性，全部跑在兜底后端上。
func TestRingContractDarwin(t *testing.T) {
	runRingContract(t, fallbackBackend(t), plainChannel())
}

// TestDarwinPlatformFacts macOS 侧的平台事实：这些是 Linux 入口里测不到的
// （那边探测恒为「支持」，且 ModeIOUring 会真的建出环）。
func TestDarwinPlatformFacts(t *testing.T) {
	pi := probeCached()
	if pi.Supported {
		t.Fatalf("macOS 不应支持 io_uring（probe=%+v）", pi)
	}
	if pi.Reason == "" {
		t.Error("探测判「不支持」时必须给出原因（供日志与错误信息）")
	}

	// 强制 io_uring：必须报错，不得静默降级。
	if r, err := NewWithOptions(Options{Mode: ModeIOUring, MaxEvents: 4}, ""); err == nil {
		_ = r.Close()
		t.Error("macOS 上强制 ModeIOUring 应报错，不得静默降级")
	}

	// auto（真实探测、无环境变量覆盖）→ 兜底实现。
	t.Setenv(envMode, "")
	r, err := NewWithOptions(Options{Mode: ModeAuto, MaxEvents: 8}, "")
	if err != nil {
		t.Fatalf("auto 建环失败: %v", err)
	}
	defer func() { _ = r.Close() }()
	if _, ok := r.(*ring); !ok {
		t.Fatalf("macOS 上 auto 应落到兜底实现，得到 %T", r)
	}
}

// fallbackBackend macOS 上唯一可跑的后端：aio_other.go 的 goroutine 兜底。
// 非 Linux 上 ModeLibAIO 没有内核接口可走，newLibAIORing 就落在这里。
// 兜底实现逐条起 goroutine，没有队列深度上限，故 depthLimit=false（契约里的
// queue_full 子测试不适用）。
func fallbackBackend(t *testing.T) backend {
	t.Helper()
	return backend{
		name: "fallback",
		new: func(t *testing.T, maxEvents int) Ring {
			t.Helper()
			r, err := NewWithOptions(Options{Mode: ModeLibAIO, MaxEvents: maxEvents}, "")
			if err != nil {
				t.Fatalf("NewWithOptions(fallback): %v", err)
			}
			return r
		},
	}
}
