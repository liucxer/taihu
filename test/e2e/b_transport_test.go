//go:build e2e

package e2e

// B 组：传输面（L1 端到端）。覆盖三条传输路径（auto/rpc/shm）、多 IP 监听与多地址
// 连接池、单连接内 streamID 多路复用配对、链路帧尺寸统计、shm 会话反复建断的
// 资源回收，以及 TCP 半包/慢客户端的健壮性。
//
// 本文件只新增用例，不改 harness/产品代码。进程内帧统计（internal/transport.stats）
// 是进程级全局计数器，故全部断言一律用「前后差值」，绝不假设绝对值。

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/rpcclient"
	"github.com/liucxer/taihu/internal/transport"
	"github.com/liucxer/taihu/internal/transport/protocol"
	taihuclient "github.com/liucxer/taihu/pkg/taihu-client"
)

// ---------------------------------------------------------------- 工具

// bPort 从 "ip:port" 提取端口号。
func bPort(t *testing.T, addr string) int {
	t.Helper()
	_, ps, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	p, err := strconv.Atoi(ps)
	if err != nil {
		t.Fatalf("端口 %q 非法: %v", ps, err)
	}
	return p
}

// bStatsMap 把 transport.StatsString() 解析为 "group.field" -> 值 的映射。
func bStatsMap() map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(transport.StatsString(), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		group := f[0]
		for _, kv := range f[1:] {
			p := strings.SplitN(kv, "=", 2)
			if len(p) != 2 {
				continue
			}
			n, err := strconv.ParseInt(p[1], 10, 64)
			if err != nil {
				continue
			}
			out[group+"."+p[0]] = n
		}
	}
	return out
}

// bRxStats 进程内客户端接收侧数据帧计数快照（只断言 transport-rx；tx 是服务端进程
// 计数，测试进程看不到服务端的发送统计）。
type bRxStats struct {
	frames int64 // 收到数据帧次数
	bytes  int64 // 收到数据帧负载字节
	data4M int64 // 其中负载恰为 4MiB 的整帧数
	take   int64 // 零拷贝移交命中帧数
	copy   int64 // 回退拷贝帧数
}

func bReadRxStats() bRxStats {
	m := bStatsMap()
	return bRxStats{
		frames: m["transport-rx.dataFrames"],
		bytes:  m["transport-rx.dataBytes"],
		data4M: m["transport-rx.data4MiB"],
		take:   m["transport-rx.take"],
		copy:   m["transport-rx.copy"],
	}
}

// bProcHexIPv4 解码 /proc/net/tcp 的 8 位十六进制 IPv4（字节序与点分十进制相反）。
func bProcHexIPv4(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 4 {
		return ""
	}
	return net.IPv4(b[3], b[2], b[1], b[0]).String()
}

// bEstablishedRemotes 统计本机 ESTABLISHED TCP 连接中「远端端口 == port」的对端
// "ip:port" -> 连接数。服务端监听套接字为 LISTEN、服务端侧已连接套接字的远端端口是
// 客户端临时端口，均不会被计入；故命中的对端只可能是主动连到该端口的客户端。
func bEstablishedRemotes(t *testing.T, port int) map[string]int {
	t.Helper()
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		t.Fatalf("读 /proc/net/tcp: %v", err)
	}
	out := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[0] == "sl" {
			continue
		}
		if f[3] != "01" { // 01 = TCP_ESTABLISHED
			continue
		}
		rh, rp, ok := strings.Cut(f[2], ":")
		if !ok {
			continue
		}
		rpn, err := strconv.ParseInt(rp, 16, 32)
		if err != nil || int(rpn) != port {
			continue
		}
		ip := bProcHexIPv4(rh)
		if ip == "" {
			continue
		}
		out[net.JoinHostPort(ip, strconv.Itoa(port))]++
	}
	return out
}

