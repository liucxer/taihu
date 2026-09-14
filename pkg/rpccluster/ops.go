package rpccluster

import (
	"context"
	"strings"

	"github.com/liucxer/taihu/internal/cluster"
)

// HasLive 是否有在线实例可服务。
func (s *Storage) HasLive() bool {
	return len(s.registry.Snapshot().all) > 0
}

// CheckPoolIsValid 确认集群有在线实例可服务（挂载/卷创建时尽早暴露配置错误）。
func (s *Storage) CheckPoolIsValid() error {
	if !s.HasLive() {
		return ErrNoInstances
	}
	return nil
}

// UsageGet 返回在线实例最低水位（0~1），无实例报错。用于对象存储用量上报。
func (s *Storage) UsageGet() (float64, error) {
	snap := s.registry.Snapshot()
	var best float64
	found := false
	for _, inf := range append(append([]cluster.InstanceInfo{}, snap.local...), snap.remote...) {
		if inf.Capacity <= 0 {
			continue
		}
		ratio := float64(inf.Used) / float64(inf.Capacity)
		if !found || ratio < best {
			best, found = ratio, true
		}
	}
	if !found {
		return 0, ErrNoInstances
	}
	return best, nil
}

// ListIndexKeys 从 KV 索引区（cluster.IndexKeyPrefix）枚举全部 key，剔除前缀后返回。
// 用于迁移 / destroy 等需要全量枚举对象的场景；prefix 过滤索引键前缀。
func (s *Storage) ListIndexKeys(ctx context.Context, prefix string) ([]string, error) {
	start := []byte(cluster.IndexKeyPrefix + prefix)
	end := []byte(cluster.IndexKeyPrefix + prefix + "\xff")
	ks, _, err := s.cfg.KV.Scan(ctx, start, end, 0)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, strings.TrimPrefix(string(k), cluster.IndexKeyPrefix))
	}
	return out, nil
}
