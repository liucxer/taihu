//go:build linux

package device

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDeviceCapacity 覆盖裸设备容量查询的错误路径：路径不存在、以及普通文件
// 不具备 BLKGETSIZE64（内核返回 ENOTTY）。成功路径需要真实块设备，单测环境
// （xfs 上的普通文件）无法构造。
func TestDeviceCapacity(t *testing.T) {
	if _, err := DeviceCapacity(filepath.Join(t.TempDir(), "no-such.img")); err == nil {
		t.Fatal("不存在的路径应报错")
	}

	// 普通文件不是块设备：BLKGETSIZE64 ioctl 须失败（不得退化成文件大小）。
	plain := filepath.Join(t.TempDir(), "plain.img")
	if err := os.WriteFile(plain, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if sz, err := DeviceCapacity(plain); err == nil {
		t.Fatalf("普通文件上 BLKGETSIZE64 应报错，实际返回 %d", sz)
	}
}
