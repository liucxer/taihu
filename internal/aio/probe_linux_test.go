//go:build linux

package aio

import (
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestTransientErrno 瞬时错误不得被缓存成「不支持」；其余（含确定性失败）返回 false。
func TestTransientErrno(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{unix.ENOMEM, true},
		{unix.EMFILE, true},
		{unix.ENFILE, true},
		{unix.EINTR, true},
		{unix.EBUSY, true},
		{unix.EAGAIN, true},
		{unix.ENOSYS, false},
		{unix.EPERM, false},
		{unix.EACCES, false},
		{unix.EINVAL, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := transientErrno(c.err); got != c.want {
			t.Errorf("transientErrno(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestDescribeUringErr 每个已知 errno 都要翻译成可读原因，未知 errno 走兜底格式。
func TestDescribeUringErr(t *testing.T) {
	cases := []struct {
		err      error
		contains string
	}{
		{unix.ENOSYS, "系统调用"},
		{unix.EPERM, "策略禁用"},
		{unix.EACCES, "策略禁用"},
		{unix.EINVAL, "参数被拒绝"},
		{unix.EMFILE, "描述符耗尽"},
		{unix.ENFILE, "描述符耗尽"},
		{unix.ENOMEM, "MEMLOCK"},
		{unix.EBUSY, "暂时失败"},
		{unix.EAGAIN, "暂时失败"},
		{unix.EIO, "io_uring_setup 失败"},
	}
	for _, c := range cases {
		got := describeUringErr(c.err)
		if !strings.Contains(got, c.contains) {
			t.Errorf("describeUringErr(%v) = %q, 应包含 %q", c.err, got, c.contains)
		}
	}
	// EPERM/EACCES 分支要带上 errno 原文，便于排障。
	if got := describeUringErr(unix.EPERM); !strings.Contains(got, unix.EPERM.Error()) {
		t.Errorf("describeUringErr(EPERM) = %q, 应带上 errno 文本", got)
	}
}

// TestUringSupportsRWBadFd REGISTER_PROBE 自身不可用（非法 fd）时不作判断，
// 以免把可用内核误判为不支持。
func TestUringSupportsRWBadFd(t *testing.T) {
	ok, why := uringSupportsRW(-1)
	if !ok {
		t.Errorf("探测不可用时应返回 ok=true，得到 ok=%v why=%q", ok, why)
	}
	if why != "" {
		t.Errorf("探测不可用时不该给出原因，得到 %q", why)
	}
}

// TestKernelRelease 内核版本字符串仅用于日志，但不得为空。
func TestKernelRelease(t *testing.T) {
	if got := kernelRelease(); got == "" {
		t.Error("kernelRelease() 不应为空")
	}
}

// TestProbeCached 探测结论被缓存：重复调用返回同一份结果，且字段自洽。
func TestProbeCached(t *testing.T) {
	first := Probe()
	second := Probe()
	if first != second {
		t.Errorf("Probe 应变缓存：\n%+v\n%+v", first, second)
	}
	if first.KernelRelease == "" {
		t.Error("Probe 应带上内核版本字符串")
	}
	if first.Supported {
		if first.SQEntries == 0 || first.CQEntries == 0 {
			t.Errorf("支持 io_uring 时队列深度不应为 0: %+v", first)
		}
		if first.Reason != "ok" {
			t.Errorf("支持时 Reason 应为 ok，得到 %q", first.Reason)
		}
		// 探测结论必须与真实建 ring 的结果一致（不能只看 errno 就下结论）。
		r, err := NewWithOptions(4, Options{Mode: ModeIOUring})
		if err != nil {
			t.Fatalf("Probe 报支持但建 ring 失败: %v", err)
		}
		_ = r.Close()
	} else if first.Reason == "" {
		t.Error("不支持时必须给出可读原因")
	}
}

// TestBackendNameAndQueueDepth 日志用的后端名与队列深度判定。
func TestBackendNameAndQueueDepth(t *testing.T) {
	lr, err := newLibAIORing(4)
	if err != nil {
		t.Fatalf("newLibAIORing: %v", err)
	}
	defer func() { _ = lr.Close() }()
	if got := backendName(lr); got != "libaio" {
		t.Errorf("backendName(libaio ring) = %q", got)
	}
	if sq, cq, ok := ringQueueDepth(lr); ok || sq != 0 || cq != 0 {
		t.Errorf("libaio 无 io_uring 队列深度，得到 (%d, %d, %v)", sq, cq, ok)
	}

	ur, err := newIOUringRing(8, false)
	if err != nil {
		t.Skipf("io_uring 不可用: %v", err)
	}
	defer func() { _ = ur.Close() }()
	if got := backendName(ur); got != "io_uring" {
		t.Errorf("backendName(uring ring) = %q", got)
	}
	sq, cq, ok := ringQueueDepth(ur)
	if !ok || sq < 8 || cq < 16 {
		t.Errorf("ringQueueDepth = (%d, %d, %v), want ok 且 sq>=8 cq>=16", sq, cq, ok)
	}
}
