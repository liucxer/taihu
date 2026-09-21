// 本文件是 cmd 包 CLI 层单测的共享脚手架，以及 helpers.go / root.go / version.go 的用例。
//
// 测试缝隙：helpers.go 的 connectKV 委托给包级变量 kvConnect（生产恒为 connectKVReal）。
// 用例把 kvConnect 换成 cluster.NewMemoryKV()，即可在无 TiKV/PD 环境下端到端跑通
// cluster/key/instance/client 各命令；真实数据面用「真实 Storage + transport server +
// 127.0.0.1:0 临时端口」提供（不起 TiKV，不占固定端口）。
package cmd

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/internal/transport"
)

// ---------- 脚手架（全部以 cliTest 前缀命名） ----------

// cliTestErrKV 是所有操作都报错的 KV 实现（覆盖各命令的 KV 错误分支）。
type cliTestErrKV struct{ err error }

func (k cliTestErrKV) Put(context.Context, []byte, []byte) error   { return k.err }
func (k cliTestErrKV) Get(context.Context, []byte) ([]byte, error) { return nil, k.err }
func (k cliTestErrKV) Delete(context.Context, []byte) error        { return k.err }
func (k cliTestErrKV) DeleteRange(context.Context, []byte, []byte) error {
	return k.err
}
func (k cliTestErrKV) Scan(context.Context, []byte, []byte, int) ([][]byte, [][]byte, error) {
	return nil, nil, k.err
}
func (k cliTestErrKV) BatchPut(context.Context, map[string][]byte) error { return k.err }
func (k cliTestErrKV) BatchGet(context.Context, [][]byte) ([][]byte, error) {
	return nil, k.err
}
func (k cliTestErrKV) Close() error { return nil }

var _ cluster.KV = cliTestErrKV{}

// cliTestSetCtx 给命令树每个节点装上非 nil context。直接调 RunE 绕过了 cobra 的
// Execute 装配，而各命令都用 cmd.Context() 派生超时上下文（nil 会让 WithTimeout panic）。
func cliTestSetCtx(c *cobra.Command) {
	c.SetContext(context.Background())
	for _, sub := range c.Commands() {
		cliTestSetCtx(sub)
	}
}

// cliTestGlobal 复位全局参数（用例内可自由改动，结束后恢复原值）。
func cliTestGlobal(t *testing.T) {
	t.Helper()
	cliTestSetCtx(rootCmd)
	old := global
	t.Cleanup(func() { global = old })
	global.pd = ""
	global.tikvCA, global.tikvCert, global.tikvKey = "", "", ""
	global.clientName = ""
	global.timeout = 5 * time.Second
	global.json = false
	global.ioUring = "auto"
	global.ioUringIO = false
}

// cliTestUseKV 把 connectKV 替换为返回给定 KV（t.Cleanup 恢复）。
func cliTestUseKV(t *testing.T, kv cluster.KV) {
	t.Helper()
	old := kvConnect
	kvConnect = func(context.Context) (cluster.KV, error) { return kv, nil }
	t.Cleanup(func() { kvConnect = old })
}

// cliTestUseKVErr 把 connectKV 替换为返回给定错误（t.Cleanup 恢复）。
func cliTestUseKVErr(t *testing.T, err error) {
	t.Helper()
	old := kvConnect
	kvConnect = func(context.Context) (cluster.KV, error) { return nil, err }
	t.Cleanup(func() { kvConnect = old })
}

// cliTestEnv 复位全局参数并把注册区换成内存 KV，返回该 KV。
func cliTestEnv(t *testing.T) *cluster.MemoryKV {
	t.Helper()
	cliTestGlobal(t)
	kv := cluster.NewMemoryKV()
	cliTestUseKV(t, kv)
	return kv
}

// cliTestFlags 以 --k=v 形式为命令解析标志（含继承的 persistent 标志）。
func cliTestFlags(t *testing.T, c *cobra.Command, pairs ...string) {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatalf("cliTestFlags: 参数须成对，实际 %d 个", len(pairs))
	}
	args := make([]string, 0, len(pairs))
	for i := 0; i < len(pairs); i += 2 {
		args = append(args, "--"+pairs[i]+"="+pairs[i+1])
	}
	if err := c.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags(%v): %v", args, err)
	}
}

// cliTestKeyFlags 设置 key 命令的寻址标志（addr/instance 在 keyCmd 的 persistent 上，
// prepareKeyStore 也从 keyCmd.Flags() 读取，故必须先合并父命令标志）与子命令局部标志。
func cliTestKeyFlags(t *testing.T, c *cobra.Command, addr, inst string, pairs ...string) {
	t.Helper()
	cliTestFlags(t, keyCmd, "addr", addr, "instance", inst)
	cliTestFlags(t, c, pairs...)
}

