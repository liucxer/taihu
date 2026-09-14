package device

import (
	"os"
	"syscall"
)

// openDevice 打开设备：O_RDWR|O_DIRECT。读写均绕过 page cache 做直接 IO。
// 不设 O_SYNC：让 WriteAt 走后端异步批量落盘，避免每次写同步等待确认拖低带宽；
// 需要强制落盘时由存储层显式调用 d.Sync()。
func openDevice(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|syscall.O_DIRECT, 0)
}
