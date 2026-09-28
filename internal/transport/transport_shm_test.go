//go:build linux

// shm（共享内存 IPC）传输层单元测试：进程内起真实 shmipc 服务端（ServeShm*）+
// 客户端（DialShm），不依赖外部实例、不使用长 sleep、无外网。
//
// 临时文件（设备镜像 / pebble 目录 / unix socket）一律落在 t.TempDir()，
// 其父目录即 TMPDIR（/var/tmp，xfs），保证 O_DIRECT 可用且测试自清理。
package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liucxer/taihu/third_party/shmipc-go"

	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/internal/transport/protocol"
	"github.com/liucxer/taihu/pkg/ierr"
)

// shmTestSegSize/shmTestSegCount 测试用段布局：小段（64MiB）避免稀疏镜像过大，
// 段数够用即可；对象上限 = shmTestSegSize。
const (
	shmTestSegSize  = 64 << 20
	shmTestSegCount = 16
)

// shmNewTestStorage 在 t.TempDir() 下建一个真实 storage.Storage（稀疏设备镜像 + pebble）。
func shmNewTestStorage(t *testing.T) *storage.Storage {
	t.Helper()
	dir := t.TempDir()
	dev := filepath.Join(dir, "nvme.img")
	f, err := os.Create(dev)
	if err != nil {
		t.Fatalf("create device file: %v", err)
	}
	_ = f.Close()
	st, err := storage.NewStorage(context.Background(), filepath.Join(dir, "meta"), dev,
		layout.Layout{SegmentSizeBytes: shmTestSegSize, SegmentCount: shmTestSegCount})
	if err != nil {
		t.Fatalf("storage.NewStorage: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// shmStartServer 在 t.TempDir() 下的 unix socket 上起 shm 服务端，返回服务端与 uds。
func shmStartServer(t *testing.T, st *storage.Storage, cfg PipelineConfig) (*shmServer, string) {
	t.Helper()
	uds := filepath.Join(t.TempDir(), "taihu.sock")
	c, err := ServeShmWithConfig(st, uds, cfg)
	if err != nil {
		t.Fatalf("ServeShmWithConfig(%+v): %v", cfg, err)
	}
	srv, ok := c.(*shmServer)
	if !ok {
		t.Fatalf("ServeShmWithConfig returned %T, want *shmServer", c)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, uds
}

// shmDial 拨号一条 shm 连接并在测试结束关闭。
func shmDial(t *testing.T, uds string) *ShmConn {
	t.Helper()
	c, err := DialShm(uds, 1)
	if err != nil {
		t.Fatalf("DialShm: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// shmFillPattern 用 seed+byte(i) 填充，便于整块/抽样比对。
func shmFillPattern(b []byte, seed byte) {
	for i := range b {
		b[i] = seed + byte(i)
	}
}

// shmCheckPattern 抽样校验填充结果（大缓冲不做逐字节比对，避免测试本身成为瓶颈）。
func shmCheckPattern(t *testing.T, got []byte, seed byte) {
	t.Helper()
	const step = 1009
	for i := 0; i < len(got); i += step {
		if want := seed + byte(i); got[i] != want {
			t.Fatalf("byte %d = %d want %d", i, got[i], want)
		}
	}
}

// shmRawStream 从连接池取一条裸流并设读超时（防止协议畸形用例阻塞到测试超时）。
func shmRawStream(t *testing.T, c *ShmConn) *shmipc.Stream {
	t.Helper()
	st, err := c.sm.GetStream()
	if err != nil {
		t.Fatalf("GetStream: %v", err)
	}
	if err := st.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	return st
}

// shmReadRespCode 读一帧响应并解出错误码，校验 op 与期望一致。
func shmReadRespCode(t *testing.T, st *shmipc.Stream, wantOp protocol.OpCode) protocol.ErrCode {
	t.Helper()
	op, payload, err := shmReadFrame(st.BufferReader())
	if err != nil {
		t.Fatalf("read response frame: %v", err)
	}
	if op != wantOp {
		t.Fatalf("response op = 0x%x, want 0x%x", byte(op), byte(wantOp))
	}
	code, err := protocol.ReadU32(protocol.NewSliceReader(payload))
	if err != nil {
		t.Fatalf("decode response code: %v", err)
	}
	return protocol.ErrCode(code)
}

// TestShmSupportedLinux 覆盖 linux 变体的 ShmSupported。
func TestShmSupportedLinux(t *testing.T) {
	if !ShmSupported() {
		t.Fatal("ShmSupported() = false on linux, want true")
	}
}

// TestShmConnRoundTrip 覆盖 DialShm/ShmConn.Close 与 Put/Get/Delete/Stat 正常往返、
// 零拷贝单帧读、size=-1 读至结尾、非对齐 off 回退读、短读与越界错误、ctx 取消。
func TestShmConnRoundTrip(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmStartServer(t, st, PipelineConfig{})
	c := shmDial(t, uds)
	// 带 Deadline 的 ctx：覆盖各方法 `if d, ok := ctx.Deadline(); ok { st.SetDeadline(d) }` 分支。
	ctx, cancelDeadline := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelDeadline()

	payload := make([]byte, 4096)
	shmFillPattern(payload, 0x11)
	if err := c.Put(ctx, "rt", int64(len(payload)), payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if sz, err := c.Stat(ctx, "rt"); err != nil || sz != int64(len(payload)) {
		t.Fatalf("Stat = (%d, %v), want (%d, nil)", sz, err, len(payload))
	}

	// 整对象读取：单帧恰为请求大小 → 客户端零拷贝移交。
	got, rel, err := c.Get(ctx, "rt", 0, int64(len(payload)))
	if err != nil {
		t.Fatalf("Get(0,4096): %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("Get len = %d, want %d", len(got), len(payload))
	}
	shmCheckPattern(t, got, 0x11)
	rel()
	rel() // release 幂等

	// size=-1：读至对象结尾。
	got, rel, err = c.Get(ctx, "rt", 0, -1)
	if err != nil || len(got) != len(payload) {
		t.Fatalf("Get(0,-1) = (len %d, %v)", len(got), err)
	}
	shmCheckPattern(t, got, 0x11)
	rel()

	// 非 4K 对齐 off：服务端回退 storage.ReadAt 路径。
	got, rel, err = c.Get(ctx, "rt", 1, 100)
	if err != nil {
		t.Fatalf("Get(1,100): %v", err)
	}
	if len(got) != 100 || got[0] != payload[1] || got[99] != payload[100] {
		t.Fatalf("Get(1,100) 内容不符: len=%d", len(got))
	}
	rel()

	// size=0 短路：不触碰流。
	if b, r, err := c.Get(ctx, "rt", 0, 0); b != nil || err != nil {
		t.Fatalf("Get(0,0) = (%v, %v), want (nil, nil)", b, err)
	} else {
		r()
	}

	// 请求超过对象末尾 → 客户端短读校验报错。
	if _, _, err := c.Get(ctx, "rt", 0, 8192); err == nil {
		t.Fatal("Get 超过对象末尾应报短读错误")
	}

	// off 超过对象大小 → 服务端回 OpGetErr(InvalidRange)。
	if _, _, err := c.Get(ctx, "rt", 8192, 10); !errors.Is(err, ierr.ErrInvalidRange) {
		t.Fatalf("Get(8192,10) = %v, want ErrInvalidRange", err)
	}
	// 负 off 同样被服务端拒绝。
	if _, _, err := c.Get(ctx, "rt", -1, 10); !errors.Is(err, ierr.ErrInvalidRange) {
		t.Fatalf("Get(-1,10) = %v, want ErrInvalidRange", err)
	}
	// size=-1 且 off 超过对象大小 → 客户端本地算出负长度即拒绝（不发请求）。
	if _, _, err := c.Get(ctx, "rt", 8192, -1); !errors.Is(err, ierr.ErrInvalidRange) {
		t.Fatalf("Get(8192,-1) = %v, want ErrInvalidRange", err)
	}
	// size=-1 且 key 不存在 → Stat 失败直接返回该错误。
	if _, _, err := c.Get(ctx, "missing-key", 0, -1); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("Get(missing,-1) = %v, want ErrNotFound", err)
	}

	// 零长对象：PutHeader(size=0) → 立即 PutEnd 路径。
	if err := c.Put(ctx, "zero", 0, nil); err != nil {
		t.Fatalf("Put(size=0): %v", err)
	}
	if sz, err := c.Stat(ctx, "zero"); err != nil || sz != 0 {
		t.Fatalf("Stat(zero) = (%d, %v), want (0, nil)", sz, err)
	}

	// 客户端侧入参校验：in 不足 size。
	if err := c.Put(ctx, "short", 10, []byte{1, 2}); !errors.Is(err, ierr.ErrShortWrite) {
		t.Fatalf("Put(in 不足) = %v, want ErrShortWrite", err)
	}

	// ctx 已取消：Put/Get/Delete/Stat 均应立即返回 context.Canceled。
	cctx, cancelCtx := context.WithCancel(ctx)
	cancelCtx()
	if err := c.Put(cctx, "rt", 1, []byte{1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put(canceled) = %v", err)
	}
	if _, _, err := c.Get(cctx, "rt", 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get(canceled) = %v", err)
	}
	if err := c.Delete(cctx, "rt"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete(canceled) = %v", err)
	}
	if _, err := c.Stat(cctx, "rt"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stat(canceled) = %v", err)
	}

	// Delete：成功 / 重复删 / Stat 已删 key。
	if err := c.Delete(ctx, "rt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := c.Delete(ctx, "rt"); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("重复 Delete = %v, want ErrNotFound", err)
	}
	if _, err := c.Stat(ctx, "rt"); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("Stat(已删) = %v, want ErrNotFound", err)
	}
}

// TestShmDialSessionsFloor 覆盖 DialShm 的 sessions < 1 下限抬升。
func TestShmDialSessionsFloor(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmStartServer(t, st, PipelineConfig{})
	c, err := DialShm(uds, 0)
	if err != nil {
		t.Fatalf("DialShm(sessions=0): %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	payload := []byte("sessions-floor")
	if err := c.Put(context.Background(), "sf", int64(len(payload)), payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, rel, err := c.Get(context.Background(), "sf", 0, int64(len(payload)))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("Get = (%q, %v)", got, err)
	}
	rel()
}

// TestShmServeVariants 覆盖 ServeShm（批读关闭）与 ServeShmWithBatch（批读开启）两条装配路径：
// 前者走"逐块链路/直读"，后者 4MiB 对齐整块请求走 shmBatchReader 聚合批读。
func TestShmServeVariants(t *testing.T) {
	st := shmNewTestStorage(t)
	ctx := context.Background()
	dir := t.TempDir()

	// ServeShm → ReadBatch=0。
	udsA := filepath.Join(dir, "a.sock")
	srvA, err := ServeShm(st, udsA)
	if err != nil {
		t.Fatalf("ServeShm: %v", err)
	}
	// 显式管理该连接：必须先关客户端（否则服务端 Close 等 serveConn 退出会一直阻塞），
	// 故不交给 t.Cleanup 以避免重复关闭。
	ca, err := DialShm(udsA, 1)
	if err != nil {
		t.Fatalf("DialShm(A): %v", err)
	}
	p := make([]byte, 4096)
	shmFillPattern(p, 0x21)
	if err := ca.Put(ctx, "a", int64(len(p)), p); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, rel, err := ca.Get(ctx, "a", 0, int64(len(p)))
	if err != nil || len(got) != len(p) {
		t.Fatalf("Get = (len %d, %v)", len(got), err)
	}
	shmCheckPattern(t, got, 0x21)
	rel()
	if err := ca.Close(); err != nil {
		t.Fatalf("ca.Close: %v", err)
	}
	if err := srvA.Close(); err != nil {
		t.Fatalf("srvA.Close: %v", err)
	}

	// ServeShmWithBatch → ReadBatch>0：4MiB 请求命中批读协调器。
	udsB := filepath.Join(dir, "b.sock")
	srvB, err := ServeShmWithBatch(st, udsB, 4, 2)
	if err != nil {
		t.Fatalf("ServeShmWithBatch: %v", err)
	}
	t.Cleanup(func() { _ = srvB.Close() })
	cb := shmDial(t, udsB)
	big := make([]byte, protocol.ChunkSize)
	shmFillPattern(big, 0x31)
	if err := cb.Put(ctx, "big", int64(len(big)), big); err != nil {
		t.Fatalf("Put(4MiB): %v", err)
	}
	got, rel, err = cb.Get(ctx, "big", 0, protocol.ChunkSize)
	if err != nil || len(got) != protocol.ChunkSize {
		t.Fatalf("Get(4MiB) = (len %d, %v)", len(got), err)
	}
	shmCheckPattern(t, got, 0x31)
	rel()
}

// TestShmGetChainAndDirectPaths 覆盖 Get 的三条数据帧路径：
//   - 整 4MiB 块单链并发直读（含多槽链）；
//   - 对齐但非整块 → 单块 O_DIRECT 直读；
//   - 非对齐 / 尾块 → ReadAt 回退拷贝；
//
// 以及三条路径上"key 不存在"的错误帧回写、off==对象尾的空短读收尾。
func TestShmGetChainAndDirectPaths(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmStartServer(t, st, PipelineConfig{}) // batched == nil
	c := shmDial(t, uds)
	ctx := context.Background()

	// 8MiB 对象 → Get 全量走两槽链（shmWriteDataFramesChain）。
	const objSize = 8 << 20
	big := make([]byte, objSize)
	shmFillPattern(big, 0x41)
	if err := c.Put(ctx, "chain", int64(objSize), big); err != nil {
		t.Fatalf("Put(8MiB): %v", err)
	}
	got, rel, err := c.Get(ctx, "chain", 0, int64(objSize))
	if err != nil || len(got) != objSize {
		t.Fatalf("Get(8MiB) = (len %d, %v)", len(got), err)
	}
	shmCheckPattern(t, got, 0x41)
	rel()

	// 对齐整块（4MiB）读 → 单槽链。
	got, rel, err = c.Get(ctx, "chain", 0, protocol.ChunkSize)
	if err != nil || len(got) != protocol.ChunkSize {
		t.Fatalf("Get(4MiB 首块) = (len %d, %v)", len(got), err)
	}
	shmCheckPattern(t, got, 0x41)
	rel()

	// 对齐非整块（8KiB）→ 单块直读路径（4096 为 256 的倍数，故 seed 不变）。
	got, rel, err = c.Get(ctx, "chain", 4096, 8192)
	if err != nil || len(got) != 8192 {
		t.Fatalf("Get(4096,8192) = (len %d, %v)", len(got), err)
	}
	shmCheckPattern(t, got, 0x41)
	rel()

	// 非对齐 off → ReadAt 回退拷贝路径。
	got, rel, err = c.Get(ctx, "chain", 101, 4096)
	if err != nil || len(got) != 4096 {
		t.Fatalf("Get(101,4096) = (len %d, %v)", len(got), err)
	}
	shmCheckPattern(t, got, 0x41+byte(101))
	rel()

	// 非对齐 off 且跨块（size > ChunkSize）：ReadAt 回退循环的块大小夹取与末尾 EOF 收尾。
	got, rel, err = c.Get(ctx, "chain", 1, objSize-1)
	if err != nil || len(got) != objSize-1 {
		t.Fatalf("Get(1,%d) = (len %d, %v)", objSize-1, len(got), err)
	}
	shmCheckPattern(t, got, 0x41+byte(1))
	rel()

	// off == 对象尾 → 直读空短读，末帧 final，客户端短读报错。
	if _, _, err := c.Get(ctx, "chain", objSize, 8192); err == nil {
		t.Fatal("off 位于对象尾应报短读错误")
	}

	// key 不存在：
	//   - 对齐整块 → 链内 ReadAtInto 报错 → OpGetErr；
	//   - 对齐小块 → 直读报错 → OpGetErr；
	//   - 非对齐 → ReadAt 报错 → OpGetErr。
	if _, _, err := c.Get(ctx, "missing", 0, protocol.ChunkSize); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("Get(missing,4MiB) = %v, want ErrNotFound", err)
	}
	if _, _, err := c.Get(ctx, "missing", 0, 8192); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("Get(missing,8KiB) = %v, want ErrNotFound", err)
	}
	if _, _, err := c.Get(ctx, "missing", 1, 100); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("Get(missing,非对齐) = %v, want ErrNotFound", err)
	}
}

// TestShmPipelineConfig 覆盖 ServeShmWithConfig 同时启用读/写/删三条流水线时的全链路：
// 写（batchWriter：整对象攒批 AppendBatch + BatchPutCommit）、读（shmBatchReader 聚合批读）、
// 删（batchDeleter：一次 BatchDelete）；含零长对象走 size==0 分支。
func TestShmPipelineConfig(t *testing.T) {
	st := shmNewTestStorage(t)
	cfg := PipelineConfig{
		ReadBatch: 4, ReadWorkers: 2,
		WriteBatch: 4, WriteWorkers: 2,
		DeleteBatch: 4, DeleteWorkers: 2,
	}
	_, uds := shmStartServer(t, st, cfg)
	c := shmDial(t, uds)
	ctx := context.Background()

	// 写流水线 + 普通大小对象。
	p := make([]byte, 4096)
	shmFillPattern(p, 0x51)
	if err := c.Put(ctx, "p1", int64(len(p)), p); err != nil {
		t.Fatalf("Put(p1): %v", err)
	}
	if sz, err := c.Stat(ctx, "p1"); err != nil || sz != int64(len(p)) {
		t.Fatalf("Stat(p1) = (%d, %v)", sz, err)
	}

	// 写流水线 + 4MiB 对象，随后 4MiB 对齐整块读走批读协调器。
	big := make([]byte, protocol.ChunkSize)
	shmFillPattern(big, 0x61)
	if err := c.Put(ctx, "p2", int64(len(big)), big); err != nil {
		t.Fatalf("Put(p2): %v", err)
	}
	got, rel, err := c.Get(ctx, "p2", 0, protocol.ChunkSize)
	if err != nil || len(got) != protocol.ChunkSize {
		t.Fatalf("Get(p2) = (len %d, %v)", len(got), err)
	}
	shmCheckPattern(t, got, 0x61)
	rel()

	// 批读协调器上的错误回写：key 不存在。
	if _, _, err := c.Get(ctx, "p-missing", 0, protocol.ChunkSize); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("Get(p-missing) = %v, want ErrNotFound", err)
	}

	// 写流水线的 size==0 分支。
	if err := c.Put(ctx, "p0", 0, nil); err != nil {
		t.Fatalf("Put(size=0): %v", err)
	}

	// 删流水线：成功 + 不存在。
	if err := c.Delete(ctx, "p1"); err != nil {
		t.Fatalf("Delete(p1): %v", err)
	}
	if err := c.Delete(ctx, "p2"); err != nil {
		t.Fatalf("Delete(p2): %v", err)
	}
	if err := c.Delete(ctx, "p1"); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("Delete(p1) 二次 = %v, want ErrNotFound", err)
	}
}

// TestShmPutBeginZeroCopy 覆盖零拷贝写全链路：PutBegin → Reserve 直写共享内存 → Commit，
// 含 Write（内部 Reserve+copy）、跨帧切帧（curLen+n > ChunkSize）与 size=-1 拒绝。
func TestShmPutBeginZeroCopy(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmStartServer(t, st, PipelineConfig{})
	c := shmDial(t, uds)
	// 带 Deadline 的 ctx：覆盖 PutBegin 的 SetDeadline 分支。
	ctx, cancelDeadline := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelDeadline()

	if _, err := c.PutBegin(ctx, "neg", -1); !errors.Is(err, ierr.ErrInvalidRange) {
		t.Fatalf("PutBegin(size=-1) = %v, want ErrInvalidRange", err)
	}

	// Reserve 直写 + Commit。
	const n = 8192
	w, err := c.PutBegin(ctx, "zc", n)
	if err != nil {
		t.Fatalf("PutBegin: %v", err)
	}
	buf, err := w.Reserve(n)
	if err != nil || len(buf) != n {
		t.Fatalf("Reserve(%d) = (len %d, %v)", n, len(buf), err)
	}
	shmFillPattern(buf, 0x71)
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if sz, err := c.Stat(ctx, "zc"); err != nil || sz != n {
		t.Fatalf("Stat(zc) = (%d, %v)", sz, err)
	}
	got, rel, err := c.Get(ctx, "zc", 0, n)
	if err != nil || len(got) != n {
		t.Fatalf("Get(zc) = (len %d, %v)", len(got), err)
	}
	shmCheckPattern(t, got, 0x71)
	rel()

	// Write（内部 Reserve + copy）。
	w2, err := c.PutBegin(ctx, "zc2", 4096)
	if err != nil {
		t.Fatalf("PutBegin(zc2): %v", err)
	}
	p := make([]byte, 4096)
	shmFillPattern(p, 0x72)
	if k, err := w2.Write(p); err != nil || k != len(p) {
		t.Fatalf("Write = (%d, %v)", k, err)
	}
	if err := w2.Commit(); err != nil {
		t.Fatalf("Commit(zc2): %v", err)
	}
	if sz, err := c.Stat(ctx, "zc2"); err != nil || sz != 4096 {
		t.Fatalf("Stat(zc2) = (%d, %v)", sz, err)
	}

	// 跨帧：Reserve(ChunkSize) 写满一帧后再次 Reserve 触发切帧（shmCommitFrame）。
	total := int64(protocol.ChunkSize) + 4096
	w3, err := c.PutBegin(ctx, "zc3", total)
	if err != nil {
		t.Fatalf("PutBegin(zc3): %v", err)
	}
	b1, err := w3.Reserve(protocol.ChunkSize)
	if err != nil || len(b1) != protocol.ChunkSize {
		t.Fatalf("Reserve(ChunkSize) = (len %d, %v)", len(b1), err)
	}
	shmFillPattern(b1, 0x73)
	b2, err := w3.Reserve(4096)
	if err != nil || len(b2) != 4096 {
		t.Fatalf("Reserve(4096) 第二帧 = (len %d, %v)", len(b2), err)
	}
	shmFillPattern(b2, 0x74)
	if err := w3.Commit(); err != nil {
		t.Fatalf("Commit(zc3): %v", err)
	}
	if sz, err := c.Stat(ctx, "zc3"); err != nil || sz != total {
		t.Fatalf("Stat(zc3) = (%d, %v), want %d", sz, err, total)
	}
}

// TestShmPutWriterReserveErrors 覆盖 ShmPutWriter 的错误边界：Reserve(0)/Reserve(-1)
// 返回空且不出错、未写满即 Commit 报 ErrShortWrite、Reserve 超 ChunkSize 报错且粘滞
// （后续 Reserve 直接复用该错误）。每个用例用独立连接，避免"流上残留未消费帧"污染后续请求。
func TestShmPutWriterReserveErrors(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmStartServer(t, st, PipelineConfig{})
	ctx := context.Background()

	for _, n := range []int{0, -1} {
		c := shmDial(t, uds)
		w, err := c.PutBegin(ctx, "edge", 4096)
		if err != nil {
			t.Fatalf("PutBegin(Reserve %d): %v", n, err)
		}
		b, err := w.Reserve(n)
		if b != nil || err != nil {
			t.Fatalf("Reserve(%d) = (%v, %v), want (nil, nil)", n, b, err)
		}
		if err := w.Commit(); !errors.Is(err, ierr.ErrShortWrite) {
			t.Fatalf("Commit(未写满) = %v, want ErrShortWrite", err)
		}
	}

	// Reserve 超过单帧上限：报错且粘滞。
	c := shmDial(t, uds)
	w, err := c.PutBegin(ctx, "too-big", int64(protocol.ChunkSize)+1)
	if err != nil {
		t.Fatalf("PutBegin: %v", err)
	}
	if _, err := w.Reserve(protocol.ChunkSize + 1); err == nil {
		t.Fatal("Reserve(>ChunkSize) 应报错")
	}
	if _, err := w.Reserve(1); err == nil {
		t.Fatal("出错后 Reserve 应返回同一错误")
	}
	if err := w.Commit(); err == nil {
		t.Fatal("出错后 Commit 应返回该错误")
	}

	// Write 超过单帧上限同样报错。
	c2 := shmDial(t, uds)
	w2, err := c2.PutBegin(ctx, "too-big-write", int64(protocol.ChunkSize)+1)
	if err != nil {
		t.Fatalf("PutBegin: %v", err)
	}
	if _, err := w2.Write(make([]byte, protocol.ChunkSize+1)); err == nil {
		t.Fatal("Write(>ChunkSize) 应报错")
	}
	if err := w2.Commit(); err == nil {
		t.Fatal("出错后 Commit 应返回该错误")
	}
}

// TestShmServerProtocolErrors 用裸流向服务端投递各类畸形/非法请求，覆盖 handleStream
// 各 handler 的错误码回写（shmRespErr）与 shmReadFrame 的解析失败分支。
func TestShmServerProtocolErrors(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmStartServer(t, st, PipelineConfig{})
	c := shmDial(t, uds)
	ctx := context.Background()

	seed := []byte("seed-data")
	if err := c.Put(ctx, "seed", int64(len(seed)), seed); err != nil {
		t.Fatalf("Put(seed): %v", err)
	}
	maxSize := st.MaxObjectSize()

	cases := []struct {
		name   string
		run    func(st *shmipc.Stream)
		wantOp protocol.OpCode
		want   protocol.ErrCode
	}{
		{
			"put-truncated-header",
			func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpPutHeader, []byte{0, 0}) },
			protocol.OpResp, protocol.CodeInvalidArgument,
		},
		{
			"put-negative-size",
			func(st *shmipc.Stream) {
				_ = shmWriteFrame(st, protocol.OpPutHeader, protocol.EncodePutHeader("neg", -1))
			},
			protocol.OpResp, protocol.CodeInvalidArgument,
		},
		{
			"put-too-large",
			func(st *shmipc.Stream) {
				_ = shmWriteFrame(st, protocol.OpPutHeader, protocol.EncodePutHeader("huge", maxSize+1))
			},
			protocol.OpResp, protocol.CodeTooLarge,
		},
		{
			"put-data-overflow",
			func(st *shmipc.Stream) {
				_ = shmWriteFrame(st, protocol.OpPutHeader, protocol.EncodePutHeader("ovf", 10))
				_ = shmWriteFrame(st, protocol.OpPutData, make([]byte, 20))
			},
			protocol.OpResp, protocol.CodeInvalidArgument,
		},
		{
			"put-end-size-mismatch",
			func(st *shmipc.Stream) {
				_ = shmWriteFrame(st, protocol.OpPutHeader, protocol.EncodePutHeader("mm", 10))
				_ = shmWriteFrame(st, protocol.OpPutData, make([]byte, 4))
				_ = shmWriteFrame(st, protocol.OpPutEnd, nil)
			},
			protocol.OpResp, protocol.CodeInvalidArgument,
		},
		{
			"put-zero-size-bad-op",
			func(st *shmipc.Stream) {
				_ = shmWriteFrame(st, protocol.OpPutHeader, protocol.EncodePutHeader("z", 0))
				_ = shmWriteFrame(st, protocol.OpPutData, make([]byte, 1))
			},
			protocol.OpResp, protocol.CodeInvalidArgument,
		},
		{
			"put-unknown-op-in-data",
			func(st *shmipc.Stream) {
				_ = shmWriteFrame(st, protocol.OpPutHeader, protocol.EncodePutHeader("uo", 8))
				_ = shmWriteFrame(st, protocol.OpResp, protocol.EncCode(protocol.CodeOK))
			},
			protocol.OpResp, protocol.CodeInvalidArgument,
		},
		{
			"del-truncated",
			func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpDelReq, []byte{1}) },
			protocol.OpResp, protocol.CodeInvalidArgument,
		},
		{
			"stat-truncated",
			func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpStatReq, []byte{1}) },
			protocol.OpResp, protocol.CodeInvalidArgument,
		},
		{
			"get-truncated",
			func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpGetReq, []byte{1, 2}) },
			protocol.OpGetErr, protocol.CodeInvalidArgument,
		},
		{
			"get-off-beyond-with-size-minus1",
			func(st *shmipc.Stream) {
				_ = shmWriteFrame(st, protocol.OpGetReq, protocol.EncodeGetReq("seed", 1<<20, -1))
			},
			protocol.OpGetErr, protocol.CodeInvalidRange,
		},
		{
			// size==-1 且 key 不存在：handleShmGet 的 Stat 失败分支。
			"get-size-minus1-missing-key",
			func(st *shmipc.Stream) {
				_ = shmWriteFrame(st, protocol.OpGetReq, protocol.EncodeGetReq("nope", 0, -1))
			},
			protocol.OpGetErr, protocol.CodeNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := shmRawStream(t, c)
			defer func() { _ = stream.Close() }()
			tc.run(stream)
			if got := shmReadRespCode(t, stream, tc.wantOp); got != tc.want {
				t.Fatalf("code = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestShmHandleStreamUnknownOpcode 覆盖 handleStream 的 default 分支：未知 opcode 不写
// 响应帧，直接关闭流（客户端读报错），避免残留帧污染可复用流。
func TestShmHandleStreamUnknownOpcode(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmStartServer(t, st, PipelineConfig{})
	c := shmDial(t, uds)

	stream := shmRawStream(t, c)
	defer func() { _ = stream.Close() }()
	if err := shmWriteFrame(stream, protocol.OpCode(0x7e), nil); err != nil {
		t.Fatalf("shmWriteFrame: %v", err)
	}
	if _, _, err := shmReadFrame(stream.BufferReader()); err == nil {
		t.Fatal("未知 opcode 后服务端应关闭流，客户端读应报错")
	}
}

// TestShmServeListenError 覆盖 ServeShm* 的 listen 失败分支（uds 父目录不存在）。
func TestShmServeListenError(t *testing.T) {
	st := shmNewTestStorage(t)
	bad := filepath.Join(t.TempDir(), "no-such-dir", "x.sock")

	if _, err := ServeShmWithConfig(st, bad, PipelineConfig{}); err == nil {
		t.Fatal("ServeShmWithConfig(bad uds) 应报错")
	}
	if _, err := ServeShm(st, bad); err == nil {
		t.Fatal("ServeShm(bad uds) 应报错")
	}
	if _, err := ServeShmWithBatch(st, bad, 1, 1); err == nil {
		t.Fatal("ServeShmWithBatch(bad uds) 应报错")
	}
}

// TestShmServerCloseIdempotent 覆盖 shmServer.Close 的 closeOnce 幂等与 wg 收敛。
func TestShmServerCloseIdempotent(t *testing.T) {
	st := shmNewTestStorage(t)
	srv, _ := shmStartServer(t, st, PipelineConfig{})
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("第二次 Close: %v", err)
	}
}

// TestShmServeConnBadHandshake 覆盖 serveConn 的 shmipc 会话初始化失败分支：
// 连上 unix socket 但发送非法握手，session 建立失败即静默退出，服务仍可正常关闭。
func TestShmServeConnBadHandshake(t *testing.T) {
	st := shmNewTestStorage(t)
	srv, uds := shmStartServer(t, st, PipelineConfig{})

	conn, err := net.Dial("unix", uds)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	if _, err := conn.Write([]byte("not-a-shmipc-handshake")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = conn.Close()

	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestShmBatchReaderConstruction 覆盖 newShmBatchReader 的 target<=0 关闭、target 上限
// 截断与 workers 下限抬升。
func TestShmBatchReaderConstruction(t *testing.T) {
	st := shmNewTestStorage(t)

	if b := newShmBatchReader(st, 0, 4); b != nil {
		t.Fatal("newShmBatchReader(target=0) 应为 nil")
	}
	b := newShmBatchReader(st, 300, 0)
	if b == nil {
		t.Fatal("newShmBatchReader(target=300) 不应为 nil")
	}
	if b.target != 256 {
		t.Fatalf("target = %d, want 256（上限截断）", b.target)
	}
	if len(b.workers) != 1 {
		t.Fatalf("workers = %d, want 1（下限抬升）", len(b.workers))
	}
}

// shmFakeCallback 假服务端的 ListenCallback：每接受一条新流即按 respond 写回伪造响应帧。
type shmFakeCallback struct {
	respond func(*shmipc.Stream)
}

func (c shmFakeCallback) OnNewStream(s *shmipc.Stream) { c.respond(s) }

func (c shmFakeCallback) OnShutdown(string) {}

// shmStartFakeServer 起一个"假"shm 服务端：只完成 shmipc 会话握手、不解析请求，
// 每接受一条新流即按 respond 写回伪造响应。真实服务端不会产生这些畸形响应，故只能伪造。
// 返回 unix socket 路径；调用方须保证客户端先关闭（t.Cleanup 后注册者先执行）。
func shmStartFakeServer(t *testing.T, respond func(*shmipc.Stream)) string {
	t.Helper()
	uds := filepath.Join(t.TempDir(), "fake.sock")
	ln, err := shmipc.NewListener(shmFakeCallback{respond: respond}, shmipc.NewDefaultListenerConfig(uds, "unix"))
	if err != nil {
		t.Fatalf("shmipc.NewListener: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ln.Run()
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return uds
}

// TestShmClientProtocolAnomalies 覆盖客户端在协议异常响应下的分支：响应 op 不符、
// 响应码截断、Get 超限/未知 op/错误码截断/非 final 续帧、Stat 未知 op 与截断。
// 每条子用例独立连接（残留帧不污染其它用例），且 ctx 带 Deadline（既是超时兜底，
// 也覆盖客户端的 SetDeadline 分支）。
func TestShmClientProtocolAnomalies(t *testing.T) {
	cases := []struct {
		name    string
		respond func(*shmipc.Stream)
		run     func(c *ShmConn, ctx context.Context) error
		wantErr string // 期望错误信息包含的子串；空表示只要求返回错误
	}{
		{
			name:    "put-op-mismatch",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpStatResp, make([]byte, 8)) },
			run:     func(c *ShmConn, ctx context.Context) error { return c.Put(ctx, "k", 1, []byte{1}) },
			wantErr: "unexpected put response op",
		},
		{
			name:    "put-resp-truncated",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpResp, []byte{0}) },
			run:     func(c *ShmConn, ctx context.Context) error { return c.Put(ctx, "k", 1, []byte{1}) },
		},
		{
			name:    "get-unknown-op",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpStatResp, make([]byte, 8)) },
			run: func(c *ShmConn, ctx context.Context) error {
				_, _, err := c.Get(ctx, "k", 0, 4)
				return err
			},
			wantErr: "unexpected get frame op",
		},
		{
			name:    "get-exceeds-size",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpGetData, make([]byte, 8)) },
			run: func(c *ShmConn, ctx context.Context) error {
				_, _, err := c.Get(ctx, "k", 0, 4)
				return err
			},
			wantErr: "get stream exceeds requested size",
		},
		{
			// 首帧恰为请求大小但非 final（协议异常，走零拷贝 continue 分支），
			// 次帧超出请求大小 → 报超限。
			name: "get-nonfinal-then-exceed",
			respond: func(st *shmipc.Stream) {
				_ = shmWriteFrame(st, protocol.OpGetData, make([]byte, 4))
				_ = shmWriteFrame(st, protocol.OpGetData, []byte{1})
			},
			run: func(c *ShmConn, ctx context.Context) error {
				_, _, err := c.Get(ctx, "k", 0, 4)
				return err
			},
			wantErr: "get stream exceeds requested size",
		},
		{
			name:    "get-err-truncated",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpGetErr, []byte{0}) },
			run: func(c *ShmConn, ctx context.Context) error {
				_, _, err := c.Get(ctx, "k", 0, 4)
				return err
			},
		},
		{
			name:    "delete-op-mismatch",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpStatResp, make([]byte, 8)) },
			run:     func(c *ShmConn, ctx context.Context) error { return c.Delete(ctx, "k") },
			wantErr: "unexpected delete response op",
		},
		{
			name:    "delete-resp-truncated",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpResp, []byte{0}) },
			run:     func(c *ShmConn, ctx context.Context) error { return c.Delete(ctx, "k") },
		},
		{
			name:    "stat-unknown-op",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpCode(0x6f), make([]byte, 4)) },
			run: func(c *ShmConn, ctx context.Context) error {
				_, err := c.Stat(ctx, "k")
				return err
			},
			wantErr: "unexpected stat response op",
		},
		{
			name:    "stat-size-truncated",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpStatResp, []byte{0, 0}) },
			run: func(c *ShmConn, ctx context.Context) error {
				_, err := c.Stat(ctx, "k")
				return err
			},
		},
		{
			name:    "stat-code-truncated",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpResp, []byte{0}) },
			run: func(c *ShmConn, ctx context.Context) error {
				_, err := c.Stat(ctx, "k")
				return err
			},
		},
		{
			name:    "commit-op-mismatch",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpStatResp, make([]byte, 8)) },
			run: func(c *ShmConn, ctx context.Context) error {
				w, err := c.PutBegin(ctx, "k", 0)
				if err != nil {
					return err
				}
				return w.Commit()
			},
			wantErr: "unexpected put response op",
		},
		{
			name:    "commit-resp-truncated",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpResp, []byte{0}) },
			run: func(c *ShmConn, ctx context.Context) error {
				w, err := c.PutBegin(ctx, "k", 0)
				if err != nil {
					return err
				}
				return w.Commit()
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uds := shmStartFakeServer(t, tc.respond)
			c, err := DialShm(uds, 1)
			if err != nil {
				t.Fatalf("DialShm: %v", err)
			}
			// t.Cleanup 逆序执行：客户端先关，再关假服务端（避免服务端会话等待对端）。
			t.Cleanup(func() { _ = c.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err = tc.run(c, ctx)
			if err == nil {
				t.Fatal("协议异常响应应使客户端报错")
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want 包含 %q", err, tc.wantErr)
			}
		})
	}
}