// cliTestStdin 把 os.Stdin 换成含 data 的管道（t.Cleanup 恢复）。
func cliTestStdin(t *testing.T, data string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		_ = r.Close()
	})
}

// cliTestCaptureStdout 把 os.Stdout 重定向到临时文件，返回「读取已写内容」的函数。
// 命令与输出都在同一 goroutine，写入是无缓冲系统调用，故返回的函数即时可读到全部输出。
func cliTestCaptureStdout(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("temp stdout: %v", err)
	}
	path := f.Name()
	old := os.Stdout
	os.Stdout = f
	t.Cleanup(func() {
		os.Stdout = old
		_ = f.Close()
	})
	return func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read stdout: %v", err)
		}
		return string(b)
	}
}

// cliTestServer 起一个真实 Storage + transport server（127.0.0.1:0），返回监听地址。
// 临时「盘」文件用 t.TempDir()（TMPDIR 在 xfs 上，满足 O_DIRECT 要求）。
func cliTestServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dev := filepath.Join(dir, "nvme.img")
	if err := os.WriteFile(dev, nil, 0o644); err != nil {
		t.Fatalf("create device: %v", err)
	}
	st, err := storage.NewStorage(context.Background(), filepath.Join(dir, "meta"), dev,
		layout.Layout{SegmentSizeBytes: layout.DefaultSegmentSizeBytes, SegmentCount: 2048})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	gs := transport.NewServer(st)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(func() {
		gs.Stop()
		_ = ln.Close()
		_ = st.Close()
	})
	return ln.Addr().String()
}

// cliTestRegister 注册实例（心跳按 hbAgo 回拨），返回注册内容。
func cliTestRegister(t *testing.T, kv cluster.KV, inst cluster.InstanceInfo, hbAgo time.Duration) cluster.InstanceInfo {
	t.Helper()
	now := time.Now()
	inst.StartTime = now.Unix()
	inst.LastHeartbeat = now.Add(-hbAgo).Unix()
	if err := cluster.Register(context.Background(), kv, &inst); err != nil {
		t.Fatalf("register %s: %v", inst.Name, err)
	}
	return inst
}

// ---------- helpers.go ----------

func TestCLISplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" a , b ,, ", []string{"a", "b"}},
		{",,", nil},
	}
	for _, c := range cases {
		got := splitCSV(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("splitCSV(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitCSV(%q) = %v, want %v", c.in, got, c.want)
			}
		}
	}
}

func TestCLIHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1 << 10, "1.00 KB"},
		{1 << 20, "1.00 MB"},
		{3 << 30, "3.00 GB"},
		{(1 << 30) - 1, "1024.00 MB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Fatalf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCLIRequirePD(t *testing.T) {
	cliTestGlobal(t)
	if err := requirePD(); err == nil {
		t.Fatal("requirePD() 在 -pd 为空时应报错")
	}
	global.pd = "127.0.0.1:2379"
	if err := requirePD(); err != nil {
		t.Fatalf("requirePD() = %v, want nil", err)
	}
}

func TestCLIAliveness(t *testing.T) {
	now := time.Now()
	if !aliveness(cluster.InstanceInfo{LastHeartbeat: now.Unix()}, now) {
		t.Fatal("刚心跳的实例应判在线")
	}
	if aliveness(cluster.InstanceInfo{LastHeartbeat: now.Add(-time.Minute).Unix()}, now) {
		t.Fatal("心跳超时 1 分钟的实例应判离线")
	}
}

func TestCLIResolveInstance(t *testing.T) {
	all := []cluster.InstanceInfo{{Name: "A"}, {Name: "B"}}
	got, err := resolveInstance(all, "B")
	if err != nil || got.Name != "B" {
		t.Fatalf("resolveInstance(B) = %+v, %v", got, err)
	}
	if _, err := resolveInstance(all, "Z"); err == nil {
		t.Fatal("未注册名字应报错")
	}
}

