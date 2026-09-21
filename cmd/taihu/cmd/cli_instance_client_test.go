// instance.go / client.go 的用例：instance segments 与 client list/info 的正常与错误路径。
package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/rpcclient"
)

func TestCLIStateName(t *testing.T) {
	cases := map[rpcclient.SegmentState]string{
		rpcclient.SegmentStateFree:       "free",
		rpcclient.SegmentStateActive:     "active",
		rpcclient.SegmentStateFull:       "full",
		rpcclient.SegmentStateReclaiming: "reclaiming",
	}
	for st, want := range cases {
		if got := stateName(st); got != want {
			t.Fatalf("stateName(%d) = %q, want %q", st, got, want)
		}
	}
	if got := stateName(rpcclient.SegmentState(99)); !strings.HasPrefix(got, "unknown(99") {
		t.Fatalf("stateName(99) = %q", got)
	}
}

func TestCLIInstanceSegmentsEmpty(t *testing.T) {
	cliTestEnv(t)
	out := cliTestCaptureStdout(t)
	if err := instanceSegmentsCmd.RunE(instanceSegmentsCmd, nil); err != nil {
		t.Fatalf("instance segments(空) = %v", err)
	}
	if got := out(); !strings.Contains(got, "no instances registered") {
		t.Fatalf("空注册区输出 = %q", got)
	}
}

func TestCLIInstanceSegments(t *testing.T) {
	kv := cliTestEnv(t)
	addr := cliTestServer(t)
	out := cliTestCaptureStdout(t)

	inst := cliTestRegister(t, kv, cluster.InstanceInfo{
		Name: "TAIHU-0", Node: "n0", Hostname: "h0", Addr: addr, ShmAddr: "/tmp/taihu-cli.sock",
		Capacity: 1 << 30, Used: 1 << 20, Available: 1<<30 - 1<<20,
	}, 0)
	// 数据面不可达的实例（探测报错）。
	cliTestRegister(t, kv, cluster.InstanceInfo{Name: "TAIHU-1", Node: "n1", Hostname: "h1", Addr: "127.0.0.1:1"}, 0)
	// 心跳超时实例。
	cliTestRegister(t, kv, cluster.InstanceInfo{Name: "TAIHU-2", Node: "n2", Hostname: "h2", Addr: addr}, time.Minute)

	// 先写一个对象，让段汇总有非空内容（detail 分支才能打印明细）。
	cliTestStdin(t, "instance-segments")
	cliTestKeyFlags(t, keyPutCmd, addr, "", "key", "seg-key", "file", "", "size", "-1")
	if err := keyPutCmd.RunE(keyPutCmd, nil); err != nil {
		t.Fatalf("put = %v", err)
	}

	cliTestFlags(t, instanceSegmentsCmd, "instance", "", "detail", "false")
	if err := instanceSegmentsCmd.RunE(instanceSegmentsCmd, nil); err != nil {
		t.Fatalf("instance segments = %v", err)
	}
	got := out()
	if !strings.Contains(got, "TAIHU-0") || !strings.Contains(got, "stale (no heartbeat)") {
		t.Fatalf("instance segments 输出 = %q", got)
	}

	// -detail：打印段明细。
	cliTestFlags(t, instanceSegmentsCmd, "instance", "", "detail", "true")
	if err := instanceSegmentsCmd.RunE(instanceSegmentsCmd, nil); err != nil {
		t.Fatalf("instance segments -detail = %v", err)
	}
	if got := out(); !strings.Contains(got, "segID") {
		t.Fatalf("-detail 输出 = %q", got)
	}

	// 指定实例。
	cliTestFlags(t, instanceSegmentsCmd, "instance", "TAIHU-0", "detail", "false")
	if err := instanceSegmentsCmd.RunE(instanceSegmentsCmd, nil); err != nil {
		t.Fatalf("instance segments -instance = %v", err)
	}
	cliTestFlags(t, instanceSegmentsCmd, "instance", inst.Name, "detail", "false")
	if err := instanceSegmentsCmd.RunE(instanceSegmentsCmd, nil); err != nil {
		t.Fatalf("instance segments -instance 重复 = %v", err)
	}

	// 指定实例不存在。
	cliTestFlags(t, instanceSegmentsCmd, "instance", "NOPE", "detail", "false")
	if err := instanceSegmentsCmd.RunE(instanceSegmentsCmd, nil); err == nil {
		t.Fatal("未注册实例名应报错")
	}

	// JSON 输出。
	global.json = true
	cliTestFlags(t, instanceSegmentsCmd, "instance", "", "detail", "true")
	if err := instanceSegmentsCmd.RunE(instanceSegmentsCmd, nil); err != nil {
		t.Fatalf("instance segments --json = %v", err)
	}
	if got := out(); !strings.Contains(got, "\"name\": \"TAIHU-0\"") {
		t.Fatalf("--json 输出 = %q", got)
	}
	global.json = false

	// KV 错误。
	cliTestUseKVErr(t, errors.New("kv down"))
	if err := instanceSegmentsCmd.RunE(instanceSegmentsCmd, nil); err == nil {
		t.Fatal("KV 连接失败应报错")
	}
	cliTestUseKV(t, cliTestErrKV{err: errors.New("scan down")})
	if err := instanceSegmentsCmd.RunE(instanceSegmentsCmd, nil); err == nil {
		t.Fatal("实例列举失败应报错")
	}
}

