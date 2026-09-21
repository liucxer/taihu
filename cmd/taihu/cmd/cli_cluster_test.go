// cluster.go 的用例：cluster list/status/index/purge 的正常与错误路径。
package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/cluster"
)

// TestCLIClusterRequirePD 集群类命令缺 -pd 时必须在连 KV 之前报错。
func TestCLIClusterRequirePD(t *testing.T) {
	cliTestGlobal(t)
	cases := map[string]*cobra.Command{
		"cluster list":   clusterListCmd,
		"cluster status": clusterStatusCmd,
		"cluster index":  clusterIndexCmd,
		"cluster purge":  clusterPurgeCmd,
		"client list":    clientListCmd,
	}
	for name, c := range cases {
		if err := c.RunE(c, nil); err == nil {
			t.Fatalf("%s: -pd 缺省应报错", name)
		}
	}
}

func TestCLIClusterList(t *testing.T) {
	kv := cliTestEnv(t)
	global.pd = "127.0.0.1:2379"
	out := cliTestCaptureStdout(t)

	// 空注册区。
	if err := clusterListCmd.RunE(clusterListCmd, nil); err != nil {
		t.Fatalf("cluster list(空) = %v", err)
	}
	if got := out(); !strings.Contains(got, "no instances registered") {
		t.Fatalf("空注册区输出 = %q", got)
	}

	// 在线 + 心跳超时（stale）+ 无 addr。
	addr := cliTestServer(t)
	cliTestRegister(t, kv, cluster.InstanceInfo{
		Name: "TAIHU-0", Node: "node-a", Hostname: "h0", Addr: addr, ShmAddr: "/tmp/taihu-cli.sock",
		Capacity: 1 << 30, Used: 1 << 20, Available: 1<<30 - 1<<20,
	}, 0)
	cliTestRegister(t, kv, cluster.InstanceInfo{
		Name: "TAIHU-1", Node: "node-b", Hostname: "h1", Addr: addr,
	}, time.Minute)

	if err := clusterListCmd.RunE(clusterListCmd, nil); err != nil {
		t.Fatalf("cluster list = %v", err)
	}
	got := out()
	if !strings.Contains(got, "TAIHU-0") || !strings.Contains(got, "online") ||
		!strings.Contains(got, "TAIHU-1") || !strings.Contains(got, "stale") {
		t.Fatalf("cluster list 输出 = %q", got)
	}

	// JSON 输出。
	global.json = true
	if err := clusterListCmd.RunE(clusterListCmd, nil); err != nil {
		t.Fatalf("cluster list --json = %v", err)
	}
	if got := out(); !strings.Contains(got, "\"name\": \"TAIHU-0\"") {
		t.Fatalf("cluster list --json 输出 = %q", got)
	}
	global.json = false

	// KV 连接失败 / 扫描失败。
	cliTestUseKVErr(t, errors.New("kv down"))
	if err := clusterListCmd.RunE(clusterListCmd, nil); err == nil {
		t.Fatal("KV 连接失败应报错")
	}
	cliTestUseKV(t, cliTestErrKV{err: errors.New("scan down")})
	if err := clusterListCmd.RunE(clusterListCmd, nil); err == nil {
		t.Fatal("KV 扫描失败应报错")
	}
}

