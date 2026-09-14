package rpcclient_test

import (
	"context"
	"errors"
	"testing"

	"github.com/liucxer/taihu/pkg/rpcclient"
)

// 本文件以**外部测试包**（package rpcclient_test，不是 package rpcclient）身份，
// 把本包导出的、以及出现在导出签名里的类型与常量逐个命名一遍。
//
// 为什么需要：reexport.go 里的 alias 是人手维护的，**漏补一条编译器不会报错** ——
// 包照样编过，只是对外悄悄不可用（调用方写不出那个类型的变量、构造不了字面量）。
// 本文件从外部包视角把这些名字全用一遍，漏了就编译失败，把「静默不可用」变成编译期错误。
//
// 局限（诚实说明）：外部测试包与库同属一个 module，因此它**不能**完全复现
// 「internal 类型对外不可命名」这一现象 —— 真正的门外验证靠模块外的消费者探针
// （/tmp 下用另一个 module + replace 测）。本文件是仓库内的廉价兜底，守住 alias 漏补。

// extStore 在**调用方自己的包**里实现 rpcclient.ObjectStore。
// 这验证接口方法签名里没有 internal 类型 —— 否则外部无法实现（方法签名里出现不可命名的
// 类型时，外部类型无法满足该接口）。
type extStore struct{}

func (extStore) Put(ctx context.Context, key string, size int64, in []byte) error { return nil }

func (extStore) Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error) {
	return nil, func() {}, nil
}

func (extStore) Delete(ctx context.Context, key string) error { return nil }

func (extStore) Stat(ctx context.Context, key string) (int64, error) { return 0, nil }

func (extStore) Close() error { return nil }

var (
	// 外部实现可满足接口，且具体类型也可赋值给接口变量。
	_ rpcclient.ObjectStore = extStore{}
	_ rpcclient.ObjectStore = (*rpcclient.Storage)(nil)
)

func TestPublicSurfaceNameable(t *testing.T) {
	// 1) 类型可命名：结构体字段类型必须能写出来，否则调用方拿不到 admin 结果。
	var (
		_ rpcclient.ObjectMeta
		_ rpcclient.SegmentEntry
		_ rpcclient.SegmentSummary
		_ rpcclient.SegmentState
	)

	// 放进调用方自己的结构体（带 json tag，模拟真实用法）。
	type holder struct {
		Meta     rpcclient.ObjectMeta     `json:"meta"`
		Summary  rpcclient.SegmentSummary `json:"summary"`
		Segments []rpcclient.SegmentEntry `json:"segments,omitempty"`
	}
	_ = holder{Summary: rpcclient.SegmentSummary{Total: 1}}

	// 2) 常量与类型配套：只导类型不导常量的话，map 字面量的 key 写不出来。
	counts := map[rpcclient.SegmentState]int{
		rpcclient.SegmentStateFree:       0,
		rpcclient.SegmentStateActive:     0,
		rpcclient.SegmentStateFull:       0,
		rpcclient.SegmentStateReclaiming: 0,
		rpcclient.SegmentStateCompacting: 0,
	}
	if len(counts) != 5 {
		t.Fatalf("段状态常量应恰好 5 个，实际 %d（有两个常量取了同一个值？）", len(counts))
	}

	// 3) 错误身份可判断：外部要用 errors.Is / == 区分「key 不存在」等情形。
	errs := map[string]error{
		"ErrNotFound":     rpcclient.ErrNotFound,
		"ErrInvalidRange": rpcclient.ErrInvalidRange,
		"ErrTooLarge":     rpcclient.ErrTooLarge,
		"ErrNoSpace":      rpcclient.ErrNoSpace,
		"ErrShortWrite":   rpcclient.ErrShortWrite,
	}
	seen := map[error]string{}
	for name, err := range errs {
		if err == nil {
			t.Fatalf("%s 为 nil", name)
		}
		if !errors.Is(err, err) {
			t.Fatalf("%s 不自等（errors.Is 失败）", name)
		}
		if prev, dup := seen[err]; dup {
			t.Fatalf("%s 与 %s 是同一个错误身份", name, prev)
		}
		seen[err] = name
	}
}
