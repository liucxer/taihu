package storage

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
)

// putAligned 写入 n 个 4KB 对象（key 形如 k%02d），返回负载。
func putAligned(t *testing.T, s *Storage, ctx context.Context, n int) []byte {
	t.Helper()
	payload := alignedPayload(int(layout.BlockSize))
	for i := 0; i < n; i++ {
		if err := s.Put(ctx, fmt.Sprintf("k%02d", i), int64(len(payload)), payload); err != nil {
			t.Fatalf("put k%02d: %v", i, err)
		}
	}
	return payload
}

// TestCompactorConfigAndLifecycle 覆盖 DefaultCompactorConfig 与 Start/run/Stop 后台循环。
func TestCompactorConfigAndLifecycle(t *testing.T) {
	cfg := DefaultCompactorConfig()
	if cfg.Interval != time.Minute || cfg.HoleThreshold != 0.8 || cfg.ForceWatermark != 0.8 || cfg.MaxMovePerRound != 512 {
		t.Fatalf("DefaultCompactorConfig = %+v", cfg)
	}

	const segSize = 64 * 1024
	l := layout.Layout{SegmentSizeBytes: segSize, SegmentCount: 16}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	putAligned(t, s, ctx, 17) // 写满段 0（第 17 个触发滚动 → 段 0 转 Full），段 1 Active
	for i := 0; i < 13; i++ {
		if err := s.Delete(ctx, fmt.Sprintf("k%02d", i)); err != nil {
			t.Fatal(err)
		}
	}

	c := NewCompactor(s, CompactorConfig{Interval: 5 * time.Millisecond, HoleThreshold: 0.8, ForceWatermark: 1.0, MaxMovePerRound: 100})
	c.Start()
	// 后台循环每 5ms 扫一轮：候选段存活对象被搬走后旧段经后台 GC 回收为 Free。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if st := s.SegmentStats(); st[metastore.SegmentStateFree] >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background compaction did not free sparse segment: %v", s.SegmentStats())
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.Stop()

	// 存活对象（k13..k15）搬移后数据往返正确。
	for i := 13; i < 16; i++ {
		key := fmt.Sprintf("k%02d", i)
		got, err := s.ReadAt(ctx, key, 0, int64(layout.BlockSize))
		if err != nil {
			t.Fatalf("read %s after background compaction: %v", key, err)
		}
		bufpool.Put(got)
	}
}

// TestCompactForceWatermark 覆盖全局水位强制压缩（空洞率低于单段阈值仍搬移）。
func TestCompactForceWatermark(t *testing.T) {
	const segSize = 64 * 1024
	l := layout.Layout{SegmentSizeBytes: segSize, SegmentCount: 16}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	putAligned(t, s, ctx, 17) // 段 0 写满转 Full，段 1 Active
	// 只删 1 个 → 空洞率 1/16 ≈ 6.3%，远低于阈值 99%；靠全局水位（2/16 ≥ 5%）强制入选。
	if err := s.Delete(ctx, "k00"); err != nil {
		t.Fatal(err)
	}

	c := NewCompactor(s, CompactorConfig{Interval: time.Hour, HoleThreshold: 0.99, ForceWatermark: 0.05, MaxMovePerRound: 100})
	moved, err := c.compactOnce(ctx)
	if err != nil {
		t.Fatalf("compactOnce: %v", err)
	}
	if moved != 15 {
		t.Fatalf("force watermark moved=%d want 15", moved)
	}
	// 搬移后存活对象读原数据。
	payload := alignedPayload(int(layout.BlockSize))
	got, err := s.ReadAt(ctx, "k01", 0, int64(len(payload)))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("read k01 after force compaction: err=%v", err)
	}
	bufpool.Put(got)
}

// TestCompactMoveLimit 覆盖每轮搬移对象数上限（跨候选段合并记账）。
func TestCompactMoveLimit(t *testing.T) {
	const segSize = 64 * 1024
	l := layout.Layout{SegmentSizeBytes: segSize, SegmentCount: 16}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	// 33 个对象：段 0 满、段 1 满（第 33 个触发滚动时标记 Full）、段 2 Active。
	putAligned(t, s, ctx, 33)
	// 两个 Full 段各删 13 个 → 各剩 3 个，空洞率均 ≈81%，都是候选（按段号升序）。
	for i := 0; i < 13; i++ {
		if err := s.Delete(ctx, fmt.Sprintf("k%02d", i)); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, fmt.Sprintf("k%02d", i+16)); err != nil {
			t.Fatal(err)
		}
	}

	c := NewCompactor(s, CompactorConfig{Interval: time.Hour, HoleThreshold: 0.8, ForceWatermark: 1.0, MaxMovePerRound: 1})
	moved, err := c.compactOnce(ctx)
	if err != nil {
		t.Fatalf("compactOnce: %v", err)
	}
	if moved != 1 {
		t.Fatalf("MaxMovePerRound=1 moved=%d want 1", moved)
	}
	// 被搬移的首个对象（段 0 的 k13）数据正确。
	payload := alignedPayload(int(layout.BlockSize))
	got, err := s.ReadAt(ctx, "k13", 0, int64(len(payload)))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("read k13 after limited compaction: err=%v", err)
	}
	bufpool.Put(got)
}
