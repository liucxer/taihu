//go:build e2e

package e2e

// H 场景：SDK 批量与零拷贝数据面 API 的端到端覆盖（此前 a–g 场景均未触达）。
//
// 覆盖 API：PutBatch / PutBatchKeys / GetBatch / GetFd / GetFdBatch /
// CheckPoolIsValid / UsageGet。单 key Put/Get/Delete/Stat 的 e2e 覆盖见
// a_data_test.go（TestASDKPath）。
//
// 零拷贝断言说明：e2e 的 SDK 与 server 同机（127.0.0.1 + auto → shm 通道），
// GetFd/GetFdBatch 必须交付 fd>0 的 splice 源；本场景进一步用 syscall.Pread
// 直接从交付的 fd 读对象内容，验证 (fd, foff) 确实指向设备内对象数据，而非仅
// 检查返回值形状。Transport=rpc 时 GetFd 应回退拷贝路径（fd==0、data 正确）。
//
// SDK 构造约定：回源回调统一设为「未知 key 即 NotFound」（与 a_data 及真实部署
// 「上层提供源」一致）。SDK 的 miss 语义：有 Source → 返回 Source 的错误；无
// Source → ErrSourceUnset。涉及缺失 key 断言的用例必须配 Source。

import (
	"bytes"
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/rpcclient"
	taihuclient "github.com/liucxer/taihu/pkg/taihu-client"
)

// notFoundSource 「未知 key 即 NotFound」的回源回调。
func notFoundSource(context.Context, string) ([]byte, error) { return nil, rpcclient.ErrNotFound }

// preadAt 从交付的 fd 读 [foff, foff+len(buf)) —— 模拟 FUSE splice 前的兜底校验：
// (fd, foff) 必须真实指向设备内对象数据。
func preadAt(t *testing.T, fd int, foff uint64, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	got := 0
	for got < n {
		r, err := syscall.Pread(fd, buf[got:], int64(foff)+int64(got))
		if err != nil {
			t.Fatalf("Pread(fd=%d, off=%d): %v", fd, foff, err)
		}
		if r == 0 {
			t.Fatalf("Pread(fd=%d, off=%d): 短读 %d/%d", fd, foff, got, n)
		}
		got += r
	}
	return buf
}

// TestH1PutBatch：PutBatch 同内容批量写 → 逐 key 读回校验 + 索引收敛 + no-op 边界。
func TestH1PutBatch(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	st := h.newSDK(nil)
	waitFor(t, 15*time.Second, "SDK 发现实例 %s", func() bool { return st.HasLive() }, srv.info.Addr)

	ctx := h.ctx
	keys := []string{h.key("h1/k0"), h.key("h1/k1"), h.key("h1/k2"), h.key("h1/k3")}
	data := randBytes(t, 1<<20) // 1MiB，跨多 chunk 边界概率低但覆盖 PutBatch 路径

	if err := st.PutBatch(ctx, keys, int64(len(data)), data); err != nil {
		t.Fatalf("PutBatch(%d keys): %v", len(keys), err)
	}
	for _, k := range keys {
		getExact(t, st, k, data)
	}

	// 索引异步写，等待收敛后 ListIndexKeys 应枚举出全部 key。
	waitFor(t, 15*time.Second, "索引收敛到 %d 个 key", func() bool {
		ks, err := st.ListIndexKeys(ctx, h.keyPrefix+"h1/")
		return err == nil && len(ks) == len(keys)
	}, len(keys))

	// no-op 边界：空 keys 与 size=0 都是 nil（不报错、不写索引）。
	if err := st.PutBatch(ctx, nil, int64(len(data)), data); err != nil {
		t.Fatalf("PutBatch(空 keys) 应为 no-op nil，got %v", err)
	}
	if err := st.PutBatch(ctx, keys[:1], 0, data); err != nil {
		t.Fatalf("PutBatch(size=0) 应为 no-op nil，got %v", err)
	}
}

// TestH2PutBatchKeys：每 key 不同内容的批量写（含「只取前 size 字节」语义）与参数校验。
func TestH2PutBatchKeys(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	st := h.newSDK(nil)
	waitFor(t, 15*time.Second, "SDK 发现实例 %s", func() bool { return st.HasLive() }, srv.info.Addr)

	ctx := h.ctx
	const size = 1 << 20
	bodies := [][]byte{
		randBytes(t, size),
		randBytes(t, size),
		randBytes(t, size),
		append(randBytes(t, size), randBytes(t, 16)...), // 比 size 长：对象应只写前 size 字节
	}
	keys := []string{h.key("h2/k0"), h.key("h2/k1"), h.key("h2/k2"), h.key("h2/k3")}

	// 参数校验：datas 与 keys 数量不一致。
	if err := st.PutBatchKeys(ctx, keys, size, bodies[:3]); !errors.Is(err, rpcclient.ErrInvalidRange) {
		t.Fatalf("PutBatchKeys 数量不一致应 ErrInvalidRange，got %v", err)
	}
	if err := st.PutBatchKeys(ctx, keys, size, bodies); err != nil {
		t.Fatalf("PutBatchKeys: %v", err)
	}
	for i, k := range keys {
		getExact(t, st, k, bodies[i][:size])
	}
	if n, err := st.Stat(ctx, keys[3]); err != nil || n != size {
		t.Fatalf("Stat(超长 data 的 key) = %d, %v, want %d", n, err, size)
	}
}

