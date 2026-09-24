package ierr

import (
	"errors"
	"fmt"
	"testing"
)

// TestErrorValues 校验公共错误集的「唯一事实源」契约：非 nil、消息非空且互不重复，
// 且经 %w 包装后仍可被 errors.Is 命中（跨层错误判定依赖该语义）。
func TestErrorValues(t *testing.T) {
	all := []struct {
		name string
		err  error
	}{
		{"ErrNotFound", ErrNotFound},
		{"ErrInvalidRange", ErrInvalidRange},
		{"ErrTooLarge", ErrTooLarge},
		{"ErrNoSpace", ErrNoSpace},
		{"ErrShortWrite", ErrShortWrite},
		{"ErrConflict", ErrConflict},
	}
	if len(all) == 0 {
		t.Fatal("错误集为空")
	}
	seen := make(map[string]string, len(all))
	for _, c := range all {
		if c.err == nil {
			t.Fatalf("%s 为 nil", c.name)
		}
		msg := c.err.Error()
		if msg == "" {
			t.Fatalf("%s 消息为空", c.name)
		}
		if prev, dup := seen[msg]; dup {
			t.Fatalf("%s 与 %s 消息重复: %q", c.name, prev, msg)
		}
		seen[msg] = c.name

		if !errors.Is(c.err, c.err) {
			t.Fatalf("%s 不能被自身 errors.Is 命中", c.name)
		}
		if !errors.Is(fmt.Errorf("wrap: %w", c.err), c.err) {
			t.Fatalf("%s 包装后不可被 errors.Is 命中", c.name)
		}
		// 各错误互不相等：包装后也不应误命中其他错误。
		for _, o := range all {
			if o.name == c.name {
				continue
			}
			if errors.Is(fmt.Errorf("wrap: %w", c.err), o.err) {
				t.Fatalf("%s 包装后被误判为 %s", c.name, o.name)
			}
		}
	}
}