func TestCLIClusterStatus(t *testing.T) {
	kv := cliTestEnv(t)
	global.pd = "127.0.0.1:2379"
	out := cliTestCaptureStdout(t)

	// 空注册区。
	if err := clusterStatusCmd.RunE(clusterStatusCmd, nil); err != nil {
		t.Fatalf("cluster status(空) = %v", err)
	}
	if got := out(); !strings.Contains(got, "no instances registered") {
		t.Fatalf("空注册区输出 = %q", got)
	}

	addr := cliTestServer(t)
	// 在线（shm 地址存在 + 可 Ping + 可拉段汇总）。
	cliTestRegister(t, kv, cluster.InstanceInfo{
		Name: "TAIHU-0", Node: "node-a", Hostname: "h0", Addr: addr, ShmAddr: "/tmp/taihu-cli.sock",
	}, 0)
	// 在线但数据面不可达（Ping 失败）。
	cliTestRegister(t, kv, cluster.InstanceInfo{
		Name: "TAIHU-1", Node: "node-b", Hostname: "h1", Addr: "127.0.0.1:1",
	}, 0)
	// 心跳超时（stale，不探测）。
	cliTestRegister(t, kv, cluster.InstanceInfo{
		Name: "TAIHU-2", Node: "node-c", Hostname: "h2", Addr: addr,
	}, time.Minute)

	if err := clusterStatusCmd.RunE(clusterStatusCmd, nil); err != nil {
		t.Fatalf("cluster status = %v", err)
	}
	got := out()
	if !strings.Contains(got, "TAIHU-0") || !strings.Contains(got, "stale (no heartbeat)") {
		t.Fatalf("cluster status 输出 = %q", got)
	}

	// 同机标注（-client-name 与实例 Node 一致 → shm 标注 open）。
	global.clientName = "node-a"
	if err := clusterStatusCmd.RunE(clusterStatusCmd, nil); err != nil {
		t.Fatalf("cluster status(local) = %v", err)
	}

	// JSON 输出。
	global.json = true
	if err := clusterStatusCmd.RunE(clusterStatusCmd, nil); err != nil {
		t.Fatalf("cluster status --json = %v", err)
	}
	if got := out(); !strings.Contains(got, "\"name\": \"TAIHU-0\"") {
		t.Fatalf("cluster status --json 输出 = %q", got)
	}
	global.json = false
}

func TestCLIClusterIndex(t *testing.T) {
	kv := cliTestEnv(t)
	global.pd = "127.0.0.1:2379"
	out := cliTestCaptureStdout(t)
	ctx := context.Background()

	put := func(key, inst string) {
		t.Helper()
		if err := kv.Put(ctx, cluster.IndexKey(key), []byte(inst)); err != nil {
			t.Fatalf("put index %s: %v", key, err)
		}
	}
	put("bench/1", "TAIHU-0")
	put("bench/2", "TAIHU-0")
	put("other/1", "TAIHU-1")
	put("ab", "TAIHU-1") // 短 key：覆盖 len(key) < len(prefix) 分支

	// 全量统计。
	if err := clusterIndexCmd.RunE(clusterIndexCmd, nil); err != nil {
		t.Fatalf("cluster index = %v", err)
	}
	if got := out(); !strings.Contains(got, "index entries: 4") || !strings.Contains(got, "-> TAIHU-0: 2") {
		t.Fatalf("cluster index 输出 = %q", got)
	}

	// 前缀过滤（命中）。
	cliTestFlags(t, clusterIndexCmd, "prefix", "bench")
	if err := clusterIndexCmd.RunE(clusterIndexCmd, nil); err != nil {
		t.Fatalf("cluster index -prefix bench = %v", err)
	}
	if got := out(); !strings.Contains(got, `index entries matching "bench": 2`) {
		t.Fatalf("带前缀输出 = %q", got)
	}

	// 前缀过滤（无命中 + 短 key）。
	cliTestFlags(t, clusterIndexCmd, "prefix", "abcdef")
	if err := clusterIndexCmd.RunE(clusterIndexCmd, nil); err != nil {
		t.Fatalf("cluster index -prefix abcdef = %v", err)
	}
	if got := out(); !strings.Contains(got, "index entries matching \"abcdef\": 0") {
		t.Fatalf("无命中输出 = %q", got)
	}

	// JSON 输出。
	global.json = true
	cliTestFlags(t, clusterIndexCmd, "prefix", "")
	if err := clusterIndexCmd.RunE(clusterIndexCmd, nil); err != nil {
		t.Fatalf("cluster index --json = %v", err)
	}
	if got := out(); !strings.Contains(got, "\"TAIHU-0\": 2") {
		t.Fatalf("cluster index --json 输出 = %q", got)
	}
	global.json = false

	// 扫描失败。
	cliTestUseKV(t, cliTestErrKV{err: errors.New("scan down")})
	if err := clusterIndexCmd.RunE(clusterIndexCmd, nil); err == nil {
		t.Fatal("索引扫描失败应报错")
	}
}

