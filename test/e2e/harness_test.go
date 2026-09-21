//go:build e2e

package e2e

// L1 端到端测试 harness：起真实 `taihu server`（真裸盘块设备 + 真 TiKV），并通过
// 直连客户端（internal/rpcclient）与集群 SDK（pkg/taihu-client）两条路径驱动。
//
// 环境变量（在集群节点上跑；未设 E2E_PD 时全部用例 t.Skip，不会误报失败）：
//
//	E2E_PD        TiKV PD 地址列表（逗号分隔），如 100.71.128.12:2379。必需。
//	E2E_BIN       taihu 二进制路径；缺省在 E2E_WORKDIR 下 `go build ./cmd/taihu`（仅构建一次）。
//	E2E_WORKDIR   用例工作目录根；缺省 /var/tmp/e2e-taihu（必须是本机真实文件系统，
//	              勿用 /tmp —— 许多节点 /tmp 是 tmpfs，O_DIRECT 打不开）。
//	E2E_DEV       复用给定块设备（如 /dev/loop9）；缺省每个 server 用 losetup 把
//	              E2E_WORKDIR 下的稀疏文件挂成独立 loop 块设备（真块设备语义：
//	              BLKGETSIZE64 / O_DIRECT / O_EXCL 均可用）。
//	E2E_DEV_SIZE  稀疏镜像大小（字节），缺省 16GiB（= 2 个 8GiB segment）。
//
// 隔离策略：TiKV 与本集群其它 taihu 实例共用，故
//   - 实例名 / 业务 key 前缀 / 工作目录全部带本次随机 tag；
//   - 客户端视角的 KV 用 scopedKV 包一层，把实例清单与索引区扫描限制在本 tag 内，
//     使 SDK 看不到（也不会写坏）同集群的其它实例；
//   - 用例结束（t.Cleanup）停 server、删注册/容量/索引记录、卸载 loop、删工作目录。
//
// 为什么必须有 scopedKV：客户端 instanceRegistry 的 Scan 覆盖整个 /taihu/instances/
// 前缀，不做隔离就会把集群里生产实例一并纳入选路（写路由/水位/跨节点断言全部失真）。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/rpcclient"
	taihuclient "github.com/liucxer/taihu/pkg/taihu-client"
)

const (
	defaultWorkdir = "/var/tmp/e2e-taihu"
	defaultDevSize = int64(16) << 30 // 2 × 8GiB segment
	// serverReadyTimeout 从进程启动到「TiKV 注册可见 + TCP 可拨通」的最长等待。
	serverReadyTimeout = 90 * time.Second
)

// ---------------------------------------------------------------- 环境

func envStr(key string) string { return strings.TrimSpace(os.Getenv(key)) }

// pdAddrs 解析 E2E_PD；未设置直接跳过（e2e 依赖真实 TiKV，无则不该误报失败）。
func pdAddrs(t *testing.T) []string {
	t.Helper()
	raw := envStr("E2E_PD")
	if raw == "" {
		t.Skip("E2E_PD 未设置：e2e 需要真实 TiKV PD，例如 E2E_PD=100.71.128.12:2379")
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Fatalf("E2E_PD=%q 解析后为空", raw)
	}
	return out
}

func workdirRoot(t *testing.T) string {
	t.Helper()
	if d := envStr("E2E_WORKDIR"); d != "" {
		return d
	}
	return defaultWorkdir
}

func devSize(t *testing.T) int64 {
	t.Helper()
	if s := envStr("E2E_DEV_SIZE"); s != "" {
		var n int64
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
			t.Fatalf("E2E_DEV_SIZE=%q 非法", s)
		}
		return n
	}
	return defaultDevSize
}

// ---------------------------------------------------------------- 二进制（进程内构建一次）

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// taihuBin 返回 taihu CLI 二进制路径：E2E_BIN 优先，否则构建一次并缓存。
func taihuBin(t *testing.T) string {
	t.Helper()
	if b := envStr("E2E_BIN"); b != "" {
		if _, err := os.Stat(b); err != nil {
			t.Fatalf("E2E_BIN=%s 不可用: %v", b, err)
		}
		return b
	}
	binOnce.Do(func() { binPath, binErr = buildTaihu() })
	if binErr != nil {
		t.Fatalf("构建 taihu 失败: %v", binErr)
	}
	return binPath
}

