package version

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// setGlobals 暂存/恢复包级注入变量，避免污染同一进程内其他测试。
func setGlobals(t *testing.T, commit, buildTime string) {
	t.Helper()
	oc, obt := Commit, BuildTime
	Commit, BuildTime = commit, buildTime
	t.Cleanup(func() { Commit, BuildTime = oc, obt })
}

// writeFakeGit 在临时目录写一个名为 git 的假可执行脚本并把 PATH 指向该目录，
// 使 queryGit 的成败可确定性构造（不依赖真实 git 与仓库）。
func writeFakeGit(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("假 git 脚本依赖 POSIX shell")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// TestStringInjected 编译期 ldflags 注入齐全时直接拼接，不查 git。
func TestStringInjected(t *testing.T) {
	writeFakeGit(t, "#!/bin/sh\nexit 1\n") // 任何 git 调用都失败：若查 git 会得到 unknown
	setGlobals(t, "8abdf4d", "202609111002")
	if got := String(); got != "8abdf4d_202609111002" {
		t.Fatalf("String()=%q want 8abdf4d_202609111002", got)
	}
}

// TestStringFromGit 未注入时运行时查询 git：commit 号与 commit 时间都取到。
func TestStringFromGit(t *testing.T) {
	writeFakeGit(t, `#!/bin/sh
case "$1" in
rev-parse) echo "abc1234" ;;
log) echo "202601020304" ;;
esac
`)
	setGlobals(t, "", "")
	if got := String(); got != "abc1234_202601020304" {
		t.Fatalf("String()=%q want abc1234_202601020304", got)
	}
}

// TestStringGitLogFails 取到 commit 号但取不到时间：时间回退为 unknown。
func TestStringGitLogFails(t *testing.T) {
	writeFakeGit(t, `#!/bin/sh
case "$1" in
rev-parse) echo "abc1234" ;;
*) exit 1 ;;
esac
`)
	setGlobals(t, "", "")
	if got := String(); got != "abc1234_unknown" {
		t.Fatalf("String()=%q want abc1234_unknown", got)
	}
}

// TestStringUnknownWithoutGit git 不可用（调用失败）时整串回退为 unknown_unknown。
func TestStringUnknownWithoutGit(t *testing.T) {
	writeFakeGit(t, "#!/bin/sh\nexit 1\n")
	setGlobals(t, "", "")
	if got := String(); got != "unknown_unknown" {
		t.Fatalf("String()=%q want unknown_unknown", got)
	}
}
