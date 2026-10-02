//go:build linux

package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/liucxer/taihu/internal/transport"
)

// 本文件仅在 Linux 上编译：
// - TestBenchStorageCmdErrorPaths 依赖普通文件上 BLKGETSIZE64 失败的 Linux 行为；
// - TestBenchSingleCmdShmRoundTrip / benchTestShmServer 依赖 shmipc（仅 Linux 支持）。

// benchTestShmServer 起一个进程内 shmipc taihu-server（unix socket），返回 socket 路径。
func benchTestShmServer(t *testing.T) string {
	t.Helper()
	st := benchTestStorage(t)
	uds := filepath.Join(t.TempDir(), "taihu.sock")
	srv, err := transport.ServeShm(st, uds)
	if err != nil {
		t.Fatalf("ServeShm: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return uds
}

// TestBenchStorageCmdErrorPaths 覆盖容量查询/AIO 选项/pebble 打开失败与 memprofile 写失败。
func TestBenchStorageCmdErrorPaths(t *testing.T) {
	benchTestGlobals(t, "", "")
	dev, db := benchTestDisk(t)

	// 未打桩 deviceCapacity：普通文件上 BLKGETSIZE64 必失败（真实缝隙行为）。
	f := benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = dev, db, "write"
	if err := benchTestRunE(t, benchStorageCmd, f); err == nil ||
		!strings.Contains(err.Error(), "DeviceCapacity") {
		t.Fatalf("plain-file capacity err=%v want DeviceCapacity", err)
	}

	// -cpuprofile 指向目录：os.Create 失败。
	benchTestStubCapacity(t)
	f = benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = dev, db, "write"
	f["cpuprofile"] = t.TempDir()
	if err := benchTestRunE(t, benchStorageCmd, f); err == nil ||
		!strings.Contains(err.Error(), "cpuprofile") {
		t.Fatalf("cpuprofile dir err=%v", err)
	}

	// 全局 -io-uring 取值非法：aioOptions 在容量查询后报错。
	oldIO := global.ioUring
	global.ioUring = "bogus"
	t.Cleanup(func() { global.ioUring = oldIO })
	f = benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = dev, db, "write"
	if err := benchTestRunE(t, benchStorageCmd, f); err == nil ||
		!strings.Contains(err.Error(), "io-uring") {
		t.Fatalf("bad io-uring err=%v", err)
	}
	global.ioUring = "auto"

	// -db 指向普通文件：pebble 打开失败。
	f = benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = dev, dev, "write"
	if err := benchTestRunE(t, benchStorageCmd, f); err == nil ||
		!strings.Contains(err.Error(), "NewStorage") {
		t.Fatalf("NewStorage err=%v", err)
	}

	// -dev 不存在：容量查询（DeviceCapacity）先失败，早于 NewStorage。
	f = benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = filepath.Join(t.TempDir(), "missing.img"), db, "write"
	if err := benchTestRunE(t, benchStorageCmd, f); err == nil ||
		!strings.Contains(err.Error(), "DeviceCapacity") {
		t.Fatalf("missing dev err=%v", err)
	}

	// -memprofile 写失败：只打日志，命令本身仍然成功。
	f = benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = dev, db, "write"
	f["count"], f["size"] = "1", "4096"
	f["memprofile"] = t.TempDir()
	if err := benchTestRunE(t, benchStorageCmd, f); err != nil {
		t.Fatalf("memprofile 写失败不应中断压测: %v", err)
	}

	// 库中 key 不全时读：worker 出错 → 各 worker 结果汇聚为 runErr 并返回。
	// count=3/threads=2 同时覆盖 partitionRange 的 i<rem 分支。
	f = benchTestStorageFlags()
	f["dev"], f["db"], f["mode"] = dev, db, "read"
	f["threads"], f["count"] = "2", "3"
	if err := benchTestRunE(t, benchStorageCmd, f); err == nil ||
		!strings.Contains(err.Error(), "run error") {
		t.Fatalf("missing key read err=%v", err)
	}
}

// TestBenchSingleCmdShmRoundTrip 端到端：直连进程内 shmipc 服务端（unix socket）。
func TestBenchSingleCmdShmRoundTrip(t *testing.T) {
	benchTestGlobals(t, "", "")
	uds := benchTestShmServer(t)

	f := benchTestSingleFlags()
	f["transport"], f["shm"] = "shm", uds
	f["mode"], f["size"], f["count"] = "write", "4096", "2"
	if err := benchTestRunE(t, benchSingleCmd, f); err != nil {
		t.Fatalf("single shm write: %v", err)
	}
	f["mode"] = "read"
	if err := benchTestRunE(t, benchSingleCmd, f); err != nil {
		t.Fatalf("single shm read: %v", err)
	}
}
