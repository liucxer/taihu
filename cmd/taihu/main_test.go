// cmd/taihu/main.go 的用例：run 的参数装配与退出码（0=成功、1=命令失败）。
package main

import (
	"os"
	"strings"
	"testing"
)

// cliTestRedirect 把 os.Stdout/os.Stderr 重定向到临时文件，返回两者的「读取已写内容」函数。
func cliTestRedirect(t *testing.T) (func() string, func() string) {
	t.Helper()
	readers := make([]func() string, 0, 2)
	for _, target := range []**os.File{&os.Stdout, &os.Stderr} {
		f, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatalf("temp file: %v", err)
		}
		path := f.Name()
		old := *target
		*target = f
		readers = append(readers, func() string {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			return string(b)
		})
		t.Cleanup(func() {
			*target = old
			_ = f.Close()
		})
	}
	return readers[0], readers[1]
}

// TestCLIRunVersion 覆盖成功路径：返回退出码 0，并把版本打到 stdout。
func TestCLIRunVersion(t *testing.T) {
	stdout, _ := cliTestRedirect(t)
	if code := run([]string{"version"}); code != 0 {
		t.Fatalf("run(version) = %d, want 0", code)
	}
	if got := stdout(); !strings.HasPrefix(got, "taihu ") {
		t.Fatalf("stdout = %q, want 版本行", got)
	}
}

// TestCLIRunBadFlag 覆盖失败路径：未知标志 → 退出码 1，错误打到 stderr。
func TestCLIRunBadFlag(t *testing.T) {
	_, stderr := cliTestRedirect(t)
	if code := run([]string{"--definitely-not-a-flag"}); code != 1 {
		t.Fatalf("run(未知标志) = %d, want 1", code)
	}
	if got := stderr(); !strings.Contains(got, "taihu:") {
		t.Fatalf("stderr = %q, want 含 \"taihu:\" 的错误行", got)
	}
}

// TestCLIRunNilArgs 覆盖 args==nil 分支（沿用 os.Args[1:]）。测试二进制的参数不属于
// CLI 已知标志，故只会得到「成功或报错」两种合法结果，这里只要求不 panic、不退出进程。
func TestCLIRunNilArgs(t *testing.T) {
	cliTestRedirect(t)
	old := os.Args
	t.Cleanup(func() { os.Args = old })
	code := run(nil)
	if code != 0 && code != 1 {
		t.Fatalf("run(nil) = %d, want 0 或 1", code)
	}
	if len(os.Args) != len(old) {
		t.Fatalf("run(nil) 改动了 os.Args 长度：%d → %d", len(old), len(os.Args))
	}
}
