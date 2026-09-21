package taihuclient

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
)

// errKV 包装 cluster.KV，按需让指定操作返回错误（错误/降级路径注入）。
// 只出现在测试里：生产代码不感知。
type errKV struct {
	cluster.KV
	scanErr  error
	getErr   error
	putErr   error
	delErr   error
	batchErr error
}

func (k *errKV) Scan(ctx context.Context, start, end []byte, limit int) ([][]byte, [][]byte, error) {
	if k.scanErr != nil {
		return nil, nil, k.scanErr
	}
	return k.KV.Scan(ctx, start, end, limit)
}

func (k *errKV) Get(ctx context.Context, key []byte) ([]byte, error) {
	if k.getErr != nil {
		return nil, k.getErr
	}
	return k.KV.Get(ctx, key)
}

func (k *errKV) Put(ctx context.Context, key, value []byte) error {
	if k.putErr != nil {
		return k.putErr
	}
	return k.KV.Put(ctx, key, value)
}

func (k *errKV) Delete(ctx context.Context, key []byte) error {
	if k.delErr != nil {
		return k.delErr
	}
	return k.KV.Delete(ctx, key)
}

func (k *errKV) BatchPut(ctx context.Context, kvs map[string][]byte) error {
	if k.batchErr != nil {
		return k.batchErr
	}
	return k.KV.BatchPut(ctx, kvs)
}

// newClusterWithKV 用给定 KV 构造集群客户端（不注册实例）；Close 由 Cleanup 兜底。
func newClusterWithKV(t *testing.T, kv cluster.KV, tweak func(*ClusterConfig)) *Storage {
	t.Helper()
	cfg := ClusterConfig{KV: kv, ClientName: "test-client"}
	if tweak != nil {
		tweak(&cfg)
	}
	s, err := NewCluster(cfg)
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// registerInstances 注册在线实例（StartTime/LastHeartbeat=now）并刷新发现快照。
func registerInstances(t *testing.T, s *Storage, insts ...*cluster.InstanceInfo) {
	t.Helper()
	now := time.Now().Unix()
	for _, inst := range insts {
		if inst.StartTime == 0 {
			inst.StartTime = now
		}
		inst.LastHeartbeat = now
		if err := cluster.Register(context.Background(), s.cfg.KV, inst); err != nil {
			t.Fatalf("register %s: %v", inst.Name, err)
		}
	}
	s.registry.refresh()
}

// newClusterWithInst 注册若干实例后构造集群客户端（tweak 可调传输/回源等）。
func newClusterWithInst(t *testing.T, kv cluster.KV, tweak func(*ClusterConfig), insts ...*cluster.InstanceInfo) *Storage {
	t.Helper()
	s := newClusterWithKV(t, kv, tweak)
	registerInstances(t, s, insts...)
	return s
}

// newClusterForInst 注册单个实例并构造集群客户端。HeartbeatTimeout 放宽到 1h，
// 避免用例执行期间实例被判离线（离线判定由 TestBuildSnapshotGroupsAndFiltersOffline 覆盖）。
func newClusterForInst(t *testing.T, inst *cluster.InstanceInfo, tweak func(*ClusterConfig)) *Storage {
	t.Helper()
	return newClusterWithInst(t, cluster.NewMemoryKV(), func(c *ClusterConfig) {
		c.ClientName = "path-test"
		c.Conns = 1
		c.HeartbeatTimeout = time.Hour
		if tweak != nil {
			tweak(c)
		}
	}, inst)
}

// registryWith 注册实例并返回已刷新快照的实例发现器。
func registryWith(t *testing.T, local string, insts ...*cluster.InstanceInfo) *instanceRegistry {
	t.Helper()
	kv := cluster.NewMemoryKV()
	now := time.Now().Unix()
	for _, inst := range insts {
		inst.LastHeartbeat = now
		if err := cluster.Register(context.Background(), kv, inst); err != nil {
			t.Fatalf("register %s: %v", inst.Name, err)
		}
	}
	reg := newInstanceRegistry(kv, local, time.Hour, time.Hour)
	reg.refresh()
	return reg
}

// closedTCPAddr 返回一个刚关闭的本地 TCP 地址：拨号必被拒绝（连接失败路径）。
func closedTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// localHostname 返回本机 hostname（同机分组判定用）；不可用时跳过用例。
func localHostname(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	if err != nil || h == "" {
		t.Skipf("os.Hostname unavailable: %v", err)
	}
	return h
}
