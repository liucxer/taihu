// Package layout 定义裸设备物理布局参数与对齐工具。
// device（IO 层）与 metastore（分配层）共同依赖，故独立成包避免循环依赖。
package layout

// 裸设备物理布局参数。
const (
	// SegmentSizeBytes 单个 segment 大小 = 8GB。
	SegmentSizeBytes int64 = 8 * 1024 * 1024 * 1024
	// SegmentCount 整盘 segment 数量 = 2048。
	SegmentCount int64 = 2048
	// BlockSize 对齐粒度 = 4KB。
	BlockSize int64 = 4096
)

// Align4k 将 n 向上补齐到 BlockSize 的整数倍。
func Align4k(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + BlockSize - 1) &^ (BlockSize - 1)
}