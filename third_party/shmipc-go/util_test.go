/*
 * Copyright 2023 CloudWeGo Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package shmipc

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/shirou/gopsutil/v3/disk"
	"github.com/stretchr/testify/assert"
)

func TestAsyncSendErr(t *testing.T) {
	ch := make(chan error)
	asyncSendErr(ch, ErrTimeout)
	select {
	case <-ch:
		t.Fatalf("should not get")
	default:
	}

	ch = make(chan error, 1)
	asyncSendErr(ch, ErrTimeout)
	select {
	case <-ch:
	default:
		t.Fatalf("should get")
	}
}

func TestAsyncNotify(t *testing.T) {
	ch := make(chan struct{})
	asyncNotify(ch)
	select {
	case <-ch:
		t.Fatalf("should not get")
	default:
	}

	ch = make(chan struct{}, 1)
	asyncNotify(ch)
	select {
	case <-ch:
	default:
		t.Fatalf("should get")
	}
}

func TestMin(t *testing.T) {
	if min(1, 2) != 1 {
		t.Fatalf("bad")
	}
	if min(2, 1) != 1 {
		t.Fatalf("bad")
	}
}

func TestMinInt(t *testing.T) {
	if minInt(1, 2) != 1 {
		t.Fatalf("bad")
	}
	if minInt(2, 1) != 1 {
		t.Fatalf("bad")
	}
}

func TestMaxInt(t *testing.T) {
	if maxInt(1, 2) != 2 {
		t.Fatalf("bad")
	}
	if maxInt(2, 1) != 2 {
		t.Fatalf("bad")
	}
}

// fork 改写过 string2bytesZeroCopy（unsafe.Slice + unsafe.StringData）。
// 除了经由 WriteString 的间接覆盖，这里直接断言改动后的语义：
// 非空串返回 len == cap == len(s) 的只读视图，空串返回 nil。
func TestString2BytesZeroCopy(t *testing.T) {
	s := "hello shmipc"
	b := string2bytesZeroCopy(s)
	assert.Equal(t, len(s), len(b))
	assert.Equal(t, len(s), cap(b))
	assert.Equal(t, []byte(s), b)

	assert.Equal(t, 0, len(string2bytesZeroCopy("")))
	assert.Equal(t, true, string2bytesZeroCopy("") == nil)
}

func TestPathExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test_path_exists")
	f, err := os.OpenFile(path, os.O_CREATE, os.ModePerm)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	assert.Equal(t, true, pathExists(path))
	os.Remove(path)
	assert.Equal(t, false, pathExists(path))
}

func TestCanCreateOnDevShm(t *testing.T) {
	switch runtime.GOOS {
	case "linux":
		//just on /dev/shm, other always return true
		assert.Equal(t, true, canCreateOnDevShm(math.MaxUint64, "sdffafds"))
		stat, err := disk.Usage("/dev/shm")
		if err != nil {
			t.Fatal(err)
		}
		assert.Equal(t, true, canCreateOnDevShm(stat.Free, "/dev/shm/xxx"))
		assert.Equal(t, false, canCreateOnDevShm(stat.Free+1, "/dev/shm/yyy"))
	case "darwin":
		//always return true
		assert.Equal(t, true, canCreateOnDevShm(33333, "sdffafds"))
	}
}

func TestSafeRemoveUdsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test_path_remove")
	f, err := os.OpenFile(path, os.O_CREATE, os.ModePerm)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	assert.Equal(t, true, safeRemoveUdsFile(path))
	assert.Equal(t, false, safeRemoveUdsFile(filepath.Join(dir, "not_existing_file")))
	// a directory is not removed
	assert.Equal(t, false, safeRemoveUdsFile(dir))
}

func TestIsArmArch(t *testing.T) {
	assert.Equal(t, runtime.GOARCH == "arm" || runtime.GOARCH == "arm64", isArmArch())
}