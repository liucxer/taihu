//go:build e2e

package e2e

// C 组：注册 / 选路面（实例注册、心跳摘除、容量记录、优雅停机、写路由、KV 不可达降级）。
//
// 全部 server 由 harness 起（真裸盘 loop 设备 + 真 TiKV）。对注册区的直接断言走 h.raw
// （不经 scopedKV 收缩）；客户端视角走 h.newSDK。
//
// C4 说明：RouteLocal 需「本地分组只剩一个实例」才可观察，而单节点 harness 上两个实例
// 的 hostname 必然相同（都=本机）。故用 cHostRewriteKV 把其中一个实例的注册 Hostname
// 改写为伪造远端值，使 SDK 将其归入 remote 分组（数据面地址/传输不变，仅影响选路分组），
// 从而在单节点上忠实复现「本地优先 vs 轮询」的差异。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
	taihuclient "github.com/liucxer/taihu/pkg/taihu-client"
)

// ---------------------------------------------------------------- C5 用：KV 全故障包装

// cErrKVDown 模拟 TiKV/PD 完全不可达时的错误。
var cErrKVDown = errors.New("e2e: kv unavailable (simulated pd down)")

// cDownKV 模拟 TiKV/PD 完全不可达的 cluster.KV：读写/扫描全部返回错误，Close 正常。
type cDownKV struct{}

func (cDownKV) Put(context.Context, []byte, []byte) error { return cErrKVDown }
func (cDownKV) Get(context.Context, []byte) ([]byte, error) {
	return nil, cErrKVDown
}
func (cDownKV) Delete(context.Context, []byte) error              { return cErrKVDown }
func (cDownKV) DeleteRange(context.Context, []byte, []byte) error { return cErrKVDown }
func (cDownKV) Scan(context.Context, []byte, []byte, int) ([][]byte, [][]byte, error) {
	return nil, nil, cErrKVDown
}
func (cDownKV) BatchPut(context.Context, map[string][]byte) error { return cErrKVDown }
func (cDownKV) BatchGet(context.Context, [][]byte) ([][]byte, error) {
	return nil, cErrKVDown
}
func (cDownKV) Close() error { return nil }

var _ cluster.KV = cDownKV{}

// ---------------------------------------------------------------- C4 用：hostname 改写包装

// cHostRewriteKV 包装 cluster.KV，在扫描实例注册区时把 name 实例的 Hostname 改写为
// hostname，使 SDK 将该实例归入 remote 分组（单节点上复现多节点本地优先语义）。
// 其余方法（含索引区 Scan）原样透传。
type cHostRewriteKV struct {
	cluster.KV
	name     string
	hostname string
}

func (k cHostRewriteKV) Scan(ctx context.Context, start, end []byte, limit int) ([][]byte, [][]byte, error) {
	ks, vs, err := k.KV.Scan(ctx, start, end, limit)
	if err != nil {
		return ks, vs, err
	}
	for i := range ks {
		if !strings.HasPrefix(string(ks[i]), cluster.InstanceKeyPrefix) {
			continue
		}
		var info cluster.InstanceInfo
		if json.Unmarshal(vs[i], &info) != nil || info.Name != k.name {
			continue
		}
		info.Hostname = k.hostname
		if b, merr := json.Marshal(info); merr == nil {
			vs[i] = b
		}
	}
	return ks, vs, nil
}

var _ cluster.KV = cHostRewriteKV{}

// ---------------------------------------------------------------- C 组工具

// cIndexName 读 key→实例 索引（/taihu/index/<key> 的值为实例名）；不存在返回 ok=false。
func cIndexName(t *testing.T, h *harness, key string) (string, bool) {
	t.Helper()
	v, err := h.raw.Get(context.Background(), cluster.IndexKey(key))
	if err != nil {
		t.Fatalf("读索引 %s: %v", key, err)
	}
	if v == nil {
		return "", false
	}
	return string(v), true
}