// ===== 异步写流水线（inflight>0 + batchWriter）测试 =====
//
// 以下用例覆盖新增的异步写路径：服务端 inflight>0 时 PutEnd 不写 OpResp、任务投
// batchWriter 异步排空、pend 保序队列按请求到达顺序写响应、延迟批量释放共享内存。

// shmAsyncServer 起一个启用异步读写的 shm 服务端（inflight=8 + 写流水线）。
func shmAsyncServer(t *testing.T, st *storage.Storage) (*shmServer, string) {
	t.Helper()
	return shmStartServer(t, st, PipelineConfig{
		Inflight:    8,
		WriteBatch:  4,
		WriteWorkers: 2,
	})
}

// TestShmAsyncPutConsistency 数据一致性核心用例：PutBatch（普通大小 + 4MiB 整块）
// 异步写 → Stat 确认落盘 → GetBatch 逐字节读回校验。同时覆盖 4MiB 对象经异步读
// 流水线（trySubmitAsyncGet）读回的组合路径。
func TestShmAsyncPutConsistency(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	// 普通大小对象批。
	small := make([]byte, 8192)
	shmFillPattern(small, 0x81)
	smallKeys := []string{"apc/s0", "apc/s1", "apc/s2", "apc/s3", "apc/s4", "apc/s5", "apc/s6", "apc/s7"}
	if err := c.PutBatch(ctx, smallKeys, int64(len(small)), small); err != nil {
		t.Fatalf("PutBatch(small): %v", err)
	}
	for _, k := range smallKeys {
		if sz, err := c.Stat(ctx, k); err != nil || sz != int64(len(small)) {
			t.Fatalf("Stat(%s) = (%d, %v), want (%d, nil)", k, sz, err, len(small))
		}
	}
	outs, rel, err := c.GetBatch(ctx, smallKeys, 0, int64(len(small)))
	if err != nil {
		t.Fatalf("GetBatch(small): %v", err)
	}
	for i, k := range smallKeys {
		if len(outs[i]) != len(small) {
			t.Fatalf("GetBatch(%s) len = %d, want %d", k, len(outs[i]), len(small))
		}
		shmCheckPattern(t, outs[i], 0x81)
	}
	rel()

	// 4MiB 整块对象批：PutBatch 异步写 → GetBatch 异步读（trySubmitAsyncGet 快路径）。
	big := make([]byte, protocol.ChunkSize)
	shmFillPattern(big, 0x82)
	bigKeys := []string{"apc/b0", "apc/b1", "apc/b2", "apc/b3"}
	if err := c.PutBatch(ctx, bigKeys, int64(len(big)), big); err != nil {
		t.Fatalf("PutBatch(4MiB): %v", err)
	}
	for _, k := range bigKeys {
		if sz, err := c.Stat(ctx, k); err != nil || sz != int64(len(big)) {
			t.Fatalf("Stat(%s) = (%d, %v), want (%d, nil)", k, sz, err, len(big))
		}
	}
	outs, rel, err = c.GetBatch(ctx, bigKeys, 0, protocol.ChunkSize)
	if err != nil {
		t.Fatalf("GetBatch(4MiB): %v", err)
	}
	for i, k := range bigKeys {
		if len(outs[i]) != protocol.ChunkSize {
			t.Fatalf("GetBatch(%s) len = %d, want %d", k, len(outs[i]), protocol.ChunkSize)
		}
		shmCheckPattern(t, outs[i], 0x82)
	}
	rel()
}