// bNodeIPv4 取本机一个非回环、非容器网桥的 IPv4 地址（用于多 IP 监听用例）。
func bNodeIPv4(t *testing.T) string {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	var cands []string
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		low := strings.ToLower(ifc.Name)
		skip := strings.HasPrefix(low, "docker") || strings.HasPrefix(low, "veth") ||
			strings.HasPrefix(low, "virbr") || strings.HasPrefix(low, "br-") ||
			strings.HasPrefix(low, "cni") || strings.HasPrefix(low, "flannel")
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			if skip {
				continue
			}
			cands = append(cands, ip4.String())
		}
	}
	if len(cands) == 0 {
		t.Fatalf("未找到可用于多 IP 监听的本地非回环 IPv4（net.Interfaces 结果: %+v）", ifs)
	}
	return cands[0]
}

// bPutGetVerify Put + Stat + Get 整对象并逐字节校验，返回带诊断信息的具体错误。
func bPutGetVerify(c *rpcclient.Storage, key string, data []byte) error {
	ctx := context.Background()
	if err := c.Put(ctx, key, int64(len(data)), data); err != nil {
		return fmt.Errorf("Put(%s, size=%d): %w", key, len(data), err)
	}
	n, err := c.Stat(ctx, key)
	if err != nil {
		return fmt.Errorf("Stat(%s): %w", key, err)
	}
	if n != int64(len(data)) {
		return fmt.Errorf("Stat(%s) = %d, want %d", key, n, len(data))
	}
	got, rel, err := c.Get(ctx, key, 0, -1)
	if err != nil {
		return fmt.Errorf("Get(%s, 0, -1): %w", key, err)
	}
	defer rel()
	if !bytes.Equal(got, data) {
		return fmt.Errorf("Get(%s) 内容不一致: got len=%d sha=%s, want len=%d sha=%s",
			key, len(got), sha256Hex(got), len(data), sha256Hex(data))
	}
	return nil
}

// bRegisterFakeInstance 直接向 TiKV 写一条「假实例」注册（仅测试用），置
// LastHeartbeat=now 使其在离线判定窗口内可见。名字以本次 nameBase 前缀开头，
// harness 兜底清理会一并删除。
func bRegisterFakeInstance(t *testing.T, h *harness, name, hostname, addr, shmAddr string) {
	t.Helper()
	now := time.Now().Unix()
	info := &cluster.InstanceInfo{
		Name:          name,
		Node:          hostname,
		Hostname:      hostname,
		Addr:          addr,
		ShmAddr:       shmAddr,
		StartTime:     now,
		LastHeartbeat: now,
	}
	if err := cluster.Register(context.Background(), h.raw, info); err != nil {
		t.Fatalf("注册假实例 %s: %v", name, err)
	}
}

// ---------------------------------------------------------------- B1 传输三态

