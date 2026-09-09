package taihu

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// alignedPayload 返回 n 个字节的测试负载，n 须为 BlockSize 整数倍。
func alignedPayload(n int) []byte {
	if n%int(BlockSize) != 0 {
		panic("alignedPayload requires 4K multiple")
	}
	b := make([]byte, n)
	copy(b, "taihu odirect object storage")
	return b
}

func newTestStorage(t *testing.T) (*Storage, string, string) {
	t.Helper()
	dir := t.TempDir()
	rocksdbDir := filepath.Join(dir, "meta")
	devPath := filepath.Join(dir, "nvme.img")
	f, err := os.Create(devPath)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	_ = f.Close()

	s, err := NewStorage(context.Background(), rocksdbDir, devPath)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	return s, rocksdbDir, devPath
}

func TestStoragePutGetRoundTrip(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()

	payload := alignedPayload(4096)
	if err := s.Put(context.Background(), "obj/1", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := s.Get(context.Background(), "obj/1", 0, int64(len(payload)))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("round trip mismatch: got %d bytes want %d", len(got), len(payload))
	}
}

func TestStorageGetRange(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()

	payload := alignedPayload(2 * int(BlockSize)) // 8192 B
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := s.Put(context.Background(), "obj", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}

	// 对齐子区间读
	rc, err := s.Get(context.Background(), "obj", 0, int64(BlockSize))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload[:BlockSize]) {
		t.Fatal("aligned sub-range mismatch")
	}

	// 越界读到末尾（钳到对象结尾）
	rc2, err := s.Get(context.Background(), "obj", int64(BlockSize), int64(4*BlockSize))
	if err != nil {
		t.Fatal(err)
	}
	got2, _ := io.ReadAll(rc2)
	_ = rc2.Close()
	if !bytes.Equal(got2, payload[BlockSize:]) {
		t.Fatal("clamped range mismatch")
	}

	// 非对齐 off/size 也能读（Storage 层吸收对齐），数据须正确
	rc3, err := s.Get(context.Background(), "obj", 100, 2048)
	if err != nil {
		t.Fatalf("unaligned off/size Get: %v", err)
	}
	got3, _ := io.ReadAll(rc3)
	_ = rc3.Close()
	if !bytes.Equal(got3, payload[100:100+2048]) {
		t.Fatal("unaligned sub-range mismatch")
	}
}

func TestStorageDelete(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()

	payload := alignedPayload(int(BlockSize))
	if err := s.Put(context.Background(), "d", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), "d", 0, int64(BlockSize)); err != ErrNotFound {
		t.Fatalf("after delete Get err=%v want ErrNotFound", err)
	}
	if err := s.Delete(context.Background(), "d"); err != ErrNotFound {
		t.Fatalf("double delete err=%v want ErrNotFound", err)
	}
}

func TestStorageNotFound(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	if _, err := s.Get(context.Background(), "nope", 0, int64(BlockSize)); err != ErrNotFound {
		t.Fatalf("Get missing err=%v want ErrNotFound", err)
	}
}

func TestStorageRestartPreservesCursor(t *testing.T) {
	dir := t.TempDir()
	rocksdbDir := filepath.Join(dir, "meta")
	devPath := filepath.Join(dir, "nvme.img")
	f, _ := os.Create(devPath)
	_ = f.Close()

	s1, err := NewStorage(context.Background(), rocksdbDir, devPath)
	if err != nil {
		t.Fatal(err)
	}
	payload := alignedPayload(int(BlockSize))
	if err := s1.Put(context.Background(), "k", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStorage(context.Background(), rocksdbDir, devPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	// 重启后读旧对象
	got1, err := s2.Get(context.Background(), "k", 0, int64(len(payload)))
	if err != nil {
		t.Fatalf("restart read old: %v", err)
	}
	b1, _ := io.ReadAll(got1)
	_ = got1.Close()
	if !bytes.Equal(b1, payload) {
		t.Fatal("restart old data mismatch")
	}

	// 重启后续写（新对象起始于老对象 4K 对齐之后的下一位置）
	payload2 := alignedPayload(int(BlockSize))
	copy(payload2, "after restart")
	if err := s2.Put(context.Background(), "k2", int64(len(payload2)), bytes.NewReader(payload2)); err != nil {
		t.Fatalf("restart put: %v", err)
	}
	got2, err := s2.Get(context.Background(), "k2", 0, int64(len(payload2)))
	if err != nil {
		t.Fatalf("restart get new: %v", err)
	}
	b2, _ := io.ReadAll(got2)
	_ = got2.Close()
	if !bytes.Equal(b2, payload2) {
		t.Fatal("restart new data mismatch")
	}
}

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

	if err := dev.append(context.Background(), 0, 0, 3, bytes.NewReader([]byte("abc"))); err != nil {
		t.Fatal(err)
	}
	// off 须推进到 4K 对齐
	if err := dev.append(context.Background(), 0, 4096, 10, bytes.NewReader(make([]byte, 10))); err != nil {
		t.Fatalf("aligned append: %v", err)
	}
	// 非 4K 对齐 offset 应报错
	if err := dev.append(context.Background(), 0, 100, 10, bytes.NewReader(make([]byte, 10))); err == nil {
		t.Fatalf("unaligned offset should error")
	}

	// 对齐整块读回：4096 字节里前 3 字节应为 "abc"，其余为补零
	r, err := dev.read(context.Background(), 0, 0, 4096)
	if err != nil {
		t.Fatalf("device read: %v", err)
	}
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

func TestStorageLoadCache(t *testing.T) {
	s, rocksdbDir, devPath := newTestStorage(t)

	// 写入若干对象后关闭，用全新 Storage 验证 LoadCache 能从 pebble 重建缓存
	payload := alignedPayload(int(BlockSize))
	keys := []string{"k1", "k2", "k3"}
	for _, k := range keys {
		if err := s.Put(context.Background(), k, int64(len(payload)), bytes.NewReader(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStorage(context.Background(), rocksdbDir, devPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if err := s2.LoadCache(context.Background()); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}

	// 预热后读取全部能正确命中
	for _, k := range keys {
		rc, err := s2.Get(context.Background(), k, 0, int64(len(payload)))
		if err != nil {
			t.Fatalf("Get %s after LoadCache: %v", k, err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !bytes.Equal(got, payload) {
			t.Fatalf("Get %s mismatch after LoadCache", k)
		}
	}
}

func TestCacheEvictionBudget(t *testing.T) {
	c := &metaCache{}
	// 塞入足够条目触发逐出，验证各分片 usedBytes 不超 perShardLimit
	for i := 0; i < 100000; i++ {
		k := "key-" + strconv.Itoa(i)
		c.put(k, ObjectMeta{SegmentID: int64(i)})
	}
	for i := 0; i < shardCount; i++ {
		if c.shards[i].usedBytes > perShardLimit {
			t.Fatalf("shard %d over budget: %d > %d", i, c.shards[i].usedBytes, perShardLimit)
		}
	}
}