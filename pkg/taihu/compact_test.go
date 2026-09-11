package taihu

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
	"github.com/liucxer/taihu/internal/ierr"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
)

// newTestStorageLayout 构造带指定布局的文件设备 Storage（testLayout 之外的小段布局便于触发搬移）。
func newTestStorageLayout(t *testing.T, l layout.Layout) *Storage {
	t.Helper()
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	f, err := os.Create(devPath)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	_ = f.Close()
	s, err := NewStorage(context.Background(), filepath.Join(dir, "meta"), devPath, l)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	return s
}

// TestCompactFreesSparseSegment：删除 80% 的高空洞段被 compaction 搬移存活对象（可动用预留缓冲段），
// 搬空后旧段走现有后台 GC 回收为 Free 复用；存活对象数据往返正确。
func TestCompactFreesSparseSegment(t *testing.T) {
	const segSize = 64 * 1024 // 每段 16 个 4KB 对象
	l := layout.Layout{SegmentSizeBytes: segSize, SegmentCount: 64}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	payload := alignedPayload(int(layout.BlockSize))
	const perSeg = 16
	for i := 0; i < 2*perSeg; i++ {
		if err := s.Put(ctx, fmt.Sprintf("k%02d", i), int64(len(payload)), payload); err != nil {
			t.Fatal(err)
		}
	}
	// 段 0 写满 16 个（Full），段 1 Active 写 4 个。删段 0 的 13 个 → 剩余 3 个，空洞率 ≈ 81%。
	for i := 0; i < 13; i++ {
		if err := s.Delete(ctx, fmt.Sprintf("k%02d", i)); err != nil {
			t.Fatal(err)
		}
	}

	c := NewCompactor(s, CompactorConfig{Interval: time.Hour, HoleThreshold: 0.8, ForceWatermark: 1.0, MaxMovePerRound: 100})
	moved, err := c.compactOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if moved == 0 {
		t.Fatal("compaction moved 0 objects")
	}

	// 搬空后段 0 → Reclaiming，后台 GC（1s 周期）回收为 Free。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if st := s.SegmentStats(); st[metastore.SegmentStateFree] >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sparse segment not reclaimed after compaction: %v", s.SegmentStats())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 存活对象（k13..k31）数据往返正确（读走新映射）。
	for i := 13; i < 2*perSeg; i++ {
		key := fmt.Sprintf("k%02d", i)
		got, err := s.ReadAt(ctx, key, 0, int64(len(payload)))
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("%s data mismatch", key)
		}
		bufpool.Put(got)
	}
}

// TestCompactConcurrentDelete：搬移期间并发删除候选对象（-race）。
// 数据一致性由 MoveMapping 的 CAS 保证：对象已删 → 跳过；读后删除 → 冲突跳过，不丢数据、不复活映射。
func TestCompactConcurrentDelete(t *testing.T) {
	const segSize = 64 * 1024
	l := layout.Layout{SegmentSizeBytes: segSize, SegmentCount: 64}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	payload := alignedPayload(int(layout.BlockSize))
	const perSeg = 16
	for i := 0; i < 2*perSeg; i++ {
		if err := s.Put(ctx, fmt.Sprintf("k%02d", i), int64(len(payload)), payload); err != nil {
			t.Fatal(err)
		}
	}
	// 段 0 删 13 个 → 剩 k13..k15 三个（空洞率 ≈ 81%），是搬移候选。
	for i := 0; i < 13; i++ {
		if err := s.Delete(ctx, fmt.Sprintf("k%02d", i)); err != nil {
			t.Fatal(err)
		}
	}

	c := NewCompactor(s, CompactorConfig{Interval: time.Hour, HoleThreshold: 0.8, ForceWatermark: 1.0, MaxMovePerRound: 100})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond) // 让搬移先读到候选对象
		_ = s.Delete(ctx, "k13")          // 候选段（0）内存活对象之一被并发删除
	}()
	moved, err := c.compactOnce(ctx)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if moved == 0 {
		t.Fatal("compaction moved 0 objects")
	}

	// k13 应已删除（不复活）；其余存活对象可读。
	if _, err := s.ObjectMeta(ctx, "k13"); err != ierr.ErrNotFound {
		t.Fatalf("k13 mapping = %v, want ErrNotFound", err)
	}
	for i := 14; i < 2*perSeg; i++ {
		key := fmt.Sprintf("k%02d", i)
		got, err := s.ReadAt(ctx, key, 0, int64(len(payload)))
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		bufpool.Put(got)
	}
}