// TestB1TransportThreeModes auto / rpc / shm 三条传输都能正常 Put/Get 读回一致；
// 且以「本机是否存在到服务端 RPC 端口的 ESTABLISHED TCP 连接」区分实际走了哪条路径
// （auto 与 shm 应无 TCP，rpc 必须走 TCP）。另验证强制 shm 在实例无 ShmAddr 时
// 报错而非静默回退 TCP。
func TestB1TransportThreeModes(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	port := bPort(t, srv.info.Addr)

	// sub 每种传输：写入并读回两个尺寸（小对象 + 跨帧 4MiB+123），比对内容。
	sub := func(mode string, wantTCP bool) {
		st := h.newSDK(func(cfg *taihuclient.ClusterConfig) {
			cfg.Transport = mode
			cfg.Source = func(ctx context.Context, key string) ([]byte, error) {
				return nil, rpcclient.ErrNotFound
			}
		})
		waitFor(t, 15*time.Second, "SDK(transport=%s) 发现本次实例(addr=%s)", func() bool {
			return st.HasLive()
		}, srv.info.Addr)

		k1 := h.key("b1/" + mode + "-small")
		d1 := pattern("B1-"+mode+"-small", 4096)
		putExact(t, st, k1, d1)
		k2 := h.key("b1/" + mode + "-big")
		d2 := pattern("B1-"+mode+"-big", 4<<20+123)
		putExact(t, st, k2, d2)

		// 传输路径判定：auto/shm 同机应走 unix 共享内存（无 TCP）；rpc 强制 TCP。
		if wantTCP {
			waitFor(t, 10*time.Second, "transport=%s 应存在到 %d 端口的 ESTABLISHED TCP", func() bool {
				return len(bEstablishedRemotes(t, port)) > 0
			}, mode, port)
		} else {
			waitFor(t, 10*time.Second, "transport=%s 不应建立到 %d 端口的 TCP（应走 shm）", func() bool {
				return len(bEstablishedRemotes(t, port)) == 0
			}, mode, port)
		}
	}

	// 顺序：先 shm/auto（都不得有 TCP），最后 rpc（必须有 TCP）。
	t.Run("shm-forced", func(t *testing.T) { sub(taihuclient.TransportShm, false) })
	t.Run("auto", func(t *testing.T) { sub(taihuclient.TransportAuto, false) })
	t.Run("rpc-forced", func(t *testing.T) { sub(taihuclient.TransportRPC, true) })

	// 强制 shm 不得静默回退：构造「实例注册 ShmAddr 为空」的情形（无真实 server），
	// Storage.clientFor 在 TransportShm 下必须报错，而不是回退 TCP 成功。
	t.Run("shm-unavailable-must-error", func(t *testing.T) {
		h2 := newHarness(t)
		hostname, _ := os.Hostname()
		bRegisterFakeInstance(t, h2, h2.nameBase+"fake", hostname, "127.0.0.1:1", "")
		st := h2.newSDK(func(cfg *taihuclient.ClusterConfig) {
			cfg.Transport = taihuclient.TransportShm
		})
		waitFor(t, 15*time.Second, "SDK 发现假实例（ShmAddr 为空）", func() bool { return st.HasLive() }, h2.nameBase+"fake")

		err := st.Put(context.Background(), h2.key("b1/shm-unavailable"), 4, []byte("data"))
		if err == nil {
			t.Fatalf("Transport=shm 且实例 ShmAddr 为空时应报错（不得静默回退 TCP），却成功写入")
		}
		if !strings.Contains(err.Error(), "no shm addr") {
			t.Fatalf("Transport=shm 无 ShmAddr 的 err = %v, want 含 \"no shm addr\"", err)
		}
	})
}

// ---------------------------------------------------------------- B2 多 IP 监听 + 多地址连接池