// repoRoot 由测试进程 cwd（<repo>/test/e2e）反推模块根。
func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(wd, "..", "..")), nil
}

func buildTaihu() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	out := filepath.Join(defaultWorkdir, "taihu-e2e")
	if err := os.MkdirAll(defaultWorkdir, 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("go", "build", "-o", out, "./cmd/taihu")
	cmd.Dir = root
	cmd.Env = os.Environ()
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build ./cmd/taihu: %v\n%s", err, b)
	}
	return out, nil
}

// ---------------------------------------------------------------- 真实 TiKV KV

var (
	kvOnce      sync.Once
	kvShared    cluster.KV
	kvSharedErr error
)

// rawKV 进程内共享一条真实 TiKV 连接（用例自身不关，进程退出即释放）。
func rawKV(t *testing.T) cluster.KV {
	t.Helper()
	pd := pdAddrs(t)
	kvOnce.Do(func() {
		kvShared, kvSharedErr = cluster.NewTiKVKV(context.Background(), pd, cluster.TLSConfig{})
	})
	if kvSharedErr != nil {
		t.Fatalf("连接 TiKV PD(%v) 失败: %v", pd, kvSharedErr)
	}
	return kvShared
}

// scopedKV 把实例注册区与索引区的扫描范围收敛到本次 tag 前缀内。
// 只用于「客户端视角」；对注册区的直接断言走 rawKV。
type scopedKV struct {
	inner      cluster.KV
	namePrefix string // 实例名前缀（唯一化）
	keyPrefix  string // 业务 key 前缀（唯一化）
}

// scope 一段允许访问的键区间 [start, end) 及其触发前缀 parent。
type kvScope struct {
	parent string
	start  []byte
	end    []byte
}

func (k *scopedKV) scopes() []kvScope {
	return []kvScope{
		{
			parent: cluster.InstanceKeyPrefix,
			start:  []byte(cluster.InstanceKeyPrefix + k.namePrefix),
			end:    []byte(cluster.InstanceKeyPrefix + k.namePrefix + "\xff"),
		},
		{
			parent: cluster.IndexKeyPrefix,
			start:  []byte(cluster.IndexKeyPrefix + k.keyPrefix),
			end:    []byte(cluster.IndexKeyPrefix + k.keyPrefix + "\xff"),
		},
	}
}

// narrow 把 [start, end) 与本次 tag 的允许区间求交（而非整体替换）：调用方自己的
// 二级前缀（如 ListIndexKeys 的 prefix）必须保留，否则会把整个 tag 范围一并返回。
// 完全落在允许区间之外时返回空区间。
func (k *scopedKV) narrow(start, end []byte) ([]byte, []byte) {
	for _, sc := range k.scopes() {
		if !strings.HasPrefix(string(start), sc.parent) {
			continue
		}
		s, e := start, end
		if bytes.Compare(s, sc.start) < 0 {
			s = sc.start
		}
		if len(e) == 0 || bytes.Compare(e, sc.end) > 0 {
			e = sc.end
		}
		if bytes.Compare(s, e) >= 0 {
			return sc.start, sc.start // 空区间
		}
		return s, e
	}
	return start, end
}

func (k *scopedKV) Put(ctx context.Context, key, value []byte) error {
	return k.inner.Put(ctx, key, value)
}
func (k *scopedKV) Get(ctx context.Context, key []byte) ([]byte, error) {
	return k.inner.Get(ctx, key)
}
func (k *scopedKV) Delete(ctx context.Context, key []byte) error { return k.inner.Delete(ctx, key) }
func (k *scopedKV) DeleteRange(ctx context.Context, start, end []byte) error {
	s, e := k.narrow(start, end)
	return k.inner.DeleteRange(ctx, s, e)
}
func (k *scopedKV) Scan(ctx context.Context, start, end []byte, limit int) ([][]byte, [][]byte, error) {
	s, e := k.narrow(start, end)
	return k.inner.Scan(ctx, s, e, limit)
}
func (k *scopedKV) BatchPut(ctx context.Context, kvs map[string][]byte) error {
	return k.inner.BatchPut(ctx, kvs)
}
func (k *scopedKV) BatchGet(ctx context.Context, keys [][]byte) ([][]byte, error) {
	return k.inner.BatchGet(ctx, keys)
}

