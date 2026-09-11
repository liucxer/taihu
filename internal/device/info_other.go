//go:build !linux

package device

import "os"

// DeviceCapacity 非 Linux 平台（本机开发/单测）：无 BLKGETSIZE64 ioctl，
// 用普通文件大小近似设备容量。
func DeviceCapacity(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
