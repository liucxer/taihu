package taihuclient

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/rpcclient"
	"github.com/liucxer/taihu/pkg/ierr"
)

// storage.go 覆盖：clientFor 各传输分支、Put/Get/Stat/Delete 端到端、回源重建、
// 索引/缓存回填与并发慢路径。需要真实服务端（internal/transport，127.0.0.1:0）。

func TestNewClusterRequiresKV(t *testing.T) {
	if _, err := NewCluster(ClusterConfig{}); err == nil {
		t.Fatal("NewCluster must reject nil KV")
	}
}

func TestConnKeyBuckets(t *testing.T) {
	inst := cluster.InstanceInfo{Addr: "1.2.3.4:1", ShmAddr: "/dev/shm/x.sock"}
	if got := connKey(inst, TransportRPC); got != "tcp://1.2.3.4:1" {
		t.Fatalf("rpc key=%q", got)
	}
	if got := connKey(inst, TransportShm); got != "shm:///dev/shm/x.sock" {
		t.Fatalf("shm key=%q", got)
	}
	if got := connKey(inst, TransportAuto); got != "shm:///dev/shm/x.sock" {
		t.Fatalf("auto+shm key=%q", got)
	}
	if got := connKey(cluster.InstanceInfo{Addr: "1.2.3.4:1"}, TransportAuto); got != "tcp://1.2.3.4:1" {
		t.Fatalf("auto tcp key=%q", got)
	}
}

func TestClientForTransportRPC(t *testing.T) {
	addrA, addrB := multiAddrTestServerPair(t)
	inst := &cluster.InstanceInfo{
		Name: "rpc-multi", Node: "remote", Hostname: "remote-node",
		Addr: addrA, Addrs: []string{addrA, addrB},
	}
	// 第二个实例（单地址）用于覆盖连接缓存 copy-on-write 复制旧快照的分支。
	inst2 := &cluster.InstanceInfo{Name: "rpc-single", Node: "remote", Hostname: "remote-node", Addr: multiAddrTestServer(t)}
	s := newClusterWithInst(t, cluster.NewMemoryKV(), func(c *ClusterConfig) {
		c.Transport = TransportRPC
		c.Conns = 1
		c.HeartbeatTimeout = time.Hour
	}, inst, inst2)

	c, err := s.clientFor(*inst)
	if err != nil {
		t.Fatalf("clientFor forced rpc: %v", err)
	}
	if err := c.Put(context.Background(), "rpc-k", 5, []byte("hello")); err != nil {
		t.Fatalf("Put via forced rpc: %v", err)
	}

	// Addrs 为空：强制 TCP 回退单地址分支。
	c2, err := s.clientFor(*inst2)
	if err != nil {
		t.Fatalf("clientFor forced rpc single addr: %v", err)
	}
	if err := c2.Put(context.Background(), "rpc-k2", 2, []byte("ok")); err != nil {
		t.Fatalf("Put via single addr: %v", err)
	}

	// 再次取第一个实例：连接缓存命中。
	if c3, err := s.clientFor(*inst); err != nil || c3 != c {
		t.Fatalf("clientFor cache hit: %p != %p err=%v", c3, c, err)
	}
}

func TestClientForTransportShmErrors(t *testing.T) {
	// 强制 shm 但实例未开放 shm → 明确报错。
	inst := &cluster.InstanceInfo{Name: "shm-none", Node: "here", Hostname: "here", Addr: closedTCPAddr(t)}
	s := newClusterForInst(t, inst, func(c *ClusterConfig) { c.Transport = TransportShm })
	if _, err := s.clientFor(*inst); err == nil {
		t.Fatal("want error when transport=shm but ShmAddr is empty")
	}

	// shm socket 不存在 → 拨号失败透出（不连数据面）。
	inst2 := &cluster.InstanceInfo{
		Name: "shm-bad", Node: "here", Hostname: "here",
		Addr: closedTCPAddr(t), ShmAddr: filepath.Join(t.TempDir(), "missing.sock"),
	}
	s2 := newClusterForInst(t, inst2, func(c *ClusterConfig) { c.Transport = TransportShm })
	if _, err := s2.clientFor(*inst2); err == nil {
		t.Fatal("want dial error for missing shm socket")
	}
}

func TestClientForAutoShmFallbackToTCP(t *testing.T) {
	addr := multiAddrTestServer(t)
	inst := &cluster.InstanceInfo{
		Name: "auto-fb", Node: "here", Hostname: localHostname(t),
		Addr: addr, ShmAddr: filepath.Join(t.TempDir(), "no-such.sock"),
	}
	s := newClusterForInst(t, inst, nil) // Transport 默认 auto：同机 shm 失败 → 回退 TCP
	c, err := s.clientFor(*inst)
	if err != nil {
		t.Fatalf("clientFor should fall back to TCP: %v", err)
	}
	if err := c.Put(context.Background(), "fb", 2, []byte("ok")); err != nil {
		t.Fatalf("Put after fallback: %v", err)
	}
	// 再取一次应命中缓存（copy-on-write 快照）。
	if c2, err := s.clientFor(*inst); err != nil || c2 != c {
		t.Fatalf("clientFor cache: %p != %p err=%v", c2, c, err)
	}
}

