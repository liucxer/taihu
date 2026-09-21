package taihuclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
)

// tikv.go 覆盖：PDAddrs 校验、TiKV 连接失败透出、连接成功后构建 Storage。
// 通过 newTiKVKV 测试缝隙注入假实现，测试不连真实 TiKV/PD。

func TestNewFromTiKVRequiresPDAddrs(t *testing.T) {
	if _, err := NewFromTiKV(context.Background(), TiKVOptions{}); err == nil {
		t.Fatal("empty PDAddrs must fail fast")
	}
}

func TestNewFromTiKVConnectError(t *testing.T) {
	orig := newTiKVKV
	t.Cleanup(func() { newTiKVKV = orig })

	wantErr := errors.New("tikv dial boom")
	newTiKVKV = func(context.Context, []string, cluster.TLSConfig) (cluster.KV, error) {
		return nil, wantErr
	}
	if _, err := NewFromTiKV(context.Background(), TiKVOptions{PDAddrs: []string{"127.0.0.1:2379"}}); !errors.Is(err, wantErr) {
		t.Fatalf("err=%v want %v", err, wantErr)
	}
}

func TestNewFromTiKVBuildsCluster(t *testing.T) {
	orig := newTiKVKV
	t.Cleanup(func() { newTiKVKV = orig })

	var gotAddrs []string
	var gotTLS cluster.TLSConfig
	newTiKVKV = func(_ context.Context, addrs []string, tls cluster.TLSConfig) (cluster.KV, error) {
		gotAddrs, gotTLS = addrs, tls
		return cluster.NewMemoryKV(), nil
	}

	st, err := NewFromTiKV(context.Background(), TiKVOptions{
		PDAddrs:          []string{"127.0.0.1:2379"},
		CA:               "ca.pem",
		Cert:             "cert.pem",
		Key:              "key.pem",
		RefreshInterval:  time.Millisecond,
		HeartbeatTimeout: time.Hour,
		UsageThreshold:   90,
		WriteRouting:     RouteRoundRobin,
		Conns:            2,
		Transport:        TransportRPC,
		ClientName:       "tikv-sdk",
		ClientID:         "tikv-sdk-01",
		ClientAddr:       "127.0.0.1:9999",
		ClientLabels:     map[string]string{"app": "cache"},
	})
	if err != nil {
		t.Fatalf("NewFromTiKV: %v", err)
	}
	if len(gotAddrs) != 1 || gotAddrs[0] != "127.0.0.1:2379" {
		t.Fatalf("addrs=%v", gotAddrs)
	}
	if gotTLS.CA != "ca.pem" || gotTLS.Cert != "cert.pem" || gotTLS.Key != "key.pem" {
		t.Fatalf("tls=%+v", gotTLS)
	}
	if st.cfg.ClientName != "tikv-sdk" || st.cfg.Conns != 2 || st.cfg.Transport != TransportRPC {
		t.Fatalf("cfg not propagated: %+v", st.cfg)
	}
	if st.picker.routing != RouteRoundRobin {
		t.Fatalf("routing=%q", st.picker.routing)
	}
	// ClientID 非空 → Close 同时走客户端注销路径。
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
