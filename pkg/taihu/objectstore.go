package taihu

import (
	"context"
	"io"
)

// ObjectStore 是对象存储的统一接口（v3 引入，供本地与远程实现共同满足）。
//
// 本地实现：*Storage（见 storage.go），数据直落本机裸设备；
// 远程实现：rpcclient.Storage（见 设计文档_v3.md §5），语义收敛在 server 端，
// 调用方通过本接口读写对象时无感切换本地/远程。
//
// 方法签名与 *Storage 一致：
//   - Put 传入逻辑 size（由调用方负责），数据从 in 流式读入；
//   - Get 支持对象内 [off, off+size) 区间读，返回的 ReadCloser 调用方必须 Close
//     （本地归还池化对齐缓冲，远程终止 gRPC 流）；
//   - Delete 删除对象映射。
//
// 注意：LoadCache（本地元数据预热）与 Stat（远程便利查询）不进入本接口，
// 前者是 server 内部行为，后者 rpcclient 单列。
type ObjectStore interface {
	// Put 写入对象。size 为对象逻辑大小（字节），in 提供数据。
	Put(ctx context.Context, key string, size int64, in io.Reader) error
	// Get 读取对象内 [off, off+size) 子区间，返回只读流（必须 Close）。
	Get(ctx context.Context, key string, off, size int64) (io.ReadCloser, error)
	// Delete 删除对象映射（物理空间回收由 segment 级 GC 处理）。
	Delete(ctx context.Context, key string) error
}