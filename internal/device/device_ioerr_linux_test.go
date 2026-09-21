//go:build linux

package device

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/liucxer/taihu/internal/bufpool"
)

// TestDeviceIOErrorPaths 把设备 fd 临时换成「方向不符」的句柄（只写/只读），
// 使内核以 EBADF 拒绝该方向的 IO，从而覆盖各 IO 入口的错误分支
// （提交即失败或完成事件 Res<0，两种后端行为都覆盖到其中一条）。
func TestDeviceIOErrorPaths(t *testing.T) {
	devPath := filepath.Join(t.TempDir(), "nvme.img")
	f, err := os.Create(devPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dev, err := NewDevice(context.Background(), devPath, covSegSize)
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}
	orig := dev.f
	t.Cleanup(func() { _ = dev.Close() }) // 后注册先执行：先还原 fd，再 Close
	t.Cleanup(func() { dev.f = orig })

	ctx := context.Background()
	blk := bufpool.Get(4096)
	defer bufpool.Put(blk)
	dst := bufpool.Get(4096)
	defer bufpool.Put(dst)

	// 只写句柄上读：读方向 IO 必失败。
	wo, err := os.OpenFile(devPath, os.O_WRONLY|syscall.O_DIRECT, 0)
	if err != nil {
		t.Skipf("O_DIRECT 只写打开不可用: %v", err)
	}
	dev.f = wo
	if _, err := dev.ReadAt(ctx, 0, 0, 4096); err == nil {
		t.Fatal("只写句柄上 ReadAt 应失败")
	}
	if _, err := dev.ReadAtInto(ctx, 0, 0, 4096, dst); err == nil {
		t.Fatal("只写句柄上 ReadAtInto 应失败")
	}
	dev.f = orig
	_ = wo.Close()

	// 只读句柄上写：写方向 IO 必失败（4K 对齐快路径与拷贝兜底路径各一次）。
	ro, err := os.OpenFile(devPath, os.O_RDONLY|syscall.O_DIRECT, 0)
	if err != nil {
		t.Skipf("O_DIRECT 只读打开不可用: %v", err)
	}
	raw := bufpool.Get(8192)
	defer bufpool.Put(raw)
	dev.f = ro
	if err := dev.Append(ctx, 0, 0, 4096, blk[:4096]); err == nil {
		t.Fatal("只读句柄上 Append（对齐主体直写）应失败")
	}
	if err := dev.Append(ctx, 0, 8192, 4096, raw[1:4097]); err == nil {
		t.Fatal("只读句柄上 Append（首地址不对齐拷贝兜底）应失败")
	}
	dev.f = orig
	_ = ro.Close()
}
