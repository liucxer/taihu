package taihu

import (
	"context"
)

// ObjectStore 是对象存储的统一接口（v3 引入，供本地与远程实现共同满足）。
//
// 本地实现：*Storage（见 storage.go），数据直落本机裸设备；
// 远程实现：rpcclient.Storage（见 设计文档_v3.md §5），语义收敛在 server 端，
// 调用方通过本接口读写对象时无感切换本地/远程。
//
// 读路径两个实现各按最优形态暴露（不进入本接口的公共契约之外仍保持方法级一致）：
//   - 本地：ReadAt（返回 bufpool 池化缓冲的零拷贝直读，须 bufpool.Put 归还，见 storage.go）；
//   - 远程：Get（返回 bufpool 池化缓冲的整块数据，须 bufpool.Put 归还，见 rpcclient）。
//
// 方法：
//   - Put 传入逻辑 size 与整块数据 in（取 in[:size]），由调用方负责数据就绪；
//   - Delete 删除对象映射。
type ObjectStore interface {
	// Put 写入对象。size 为对象逻辑大小（字节），in 提供数据（len(in) >= size）。
	Put(ctx context.Context, key string, size int64, in []byte) error
	// Delete 删除对象映射（物理空间回收由 segment 级 GC 处理）。
	Delete(ctx context.Context, key string) error
}

var _ ObjectStore = (*Storage)(nil)
