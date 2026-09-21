//go:build e2e

package e2e

// D 组：生命周期 / 崩溃恢复 / 段水位。
//
//	D1 SIGTERM 优雅停机 → 同盘同元数据目录重启，数据可读、注册正常；
//	D2 SIGKILL 崩溃 → 重启（pebble + 段计数自愈），数据可读、可继续写；
//	D3 同一裸设备 O_EXCL 独占：第二实例启动失败，首实例不受影响；
//	D4 段水位与回收：段汇总自洽、写满一段后 Full/Active 增长、删除后段退出 Full；
//	D5 容量水位：写满唯一可用段后写入被拒（ErrNoSpace），不静默截断、不崩溃；
//	D6 pprof 端点与 5s [stat] 日志存活。
//
// 全部 server 均由 harness 起（loop 裸设备 + 真 TiKV），不触碰集群既有实例。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/rpcclient"
)

// dSegSize 段大小内置值（layout.DefaultSegmentSizeBytes，写死 8GiB，不允许命令行覆盖）。
const dSegSize = int64(8) << 30

// ---------------------------------------------------------------- 工具

// dReadLog 读 server 日志全文（harness 同包，logPath 可直接访问）。
func dReadLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读日志 %s: %v", path, err)
	}
	return string(b)
}

// dCountSub 统计子串出现次数。
func dCountSub(s, sub string) int {
	n := 0
	for {
		i := strings.Index(s, sub)
		if i < 0 {
			return n
		}
		n++
		s = s[i+len(sub):]
	}
}

// dPprofPort 从日志中解析 `pprof listening on :<port>` 的端口。
func dPprofPort(log string) (int, bool) {
	const marker = "pprof listening on :"
	i := strings.Index(log, marker)
	if i < 0 {
		return 0, false
	}
	rest := log[i+len(marker):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:j])
	if err != nil {
		return 0, false
	}
	return n, true
}

