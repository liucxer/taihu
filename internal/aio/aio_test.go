package aio

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testChunk = 4096

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

// TestRoundTrip 写后读回，校验数据一致。
func TestRoundTrip(t *testing.T) {
	r, err := New(8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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
	r, err := New(16)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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
	r, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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
func TestWaitTimeout(t *testing.T) {
	r, err := New(2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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