// TestH3GetBatch：批量读（全量/区间/错误路径），out[i] 与 keys[i] 一一对应。
func TestH3GetBatch(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	st := h.newSDK(func(c *taihuclient.ClusterConfig) { c.Source = notFoundSource })
	waitFor(t, 15*time.Second, "SDK 发现实例 %s", func() bool { return st.HasLive() }, srv.info.Addr)

	ctx := h.ctx
	const size = 128 << 10 // 注意：SDK 层 GetBatch 不接受 size<0（-1 需逐 key Get）
	keys := []string{h.key("h3/k0"), h.key("h3/k1"), h.key("h3/k2"), h.key("h3/k3")}
	wants := make([][]byte, len(keys))
	for i, k := range keys {
		wants[i] = pattern(k, size)
		putExact(t, st, k, wants[i])
	}

	// 全量读：out[i] == wants[i]。
	out, rel, err := st.GetBatch(ctx, keys, 0, size)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	for i := range keys {
		if !bytes.Equal(out[i], wants[i]) {
			t.Fatalf("GetBatch out[%d] 内容不一致（len=%d）", i, len(out[i]))
		}
	}
	rel()

	// 区间读：同一 [off,size) 作用于每个 key。
	off, n := int64(4096), int64(8192)
	out2, rel2, err := st.GetBatch(ctx, keys[:2], off, n)
	if err != nil {
		t.Fatalf("GetBatch(区间): %v", err)
	}
	for i := range 2 {
		if !bytes.Equal(out2[i], wants[i][off:off+n]) {
			t.Fatalf("GetBatch 区间读 out[%d] 不一致", i)
		}
	}
	rel2()

	// 错误路径：size<0 拒绝；缺失 key 整批 ErrNotFound。
	if _, _, err := st.GetBatch(ctx, keys, 0, -1); !errors.Is(err, rpcclient.ErrInvalidRange) {
		t.Fatalf("GetBatch(size=-1) 应 ErrInvalidRange，got %v", err)
	}
	if _, _, err := st.GetBatch(ctx, []string{keys[0], h.key("h3/missing")}, 0, size); !errors.Is(err, rpcclient.ErrNotFound) {
		t.Fatalf("GetBatch(含缺失 key) 应 ErrNotFound，got %v", err)
	}
	if out3, rel3, err := st.GetBatch(ctx, nil, 0, size); err != nil || out3 != nil || rel3 == nil {
		t.Fatalf("GetBatch(空 keys) 应为 no-op：err=%v out3=%v rel3==nil: %v", err, out3, rel3 == nil)
	} else {
		rel3()
	}
}

// TestH4GetFd：单 key 零拷贝读——shm 路径必须交付 fd>0 且 (fd,foff) 可 Pread 出
// 对象内容；rpc 强制 TCP 时回退拷贝路径（fd==0，data 正确）。
func TestH4GetFd(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})

	// 路径一：auto（同机 → shm）。
	st := h.newSDK(nil)
	waitFor(t, 15*time.Second, "SDK 发现实例 %s", func() bool { return st.HasLive() }, srv.info.Addr)
	key := h.key("h4/zero-copy")
	data := pattern(key, 1<<20)
	putExact(t, st, key, data)
	// st2 是全新 SDK（空路由缓存），定位只能靠 TiKV 索引（100ms 周期异步批量写），
	// 先等索引收敛，否则 st2 查不到 key 会走回源（未配 Source → ErrSourceUnset）。
	waitFor(t, 15*time.Second, "索引收敛（供 st2 经 TiKV 定位）", func() bool {
		ks, err := st.ListIndexKeys(h.ctx, h.keyPrefix+"h4/")
		return err == nil && len(ks) == 1
	}, 1)

	fd, foff, got, rel, err := st.GetFd(h.ctx, key, 0, -1)
	if err != nil {
		t.Fatalf("GetFd(shm): %v", err)
	}
	if fd <= 0 {
		t.Fatalf("GetFd(shm) 应交付 fd>0 的 splice 源，got fd=%d", fd)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("GetFd(shm) Data 内容不一致（len=%d/%d）", len(got), len(data))
	}
	if !bytes.Equal(preadAt(t, fd, foff, len(data)), data) {
		t.Fatalf("Pread(fd=%d, foff=%d) 与对象内容不一致：交付的 (fd,foff) 未指向对象数据", fd, foff)
	}
	rel()
	// 释放后连接复用：普通 Get 仍正常。
	getExact(t, st, key, data)

	// 路径二：强制 TCP —— 连接不支持 fd 交付，回退拷贝路径（fd==0）。
	st2 := h.newSDK(func(c *taihuclient.ClusterConfig) { c.Transport = taihuclient.TransportRPC })
	waitFor(t, 15*time.Second, "TCP SDK 发现实例 %s", func() bool { return st2.HasLive() }, srv.info.Addr)
	fd2, _, got2, rel2, err := st2.GetFd(h.ctx, key, 0, -1)
	if err != nil {
		t.Fatalf("GetFd(rpc): %v", err)
	}
	if fd2 != 0 {
		t.Fatalf("GetFd(rpc) 应回退拷贝路径 fd==0，got fd=%d", fd2)
	}
	if !bytes.Equal(got2, data) {
		t.Fatalf("GetFd(rpc) 拷贝路径内容不一致（len=%d/%d）", len(got2), len(data))
	}
	rel2()
}

