package taihuclient

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/rpcclient"
	"github.com/liucxer/taihu/internal/version"
)

var (
	// ErrNoInstances 集群无在线实例可写。
	ErrNoInstances = errors.New("taihu-cluster: no online instance")
	// errSourceUnset 集群全 miss 且未配置回源。
	errSourceUnset = errors.New("taihu-cluster: no source getter configured")
)

// Storage 集群对象存储：写=本地优先选实例（首写锚定），读=本地实例直查 →
// 索引定位远端 → 回源重建。满足 rpcclient.ObjectStore。
// 数据面复用 rpcclient.Storage（netpoll 零拷贝），定位层不进数据面热路径。
//
// 约束（设计文档）：key 不可变（无覆盖写）；更新语义=Delete 后重建；
// 缓存可丢——索引尽力而为，TiKV 不可用退化为"本地实例 + 回源"。
type Storage struct {
	cfg      ClusterConfig
	registry *instanceRegistry
	picker   *instancePicker
	index    *indexManager
	cache    *routeCache
	hostname string // 本机 hostname（os.Hostname）：与实例注册 Hostname 比较判同机 → shm / TCP

	mu    sync.Mutex
	conns atomic.Value // map[string]*rpcclient.Storage 只读快照（copy-on-write，读无锁）
	// connsMu 仅保护懒建连接的写路径（快照替换），读热路径不触碰。

	// SDK 客户端保活（配置了 ClientID 时启用）：心跳 goroutine 取消函数与注销所需。
	clientCancel context.CancelFunc
	clientKV     cluster.KV
	clientID     string
}

// 编译期断言：集群客户端与直连客户端的方法集不得漂移（ObjectStore 声明在
// rpcclient，两个实现都必须满足）。接口声明处无法反向断言，故放在这里。
var _ rpcclient.ObjectStore = (*Storage)(nil)

// NewCluster 构建集群客户端并启动发现/索引后台任务。
func NewCluster(cfg ClusterConfig) (*Storage, error) {
	if cfg.KV == nil {
		return nil, errors.New("taihuclient: KV is required")
	}
	hostname, _ := os.Hostname()
	reg := newInstanceRegistry(cfg.KV, hostname, cfg.RefreshInterval, cfg.HeartbeatTimeout)
	s := &Storage{
		cfg:      cfg,
		registry: reg,
		hostname: hostname,
		index:    newIndexManager(cfg.KV),
		cache:    newRouteCache(4096),
	}
	s.conns.Store(make(map[string]*rpcclient.Storage))
	s.picker = newInstancePicker(reg, cfg.UsageThreshold, cfg.WriteRouting)
	reg.start()
	s.index.start()
	s.startClientKeepalive(cfg)
	return s, nil
}

// startClientKeepalive 配置了 ClientID 时自动向 KV 注册 SDK 客户端并周期心跳续约
// （taihu-cli 设计文档 §5）。注册失败不阻断（心跳每周期重试注册，自愈）。
func (s *Storage) startClientKeepalive(cfg ClusterConfig) {
	if cfg.ClientID == "" || cfg.KV == nil {
		return
	}
	host, _ := os.Hostname()
	info := &cluster.ClientInfo{
		ID:         cfg.ClientID,
		Node:       cfg.ClientName,
		Addr:       cfg.ClientAddr,
		Host:       host,
		Pid:        os.Getpid(),
		SDKVersion: version.String(),
		Extra:      cfg.ClientLabels,
		StartTime:  time.Now().Unix(),
	}
	refresh := func() *cluster.ClientInfo { n := *info; return &n }
	_ = cluster.RegisterClient(context.Background(), cfg.KV, refresh())
	var hctx context.Context
	hctx, s.clientCancel = context.WithCancel(context.Background())
	go cluster.RunClientHeartbeat(hctx, cfg.KV, refresh, time.Second)
	s.clientKV = cfg.KV
	s.clientID = cfg.ClientID
}

// conns 缓存键：按实际传输分桶——shm（共享内存零拷贝）走 "shm://"+ShmAddr，
// TCP 走 "tcp://"+Addr，避免同一实例在强制传输下连接误复用。auto 时同机（有
// ShmAddr 且 hostname 一致才可能走 shm）用 shm 键，否则 tcp 键。
func connKey(inst cluster.InstanceInfo, transport string) string {
	if transport == TransportRPC {
		return "tcp://" + inst.Addr
	}
	if transport == TransportShm || inst.ShmAddr != "" {
		return "shm://" + inst.ShmAddr
	}
	return "tcp://" + inst.Addr
}

