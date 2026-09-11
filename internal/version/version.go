// Package version 提供二进制版本号：最后一次 commit 的短哈希 + commit 时间，形如 "8abdf4d_202609111002"。
//
// 编译期通过 -ldflags "-X github.com/liucxer/taihu/internal/version.Commit=<sha> -X
// github.com/liucxer/taihu/internal/version.BuildTime=<YYYYMMDDHHMM>" 注入；
// 未注入时回退为运行时查询 git。
package version

import (
	"os/exec"
	"strings"
)

var (
	// Commit 为最后一次 commit 的短哈希（git rev-parse --short HEAD）。
	Commit string
	// BuildTime 为最后一次 commit 的时间，格式 YYYYMMDDHHMM。
	BuildTime string
)

// String 返回形如 "8abdf4d_202609111002" 的版本号。
func String() string {
	c, t := Commit, BuildTime
	if c == "" || t == "" {
		c, t = queryGit()
	}
	if c == "" {
		c = "unknown"
	}
	if t == "" {
		t = "unknown"
	}
	return c + "_" + t
}

// queryGit 在 ldflags 未注入时，运行时查询最后一次 commit 号与时间。
func queryGit() (string, string) {
	c, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", ""
	}
	c = []byte(strings.TrimSpace(string(c)))
	t, err := exec.Command("git", "log", "-1", "--format=%cd", "--date=format:%Y%m%d%H%M").Output()
	if err != nil {
		return string(c), ""
	}
	return string(c), strings.TrimSpace(string(t))
}