func TestCLIClientList(t *testing.T) {
	kv := cliTestEnv(t)
	global.pd = "127.0.0.1:2379"
	out := cliTestCaptureStdout(t)
	ctx := context.Background()

	// 空注册区。
	if err := clientListCmd.RunE(clientListCmd, nil); err != nil {
		t.Fatalf("client list(空) = %v", err)
	}
	if got := out(); !strings.Contains(got, "no clients registered") {
		t.Fatalf("空注册区输出 = %q", got)
	}

	now := time.Now()
	for _, c := range []cluster.ClientInfo{
		{ID: "cli-1", Node: "n1", Host: "h1", Pid: 10, SDKVersion: "v1", Addr: "127.0.0.1:1",
			Extra: map[string]string{"k": "v"}, StartTime: now.Unix(), LastHeartbeat: now.Unix()},
		{ID: "cli-2", Node: "n0", Host: "h0", Pid: 11, SDKVersion: "v1",
			StartTime: now.Add(-time.Hour).Unix(), LastHeartbeat: now.Add(-time.Minute).Unix()},
	} {
		cc := c
		if err := cluster.RegisterClient(ctx, kv, &cc); err != nil {
			t.Fatalf("register client: %v", err)
		}
	}

	if err := clientListCmd.RunE(clientListCmd, nil); err != nil {
		t.Fatalf("client list = %v", err)
	}
	if got := out(); !strings.Contains(got, "2 registered, 1 online, 1 stale") {
		t.Fatalf("client list 输出 = %q", got)
	}

	global.json = true
	if err := clientListCmd.RunE(clientListCmd, nil); err != nil {
		t.Fatalf("client list --json = %v", err)
	}
	if got := out(); !strings.Contains(got, "\"id\": \"cli-1\"") {
		t.Fatalf("--json 输出 = %q", got)
	}
	global.json = false

	cliTestUseKVErr(t, errors.New("kv down"))
	if err := clientListCmd.RunE(clientListCmd, nil); err == nil {
		t.Fatal("KV 连接失败应报错")
	}
	cliTestUseKV(t, cliTestErrKV{err: errors.New("scan down")})
	if err := clientListCmd.RunE(clientListCmd, nil); err == nil {
		t.Fatal("客户端列举失败应报错")
	}
}

func TestCLIClientInfo(t *testing.T) {
	kv := cliTestEnv(t)
	out := cliTestCaptureStdout(t)
	ctx := context.Background()

	// -pd 缺省：无集群视角。
	if err := clientInfoCmd.RunE(clientInfoCmd, nil); err != nil {
		t.Fatalf("client info(无 -pd) = %v", err)
	}
	if got := out(); !strings.Contains(got, "unset (-pd 缺省：无集群视角)") {
		t.Fatalf("无 -pd 输出 = %q", got)
	}

	// 带集群视角：实例注册 + 索引 + SDK 客户端。
	global.pd = "127.0.0.1:2379"
	global.clientName = "n0"
	addr := cliTestServer(t)
	cliTestRegister(t, kv, cluster.InstanceInfo{
		Name: "TAIHU-0", Node: "n0", Hostname: "h0", Addr: addr, ShmAddr: "/tmp/taihu-cli.sock",
		Capacity: 1 << 30, Used: 1 << 20, Available: 1<<30 - 1<<20,
	}, 0)
	cliTestRegister(t, kv, cluster.InstanceInfo{
		Name: "TAIHU-1", Node: "n1", Hostname: "h1", Addr: "127.0.0.1:1",
	}, 0)
	if err := kv.Put(ctx, cluster.IndexKey("k1"), []byte("TAIHU-0")); err != nil {
		t.Fatalf("put index: %v", err)
	}
	if err := cluster.RegisterClient(ctx, kv, &cluster.ClientInfo{
		ID: "sdk-1", Node: "n1", StartTime: time.Now().Unix(), LastHeartbeat: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("register client: %v", err)
	}

	if err := clientInfoCmd.RunE(clientInfoCmd, nil); err != nil {
		t.Fatalf("client info = %v", err)
	}
	got := out()
	if !strings.Contains(got, "instances: 2, clients: 1, index entries: 1") {
		t.Fatalf("client info 计数输出 = %q", got)
	}
	if !strings.Contains(got, "(LOCAL)") || !strings.Contains(got, "err:") {
		t.Fatalf("client info 连通性输出 = %q", got)
	}

	global.json = true
	if err := clientInfoCmd.RunE(clientInfoCmd, nil); err != nil {
		t.Fatalf("client info --json = %v", err)
	}
	if got := out(); !strings.Contains(got, "\"kv_status\": \"tikv txnkv ok\"") {
		t.Fatalf("--json 输出 = %q", got)
	}
	global.json = false

	// KV 连接失败：状态置为 error 而非中断命令。
	cliTestUseKVErr(t, errors.New("kv down"))
	if err := clientInfoCmd.RunE(clientInfoCmd, nil); err != nil {
		t.Fatalf("KV 失败时 client info 不应报错，实际 %v", err)
	}
	if got := out(); !strings.Contains(got, "error: kv down") {
		t.Fatalf("KV 失败输出 = %q", got)
	}
	// 统计项扫描失败：计数缺省（不打印计数括注）。
	cliTestUseKV(t, cliTestErrKV{err: errors.New("scan down")})
	if err := clientInfoCmd.RunE(clientInfoCmd, nil); err != nil {
		t.Fatalf("KV 扫描失败时 client info 不应报错，实际 %v", err)
	}
	if got := out(); !strings.Contains(got, "kv backend: tikv txnkv ok") {
		t.Fatalf("KV 扫描失败输出 = %q", got)
	}
}