// cIndexOwners 统计 keys 的索引归属（实例名 → 条数）。索引异步批量写，调用方需先轮询就绪。
func cIndexOwners(t *testing.T, h *harness, keys []string) map[string]int {
	t.Helper()
	out := make(map[string]int, len(keys))
	for _, k := range keys {
		if name, ok := cIndexName(t, h, k); ok {
			out[name]++
		}
	}
	return out
}

// cIndexCount 统计 keys 中已落索引的条数。
func cIndexCount(t *testing.T, h *harness, keys []string) int {
	t.Helper()
	n := 0
	for _, k := range keys {
		if _, ok := cIndexName(t, h, k); ok {
			n++
		}
	}
	return n
}

// cLoopCapacityBytes 取块设备字节容量（与 server 的 deviceCapacity 同源）；
// blockdev 不可用时退回 harness 建 loop 镜像时用的 devSize。
func cLoopCapacityBytes(t *testing.T, dev string) int64 {
	t.Helper()
	out, err := exec.Command("blockdev", "--getsize64", dev).Output()
	if err != nil {
		return devSize(t)
	}
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n); err != nil || n <= 0 {
		t.Fatalf("解析 blockdev --getsize64 %s 输出 %q 失败: %v", dev, out, err)
	}
	return n
}

// cServerArgs 构造与 harness.startServer 一致的 `taihu server` 参数（长 flag 用 --）。
func cServerArgs(h *harness, name, dev, dbDir string) []string {
	return []string{
		"server",
		"--listen", "127.0.0.1",
		"--db", dbDir,
		"--dev", dev,
		"--server-name", name,
		"--pd", strings.Join(h.pd, ","),
	}
}

// ---------------------------------------------------------------- C1

// TestC1RegistryHeartbeatAndEviction 多实例注册 + 心跳 + 摘除：
// 两实例注册可见且 SDK 能看到两个；SIGKILL 其一后注册记录残留，SDK 依赖心跳超时把
// 它从快照摘除（写入不再路由到它），另一实例读写仍正常。
func TestC1RegistryHeartbeatAndEviction(t *testing.T) {
	h := newHarness(t)
	a := h.startServer(serverOpts{name: h.nameBase + "a"})
	b := h.startServer(serverOpts{name: h.nameBase + "b"})

	waitFor(t, 20*time.Second, "两个实例注册可见（a=%s b=%s）", func() bool {
		return len(h.liveInstances()) == 2
	}, a.name, b.name)

	// 短心跳超时（2s）便于快速观察摘除；round-robin 保证两个实例都会被选中。
	stRR := h.newSDK(func(cfg *taihuclient.ClusterConfig) {
		cfg.WriteRouting = taihuclient.RouteRoundRobin
		cfg.HeartbeatTimeout = 2 * time.Second
		cfg.RefreshInterval = 200 * time.Millisecond
	})
	waitFor(t, 15*time.Second, "SDK 发现在线实例", func() bool { return stRR.HasLive() })

	data := []byte("c1-payload")
	preKeys := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		k := h.key(fmt.Sprintf("c1/pre-%02d", i))
		if err := stRR.Put(h.ctx, k, int64(len(data)), data); err != nil {
			t.Fatalf("SDK Put(%s): %v", k, err)
		}
		preKeys = append(preKeys, k)
	}
	// 索引异步批量写（100ms 周期）：轮询直到两个实例名都出现 → SDK 确实看到 2 个实例。
	waitFor(t, 15*time.Second, "SDK 选路覆盖两个实例（a=%s b=%s）", func() bool {
		owners := cIndexOwners(t, h, preKeys)
		return owners[a.name] > 0 && owners[b.name] > 0
	}, a.name, b.name)

	// SIGKILL b：心跳停摆，注册记录残留（TiKV 无主动删除）。
	b.kill()
	if h.instanceByName(b.name) == nil {
		t.Fatalf("SIGKILL 后实例 %s 的注册记录应残留，却已消失", b.name)
	}

	// 摘除判定：round-robin 在 b 仍处快照时每隔一次选到已死 b → 写 shm 失败。
	// 故「连续 6 次写全部成功」等价于 b 已被心跳超时摘除。
	var round int
	var roundKeys []string
	waitFor(t, 30*time.Second, "SDK 心跳超时摘除被 kill 的实例 %s", func() bool {
		round++
		roundKeys = roundKeys[:0]
		for i := 0; i < 6; i++ {
			k := h.key(fmt.Sprintf("c1/post-%d-%02d", round, i))
			ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
			err := stRR.Put(ctx, k, int64(len(data)), data)
			cancel()
			if err != nil {
				return false
			}
			roundKeys = append(roundKeys, k)
		}
		return true
	}, b.name)

	// 本轮 key 全部锚定存活实例 a（b 不再被路由）。
	waitFor(t, 15*time.Second, "索引区确认本轮 key 全部锚定 %s", func() bool {
		for _, k := range roundKeys {
			if n, ok := cIndexName(t, h, k); !ok || n != a.name {
				return false
			}
		}
		return true
	}, a.name)

	// 注册区仍残留 b（靠心跳超时摘除，而非删除）。
	if h.instanceByName(b.name) == nil {
		t.Fatalf("b 的注册记录不应被删除（应靠心跳超时摘除）: %s", b.name)
	}
	waitFor(t, 10*time.Second, "存活实例仅剩 %s", func() bool {
		live := h.liveInstances()
		return len(live) == 1 && live[0].Name == a.name
	}, a.name)

	// 存活实例 a 的读写仍正常（新起 SDK，初始刷新即已摘除 b）。
	st := h.newSDK(func(cfg *taihuclient.ClusterConfig) {
		cfg.HeartbeatTimeout = 2 * time.Second
		cfg.RefreshInterval = 200 * time.Millisecond
	})
	waitFor(t, 15*time.Second, "SDK 发现存活实例 %s", func() bool { return st.HasLive() }, a.name)
	payload := randBytes(t, 4097)
	putExact(t, st, h.key("c1/after"), payload)
	getExact(t, st, h.key("c1/after"), payload)
}

