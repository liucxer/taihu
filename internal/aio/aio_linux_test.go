//go:build linux

package aio

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// 本文件是 Linux 侧共用的测试基建：两条环境入口（aio_linux419_test.go 与
// aio_linux510_test.go）都从这里取后端清单、文件通道与内核门控。
//
// 为什么必须共用：两份入口都会编译进同一个测试二进制（build tag 分不出内核版本），
// 同名符号会重复定义，所以只能在这里定义一次，由运行期内核门控挑选入口。

// alignedBuf 返回首地址 4K 对齐的 n 字节切片（O_DIRECT 要求）。
func alignedBuf(n int) []byte {
	const block = 4096
	b := make([]byte, n+block)
	base := uintptr(unsafe.Pointer(&b[0]))
	if base%block == 0 {
		return b[:n]
	}
	start := block - int(base%block)
	return b[start : start+n]
}

// linuxRingBackends 返回 Linux 上要跑契约的后端：libaio 与 io_uring。
//
// io_uring 不可用时在 new 里 t.Skipf（而不是在这里过滤）——这样 go test -v 会打出
// `--- SKIP: .../io_uring` 并带上 errno 原因。若在这里悄悄剔除，输出与「全部通过」
// 完全一样，没人会发现 io_uring 根本没跑。
func linuxRingBackends(t *testing.T) []backend {
	t.Helper()
	return []backend{
		{
			name: "libaio",
			// 内核 ctx 容量上限：满了 io_submit 返回 EAGAIN → ErrFull。
			depthLimit: true,
			new: func(t *testing.T, maxEvents int) Ring {
				t.Helper()
				r, err := NewWithOptions(Options{Mode: ModeLibAIO, MaxEvents: maxEvents}, "")
				if err != nil {
					t.Fatalf("NewWithOptions(libaio): %v", err)
				}
				return r
			},
		},
		{
			name: "io_uring",
			// 复刻 libaio 的在途上限（见 aio_uring_linux.go 的 inflight）：满了返回 ErrFull。
			depthLimit: true,
			new: func(t *testing.T, maxEvents int) Ring {
				t.Helper()
				if pi := probeCached(); !pi.Supported {
					t.Skipf("io_uring 不可用: %s (kernel=%s)", pi.Reason, pi.KernelRelease)
				}
				r, err := NewWithOptions(Options{Mode: ModeIOUring, MaxEvents: maxEvents}, "")
				if err != nil {
					t.Fatalf("NewWithOptions(io_uring): %v", err)
				}
				return r
			},
		},
	}
}

// linuxTestChannels 返回 Linux 上要跑的通道：普通缓冲 IO 与 O_DIRECT（生产形态）。
func linuxTestChannels() []testChannel {
	return []testChannel{plainChannel(), oDirectChannel()}
}

// oDirectChannel O_DIRECT 通道：device 层在生产路径上就是用 O_DIRECT 打开设备
// （见 internal/device），这里复刻同一形态，验证 4K 对齐下的读写一致性。
//
// 文件系统不支持时整条通道以 t.Skipf 退出（不静默退化成普通 IO —— 那会让
// 「O_DIRECT 通道跑过了」变成假象）。
func oDirectChannel() testChannel {
	return testChannel{
		name: "odirect",
		open: func(t *testing.T, size int64) (*os.File, *os.File) {
			path := filepath.Join(oDirectDir(t), "aio-odirect-test")
			t.Cleanup(func() { _ = os.Remove(path) })
			open := func() *os.File {
				f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC|syscall.O_DIRECT, 0o600)
				if err != nil {
					t.Skipf("O_DIRECT 打开 %s 失败: %v", path, err)
				}
				if err := f.Truncate(size); err != nil {
					t.Fatalf("truncate %s: %v", path, err)
				}
				t.Cleanup(func() { _ = f.Close() })
				return f
			}
			return open(), open()
		},
		buf: alignedBuf,
	}
}

