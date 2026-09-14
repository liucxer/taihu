package device

import (
	"os"
	"syscall"
	"unsafe"
)

// BLKGETSIZE64 块设备大小查询 ioctl（_IOR(0x12, 114, uint64)，定义于 linux/fs.h）。
const blkGetSize64 = 0x80081272

// DeviceCapacity 查询裸设备容量（字节）。Linux 上经 BLKGETSIZE64 ioctl 获取块设备真实大小，
// 不依赖文件系统（裸盘无文件系统）。
func DeviceCapacity(path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var sz uint64
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkGetSize64, uintptr(unsafe.Pointer(&sz)))
	if errno != 0 {
		return 0, errno
	}
	return int64(sz), nil
}