// ---------------------------------------------------------------- C2

// TestC2CapacityRecordAndTamperRejectsStart 容量记录：
// 正常启动写入的容量记录 CapacityBytes == loop 设备容量；篡改记录后用同一 --server-name
// 再起进程必须拒绝启动，日志含 "capacity changed"。
func TestC2CapacityRecordAndTamperRejectsStart(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})

	rec, ok, err := cluster.GetCapacity(h.ctx, h.raw, srv.name)
	if err != nil {
		t.Fatalf("GetCapacity(%s): %v", srv.name, err)
	}
	if !ok {
		t.Fatalf("实例 %s 启动后应写入容量记录，实际不存在", srv.name)
	}
	wantCap := cLoopCapacityBytes(t, srv.dev)
	if rec.CapacityBytes != wantCap {
		t.Fatalf("容量记录 CapacityBytes=%d, 期望 loop 设备容量 %d（dev=%s）", rec.CapacityBytes, wantCap, srv.dev)
	}
	if rec.SegmentSizeBytes <= 0 || rec.SegmentCount != rec.CapacityBytes/rec.SegmentSizeBytes {
		t.Fatalf("容量记录段字段不自洽: %+v", rec)
	}
	if rec.ListenAddr != srv.info.Addr {
		t.Fatalf("容量记录 ListenAddr=%q, 期望 %q", rec.ListenAddr, srv.info.Addr)
	}

	// 篡改容量记录（明显不同），随后用同一 --server-name 再起一次。
	tampered := rec
	tampered.CapacityBytes = rec.CapacityBytes + (1 << 30)
	tampered.SegmentCount = tampered.CapacityBytes / tampered.SegmentSizeBytes
	tampered.UpdateTime = time.Now().Unix()
	if err := cluster.PutCapacity(h.ctx, h.raw, srv.name, tampered); err != nil {
		t.Fatalf("PutCapacity(tampered): %v", err)
	}
	defer func() { // 测完恢复原始容量记录，避免污染
		if err := cluster.PutCapacity(h.ctx, h.raw, srv.name, rec); err != nil {
			t.Errorf("恢复容量记录失败: %v", err)
		}
	}()

	dbDir := filepath.Join(h.dir, srv.name+"-tamper-db")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dbDir, err)
	}
	ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, cServerArgs(h, srv.name, srv.dev, dbDir)...)
	out, runErr := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("篡改容量后启动未能在 30s 内失败退出（进程仍在运行？）\n--- 输出 ---\n%s", out)
	}
	if runErr == nil {
		t.Fatalf("容量记录被篡改后启动本应失败，却成功退出\n--- 输出 ---\n%s", out)
	}
	if !strings.Contains(string(out), "capacity changed") {
		t.Fatalf("启动失败输出应含 \"capacity changed\"，实际:\n%s", out)
	}
}