// TestShmAsyncPutGetOrder 响应保序用例（裸流）：同一流上连发 2 个小 Put + 1 个 4MiB
// Put + 1 个 GetReq(小) + 1 个 GetReq(4MiB)，验证响应严格按请求到达顺序写出：
// OpResp ×3 → OpGetDataFinal(小) → OpGetDataFinal(4MiB)。GetReq 到达时写任务可能
// 未完成（映射未提交）：无论排空后同步读还是异步入队，响应序必须一致。
func TestShmAsyncPutGetOrder(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)

	stream := shmRawStream(t, c)
	defer func() { _ = stream.Close() }()

	small := make([]byte, 4096)
	shmFillPattern(small, 0x91)
	big := make([]byte, protocol.ChunkSize)
	shmFillPattern(big, 0x92)

	writePut := func(key string, data []byte) {
		t.Helper()
		if err := shmWriteFrame(stream, protocol.OpPutHeader, protocol.EncodePutHeader(key, int64(len(data)))); err != nil {
			t.Fatalf("PutHeader(%s): %v", key, err)
		}
		if err := shmWriteFrame(stream, protocol.OpPutData, data); err != nil {
			t.Fatalf("PutData(%s): %v", key, err)
		}
		if err := shmWriteFrame(stream, protocol.OpPutEnd, nil); err != nil {
			t.Fatalf("PutEnd(%s): %v", key, err)
		}
	}
	writePut("ord/a", small)
	writePut("ord/b", small)
	writePut("ord/c", big)
	if err := shmWriteFrame(stream, protocol.OpGetReq, protocol.EncodeGetReq("ord/a", 0, int64(len(small)))); err != nil {
		t.Fatalf("GetReq(a): %v", err)
	}
	if err := shmWriteFrame(stream, protocol.OpGetReq, protocol.EncodeGetReq("ord/c", 0, protocol.ChunkSize)); err != nil {
		t.Fatalf("GetReq(c): %v", err)
	}

	// 3 个 Put 的 OpResp 必须按到达顺序返回且全部成功。
	for i := 0; i < 3; i++ {
		if got := shmReadRespCode(t, stream, protocol.OpResp); got != protocol.CodeOK {
			t.Fatalf("OpResp[%d] code = %d, want CodeOK", i, got)
		}
	}
	// Get(a)：单帧 4096。
	op, payload, err := shmReadFrame(stream.BufferReader())
	if err != nil {
		t.Fatalf("read Get(a) frame: %v", err)
	}
	if op != protocol.OpGetDataFinal {
		t.Fatalf("Get(a) op = 0x%x, want OpGetDataFinal", byte(op))
	}
	if len(payload) != len(small) {
		t.Fatalf("Get(a) len = %d, want %d", len(payload), len(small))
	}
	shmCheckPattern(t, payload, 0x91)
	// Get(c)：单帧 4MiB（异步读快路径）。
	op, payload, err = shmReadFrame(stream.BufferReader())
	if err != nil {
		t.Fatalf("read Get(c) frame: %v", err)
	}
	if op != protocol.OpGetDataFinal {
		t.Fatalf("Get(c) op = 0x%x, want OpGetDataFinal", byte(op))
	}
	if len(payload) != protocol.ChunkSize {
		t.Fatalf("Get(c) len = %d, want %d", len(payload), protocol.ChunkSize)
	}
	shmCheckPattern(t, payload, 0x92)
}

