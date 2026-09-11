package rpccluster

import (
	"context"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
)

// SDK 客户端保活测试（taihu-cli 设计文档 §5）：注册 + 周期心跳 + Close 注销。

// awaitClient 轮询等待客户端出现且状态为 online（心跳已写入），最多 timeout。
func awaitClient(t *testing.T, kv cluster.KV, id string, timeout time.Duration) *cluster.ClientInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := cluster.GetClient(context.Background(), kv, id)
		if err != nil || info == nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if info.Aliveness(time.Now(), 0) {
			return info
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("client %q not online within %v", id, timeout)
	return nil
}

func TestClientKeepalive(t *testing.T) {
	kv := cluster.NewMemoryKV()
	defer kv.Close()

	s, err := NewCluster(ClusterConfig{
		KV:          kv,
		ClientName:  "test-node",
		ClientID:    "cache-svc-01",
		ClientLabels: map[string]string{"app": "cache", "env": "test"},
	})
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}

	// 等注册 + 首轮心跳落盘。
	info := awaitClient(t, kv, "cache-svc-01", 3*time.Second)
	if info.Node != "test-node" {
		t.Fatalf("Node=%q want test-node", info.Node)
	}
	if info.SDKVersion == "" {
		t.Fatalf("SDKVersion empty")
	}
	if info.Pid <= 0 {
		t.Fatalf("Pid=%d want > 0", info.Pid)
	}
	if info.Extra["app"] != "cache" {
		t.Fatalf("Extra=%v want app=cache", info.Extra)
	}
	if info.StartTime <= 0 || info.LastHeartbeat <= 0 {
		t.Fatalf("StartTime=%d LastHeartbeat=%d 需 >0", info.StartTime, info.LastHeartbeat)
	}

	// 心跳应随周期刷新（LastHeartbeat 前进）。
	h1 := info.LastHeartbeat
	time.Sleep(1200 * time.Millisecond)
	h2v, err := cluster.GetClient(context.Background(), kv, "cache-svc-01")
	if err != nil || h2v == nil {
		t.Fatalf("heartbeat lost: %v", err)
	}
	if h2v.LastHeartbeat <= h1 {
		t.Fatalf("LastHeartbeat 未刷新: %d -> %d", h1, h2v.LastHeartbeat)
	}

	// Close 应注销（无僵尸客户端）。
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gone, err := cluster.GetClient(context.Background(), kv, "cache-svc-01")
		if err != nil || gone == nil {
			return // 已注销
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("client %q 未在 Close 后注销", "cache-svc-01")
}

// 未配置 ClientID 时不得产生注册记录。
func TestClientKeepaliveDisabled(t *testing.T) {
	kv := cluster.NewMemoryKV()
	defer kv.Close()

	s, err := NewCluster(ClusterConfig{KV: kv, ClientName: "n"})
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	defer s.Close()
	time.Sleep(300 * time.Millisecond)
	clients, err := cluster.ListClients(context.Background(), kv)
	if err != nil {
		t.Fatalf("ListClients: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("unexpected clients registered: %v", clients)
	}
}