// ---------------------------------------------------------------- C3

// TestC3GracefulShutdownSIGTERM 优雅停机：SIGTERM 后进程在超时内退出、退出码 0、
// 注册记录被删除、容量记录保留、无存活僵尸。
func TestC3GracefulShutdownSIGTERM(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	data := pattern("C3", 4<<20+7)
	if err := c.Put(h.ctx, h.key("c3/obj"), int64(len(data)), data); err != nil {
		t.Fatalf("写数据: %v", err)
	}
	getExact(t, c, h.key("c3/obj"), data)

	// 启动注册后 LastHeartbeat 由 1s 周期心跳刷新（首次注册时尚未赋值），等其变新鲜。
	waitFor(t, 10*time.Second, "实例 %s 心跳刷新（注册记录存在且新鲜）", func() bool {
		info := h.instanceByName(srv.name)
		return info != nil && info.Aliveness(time.Now(), 5*time.Second)
	}, srv.name)

	srv.term() // SIGTERM：优雅停机（注销注册 + 停数据面）

	if code := srv.exitCode(); code != 0 {
		t.Fatalf("SIGTERM 后退出码=%d, 期望 0\n--- 日志 ---\n%s", code, srv.logTail())
	}
	if got := h.instanceByName(srv.name); got != nil {
		t.Fatalf("优雅停机后注册记录应被删除，实际仍存在: %+v", got)
	}
	rec, ok, err := cluster.GetCapacity(h.ctx, h.raw, srv.name)
	if err != nil || !ok {
		t.Fatalf("优雅停机不应删除容量记录: ok=%v err=%v", ok, err)
	}
	if rec.CapacityBytes <= 0 {
		t.Fatalf("容量记录异常: %+v", rec)
	}
	if live := h.liveInstances(); len(live) != 0 {
		t.Fatalf("优雅停机后不应残留存活实例（僵尸）: %+v", live)
	}
}

// ---------------------------------------------------------------- C4