// TestShmAsyncPutOverwrite 覆盖写用例：同一 key 经异步 PutBatch 反复写不同内容，
// 读回必须是最近一次写入的数据（映射覆盖正确、无旧数据残留）。
func TestShmAsyncPutOverwrite(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	keys := []string{"ovw/k"}
	write := func(seed byte) {
		t.Helper()
		p := make([]byte, 8192)
		shmFillPattern(p, seed)
		if err := c.PutBatch(ctx, keys, int64(len(p)), p); err != nil {
			t.Fatalf("PutBatch(seed %#x): %v", seed, err)
		}
	}
	verify := func(seed byte) {
		t.Helper()
		outs, rel, err := c.GetBatch(ctx, keys, 0, 8192)
		if err != nil {
			t.Fatalf("GetBatch: %v", err)
		}
		defer rel()
		if len(outs[0]) != 8192 {
			t.Fatalf("len = %d, want 8192", len(outs[0]))
		}
		shmCheckPattern(t, outs[0], seed)
	}

	// 三次覆盖，每次后读回都必须是最新内容。
	write(0xA1)
	verify(0xA1)
	write(0xB2)
	verify(0xB2)
	write(0xC3)
	verify(0xC3)
	if sz, err := c.Stat(ctx, keys[0]); err != nil || sz != 8192 {
		t.Fatalf("Stat = (%d, %v), want (8192, nil)", sz, err)
	}
}

