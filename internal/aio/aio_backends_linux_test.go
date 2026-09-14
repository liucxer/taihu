//go:build linux

package aio

import "testing"

// testBackends 返回 Linux 上要跑契约测试的后端：libaio 与 io_uring。
//
// io_uring 不可用时不在这里过滤掉，而是让 new 回调 t.Skipf —— 这样 go test -v
// 会打出 `--- SKIP: TestRoundTrip/io_uring` 并带上 errno 原因。若在这里悄悄
// 剔除，老节点上的输出与「全部通过」完全一样，没人会发现 io_uring 根本没跑。
func testBackends(t *testing.T) []backend {
	t.Helper()
	return []backend{
		{
			name: "libaio",
			new: func(t *testing.T, maxEvents int) Ring {
				t.Helper()
				r, err := NewWithMode(maxEvents, ModeLibAIO)
				if err != nil {
					t.Fatalf("New libaio: %v", err)
				}
				return r
			},
		},
		{
			name: "io_uring",
			new: func(t *testing.T, maxEvents int) Ring {
				t.Helper()
				if info := Probe(); !info.Supported {
					t.Skipf("io_uring 不可用: %s (kernel=%s)", info.Reason, info.KernelRelease)
				}
				r, err := NewWithMode(maxEvents, ModeIOUring)
				if err != nil {
					t.Fatalf("New io_uring: %v", err)
				}
				return r
			},
		},
	}
}
