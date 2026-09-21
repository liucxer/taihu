//go:build linux

package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/cluster"
)

// errStubKV 注入 newTiKVKV 的假连接错误。
var errStubKV = errors.New("pd unreachable")

// 本文件覆盖 server 子命令的 RunE：参数校验失败路径 + 完整生命周期
// （容量记录/集群注册/段压缩/数据面监听/信号停机注销）。单测里把 helpers.go 的
// newTiKVKV 换成内存 KV、deviceCapacity 换成普通文件大小，故不需要真实 TiKV 与裸盘。
// 停机靠向本进程发 SIGTERM（RunE 内 signal.Notify 处理），测试自身也接管 SIGTERM
// 以免在 RunE 注册监听前收到信号时被默认处置杀掉。

// benchTestServerFlags 返回 server 的全量 flag 默认值。
func benchTestServerFlags() map[string]string {
	return map[string]string{
		"listen": "127.0.0.1", "db": "db-dir", "dev": "dev-path", "server-name": "TAIHU-BENCH",
		"batch": "0", "batch-workers": "2",
		"write-batch": "0", "write-workers": "2",
		"del-batch": "0", "del-workers": "2",
	}
}

// benchTestRunArgs 与 benchTestRunE 等价但不在解析失败时 t.Fatalf，
// 以便在被测命令跑在独立 goroutine 时安全使用。
func benchTestRunArgs(c *cobra.Command, flags map[string]string) error {
	names := make([]string, 0, len(flags))
	for k := range flags {
		names = append(names, k)
	}
	sort.Strings(names)
	args := make([]string, 0, len(names))
	for _, k := range names {
		args = append(args, "--"+k+"="+flags[k])
	}
	if err := c.ParseFlags(args); err != nil {
		return err
	}
	return c.RunE(c, nil)
}

// benchTestCatchTerm 接管 SIGTERM：测试进程不被信号杀掉，RunE 内的 signal.Notify
// 无论何时注册都能收到停机信号。
func benchTestCatchTerm(t *testing.T) {
	t.Helper()
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(ch) })
}

