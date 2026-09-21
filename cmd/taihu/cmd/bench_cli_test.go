package cmd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/internal/transport"
)

// 本文件覆盖 bench storage / bench single / bench cluster 三个子命令的 RunE：
// 参数校验失败路径、拨号/连接/容量查询等错误路径、以及用本地临时"盘"+ 进程内
// 真实 taihu-server（TCP 与 shm 两条数据面）跑通的完整压测流程。
// 全部确定性：随机端口、临时目录、极小 size/count，无 TiKV/PD、无真实块设备、无长 sleep。

// benchTestRunE 解析给定 flag 并直接执行命令 RunE（不经 cobra Execute：不依赖命令树
// 与 os.Args，但 RunE 本体真正被执行到）。flags 须给全，避免上次解析残留。
func benchTestRunE(t *testing.T, c *cobra.Command, flags map[string]string) error {
	t.Helper()
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
		t.Fatalf("ParseFlags(%v): %v", args, err)
	}
	return c.RunE(c, nil)
}

// benchTestGlobals 设置根级全局参数并保证用例结束还原（含 AIO 后端）。
func benchTestGlobals(t *testing.T, pd, clientName string) {
	t.Helper()
	oldPD, oldClient, oldIO := global.pd, global.clientName, global.ioUring
	global.pd, global.clientName, global.ioUring = pd, clientName, "auto"
	t.Cleanup(func() { global.pd, global.clientName, global.ioUring = oldPD, oldClient, oldIO })
}

// benchTestStubKVConnect 替换 helpers.go 的 newTiKVKV 缝隙（内存 KV / 连接错误），
// 供 server 与 bench cluster 两条直连 TiKV 的启动路径在无 TiKV 环境下跑通。
func benchTestStubKVConnect(t *testing.T, kv cluster.KV, err error) {
	t.Helper()
	old := newTiKVKV
	newTiKVKV = func(context.Context, []string, cluster.TLSConfig) (cluster.KV, error) { return kv, err }
	t.Cleanup(func() { newTiKVKV = old })
}

// benchTestStubCapacity 把 deviceCapacity 缝隙换成"按普通文件大小"（模拟裸盘容量），
// 使 bench storage / server 能在临时目录的普通文件上端到端跑通。
func benchTestStubCapacity(t *testing.T) {
	t.Helper()
	old := deviceCapacity
	deviceCapacity = func(path string) (int64, error) {
		st, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		return st.Size(), nil
	}
	t.Cleanup(func() { deviceCapacity = old })
}

// benchTestDisk 造一个稀疏"盘"文件（xfs 上普通文件即可满足 O_DIRECT）与 pebble 目录，
// 容量 2 段（16GiB 稀疏，不占实际空间）。
func benchTestDisk(t *testing.T) (dev, db string) {
	t.Helper()
	dir := t.TempDir()
	dev = filepath.Join(dir, "nvme.img")
	f, err := os.Create(dev)
	if err != nil {
		t.Fatalf("create disk: %v", err)
	}
	if err := f.Truncate(2 * layout.DefaultSegmentSizeBytes); err != nil {
		_ = f.Close()
		t.Fatalf("truncate disk: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close disk: %v", err)
	}
	return dev, filepath.Join(dir, "meta")
}

