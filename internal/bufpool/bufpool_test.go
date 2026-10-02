package bufpool

import (
	"testing"
	"unsafe"
)

// TestGetNonPositive 锁定 Get 的非正长度语义：返回 nil，而不是 panic。
// 回归自 Get(0) 的 "index out of range [52] with length 22" —— n<=0 时
// bufBucket 把 uint64(n-1) 下溢成 2^64-1，bits.Len64 得 64，算出越界索引。
func TestGetNonPositive(t *testing.T) {
	for _, n := range []int{0, -1, -4096} {
		if buf := Get(n); buf != nil {
			t.Fatalf("Get(%d) = len %d, want nil", n, len(buf))
		}
	}
}

// TestGetBucket 校验 2 幂档位语义：返回值是档位容量，可能大于请求的 n。
func TestGetBucket(t *testing.T) {
	cases := []struct{ n, want int }{
		{1, 4096}, {4096, 4096}, {4097, 8192}, {8192, 8192}, {8193, 16384},
	}
	for _, tc := range cases {
		buf := Get(tc.n)
		if len(buf) != tc.want || cap(buf) != tc.want {
			t.Fatalf("Get(%d) len=%d cap=%d, want %d", tc.n, len(buf), cap(buf), tc.want)
		}
		if off := uintptr(unsafe.Pointer(&buf[0])) % 4096; off != 0 {
			t.Fatalf("Get(%d) 缓冲未按 4K 对齐（余 %d）", tc.n, off)
		}
		Put(buf)
	}
}

// TestPutTruncatedSlice 归还「尾部截断但 cap 未失」的子切片应回到原档位 ——
// 这正是消费方填了 k 字节后用 buf[:k] 归还的形状（low 必须为 0：buf[low:] 会把
// cap 削掉 low，低偏移够大时会跌进低一档桶，那不是本测试要覆盖的约定）。
func TestPutTruncatedSlice(t *testing.T) {
	Put(Get(8192))       // 确保 8192 档有库存
	Put(Get(8192)[:200]) // cap 仍为 8192，应归回 8192 档
	if c := cap(Get(8192)); c != 8192 {
		t.Fatalf("截断子切片归还后取到 cap=%d, want 8192", c)
	}
}

// TestGetExactSemantics 精确池：len==cap==n 不按 2 幂取整，n<=0 为 nil。
func TestGetExactSemantics(t *testing.T) {
	for _, n := range []int{0, -1} {
		if b := GetExact(n); b != nil {
			t.Fatalf("GetExact(%d) = len %d, want nil", n, len(b))
		}
	}

	b := GetExact(4097)
	if len(b) != 4097 || cap(b) != 4097 {
		t.Fatalf("GetExact(4097) len=%d cap=%d, want 4097/4097", len(b), cap(b))
	}
	PutExact(b[:512])
	if c := cap(GetExact(4097)); c != 4097 {
		t.Fatalf("子切片归还后取到 cap=%d, want 4097", c)
	}

	// 两池互不相通：4097 只存在于精确池，Get 走的仍是 8192 档。
	if c := cap(Get(4097)); c != 8192 {
		t.Fatalf("Get(4097) cap=%d, want 8192（精确池不应被 Get 取到）", c)
	}
}
