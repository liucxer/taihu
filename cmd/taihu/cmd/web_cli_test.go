// web 子命令与 webService 的用例：装配检查、MemoryKV 上的 cluster/client 类路径、
// 真实 transport server 上的 key 读写删路径（复用 cli_core_test.go 脚手架）。
package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
)

// TestWebCmdRegistered：web 子命令挂到根命令，-listen 默认 0.0.0.0:18080。
func TestWebCmdRegistered(t *testing.T) {
	cliTestGlobal(t)
	if webCmd == nil {
		t.Fatal("webCmd 未定义")
	}
	found := false
	for _, c := range rootCmd.Commands() {
		if c == webCmd {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("web 子命令未注册到 rootCmd")
	}
	v, err := webCmd.Flags().GetString("listen")
	if err != nil || v != "0.0.0.0:18080" {
		t.Fatalf("listen 默认 = %q, err=%v", v, err)
	}
}

// TestWebServiceErrPD：无 KV 时 KV 类端点统一报 -pd required；Version/ClientInfo 可用。
func TestWebServiceErrPD(t *testing.T) {
	cliTestGlobal(t)
	ws := newWebService(nil)
	ctx := context.Background()
	kvEndpoints := map[string]func() error{
		"ClusterList":    func() error { _, err := ws.ClusterList(ctx); return err },
		"ClusterStatus":  func() error { _, err := ws.ClusterStatus(ctx); return err },
		"ClusterIndex":   func() error { _, err := ws.ClusterIndex(ctx, ""); return err },
		"ClusterPurge":   func() error { _, err := ws.ClusterPurge(ctx, false); return err },
		"ClientList":     func() error { _, err := ws.ClientList(ctx); return err },
		"InstanceSeg":    func() error { _, err := ws.InstanceSegments(ctx, "", false); return err },
		"KeyStatNoTgt":   func() error { _, err := ws.KeyStat(ctx, "k", "", ""); return err },
	}
	for name, fn := range kvEndpoints {
		if err := fn(); err == nil {
			t.Fatalf("%s 缺 -pd 应报错", name)
		}
	}
	if _, err := ws.Version(ctx); err != nil {
		t.Fatalf("Version 应可用: %v", err)
	}
	ci, err := ws.ClientInfo(ctx)
	if err != nil {
		t.Fatalf("ClientInfo 应可用: %v", err)
	}
	if ci.KVStatus == "" || !strings.Contains(ci.KVStatus, "unset") {
		t.Fatalf("KVStatus = %q", ci.KVStatus)
	}
}

// TestWebServiceClusterKV：MemoryKV 上 cluster list/status/index 与 purge 预览/确认删除。
func TestWebServiceClusterKV(t *testing.T) {
	cliTestGlobal(t)
	kv := cluster.NewMemoryKV()
	ws := newWebService(kv)
	ctx := context.Background()

	if err := cluster.Register(ctx, kv, &cluster.InstanceInfo{Name: "TAIHU-0", Node: "n11", Addr: "127.0.0.1:1"}); err != nil {
		t.Fatalf("register instance: %v", err)
	}
	if err := cluster.RegisterClient(ctx, kv, &cluster.ClientInfo{ID: "c1", Node: "n11", Pid: 1, SDKVersion: "v1"}); err != nil {
		t.Fatalf("register client: %v", err)
	}

	rows, err := ws.ClusterList(ctx)
	if err != nil {
		t.Fatalf("ClusterList: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "TAIHU-0" {
		t.Fatalf("ClusterList = %+v", rows)
	}

	// 未心跳实例 → status 行带 error。
	st, err := ws.ClusterStatus(ctx)
	if err != nil {
		t.Fatalf("ClusterStatus: %v", err)
	}
	if len(st) != 1 || st[0].Error == "" {
		t.Fatalf("离线实例 status 应带 error: %+v", st)
	}

	// 无索引条目 → total 0。
	idx, err := ws.ClusterIndex(ctx, "")
	if err != nil || idx.Total != 0 {
		t.Fatalf("ClusterIndex = %+v, err=%v", idx, err)
	}

	// purge 预览：只统计不清除。
	pre, err := ws.ClusterPurge(ctx, false)
	if err != nil {
		t.Fatalf("purge 预览: %v", err)
	}
	if pre.Confirmed || pre.Deleted != 0 || pre.Total != 2 {
		t.Fatalf("预览 = %+v", pre)
	}
	if len(mustWebList(t, ws, ctx)) != 1 {
		t.Fatal("预览不应删除注册记录")
	}

	// purge 确认：清除注册区。
	got, err := ws.ClusterPurge(ctx, true)
	if err != nil {
		t.Fatalf("purge 确认: %v", err)
	}
	if !got.Confirmed || got.Deleted != 2 {
		t.Fatalf("确认 = %+v", got)
	}
	if len(mustWebList(t, ws, ctx)) != 0 {
		t.Fatal("purge 后注册区应清空")
	}
}

func mustWebList(t *testing.T, ws *webService, ctx context.Context) []interface{} {
	t.Helper()
	rows, err := ws.ClusterList(ctx)
	if err != nil {
		t.Fatalf("ClusterList: %v", err)
	}
	out := make([]interface{}, len(rows))
	for i := range rows {
		out[i] = rows[i]
	}
	return out
}

// TestWebServiceClientList：MemoryKV 上 client list 排序与统计。
func TestWebServiceClientList(t *testing.T) {
	cliTestGlobal(t)
	kv := cluster.NewMemoryKV()
	ws := newWebService(kv)
	ctx := context.Background()

	for _, c := range []*cluster.ClientInfo{
		{ID: "b", Node: "n11"},
		{ID: "a", Node: "n10"},
	} {
		if err := cluster.RegisterClient(ctx, kv, c); err != nil {
			t.Fatalf("register client: %v", err)
		}
	}
	res, err := ws.ClientList(ctx)
	if err != nil {
		t.Fatalf("ClientList: %v", err)
	}
	if res.Total != 2 || len(res.Rows) != 2 {
		t.Fatalf("res = %+v", res)
	}
	if res.Rows[0].Node != "n10" || res.Rows[1].Node != "n11" {
		t.Fatalf("按 Node 排序失败: %+v", res.Rows)
	}
	// 未心跳 → stale。
	if res.Rows[0].Status != "stale" {
		t.Fatalf("status = %q", res.Rows[0].Status)
	}
}

// TestWebServiceKeyRoundTrip：真实 storage + transport server 上 put/stat/get/meta/list/delete。
func TestWebServiceKeyRoundTrip(t *testing.T) {
	cliTestGlobal(t)
	addr := cliTestServer(t)
	ws := newWebService(cluster.NewMemoryKV())
	ctx := context.Background()

	put, err := ws.KeyPut(ctx, "hello", []byte("hello-taihu"), 5, addr, "")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if put.Key != "hello" || put.Size != 5 {
		t.Fatalf("put = %+v", put)
	}

	st, err := ws.KeyStat(ctx, "hello", addr, "")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size != 5 {
		t.Fatalf("stat size = %d", st.Size)
	}

	data, rel, err := ws.KeyGet(ctx, "hello", 0, -1, addr, "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("get data = %q", data)
	}
	rel()

	meta, err := ws.KeyMeta(ctx, "hello", addr, "")
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if meta.Size != 5 {
		t.Fatalf("meta = %+v", meta)
	}

	kl, err := ws.KeyList(ctx, "h", 0, addr, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if kl.Total != 1 || len(kl.Keys) != 1 || kl.Keys[0] != "hello" {
		t.Fatalf("list = %+v", kl)
	}

	del, err := ws.KeyDelete(ctx, "hello", addr, "")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if del.Key != "hello" {
		t.Fatalf("del = %+v", del)
	}
	if _, err := ws.KeyStat(ctx, "hello", addr, ""); err == nil {
		t.Fatal("删除后 stat 应报错")
	}
}

// TestWebServiceKeyNoTarget：三寻址全空且无 KV 时报目标缺失。
func TestWebServiceKeyNoTarget(t *testing.T) {
	cliTestGlobal(t)
	ws := newWebService(nil)
	ctx := context.Background()
	if _, _, _, err := ws.prepareStore(ctx, "", ""); err == nil {
		t.Fatal("无目标应报错")
	}
	if _, err := ws.KeyStat(ctx, "k", "", ""); err == nil {
		t.Fatal("无目标应报错")
	}
}

// TestWebServiceKeyViaInstance：-instance 寻址（注册区）+ 真实 server。
func TestWebServiceKeyViaInstance(t *testing.T) {
	cliTestGlobal(t)
	kv := cluster.NewMemoryKV()
	addr := cliTestServer(t)
	ws := newWebService(kv)
	ctx := context.Background()

	if err := cluster.Register(ctx, kv, &cluster.InstanceInfo{Name: "TAIHU-0", Node: "n11", Addr: addr}); err != nil {
		t.Fatalf("register: %v", err)
	}
	put, err := ws.KeyPut(ctx, "x", []byte("xyz"), 3, "", "TAIHU-0")
	if err != nil {
		t.Fatalf("put via instance: %v", err)
	}
	if put.Instance != "TAIHU-0" {
		t.Fatalf("instance = %q", put.Instance)
	}
	st, err := ws.KeyStat(ctx, "x", "", "TAIHU-0")
	if err != nil {
		t.Fatalf("stat via instance: %v", err)
	}
	if st.Size != 3 {
		t.Fatalf("stat size = %d", st.Size)
	}
	if _, err := ws.KeyDelete(ctx, "x", "", "TAIHU-0"); err != nil {
		t.Fatalf("delete via instance: %v", err)
	}
}

// TestWebServiceKeyNotFound：直连未找到 key 时报错（Stat 路径）。
func TestWebServiceKeyNotFound(t *testing.T) {
	cliTestGlobal(t)
	addr := cliTestServer(t)
	ws := newWebService(cluster.NewMemoryKV())
	ctx := context.Background()
	start := time.Now()
	if _, err := ws.KeyStat(ctx, "no-such-key", addr, ""); err == nil {
		t.Fatal("未找到 key 应报错")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("未找到路径耗时异常")
	}
}