// TestShmAsyncPutConcurrent 并发用例：多 goroutine 同时经异步 PutBatch 写入各自
// 独立 key 区间，全部完成后并发 GetBatch 读回逐字节校验。覆盖跨流并发下 rr 轮转
// 分发、多 worker 并行取批与共享内存延迟批量释放的线程安全。
func TestShmAsyncPutConcurrent(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	const (
		writers = 16
		perW    = 8
	)
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for g := 0; g < writers; g++ {
		keys := make([]string, perW)
		payload := make([]byte, 4096)
		shmFillPattern(payload, byte(0x30+g))
		for i := range keys {
			keys[i] = fmt.Sprintf("conc/g%d/%d", g, i)
		}
		wg.Add(1)
		go func(g int, keys []string, payload []byte) {
			defer wg.Done()
			errs[g] = c.PutBatch(ctx, keys, int64(len(payload)), payload)
		}(g, keys, payload)
	}
	wg.Wait()
	for g, err := range errs {
		if err != nil {
			t.Fatalf("PutBatch(g%d): %v", g, err)
		}
	}
	// 并发读回全部 key 并校验。
	var rwg sync.WaitGroup
	rerrs := make([]error, writers)
	for g := 0; g < writers; g++ {
		keys := make([]string, perW)
		for i := range keys {
			keys[i] = fmt.Sprintf("conc/g%d/%d", g, i)
		}
		rwg.Add(1)
		go func(g int, keys []string) {
			defer rwg.Done()
			outs, rel, err := c.GetBatch(ctx, keys, 0, 4096)
			if err == nil {
				for i := range outs {
					if len(outs[i]) != 4096 {
						err = fmt.Errorf("key %s: len %d != 4096", keys[i], len(outs[i]))
						break
					}
					shmCheckPattern(t, outs[i], byte(0x30+g))
				}
				rel()
			}
			rerrs[g] = err
		}(g, keys)
	}
	rwg.Wait()
	for g, err := range rerrs {
		if err != nil {
			t.Fatalf("GetBatch(g%d): %v", g, err)
		}
	}
}