// dPoll 轮询 cond 直到返回 nil；超时失败信息附 diag（日志尾部）以便诊断。
func dPoll(t *testing.T, timeout time.Duration, desc string, diag func() string, cond func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if last = cond(); last == nil {
			return
		}
		if time.Now().After(deadline) {
			extra := ""
			if diag != nil {
				extra = "\n--- log tail ---\n" + diag()
			}
			t.Fatalf("等待超时(%s): %s: %v%s", timeout, desc, last, extra)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// dRestartSameDisk 用同一裸设备 + 同一 pebble 目录 + 同一实例名重启（重启用例关键）。
func dRestartSameDisk(t *testing.T, h *harness, old *testServer) *testServer {
	t.Helper()
	return h.startServer(serverOpts{name: old.name, dev: old.dev, dbDir: old.dbDir})
}

// dPutMany 连续写 n 个同尺寸对象（复用同一缓冲，避免为水位测试生成 GB 级随机内容）。
func dPutMany(t *testing.T, h *harness, srv *testServer, c *rpcclient.Storage, prefix string, n, size int, buf []byte) {
	t.Helper()
	for i := 0; i < n; i++ {
		key := h.key(fmt.Sprintf("%s-%03d", prefix, i))
		if err := c.Put(h.ctx, key, int64(size), buf); err != nil {
			t.Fatalf("Put(%s) 第 %d/%d 个: %v\n--- log tail ---\n%s", key, i+1, n, err, srv.logTail())
		}
	}
}

// dSegSummary 读段汇总与明细（admin RPC 仅 TCP 直连支持）。
func dSegSummary(t *testing.T, srv *testServer, c *rpcclient.Storage) (rpcclient.SegmentSummary, []rpcclient.SegmentEntry) {
	t.Helper()
	sum, entries, err := c.Segments(context.Background())
	if err != nil {
		t.Fatalf("Segments: %v\n--- log tail ---\n%s", err, srv.logTail())
	}
	return sum, entries
}

// dSegStateOf 取指定段的明细状态。
func dSegStateOf(entries []rpcclient.SegmentEntry, id int64) (rpcclient.SegmentState, bool) {
	for _, e := range entries {
		if e.SegmentID == id {
			return e.State, true
		}
	}
	return 0, false
}

// dAssertSegSelfConsistent 断言段汇总字段自洽：Total == 明细条数 == 各状态计数之和。
func dAssertSegSelfConsistent(t *testing.T, srv *testServer, sum rpcclient.SegmentSummary, entries []rpcclient.SegmentEntry) {
	t.Helper()
	var free, active, full, reclaiming, compacting int64
	for _, e := range entries {
		switch e.State {
		case rpcclient.SegmentStateFree:
			free++
		case rpcclient.SegmentStateActive:
			active++
		case rpcclient.SegmentStateFull:
			full++
		case rpcclient.SegmentStateReclaiming:
			reclaiming++
		case rpcclient.SegmentStateCompacting:
			compacting++
		}
	}
	diag := func() string { return srv.logTail() }
	if sum.Total != int64(len(entries)) {
		t.Fatalf("段汇总 Total=%d 与明细条数 %d 不一致\n%s", sum.Total, len(entries), diag())
	}
	if sum.Free != free || sum.Active != active || sum.Full != full || sum.Reclaiming != reclaiming {
		t.Fatalf("段汇总与明细计数不一致: summary{free=%d active=%d full=%d reclaiming=%d} 明细{free=%d active=%d full=%d reclaiming=%d compacting=%d}\n%s",
			sum.Free, sum.Active, sum.Full, sum.Reclaiming, free, active, full, reclaiming, compacting, diag())
	}
	if sum.Total != free+active+full+reclaiming+compacting {
		t.Fatalf("段汇总 Total=%d != 各状态之和 %d（free=%d active=%d full=%d reclaiming=%d compacting=%d）\n%s",
			sum.Total, free+active+full+reclaiming+compacting, free, active, full, reclaiming, compacting, diag())
	}
	if sum.SegSize != dSegSize {
		t.Fatalf("SegSize=%d, want %d\n%s", sum.SegSize, dSegSize, diag())
	}
	if sum.CursorSeg < 0 || sum.CursorOff < 0 || sum.CursorOff > dSegSize {
		t.Fatalf("写游标非法: seg=%d off=%d（SegSize=%d）\n%s", sum.CursorSeg, sum.CursorOff, dSegSize, diag())
	}
}

// ---------------------------------------------------------------- D1

// TestD1GracefulRestart：SIGTERM 优雅停机（退出码 0、监听端口释放），用同一裸设备与同一
// pebble 目录重启后，对象逐字节可读、Stat 一致，集群注册恰好一个存活实例。
func TestD1GracefulRestart(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	objs := []struct {
		key  string
		data []byte
	}{
		{h.key("d1/block-4m"), randBytes(t, 4<<20)},
		{h.key("d1/unaligned"), randBytes(t, 4<<20+4097)},
		{h.key("d1/small"), randBytes(t, 4097)},
		{h.key("d1/empty"), nil},
	}
	for _, o := range objs {
		putExact(t, c, o.key, o.data)
	}

	srv.term()
	if code := srv.exitCode(); code != 0 {
		t.Fatalf("SIGTERM 后退出码=%d, want 0\n--- log tail ---\n%s", code, srv.logTail())
	}
	// 优雅停机后监听端口应已释放（进程确实停了，而非仍驻留）。
	if conn, err := net.DialTimeout("tcp", srv.info.Addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatalf("SIGTERM 后 %s 仍可拨通，进程未真正停止\n--- log tail ---\n%s", srv.info.Addr, srv.logTail())
	}

	srv2 := dRestartSameDisk(t, h, srv)
	c2 := h.dialDirect(srv2, 1)
	for _, o := range objs {
		getExact(t, c2, o.key, o.data)
	}

	// 注意：启动时的首次 Register 不带 LastHeartbeat（=0），存活判定需等首个 1s 心跳落盘，
	// 故此处轮询等待，而非立即断言。
	dPoll(t, 20*time.Second, "重启后集群注册恰好 1 个存活实例且 addr 为新实例",
		func() string { return srv2.logTail() }, func() error {
			live := h.liveInstances()
			if len(live) != 1 {
				return fmt.Errorf("存活实例数=%d, want 1", len(live))
			}
			info := h.instanceByName(srv.name)
			if info == nil {
				return errors.New("注册记录缺失")
			}
			if info.Addr != srv2.info.Addr {
				return fmt.Errorf("注册 addr=%s, want %s", info.Addr, srv2.info.Addr)
			}
			return nil
		})
}

// ---------------------------------------------------------------- D2

// TestD2KillRestart：SIGKILL 模拟崩溃（注册记录残留、无优雅注销），同盘同目录重启后
// pebble 与段存活计数自愈：旧对象可读且 Stat 正确，并可继续写新对象。
func TestD2KillRestart(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	objs := []struct {
		key  string
		data []byte
	}{
		{h.key("d2/block-4m"), randBytes(t, 4<<20)},
		{h.key("d2/unaligned"), randBytes(t, 4<<20+123)},
		{h.key("d2/small"), randBytes(t, 1)},
		{h.key("d2/empty"), nil},
	}
	for _, o := range objs {
		putExact(t, c, o.key, o.data)
	}

	srv.kill()
	// SIGKILL 无注销路径：注册记录必然残留（崩溃语义）。
	if info := h.instanceByName(srv.name); info == nil {
		t.Fatalf("SIGKILL 后注册记录竟已消失（不该走注销路径）\n--- log tail ---\n%s", srv.logTail())
	}

	srv2 := dRestartSameDisk(t, h, srv)
	c2 := h.dialDirect(srv2, 1)

	// 崩溃前写入的对象必须原样可读，且 Stat 尺寸正确（getExact 内含 Stat 校验）。
	for _, o := range objs {
		getExact(t, c2, o.key, o.data)
	}

	// 重启后可继续写（pebble 崩溃自愈 + 段游标续写）。
	fresh := randBytes(t, 4<<20+999)
	putExact(t, c2, h.key("d2/after-restart"), fresh)
	// 覆盖写重启前的 key 也应正常。
	over := pattern("D2-overwrite", 8<<20)
	if err := c2.Put(h.ctx, objs[0].key, int64(len(over)), over); err != nil {
		t.Fatalf("重启后覆盖写: %v\n--- log tail ---\n%s", err, srv2.logTail())
	}
	getExact(t, c2, objs[0].key, over)

	// 段统计在崩溃重启后仍自洽。
	sum, entries := dSegSummary(t, srv2, c2)
	dAssertSegSelfConsistent(t, srv2, sum, entries)
}

// ---------------------------------------------------------------- D3

// TestD3DeviceExclusive：实例 A 在跑时，实例 B 以 O_EXCL 打开同一裸设备必须失败
// （非 0 退出 + 设备独占错误），A 的数据面不受任何影响。
func TestD3DeviceExclusive(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	key := h.key("d3/obj")
	data := randBytes(t, 4<<20+7)
	putExact(t, c, key, data)

	// 同 --dev、不同 --server-name / --db：唯一预期失败点是设备独占。
	nameB := h.nameBase + "b"
	dbB := filepath.Join(h.dir, nameB+"-db")
	if err := os.MkdirAll(dbB, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dbB, err)
	}
	args := []string{
		"server",
		"--listen", "127.0.0.1",
		"--db", dbB,
		"--dev", srv.dev,
		"--server-name", nameB,
		"--pd", strings.Join(h.pd, ","),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, args...)
	cmd.Dir = h.dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	if ctx.Err() != nil {
		t.Fatalf("B 指向同盘启动后 90s 内未退出（本应因设备独占失败）: %v\n--- B log ---\n%s\n--- A log tail ---\n%s",
			ctx.Err(), out.String(), srv.logTail())
	}
	if err == nil {
		t.Fatalf("B 指向同盘启动竟然成功（应因 O_EXCL 独占失败）\n--- B log ---\n%s", out.String())
	}
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		t.Fatalf("B 退出码=0, want 非 0\n--- B log ---\n%s", out.String())
	}
	low := strings.ToLower(out.String())
	hit := ""
	for _, pat := range []string{"device or resource busy", "ebusy", "open device", "o_excl", "exclusive"} {
		if strings.Contains(low, pat) {
			hit = pat
			break
		}
	}
	if hit == "" {
		t.Fatalf("B 失败原因未见设备独占/O_EXCL 相关错误\n--- B log ---\n%s\n--- A log tail ---\n%s", out.String(), srv.logTail())
	}
	t.Logf("B 按预期失败（匹配 %q）: %s", hit, strings.TrimSpace(firstLine(out.String())))

	// A 仍正常读写。
	getExact(t, c, key, data)
	putExact(t, c, h.key("d3/obj2"), randBytes(t, 4097))
	if _, _, err := c.Ping(h.ctx); err != nil {
		t.Fatalf("B 失败后 A Ping: %v\n--- A log tail ---\n%s", err, srv.logTail())
	}
}

