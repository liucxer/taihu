// key.go 的用例：resolveKeyTarget/prepareKeyStore/targetName 与 put/get/delete/stat/meta/list
// 的正常（-addr 直连、-instance 寻址、-pd 集群路由）与错误路径。
package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/cluster"
)

func TestCLITargetName(t *testing.T) {
	if got := targetName(nil); got != "cluster-routed" {
		t.Fatalf("targetName(nil) = %q", got)
	}
	if got := targetName(&keyTarget{}); got != "direct" {
		t.Fatalf("targetName(空 target) = %q", got)
	}
	inst := cluster.InstanceInfo{Name: "TAIHU-0"}
	if got := targetName(&keyTarget{inst: &inst}); got != "TAIHU-0" {
		t.Fatalf("targetName(实例) = %q", got)
	}
}

func TestCLIResolveKeyTarget(t *testing.T) {
	cliTestGlobal(t)
	ctx := context.Background()
	addr := cliTestServer(t)

	// -addr 直连。
	store, tgt, err := resolveKeyTarget(ctx, addr, "", nil, nil)
	if err != nil {
		t.Fatalf("resolveKeyTarget(-addr) = %v", err)
	}
	if tgt == nil || tgt.inst == nil || tgt.inst.Name != addr {
		t.Fatalf("target = %+v", tgt)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// -addr 拨号失败。
	if _, _, err := resolveKeyTarget(ctx, "127.0.0.1:1", "", nil, nil); err == nil {
		t.Fatal("不可达 -addr 应报错")
	}

	// -instance 但无 KV（-pd 缺省）。
	if _, _, err := resolveKeyTarget(ctx, "", "TAIHU-0", nil, nil); err == nil {
		t.Fatal("-instance 无 KV 应报错")
	}

	// -instance 未注册。
	kv := cluster.NewMemoryKV()
	if _, _, err := resolveKeyTarget(ctx, "", "TAIHU-0", kv, nil); err == nil {
		t.Fatal("未注册实例名应报错")
	}

	// -instance 命中（走注册区寻址）。
	inst := cliTestRegister(t, kv, cluster.InstanceInfo{Name: "TAIHU-0", Node: "n0", Hostname: "h0", Addr: addr}, 0)
	store, tgt, err = resolveKeyTarget(ctx, "", "TAIHU-0", kv, []cluster.InstanceInfo{inst})
	if err != nil {
		t.Fatalf("resolveKeyTarget(-instance) = %v", err)
	}
	if tgt == nil || tgt.inst == nil || tgt.inst.Name != "TAIHU-0" {
		t.Fatalf("target = %+v", tgt)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 集群路由模式（KV 为空指针时 NewCluster 报错）。
	if _, _, err := resolveKeyTarget(ctx, "", "", nil, nil); err == nil {
		t.Fatal("集群路由缺 KV 应报错")
	}
	// 集群路由（内存注册区，无在线实例）。
	store, tgt, err = resolveKeyTarget(ctx, "", "", cluster.NewMemoryKV(), nil)
	if err != nil {
		t.Fatalf("resolveKeyTarget(集群) = %v", err)
	}
	if tgt != nil {
		t.Fatalf("集群路由 target 应为 nil，实际 %+v", tgt)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestCLIPrepareKeyStore(t *testing.T) {
	cliTestEnv(t)
	ctx := context.Background()

	// 三类寻址全空。
	cliTestKeyFlags(t, keyPutCmd, "", "")
	if _, _, _, err := prepareKeyStore(ctx); err == nil {
		t.Fatal("无目标应报错")
	}

	// -pd 但 KV 连接失败。
	global.pd = "127.0.0.1:2379"
	cliTestUseKVErr(t, errors.New("kv down"))
	if _, _, _, err := prepareKeyStore(ctx); err == nil {
		t.Fatal("KV 连接失败应报错")
	}

	// -pd 但实例列举失败。
	cliTestUseKV(t, cliTestErrKV{err: errors.New("scan down")})
	if _, _, _, err := prepareKeyStore(ctx); err == nil {
		t.Fatal("实例列举失败应报错")
	}

	// -pd 集群路由成功（无在线实例也不报错，Put 时才失败）。
	cliTestUseKV(t, cluster.NewMemoryKV())
	store, tgt, cleanup, err := prepareKeyStore(ctx)
	if err != nil {
		t.Fatalf("prepareKeyStore: %v", err)
	}
	if tgt != nil {
		t.Fatalf("集群路由 target 应为 nil，实际 %+v", tgt)
	}
	cleanup()
	if err := store.Close(); err != nil {
		t.Fatalf("二次 Close 应幂等: %v", err)
	}
}

func TestCLIKeyRequired(t *testing.T) {
	cliTestEnv(t)
	addr := cliTestServer(t)
	cases := []struct {
		name string
		c    *cobra.Command
	}{
		{"put", keyPutCmd},
		{"get", keyGetCmd},
		{"delete", keyDeleteCmd},
		{"stat", keyStatCmd},
		{"meta", keyMetaCmd},
	}
	for _, tc := range cases {
		cliTestKeyFlags(t, tc.c, addr, "", "key", "")
		err := tc.c.RunE(tc.c, nil)
		if err == nil || !strings.Contains(err.Error(), "-key required") {
			t.Fatalf("%s 缺 -key: err = %v, want -key required", tc.name, err)
		}
	}
}

func TestCLIKeyDirectRoundTrip(t *testing.T) {
	cliTestEnv(t)
	addr := cliTestServer(t)
	dir := t.TempDir()
	out := cliTestCaptureStdout(t)

	inFile := filepath.Join(dir, "in.bin")
	if err := os.WriteFile(inFile, []byte("hello-taihu"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}

	// 缺 -key。
	cliTestKeyFlags(t, keyPutCmd, addr, "", "key", "", "file", "", "size", "-1")
	if err := keyPutCmd.RunE(keyPutCmd, nil); err == nil {
		t.Fatal("put 缺 -key 应报错")
	}

	// 从文件上传，逻辑大小截断为 5。
	cliTestKeyFlags(t, keyPutCmd, addr, "", "key", "hello", "file", inFile, "size", "5")
	if err := keyPutCmd.RunE(keyPutCmd, nil); err != nil {
		t.Fatalf("put -file = %v", err)
	}
	if got := out(); !strings.Contains(got, "put \"hello\" ok (5 bytes)") {
		t.Fatalf("put 输出 = %q", got)
	}

	// get 缺 -key。
	cliTestKeyFlags(t, keyGetCmd, addr, "", "key", "", "file", "", "off", "0", "size", "-1")
	if err := keyGetCmd.RunE(keyGetCmd, nil); err == nil {
		t.Fatal("get 缺 -key 应报错")
	}

	// -json 与 stdout 混用被拒。
	global.json = true
	cliTestKeyFlags(t, keyGetCmd, addr, "", "key", "hello", "file", "", "off", "0", "size", "-1")
	if err := keyGetCmd.RunE(keyGetCmd, nil); err == nil {
		t.Fatal("get -json 无 -file 应报错")
	}
	global.json = false

	// 全量读到文件。
	outFile := filepath.Join(dir, "out.bin")
	cliTestKeyFlags(t, keyGetCmd, addr, "", "key", "hello", "file", outFile, "off", "0", "size", "-1")
	if err := keyGetCmd.RunE(keyGetCmd, nil); err != nil {
		t.Fatalf("get -file = %v", err)
	}
	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, []byte("hello")) {
		t.Fatalf("get 数据 = %q, want %q", got, "hello")
	}

	// 区间读到 stdout（-json 关）。
	cliTestKeyFlags(t, keyGetCmd, addr, "", "key", "hello", "file", "", "off", "1", "size", "3")
	if err := keyGetCmd.RunE(keyGetCmd, nil); err != nil {
		t.Fatalf("get 区间 = %v", err)
	}
	if got := out(); !strings.HasSuffix(got, "ell") {
		t.Fatalf("区间读输出 = %q", got)
	}

	// 非法区间。
	cliTestKeyFlags(t, keyGetCmd, addr, "", "key", "hello", "file", "", "off", "100", "size", "-1")
	if err := keyGetCmd.RunE(keyGetCmd, nil); err == nil {
		t.Fatal("越界区间应报错")
	}

	// 未找到。
	cliTestKeyFlags(t, keyGetCmd, addr, "", "key", "no-such-key", "file", "", "off", "0", "size", "-1")
	if err := keyGetCmd.RunE(keyGetCmd, nil); err == nil {
		t.Fatal("未找到 key 应报错")
	}

	// stat（文本 + JSON）。
	cliTestKeyFlags(t, keyStatCmd, addr, "", "key", "hello")
	if err := keyStatCmd.RunE(keyStatCmd, nil); err != nil {
		t.Fatalf("stat = %v", err)
	}
	if got := out(); !strings.Contains(got, "size=5 (5 B)") {
		t.Fatalf("stat 输出 = %q", got)
	}
	global.json = true
	if err := keyStatCmd.RunE(keyStatCmd, nil); err != nil {
		t.Fatalf("stat --json = %v", err)
	}
	global.json = false

	// meta（文本 + JSON）。
	cliTestKeyFlags(t, keyMetaCmd, addr, "", "key", "hello")
	if err := keyMetaCmd.RunE(keyMetaCmd, nil); err != nil {
		t.Fatalf("meta = %v", err)
	}
	global.json = true
	if err := keyMetaCmd.RunE(keyMetaCmd, nil); err != nil {
		t.Fatalf("meta --json = %v", err)
	}
	global.json = false

	// list（全量 / limit / JSON）。
	cliTestKeyFlags(t, keyListCmd, addr, "", "prefix", "", "limit", "0")
	if err := keyListCmd.RunE(keyListCmd, nil); err != nil {
		t.Fatalf("list = %v", err)
	}
	if got := out(); !strings.Contains(got, "hello") {
		t.Fatalf("list 输出 = %q", got)
	}
	cliTestKeyFlags(t, keyListCmd, addr, "", "prefix", "zzz", "limit", "0")
	if err := keyListCmd.RunE(keyListCmd, nil); err != nil {
		t.Fatalf("list -prefix zzz = %v", err)
	}
	cliTestKeyFlags(t, keyListCmd, addr, "", "prefix", "", "limit", "1")
	if err := keyListCmd.RunE(keyListCmd, nil); err != nil {
		t.Fatalf("list -limit 1 = %v", err)
	}
	global.json = true
	if err := keyListCmd.RunE(keyListCmd, nil); err != nil {
		t.Fatalf("list --json = %v", err)
	}
	global.json = false

	// 从 stdin 上传。
	cliTestStdin(t, "from-stdin")
	cliTestKeyFlags(t, keyPutCmd, addr, "", "key", "stdin-key", "file", "", "size", "-1")
	if err := keyPutCmd.RunE(keyPutCmd, nil); err != nil {
		t.Fatalf("put stdin = %v", err)
	}

	// 输入文件不存在。
	cliTestKeyFlags(t, keyPutCmd, addr, "", "key", "x", "file", filepath.Join(dir, "nope"), "size", "-1")
	if err := keyPutCmd.RunE(keyPutCmd, nil); err == nil {
		t.Fatal("输入文件不存在应报错")
	}

	// delete（文本 + JSON）。
	cliTestKeyFlags(t, keyDeleteCmd, addr, "", "key", "hello")
	if err := keyDeleteCmd.RunE(keyDeleteCmd, nil); err != nil {
		t.Fatalf("delete = %v", err)
	}
	if got := out(); !strings.Contains(got, "delete \"hello\" ok") {
		t.Fatalf("delete 输出 = %q", got)
	}
	global.json = true
	if err := keyDeleteCmd.RunE(keyDeleteCmd, nil); err == nil {
		t.Fatal("重复 delete 应报错（对象已不存在）")
	}
	global.json = false

	// 缺 -key / 不可达 -addr 的公共错误路径。
	cliTestKeyFlags(t, keyDeleteCmd, addr, "", "key", "")
	if err := keyDeleteCmd.RunE(keyDeleteCmd, nil); err == nil {
		t.Fatal("delete 缺 -key 应报错")
	}
	cliTestKeyFlags(t, keyStatCmd, "", "", "key", "hello")
	if err := keyStatCmd.RunE(keyStatCmd, nil); err == nil {
		t.Fatal("stat 无目标应报错")
	}
	cliTestKeyFlags(t, keyMetaCmd, "127.0.0.1:1", "", "key", "hello")
	if err := keyMetaCmd.RunE(keyMetaCmd, nil); err == nil {
		t.Fatal("meta -addr 不可达应报错")
	}
	cliTestKeyFlags(t, keyListCmd, "", "", "prefix", "", "limit", "0")
	if err := keyListCmd.RunE(keyListCmd, nil); err == nil {
		t.Fatal("list 无目标应报错")
	}
}

func TestCLIKeyClusterRoute(t *testing.T) {
	kv := cliTestEnv(t)
	global.pd = "127.0.0.1:2379"
	addr := cliTestServer(t)
	out := cliTestCaptureStdout(t)
	cliTestRegister(t, kv, cluster.InstanceInfo{
		Name: "TAIHU-0", Node: "n0", Hostname: "cli-test-remote-host", Addr: addr,
		Capacity: 1 << 30, Used: 1 << 20, Available: 1<<30 - 1<<20,
	}, 0)

	// 无在线实例时 put 报 NoInstances。
	empty := cluster.NewMemoryKV()
	cliTestUseKV(t, empty)
	cliTestStdin(t, "x")
	cliTestKeyFlags(t, keyPutCmd, "", "", "key", "ck0", "file", "", "size", "-1")
	if err := keyPutCmd.RunE(keyPutCmd, nil); err == nil {
		t.Fatal("无在线实例应报错")
	}
	cliTestUseKV(t, kv)

	// 集群路由 put（走注册区选实例 + TCP 数据面）。
	cliTestStdin(t, "cluster-data")
	cliTestKeyFlags(t, keyPutCmd, "", "", "key", "ck1", "file", "", "size", "-1")
	if err := keyPutCmd.RunE(keyPutCmd, nil); err != nil {
		t.Fatalf("集群 put = %v", err)
	}
	if got := out(); !strings.Contains(got, "put \"ck1\" ok (12 bytes) -> cluster-routed") {
		t.Fatalf("集群 put 输出 = %q", got)
	}

	// JSON 形态。
	global.json = true
	cliTestStdin(t, "cluster-data-2")
	cliTestKeyFlags(t, keyPutCmd, "", "", "key", "ck2", "file", "", "size", "-1")
	if err := keyPutCmd.RunE(keyPutCmd, nil); err != nil {
		t.Fatalf("集群 put --json = %v", err)
	}
	global.json = false

	// 集群路由 get（按索引定位实例）。
	cliTestKeyFlags(t, keyGetCmd, "", "", "key", "ck1", "file", "", "off", "0", "size", "-1")
	if err := keyGetCmd.RunE(keyGetCmd, nil); err != nil {
		t.Fatalf("集群 get = %v", err)
	}
	if got := out(); !strings.Contains(got, "cluster-data") {
		t.Fatalf("集群 get 输出 = %q", got)
	}

	// 集群路由 stat。
	cliTestKeyFlags(t, keyStatCmd, "", "", "key", "ck1")
	if err := keyStatCmd.RunE(keyStatCmd, nil); err != nil {
		t.Fatalf("集群 stat = %v", err)
	}
	if got := out(); !strings.Contains(got, "size=12") {
		t.Fatalf("集群 stat 输出 = %q", got)
	}

	// meta/list 只支持直连：集群路由下报错。
	cliTestKeyFlags(t, keyMetaCmd, "", "", "key", "ck1")
	if err := keyMetaCmd.RunE(keyMetaCmd, nil); err == nil {
		t.Fatal("集群路由 meta 应报错")
	}
	cliTestKeyFlags(t, keyListCmd, "", "", "prefix", "", "limit", "0")
	if err := keyListCmd.RunE(keyListCmd, nil); err == nil {
		t.Fatal("集群路由 list 应报错")
	}

	// -instance 寻址（配合 -pd）。
	cliTestKeyFlags(t, keyStatCmd, "", "TAIHU-0", "key", "ck1")
	if err := keyStatCmd.RunE(keyStatCmd, nil); err != nil {
		t.Fatalf("instance stat = %v", err)
	}
	cliTestKeyFlags(t, keyMetaCmd, "", "TAIHU-0", "key", "ck1")
	if err := keyMetaCmd.RunE(keyMetaCmd, nil); err != nil {
		t.Fatalf("instance meta = %v", err)
	}
	cliTestKeyFlags(t, keyListCmd, "", "TAIHU-0", "prefix", "", "limit", "0")
	if err := keyListCmd.RunE(keyListCmd, nil); err != nil {
		t.Fatalf("instance list = %v", err)
	}
	cliTestKeyFlags(t, keyGetCmd, "", "TAIHU-0", "key", "ck1", "file", "", "off", "0", "size", "-1")
	if err := keyGetCmd.RunE(keyGetCmd, nil); err != nil {
		t.Fatalf("instance get = %v", err)
	}
	cliTestKeyFlags(t, keyDeleteCmd, "", "TAIHU-0", "key", "ck2")
	if err := keyDeleteCmd.RunE(keyDeleteCmd, nil); err != nil {
		t.Fatalf("instance delete = %v", err)
	}

	// -instance 未注册 / 无 -pd。
	cliTestKeyFlags(t, keyStatCmd, "", "NOPE", "key", "ck1")
	if err := keyStatCmd.RunE(keyStatCmd, nil); err == nil {
		t.Fatal("未注册实例名应报错")
	}
	global.pd = ""
	cliTestKeyFlags(t, keyStatCmd, "", "TAIHU-0", "key", "ck1")
	if err := keyStatCmd.RunE(keyStatCmd, nil); err == nil {
		t.Fatal("-instance 缺 -pd 应报错")
	}
	global.pd = "127.0.0.1:2379"

	// 集群路由 delete + 删除后 get 报错。
	cliTestKeyFlags(t, keyDeleteCmd, "", "", "key", "ck1")
	if err := keyDeleteCmd.RunE(keyDeleteCmd, nil); err != nil {
		t.Fatalf("集群 delete = %v", err)
	}
	cliTestKeyFlags(t, keyGetCmd, "", "", "key", "ck1", "file", "", "off", "0", "size", "-1")
	if err := keyGetCmd.RunE(keyGetCmd, nil); err == nil {
		t.Fatal("删除后 get 应报错")
	}
	cliTestKeyFlags(t, keyStatCmd, "", "", "key", "ck1")
	if err := keyStatCmd.RunE(keyStatCmd, nil); err == nil {
		t.Fatal("删除后 stat 应报错")
	}
	global.json = true
	cliTestKeyFlags(t, keyDeleteCmd, "", "", "key", "ck1")
	if err := keyDeleteCmd.RunE(keyDeleteCmd, nil); err != nil {
		t.Fatalf("集群 delete(索引已清) = %v", err)
	}
	global.json = false

	// get 写文件时输出 JSON 元信息。
	dir := t.TempDir()
	cliTestStdin(t, "json-file")
	cliTestKeyFlags(t, keyPutCmd, "", "", "key", "ck3", "file", "", "size", "-1")
	if err := keyPutCmd.RunE(keyPutCmd, nil); err != nil {
		t.Fatalf("集群 put ck3 = %v", err)
	}
	global.json = true
	cliTestKeyFlags(t, keyGetCmd, "", "", "key", "ck3", "file", filepath.Join(dir, "o.bin"), "off", "0", "size", "-1")
	if err := keyGetCmd.RunE(keyGetCmd, nil); err != nil {
		t.Fatalf("集群 get -file --json = %v", err)
	}
	global.json = false
}

func TestCLIKeyStdinReadError(t *testing.T) {
	cliTestEnv(t)
	addr := cliTestServer(t)
	// 关闭的 stdin：io.ReadAll 立即返回错误。
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	_ = r.Close()
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })

	cliTestKeyFlags(t, keyPutCmd, addr, "", "key", "k", "file", "", "size", "-1")
	if err := keyPutCmd.RunE(keyPutCmd, nil); err == nil {
		t.Fatal("stdin 读失败应报错")
	}
}

func TestCLIKeyGetWriteError(t *testing.T) {
	cliTestEnv(t)
	addr := cliTestServer(t)
	dir := t.TempDir()
	inFile := filepath.Join(dir, "in.bin")
	if err := os.WriteFile(inFile, []byte("data"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	cliTestKeyFlags(t, keyPutCmd, addr, "", "key", "k1", "file", inFile, "size", "-1")
	if err := keyPutCmd.RunE(keyPutCmd, nil); err != nil {
		t.Fatalf("put = %v", err)
	}
	// 输出目录不存在 → os.Create 失败。
	cliTestKeyFlags(t, keyGetCmd, addr, "", "key", "k1", "file", filepath.Join(dir, "no-dir", "o.bin"), "off", "0", "size", "-1")
	if err := keyGetCmd.RunE(keyGetCmd, nil); err == nil {
		t.Fatal("输出路径不可写应报错")
	}
}
