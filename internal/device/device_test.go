package device

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDeviceAppendAlignment(t *testing.T) {
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	f, _ := os.Create(devPath)
	_ = f.Close()

	dev, err := NewDevice(context.Background(), devPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()

	if err := dev.Append(context.Background(), 0, 0, 3, bytes.NewReader([]byte("abc"))); err != nil {
		t.Fatal(err)
	}
	// off 须推进到 4K 对齐
	if err := dev.Append(context.Background(), 0, 4096, 10, bytes.NewReader(make([]byte, 10))); err != nil {
		t.Fatalf("aligned append: %v", err)
	}
	// 非 4K 对齐 offset 应报错
	if err := dev.Append(context.Background(), 0, 100, 10, bytes.NewReader(make([]byte, 10))); err == nil {
		t.Fatalf("unaligned offset should error")
	}

	// 对齐整块读回：4096 字节里前 3 字节应为 "abc"，其余为补零
	r, err := dev.Read(context.Background(), 0, 0, 4096)
	if err != nil {
		t.Fatalf("device read: %v", err)
	}
	defer r.Close() // 归还池化对齐缓冲
	b, _ := io.ReadAll(r)
	if string(b[:3]) != "abc" {
		t.Fatalf("device read prefix got %q", b[:3])
	}
	for _, v := range b[3:] {
		if v != 0 {
			t.Fatalf("device read non-zero padding at byte 3: %d", v)
		}
	}
}