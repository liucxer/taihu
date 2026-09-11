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
}
