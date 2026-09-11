//go:build linux

package taihu

import "golang.org/x/sys/unix"

// diskCapacity 返回 path 所在文件系统容量/可用/已用字节（statfs，Bavail 对非 root 更准确）。
func diskCapacity(path string) (capacity, available, used int64, err error) {
	var st unix.Statfs_t
	if err = unix.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	capacity = int64(st.Blocks) * int64(st.Bsize)
	available = int64(st.Bavail) * int64(st.Bsize)
	used = capacity - available
	return capacity, available, used, nil
}
