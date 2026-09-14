package aio

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const testChunk = 4096

// backend 一个待测的后端实现。new 负责建好队列；后端在当前机器不可用时应 t.Skipf
// （带上原因，避免「静默跳过」在日志里看起来和「通过」一样）。
type backend struct {
	name string
	new  func(t *testing.T, maxEvents int) Ring
}

// newTestFile 建一个定长临时文件并返回句柄。
func newTestFile(t *testing.T, size int64) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "aio-dev"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open temp file: %v", err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// pattern 生成确定性数据块。
func pattern(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

// TestRoundTrip 写后读回，校验数据一致。对每个可用后端各跑一遍。
func TestRoundTrip(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) { assertRoundTrip(t, b.new(t, 8)) })
	}
}

// assertRoundTrip 是 TestRoundTrip 的后端无关断言体。
func assertRoundTrip(t *testing.T, r Ring) {
	t.Helper()
	defer r.Close()

	f := newTestFile(t, testChunk)
	fd := int(f.Fd())
	data := pattern(0x5A, testChunk)

	seq, err := r.SubmitWrite(fd, data, 0)
	if err != nil {
		t.Fatalf("SubmitWrite: %v", err)
	}
	evs, err := r.Wait(1, 1, nil)
	if err != nil {
		t.Fatalf("Wait write: %v", err)
	}
	if len(evs) != 1 || evs[0].Data != seq || evs[0].Res != testChunk {
		t.Fatalf("write event mismatch: %+v want data=%d res=%d", evs, seq, testChunk)
	}

	// 读回并校验（读缓冲可与写缓冲不同，经内核落盘后读）。
	out := make([]byte, testChunk)
	seq2, err := r.SubmitRead(fd, out, 0)
	if err != nil {
		t.Fatalf("SubmitRead: %v", err)
	}
	evs, err = r.Wait(1, 1, nil)
	if err != nil {
		t.Fatalf("Wait read: %v", err)
	}
	if len(evs) != 1 || evs[0].Data != seq2 || evs[0].Res != testChunk {
		t.Fatalf("read event mismatch: %+v want data=%d res=%d", evs, seq2, testChunk)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("round-trip mismatch")
	}
}

// TestMultipleInflight 一次提交多请求，按 seq 关联各偏移结果。
func TestMultipleInflight(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) { assertMultipleInflight(t, b.new(t, 16)) })
	}
}

// assertMultipleInflight 是 TestMultipleInflight 的后端无关断言体。
func assertMultipleInflight(t *testing.T, r Ring) {
	t.Helper()
	defer r.Close()

	const n = 8
	f := newTestFile(t, int64(n)*testChunk)
	fd := int(f.Fd())

	seqs := make([]uint64, n)
	datas := make([][]byte, n)
	for i := 0; i < n; i++ {
		datas[i] = pattern(byte(i+1), testChunk)
		s, err := r.SubmitWrite(fd, datas[i], int64(i)*testChunk)
		if err != nil {
			t.Fatalf("SubmitWrite#%d: %v", i, err)
		}
		seqs[i] = s
	}
	evs, err := r.Wait(n, n, nil)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(evs) != n {
		t.Fatalf("got %d events, want %d", len(evs), n)
	}
	bySeq := make(map[uint64]int64, n)
	for _, ev := range evs {
		bySeq[ev.Data] = ev.Res
	}
	for i := 0; i < n; i++ {
		if bySeq[seqs[i]] != testChunk {
			t.Fatalf("chunk#%d res=%d want %d", i, bySeq[seqs[i]], testChunk)
		}
	}

	// 并发读回并校验。
	bufs := make([][]byte, n)
	seqR := make([]uint64, n)
	for i := 0; i < n; i++ {
		bufs[i] = make([]byte, testChunk)
		s, err := r.SubmitRead(fd, bufs[i], int64(i)*testChunk)
		if err != nil {
			t.Fatalf("SubmitRead#%d: %v", i, err)
		}
		seqR[i] = s
	}
	evs, err = r.Wait(n, n, nil)
	if err != nil {
		t.Fatalf("Wait read: %v", err)
	}
	for _, ev := range evs {
		if ev.Res != testChunk {
			t.Fatalf("read res=%d want %d", ev.Res, testChunk)
		}
	}
	gotSeq := make(map[uint64]bool, n)
	for _, ev := range evs {
		gotSeq[ev.Data] = true
	}
	for i := 0; i < n; i++ {
		if !gotSeq[seqR[i]] {
			t.Fatalf("read seq %d missing", seqR[i])
		}
		if !bytes.Equal(bufs[i], datas[i]) {
			t.Fatalf("chunk#%d mismatch", i)
		}
	}
}

// TestReadBeyondEOF 越界读返回 0 字节而非错误。
func TestReadBeyondEOF(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) { assertReadBeyondEOF(t, b.new(t, 4)) })
	}
}

