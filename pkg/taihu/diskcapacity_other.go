//go:build !linux

package taihu

import "errors"

// diskCapacity 非 linux 平台不支持（taihu 目标平台为 linux/arm64）。
func diskCapacity(path string) (capacity, available, used int64, err error) {
	return 0, 0, 0, errors.New("diskCapacity: unsupported platform")
}
