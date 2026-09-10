package device

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/bufpool"
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

	if err := dev.Append(context.Background(), 0, 0, 3, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	// off 须推进到 4K 对齐
	if err := dev.Append(context.Background(), 0, 4096, 10, make([]byte, 10)); err != nil {
		t.Fatalf("aligned append: %v", err)
	}
	// 非 4K 对齐 offset 应报错
	if err := dev.Append(context.Background(), 0, 100, 10, make([]byte, 10)); err == nil {
		t.Fatalf("unaligned offset should error")
	}
	// 数据不足 size 应报错
	if err := dev.Append(context.Background(), 0, 8192, 10, make([]byte, 5)); err == nil {
		t.Fatalf("short data should error")
	}

	// 对齐整块读回：4096 字节里前 3 字节应为 "abc"，其余为补零（ReadAt 返回池化对齐缓冲）
	data, err := dev.ReadAt(context.Background(), 0, 0, 4096)
	if err != nil {
		t.Fatalf("device read: %v", err)
	}
	defer bufpool.Put(data)
	if len(data) != 4096 {
		t.Fatalf("device read got %dB want 4096", len(data))
	}
	if string(data[:3]) != "abc" {
		t.Fatalf("device read prefix got %q", data[:3])
	}
	for _, v := range data[3:] {
		if v != 0 {
			t.Fatalf("device read non-zero padding at byte 3: %d", v)
		}
	}
	// 读第二段（偏移 4096 处的 10 字节内容）
	data2, err := dev.ReadAt(context.Background(), 0, 4096, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer bufpool.Put(data2)
	for _, v := range data2[:10] {
		if v != 0 {
			t.Fatalf("second append misalign: %d", v)
		}
	}
}

func TestDeviceAppendAlignedFastPath(t *testing.T) {
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	f, _ := os.Create(devPath)
	_ = f.Close()

	dev, err := NewDevice(context.Background(), devPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()

	// 4K 对齐地址 + 4K 倍数长度 → 命中直写快路径（bufpool.Get 保证首地址 4K 对齐）。
	data := bufpool.Get(int(4096))
	defer bufpool.Put(data)
	for i := range data[:4096] {
		data[i] = byte(i)
	}
	if err := dev.Append(context.Background(), 0, 0, 4096, data[:4096]); err != nil {
		t.Fatalf("aligned fast-path append: %v", err)
	}

	// 读回对比：快路径直写内容须与源一致（无补零、无错位）。
	got, err := dev.ReadAt(context.Background(), 0, 0, 4096)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer bufpool.Put(got)
	if !bytes.Equal(got[:4096], data[:4096]) {
		t.Fatal("aligned fast-path round trip mismatch")
	}

	// 非快路径（地址/长度不同时为 4K 对齐）仍走拷贝兜底，数据须一致。
	unaligned := make([]byte, 4096)
	for i := range unaligned {
		unaligned[i] = byte(0xff - i)
	}
	if err := dev.Append(context.Background(), 0, 8192, 4096, unaligned); err != nil {
		t.Fatalf("fallback append: %v", err)
	}
	got2, err := dev.ReadAt(context.Background(), 0, 8192, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer bufpool.Put(got2)
	if !bytes.Equal(got2[:4096], unaligned) {
		t.Fatal("fallback round trip mismatch")
	}

	// 对齐地址 + 非 4K 倍数长度（"4M+1k" 缩小版）：主体直写 + 仅 4K 尾块缓冲，数据与补零须正确。
	obj := bufpool.Get(16384)
	defer bufpool.Put(obj)
	payload := obj[:12288+100] // 12388 B：12288 对齐主体 + 100 字节尾块
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	if err := dev.Append(context.Background(), 0, 16384, int64(len(payload)), payload); err != nil {
		t.Fatalf("aligned bulk+tail append: %v", err)
	}
	got3, err := dev.ReadAt(context.Background(), 0, 16384, 16384)
	if err != nil {
		t.Fatal(err)
	}
	defer bufpool.Put(got3)
	if !bytes.Equal(got3[:len(payload)], payload) {
		t.Fatal("aligned bulk+tail payload mismatch")
	}
	for _, v := range got3[len(payload):16384] {
		if v != 0 {
			t.Fatal("aligned bulk+tail tail padding not zero")
		}
	}
}

// TestDeviceConcurrent 多 goroutine 异偏移并发 Append/ReadAt，校验完成泵分发正确性（-race）。
func TestDeviceConcurrent(t *testing.T) {
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	f, _ := os.Create(devPath)
	_ = f.Close()

	dev, err := NewDevice(context.Background(), devPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()

	const n = 32
	const chunk = 4096
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := bufpool.Get(chunk)
			defer bufpool.Put(data)
			for j := range data {
				data[j] = byte(i)
			}
			if err := dev.Append(context.Background(), 0, int64(i)*chunk, chunk, data); err != nil {
				errs[i] = err
				return
			}
			got, err := dev.ReadAt(context.Background(), 0, int64(i)*chunk, chunk)
			if err != nil {
				errs[i] = err
				return
			}
			defer bufpool.Put(got)
			if !bytes.Equal(got, data) {
				errs[i] = fmt.Errorf("chunk %d mismatch", i)
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("worker %d: %v", i, e)
		}
	}
}

// TestDeviceCloseInflight 在途请求存在时 Close 须排空完成事件后返回，不挂起。
func TestDeviceCloseInflight(t *testing.T) {
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	f, _ := os.Create(devPath)
	_ = f.Close()

	dev, err := NewDevice(context.Background(), devPath)
	if err != nil {
		t.Fatal(err)
	}

	// 4MB 写：O_SYNC（非 Linux 打开方式）下耗时足够，可稳定被观测为在途。
	data := bufpool.Get(4 << 20)
	defer bufpool.Put(data)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = dev.Append(context.Background(), 0, 0, 4<<20, data)
	}()

	deadline := time.Now().Add(5 * time.Second)
poll:
	for {
		select {
		case <-done:
			break poll // 已完成也直接测 Close
		default:
		}
		dev.mu.Lock()
		inflight := len(dev.m) > 0
		dev.mu.Unlock()
		if inflight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("submit never registered")
		}
		time.Sleep(time.Millisecond)
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("close with inflight: %v", err)
	}
	<-done
}