// assertReadBeyondEOF 是 TestReadBeyondEOF 的后端无关断言体。
func assertReadBeyondEOF(t *testing.T, r Ring) {
	t.Helper()
	defer r.Close()

	f := newTestFile(t, testChunk)
	fd := int(f.Fd())
	buf := make([]byte, testChunk)
	if _, err := r.SubmitRead(fd, buf, testChunk*2); err != nil {
		t.Fatalf("SubmitRead: %v", err)
	}
	evs, err := r.Wait(1, 1, nil)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if evs[0].Res != 0 {
		t.Fatalf("beyond-EOF read res=%d want 0", evs[0].Res)
	}
}

// TestWaitTimeout 无事件时 Wait 按超时返回 ErrTimeout 与空列表。
// 零超时这一例同时校验「不阻塞进内核、纯 deadline 判定」这条路径。
func TestWaitTimeout(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) { assertWaitTimeout(t, b.new(t, 2)) })
	}
}

// assertWaitTimeout 是 TestWaitTimeout 的后端无关断言体。
func assertWaitTimeout(t *testing.T, r Ring) {
	t.Helper()
	defer r.Close()

	zero := time.Duration(0)
	evs, err := r.Wait(1, 4, &zero)
	if err == nil {
		t.Fatalf("want ErrTimeout, got nil (evs=%v)", evs)
	}
	if len(evs) != 0 {
		t.Fatalf("want no events, got %d", len(evs))
	}
}

// TestWaitTimeoutExpires 非零超时且无事件时，Wait 应在超时后返回 ErrTimeout。
func TestWaitTimeoutExpires(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) { assertWaitTimeoutExpires(t, b.new(t, 2)) })
	}
}

// assertWaitTimeoutExpires 校验非零超时路径：既不能提前返回，也不能一直挂着。
func assertWaitTimeoutExpires(t *testing.T, r Ring) {
	t.Helper()
	defer r.Close()

	d := 50 * time.Millisecond
	start := time.Now()
	evs, err := r.Wait(1, 4, &d)
	elapsed := time.Since(start)

	if err != ErrTimeout {
		t.Fatalf("want ErrTimeout, got %v (evs=%v)", err, evs)
	}
	if len(evs) != 0 {
		t.Fatalf("want no events, got %d", len(evs))
	}
	if elapsed < d/2 {
		t.Fatalf("returned too early: %v < %v", elapsed, d/2)
	}
}

// TestFdSurvivesGC 回归：后端不得持有「用完即丢、却会关闭调用方 fd」的包装对象。
//
// macOS 兜底后端曾把 fd 包成 os.NewFile 做带偏移读写、用完即丢。而 os.NewFile 会给
// *os.File 挂 finalizer（os/file_unix.go: runtime.SetFinalizer(f.file, (*file).close)），
// 包装对象一旦成为垃圾，GC 就会 close 掉**调用方持有的同一个 fd**：轻则后续 IO 随机
// EBADF（表现为「随机用例、随机文件报 bad file descriptor」），重则 kevent 对该 fd
// 报 EBADF 触发 runtime fatal（runtime: netpoll failed）导致整个测试进程退出。
func TestFdSurvivesGC(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) { assertFdSurvivesGC(t, b.new(t, 8)) })
	}
}

// assertFdSurvivesGC 提交若干次制造包装对象垃圾，强制 GC 后再验证同一个 fd 仍可用。
func assertFdSurvivesGC(t *testing.T, r Ring) {
	t.Helper()
	defer r.Close()

	f := newTestFile(t, testChunk*2)
	fd := int(f.Fd())
	data := pattern(0x33, testChunk)

	// roundTrip 在同一个 fd 上做一次写后读回。
	roundTrip := func() {
		t.Helper()
		if _, err := r.SubmitWrite(fd, data, 0); err != nil {
			t.Fatalf("SubmitWrite: %v", err)
		}
		if _, err := r.Wait(1, 1, nil); err != nil {
			t.Fatalf("Wait write: %v", err)
		}
		out := make([]byte, testChunk)
		if _, err := r.SubmitRead(fd, out, 0); err != nil {
			t.Fatalf("SubmitRead: %v", err)
		}
		if _, err := r.Wait(1, 1, nil); err != nil {
			t.Fatalf("Wait read: %v", err)
		}
		if !bytes.Equal(out, data) {
			t.Fatalf("round-trip mismatch")
		}
	}

	roundTrip()
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	roundTrip() // fd 必须仍然有效

	// 直接查原症状：fd 被别人的 finalizer 关掉时，这里会报 EBADF。
	if err := f.Close(); err != nil {
		t.Fatalf("Close after GC: %v（fd 被包装对象的 finalizer 关闭了？）", err)
	}
}
