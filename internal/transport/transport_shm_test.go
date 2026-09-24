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
	"net"
	"os"
	"path/filepath"
	"strings"
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
