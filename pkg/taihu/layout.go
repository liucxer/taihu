package taihu

import "github.com/liucxer/taihu/internal/layout"

// 裸设备物理布局参数，re-export（v3：定义已迁移至 internal/layout/layout.go）。
const (
	// SegmentSizeBytes 单个 segment 大小 = 8GB。
	SegmentSizeBytes = layout.SegmentSizeBytes
	// SegmentCount 整盘 segment 数量 = 2048。
	SegmentCount = layout.SegmentCount
	// BlockSize 对齐粒度 = 4KB。
	BlockSize = layout.BlockSize
)