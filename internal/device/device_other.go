//go:build !linux

package device

import "os"

// openDevice 打开设备：O_RDWR|O_SYNC。非 Linux 平台无 O_DIRECT，退回普通直接打开，
// 仅用于本机（macOS）单测/联调；性能路径在 Linux 上以 O_DIRECT 运行。
func openDevice(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_SYNC, 0)
}

// deviceCapacity 非 Linux 平台（本机开发/单测）：无 BLKGETSIZE64 ioctl，
// 用普通文件大小近似设备容量。
func deviceCapacity(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