func TestCLIListAllInstances(t *testing.T) {
	ctx := context.Background()
	kv := cluster.NewMemoryKV()
	for _, inst := range []cluster.InstanceInfo{
		{Name: "b2", Node: "n1"},
		{Name: "a1", Node: "n1"},
		{Name: "a0", Node: "n0"},
	} {
		if err := cluster.Register(ctx, kv, &inst); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	all, err := listAllInstances(ctx, kv)
	if err != nil {
		t.Fatalf("listAllInstances: %v", err)
	}
	want := []string{"a0", "a1", "b2"}
	if len(all) != len(want) {
		t.Fatalf("len = %d, want %d", len(all), len(want))
	}
	for i, n := range want {
		if all[i].Name != n {
			t.Fatalf("排序结果 [%d] = %s, want %s", i, all[i].Name, n)
		}
	}

	// KV 失败透传。
	if _, err := listAllInstances(ctx, cliTestErrKV{err: errors.New("scan down")}); err == nil {
		t.Fatal("KV Scan 失败应透传错误")
	}
}

func TestCLIDialInstance(t *testing.T) {
	addr := cliTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := dialInstance(ctx, cluster.InstanceInfo{Name: "TAIHU-0", Addr: addr})
	if err != nil {
		t.Fatalf("dialInstance: %v", err)
	}
	if _, _, err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 无 addr。
	if _, err := dialInstance(ctx, cluster.InstanceInfo{Name: "no-addr"}); err == nil {
		t.Fatal("实例无 addr 应报错")
	}
	// 拨号失败（端口未监听）。
	if _, err := dialInstance(ctx, cluster.InstanceInfo{Name: "dead", Addr: "127.0.0.1:1"}); err == nil {
		t.Fatal("拨号不可达地址应报错")
	}
}

func TestCLIConnectKV(t *testing.T) {
	kv := cliTestEnv(t)
	got, err := connectKV(context.Background())
	if err != nil {
		t.Fatalf("connectKV: %v", err)
	}
	if got != cluster.KV(kv) {
		t.Fatalf("connectKV 未返回注入的 KV：%T", got)
	}
}

// TestCLIConnectKVReal 覆盖 connectKVReal（真实 TiKV 连接）的错误/超时兜底路径：
// 不连真实 TiKV，PD 指向不可达地址，靠 ctx 超时兜底。不断言具体分支（二者都是合法的
// 失败形态），只要求不 panic、不长时间阻塞。
func TestCLIConnectKVReal(t *testing.T) {
	cliTestGlobal(t)
	global.pd = "10.255.255.1:2379" // 不可路由：拨号不会成功
	global.timeout = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	kv, err := connectKVReal(ctx)
	if kv != nil {
		_ = kv.Close()
	}
	if err == nil && kv == nil {
		t.Fatal("connectKVReal 既无 KV 也无错误")
	}
}

func TestCLIPrintJSON(t *testing.T) {
	out := cliTestCaptureStdout(t)
	printJSON(map[string]int{"a": 1})
	if got := out(); !strings.Contains(got, "\"a\": 1") {
		t.Fatalf("printJSON 输出 = %q", got)
	}
}

// ---------- root.go ----------

func TestCLICtxWithTimeout(t *testing.T) {
	cliTestGlobal(t)
	global.timeout = 30 * time.Millisecond
	ctx, cancel := ctxWithTimeout(context.Background())
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("ctxWithTimeout 应带 deadline")
	}
	if d := time.Until(dl); d <= 0 || d > time.Second {
		t.Fatalf("deadline 距now %v，不符合 30ms", d)
	}
}

func TestCLIAioOptions(t *testing.T) {
	cliTestGlobal(t)
	for _, mode := range []string{"auto", "on", "off"} {
		global.ioUring = mode
		global.ioUringIO = true
		opts, err := aioOptions()
		if err != nil {
			t.Fatalf("aioOptions(%s): %v", mode, err)
		}
		if len(opts) != 2 {
			t.Fatalf("aioOptions(%s) 选项数 = %d, want 2", mode, len(opts))
		}
	}
	global.ioUring = "bogus"
	if _, err := aioOptions(); err == nil {
		t.Fatal("非法 -io-uring 取值应报错")
	}
}

// TestCLIExecute 覆盖 rootCmd 的命令装配与 Execute 的两条返回路径。
func TestCLIExecute(t *testing.T) {
	cliTestGlobal(t)
	out := cliTestCaptureStdout(t)
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	rootCmd.SetArgs([]string{"version"})
	if err := Execute(); err != nil {
		t.Fatalf("Execute(version) = %v", err)
	}
	if got := out(); !strings.Contains(got, "taihu ") {
		t.Fatalf("version 输出 = %q", got)
	}

	// 无子命令：打印帮助并正常返回。
	rootCmd.SetArgs([]string{})
	if err := Execute(); err != nil {
		t.Fatalf("Execute() 无参数 = %v", err)
	}

	// 未知标志：返回错误。
	rootCmd.SetArgs([]string{"--no-such-flag"})
	if err := Execute(); err == nil {
		t.Fatal("Execute(--no-such-flag) 应报错")
	}
}

func TestCLIVersionCmd(t *testing.T) {
	out := cliTestCaptureStdout(t)
	versionCmd.Run(versionCmd, nil)
	if got := out(); !strings.HasPrefix(got, "taihu ") {
		t.Fatalf("version 输出 = %q", got)
	}
}