// clientFor 懒建并缓存某实例的数据面连接（幂等；连接复用避免重复拨号）。
// 无锁快路径：连接缓存为 atomic.Value 只读快照，已建连接直接查表返回（零锁）；
// miss 时加锁双检并 copy-on-write 替换快照（仅懒建首连触碰锁）。
// 传输方式由 cfg.Transport 决定：auto（默认，同机 shm / 跨节点 TCP）、
// rpc（强制 TCP，含同机）或 shm（强制共享内存，仅同机实例）。
func (s *Storage) clientFor(inst cluster.InstanceInfo) (*rpcclient.Storage, error) {
	key := connKey(inst, s.cfg.Transport)
	if c, ok := s.conns.Load().(map[string]*rpcclient.Storage)[key]; ok {
		return c, nil
	}
	// 懒建慢路径（首个请求触发）：加锁双检，避免并发重复拨号。
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.conns.Load().(map[string]*rpcclient.Storage)[key]; ok {
		return c, nil
	}
	var (
		c   *rpcclient.Storage
		err error
	)
	switch s.cfg.Transport {
	case TransportRPC:
		// 强制 TCP：同机实例也走网络路径（测同机 RPC 性能 / 网络路径正确性）。
		if len(inst.Addrs) > 0 {
			c, err = rpcclient.DialPoolMulti(context.Background(), inst.Addrs, s.cfg.Conns)
		} else {
			c, err = rpcclient.DialPool(context.Background(), inst.Addr, s.cfg.Conns)
		}
	case TransportShm:
		// 强制共享内存：仅同机实例可用（uds socket）。
		if inst.ShmAddr == "" {
			return nil, errors.New("taihuclient: instance has no shm addr (transport=shm requires same-host instance)")
		}
		c, err = rpcclient.DialShmPool(context.Background(), inst.ShmAddr, s.cfg.Conns)
	default:
		// 自动（默认）：客户端与服务端同机（Hostname 一致）优先走共享内存（零拷贝传输），
		// shm 不可用（socket 未建等）回退 TCP，保证功能不中断。
		if s.hostname != "" && inst.Hostname == s.hostname && inst.ShmAddr != "" {
			c, err = rpcclient.DialShmPool(context.Background(), inst.ShmAddr, s.cfg.Conns)
			if err != nil {
				c, err = rpcclient.DialPool(context.Background(), inst.Addr, s.cfg.Conns)
			}
		} else {
			// 跨节点（Hostname 不一致）：走 TCP。服务端通告多地址（Addrs）时为每个地址各建
			// cfg.Conns 条连接，读写按 round-robin 均分到全部地址链路；旧 server 无 Addrs
			// 时回退单地址（兼容）。
			if len(inst.Addrs) > 0 {
				c, err = rpcclient.DialPoolMulti(context.Background(), inst.Addrs, s.cfg.Conns)
			} else {
				c, err = rpcclient.DialPool(context.Background(), inst.Addr, s.cfg.Conns)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	// copy-on-write：复制旧快照 + 新连接后原子替换（读者始终看到完整 map）。
	old := s.conns.Load().(map[string]*rpcclient.Storage)
	nw := make(map[string]*rpcclient.Storage, len(old)+1)
	for k, v := range old {
		nw[k] = v
	}
	nw[key] = c
	s.conns.Store(nw)
	return c, nil
}

// Put 写入对象：Picker 本地优先选实例 → 数据面写入 → 异步索引 + 路由缓存。
// key 只写一次（无覆盖写），无需写前查归属。
func (s *Storage) Put(ctx context.Context, key string, size int64, in []byte) error {
	inst, ok := s.picker.pick()
	if !ok {
		return ErrNoInstances
	}
	c, err := s.clientFor(inst)
	if err != nil {
		return err
	}
	if err := c.Put(ctx, key, size, in); err != nil {
		return err
	}
	s.index.put(key, inst.Name)
	s.cache.put(key, inst.Name)
	return nil
}

// Get 读取对象 [off, off+size) 子区间（size=-1 读至结尾）。
// 顺序：路由缓存 → TiKV 索引定位实例 → 回源重建（不再本地逐个试读）。
// 返回 (data, release, err)，用毕必须 release()（幂等，归还池化缓冲）。
func (s *Storage) Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error) {
	// 1) 路由缓存优先（写路径已记录 key→实例名），2) 未命中查 TiKV 索引
	if inst, ok := s.lookup(ctx, key); ok {
		if data, rel, err := s.getFrom(ctx, key, off, size, []cluster.InstanceInfo{inst}); err != rpcclient.ErrNotFound {
			return data, rel, err
		}
	}
	// 3) 回源重建
	return s.getFromSource(ctx, key, off, size)
}

// PreloadRoute 预热路由缓存：逐个 key 查索引/路由并填充 routeCache（不读数据）。
// 压测/预热场景用：热缓存下 Get 首查 routeCache 命中，不再打 TiKV 索引，
// 消除"每 key 先查 TiKV"的冷启动开销。串行预热大 key 集时 TiKV 查询延迟
// 主导（~10ms/key），故用固定并发 worker 并行填充（lookup 幂等且并发安全）。
func (s *Storage) PreloadRoute(ctx context.Context, keys []string) {
	const workers = 64
	if len(keys) == 0 {
		return
	}
	ch := make(chan string)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for key := range ch {
				_, _ = s.lookup(ctx, key)
			}
		}()
	}
	for _, key := range keys {
		ch <- key
	}
	close(ch)
	wg.Wait()
}

