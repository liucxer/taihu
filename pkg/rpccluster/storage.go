package rpccluster

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/version"
	"github.com/liucxer/taihu/pkg/rpcclient"
	"github.com/liucxer/taihu/pkg/taihu"
)

var (
	// ErrNoInstances 集群无在线实例可写。
	ErrNoInstances = errors.New("taihu-cluster: no online instance")
	// ErrSourceUnset 集群全 miss 且未配置回源。
	ErrSourceUnset = errors.New("taihu-cluster: no source getter configured")
)

// Storage 集群对象存储：写=本地优先选实例（首写锚定），读=本地实例直查 →
// 索引定位远端 → 回源重建。实现 taihu.ObjectStore + Get/Stat/Close。
// 数据面复用 rpcclient.Storage（netpoll 零拷贝），定位层不进数据面热路径。
//
// 约束（设计文档）：key 不可变（无覆盖写）；更新语义=Delete 后重建；
// 缓存可丢——索引尽力而为，TiKV 不可用退化为"本地实例 + 回源"。
type Storage struct {
	cfg      ClusterConfig
	registry *InstanceRegistry
	picker   *InstancePicker
	index    *IndexManager
	cache    *RouteCache

	mu    sync.Mutex
	conns map[string]*rpcclient.Storage // addr -> 数据面客户端（懒建复用）

	// SDK 客户端保活（配置了 ClientID 时启用）：心跳 goroutine 取消函数与注销所需。
	clientCancel context.CancelFunc
	clientKV     cluster.KV
	clientID     string
}

var _ taihu.ObjectStore = (*Storage)(nil)

// NewCluster 构建集群客户端并启动发现/索引后台任务。
func NewCluster(cfg ClusterConfig) (*Storage, error) {
	if cfg.KV == nil {
		return nil, errors.New("rpccluster: KV is required")
	}
	reg := NewInstanceRegistry(cfg.KV, cfg.Node, cfg.RefreshInterval, cfg.HeartbeatTimeout)
	s := &Storage{
		cfg:      cfg,
		registry: reg,
		index:    NewIndexManager(cfg.KV),
		cache:    NewRouteCache(4096),
		conns:    make(map[string]*rpcclient.Storage),
	}
	s.picker = NewInstancePicker(reg, cfg.UsageThreshold)
	reg.Start()
	s.index.Start()
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
		Node:       cfg.Node,
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

// conns 缓存键：本地实例用 shm 地址，跨节点用网络地址；同机 shm 走共享内存零拷贝，
// 跨节点走 netpoll TCP。按传输类型分桶避免地址冲突。
func connKey(inst cluster.InstanceInfo) string {
	if inst.ShmAddr != "" {
		return "shm://" + inst.ShmAddr
	}
	return "tcp://" + inst.Addr
}

// clientFor 懒建并缓存某实例的数据面连接（幂等；连接复用避免重复拨号）。
// 本地实例（同 node）优先用 DialShm 共享内存；否则（跨节点）用 DialPool TCP。
func (s *Storage) clientFor(inst cluster.InstanceInfo) (*rpcclient.Storage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := connKey(inst)
	if c, ok := s.conns[key]; ok {
		return c, nil
	}
	var (
		c   *rpcclient.Storage
		err error
	)
	if s.cfg.Node != "" && inst.ShmAddr != "" {
		// 同机共享内存：零拷贝传输（若本机未开放 shm，则回退 TCP 由 shmAddr 为空判空）
		c, err = rpcclient.DialShm(context.Background(), inst.ShmAddr)
		if err != nil {
			// shm 不可用（socket 未建等）回退 TCP，保证功能不中断
			c, err = rpcclient.DialPool(context.Background(), inst.Addr, 1)
		}
	} else {
		c, err = rpcclient.DialPool(context.Background(), inst.Addr, 1)
	}
	if err != nil {
		return nil, err
	}
	s.conns[key] = c
	return c, nil
}

// Put 写入对象：Picker 本地优先选实例 → 数据面写入 → 异步索引 + 路由缓存。
// key 只写一次（无覆盖写），无需写前查归属。
func (s *Storage) Put(ctx context.Context, key string, size int64, in []byte) error {
	inst, ok := s.picker.Pick()
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
	s.index.Put(key, inst.Name)
	s.cache.Put(key, inst.Name)
	return nil
}

// Get 读取对象 [off, off+size) 子区间（size=-1 读至结尾）。
// 顺序：路由缓存 → TiKV 索引定位实例 → 回源重建（不再本地逐个试读）。
// 返回 (data, release, err)，用毕必须 release()（幂等，归还池化缓冲）。
func (s *Storage) Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error) {
	// 1) 路由缓存优先（写路径已记录 key→实例名），2) 未命中查 TiKV 索引
	if inst, ok := s.lookup(ctx, key); ok {
		if data, rel, err := s.getFrom(ctx, key, off, size, []cluster.InstanceInfo{inst}); err != taihu.ErrNotFound {
			return data, rel, err
		}
	}
	// 3) 回源重建
	return s.getFromSource(ctx, key, off, size)
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
		if err != taihu.ErrNotFound {
			return nil, nil, err
		}
	}
	return nil, nil, taihu.ErrNotFound
}