// firstLine 取首行（日志头部噪声较少）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------- D4

// TestD4SegmentWatermarkReclaim：段汇总字段自洽；写入超过一个段大小（8GiB）的数据后出现
// Full+Active 段、对象跨段且落在 active/full 段、Meta/Stat 与内容一致；删除整段对象后
// 该段退出 Full（Reclaiming/Free），并把已删对象变为不可读。
func TestD4SegmentWatermarkReclaim(t *testing.T) {
	h := newHarness(t)
	// 64GiB = 8 段。用户写路径的顺序滚动上限是「段数 - 预留缓冲段(2)」，即本盘最多可
	// 顺序滚到 seg5；留足余量以免在断言点之前就先撞上 ErrNoSpace（那是 D5 的题目）。
	srv := h.startServer(serverOpts{devSize: 64 << 30})
	c := h.dialDirect(srv, 1)

	sum, entries := dSegSummary(t, srv, c)
	dAssertSegSelfConsistent(t, srv, sum, entries)
	if sum.Total != 0 {
		t.Fatalf("全新实例（无写入）不应有段记录，实际 Total=%d\n--- log tail ---\n%s", sum.Total, srv.logTail())
	}

	// 128MiB × 64 = 8GiB = 一整段（段大小内置 8GiB）；第 65 个对象必然滚入下一段。
	const objSize = 128 << 20
	const nSeg0 = 64
	buf := randBytes(t, objSize)
	dPutMany(t, h, srv, c, "d4/obj", nSeg0, objSize, buf)

	keyCross := h.key("d4/obj-cross")
	if err := c.Put(h.ctx, keyCross, objSize, buf); err != nil {
		t.Fatalf("跨段对象 Put: %v\n--- log tail ---\n%s", err, srv.logTail())
	}

	sum, entries = dSegSummary(t, srv, c)
	dAssertSegSelfConsistent(t, srv, sum, entries)
	// 诊断留痕：游标位置反映「已写数据量 vs 已分配空间」。
	t.Logf("写入 %d×%dMiB 后：total=%d free=%d active=%d full=%d reclaiming=%d cursor=(seg %d, off %d) 对象=%d",
		nSeg0+1, objSize>>20, sum.Total, sum.Free, sum.Active, sum.Full, sum.Reclaiming,
		sum.CursorSeg, sum.CursorOff, sum.ObjectCount)
	if sum.CursorSeg < 0 || sum.CursorSeg >= 8 {
		t.Fatalf("写游标 seg=%d 越界（本盘 64GiB/8GiB = 8 段）\n--- log tail ---\n%s", sum.CursorSeg, srv.logTail())
	}
	if sum.Full < 1 || sum.Active < 1 {
		t.Fatalf("写满一段后应出现 Full+Active，实际 free=%d active=%d full=%d reclaiming=%d\n--- log tail ---\n%s",
			sum.Free, sum.Active, sum.Full, sum.Reclaiming, srv.logTail())
	}
	if sum.ObjectCount != int64(nSeg0+1) {
		t.Fatalf("对象数=%d, want %d\n--- log tail ---\n%s", sum.ObjectCount, nSeg0+1, srv.logTail())
	}

	firstKey := h.key("d4/obj-000")
	mFirst, err := c.Meta(h.ctx, firstKey)
	if err != nil {
		t.Fatalf("Meta(%s): %v\n--- log tail ---\n%s", firstKey, err, srv.logTail())
	}
	mCross, err := c.Meta(h.ctx, keyCross)
	if err != nil {
		t.Fatalf("Meta(%s): %v\n--- log tail ---\n%s", keyCross, err, srv.logTail())
	}
	if mCross.SegmentID == mFirst.SegmentID {
		t.Fatalf("第 %d 个对象未跨段：seg=%d 与首对象同段（段大小=%d）\n--- log tail ---\n%s",
			nSeg0+1, mCross.SegmentID, dSegSize, srv.logTail())
	}
	for _, id := range []int64{mFirst.SegmentID, mCross.SegmentID} {
		st, ok := dSegStateOf(entries, id)
		if !ok {
			t.Fatalf("对象所在段 %d 无明细\n--- log tail ---\n%s", id, srv.logTail())
		}
		if st != rpcclient.SegmentStateActive && st != rpcclient.SegmentStateFull {
			t.Fatalf("已写对象所在段 %d 状态=%d, want active(1)/full(2)\n--- log tail ---\n%s", id, st, srv.logTail())
		}
	}
	// Meta / Stat / 内容三者一致（抽查首对象与跨段对象）。
	for _, k := range []string{firstKey, keyCross} {
		meta, err := c.Meta(h.ctx, k)
		if err != nil {
			t.Fatalf("Meta(%s): %v\n--- log tail ---\n%s", k, err, srv.logTail())
		}
		n, err := c.Stat(h.ctx, k)
		if err != nil {
			t.Fatalf("Stat(%s): %v\n--- log tail ---\n%s", k, err, srv.logTail())
		}
		if n != objSize || meta.Size != objSize {
			t.Fatalf("%s 尺寸不一致: Stat=%d Meta.Size=%d, want %d", k, n, meta.Size, objSize)
		}
		getExact(t, c, k, buf)
	}

	// 删除整段（seg0）对象：AliveCount 归零 → 段立即退出 Full（Reclaiming），随后 GC 回收入池。
	seg0 := mFirst.SegmentID
	dPutDeletePrefix(t, h, srv, c, "d4/obj", nSeg0)
	dPoll(t, 20*time.Second, "段 "+strconv.FormatInt(seg0, 10)+" 退出 Full（对象删光 → 回收）",
		func() string { return srv.logTail() }, func() error {
			_, es, err := c.Segments(context.Background())
			if err != nil {
				return err
			}
			st, ok := dSegStateOf(es, seg0)
			if !ok {
				return fmt.Errorf("段 %d 无明细", seg0)
			}
			if st == rpcclient.SegmentStateFull || st == rpcclient.SegmentStateActive {
				return fmt.Errorf("段 %d 仍为 %d", seg0, st)
			}
			return nil
		})

	sum, entries = dSegSummary(t, srv, c)
	dAssertSegSelfConsistent(t, srv, sum, entries)
	if sum.ObjectCount != 1 {
		t.Fatalf("删掉 %d 个对象后对象数=%d, want 1\n--- log tail ---\n%s", nSeg0, sum.ObjectCount, srv.logTail())
	}
	if _, rel, err := c.Get(h.ctx, firstKey, 0, -1); !errors.Is(err, rpcclient.ErrNotFound) {
		if rel != nil {
			rel()
		}
		t.Fatalf("已删对象 %s Get err=%v, want ErrNotFound\n--- log tail ---\n%s", firstKey, err, srv.logTail())
	}
	getExact(t, c, keyCross, buf)
}

