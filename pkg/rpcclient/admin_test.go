package rpcclient

import (
	"context"
	"testing"

	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/pkg/taihu"
)

// admin RPC 往返测试（taihu-cli 设计文档 §4）：Ping / Meta / Segments / ListKeys。
// 复用 pool_test.go 的本机 TCP taihu-server。

func TestAdminPing(t *testing.T) {
	addr, cleanup := newTestServer(t)
	defer cleanup()

	s, err := Dial(context.Background(), addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()

	rtt, serverTime, err := s.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if serverTime <= 0 {
		t.Fatalf("Ping serverTime=%d want > 0", serverTime)
	}
	if rtt <= 0 {
		t.Fatalf("Ping rtt=%v want > 0", rtt)
	}
}

func TestAdminMeta(t *testing.T) {
	addr, cleanup := newTestServer(t)
	defer cleanup()

	s, err := Dial(context.Background(), addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// 缺失 key → ErrNotFound。
	if _, err := s.Meta(ctx, "no-such-key"); err != taihu.ErrNotFound {
		t.Fatalf("Meta missing: %v, want ErrNotFound", err)
	}

	const size = 4096 // 4K 对齐，段内偏移应恒 4K 对齐
	if err := s.Put(ctx, "m1", size, make([]byte, size)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var m taihu.ObjectMeta
	m, err = s.Meta(ctx, "m1")
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if m.Size != size {
		t.Fatalf("Meta size=%d want %d", m.Size, size)
	}
	if m.SegmentID < 0 || m.Offset%4096 != 0 {
		t.Fatalf("Meta seg=%d off=%d: want seg>=0 且 off 4K 对齐", m.SegmentID, m.Offset)
	}
}

func TestAdminSegments(t *testing.T) {
	addr, cleanup := newTestServer(t)
	defer cleanup()

	s, err := Dial(context.Background(), addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.Put(ctx, "s1", 4096, make([]byte, 4096)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// 新实例初始无 Free 段（段状态表随首次写入创建）；至少应有 Active 段。
	var sum taihu.SegmentSummary
	var entries []taihu.SegmentEntry
	sum, entries, err = s.Segments(ctx)
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}
	if sum.Total != int64(len(entries)) {
		t.Fatalf("Total=%d entries=%d 不一致", sum.Total, len(entries))
	}
	if sum.ObjectCount < 1 {
		t.Fatalf("ObjectCount=%d want >= 1", sum.ObjectCount)
	}
	if sum.Active < 1 {
		t.Fatalf("Active=%d: 写后应至少 1", sum.Active)
	}
	// 明细按 SegmentID 升序。
	for i := 1; i < len(entries); i++ {
		if entries[i].SegmentID <= entries[i-1].SegmentID {
			t.Fatalf("entries 未按 SegmentID 升序: %d -> %d", entries[i-1].SegmentID, entries[i].SegmentID)
		}
	}
	// 写游标段必须存在且为 Active。
	if sum.CursorSeg < 0 {
		t.Fatalf("CursorSeg=%d 非法", sum.CursorSeg)
	}
	found := false
	for _, e := range entries {
		if e.SegmentID == sum.CursorSeg && e.State == metastore.SegmentStateActive {
			found = true
		}
	}
	if !found {
		t.Fatalf("cursor seg %d 不在 Active 明细中", sum.CursorSeg)
	}
}

func TestAdminListKeys(t *testing.T) {
	addr, cleanup := newTestServer(t)
	defer cleanup()

	s, err := Dial(context.Background(), addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	keys := []string{"rk-0001", "rk-0002", "other", "rk-0003"}
	for _, k := range keys {
		if perr := s.Put(ctx, k, 4096, make([]byte, 4096)); perr != nil {
			t.Fatalf("Put %s: %v", k, perr)
		}
	}

	var got []string
	got, err = s.ListKeys(ctx, "")
	if err != nil {
		t.Fatalf("ListKeys all: %v", err)
	}
	if len(got) != len(keys) {
		t.Fatalf("ListKeys all got %d want %d", len(got), len(keys))
	}

	got, err = s.ListKeys(ctx, "rk-")
	if err != nil {
		t.Fatalf("ListKeys prefix: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListKeys rk- got %d want 3: %v", len(got), got)
	}
	for _, k := range got {
		if len(k) < 3 || k[:3] != "rk-" {
			t.Fatalf("ListKeys 返回了非前缀 key: %q", k)
		}
	}

	got, err = s.ListKeys(ctx, "not-exist-")
	if err != nil {
		t.Fatalf("ListKeys nomatch: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListKeys nomatch got %d want 0", len(got))
	}
}