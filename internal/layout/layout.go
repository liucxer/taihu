// Package layout 定义裸设备物理布局参数与对齐工具。
// device（IO 层）与 metastore（分配层）共同依赖，故独立成包避免循环依赖。
package layout

// 裸设备物理布局参数。
// 整盘段数不再硬编码（v1 曾固定 2048 段 × 8GB = 16TB）：由启动时查询的真实设备容量
// 与段大小经 ComputeLayout 计算，运行时注入 device（IO 层）与 metastore（分配层）。
var (
	// DefaultSegmentSizeBytes 默认单个 segment 大小 = 8GB（-seg-size 可覆盖）。
	DefaultSegmentSizeBytes int64 = 8 * 1024 * 1024 * 1024
)

// BlockSize 对齐粒度 = 4KB。
const BlockSize int64 = 4096

// Layout 设备物理布局：段大小与整盘段数。
type Layout struct {
	SegmentSizeBytes int64
	SegmentCount     int64
}

// ComputeLayout 按设备容量与段大小计算布局：SegmentCount = capacity / SegmentSizeBytes
// （向下取整，余量不参与布局）。capacity 或 segmentSizeBytes 非正时返回空布局。
func ComputeLayout(capacity, segmentSizeBytes int64) Layout {
	if capacity <= 0 || segmentSizeBytes <= 0 {
		return Layout{}
	}
	return Layout{
		SegmentSizeBytes: segmentSizeBytes,
		SegmentCount:     capacity / segmentSizeBytes,
	}
}

// Align4k 将 n 向上补齐到 BlockSize 的整数倍。
func Align4k(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + BlockSize - 1) &^ (BlockSize - 1)
}
