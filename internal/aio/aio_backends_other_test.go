//go:build !linux

package aio

import "testing"

// testBackends 返回非 Linux 平台可跑的后端：只有 goroutine 兜底实现
// （aio_other.go）。io_uring 与 libaio 都是 Linux 专有，此处无需出现。
func testBackends(t *testing.T) []backend {
	t.Helper()
	return []backend{
		{
			name: "fallback",
			new: func(t *testing.T, maxEvents int) Ring {
				t.Helper()
				r, err := NewWithOptions(maxEvents, Options{Mode: ModeLibAIO})
				if err != nil {
					t.Fatalf("NewWithOptions: %v", err)
				}
				return r
			},
		},
	}
}