// dPutDeletePrefix 删除 dPutMany 写入的整批对象。
func dPutDeletePrefix(t *testing.T, h *harness, srv *testServer, c *rpcclient.Storage, prefix string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		key := h.key(fmt.Sprintf("%s-%03d", prefix, i))
		if err := c.Delete(h.ctx, key); err != nil {
			t.Fatalf("Delete(%s) 第 %d/%d 个: %v\n--- log tail ---\n%s", key, i+1, n, err, srv.logTail())
		}
	}
}

// ---------------------------------------------------------------- D5

// TestD5CapacityWatermark：小盘（10GiB = 1 段 8GiB，恰好是物理设备的 80%）写满唯一可用段
// 后，后续写入必须被明确拒绝，而非静默截断或崩溃；已写对象仍完整可读，心跳上报水位到位。
//
// 拒绝的观测形态有两种（都在预期内）：
//   - 客户端收到 OpResp 错误码 → rpcclient.ErrNoSpace；
//   - 服务端在 PutBegin 阶段失败时立即回错误帧并结束该流，而客户端此前已把整包数据帧
//     发在途，这些帧命中 dispatch 的「处理器已结束」分支 → deliverFatal 关闭整条连接，
//     客户端于是看到连接级错误（ErrNoSpace 被掩盖）。
//
// 两种形态都断言「写入未被接受」：新连接复核被拒对象不存在、服务仍存活、既有数据无损。
func TestD5CapacityWatermark(t *testing.T) {
	h := newHarness(t)
	// 10GiB → SegmentCount = 10GiB/8GiB = 1：唯一可用段 8GiB 即布局容量（= 物理设备 80%），
	// 段满后无下一段可滚（预留缓冲段不参与用户写路径）、空闲池也为空 → ErrNoSpace。
	srv := h.startServer(serverOpts{devSize: 10 << 30})
	c := h.dialDirect(srv, 1)

	const objSize = 64 << 20
	const nFill = 64
	buf := randBytes(t, objSize)
	dPutMany(t, h, srv, c, "d5/obj", nFill, objSize, buf)

	// 继续写直到被拒；每次尝试都换新连接（拒绝可能以关连接的形式呈现，旧连接会失效）。
	const probeMax = 80
	probeOK := 0
	lastOKKey := h.key(fmt.Sprintf("d5/obj-%03d", nFill-1))
	var refusal error
	probeKey := ""
	for i := 0; i < probeMax && refusal == nil; i++ {
		probeKey = h.key(fmt.Sprintf("d5/probe-%03d", i))
		pc := h.dialDirect(srv, 1)
		err := pc.Put(h.ctx, probeKey, objSize, buf)
		_ = pc.Close()
		if err != nil {
			refusal = err
			break
		}
		probeOK++
		lastOKKey = probeKey
	}
	if refusal == nil {
		t.Fatalf("连写 %d 个对象（%dMiB/个）后仍未被拒，段似乎未耗尽\n--- log tail ---\n%s",
			nFill+probeOK, objSize>>20, srv.logTail())
	}
	t.Logf("段耗尽：成功 %d 个（%dMiB/个），第 %d 次尝试被拒: %v", nFill+probeOK, objSize>>20, probeOK+1, refusal)
	if errors.Is(refusal, rpcclient.ErrNoSpace) {
		t.Logf("拒绝以 rpcclient.ErrNoSpace 原样返回")
	} else if dIsConnAbort(refusal) {
		t.Logf("拒绝经由「服务端关闭连接」呈现（在预期内，见用例注释）")
	} else {
		t.Fatalf("写入被拒但错误形态不符合预期: %v（want ErrNoSpace 或连接中止）\n--- log tail ---\n%s",
			refusal, srv.logTail())
	}
	if log := dReadLog(t, srv.logPath); strings.Contains(log, "panic") {
		t.Fatalf("写满被拒前后服务端日志出现 panic\n--- log tail ---\n%s", srv.logTail())
	}

	// 新连接复核：被拒对象未落盘（无静默截断/半对象），服务存活。
	c2 := h.dialDirect(srv, 1)
	if _, serr := c2.Stat(h.ctx, probeKey); !errors.Is(serr, rpcclient.ErrNotFound) {
		t.Fatalf("被拒对象 %s 不应存在: Stat err=%v, want ErrNotFound\n--- log tail ---\n%s", probeKey, serr, srv.logTail())
	}
	if _, _, perr := c2.Ping(h.ctx); perr != nil {
		t.Fatalf("写满被拒后 Ping: %v\n--- log tail ---\n%s", perr, srv.logTail())
	}

	// 已写对象仍完整可读（逐字节 + Stat 校验）：首个、末个、以及最后一个成功的探测对象。
	for _, k := range []string{h.key("d5/obj-000"), lastOKKey} {
		getExact(t, c2, k, buf)
	}

	// 段统计自洽：唯一段已 Full，无空闲段。
	sum, entries := dSegSummary(t, srv, c2)
	dAssertSegSelfConsistent(t, srv, sum, entries)
	if sum.Full < 1 || sum.Free != 0 {
		t.Fatalf("段耗尽后应 Full>=1 且 Free==0，实际 free=%d active=%d full=%d reclaiming=%d\n--- log tail ---\n%s",
			sum.Free, sum.Active, sum.Full, sum.Reclaiming, srv.logTail())
	}
	if want := int64(nFill + probeOK); sum.ObjectCount != want {
		t.Fatalf("对象数=%d, want %d（被拒对象不得计入）\n--- log tail ---\n%s", sum.ObjectCount, want, srv.logTail())
	}

	// 心跳上报：布局容量 8GiB（= 物理设备 10GiB 的 80%）已满，可用归零。
	dPoll(t, 20*time.Second, "心跳上报容量已满（Used==Capacity、Available==0）",
		func() string { return srv.logTail() }, func() error {
			info := h.instanceByName(srv.name)
			if info == nil {
				return errors.New("注册记录缺失")
			}
			if info.Capacity != dSegSize {
				return fmt.Errorf("Capacity=%d, want %d（布局容量）", info.Capacity, dSegSize)
			}
			if info.Used < 7<<30 {
				return fmt.Errorf("Used=%d 低于 7GiB", info.Used)
			}
			if info.Available != info.Capacity-info.Used {
				return fmt.Errorf("Available=%d != Capacity-Used=%d", info.Available, info.Capacity-info.Used)
			}
			if info.Available != 0 {
				return fmt.Errorf("段已耗尽但 Available=%d, want 0", info.Available)
			}
			return nil
		})

	// 段确实耗尽：再写一个 4KiB 小对象仍必须被拒（独立连接，避免失败连接的后续噪声）。
	c3 := h.dialDirect(srv, 1)
	if err := c3.Put(h.ctx, h.key("d5/overflow"), 4096, buf[:4096]); err == nil {
		t.Fatalf("段耗尽后写入 4KiB 对象竟然成功（应被拒）\n--- log tail ---\n%s", srv.logTail())
	}
}