// ===== 数据一致性专项（异步读写接口） =====
//
// 以下用例按数据一致性风险维度组织，与上方既有异步用例（一致性/保序/覆盖写/并发/稳定性）
// 构成完整的数据一致性套件：
//   - 大小边界矩阵：单帧/多帧/跨帧切帧、零长对象的数据完整性；
//   - 写-读交错可见性：异步写提交后（DMA 完成窗口内）同流后续请求读必可见最新值；
//   - 零长对象保序：size==0 立即完成项混入 pend 队列不越序；
//   - backpressure 路径：inflight 满触发 drainAll 排空，无丢写/乱序；
//   - 错误后恢复：CodeTooLarge 关流不污染后续请求；
//   - 并发覆盖无撕裂：并发写同一 key，最终值必须完整属于某一次写入。

// shmAsyncServerInflight 起一个指定 inflight 的异步读写 shm 服务端（backpressure 用例用）。
func shmAsyncServerInflight(t *testing.T, st *storage.Storage, inflight int) (*shmServer, string) {
	t.Helper()
	return shmStartServer(t, st, PipelineConfig{
		Inflight:     inflight,
		WriteBatch:   4,
		WriteWorkers: 2,
	})
}

// shmMatchesPattern 整缓冲抽样校验（与 shmCheckPattern 同 stride），返回是否全部匹配
// （不 t.Fatal，供「读回值必须属于某次写入」这类多候选校验）。
func shmMatchesPattern(b []byte, seed byte) bool {
	const step = 1009
	for i := 0; i < len(b); i += step {
		if b[i] != seed+byte(i) {
			return false
		}
	}
	return true
}

// TestShmAsyncPutSizeMatrix 大小边界矩阵：{0,1,4095,4096,4MiB,4MiB+1,8MiB-1,8MiB} 覆盖
// 单帧、多帧、跨帧切帧（4MiB+1 两帧）与 8MiB 双槽整块；零长对象走 size==0 立即完成项分支。
// 全部经异步写后读回，校验长度与首尾/抽样内容。
func TestShmAsyncPutSizeMatrix(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	sizes := []int64{0, 1, 4095, 4096, protocol.ChunkSize, protocol.ChunkSize + 1, 8<<20 - 1, 8 << 20}
	keys := make([]string, len(sizes))
	for i, size := range sizes {
		key := fmt.Sprintf("mat/%d", i)
		keys[i] = key
		seed := byte(0x20 + i)
		if size == 0 {
			// size==0：PutBatch 短路（不发任何帧），用单 key Put 走 size==0 立即完成分支。
			if err := c.Put(ctx, key, 0, nil); err != nil {
				t.Fatalf("Put(%s, size=0): %v", key, err)
			}
			continue
		}
		p := make([]byte, size)
		shmFillPattern(p, seed)
		if err := c.PutBatch(ctx, []string{key}, size, p); err != nil {
			t.Fatalf("PutBatch(%s, size=%d): %v", key, size, err)
		}
	}
	// 读回校验：非零对象 GetBatch 全量抽样；零长对象 Get(size=-1) 应为空。
	for i, size := range sizes {
		seed := byte(0x20 + i)
		if size == 0 {
			got, rel, err := c.Get(ctx, keys[i], 0, -1)
			if err != nil || len(got) != 0 {
				t.Fatalf("Get(%s, size=0) = (len %d, %v), want (0, nil)", keys[i], len(got), err)
			}
			rel()
			continue
		}
		outs, rel, err := c.GetBatch(ctx, []string{keys[i]}, 0, size)
		if err != nil {
			t.Fatalf("GetBatch(%s, size=%d): %v", keys[i], size, err)
		}
		if int64(len(outs[0])) != size {
			t.Fatalf("GetBatch(%s) len = %d, want %d", keys[i], len(outs[0]), size)
		}
		if outs[0][0] != seed || outs[0][len(outs[0])-1] != seed+byte(size-1) {
			t.Fatalf("GetBatch(%s) 首尾字节不符", keys[i])
		}
		shmCheckPattern(t, outs[0], seed)
		rel()
	}
}

// TestShmAsyncPutReadWriteInterleave 写-读交错可见性：同一 key 反复异步写→立即读回，
// 每次读必须返回最近一次提交的完整内容（异步写提交后同流后续请求读必可见）。
func TestShmAsyncPutReadWriteInterleave(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	const iters = 8
	for i := 0; i < iters; i++ {
		size := int64(4096 * (i + 1))
		seed := byte(0x40 + i)
		p := make([]byte, size)
		shmFillPattern(p, seed)
		if err := c.PutBatch(ctx, []string{"iw/k"}, size, p); err != nil {
			t.Fatalf("PutBatch(iter %d): %v", i, err)
		}
		outs, rel, err := c.GetBatch(ctx, []string{"iw/k"}, 0, size)
		if err != nil {
			t.Fatalf("GetBatch(iter %d): %v", i, err)
		}
		if int64(len(outs[0])) != size {
			t.Fatalf("GetBatch(iter %d) len = %d, want %d", i, len(outs[0]), size)
		}
		shmCheckPattern(t, outs[0], seed)
		rel()
	}
	// 4MiB 整块大对象写后立即读（异步读快路径，DMA 窗口内）。
	big := make([]byte, protocol.ChunkSize)
	shmFillPattern(big, 0x77)
	if err := c.PutBatch(ctx, []string{"iw/big"}, int64(len(big)), big); err != nil {
		t.Fatalf("PutBatch(big): %v", err)
	}
	outs, rel, err := c.GetBatch(ctx, []string{"iw/big"}, 0, protocol.ChunkSize)
	if err != nil {
		t.Fatalf("GetBatch(big): %v", err)
	}
	if len(outs[0]) != protocol.ChunkSize {
		t.Fatalf("GetBatch(big) len = %d, want %d", len(outs[0]), protocol.ChunkSize)
	}
	shmCheckPattern(t, outs[0], 0x77)
	rel()
}

// TestShmAsyncPutZeroSizeOrder 零长对象保序（裸流）：size==0 的立即完成项混在普通 put 与
// 4MiB put 之间，响应必须严格按到达顺序返回（0→小→4MiB→0→小），零长对象读回为空。
func TestShmAsyncPutZeroSizeOrder(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	stream := shmRawStream(t, c)
	defer func() { _ = stream.Close() }()

	small := make([]byte, 4096)
	shmFillPattern(small, 0x55)
	big := make([]byte, protocol.ChunkSize)
	shmFillPattern(big, 0x56)
	writePut := func(key string, size int64, data []byte) {
		t.Helper()
		if err := shmWriteFrame(stream, protocol.OpPutHeader, protocol.EncodePutHeader(key, size)); err != nil {
			t.Fatalf("PutHeader(%s): %v", key, err)
		}
		if size > 0 {
			if err := shmWriteFrame(stream, protocol.OpPutData, data); err != nil {
				t.Fatalf("PutData(%s): %v", key, err)
			}
		}
		if err := shmWriteFrame(stream, protocol.OpPutEnd, nil); err != nil {
			t.Fatalf("PutEnd(%s): %v", key, err)
		}
	}
	writePut("zo/a", 0, nil)
	writePut("zo/b", int64(len(small)), small)
	writePut("zo/c", protocol.ChunkSize, big)
	writePut("zo/d", 0, nil)
	writePut("zo/e", int64(len(small)), small)

	for i := 0; i < 5; i++ {
		if got := shmReadRespCode(t, stream, protocol.OpResp); got != protocol.CodeOK {
			t.Fatalf("OpResp[%d] code = %d, want CodeOK", i, got)
		}
	}
	// 零长对象读回为空。
	for _, zk := range []string{"zo/a", "zo/d"} {
		got, rel, err := c.Get(ctx, zk, 0, -1)
		if err != nil || len(got) != 0 {
			t.Fatalf("Get(%s) = (len %d, %v), want (0, nil)", zk, len(got), err)
		}
		rel()
	}
	// 非零对象读回前 4096 字节校验（b/e 种子 0x55，c 种子 0x56）。
	outs, rel, err := c.GetBatch(ctx, []string{"zo/b", "zo/c", "zo/e"}, 0, 4096)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	defer rel()
	for i, k := range []string{"zo/b", "zo/c", "zo/e"} {
		if len(outs[i]) != 4096 {
			t.Fatalf("GetBatch(%s) len = %d, want 4096", k, len(outs[i]))
		}
		seed := byte(0x55)
		if k == "zo/c" {
			seed = 0x56
		}
		shmCheckPattern(t, outs[i], seed)
	}
}