// TestC4WriteRoutingLocalVsRoundRobin 写路由：
// RouteLocal 全部锚定本地实例；RouteRoundRobin 在两个实例间都出现。
func TestC4WriteRoutingLocalVsRoundRobin(t *testing.T) {
	h := newHarness(t)
	a := h.startServer(serverOpts{name: h.nameBase + "a"})
	b := h.startServer(serverOpts{name: h.nameBase + "b"})
	waitFor(t, 20*time.Second, "两个实例注册可见", func() bool { return len(h.liveInstances()) == 2 })

	// 单节点上两实例 hostname 相同：把 b 改写成伪造远端，令本地分组只剩 a。
	fakeHost := "e2e-fake-remote-" + h.tag
	mkKV := func() cluster.KV { return cHostRewriteKV{KV: h.kv, name: b.name, hostname: fakeHost} }

	data := []byte("c4-payload")
	const n = 8

	stLocal := h.newSDK(func(cfg *taihuclient.ClusterConfig) {
		cfg.KV = mkKV()
		cfg.WriteRouting = taihuclient.RouteLocal
	})
	waitFor(t, 15*time.Second, "SDK 发现本地实例 %s", func() bool { return stLocal.HasLive() }, a.name)

	localKeys := make([]string, 0, n)
	for i := 0; i < n; i++ {
		k := h.key(fmt.Sprintf("c4/local-%02d", i))
		if err := stLocal.Put(h.ctx, k, int64(len(data)), data); err != nil {
			t.Fatalf("RouteLocal Put(%s): %v", k, err)
		}
		localKeys = append(localKeys, k)
	}
	waitFor(t, 15*time.Second, "RouteLocal 的 %d 个 key 索引落盘", func() bool {
		return cIndexCount(t, h, localKeys) == n
	}, n)
	owners := cIndexOwners(t, h, localKeys)
	if len(owners) != 1 || owners[a.name] != n {
		t.Fatalf("RouteLocal 应全部锚定本地实例 %s，实际分布: %v", a.name, owners)
	}

	stRR := h.newSDK(func(cfg *taihuclient.ClusterConfig) {
		cfg.KV = mkKV()
		cfg.WriteRouting = taihuclient.RouteRoundRobin
	})
	waitFor(t, 15*time.Second, "SDK 发现两个实例", func() bool { return stRR.HasLive() })

	rrKeys := make([]string, 0, n)
	for i := 0; i < n; i++ {
		k := h.key(fmt.Sprintf("c4/rr-%02d", i))
		if err := stRR.Put(h.ctx, k, int64(len(data)), data); err != nil {
			t.Fatalf("RouteRoundRobin Put(%s): %v", k, err)
		}
		rrKeys = append(rrKeys, k)
	}
	waitFor(t, 15*time.Second, "RouteRoundRobin 的 %d 个 key 索引落盘", func() bool {
		return cIndexCount(t, h, rrKeys) == n
	}, n)
	rrOwners := cIndexOwners(t, h, rrKeys)
	if rrOwners[a.name] == 0 || rrOwners[b.name] == 0 {
		t.Fatalf("RouteRoundRobin 应在两个实例间分布，实际: %v（a=%s b=%s）", rrOwners, a.name, b.name)
	}
}

// ---------------------------------------------------------------- C5

// TestC5KVUnavailableDegrades KV（TiKV/PD）不可达降级：
// HasLive()=false、Put 返回 ErrNoInstances、不 panic、Close 正常；配置 Source 时
// Get 绕过索引/注册走回源成功返回请求区间。
func TestC5KVUnavailableDegrades(t *testing.T) {
	full := pattern("C5", 4<<20+123)
	var calls int
	st, err := taihuclient.NewCluster(taihuclient.ClusterConfig{
		KV:               cDownKV{},
		ClientName:       "e2e-c5",
		RefreshInterval:  100 * time.Millisecond,
		HeartbeatTimeout: 2 * time.Second,
		Conns:            1,
		Source: func(ctx context.Context, key string) ([]byte, error) {
			calls++
			return full, nil
		},
	})
	if err != nil {
		t.Fatalf("NewCluster(KV 不可达): %v", err)
	}

	if st.HasLive() {
		t.Fatal("KV 不可达时 HasLive() 应为 false")
	}
	key := "c5/key"
	if err := st.Put(context.Background(), key, 4, []byte("abcd")); !errors.Is(err, taihuclient.ErrNoInstances) {
		t.Fatalf("KV 不可达时 Put err = %v, want ErrNoInstances", err)
	}

	// 索引/注册不可用 → lookup 失败 → 走回源：返回请求区间且不 panic。
	got, rel, err := st.Get(context.Background(), key, 4097, 4096)
	if err != nil {
		t.Fatalf("无在线实例时 Get(回源) 应成功，实际 err=%v", err)
	}
	exp := full[4097 : 4097+4096]
	if sha256Hex(got) != sha256Hex(exp) {
		rel()
		t.Fatalf("回源区间内容不一致: got len=%d want len=%d", len(got), len(exp))
	}
	rel()
	if calls != 1 {
		t.Fatalf("Source 调用次数=%d, want 1", calls)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("KV 不可达时 Close() 应正常返回，实际 err=%v", err)
	}
}
