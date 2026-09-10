//go:build linux

package aio

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

// alignedBuf 返回首地址 4K 对齐的 n 字节切片（O_DIRECT 要求）。
func alignedBuf(n int) []byte {
	const block = 4096
	b := make([]byte, n+block)
	base := uintptr(unsafe.Pointer(&b[0]))
	if base%block == 0 {
		return b[:n]
	}
	start := block - int(base%block)
	return b[start : start+n]
}

// TestRoundTripODirect 通过 libaio 对 O_DIRECT 文件做对齐读写往返。
// 文件系统不支持 O_DIRECT（如 tmpfs）时跳过。
func TestRoundTripODirect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aio-odirect")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_DIRECT, 0o600)
	if err != nil {
		t.Skipf("O_DIRECT unsupported: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	r, err := New(8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	fd := int(f.Fd())
	data := pattern(0x3C, testChunk) // 测试文件内 pattern 分配非对齐缓冲，仅校验时用

	wbuf := alignedBuf(testChunk)
	copy(wbuf, data)
	if _, err := r.SubmitWrite(fd, wbuf, 0); err != nil {
		t.Fatalf("SubmitWrite: %v", err)
	}
	if evs, err := r.Wait(1, 1, nil); err != nil || evs[0].Res != testChunk {
		t.Fatalf("Wait write: evs=%v err=%v", evs, err)
	}

	rbuf := alignedBuf(testChunk)
	if _, err := r.SubmitRead(fd, rbuf, 0); err != nil {
		t.Fatalf("SubmitRead: %v", err)
	}
	if evs, err := r.Wait(1, 1, nil); err != nil || evs[0].Res != testChunk {
		t.Fatalf("Wait read: evs=%v err=%v", evs, err)
	}
	if !bytes.Equal(rbuf, data) {
		t.Fatalf("O_DIRECT round-trip mismatch")
	}
}