func TestClientForSlowPathDoubleCheck(t *testing.T) {
	addr := multiAddrTestServer(t)
	inst := &cluster.InstanceInfo{Name: "slow", Node: "remote", Hostname: "remote-node", Addr: addr}
	s := newClusterForInst(t, inst, nil)

	sc, err := rpcclient.DialPool(context.Background(), addr, 1)
	if err != nil {
		t.Fatalf("DialPool: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })

	key := connKey(*inst, s.cfg.Transport)
	s.mu.Lock()
	done := make(chan struct{})
	var got *rpcclient.Storage
	var gerr error
	go func() {
		defer close(done)
		got, gerr = s.clientFor(*inst) // 快路径 miss → 阻塞在 s.mu
	}()
	// 持锁期间让 goroutine 走完无锁快路径并阻塞在锁上，再填充缓存 → 触发锁内双检命中。
	time.Sleep(50 * time.Millisecond)
	s.conns.Store(map[string]*rpcclient.Storage{key: sc})
	s.mu.Unlock()
	<-done
	if gerr != nil || got != sc {
		t.Fatalf("slow-path double check: got=%p want=%p err=%v", got, sc, gerr)
	}
}

func TestStoragePutGetStatDeleteRemote(t *testing.T) {
	addr := multiAddrTestServer(t)
	inst := &cluster.InstanceInfo{Name: "e2e", Node: "remote", Hostname: "remote-node", Addr: addr}
	s := newClusterForInst(t, inst, nil)
	ctx := context.Background()
	payload := bytes.Repeat([]byte("payload-"), 512) // 4096B

	if err := s.Put(ctx, "obj-1", int64(len(payload)), payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, rel, err := s.Get(ctx, "obj-1", 0, int64(len(payload)))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Get mismatch: %d bytes", len(got))
	}
	rel()

	// 子区间读取。
	got, rel, err = s.Get(ctx, "obj-1", 8, 5)
	if err != nil || string(got) != "paylo" {
		t.Fatalf("sub-range Get: %q %v", got, err)
	}
	rel()

	if n, err := s.Stat(ctx, "obj-1"); err != nil || n != int64(len(payload)) {
		t.Fatalf("Stat: n=%d err=%v", n, err)
	}
	if _, err := s.Stat(ctx, "obj-missing"); err != rpcclient.ErrNotFound {
		t.Fatalf("Stat missing: %v", err)
	}

	if err := s.Delete(ctx, "obj-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 已删除且未配置回源 → ierr.ErrSourceUnset（不再从本地逐个试读）。
	if _, _, err := s.Get(ctx, "obj-1", 0, -1); !errors.Is(err, ierr.ErrSourceUnset) {
		t.Fatalf("Get after delete: %v", err)
	}
}

func TestStorageLocalInstanceStatAndDelete(t *testing.T) {
	addr := multiAddrTestServer(t)
	inst := &cluster.InstanceInfo{Name: "local-1", Node: "here", Hostname: localHostname(t), Addr: addr}
	s := newClusterForInst(t, inst, nil)
	ctx := context.Background()
	payload := []byte("local-payload")

	if err := s.Put(ctx, "lk-1", int64(len(payload)), payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Stat 直接命中本地实例。
	if n, err := s.Stat(ctx, "lk-1"); err != nil || n != int64(len(payload)) {
		t.Fatalf("Stat local: n=%d err=%v", n, err)
	}
	// 索引/缓存 miss 的删除 → 本地全部实例兜底（服务端无此 key，容忍 ErrNotFound）。
	if err := s.Delete(ctx, "lk-unknown"); err != nil {
		t.Fatalf("Delete local fallback: %v", err)
	}
	if err := s.Delete(ctx, "lk-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestStorageGetSourcePaths(t *testing.T) {
	addr := multiAddrTestServer(t)
	inst := &cluster.InstanceInfo{Name: "src", Node: "remote", Hostname: "remote-node", Addr: addr}
	payload := []byte("0123456789")
	srcErr := errors.New("source boom")
	s := newClusterForInst(t, inst, func(c *ClusterConfig) {
		c.Source = func(_ context.Context, key string) ([]byte, error) {
			if key == "boom" {
				return nil, srcErr
			}
			return payload, nil
		}
	})
	ctx := context.Background()

	// size=-1 → 读到结尾。
	data, rel, err := s.Get(ctx, "src-tail", 2, -1)
	if err != nil || string(data) != "23456789" {
		t.Fatalf("tail: %q %v", data, err)
	}
	rel()

	// off+size 越界 → 截到结尾。
	data, rel, err = s.Get(ctx, "src-clamp", 5, 100)
	if err != nil || string(data) != "56789" {
		t.Fatalf("clamp: %q %v", data, err)
	}
	rel()

	if _, _, err := s.Get(ctx, "src-off-neg", -1, 2); err != rpcclient.ErrInvalidRange {
		t.Fatalf("off<0: %v", err)
	}
	if _, _, err := s.Get(ctx, "src-off-big", int64(len(payload))+1, 2); err != rpcclient.ErrInvalidRange {
		t.Fatalf("off>len: %v", err)
	}

	// 回源错误透出。
	if _, _, err := s.Get(ctx, "boom", 0, -1); !errors.Is(err, srcErr) {
		t.Fatalf("source error: %v", err)
	}
}

func TestStorageGetFromDialErrorAndMiss(t *testing.T) {
	addr := multiAddrTestServer(t)
	inst := &cluster.InstanceInfo{Name: "g", Node: "remote", Hostname: "remote-node", Addr: addr}
	s := newClusterForInst(t, inst, nil)
	ctx := context.Background()
	dead := cluster.InstanceInfo{Name: "dead", Node: "remote", Hostname: "remote-node", Addr: closedTCPAddr(t)}

	// 首个实例拨号失败 → continue；第二个实例在线但对象不存在 → ErrNotFound。
	if _, _, err := s.getFrom(ctx, "nope", 0, -1, []cluster.InstanceInfo{dead, *inst}); err != rpcclient.ErrNotFound {
		t.Fatalf("getFrom: %v", err)
	}
	// 全部实例不可用 → ErrNotFound。
	if _, _, err := s.getFrom(ctx, "nope", 0, -1, []cluster.InstanceInfo{dead}); err != rpcclient.ErrNotFound {
		t.Fatalf("getFrom all dead: %v", err)
	}
	// 数据面返回非 NotFound 错误（非法区间）→ 原样透出，不回源。
	if err := s.Put(ctx, "existing", 3, []byte("abc")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, _, err := s.getFrom(ctx, "existing", 100, -1, []cluster.InstanceInfo{*inst}); err != rpcclient.ErrInvalidRange {
		t.Fatalf("getFrom invalid range: %v", err)
	}
}

func TestStoragePutErrors(t *testing.T) {
	// 无在线实例。
	s := newClusterWithKV(t, cluster.NewMemoryKV(), nil)
	if err := s.Put(context.Background(), "k", 1, []byte("x")); !errors.Is(err, ErrNoInstances) {
		t.Fatalf("Put without instance: %v", err)
	}

	// 实例在线但拨号失败。
	inst := &cluster.InstanceInfo{Name: "dead", Node: "remote", Hostname: "remote-node", Addr: closedTCPAddr(t)}
	s2 := newClusterForInst(t, inst, nil)
	if err := s2.Put(context.Background(), "k", 1, []byte("x")); err == nil {
		t.Fatal("Put must surface dial error")
	}

	// 数据面写入失败（size > len(in) → ErrShortWrite）原样透出。
	instOK := &cluster.InstanceInfo{Name: "live", Node: "remote", Hostname: "remote-node", Addr: multiAddrTestServer(t)}
	s3 := newClusterForInst(t, instOK, nil)
	if err := s3.Put(context.Background(), "k", 8, []byte("x")); !errors.Is(err, rpcclient.ErrShortWrite) {
		t.Fatalf("Put short write: %v", err)
	}
}

func TestStorageIndexLookupFallbackAndPreload(t *testing.T) {
	addr := multiAddrTestServer(t)
	inst := &cluster.InstanceInfo{Name: "idx", Node: "remote", Hostname: "remote-node", Addr: addr}
	s := newClusterForInst(t, inst, nil)
	ctx := context.Background()

	// 直接写索引 KV（模拟进程重启后的冷路由缓存）：cache miss → 索引命中 → 回填 cache。
	if err := s.cfg.KV.Put(ctx, []byte(cluster.IndexKeyPrefix+"idx-key"), []byte("idx")); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	if _, _, err := s.Get(ctx, "idx-key", 0, -1); !errors.Is(err, ierr.ErrSourceUnset) {
		t.Fatalf("Get via index: %v", err)
	}
	if name, ok := s.cache.get("idx-key"); !ok || name != "idx" {
		t.Fatalf("index lookup must fill route cache: %q ok=%v", name, ok)
	}

	// PreloadRoute：空集合直接返回；含索引命中与 miss 两类 key。
	s.PreloadRoute(ctx, nil)
	s.PreloadRoute(ctx, []string{"idx-key", "no-such-key"})

	// Delete 走索引定位（服务端无此 key → 容忍 ErrNotFound）。
	s.cache.delete("idx-key")
	if err := s.Delete(ctx, "idx-key"); err != nil {
		t.Fatalf("Delete via index: %v", err)
	}
	if name, err := s.index.get(ctx, "idx-key"); err != nil || name != "" {
		t.Fatalf("index entry should be deleted: %q %v", name, err)
	}
}