// TestShmAsyncPutBackpressure backpressure 路径（裸流）：inflight=2 下连发 10 个 Put，
// 服务端在第 2 个在途时触发 drainAll 整链排空（规则 2），随后继续；全部 OpResp 按序
// CodeOK，读回全部内容完整 —— 验证排空路径无丢写、无乱序、无数据串扰。
func TestShmAsyncPutBackpressure(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServerInflight(t, st, 2)
	c := shmDial(t, uds)
	ctx := context.Background()

	stream := shmRawStream(t, c)
	defer func() { _ = stream.Close() }()

	const n = 10
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("bp/%d", i)
		keys[i] = key
		p := make([]byte, 4096)
		shmFillPattern(p, byte(0x50+i))
		if err := shmWriteFrame(stream, protocol.OpPutHeader, protocol.EncodePutHeader(key, 4096)); err != nil {
			t.Fatalf("PutHeader(%s): %v", key, err)
		}
		if err := shmWriteFrame(stream, protocol.OpPutData, p); err != nil {
			t.Fatalf("PutData(%s): %v", key, err)
		}
		if err := shmWriteFrame(stream, protocol.OpPutEnd, nil); err != nil {
			t.Fatalf("PutEnd(%s): %v", key, err)
		}
	}
	for i := 0; i < n; i++ {
		if got := shmReadRespCode(t, stream, protocol.OpResp); got != protocol.CodeOK {
			t.Fatalf("OpResp[%d] code = %d, want CodeOK", i, got)
		}
	}
	outs, rel, err := c.GetBatch(ctx, keys, 0, 4096)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	defer rel()
	for i, k := range keys {
		if len(outs[i]) != 4096 {
			t.Fatalf("GetBatch(%s) len = %d, want 4096", k, len(outs[i]))
		}
		shmCheckPattern(t, outs[i], byte(0x50+i))
	}
}

// TestShmAsyncPutErrorThenRecovery 错误后恢复：超大对象（size > MaxObjectSize）在
// PutHeader 即被拒（CodeTooLarge）并关闭流；随后新流上的正常写-读必须完全不受影响
// （错误不污染服务端状态/后续请求）。
func TestShmAsyncPutErrorThenRecovery(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	stream := shmRawStream(t, c)
	if err := shmWriteFrame(stream, protocol.OpPutHeader, protocol.EncodePutHeader("err/huge", st.MaxObjectSize()+1)); err != nil {
		t.Fatalf("PutHeader: %v", err)
	}
	if got := shmReadRespCode(t, stream, protocol.OpResp); got != protocol.CodeTooLarge {
		t.Fatalf("code = %d, want CodeTooLarge", got)
	}
	_ = stream.Close()

	p := make([]byte, 8192)
	shmFillPattern(p, 0x60)
	if err := c.PutBatch(ctx, []string{"err/ok"}, int64(len(p)), p); err != nil {
		t.Fatalf("PutBatch(after error): %v", err)
	}
	outs, rel, err := c.GetBatch(ctx, []string{"err/ok"}, 0, int64(len(p)))
	if err != nil || len(outs[0]) != len(p) {
		t.Fatalf("GetBatch(after error) = (len %d, %v)", len(outs[0]), err)
	}
	shmCheckPattern(t, outs[0], 0x60)
	rel()
}

