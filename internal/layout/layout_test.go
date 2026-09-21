package layout

import "testing"

func TestComputeLayout(t *testing.T) {
	segSize := int64(8 * 1024 * 1024 * 1024) // 8GiB

	cases := []struct {
		name     string
		capacity int64
		wantCnt  int64
	}{
		{"16TiB 整除", 16 * 1024 * 1024 * 1024 * 1024, 2048},
		{"非整除向下取整", 16*1024*1024*1024*1024 + 1, 2048},
		{"小于一段", segSize - 1, 0},
		{"恰好一段", segSize, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ComputeLayout(c.capacity, segSize)
			if got.SegmentSizeBytes != segSize {
				t.Fatalf("SegmentSizeBytes=%d want %d", got.SegmentSizeBytes, segSize)
			}
			if got.SegmentCount != c.wantCnt {
				t.Fatalf("SegmentCount=%d want %d", got.SegmentCount, c.wantCnt)
			}
		})
	}

	// 非正入参：返回空布局（SegmentCount=0）。
	if got := ComputeLayout(0, segSize); got.SegmentCount != 0 {
		t.Fatalf("zero capacity: SegmentCount=%d want 0", got.SegmentCount)
	}
	if got := ComputeLayout(segSize, 0); got.SegmentCount != 0 {
		t.Fatalf("zero segSize: SegmentCount=%d want 0", got.SegmentCount)
	}
	// 负入参同样返回空布局（SegmentSizeBytes 也须为 0，不能带出非法段大小）。
	for _, c := range []struct{ cap, seg int64 }{{-1, segSize}, {segSize, -1}, {-1, -1}} {
		if got := ComputeLayout(c.cap, c.seg); got != (Layout{}) {
			t.Fatalf("ComputeLayout(%d,%d)=%+v want 空布局", c.cap, c.seg, got)
		}
	}
}

func TestAlign4k(t *testing.T) {
	if BlockSize != 4096 {
		t.Fatalf("BlockSize=%d want 4096", BlockSize)
	}
	cases := []struct{ in, want int64 }{
		{-1, 0},
		{0, 0},
		{1, BlockSize},
		{BlockSize - 1, BlockSize},
		{BlockSize, BlockSize},
		{BlockSize + 1, 2 * BlockSize},
		{3 * BlockSize, 3 * BlockSize},
		{DefaultSegmentSizeBytes, DefaultSegmentSizeBytes},
	}
	for _, c := range cases {
		if got := Align4k(c.in); got != c.want {
			t.Fatalf("Align4k(%d)=%d want %d", c.in, got, c.want)
		}
	}
}
