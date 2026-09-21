package device

import (
	"os"
	"syscall"
)

// openDevice 打开设备：O_RDWR|O_DIRECT|O_EXCL。读写均绕过 page cache 做直接 IO。
// O_EXCL：对块设备执行排他占用（bd_claim）——taihu server 存活期间，其他进程对该盘
// 做排他打开（mount/mkfs/swapon/mdadm/LVM 等）及重写分区表（fdisk/parted 的
// BLKRRPART 因分区在开而返回 EBUSY）都会被内核拒绝，防止盘被误格式化；server 退出
// 释放句柄后自动解除。普通文件/非块设备上 O_EXCL（无 O_CREAT）无副作用。
// 不设 O_SYNC：让 WriteAt 走后端异步批量落盘，避免每次写同步等待确认拖低带宽；
// 需要强制落盘时由存储层显式调用 d.Sync()。
func openDevice(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|syscall.O_DIRECT|syscall.O_EXCL, 0)
}