// Close 是空实现：底层 rawKV 为进程级共享连接，不随单个用例关闭。
func (k *scopedKV) Close() error { return nil }

var _ cluster.KV = (*scopedKV)(nil)

// ---------------------------------------------------------------- harness

type harness struct {
	t         *testing.T
	ctx       context.Context
	pd        []string
	bin       string
	dir       string // 本次用例独占工作目录
	tag       string
	nameBase  string // 实例名前缀（含 tag）
	keyPrefix string // 业务 key 前缀（含 tag）
	raw       cluster.KV
	kv        *scopedKV
	servers   []*testServer
	loops     []string
}

// newHarness 准备一次用例的隔离环境，并注册全部清理动作。
func newHarness(t *testing.T) *harness {
	t.Helper()
	pd := pdAddrs(t)
	raw := rawKV(t)
	tag := randHex(t, 8)
	dir := filepath.Join(workdirRoot(t), "e2e-"+tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	h := &harness{
		t: t, ctx: context.Background(), pd: pd, bin: taihuBin(t), dir: dir, tag: tag,
		nameBase: "e2e-" + tag + "-", keyPrefix: "e2e-" + tag + "/", raw: raw,
	}
	h.kv = &scopedKV{inner: raw, namePrefix: h.nameBase, keyPrefix: h.keyPrefix}
	t.Cleanup(h.cleanup)
	return h
}

// key 给用例内业务 key 加上本次唯一前缀（同时保证索引清理可整段删除）。
func (h *harness) key(name string) string { return h.keyPrefix + name }

// cleanup 停 server → 清 TiKV 残留 → 卸 loop → 删工作目录。
func (h *harness) cleanup() {
	for _, s := range h.servers {
		s.kill()
		_ = os.Remove(s.shmPath())
	}
	ctx := context.Background()
	// 索引区：整段删本次 key 前缀。
	_ = h.raw.DeleteRange(ctx,
		[]byte(cluster.IndexKeyPrefix+h.keyPrefix),
		[]byte(cluster.IndexKeyPrefix+h.keyPrefix+"\xff"))
	// 客户端注册区：删本次 SDK 客户端。
	_ = h.raw.DeleteRange(ctx,
		[]byte(cluster.ClientKeyPrefix+"e2e-"+h.tag),
		[]byte(cluster.ClientKeyPrefix+"e2e-"+h.tag+"\xff"))
	// 实例注册 + 容量记录：按本次前缀兜底清扫（覆盖被 kill -9 残留的记录）。
	start, end := cluster.InstanceScanRange()
	if ks, vs, err := h.raw.Scan(ctx, start, end, 0); err == nil {
		for i := range ks {
			var info cluster.InstanceInfo
			if json.Unmarshal(vs[i], &info) != nil || !strings.HasPrefix(info.Name, h.nameBase) {
				continue
			}
			_ = h.raw.Delete(ctx, ks[i])
			_ = h.raw.Delete(ctx, cluster.CapacityKey(info.Name))
		}
	}
	for _, dev := range h.loops {
		if out, err := exec.Command("losetup", "-d", dev).CombinedOutput(); err != nil {
			h.t.Logf("losetup -d %s: %v (%s)", dev, err, out)
		}
	}
	if err := os.RemoveAll(h.dir); err != nil {
		h.t.Logf("RemoveAll %s: %v", h.dir, err)
	}
}

// attachLoop 把稀疏文件挂成独立 loop 块设备（真块设备语义）。
func (h *harness) attachLoop(size int64, label string) string {
	h.t.Helper()
	img := filepath.Join(h.dir, label+".img")
	f, err := os.Create(img)
	if err != nil {
		h.t.Fatalf("create %s: %v", img, err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		h.t.Fatalf("truncate %s to %d: %v", img, size, err)
	}
	if err := f.Close(); err != nil {
		h.t.Fatalf("close %s: %v", img, err)
	}
	out, err := exec.Command("losetup", "-f", "--show", img).CombinedOutput()
	if err != nil {
		h.t.Fatalf("losetup %s: %v (%s)", img, err, out)
	}
	dev := strings.TrimSpace(string(out))
	if dev == "" {
		h.t.Fatalf("losetup %s 未返回设备名", img)
	}
	h.loops = append(h.loops, dev)
	return dev
}

// newSDK 构建集群 SDK 客户端（KV 为 scopedKV，只看得见本次实例）。
func (h *harness) newSDK(mut func(*taihuclient.ClusterConfig)) *taihuclient.Storage {
	h.t.Helper()
	cfg := taihuclient.ClusterConfig{
		KV:               h.kv,
		ClientName:       "e2e-" + h.tag,
		RefreshInterval:  200 * time.Millisecond,
		HeartbeatTimeout: 3 * time.Second,
		Conns:            1,
	}
	if mut != nil {
		mut(&cfg)
	}
	st, err := taihuclient.NewCluster(cfg)
	if err != nil {
		h.t.Fatalf("taihuclient.NewCluster: %v", err)
	}
	h.t.Cleanup(func() { _ = st.Close() })
	return st
}

// dialDirect 直连某 server 的数据面（TCP），返回直连客户端。
func (h *harness) dialDirect(s *testServer, conns int) *rpcclient.Storage {
	h.t.Helper()
	c, err := rpcclient.DialPool(h.ctx, s.info.Addr, conns)
	if err != nil {
		h.t.Fatalf("DialPool %s: %v", s.info.Addr, err)
	}
	h.t.Cleanup(func() { _ = c.Close() })
	return c
}

// ---------------------------------------------------------------- server 生命周期

type serverOpts struct {
	name    string   // 完整实例名；缺省 <nameBase>0
	listen  []string // 监听 IP 列表；缺省 ["127.0.0.1"]
	dev     string   // 复用既有块设备（重启用例）；缺省新建 loop
	devSize int64    // 新建 loop 的大小；缺省 16GiB
	dbDir   string   // 复用既有 pebble 目录（重启用例）
	ioUring string   // "on"/"off"/"auto"；空=不显式传（server 默认 auto）
	extra   []string // 追加参数
}

type testServer struct {
	h       *harness
	name    string
	dev     string
	dbDir   string
	logPath string
	logFile *os.File
	cmd     *exec.Cmd
	done    chan struct{}
	exitErr error
	info    cluster.InstanceInfo
	killed  bool
}

func (s *testServer) shmPath() string { return "/dev/" + s.name }

// startServer 启动一个真实 `taihu server` 并等它就绪（TiKV 注册可见 + TCP 可拨通）。
func (h *harness) startServer(o serverOpts) *testServer {
	h.t.Helper()
	if o.name == "" {
		o.name = h.nameBase + "0"
	}
	if len(o.listen) == 0 {
		o.listen = []string{"127.0.0.1"}
	}
	s := &testServer{h: h, name: o.name}
	if o.dev != "" {
		s.dev = o.dev
	} else if dev := envStr("E2E_DEV"); dev != "" {
		s.dev = dev
	} else {
		size := o.devSize
		if size == 0 {
			size = devSize(h.t)
		}
		s.dev = h.attachLoop(size, o.name)
	}
	if o.dbDir != "" {
		s.dbDir = o.dbDir
	} else {
		s.dbDir = filepath.Join(h.dir, o.name+"-db")
		if err := os.MkdirAll(s.dbDir, 0o755); err != nil {
			h.t.Fatalf("mkdir %s: %v", s.dbDir, err)
		}
	}
	s.logPath = filepath.Join(h.dir, o.name+".log")
	lf, err := os.Create(s.logPath)
	if err != nil {
		h.t.Fatalf("create log %s: %v", s.logPath, err)
	}
	s.logFile = lf

	args := []string{
		"server",
		"--listen", strings.Join(o.listen, ","),
		"--db", s.dbDir,
		"--dev", s.dev,
		"--server-name", s.name,
		"--pd", strings.Join(h.pd, ","),
	}
	if o.ioUring != "" {
		args = append(args, "--io-uring", o.ioUring)
	}
	args = append(args, o.extra...)

	cmd := exec.Command(h.bin, args...)
	cmd.Dir = h.dir
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start %s: %v", h.bin, err)
	}
	s.cmd = cmd
	s.done = make(chan struct{})
	go func() {
		s.exitErr = cmd.Wait()
		close(s.done)
	}()
	h.servers = append(h.servers, s)
	s.info = s.waitReady(serverReadyTimeout)
	h.t.Logf("server %s ready: addr=%s shm=%s dev=%s", s.name, s.info.Addr, s.info.ShmAddr, s.dev)
	return s
}

// waitReady 轮询注册记录 + TCP 拨号，直到就绪；进程提前退出则带日志失败。
func (s *testServer) waitReady(timeout time.Duration) cluster.InstanceInfo {
	s.h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-s.done:
			s.h.t.Fatalf("server %s 启动即退出: %v\n--- log ---\n%s", s.name, s.exitErr, s.logTail())
		default:
		}
		if info, err := cluster.GetInstance(s.h.ctx, s.h.raw, s.name); err == nil && info != nil && info.Addr != "" {
			if c, derr := net.DialTimeout("tcp", info.Addr, time.Second); derr == nil {
				_ = c.Close()
				return *info
			}
		}
		if time.Now().After(deadline) {
			s.h.t.Fatalf("server %s 在 %s 内未就绪\n--- log tail ---\n%s", s.name, timeout, s.logTail())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// exitCode 返回进程退出码（仅已退出时有效）。
func (s *testServer) exitCode() int {
	if s.cmd == nil || s.cmd.ProcessState == nil {
		return -1
	}
	return s.cmd.ProcessState.ExitCode()
}

// term 发 SIGTERM 并等进程退出（优雅停机：注销注册 + 摘除 shm socket）。
func (s *testServer) term() {
	s.h.t.Helper()
	s.signal(syscall.SIGTERM, 30*time.Second)
}

// kill 发 SIGKILL 并等进程退出（模拟崩溃；注册记录与 shm socket 均残留）。
func (s *testServer) kill() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	s.signal(syscall.SIGKILL, 15*time.Second)
}

func (s *testServer) signal(sig syscall.Signal, wait time.Duration) {
	if s.killed {
		return
	}
	s.killed = true
	select {
	case <-s.done: // 已退出
		_ = s.logFile.Close()
		return
	default:
	}
	_ = s.cmd.Process.Signal(sig)
	select {
	case <-s.done:
	case <-time.After(wait):
		s.h.t.Errorf("server %s 在收到 %v 后 %s 内未退出，强杀", s.name, sig, wait)
		_ = s.cmd.Process.Kill()
		<-s.done
	}
	_ = s.logFile.Close()
}

// logTail 读日志尾部（失败诊断）。
func (s *testServer) logTail() string {
	b, err := os.ReadFile(s.logPath)
	if err != nil {
		return "(无法读取日志 " + s.logPath + ": " + err.Error() + ")"
	}
	const max = 6000
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(b)
}

// ---------------------------------------------------------------- 通用断言/数据工具

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// randBytes 生成不可压缩随机内容（每字节近似均匀），避免零填充/可压缩内容掩盖截断类 bug。
func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	return b
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pattern 生成确定性内容：同一 seed + 长度可重复计算，用于区间读「按子区间重算期望值」。
func pattern(seed string, n int) []byte {
	out := make([]byte, 0, n)
	var ctr uint64
	for len(out) < n {
		ctr++
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", seed, ctr)))
		out = append(out, sum[:]...)
	}
	return out[:n]
}

// store 是被测对象存储的最小接口；直连客户端与集群 SDK 都满足。
type store interface {
	Put(ctx context.Context, key string, size int64, in []byte) error
	Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error)
	Delete(ctx context.Context, key string) error
	Stat(ctx context.Context, key string) (int64, error)
}