// getFrom 从给定实例列表逐个试读，全部 miss 返回 ErrNotFound。
func (s *Storage) getFrom(ctx context.Context, key string, off, size int64, insts []cluster.InstanceInfo) ([]byte, func(), error) {
	for _, inst := range insts {
		c, err := s.clientFor(inst)
		if err != nil {
			continue
		}
		data, rel, err := c.Get(ctx, key, off, size)
		if err == nil {
			return data, rel, nil
		}
		if err != rpcclient.ErrNotFound {
			return nil, nil, err
		}
	}
	return nil, nil, rpcclient.ErrNotFound
}

// lookup 路由缓存 → 索引，解析为活跃实例。
func (s *Storage) lookup(ctx context.Context, key string) (cluster.InstanceInfo, bool) {
	if name, ok := s.cache.get(key); ok {
		if inst, ok := s.registry.lookup(name); ok {
			return inst, true
		}
	}
	name, err := s.index.get(ctx, key)
	if err != nil || name == "" {
		return cluster.InstanceInfo{}, false
	}
	inst, ok := s.registry.lookup(name)
	if ok {
		s.cache.put(key, name)
	}
	return inst, ok
}

// getFromSource 回源拉整对象，按 [off, size) 截取返回，并本地优先回写缓存。
// 返回的 data 无池化归属，release 为 noop。
func (s *Storage) getFromSource(ctx context.Context, key string, off, size int64) ([]byte, func(), error) {
	if s.cfg.Source == nil {
		return nil, nil, errSourceUnset
	}
	data, err := s.cfg.Source(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	n := int64(len(data))
	if off < 0 || off > n {
		return nil, nil, rpcclient.ErrInvalidRange
	}
	if size < 0 || off+size > n {
		size = n - off
	}
	// 回写缓存（本地优先 Put；失败不阻塞读）
	_ = s.Put(ctx, key, n, data)
	return data[off : off+size], func() {}, nil
}

// Delete 删除对象映射：索引定位删，索引 miss 则删本地全部实例；并清索引/路由缓存。
func (s *Storage) Delete(ctx context.Context, key string) error {
	if inst, ok := s.lookup(ctx, key); ok {
		if c, err := s.clientFor(inst); err == nil {
			if err := c.Delete(ctx, key); err != nil && err != rpcclient.ErrNotFound {
				return err
			}
		}
	} else {
		for _, inst := range s.registry.snapshot().local {
			if c, err := s.clientFor(inst); err == nil {
				if err := c.Delete(ctx, key); err != nil && err != rpcclient.ErrNotFound {
					return err
				}
			}
		}
	}
	_ = s.index.delete(ctx, key)
	s.cache.delete(key)
	return nil
}

// Stat 返回对象逻辑大小：本地实例优先 → 索引定位。全 miss 返回 ErrNotFound。
func (s *Storage) Stat(ctx context.Context, key string) (int64, error) {
	for _, inst := range s.registry.snapshot().local {
		if c, err := s.clientFor(inst); err == nil {
			if n, err := c.Stat(ctx, key); err == nil {
				return n, nil
			} else if err != rpcclient.ErrNotFound {
				return 0, err
			}
		}
	}
	if inst, ok := s.lookup(ctx, key); ok {
		if c, err := s.clientFor(inst); err == nil {
			return c.Stat(ctx, key)
		}
	}
	return 0, rpcclient.ErrNotFound
}

// Close 停止后台任务并关闭全部数据面连接；SDK 客户端保活同时注销注册记录。
func (s *Storage) Close() error {
	s.index.stop()
	s.registry.stop()
	// SDK 客户端注销：停止心跳并尽力删除注册 key（避免残留僵尸客户端）。
	if s.clientCancel != nil {
		s.clientCancel()
		if s.clientKV != nil {
			_ = cluster.UnregisterClient(context.Background(), s.clientKV, s.clientID)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	// 遍历快照关闭连接；随后以空快照原子替换（不修改旧 map，避免并发读者冲突）。
	for _, c := range s.conns.Load().(map[string]*rpcclient.Storage) {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	s.conns.Store(make(map[string]*rpcclient.Storage))
	return first
}
