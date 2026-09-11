package rpccluster

import (
	"context"
	"errors"
	"sync"

	"github.com/liucxer/taihu/internal/cluster"
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
	return s, nil
}

// clientFor 懒建并缓存某实例的数据面连接（幂等；连接复用避免重复拨号）。
func (s *Storage) clientFor(inst cluster.InstanceInfo) (*rpcclient.Storage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.conns[inst.Addr]; ok {
		return c, nil
	}
	c, err := rpcclient.DialPool(context.Background(), inst.Addr, 1)
	if err != nil {
		return nil, err
	}
	s.conns[inst.Addr] = c
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
// 顺序：本地实例直查 → 索引定位远端 → 回源重建。
// 返回 (data, release, err)，用毕必须 release()（幂等，归还池化缓冲）。
func (s *Storage) Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error) {
	// 1) 本地实例优先（同 node 全部实例，逐个试读）
	if data, rel, err := s.getFrom(ctx, key, off, size, s.registry.Snapshot().local); err != taihu.ErrNotFound {
		return data, rel, err
	}
	// 2) 索引定位远端
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

// Close 停止后台任务并关闭全部数据面连接。
func (s *Storage) Close() error {
	s.index.Stop()
	s.registry.Stop()
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