// getExact 读整对象并按 sha256 + 逐字节双重校验。
// size 传 -1（读至结尾），同时校验 Stat 一致。
func getExact(t *testing.T, st store, key string, want []byte) {
	t.Helper()
	ctx := context.Background()
	n, err := st.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat(%s): %v", key, err)
	}
	if n != int64(len(want)) {
		t.Fatalf("Stat(%s) = %d, want %d", key, n, len(want))
	}
	got, rel, err := st.Get(ctx, key, 0, -1)
	if err != nil {
		t.Fatalf("Get(%s, 0, -1): %v", key, err)
	}
	defer rel()
	if sha256Hex(got) != sha256Hex(want) {
		t.Fatalf("Get(%s) 内容不一致: len=%d/%d sha=%s/%s", key, len(got), len(want), sha256Hex(got), sha256Hex(want))
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Get(%s) 逐字节不一致（sha 相同但内容不同？）", key)
	}
}

// getRange 读 [off, off+size) 并与 want 的对应子区间比对。
func getRange(t *testing.T, st store, key string, off, size int64, want []byte) {
	t.Helper()
	ctx := context.Background()
	if off > int64(len(want)) {
		t.Fatalf("用例错误：off=%d 超出 want 长度 %d", off, len(want))
	}
	exp := want[off:]
	if size >= 0 {
		if off+size > int64(len(want)) {
			t.Fatalf("用例错误：off+size=%d 超出 want 长度 %d", off+size, len(want))
		}
		exp = want[off : off+size]
	}
	got, rel, err := st.Get(ctx, key, off, size)
	if err != nil {
		t.Fatalf("Get(%s, %d, %d): %v", key, off, size, err)
	}
	defer rel()
	if !bytes.Equal(got, exp) {
		t.Fatalf("Get(%s, %d, %d) 内容不一致: got len=%d want len=%d\ngot  sha=%s\nwant sha=%s",
			key, off, size, len(got), len(exp), sha256Hex(got), sha256Hex(exp))
	}
}

