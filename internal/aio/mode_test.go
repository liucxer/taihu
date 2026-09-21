//go:build linux

package aio

import (
	"strings"
	"testing"
)

// TestModeString 覆盖 Mode.String() 的三个具名取值与未知取值的兜底格式。
func TestModeString(t *testing.T) {
	cases := []struct {
		m    Mode
		want string
	}{
		{ModeAuto, "auto"},
		{ModeLibAIO, "libaio"},
		{ModeIOUring, "io_uring"},
		{Mode(7), "Mode(7)"},
		{Mode(-1), "Mode(-1)"},
	}
	for _, c := range cases {
		if got := c.m.String(); got != c.want {
			t.Errorf("Mode(%d).String() = %q, want %q", int(c.m), got, c.want)
		}
	}
}

// TestParseMode 覆盖命令行取值解析：大小写/空白归一化、三个合法取值与非法取值。
func TestParseMode(t *testing.T) {
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeAuto, false},
		{"auto", ModeAuto, false},
		{"AUTO", ModeAuto, false},
		{"  Auto\t", ModeAuto, false},
		{"on", ModeIOUring, false},
		{"ON", ModeIOUring, false},
		{" off ", ModeLibAIO, false},
		{"Off", ModeLibAIO, false},
		{"bogus", ModeAuto, true},
		{"1", ModeAuto, true},
	}
	for _, c := range cases {
		got, err := ParseMode(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseMode(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMode(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMode(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestNewWithModeInvalidMaxEvents maxEvents 越界时两个后端都必须报错，不得静默建出队列。
func TestNewWithModeInvalidMaxEvents(t *testing.T) {
	for _, m := range []Mode{ModeLibAIO, ModeIOUring, ModeAuto} {
		for _, n := range []int{0, -1, 1<<16 + 1} {
			r, err := NewWithMode(n, m)
			if err == nil {
				_ = r.Close()
				t.Errorf("NewWithMode(%d, %v) 应报错", n, m)
			}
		}
	}
}

// TestNew New 必须固定走 libaio，且越界 maxEvents 报错。
func TestNew(t *testing.T) {
	r, err := New(4)
	if err != nil {
		t.Fatalf("New(4): %v", err)
	}
	defer func() { _ = r.Close() }()
	if _, ok := r.(*ring); !ok {
		t.Fatalf("New 应固定用 libaio 后端，得到 %T", r)
	}
	if r2, err := New(0); err == nil {
		_ = r2.Close()
		t.Error("New(0) 应报错")
	}
}

// TestNewWithOptionsEnvOverride EnvMode 仅在 ModeAuto 下生效，且非法取值必须报错
// 而不是静默降级。
func TestNewWithOptionsEnvOverride(t *testing.T) {
	// off → libaio
	t.Setenv(EnvMode, "off")
	r, err := NewWithOptions(4, Options{})
	if err != nil {
		t.Fatalf("EnvMode=off: %v", err)
	}
	if _, ok := r.(*ring); !ok {
		t.Errorf("EnvMode=off 应落到 libaio，得到 %T", r)
	}
	_ = r.Close()

	// 非法取值 → 报错（不得静默降级）
	t.Setenv(EnvMode, "bogus")
	if r, err := NewWithOptions(4, Options{}); err == nil {
		_ = r.Close()
		t.Error("非法 EnvMode 应报错")
	} else if !strings.Contains(err.Error(), EnvMode) {
		t.Errorf("err=%v 应带上环境变量名 %s", err, EnvMode)
	}

	// 显式指定后端时 env 被忽略（即使取值非法）
	if r, err := NewWithOptions(4, Options{Mode: ModeLibAIO}); err != nil {
		t.Errorf("显式 ModeLibAIO 时不应受非法 EnvMode 影响: %v", err)
	} else {
		_ = r.Close()
	}

	// auto（env 与内核能力都指向 auto）→ 走探测
	t.Setenv(EnvMode, "auto")
	rAuto, err := NewWithOptions(4, Options{})
	if err != nil {
		t.Fatalf("EnvMode=auto: %v", err)
	}
	_ = rAuto.Close()

	// on → io_uring（内核不支持时只能报错）
	t.Setenv(EnvMode, "on")
	rOn, err := NewWithOptions(4, Options{})
	if err != nil {
		if !Probe().Supported {
			t.Skipf("io_uring 不可用: %v", err)
		}
		t.Fatalf("EnvMode=on: %v", err)
	}
	if _, ok := rOn.(*uringRing); !ok {
		t.Errorf("EnvMode=on 应落到 io_uring，得到 %T", rOn)
	}
	_ = rOn.Close()

	// on + IOPoll：只创建不提交（目标块设备未开队列轮询时提交会挂死）
	t.Setenv(EnvMode, "on")
	rPoll, err := NewWithOptions(4, Options{IOPoll: true})
	if err != nil {
		if !Probe().Supported {
			t.Skipf("io_uring 不可用: %v", err)
		}
		t.Fatalf("EnvMode=on + IOPoll: %v", err)
	}
	_ = rPoll.Close()
}

// TestNewWithOptionsAuto Probe 结论决定 auto 的落点。真实缓存先固化以便覆盖
// 「探测成功→io_uring」「探测失败→libaio 回退」两条分支。
func TestNewWithOptionsAuto(t *testing.T) {
	real := Probe() // 先触发一次真实探测并固化缓存

	probeMu.Lock()
	savedInfo, savedDone := probeInfo, probeDone
	probeMu.Unlock()
	t.Cleanup(func() {
		probeMu.Lock()
		probeInfo, probeDone = savedInfo, savedDone
		probeMu.Unlock()
	})

	setProbe := func(i Info) {
		probeMu.Lock()
		probeInfo, probeDone = i, true
		probeMu.Unlock()
	}

	if real.Supported {
		setProbe(Info{Supported: true, Reason: "ok", Features: 0xf})
		r, err := NewWithOptions(4, Options{Mode: ModeAuto})
		if err != nil {
			t.Fatalf("auto+supported: %v", err)
		}
		if _, ok := r.(*uringRing); !ok {
			t.Errorf("探测支持时应建出 io_uring，得到 %T", r)
		}
		_ = r.Close()

		setProbe(Info{Supported: true, Reason: "ok", Features: 0xf})
		rp, err := NewWithOptions(4, Options{Mode: ModeAuto, IOPoll: true})
		if err != nil {
			t.Fatalf("auto+supported+iopoll: %v", err)
		}
		_ = rp.Close()
	} else {
		t.Logf("本机 io_uring 不可用（%s），跳过 auto 命中 io_uring 的分支", real.Reason)
	}

	// 探测结论为「不支持」→ 回退 libaio 并记录原因
	setProbe(Info{Supported: false, Reason: "test: 模拟内核不支持 io_uring"})
	r2, err := NewWithOptions(4, Options{Mode: ModeAuto})
	if err != nil {
		t.Fatalf("auto+unsupported: %v", err)
	}
	if _, ok := r2.(*ring); !ok {
		t.Errorf("探测不支持时应回退 libaio，得到 %T", r2)
	}
	_ = r2.Close()

	// 回退路径上 maxEvents 越界同样必须报错（不得静默建出队列）。
	if r3, err := NewWithOptions(0, Options{Mode: ModeAuto}); err == nil {
		_ = r3.Close()
		t.Error("auto 回退 libaio 时 maxEvents 越界也应报错")
	}
}