// lookup 路由缓存 → 索引，解析为活跃实例。
func (s *Storage) lookup(ctx context.Context, key string) (cluster.InstanceInfo, bool) {
	if name, ok := s.cache.Get(key); ok {
		if inst, ok := s.registry.Lookup(name); ok {
			return inst, true
		}
	}
	name, err := s.index.Get(ctx, key)
	if err != nil || name == "" {
		return cluster.InstanceInfo{}, false
	}
	inst, ok := s.registry.Lookup(name)
	if ok {
		s.cache.Put(key, name)
	}
	return inst, ok
}

// getFromSource 回源拉整对象，按 [off, size) 截取返回，并本地优先回写缓存。
// 返回的 data 无池化归属，release 为 noop。
func (s *Storage) getFromSource(ctx context.Context, key string, off, size int64) ([]byte, func(), error) {
	if s.cfg.Source == nil {
		return nil, nil, ErrSourceUnset
	}
	data, err := s.cfg.Source(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	n := int64(len(data))
	if off < 0 || off > n {
		return nil, nil, taihu.ErrInvalidRange
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
			if err := c.Delete(ctx, key); err != nil && err != taihu.ErrNotFound {
				return err
			}
		}
	} else {
		for _, inst := range s.registry.Snapshot().local {
			if c, err := s.clientFor(inst); err == nil {
				if err := c.Delete(ctx, key); err != nil && err != taihu.ErrNotFound {
					return err
				}
			}
		}
	}
	_ = s.index.Delete(ctx, key)
	s.cache.Delete(key)
	return nil
}

// Stat 返回对象逻辑大小：本地实例优先 → 索引定位。全 miss 返回 ErrNotFound。
func (s *Storage) Stat(ctx context.Context, key string) (int64, error) {
	for _, inst := range s.registry.Snapshot().local {
		if c, err := s.clientFor(inst); err == nil {
			if n, err := c.Stat(ctx, key); err == nil {
				return n, nil
			} else if err != taihu.ErrNotFound {
				return 0, err
			}
		}
	}
	if inst, ok := s.lookup(ctx, key); ok {
		if c, err := s.clientFor(inst); err == nil {
			return c.Stat(ctx, key)
		}
	}
	return 0, taihu.ErrNotFound
}

// Close 停止后台任务并关闭全部数据面连接；SDK 客户端保活同时注销注册记录。
func (s *Storage) Close() error {
	s.index.Stop()
	s.registry.Stop()
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
	for addr, c := range s.conns {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
		delete(s.conns, addr)
	}
	return first
}
