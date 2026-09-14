package rpcclient

import "context"

// ObjectStore 是 taihu 客户端共同满足的对象存储接口。
//
// 两个实现：
//   - *Storage（本包）：直连一个已知实例 —— TCP（netpoll 帧协议）或同机 shm（shmipc）；
//   - rpccluster.Storage：经 TiKV 定位实例后路由（本地优先、回源重建）。
//
// 调用方持本接口即可在两种客户端形态之间替换，不必改调用代码。
//
// 为什么声明在本包而不是 rpccluster：rpccluster 依赖本包，反向声明会成环。
// Go 是结构化类型 —— rpccluster.Storage 天然满足这里声明的接口，无需 import 本
// 接口，只需一条 `var _ ObjectStore = (*Storage)(nil)` 断言（见 pkg/rpccluster/storage.go），
// 那条断言同时充当两个客户端「方法集不漂移」的编译期检查。
//
// 签名里只有 context / []byte / error 等标准库类型，因此**外部实现也能满足本接口**
// （内部类型出现在导出接口的方法签名里会让它对外不可实现）。
// 本接口是本包 rpcConn（未导出，两个传输实现共同满足）的对外镜像。
//
// 方法语义（两个实现一致）：
//   - Put：写入对象，size 为逻辑大小，in 提供数据（取 in[:size]），调用方负责数据就绪；
//   - Get：读 [off, off+size)，size=-1 读至结尾；返回 (data, release, err)，
//     用毕必须调用 release()（幂等）归还池化缓冲；
//   - Delete：删除对象映射（物理空间回收由 segment 级 GC 处理），key 不存在返回 ErrNotFound；
//   - Stat：返回对象逻辑大小，key 不存在返回 ErrNotFound；
//   - Close：释放连接与后台保活。
type ObjectStore interface {
	Put(ctx context.Context, key string, size int64, in []byte) error
	Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error)
	Delete(ctx context.Context, key string) error
	Stat(ctx context.Context, key string) (int64, error)
	Close() error
}

var _ ObjectStore = (*Storage)(nil)