func TestCLIClusterPurge(t *testing.T) {
	kv := cliTestEnv(t)
	global.pd = "127.0.0.1:2379"
	out := cliTestCaptureStdout(t)
	ctx := context.Background()
	cliTestFlags(t, clusterPurgeCmd, "confirm", "false")

	// 空元数据：直接提示无需清洗。
	if err := clusterPurgeCmd.RunE(clusterPurgeCmd, nil); err != nil {
		t.Fatalf("cluster purge(空) = %v", err)
	}
	if got := out(); !strings.Contains(got, "无元数据，无需清洗") {
		t.Fatalf("空元数据输出 = %q", got)
	}

	// 三类元数据各 1 条：不带 --confirm 只预览并报错。
	if err := cluster.Register(ctx, kv, &cluster.InstanceInfo{Name: "TAIHU-0"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := cluster.RegisterClient(ctx, kv, &cluster.ClientInfo{ID: "c1"}); err != nil {
		t.Fatalf("register client: %v", err)
	}
	if err := kv.Put(ctx, cluster.IndexKey("k1"), []byte("TAIHU-0")); err != nil {
		t.Fatalf("put index: %v", err)
	}
	err := clusterPurgeCmd.RunE(clusterPurgeCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("预览态应提示 --confirm，实际 %v", err)
	}
	if got := out(); !strings.Contains(got, "total: 3") {
		t.Fatalf("预览输出 = %q", got)
	}

	// --confirm：真正清空 /taihu/ 命名空间。
	cliTestFlags(t, clusterPurgeCmd, "confirm", "true")
	if err := clusterPurgeCmd.RunE(clusterPurgeCmd, nil); err != nil {
		t.Fatalf("cluster purge --confirm = %v", err)
	}
	if got := out(); !strings.Contains(got, "已清空 /taihu/ 命名空间 3 条元数据") {
		t.Fatalf("清空输出 = %q", got)
	}
	keys, values, err := kv.Scan(ctx, []byte(cluster.UserDataPrefix), []byte(cluster.UserDataPrefix+"\xff"), 0)
	if err != nil {
		t.Fatalf("scan after purge: %v", err)
	}
	if len(keys) != 0 || len(values) != 0 {
		t.Fatalf("清空后仍有 %d 条元数据", len(keys))
	}
}

// TestCLIClusterPurgeScanCap 覆盖「已达 Scan 上限」提示分支（10000 条样本）。
func TestCLIClusterPurgeScanCap(t *testing.T) {
	kv := cluster.NewMemoryKV()
	cliTestEnv(t)
	cliTestUseKV(t, kv)
	global.pd = "127.0.0.1:2379"
	cliTestFlags(t, clusterPurgeCmd, "confirm", "false")
	out := cliTestCaptureStdout(t)

	const n = 10000
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if err := kv.Put(ctx, cluster.InstanceKey(fmt.Sprintf("TAIHU-%d", i)), []byte("{}")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	err := clusterPurgeCmd.RunE(clusterPurgeCmd, nil)
	if err == nil {
		t.Fatal("预览态应报错")
	}
	if got := out(); !strings.Contains(got, "已达 Scan 上限") {
		t.Fatalf("上限提示缺失：%q", got)
	}
}

// TestCLIClusterPurgeScanErr 覆盖预览期 Scan 报错（走 stderr 提示后继续统计）。
func TestCLIClusterPurgeScanErr(t *testing.T) {
	cliTestEnv(t)
	cliTestUseKV(t, cliTestErrKV{err: errors.New("scan down")})
	global.pd = "127.0.0.1:2379"
	cliTestFlags(t, clusterPurgeCmd, "confirm", "true")
	if err := clusterPurgeCmd.RunE(clusterPurgeCmd, nil); err != nil {
		t.Fatalf("Scan 失败应降级为无元数据而非报错，实际 %v", err)
	}
}

func TestCLIMustBool(t *testing.T) {
	cliTestGlobal(t)
	cliTestFlags(t, clusterPurgeCmd, "confirm", "true")
	if !mustBool(clusterPurgeCmd, "confirm") {
		t.Fatal("mustBool(confirm=true) 应为 true")
	}
	if mustBool(clusterPurgeCmd, "no-such-flag") {
		t.Fatal("不存在的标志应为 false")
	}
}