// putExact 写入并立即整对象校验（走 store 的 Put/Stat/Get 全链路）。
func putExact(t *testing.T, st store, key string, data []byte) {
	t.Helper()
	if err := st.Put(context.Background(), key, int64(len(data)), data); err != nil {
		t.Fatalf("Put(%s, size=%d): %v", key, len(data), err)
	}
	getExact(t, st, key, data)
}

// waitFor 轮询 cond 直到为真；超时用 format+args 描述失败原因。
func waitFor(t *testing.T, timeout time.Duration, format string, cond func() bool, args ...interface{}) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待超时(%s): %s", timeout, fmt.Sprintf(format, args...))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// liveInstances 从注册区读本次实例（按前缀过滤）并附带存活判定。
func (h *harness) liveInstances() []cluster.InstanceInfo {
	start, end := cluster.InstanceScanRange()
	ks, vs, err := h.raw.Scan(h.ctx, start, end, 0)
	if err != nil {
		h.t.Fatalf("scan instances: %v", err)
	}
	now := time.Now()
	var out []cluster.InstanceInfo
	for i := range ks {
		var info cluster.InstanceInfo
		if json.Unmarshal(vs[i], &info) != nil || !strings.HasPrefix(info.Name, h.nameBase) {
			continue
		}
		if info.Aliveness(now, 3*time.Second) {
			out = append(out, info)
		}
	}
	return out
}

// instanceByName 读单个注册记录（不判存活）；不存在返回 nil。
func (h *harness) instanceByName(name string) *cluster.InstanceInfo {
	info, err := cluster.GetInstance(h.ctx, h.raw, name)
	if err != nil {
		h.t.Fatalf("GetInstance(%s): %v", name, err)
	}
	return info
}