// TestB2MultiIPListen 服务端在 127.0.0.1 与节点真实内网 IP 上同端口监听：注册
// Addrs 含两个 IP:port（同端口）、Addr 为首个 IP；DialPoolMulti 建池后大量读写
// 全部成功，且两个地址各自的 ESTABLISHED 连接都被建立（证明多地址被真正使用）。
func TestB2MultiIPListen(t *testing.T) {
	h := newHarness(t)
	nodeIP := bNodeIPv4(t)
	t.Logf("本机内网 IPv4 = %s", nodeIP)
	srv := h.startServer(serverOpts{listen: []string{"127.0.0.1", nodeIP}})

	addrs := srv.info.Addrs
	if len(addrs) != 2 {
		t.Fatalf("Addrs = %v (len=%d), want 2 个地址", addrs, len(addrs))
	}
	p0, p1 := bPort(t, addrs[0]), bPort(t, addrs[1])
	if p0 != p1 {
		t.Fatalf("多 IP 监听应同端口: %v -> 端口 %d/%d", addrs, p0, p1)
	}
	wantAddrs := []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(p0)), net.JoinHostPort(nodeIP, strconv.Itoa(p0))}
	for i, want := range wantAddrs {
		if addrs[i] != want {
			t.Fatalf("Addrs[%d] = %q, want %q (全部: %v)", i, addrs[i], want, addrs)
		}
	}
	if srv.info.Addr != wantAddrs[0] {
		t.Fatalf("Addr = %q, want 首个 IP %q", srv.info.Addr, wantAddrs[0])
	}

	// 每个地址都能被单独 DialPool 连上并读写。
	for _, a := range addrs {
		c, err := rpcclient.DialPool(h.ctx, a, 1)
		if err != nil {
			t.Fatalf("DialPool(%s): %v", a, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		d := pattern("B2-single-"+a, 4<<20+7)
		if err := bPutGetVerify(c, h.key("b2/single-"+a), d); err != nil {
			t.Fatalf("单地址 %s 读写失败: %v", a, err)
		}
	}

	// 多地址连接池：大量读写全部成功。
	pool, err := rpcclient.DialPoolMulti(h.ctx, addrs, 2)
	if err != nil {
		t.Fatalf("DialPoolMulti(%v): %v", addrs, err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	const n = 48
	sizes := []int{1, 4096, 64 << 10, 4 << 20, 4<<20 + 123}
	keys := make([]string, n)
	contents := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = h.key(fmt.Sprintf("b2/obj-%02d", i))
		contents[i] = pattern(fmt.Sprintf("B2-%d", i), sizes[i%len(sizes)])
		putExact(t, pool, keys[i], contents[i])
	}
	for i := range keys {
		getExact(t, pool, keys[i], contents[i])
	}

	// 连接确实落到两个地址：两个对端各应有 ESTABLISHED 连接。
	waitFor(t, 10*time.Second, "两个监听地址都被连上 (remotes=%v)", func() bool {
		rem := bEstablishedRemotes(t, p0)
		return rem[wantAddrs[0]] > 0 && rem[wantAddrs[1]] > 0
	}, p0)
}

// ---------------------------------------------------------------- B3 单连接多路复用配对

// TestB3PipelineMultiplex Conns=1（单条连接）下 8 个 goroutine 各自反复读写不同 key
// （并发在途），断言每个 key 读回的都是自身内容、无串扰——验证单连接内 streamID
// 多路复用的请求/响应配对。
//
// 注：本用例在节点 12 上稳定失败，已确认为产品缺陷（跨流数据错配 + 连接被关闭），
// 与 AIO 后端无关（io-uring on/off 均复现），且可在进程内无 PD 复现：
// internal/transport 同款单连接 8 路并发（800B，One Conn）5 轮内即报错。详见汇报。
func TestB3PipelineMultiplex(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1) // 单连接：并发全部压在一条连接的多 stream 上

	const (
		workers = 8
		rounds  = 50
	)
	sizes := []int{1, 4097, 64 << 10, 1 << 20, 4 << 20, 4<<20 + 123, 8 << 20, 12345}
	keys := make([]string, workers)
	datas := make([][]byte, workers)
	expect := map[string]int{} // sha -> worker（诊断串扰用）
	for g := 0; g < workers; g++ {
		keys[g] = h.key(fmt.Sprintf("b3/worker-%d", g))
		datas[g] = pattern(fmt.Sprintf("B3-w%d", g), sizes[g])
		expect[sha256Hex(datas[g])] = g
	}

	run := func(tag string, body func(g int) error) {
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			errs []string
		)
		for g := 0; g < workers; g++ {
			g := g
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := body(g); err != nil {
					mu.Lock()
					errs = append(errs, fmt.Sprintf("%s worker %d (size=%d): %v", tag, g, len(datas[g]), err))
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(errs) > 0 {
			t.Fatalf("%d 个 worker 失败:\n%s", len(errs), strings.Join(errs, "\n"))
		}
	}

	// 两个子场景分开跑，便于定位问题在「只读并发」还是「读写混合并发」：
	//   readonly-concurrent：先串行写全部 key（各 key 尺寸/内容互不相同），再 8 路并发只读；
	//   mixed：8 路并发各自 Put 自己的 key 再 Get（size=-1，客户端需先 Stat）。
	t.Run("readonly-concurrent", func(t *testing.T) {
		for g := 0; g < workers; g++ {
			if err := bPutGetVerify(c, keys[g], datas[g]); err != nil {
				t.Fatalf("串行预写 worker %d: %v", g, err)
			}
		}
		run("readonly-concurrent", func(g int) error {
			for r := 0; r < rounds; r++ {
				// 每次 RPC 限时：单连接多路复用若丢响应，必须显式失败而非永久挂起。
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				got, rel, err := c.Get(ctx, keys[g], 0, int64(len(datas[g])))
				cancel()
				if err != nil {
					return fmt.Errorf("round %d Get(size=%d): %w", r, len(datas[g]), err)
				}
				if !bytes.Equal(got, datas[g]) {
					via := expect[sha256Hex(got)]
					rel()
					return fmt.Errorf("round %d 内容串扰（got sha 归属 worker %d）: got len=%d sha=%s, want sha=%s",
						r, via, len(got), sha256Hex(got), sha256Hex(datas[g]))
				}
				rel()
			}
			return nil
		})
	})

	t.Run("mixed", func(t *testing.T) {
		run("mixed", func(g int) error {
			for r := 0; r < rounds; r++ {
				// 每次 RPC 限时：单连接多路复用若丢响应，必须显式失败而非永久挂起。
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				putErr := c.Put(ctx, keys[g], int64(len(datas[g])), datas[g])
				cancel()
				if putErr != nil {
					return fmt.Errorf("round %d Put(size=%d): %w", r, len(datas[g]), putErr)
				}
				ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
				got, rel, err := c.Get(ctx, keys[g], 0, -1)
				cancel()
				if err != nil {
					return fmt.Errorf("round %d Get(size=%d): %w", r, len(datas[g]), err)
				}
				if !bytes.Equal(got, datas[g]) {
					via := expect[sha256Hex(got)]
					rel()
					return fmt.Errorf("round %d 内容串扰（got sha 归属 worker %d）: got len=%d sha=%s, want sha=%s",
						r, via, len(got), sha256Hex(got), sha256Hex(datas[g]))
				}
				rel()
			}
			return nil
		})
	})
}

// ---------------------------------------------------------------- B4 帧尺寸统计

// TestB4FrameStats 用进程内客户端计数器（transport-rx）的前后差值验证：
// 4MiB 整块读恰一帧；4MiB+123 读恰为 1 个 4MiB 整帧 + 1 个尾帧；
// take+copy 之和等于数据帧数；rx 字节数与请求尺寸一致。
//
// take/copy 的数值以记录式打印（TCP 路径当前停用零拷贝移交，take 恒 0），
// 只断言与移交实现无关的不变式，避免测试绑定某一版实现。
func TestB4FrameStats(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	// --- 4MiB 整块：写（无接收数据帧）→ 整对象读回（单帧，零拷贝移交）---
	const blk = 4 << 20
	d4 := pattern("B4-blk", blk)
	k4 := h.key("b4/blk")

	s0 := bReadRxStats()
	if err := c.Put(h.ctx, k4, int64(len(d4)), d4); err != nil {
		t.Fatalf("Put(%s, %d): %v", k4, len(d4), err)
	}
	s1 := bReadRxStats()
	if df, db := s1.frames-s0.frames, s1.bytes-s0.bytes; df != 0 || db != 0 {
		t.Fatalf("Put 4MiB 不应产生接收数据帧: Δframes=%d Δbytes=%d", df, db)
	}

	readBlk := func(tag string, base bRxStats) bRxStats {
		got, rel, err := c.Get(h.ctx, k4, 0, blk)
		if err != nil {
			t.Fatalf("%s Get(%s, 0, %d): %v", tag, k4, blk, err)
		}
		if !bytes.Equal(got, d4) {
			rel()
			t.Fatalf("%s Get(%s) 内容不一致: len=%d/%d sha=%s/%s", tag, k4, len(got), len(d4), sha256Hex(got), sha256Hex(d4))
		}
		rel()
		s := bReadRxStats()
		if df := s.frames - base.frames; df != 1 {
			t.Fatalf("%s 4MiB 整块读应恰 1 个数据帧: Δframes=%d", tag, df)
		}
		if db := s.bytes - base.bytes; db != blk {
			t.Fatalf("%s rx 字节 = %d, want %d", tag, db, blk)
		}
		if d := s.data4M - base.data4M; d != 1 {
			t.Fatalf("%s 应恰 1 个 4MiB 整帧: Δdata4MiB=%d", tag, d)
		}
		if s.take-base.take+s.copy-base.copy != s.frames-base.frames {
			t.Fatalf("%s take+copy 应等于数据帧数: Δtake=%d Δcopy=%d Δframes=%d",
				tag, s.take-base.take, s.copy-base.copy, s.frames-base.frames)
		}
		return s
	}

	// 首读：连接上尚无大帧，收流节点自适应尺寸未增长，整帧跨多节点。
	s2 := readBlk("首次整块读", s1)
	// 次读：记录式对照（TCP 移交已停用：take 恒 0、copy 恰 1），不设为断言。
	s3 := readBlk("二次整块读", s2)
	t.Logf("4MiB 整块读 take/copy 对照：首次 Δtake=%d Δcopy=%d，二次 Δtake=%d Δcopy=%d",
		s2.take-s1.take, s2.copy-s1.copy, s3.take-s2.take, s3.copy-s2.copy)

	// --- 4MiB+123：整帧 4MiB + 尾帧 123B ---
	const extra = 123
	dA := pattern("B4-blk123", blk+extra)
	kA := h.key("b4/blk123")
	if err := c.Put(h.ctx, kA, int64(len(dA)), dA); err != nil {
		t.Fatalf("Put(%s, %d): %v", kA, len(dA), err)
	}
	s3 = bReadRxStats()
	gotA, relA, err := c.Get(h.ctx, kA, 0, -1)
	if err != nil {
		t.Fatalf("Get(%s, 0, -1): %v", kA, err)
	}
	if !bytes.Equal(gotA, dA) {
		relA()
		t.Fatalf("Get(%s) 内容不一致: len=%d/%d sha=%s/%s", kA, len(gotA), len(dA), sha256Hex(gotA), sha256Hex(dA))
	}
	relA()
	s4 := bReadRxStats()
	if df := s4.frames - s3.frames; df != 2 {
		t.Fatalf("4MiB+123 读应恰 2 个数据帧（1 整帧 + 1 尾帧）: Δframes=%d", df)
	}
	if db := s4.bytes - s3.bytes; db != blk+extra {
		t.Fatalf("4MiB+123 读 rx 字节 = %d, want %d", db, blk+extra)
	}
	if d := s4.data4M - s3.data4M; d != 1 {
		t.Fatalf("4MiB+123 读应恰含 1 个 4MiB 整帧: Δdata4MiB=%d", d)
	}
	if dTake, dCopy := s4.take-s3.take, s4.copy-s3.copy; dTake != 0 || dCopy != 2 {
		t.Fatalf("多帧读应全走汇入拷贝: Δtake=%d Δcopy=%d", dTake, dCopy)
	}
	if s4.take-s3.take+s4.copy-s3.copy != s4.frames-s3.frames {
		t.Fatalf("take+copy 应等于数据帧数: Δtake=%d Δcopy=%d Δframes=%d",
			s4.take-s3.take, s4.copy-s3.copy, s4.frames-s3.frames)
	}
}

// ---------------------------------------------------------------- B5 shm 会话反复建断

func bFDCount() int {
	es, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(es)
}

// TestB5ShmSessionChurn 反复 DialShmPool/Close（20 轮）后无 fd/goroutine 泄漏
// （回落到基线附近），且 Close 后 unix socket 残留可被删除。
func TestB5ShmSessionChurn(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	uds := srv.shmPath()

	// 预热一轮：让池/映射等懒资源就位，再取基线。
	warm, err := rpcclient.DialShmPool(h.ctx, uds, 1)
	if err != nil {
		t.Fatalf("DialShmPool(uds=%s): %v", uds, err)
	}
	if err := bPutGetVerify(warm, h.key("b5/warm"), pattern("B5-warm", 8192)); err != nil {
		t.Fatalf("预热会话读写失败: %v", err)
	}
	if err := warm.Close(); err != nil {
		t.Fatalf("预热会话 Close: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // 等预热会话后台 goroutine 退出后再取基线

	baseG, baseF := runtime.NumGoroutine(), bFDCount()
	if baseF < 0 {
		t.Fatalf("无法读取 /proc/self/fd（基线）")
	}

	const rounds = 20
	for i := 0; i < rounds; i++ {
		c, err := rpcclient.DialShmPool(h.ctx, uds, 1)
		if err != nil {
			t.Fatalf("第 %d 轮 DialShmPool(%s): %v", i, uds, err)
		}
		if err := bPutGetVerify(c, h.key(fmt.Sprintf("b5/obj-%d", i)), pattern(fmt.Sprintf("B5-%d", i), 1024+i)); err != nil {
			_ = c.Close()
			t.Fatalf("第 %d 轮会话读写失败: %v", i, err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("第 %d 轮 Close: %v", i, err)
		}
	}

	waitFor(t, 20*time.Second, "shm 会话反复建断后 goroutine 回落（基线 %d）", func() bool {
		return runtime.NumGoroutine() <= baseG+4
	}, baseG)
	waitFor(t, 10*time.Second, "shm 会话反复建断后 fd 回落（基线 %d）", func() bool {
		return bFDCount() <= baseF+4
	}, baseF)

	if err := os.Remove(uds); err != nil {
		t.Fatalf("Close 后 os.Remove(%s) 失败: %v", uds, err)
	}
}

// ---------------------------------------------------------------- B6 TCP 半包 / 慢客户端

// TestB6PartialFrame 裸 TCP 连接发送不完整帧（帧头声明 len 但负载只发一部分），
// 断言服务端不崩（进程存活、日志无 panic/fatal）、其它正常客户端读写不受影响；
// 之后断开裸连接服务端仍能服务。
func TestB6PartialFrame(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	raw, err := net.Dial("tcp", srv.info.Addr)
	if err != nil {
		t.Fatalf("裸 Dial(%s): %v", srv.info.Addr, err)
	}

	// 帧格式 [4B len][4B streamID][1B op][payload]，len = FrameHeaderLen + payload。
	// 声明 payload=64（len=69），但只发 9B 头 + 10B 负载（共 19B），服务端 Slice 阻塞。
	const claimPayload = 64
	hdr := make([]byte, 4+protocol.FrameHeaderLen)
	binary.BigEndian.PutUint32(hdr[0:4], uint32(protocol.FrameHeaderLen+claimPayload))
	binary.BigEndian.PutUint32(hdr[4:8], 1) // streamID
	hdr[8] = byte(protocol.OpPutHeader)
	if _, err := raw.Write(hdr); err != nil {
		t.Fatalf("写半包帧头: %v", err)
	}
	if _, err := raw.Write(make([]byte, 10)); err != nil {
		t.Fatalf("写半包负载: %v", err)
	}

	// 服务端应仍存活。
	select {
	case <-srv.done:
		t.Fatalf("服务端在半包帧后退出: %v\n--- log tail ---\n%s", srv.exitErr, srv.logTail())
	default:
	}

	// 其它正常客户端读写不受影响（半包连接仍挂着）。
	k1 := h.key("b6/while-partial")
	d1 := pattern("B6-while", 4<<20+5)
	if err := bPutGetVerify(c, k1, d1); err != nil {
		t.Fatalf("半包连接挂着时正常客户端读写失败: %v", err)
	}

	// 断开裸连接，服务端仍能服务。
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭裸连接: %v", err)
	}
	k2 := h.key("b6/after-close")
	d2 := pattern("B6-after", 64<<10)
	if err := bPutGetVerify(c, k2, d2); err != nil {
		t.Fatalf("裸连接断开后正常客户端读写失败: %v", err)
	}
	select {
	case <-srv.done:
		t.Fatalf("服务端在裸连接断开后退出: %v\n--- log tail ---\n%s", srv.exitErr, srv.logTail())
	default:
	}

	// 日志不得有 panic / fatal error。
	if log, err := os.ReadFile(srv.logPath); err == nil {
		s := string(log)
		if strings.Contains(s, "panic") || strings.Contains(s, "fatal error") {
			t.Fatalf("服务端日志出现 panic/fatal error:\n%s", srv.logTail())
		}
	}
}