// TestShmAsyncPutConcurrentOverwrite 并发覆盖一致性：多 goroutine 并发 PutBatch 写
// 同一 key 各自不同内容，全部成功后读回 —— 最终值必须完整等于某一次写入（不允许
// 撕裂/混写）。覆盖跨流并发下写同一映射的原子性（PutCommit 整体覆盖）。
func TestShmAsyncPutConcurrentOverwrite(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for g := 0; g < writers; g++ {
		p := make([]byte, 8192)
		shmFillPattern(p, byte(0x70+g))
		wg.Add(1)
		go func(g int, p []byte) {
			defer wg.Done()
			errs[g] = c.PutBatch(ctx, []string{"cow/k"}, int64(len(p)), p)
		}(g, p)
	}
	wg.Wait()
	for g, err := range errs {
		if err != nil {
			t.Fatalf("PutBatch(g%d): %v", g, err)
		}
	}
	outs, rel, err := c.GetBatch(ctx, []string{"cow/k"}, 0, 8192)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	defer rel()
	if len(outs[0]) != 8192 {
		t.Fatalf("len = %d, want 8192", len(outs[0]))
	}
	matched := false
	for g := 0; g < writers; g++ {
		if shmMatchesPattern(outs[0], byte(0x70+g)) {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatal("并发覆盖后读回的值不属于任何一次写入（撕裂/混写）")
	}
}

// TestShmAsyncPutStability 稳定性用例：多轮异步 PutBatch（覆盖大小对象）后逐轮抽样
// 读回校验，模拟持续写入负载下的数据一致性（无丢写、无串扰、无 hang）。
func TestShmAsyncPutStability(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	const rounds = 12
	verifyRound := func(keys []string, size int64, seed byte) {
		t.Helper()
		outs, rel, err := c.GetBatch(ctx, keys, 0, size)
		if err != nil {
			t.Fatalf("GetBatch(round): %v", err)
		}
		defer rel()
		for i, k := range keys {
			if int64(len(outs[i])) != size {
				t.Fatalf("GetBatch(%s) len = %d, want %d", k, len(outs[i]), size)
			}
			shmCheckPattern(t, outs[i], seed)
		}
	}
	for r := 0; r < rounds; r++ {
		// 小对象批（8 个 4KiB）。
		small := make([]byte, 4096)
		shmFillPattern(small, byte(0x40+r))
		sKeys := make([]string, 8)
		for i := range sKeys {
			sKeys[i] = fmt.Sprintf("stab/s/r%d/%d", r, i)
		}
		if err := c.PutBatch(ctx, sKeys, int64(len(small)), small); err != nil {
			t.Fatalf("PutBatch(small r%d): %v", r, err)
		}
		verifyRound(sKeys, 4096, byte(0x40+r))

		// 4MiB 对象批（2 个整块，兼压异步读流水线）。
		big := make([]byte, protocol.ChunkSize)
		shmFillPattern(big, byte(0x80+r))
		bKeys := make([]string, 2)
		for i := range bKeys {
			bKeys[i] = fmt.Sprintf("stab/b/r%d/%d", r, i)
		}
		if err := c.PutBatch(ctx, bKeys, int64(len(big)), big); err != nil {
			t.Fatalf("PutBatch(big r%d): %v", r, err)
		}
		verifyRound(bKeys, protocol.ChunkSize, byte(0x80+r))
	}
}

// ===== 批量零拷贝读（GetFdBatch）/ 批量异内容写（PutBatchKeys）测试 =====
//
// 以下用例覆盖 EFS FUSE 数据面批量接口（攒批器下发的形态）：
//   - PutBatchKeys 各 key 写不同内容 → GetFdBatch 逐 key 逐字节校验（与 PutBatch
//     同内容语义的关键差异点：内容必须一一对应）；
//   - 零拷贝交付（单帧整块 → fd>0 splice 源）与多帧回退（fd==0 池化拷贝）两路径；
//   - per-key Release：部分释放后其余 FdBuf 仍有效，全部释放后流可复用；
//   - 错误路径：批内 key 缺失整批失败、不返回 FdBuf，后续请求不受污染。

// TestShmGetFdBatchPutBatchKeys 核心用例：PutBatchKeys 写入互不相同的内容 →
// GetFdBatch 逐 key 校验长度与内容；覆盖小对象（单帧零拷贝 fd>0）与 8MiB 大对象
// （多帧/跨切片回退，内容一致即可）。
func TestShmGetFdBatchPutBatchKeys(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	// 小对象批：8KiB × 4，各 key 内容不同（seed 区分）。
	smallKeys := []string{"gfb/s0", "gfb/s1", "gfb/s2", "gfb/s3"}
	smallSize := int64(8192)
	smalls := make([][]byte, len(smallKeys))
	for i := range smallKeys {
		p := make([]byte, smallSize)
		shmFillPattern(p, byte(0x20+i))
		smalls[i] = p
	}
	if err := c.PutBatchKeys(ctx, smallKeys, smallSize, smalls); err != nil {
		t.Fatalf("PutBatchKeys(small): %v", err)
	}
	for _, k := range smallKeys {
		if sz, err := c.Stat(ctx, k); err != nil || sz != smallSize {
			t.Fatalf("Stat(%s) = (%d, %v), want (%d, nil)", k, sz, err, smallSize)
		}
	}

	// 4MiB 整块批（异步读快路径，单帧 → 零拷贝 splice 源）。
	bigKeys := []string{"gfb/b0", "gfb/b1"}
	bigSize := int64(protocol.ChunkSize)
	bigs := make([][]byte, len(bigKeys))
	for i := range bigKeys {
		p := make([]byte, bigSize)
		shmFillPattern(p, byte(0x40+i))
		bigs[i] = p
	}
	if err := c.PutBatchKeys(ctx, bigKeys, bigSize, bigs); err != nil {
		t.Fatalf("PutBatchKeys(4MiB): %v", err)
	}

	// 8MiB 大对象（读回多帧/跨切片 → fd==0 池化拷贝路径）。
	hugeKey := "gfb/h0"
	hugeSize := int64(8 << 20)
	huge := make([]byte, hugeSize)
	shmFillPattern(huge, 0x60)
	if err := c.PutBatchKeys(ctx, []string{hugeKey}, hugeSize, [][]byte{huge}); err != nil {
		t.Fatalf("PutBatchKeys(8MiB): %v", err)
	}

	// 读回小对象批：内容逐 key 对应 + 单帧零拷贝交付。
	outs, rel, err := c.GetFdBatch(ctx, smallKeys, 0, smallSize)
	if err != nil {
		t.Fatalf("GetFdBatch(small): %v", err)
	}
	for i, k := range smallKeys {
		if int64(len(outs[i].Data)) != smallSize {
			t.Fatalf("GetFdBatch(%s) len = %d, want %d", k, len(outs[i].Data), smallSize)
		}
		shmCheckPattern(t, outs[i].Data, byte(0x20+i))
		if outs[i].Fd <= 0 {
			t.Fatalf("GetFdBatch(%s) fd = %d, want >0（单帧整块零拷贝）", k, outs[i].Fd)
		}
	}
	rel()

	// 读回 4MiB 整块批：内容逐 key 对应 + 零拷贝交付。
	outs, rel, err = c.GetFdBatch(ctx, bigKeys, 0, bigSize)
	if err != nil {
		t.Fatalf("GetFdBatch(4MiB): %v", err)
	}
	for i, k := range bigKeys {
		if int64(len(outs[i].Data)) != bigSize {
			t.Fatalf("GetFdBatch(%s) len = %d, want %d", k, len(outs[i].Data), bigSize)
		}
		shmCheckPattern(t, outs[i].Data, byte(0x40+i))
		if outs[i].Fd <= 0 {
			t.Fatalf("GetFdBatch(%s) fd = %d, want >0", k, outs[i].Fd)
		}
	}
	rel()

	// 读回 8MiB 大对象：内容一致即可（多帧/跨切片时 fd 可能为 0，不做硬断言）。
	outs, rel, err = c.GetFdBatch(ctx, []string{hugeKey}, 0, hugeSize)
	if err != nil {
		t.Fatalf("GetFdBatch(8MiB): %v", err)
	}
	if int64(len(outs[0].Data)) != hugeSize {
		t.Fatalf("GetFdBatch(h0) len = %d, want %d", len(outs[0].Data), hugeSize)
	}
	shmCheckPattern(t, outs[0].Data, 0x60)
	rel()
}

// TestShmGetFdBatchRelease per-key Release 语义：部分释放后其余 FdBuf 数据仍有效
// （帧 pin 未归零，共享内存未被回收复用）；全部释放后整批 pin 归零、流 PutBack 复用，
// 同一连接后续请求必须正常。同时校验 Release 幂等（重复释放不 panic、不 double-free）。
func TestShmGetFdBatchRelease(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	keys := []string{"gfbr/k0", "gfbr/k1", "gfbr/k2"}
	size := int64(8192)
	datas := make([][]byte, len(keys))
	for i := range keys {
		p := make([]byte, size)
		shmFillPattern(p, byte(0x70+i))
		datas[i] = p
	}
	if err := c.PutBatchKeys(ctx, keys, size, datas); err != nil {
		t.Fatalf("PutBatchKeys: %v", err)
	}

	outs, rel, err := c.GetFdBatch(ctx, keys, 0, size)
	if err != nil {
		t.Fatalf("GetFdBatch: %v", err)
	}
	// 部分释放：仅归还前两个，第三个仍须有效。
	outs[0].Release()
	outs[1].Release()
	outs[1].Release() // Release 幂等
	shmCheckPattern(t, outs[2].Data, 0x72)
	// 全部释放：整批归零 → 帧 pin 释放 + 流 PutBack。
	outs[2].Release()
	// 兜底 release 也已幂等调用（不应 double-free）。
	rel()
	rel()

	// 同一连接后续请求必须正常（流已复用）。
	got, r, err := c.Get(ctx, keys[0], 0, size)
	if err != nil || int64(len(got)) != size {
		t.Fatalf("Get(after release) = (len %d, %v)", len(got), err)
	}
	shmCheckPattern(t, got, 0x70)
	r()
}

// TestShmGetFdBatchSingleKey 单 key 批（攒批器单 key 直发等价形态）：正常往返 + 零拷贝。
func TestShmGetFdBatchSingleKey(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	key := "gfb1/k"
	size := int64(4096)
	p := make([]byte, size)
	shmFillPattern(p, 0x66)
	if err := c.PutBatchKeys(ctx, []string{key}, size, [][]byte{p}); err != nil {
		t.Fatalf("PutBatchKeys: %v", err)
	}
	outs, rel, err := c.GetFdBatch(ctx, []string{key}, 0, size)
	if err != nil {
		t.Fatalf("GetFdBatch: %v", err)
	}
	if len(outs) != 1 || int64(len(outs[0].Data)) != size {
		t.Fatalf("GetFdBatch len = %d, data len = %d", len(outs), len(outs[0].Data))
	}
	shmCheckPattern(t, outs[0].Data, 0x66)
	if outs[0].Fd <= 0 {
		t.Fatalf("GetFdBatch(单 key) fd = %d, want >0", outs[0].Fd)
	}
	rel()
}

// TestShmGetFdBatchErrors 错误路径：批内 key 缺失 → 整批失败且不返回 FdBuf（错误帧
// 关流，残留响应帧不污染后续请求）；PutBatchKeys 入参不齐（datas 数量不符）不触碰流。
func TestShmGetFdBatchErrors(t *testing.T) {
	st := shmNewTestStorage(t)
	_, uds := shmAsyncServer(t, st)
	c := shmDial(t, uds)
	ctx := context.Background()

	// 铺底正常对象，与缺失 key 混合成批。
	p := make([]byte, 4096)
	shmFillPattern(p, 0x55)
	if err := c.PutBatchKeys(ctx, []string{"gfbe/ok"}, int64(len(p)), [][]byte{p}); err != nil {
		t.Fatalf("PutBatchKeys: %v", err)
	}
	// 批内第二个 key 缺失 → 整批失败、不返回 FdBuf。
	outs, rel, err := c.GetFdBatch(ctx, []string{"gfbe/ok", "gfbe/missing"}, 0, int64(len(p)))
	if err == nil {
		rel()
		t.Fatal("GetFdBatch(批内缺失 key) 应整体失败")
	}
	if len(outs) != 0 {
		rel()
		t.Fatalf("错误路径返回了 %d 个 FdBuf, want 0", len(outs))
	}
	if !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	// 错误后的后续请求（新流）不受污染。
	outs, rel, err = c.GetFdBatch(ctx, []string{"gfbe/ok"}, 0, int64(len(p)))
	if err != nil {
		t.Fatalf("GetFdBatch(after error): %v", err)
	}
	shmCheckPattern(t, outs[0].Data, 0x55)
	rel()

	// PutBatchKeys datas 数量与 keys 不符 → 调用前校验报错，不触碰流。
	if err := c.PutBatchKeys(ctx, []string{"x", "y"}, 4, [][]byte{[]byte("1234")}); err == nil {
		t.Fatal("PutBatchKeys(datas 数量不符) 应报错")
	}
	// 后续写正常。
	if err := c.PutBatchKeys(ctx, []string{"gfbe/a"}, int64(len(p)), [][]byte{p}); err != nil {
		t.Fatalf("PutBatchKeys(after error): %v", err)
	}
}

// TestShmGetFdBatchProtocolAnomalies 客户端在 GetFdBatch 的协议异常响应下的分支：
// 响应 op 不符、超限（首帧超请求大小）。每条子用例独立连接（残留帧不污染其它用例）。
func TestShmGetFdBatchProtocolAnomalies(t *testing.T) {
	cases := []struct {
		name    string
		respond func(*shmipc.Stream)
		wantErr string
	}{
		{
			name:    "op-mismatch",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpStatResp, make([]byte, 8)) },
			wantErr: "unexpected get fd batch frame op",
		},
		{
			name:    "exceeds-size",
			respond: func(st *shmipc.Stream) { _ = shmWriteFrame(st, protocol.OpGetData, make([]byte, 8)) },
			wantErr: "exceeds requested size",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uds := shmStartFakeServer(t, tc.respond)
			c, err := DialShm(uds, 1)
			if err != nil {
				t.Fatalf("DialShm: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			outs, rel, err := c.GetFdBatch(ctx, []string{"k"}, 0, 4)
			if err == nil {
				rel()
				t.Fatal("协议异常响应应使 GetFdBatch 报错")
			}
			if len(outs) != 0 {
				rel()
				t.Fatalf("错误路径返回了 %d 个 FdBuf, want 0", len(outs))
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want 包含 %q", err, tc.wantErr)
			}
		})
	}
}