// oDirectDir 找一个真正支持 O_DIRECT 的目录。tmpfs（/tmp 常见形态）不支持，故依次
// 试 TAIHU_AIO_TEST_DIR、t.TempDir()、当前工作目录；全不支持则跳过。
func oDirectDir(t *testing.T) string {
	t.Helper()
	var dirs []string
	if d := os.Getenv("TAIHU_AIO_TEST_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	dirs = append(dirs, t.TempDir())
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	for _, d := range dirs {
		probe := filepath.Join(d, ".aio-odirect-probe")
		f, err := os.OpenFile(probe, os.O_RDWR|os.O_CREATE|syscall.O_DIRECT, 0o600)
		if err != nil {
			continue
		}
		_ = f.Close()
		_ = os.Remove(probe)
		return d
	}
	t.Skipf("没有支持 O_DIRECT 的文件系统（试过 %v）；可设 TAIHU_AIO_TEST_DIR 指定目录", dirs)
	return ""
}

// requireKernel 按内核版本前缀做运行期门控：4.19 与 5.10 两份入口同时编译进同一个
// 测试二进制，靠这里只让与当前内核匹配的那一份真正跑，其余跳过（build tag 做不到
// 按内核版本区分）。
func requireKernel(t *testing.T, prefix string) {
	t.Helper()
	rel := kernelRelease()
	if !strings.HasPrefix(rel, prefix) {
		t.Skipf("本机内核 %s 不属于 %s*，跳过该内核的专用用例", rel, prefix)
	}
}

// assertIOUringUsable 断言本机 io_uring 真的能用：探测结论、内核回填的队列深度、
// 以及按该深度建出的环。
func assertIOUringUsable(t *testing.T) {
	t.Helper()
	pi := probeCached()
	if !pi.Supported {
		t.Fatalf("本机 io_uring 不可用: %s (kernel=%s)；"+
			"请确认 CONFIG_IO_URING 已开、io_uring_disabled 未置 2、容器未用 seccomp 拦下该系统调用",
			pi.Reason, pi.KernelRelease)
	}
	if pi.SQEntries == 0 || pi.CQEntries == 0 {
		t.Errorf("探测回填的队列深度 sq=%d cq=%d，都不应为 0", pi.SQEntries, pi.CQEntries)
	}

	r, err := NewWithOptions(Options{Mode: ModeIOUring, MaxEvents: 8}, "")
	if err != nil {
		t.Fatalf("强制 io_uring 建环失败: %v", err)
	}
	defer func() { _ = r.Close() }()
	if sq, cq, ok := ringQueueDepth(r); !ok || sq == 0 || cq == 0 {
		t.Errorf("实际建出的 ring 深度 sq=%d cq=%d ok=%v，都不应为 0", sq, cq, ok)
	}
}

// assertAutoPicksIOUring 断言 auto（真实探测、无环境变量覆盖）落到 io_uring。
// 这正是「探测按 io_uring_setup 的 errno 判定、不比较版本号」在两种内核上的落点验证。
func assertAutoPicksIOUring(t *testing.T) {
	t.Helper()
	t.Setenv(envMode, "") // 排除环境变量干扰，强制走真实探测
	r, err := NewWithOptions(Options{Mode: ModeAuto, MaxEvents: 8}, "")
	if err != nil {
		t.Fatalf("auto 建环失败: %v", err)
	}
	defer func() { _ = r.Close() }()
	if _, ok := r.(*uringRing); !ok {
		t.Fatalf("io_uring 可用时 auto 应落 io_uring，得到 %T", r)
	}
}

// assertLibAIOUsable 断言 libaio 可用：双后端并存期两者都要能建环。
func assertLibAIOUsable(t *testing.T) {
	t.Helper()
	r, err := NewWithOptions(Options{Mode: ModeLibAIO, MaxEvents: 8}, "")
	if err != nil {
		t.Fatalf("libaio 建环失败: %v", err)
	}
	_ = r.Close()
}