// benchTestStorage 进程内真实 Storage（临时盘 + 临时 pebble 目录），用例结束关闭。
func benchTestStorage(t *testing.T) *storage.Storage {
	t.Helper()
	dev, db := benchTestDisk(t)
	st, err := storage.NewStorage(context.Background(), db, dev,
		layout.Layout{SegmentSizeBytes: layout.DefaultSegmentSizeBytes, SegmentCount: 2048})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// benchTestRPCServer 起一个进程内 TCP taihu-server（随机端口），返回监听地址。
func benchTestRPCServer(t *testing.T) string {
	t.Helper()
	st := benchTestStorage(t)
	gs := transport.NewServer(st)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(func() {
		gs.Stop()
		_ = ln.Close()
	})
	return ln.Addr().String()
}

// benchTestShmServer 起一个进程内 shmipc taihu-server（unix socket），返回 socket 路径。
func benchTestShmServer(t *testing.T) string {
	t.Helper()
	st := benchTestStorage(t)
	uds := filepath.Join(t.TempDir(), "taihu.sock")
	srv, err := transport.ServeShm(st, uds)
	if err != nil {
		t.Fatalf("ServeShm: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return uds
}

// benchTestWaitIndex 等待异步索引写入 KV（集群读模式定位依赖）。
func benchTestWaitIndex(t *testing.T, kv cluster.KV, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ks, _, err := kv.Scan(context.Background(),
			[]byte(cluster.IndexKeyPrefix), []byte(cluster.IndexKeyPrefix+"\xff"), 0)
		if err == nil && len(ks) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("索引未在 3s 内写入 KV：%d/%d (err=%v)", len(ks), want, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- bench storage

// benchTestStorageFlags 返回 bench storage 的全量 flag 默认值（每次调用都整体下发，
// 避免上一次 ParseFlags 的值残留影响用例）。
func benchTestStorageFlags() map[string]string {
	return map[string]string{
		"mode": "write", "size": "4096", "threads": "1", "count": "2",
		"keys-prefix": "benchtest", "db": "db-dir", "dev": "dev-path",
		"report-interval": "1h", "latency": "false", "rand": "false",
		"cpuprofile": "", "memprofile": "",
	}
}

// TestBenchStorageCmdValidate 覆盖 validate 的每条校验分支。
func TestBenchStorageCmdValidate(t *testing.T) {
	cases := []struct {
		name  string
		mut   map[string]string
		parts string
	}{
		{"no-mode", map[string]string{"mode": ""}, "invalid -mode"},
		{"bad-mode", map[string]string{"mode": "delete"}, "invalid -mode"},
		{"neg-size", map[string]string{"mode": "write", "size": "-1"}, "-size must be >= 0"},
		{"zero-threads", map[string]string{"mode": "write", "threads": "0"}, "-threads must be > 0"},
		{"neg-threads", map[string]string{"mode": "write", "threads": "-4"}, "-threads must be > 0"},
		{"zero-count", map[string]string{"mode": "write", "count": "0"}, "-count must be > 0"},
		{"no-db", map[string]string{"mode": "read", "db": ""}, "-db and -dev are required"},
		{"no-dev", map[string]string{"mode": "read", "dev": ""}, "-db and -dev are required"},
	}
	for _, tc := range cases {
		f := benchTestStorageFlags()
		for k, v := range tc.mut {
			f[k] = v
		}
		err := benchTestRunE(t, benchStorageCmd, f)
		if err == nil || !strings.Contains(err.Error(), tc.parts) {
			t.Fatalf("%s: err=%v want contains %q", tc.name, err, tc.parts)
		}
	}
}

// TestBenchStorageCmdWriteRead 端到端：write 灌数 → read（含随机读、CPU/内存 profile）。
func TestBenchStorageCmdWriteRead(t *testing.T) {
	benchTestGlobals(t, "", "")
	benchTestStubCapacity(t)
	dev, db := benchTestDisk(t)
	profDir := t.TempDir()
	cpuProf := filepath.Join(profDir, "cpu.prof")
	memProf := filepath.Join(profDir, "mem.prof")

	f := benchTestStorageFlags()
	f["dev"], f["db"] = dev, db
	f["mode"], f["size"], f["threads"], f["count"] = "write", "4096", "2", "4"
	f["latency"], f["report-interval"] = "true", "1ms"
	f["cpuprofile"], f["memprofile"] = cpuProf, memProf
	if err := benchTestRunE(t, benchStorageCmd, f); err != nil {
		t.Fatalf("bench storage write: %v", err)
	}

	// 读模式：-rand 覆盖随机读分支，-cpuprofile 复用上一轮文件会重复 StartCPUProfile，
	// 故本轮不再开 profile。
	f["mode"], f["rand"] = "read", "true"
	f["cpuprofile"], f["memprofile"] = "", ""
	if err := benchTestRunE(t, benchStorageCmd, f); err != nil {
		t.Fatalf("bench storage read: %v", err)
	}

	for _, p := range []string{cpuProf, memProf} {
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			t.Fatalf("profile %s 应被写出: %v", p, err)
		}
	}

	// 单线程读（-threads=1）且 count 大于线程数：另一条区间切分路径。
	f["mode"], f["rand"], f["threads"], f["count"] = "read", "false", "1", "4"
	f["latency"] = "false"
	if err := benchTestRunE(t, benchStorageCmd, f); err != nil {
		t.Fatalf("bench storage single-thread read: %v", err)
	}
}

// TestBenchStorageCmdErrorPaths 覆盖容量查询/AIO 选项/pebble 打开失败与 memprofile 写失败。
func TestBenchStorageCmdErrorPaths(t *testing.T) {
	benchTestGlobals(t, "", "")
	dev, db := benchTestDisk(t)

	// 未打桩 deviceCapacity：普通文件上 BLKGETSIZE64 必失败（真实缝隙行为）。
	f := benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = dev, db, "write"
	if err := benchTestRunE(t, benchStorageCmd, f); err == nil ||
		!strings.Contains(err.Error(), "DeviceCapacity") {
		t.Fatalf("plain-file capacity err=%v want DeviceCapacity", err)
	}

	// -cpuprofile 指向目录：os.Create 失败。
	benchTestStubCapacity(t)
	f = benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = dev, db, "write"
	f["cpuprofile"] = t.TempDir()
	if err := benchTestRunE(t, benchStorageCmd, f); err == nil ||
		!strings.Contains(err.Error(), "cpuprofile") {
		t.Fatalf("cpuprofile dir err=%v", err)
	}

	// 全局 -io-uring 取值非法：aioOptions 在容量查询后报错。
	oldIO := global.ioUring
	global.ioUring = "bogus"
	t.Cleanup(func() { global.ioUring = oldIO })
	f = benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = dev, db, "write"
	if err := benchTestRunE(t, benchStorageCmd, f); err == nil ||
		!strings.Contains(err.Error(), "io-uring") {
		t.Fatalf("bad io-uring err=%v", err)
	}
	global.ioUring = "auto"

	// -db 指向普通文件：pebble 打开失败。
	f = benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = dev, dev, "write"
	if err := benchTestRunE(t, benchStorageCmd, f); err == nil ||
		!strings.Contains(err.Error(), "NewStorage") {
		t.Fatalf("NewStorage err=%v", err)
	}

	// -dev 不存在：容量查询（DeviceCapacity）先失败，早于 NewStorage。
	f = benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = filepath.Join(t.TempDir(), "missing.img"), db, "write"
	if err := benchTestRunE(t, benchStorageCmd, f); err == nil ||
		!strings.Contains(err.Error(), "DeviceCapacity") {
		t.Fatalf("missing dev err=%v", err)
	}

	// -memprofile 写失败：只打日志，命令本身仍然成功。
	f = benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = dev, db, "write"
	f["count"], f["size"] = "1", "4096"
	f["memprofile"] = t.TempDir()
	if err := benchTestRunE(t, benchStorageCmd, f); err != nil {
		t.Fatalf("memprofile 写失败不应中断压测: %v", err)
	}

	// 库中 key 不全时读：worker 出错 → 各 worker 结果汇聚为 runErr 并返回。
	// count=3/threads=2 同时覆盖 partitionRange 的 i<rem 分支。
	f = benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = dev, db, "read"
	f["threads"], f["count"] = "2", "3"
	if err := benchTestRunE(t, benchStorageCmd, f); err == nil ||
		!strings.Contains(err.Error(), "run error") {
		t.Fatalf("missing key read err=%v", err)
	}
}

// ----------------------------------------------------------------- bench single

func benchTestSingleFlags() map[string]string {
	return map[string]string{
		"transport": "", "addr": "", "shm": "", "conns": "2", "cpuprofile": "",
		"mode": "write", "size": "4096", "threads": "1", "count": "2",
		"keys-prefix": "sbenchtest", "report-interval": "1h", "latency": "false",
		"pipeline": "1",
	}
}

// TestBenchSingleCmdValidate 覆盖 single 的 transport/addr/shm 与公共参数校验。
func TestBenchSingleCmdValidate(t *testing.T) {
	cases := []struct {
		name  string
		mut   map[string]string
		parts string
	}{
		{"no-transport", map[string]string{"transport": ""}, "-transport must be rpc or shm"},
		{"bad-transport", map[string]string{"transport": "udp"}, "-transport must be rpc or shm"},
		{"rpc-no-addr", map[string]string{"transport": "rpc"}, "requires -addr"},
		{"shm-no-shm", map[string]string{"transport": "shm"}, "requires -shm"},
		{"bad-mode", map[string]string{"transport": "rpc", "addr": "127.0.0.1:1", "mode": "load"}, "invalid -mode"},
		{"zero-size", map[string]string{"transport": "rpc", "addr": "127.0.0.1:1", "size": "-1"}, "-size must be >= 0"},
		{"zero-threads", map[string]string{"transport": "rpc", "addr": "127.0.0.1:1", "threads": "0"}, "-threads must be > 0"},
		{"zero-count", map[string]string{"transport": "rpc", "addr": "127.0.0.1:1", "count": "0"}, "-count must be > 0"},
		{"zero-pipeline", map[string]string{"transport": "rpc", "addr": "127.0.0.1:1", "pipeline": "0"}, "-pipeline must be >= 1"},
	}
	for _, tc := range cases {
		f := benchTestSingleFlags()
		for k, v := range tc.mut {
			f[k] = v
		}
		err := benchTestRunE(t, benchSingleCmd, f)
		if err == nil || !strings.Contains(err.Error(), tc.parts) {
			t.Fatalf("%s: err=%v want contains %q", tc.name, err, tc.parts)
		}
	}
}

// TestBenchSingleCmdRPCRoundTrip 端到端：直连进程内 taihu-server（TCP）跑 write/read/delete。
func TestBenchSingleCmdRPCRoundTrip(t *testing.T) {
	benchTestGlobals(t, "", "")
	addr := benchTestRPCServer(t)

	f := benchTestSingleFlags()
	f["transport"], f["addr"] = "rpc", addr
	f["mode"], f["count"], f["threads"] = "write", "3", "2"
	f["latency"], f["report-interval"] = "true", "1ms"
	f["cpuprofile"] = filepath.Join(t.TempDir(), "single-cpu.prof")
	if err := benchTestRunE(t, benchSingleCmd, f); err != nil {
		t.Fatalf("single rpc write: %v", err)
	}

	f["mode"], f["cpuprofile"], f["pipeline"] = "read", "", "2"
	if err := benchTestRunE(t, benchSingleCmd, f); err != nil {
		t.Fatalf("single rpc read: %v", err)
	}

	f["mode"], f["pipeline"] = "delete", "1"
	if err := benchTestRunE(t, benchSingleCmd, f); err != nil {
		t.Fatalf("single rpc delete: %v", err)
	}
}

// TestBenchSingleCmdShmRoundTrip 端到端：直连进程内 shmipc 服务端（unix socket）。
func TestBenchSingleCmdShmRoundTrip(t *testing.T) {
	benchTestGlobals(t, "", "")
	uds := benchTestShmServer(t)

	f := benchTestSingleFlags()
	f["transport"], f["shm"] = "shm", uds
	f["mode"], f["size"], f["count"] = "write", "4096", "2"
	if err := benchTestRunE(t, benchSingleCmd, f); err != nil {
		t.Fatalf("single shm write: %v", err)
	}
	f["mode"] = "read"
	if err := benchTestRunE(t, benchSingleCmd, f); err != nil {
		t.Fatalf("single shm read: %v", err)
	}
}

// TestBenchSingleCmdDialAndProfileErrors 覆盖拨号失败与 cpuprofile 创建失败。
func TestBenchSingleCmdDialAndProfileErrors(t *testing.T) {
	benchTestGlobals(t, "", "")

	// TCP 端口不可达（127.0.0.1:1 无监听）。
	f := benchTestSingleFlags()
	f["transport"], f["addr"] = "rpc", "127.0.0.1:1"
	if err := benchTestRunE(t, benchSingleCmd, f); err == nil ||
		!strings.Contains(err.Error(), "dial(rpc)") {
		t.Fatalf("rpc dial err=%v", err)
	}

	// shm socket 不存在。
	f = benchTestSingleFlags()
	f["transport"], f["shm"] = "shm", filepath.Join(t.TempDir(), "nope.sock")
	if err := benchTestRunE(t, benchSingleCmd, f); err == nil ||
		!strings.Contains(err.Error(), "dial(shm)") {
		t.Fatalf("shm dial err=%v", err)
	}

	// -cpuprofile 指向目录。
	f = benchTestSingleFlags()
	f["transport"], f["addr"] = "rpc", "127.0.0.1:1"
	f["cpuprofile"] = t.TempDir()
	if err := benchTestRunE(t, benchSingleCmd, f); err == nil ||
		!strings.Contains(err.Error(), "cpuprofile") {
		t.Fatalf("cpuprofile err=%v", err)
	}

	// 空服务端读不存在的 key：数据面报错 → benchkit.Run 失败 → run error。
	f = benchTestSingleFlags()
	f["transport"], f["addr"] = "rpc", benchTestRPCServer(t)
	f["mode"], f["count"] = "read", "1"
	if err := benchTestRunE(t, benchSingleCmd, f); err == nil ||
		!strings.Contains(err.Error(), "run error") {
		t.Fatalf("read-miss err=%v", err)
	}
}

// ---------------------------------------------------------------- bench cluster

func benchTestClusterFlags() map[string]string {
	return map[string]string{
		"transport": "rpc", "write-routing": "", "conns": "2", "preload": "false",
		"cpuprofile": "", "mode": "write", "size": "4096", "threads": "1",
		"pipeline": "1", "keys-prefix": "cbenchtest", "count": "2",
		"report-interval": "1h", "latency": "false",
	}
}

// TestBenchClusterCmdValidate 覆盖 transport/client-name/pd 与公共参数校验。
func TestBenchClusterCmdValidate(t *testing.T) {
	cases := []struct {
		name       string
		pd, client string
		mut        map[string]string
		parts      string
	}{
		{"bad-transport", "127.0.0.1:2379", "c1", map[string]string{"transport": "udp"}, "invalid -transport"},
		{"empty-transport", "127.0.0.1:2379", "c1", map[string]string{"transport": ""}, "invalid -transport"},
		{"no-client-name", "127.0.0.1:2379", "", nil, "-client-name is required"},
		{"no-pd", "", "c1", nil, "-client-name requires -pd"},
		{"bad-mode", "127.0.0.1:2379", "c1", map[string]string{"mode": "load"}, "invalid -mode"},
		{"zero-threads", "127.0.0.1:2379", "c1", map[string]string{"threads": "0"}, "-threads must be > 0"},
		{"zero-count", "127.0.0.1:2379", "c1", map[string]string{"count": "0"}, "-count must be > 0"},
		{"zero-pipeline", "127.0.0.1:2379", "c1", map[string]string{"pipeline": "0"}, "-pipeline must be >= 1"},
	}
	for _, tc := range cases {
		benchTestGlobals(t, tc.pd, tc.client)
		f := benchTestClusterFlags()
		for k, v := range tc.mut {
			f[k] = v
		}
		err := benchTestRunE(t, benchClusterCmd, f)
		if err == nil || !strings.Contains(err.Error(), tc.parts) {
			t.Fatalf("%s: err=%v want contains %q", tc.name, err, tc.parts)
		}
	}
}

// TestBenchClusterCmdErrorPaths 覆盖 cpuprofile 失败、TiKV 连接失败、运行期错误。
func TestBenchClusterCmdErrorPaths(t *testing.T) {
	benchTestGlobals(t, "127.0.0.1:2379", "benchtest")
	benchTestStubKVConnect(t, cluster.NewMemoryKV(), errors.New("pd unreachable"))

	// -cpuprofile 指向目录：在连 TiKV 之前就失败。
	f := benchTestClusterFlags()
	f["cpuprofile"] = t.TempDir()
	if err := benchTestRunE(t, benchClusterCmd, f); err == nil ||
		!strings.Contains(err.Error(), "cpuprofile") {
		t.Fatalf("cpuprofile err=%v", err)
	}

	// newTiKVKV 报错：错误里带 PD 地址。
	f = benchTestClusterFlags()
	if err := benchTestRunE(t, benchClusterCmd, f); err == nil ||
		!strings.Contains(err.Error(), "tikv 127.0.0.1:2379") {
		t.Fatalf("tikv connect err=%v", err)
	}

	// 注册区空：run error（picker 无在线实例）。
	benchTestStubKVConnect(t, cluster.NewMemoryKV(), nil)
	f = benchTestClusterFlags()
	if err := benchTestRunE(t, benchClusterCmd, f); err == nil ||
		!strings.Contains(err.Error(), "run error") {
		t.Fatalf("no-instance err=%v", err)
	}

	// 注册实例地址不可达：写入失败同样走 run error。
	kv := cluster.NewMemoryKV()
	benchTestStubKVConnect(t, kv, nil)
	if err := cluster.Register(context.Background(), kv, &cluster.InstanceInfo{
		Name: "benchtest-dead", Node: "n", Hostname: "benchtest-remote",
		Addr: "127.0.0.1:1", LastHeartbeat: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	f = benchTestClusterFlags()
	if err := benchTestRunE(t, benchClusterCmd, f); err == nil ||
		!strings.Contains(err.Error(), "run error") {
		t.Fatalf("dead-instance err=%v", err)
	}
}

// TestBenchClusterCmdRoundTrip 端到端：内存 KV 注册进程内真实实例，
// 跑 write（含 index 落 KV）→ read（-preload 预热 RouteCache）→ delete。
func TestBenchClusterCmdRoundTrip(t *testing.T) {
	benchTestGlobals(t, "127.0.0.1:2379", "benchtest")
	kv := cluster.NewMemoryKV()
	benchTestStubKVConnect(t, kv, nil)
	addr := benchTestRPCServer(t)
	now := time.Now().Unix()
	if err := cluster.Register(context.Background(), kv, &cluster.InstanceInfo{
		Name: "benchtest-srv", Node: "benchtest-node", Hostname: "benchtest-remote-host",
		Addr: addr, Addrs: []string{addr},
		Capacity: 1 << 40, Available: 1 << 39, Used: 1 << 30,
		StartTime: now, LastHeartbeat: now,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	const count = 3
	f := benchTestClusterFlags()
	f["mode"], f["count"], f["size"] = "write", "3", "4096"
	f["latency"], f["report-interval"] = "true", "1ms"
	f["cpuprofile"] = filepath.Join(t.TempDir(), "cluster-cpu.prof")
	if err := benchTestRunE(t, benchClusterCmd, f); err != nil {
		t.Fatalf("cluster write: %v", err)
	}
	benchTestWaitIndex(t, kv, count)

	// 读：-preload 覆盖 PreloadRoute 分支（索引已落 KV → 热缓存命中）。
	f["mode"], f["preload"], f["cpuprofile"] = "read", "true", ""
	if err := benchTestRunE(t, benchClusterCmd, f); err != nil {
		t.Fatalf("cluster read: %v", err)
	}

	// 删：走索引定位实例。
	f["mode"], f["preload"] = "delete", "false"
	if err := benchTestRunE(t, benchClusterCmd, f); err != nil {
		t.Fatalf("cluster delete: %v", err)
	}

	// 读已删 key：索引/缓存均 miss → 回源闭包（os.ErrNotExist）→ run error。
	f["mode"], f["preload"] = "read", "false"
	if err := benchTestRunE(t, benchClusterCmd, f); err == nil ||
		!strings.Contains(err.Error(), "run error") {
		t.Fatalf("cluster read miss err=%v", err)
	}

	// 多线程 + round-robin 写路由（覆盖 -write-routing 参数传递）。
	f["mode"], f["write-routing"], f["threads"], f["keys-prefix"] = "write", "round-robin", "2", "cbenchtest2"
	if err := benchTestRunE(t, benchClusterCmd, f); err != nil {
		t.Fatalf("cluster round-robin write: %v", err)
	}
}
