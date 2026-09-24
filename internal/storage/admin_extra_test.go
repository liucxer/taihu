package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/pkg/ierr"
)

// TestSegmentsSummary 覆盖 admin Segments 的段明细/汇总/游标与四种段状态分支。
func TestSegmentsSummary(t *testing.T) {
	const segSize = 64 * 1024
	l := layout.Layout{SegmentSizeBytes: segSize, SegmentCount: 16}
	s := newTestStorageLayout(t, l)
	defer s.Close()
	ctx := context.Background()

	payload := alignedPayload(int(layout.BlockSize))
	// 每段容纳 16 个 4KB 对象。
	const perSeg = 16
	for i := 0; i < perSeg+1; i++ { // 写满段 0（Full），第 17 个落在 Active 的段 1
		if err := s.Put(ctx, fmt.Sprintf("k%02d", i), int64(len(payload)), payload); err != nil {
			t.Fatal(err)
		}
	}

	sum, entries, err := s.Segments(ctx)
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}
	if sum.SegSize != segSize {
		t.Fatalf("SegSize=%d want %d", sum.SegSize, segSize)
	}
	if sum.ObjectCount != perSeg+1 {
		t.Fatalf("ObjectCount=%d want %d", sum.ObjectCount, perSeg+1)
	}
	if sum.Total != 2 || sum.Full != 1 || sum.Active != 1 || sum.Free != 0 || sum.Reclaiming != 0 {
		t.Fatalf("summary=%+v want Total=2 Full=1 Active=1", sum)
	}
	if sum.CursorSeg != 1 || sum.CursorOff != int64(len(payload)) {
		t.Fatalf("Cursor=(%d,%d) want (1,%d)", sum.CursorSeg, sum.CursorOff, len(payload))
	}
	if len(entries) != 2 || entries[0].SegmentID != 0 || entries[1].SegmentID != 1 {
		t.Fatalf("entries=%+v want ascending [0 1]", entries)
	}
	if entries[0].State != metastore.SegmentStateFull || entries[0].AliveCount != perSeg {
		t.Fatalf("entry0=%+v want Full/%d", entries[0], perSeg)
	}

	// 持段 0 读引用 → 删光后稳定停留在 Reclaiming（后台 GC 不会回收有引用的段）。
	s.db.RefSegment(0)
	for i := 0; i < perSeg; i++ {
		if err := s.Delete(ctx, fmt.Sprintf("k%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	sum, _, err = s.Segments(ctx)
	if err != nil {
		t.Fatalf("Segments after delete: %v", err)
	}
	if sum.Reclaiming != 1 || sum.Active != 1 || sum.Free != 0 || sum.Full != 0 {
		t.Fatalf("after delete summary=%+v want Reclaiming=1 Active=1", sum)
	}
	if sum.ObjectCount != 1 {
		t.Fatalf("ObjectCount after delete=%d want 1", sum.ObjectCount)
	}
	s.db.UnrefSegment(0)

	// 释放引用后后台 GC（1s 周期）把 Reclaiming 段回收为 Free。
	deadline := time.Now().Add(3 * time.Second)
	for {
		sum, _, err = s.Segments(ctx)
		if err != nil {
			t.Fatalf("Segments while waiting GC: %v", err)
		}
		if sum.Free == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("segment not freed after 3s: %+v", sum)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sum.Reclaiming != 0 || sum.Full != 0 {
		t.Fatalf("final summary=%+v want Reclaiming=0 Full=0", sum)
	}
}

// TestAdminListKeysObjectMetaAndPing 覆盖 ListKeys 前缀过滤、ObjectMeta 与 Ping。
func TestAdminListKeysObjectMetaAndPing(t *testing.T) {
	s, _, _ := newTestStorage(t)
	defer s.Close()
	ctx := context.Background()

	payload := alignedPayload(int(layout.BlockSize))
	for _, k := range []string{"a/1", "a/2", "b/1"} {
		if err := s.Put(ctx, k, int64(len(payload)), payload); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.ListKeys(ctx, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("ListKeys all = %v,%v want 3 keys", all, err)
	}
	prefixed, err := s.ListKeys(ctx, "a/")
	if err != nil {
		t.Fatalf("ListKeys a/: %v", err)
	}
	if len(prefixed) != 2 || prefixed[0] != "a/1" || prefixed[1] != "a/2" {
		t.Fatalf("ListKeys a/ = %v want [a/1 a/2]", prefixed)
	}
	if none, err := s.ListKeys(ctx, "a/1/x"); err != nil || len(none) != 0 {
		t.Fatalf("ListKeys longer-than-key prefix = %v,%v want empty", none, err)
	}

	m, err := s.ObjectMeta(ctx, "a/1")
	if err != nil {
		t.Fatalf("ObjectMeta: %v", err)
	}
	if m.Size != int64(len(payload)) {
		t.Fatalf("ObjectMeta size=%d want %d", m.Size, len(payload))
	}
	if _, err := s.ObjectMeta(ctx, "nope"); err != ierr.ErrNotFound {
		t.Fatalf("ObjectMeta missing err=%v want ErrNotFound", err)
	}

	ts, err := s.Ping(ctx)
	if err != nil || ts <= 0 {
		t.Fatalf("Ping = %d,%v want >0,nil", ts, err)
	}
}