// benchTestWaitRegistered 等待实例注册完成（RunE 在监听与注册都完成后才阻塞）。
func benchTestWaitRegistered(t *testing.T, kv cluster.KV, name string, done <-chan error, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		select {
		case err := <-done:
			t.Fatalf("server 在完成注册前退出: %v", err)
		default:
		}
		all, err := cluster.ListInstances(context.Background(), kv)
		if err == nil {
			for _, inst := range all {
				if inst.Name == name {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server 未在 %s 内完成注册", d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// benchTestWaitTCP 等待 addr 可连接（数据面 Serve 已启动）。
func benchTestWaitTCP(t *testing.T, addr string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var err error
	for {
		var c net.Conn
		if c, err = net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server 未在 %s 内监听 %s: %v", d, addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// benchTestWaitShutdown 周期发 SIGTERM 直至 RunE 返回（兜住"信号早于 signal.Notify 注册"）。
func benchTestWaitShutdown(t *testing.T, done <-chan error) error {
	t.Helper()
	for i := 0; i < 60; i++ {
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case err := <-done:
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("server 未在 SIGTERM 后退出")
	return nil
}

// benchTestWaitTCPClosed 等待 addr 不再可连接（GracefulStop 释放监听）。
func benchTestWaitTCPClosed(addr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return true
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestServerCmdValidate 覆盖参数校验的每条分支（必填项与监听 IP 解析）。
func TestServerCmdValidate(t *testing.T) {
	cases := []struct {
		name string
		pd   string
		mut  map[string]string
		part string
	}{
		{"no-db", "127.0.0.1:2379", map[string]string{"db": ""}, "-db and -dev are required"},
		{"no-dev", "127.0.0.1:2379", map[string]string{"dev": ""}, "-db and -dev are required"},
		{"no-server-name", "127.0.0.1:2379", map[string]string{"server-name": ""}, "-server-name and -pd are required"},
		{"no-pd", "", nil, "-server-name and -pd are required"},
		{"no-listen", "127.0.0.1:2379", map[string]string{"listen": ""}, "-listen is required"},
		{"bad-ip", "127.0.0.1:2379", map[string]string{"listen": "localhost"}, `invalid listen IP "localhost"`},
		{"bad-ip-second", "127.0.0.1:2379", map[string]string{"listen": "127.0.0.1,300.1.1.1"}, `invalid listen IP "300.1.1.1"`},
	}
	for _, tc := range cases {
		benchTestGlobals(t, tc.pd, "")
		f := benchTestServerFlags()
		for k, v := range tc.mut {
			f[k] = v
		}
		err := benchTestRunE(t, serverCmd, f)
		if err == nil || !strings.Contains(err.Error(), tc.part) {
			t.Fatalf("%s: err=%v want contains %q", tc.name, err, tc.part)
		}
	}
}

// TestServerCmdLifecycle 端到端跑通 server 启动（容量记录→注册→TCP/shm 数据面→pprof）
// 与 SIGTERM 停机（注销 + GracefulStop + 各 defer 收尾）。
func TestServerCmdLifecycle(t *testing.T) {
	benchTestGlobals(t, "127.0.0.1:2379", "")
	benchTestStubCapacity(t)
	kv := cluster.NewMemoryKV()
	benchTestStubKVConnect(t, kv, nil)
	benchTestCatchTerm(t)

	name := fmt.Sprintf("TAIHU-BENCH-%d", os.Getpid())
	sock := "/dev/" + name
	t.Cleanup(func() { _ = os.Remove(sock) })

	dev, db := benchTestDisk(t)
	f := benchTestServerFlags()
	// 第二个 IP 带空格且与首个重复：覆盖 TrimSpace 与去重分支（最终只绑一个地址）。
	f["listen"] = "127.0.0.1, 127.0.0.1"
	f["db"], f["dev"], f["server-name"] = db, dev, name
	// 打开两条流水线（写/删批处理 + shm 批读），覆盖各参数解析与 shm 服务启动。
	f["batch"], f["batch-workers"] = "1", "2"
	f["write-batch"], f["write-workers"] = "1", "2"
	f["del-batch"], f["del-workers"] = "1", "2"

	done := make(chan error, 1)
	go func() { done <- benchTestRunArgs(serverCmd, f) }()

	benchTestWaitRegistered(t, kv, name, done, 10*time.Second)

	all, err := cluster.ListInstances(context.Background(), kv)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	inst, err := resolveInstance(all, name)
	if err != nil {
		t.Fatalf("注册记录缺少实例: %v", err)
	}
	if len(inst.Addrs) != 1 {
		t.Fatalf("重复 IP 应去重为 1 个地址，实际 %v", inst.Addrs)
	}
	// 容量记录（PutCapacity）与段数写进 KV，供客户端/SDK 判定水位。
	if rec, ok, err := cluster.GetCapacity(context.Background(), kv, name); err != nil || !ok {
		t.Fatalf("容量记录缺失: ok=%v err=%v", ok, err)
	} else if rec.ListenAddr != inst.Addr {
		t.Fatalf("容量记录监听地址=%q 与注册地址=%q 不一致", rec.ListenAddr, inst.Addr)
	}
	if inst.ShmAddr != sock {
		t.Fatalf("shm 地址=%q want %q", inst.ShmAddr, sock)
	}
	benchTestWaitTCP(t, inst.Addr, 10*time.Second)
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("shm unix socket 未创建: %v", err)
	}

	// 停机：RunE 内 shutdown goroutine 注销注册 + GracefulStop 数据面。
	if err := benchTestWaitShutdown(t, done); err != nil {
		t.Fatalf("server 退出错误: %v", err)
	}
	if !benchTestWaitTCPClosed(inst.Addr, 5*time.Second) {
		t.Errorf("停机后数据面端口 %s 仍可连接（GracefulStop 未释放监听）", inst.Addr)
	}
}

// TestServerCmdShmServeError 覆盖 shm 数据面启动失败的错误路径：
// -server-name 含 "/" 时 /dev/<name> 无法作为 unix socket 路径创建。
func TestServerCmdShmServeError(t *testing.T) {
	benchTestGlobals(t, "127.0.0.1:2379", "")
	benchTestStubCapacity(t)
	kv := cluster.NewMemoryKV()
	benchTestStubKVConnect(t, kv, nil)
	benchTestCatchTerm(t)

	dev, db := benchTestDisk(t)
	f := benchTestServerFlags()
	f["listen"] = "127.0.0.1"
	f["db"], f["dev"], f["server-name"] = db, dev, "TAIHU/BENCH-BAD"

	done := make(chan error, 1)
	go func() { done <- benchTestRunArgs(serverCmd, f) }()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "serve shm") {
			t.Fatalf("server-name 含 / 时应报 serve shm 错误，实际 %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("shm 启动失败路径未在 15s 内返回")
	}
	// 实例应已注册（注册在 shm 之前），且心跳随 defer clusterCancel 停止。
	all, err := cluster.ListInstances(context.Background(), kv)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if _, err := resolveInstance(all, "TAIHU/BENCH-BAD"); err != nil {
		t.Fatalf("注册记录缺失: %v (all=%v)", err, all)
	}
}

// TestServerCmdCapacityChanged 覆盖"容量与 TiKV 记录不一致拒绝启动"的分支。
func TestServerCmdCapacityChanged(t *testing.T) {
	benchTestGlobals(t, "127.0.0.1:2379", "")
	benchTestStubCapacity(t)
	kv := cluster.NewMemoryKV()
	benchTestStubKVConnect(t, kv, nil)
	benchTestCatchTerm(t)

	name := fmt.Sprintf("TAIHU-BENCH-CAP-%d", os.Getpid())
	dev, db := benchTestDisk(t)
	if err := cluster.PutCapacity(context.Background(), kv, name, cluster.CapacityRecord{
		CapacityBytes: 12345, SegmentSizeBytes: 1 << 30, SegmentCount: 1,
		ListenAddr: "127.0.0.1:1", UpdateTime: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutCapacity: %v", err)
	}

	f := benchTestServerFlags()
	f["db"], f["dev"], f["server-name"], f["listen"] = db, dev, name, "127.0.0.1"
	err := benchTestRunE(t, serverCmd, f)
	if err == nil || !strings.Contains(err.Error(), "capacity changed") {
		t.Fatalf("容量不一致应拒绝启动，实际 %v", err)
	}
}

// TestServerCmdKVError 覆盖 TiKV 连接失败路径。
func TestServerCmdKVError(t *testing.T) {
	benchTestGlobals(t, "127.0.0.1:2379", "")
	benchTestStubKVConnect(t, nil, errStubKV)
	dev, db := benchTestDisk(t)

	f := benchTestServerFlags()
	f["db"], f["dev"], f["server-name"], f["listen"] = db, dev, "TAIHU-BENCH-KVERR", "127.0.0.1"
	err := benchTestRunE(t, serverCmd, f)
	if err == nil || !strings.Contains(err.Error(), "tikv connect") {
		t.Fatalf("TiKV 连接失败应报错，实际 %v", err)
	}
}

// TestServerCmdDeviceCapacityError 覆盖容量查询失败路径（默认缝隙=真实 BLKGETSIZE64，
// 普通文件上必失败）。
func TestServerCmdDeviceCapacityError(t *testing.T) {
	benchTestGlobals(t, "127.0.0.1:2379", "")
	benchTestStubKVConnect(t, cluster.NewMemoryKV(), nil)
	dev, db := benchTestDisk(t)

	f := benchTestServerFlags()
	f["db"], f["dev"], f["server-name"], f["listen"] = db, dev, "TAIHU-BENCH-CAPERR", "127.0.0.1"
	err := benchTestRunE(t, serverCmd, f)
	if err == nil || !strings.Contains(err.Error(), "DeviceCapacity") {
		t.Fatalf("普通文件容量查询应失败，实际 %v", err)
	}
}

// TestServerCmdAIOPreflightError 覆盖 aioOptions 失败路径：容量记录已写入 KV、
// 但全局 -io-uring 取值非法时，在打开 Storage 之前返回错误。
func TestServerCmdAIOPreflightError(t *testing.T) {
	benchTestGlobals(t, "127.0.0.1:2379", "")
	benchTestStubCapacity(t)
	benchTestStubKVConnect(t, cluster.NewMemoryKV(), nil)
	oldIO := global.ioUring
	global.ioUring = "bogus"
	t.Cleanup(func() { global.ioUring = oldIO })

	dev, db := benchTestDisk(t)
	f := benchTestServerFlags()
	f["db"], f["dev"], f["server-name"], f["listen"] = db, dev, "TAIHU-BENCH-IOURING", "127.0.0.1"
	err := benchTestRunE(t, serverCmd, f)
	if err == nil || !strings.Contains(err.Error(), "io-uring") {
		t.Fatalf("非法 -io-uring 应在打开 Storage 前报错，实际 %v", err)
	}
}

// TestListenMultiPortAndPickPort 覆盖端口抢占辅助函数：成功路径、端口耗尽与去重。
func TestListenMultiPortAndPickPort(t *testing.T) {
	lns, port, err := listenMultiPort([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("listenMultiPort: %v", err)
	}
	if port < portRangeStart || port > portRangeEnd || len(lns) != 1 {
		t.Fatalf("listenMultiPort=(%d listeners, port %d)", len(lns), port)
	}
	for _, ln := range lns {
		if ln.Addr().String() != net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port)) {
			t.Fatalf("listener addr=%s port=%d", ln.Addr(), port)
		}
	}

	// 同一端口已被 lns[0] 占用 → pickPort 排除后必须返回其它端口。
	pprofLn, pport, err := pickPort(map[int]struct{}{port: {}})
	if err != nil {
		t.Fatalf("pickPort: %v", err)
	}
	if pport == port {
		t.Fatalf("pickPort 返回了被排除的端口 %d", port)
	}
	_ = pprofLn.Close()
	for _, ln := range lns {
		_ = ln.Close()
	}

	// 监听 IP 非法（绑不上）→ 区间内所有端口都失败 → 返回错误。
	bad := []string{"203.0.113.7"} // TEST-NET-3：本机未持有该地址，绑定必失败
	if lns, port, err := listenMultiPort(bad); err == nil {
		for _, ln := range lns {
			_ = ln.Close()
		}
		t.Fatalf("不可绑定的 IP 应返回错误，实际 port=%d", port)
	}
}