// dIsConnAbort 判断错误是否为「连接/流被对端中止」形态：产品拒绝写入后结束该流，客户端
// 在途的数据帧会让服务端 dispatch 返回 deliverFatal（关闭整条连接），ErrNoSpace 被掩盖。
func dIsConnAbort(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	for _, pat := range []string{"connection", "closed", "peer", "eof", "broken pipe", "reset", "stream"} {
		if strings.Contains(low, pat) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- D6

// TestD6StatLogAndPprof：起实例后 ≥12s 的 5s [stat] 日志至少两轮（disk-io / segments），
// 且从日志解析出的 pprof 端口可正常响应 HTTP 200。
func TestD6StatLogAndPprof(t *testing.T) {
	h := newHarness(t)
	srv := h.startServer(serverOpts{})
	c := h.dialDirect(srv, 1)

	// 触发一次磁盘 IO 让 disk-io 统计有内容。
	putExact(t, c, h.key("d6/obj"), randBytes(t, 4<<20))

	dPoll(t, 60*time.Second, "日志出现 >=2 次 [stat] disk-io 与 [stat] segments（5s 周期）",
		func() string { return srv.logTail() }, func() error {
			log := dReadLog(t, srv.logPath)
			ioN, segN := dCountSub(log, "[stat] disk-io"), dCountSub(log, "[stat] segments")
			if ioN < 2 || segN < 2 {
				return fmt.Errorf("disk-io=%d segments=%d，均需 >=2", ioN, segN)
			}
			return nil
		})

	var port int
	dPoll(t, 15*time.Second, "日志出现 pprof listening 行", func() string { return srv.logTail() }, func() error {
		p, ok := dPprofPort(dReadLog(t, srv.logPath))
		if !ok {
			return errors.New("未找到 pprof listening on :<port>")
		}
		port = p
		return nil
	})

	url := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/goroutine?debug=1", port)
	client := &http.Client{Timeout: 5 * time.Second}
	dPoll(t, 20*time.Second, "pprof "+url+" 返回 200", func() string { return srv.logTail() }, func() error {
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if len(body) == 0 {
			return errors.New("HTTP 200 但响应体为空")
		}
		return nil
	})
}
