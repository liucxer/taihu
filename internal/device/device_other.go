//go:build !linux

package device

import "os"

// openDevice 打开设备：O_RDWR|O_SYNC。非 Linux 平台无 O_DIRECT，退回普通直接打开，
// 仅用于本机（macOS）单测/联调；性能路径在 Linux 上以 O_DIRECT 运行。
func openDevice(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_SYNC, 0)
}