// TestH5GetFdBatch：批量零拷贝读——逐 buf 校验 (Fd,Foff,Data)、逐个 Release、
// 整批兜底 release 幂等、流复用与错误路径。
func TestH5GetFdBatch(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	st := h.newSDK(func(c *taihuclient.ClusterConfig) { c.Source = notFoundSource })
	waitFor(t, 15*time.Second, "SDK 发现实例 %s", func() bool { return st.HasLive() }, srv.info.Addr)

	ctx := h.ctx
	const size = 128 << 10 // 注意：SDK 层 GetBatch 不接受 size<0（-1 需逐 key Get）
	keys := []string{h.key("h5/k0"), h.key("h5/k1"), h.key("h5/k2"), h.key("h5/k3")}
	wants := make([][]byte, len(keys))
	for i, k := range keys {
		wants[i] = pattern(k, size)
	}
	if err := st.PutBatchKeys(ctx, keys, size, wants); err != nil {
		t.Fatalf("PutBatchKeys: %v", err)
	}

	bufs, releaseAll, err := st.GetFdBatch(ctx, keys, 0, size)
	if err != nil {
		t.Fatalf("GetFdBatch: %v", err)
	}
	if len(bufs) != len(keys) {
		t.Fatalf("GetFdBatch 返回 %d bufs, want %d", len(bufs), len(keys))
	}
	for i, b := range bufs {
		if b.Fd <= 0 {
			t.Fatalf("GetFdBatch bufs[%d].Fd 应 >0（同机 shm），got %d", i, b.Fd)
		}
		if !bytes.Equal(b.Data, wants[i]) {
			t.Fatalf("GetFdBatch bufs[%d].Data 内容不一致（len=%d）", i, len(b.Data))
		}
		if !bytes.Equal(preadAt(t, b.Fd, b.Foff, size), wants[i]) {
			t.Fatalf("GetFdBatch bufs[%d] (fd=%d,foff=%d) Pread 与对象内容不一致", i, b.Fd, b.Foff)
		}
	}
	// 逐个 Release（幂等）+ 整批兜底再调一次（不得 panic）。
	for _, b := range bufs {
		b.Release()
		b.Release()
	}
	releaseAll()
	releaseAll()
	// 全部归还后流已复用：普通 Get 正常。
	getExact(t, st, keys[0], wants[0])

	// 错误路径：size<0 拒绝；缺失 key 整批 ErrNotFound。
	if _, _, err := st.GetFdBatch(ctx, keys, 0, -1); !errors.Is(err, rpcclient.ErrInvalidRange) {
		t.Fatalf("GetFdBatch(size=-1) 应 ErrInvalidRange，got %v", err)
	}
	if _, _, err := st.GetFdBatch(ctx, []string{keys[0], h.key("h5/missing")}, 0, size); !errors.Is(err, rpcclient.ErrNotFound) {
		t.Fatalf("GetFdBatch(含缺失 key) 应 ErrNotFound，got %v", err)
	}
}

// TestH6Ops：CheckPoolIsValid / UsageGet 在有在线实例时的运维语义。
func TestH6Ops(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	st := h.newSDK(nil)
	waitFor(t, 15*time.Second, "SDK 发现实例 %s", func() bool { return st.HasLive() }, srv.info.Addr)

	if err := st.CheckPoolIsValid(); err != nil {
		t.Fatalf("CheckPoolIsValid（有在线实例）应 nil，got %v", err)
	}
	u, err := st.UsageGet()
	if err != nil {
		t.Fatalf("UsageGet: %v", err)
	}
	if u < 0 || u >= 1 {
		t.Fatalf("UsageGet = %v，应在 [0,1)（16GiB 设备刚写入少量对象）", u)
	}
}
