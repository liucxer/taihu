// 写/删批处理流水线（batch.go）测试：drain 取批边界、worker 池 submit 语义、
// 线上启用流水线后的 Put/Delete 真往返。
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/liucxer/taihu/internal/bufpool"
	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/ierr"
	"github.com/liucxer/taihu/internal/transport/protocol"
)

// TestTCPDrainBatch 覆盖 drain 的取批上限与「有多少取多少」语义。
func TestTCPDrainBatch(t *testing.T) {
	t.Run("上限<=0 只取一个", func(t *testing.T) {
		q := make(chan int, 4)
		q <- 1
		q <- 2
		if got := drain(q, 0); len(got) != 1 || got[0] != 1 {
			t.Fatalf("drain(max=0) = %v", got)
		}
		if got := drain(q, -3); len(got) != 1 || got[0] != 2 {
			t.Fatalf("drain(max<0) = %v", got)
		}
	})

	t.Run("按 max 截断", func(t *testing.T) {
		q := make(chan int, 8)
		for i := 0; i < 5; i++ {
			q <- i
		}
		got := drain(q, 3)
		if len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 2 {
			t.Fatalf("drain(max=3) = %v", got)
		}
		rest := drain(q, 10)
		if len(rest) != 2 || rest[0] != 3 || rest[1] != 4 {
			t.Fatalf("drain(rest) = %v", rest)
		}
	})

	t.Run("队空阻塞至首个任务", func(t *testing.T) {
		q := make(chan int)
		done := make(chan []int, 1)
		go func() { done <- drain(q, 8) }()
		q <- 7
		select {
		case got := <-done:
			if len(got) != 1 || got[0] != 7 {
				t.Fatalf("drain = %v", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("drain 未在首个任务到达后返回")
		}
	})
}

// TestTCPBatchWriterSubmit 覆盖写流水线的 worker 池构建、批量提交与失败回投。
func TestTCPBatchWriterSubmit(t *testing.T) {
	st := tcpNewTestStorage(t)
	ctx := context.Background()

	if b := newBatchWriter(st, 2, 0); b != nil {
		t.Fatal("batchCap<=0 应关闭写流水线")
	}
	if b := newBatchWriter(st, 0, 4); b == nil || len(b.queues) != 1 {
		t.Fatalf("workers<1 应回落 1 个 worker: %+v", b)
	}
	b := newBatchWriter(st, 3, 4)
	if b == nil || b.batchCap != 4 || len(b.queues) != 3 {
		t.Fatalf("batchWriter = %+v", b)
	}

	itemsBefore := statWriteItems.Load()
	batchesBefore := statWriteBatches.Load()

	for i := 0; i < 6; i++ {
		key := fmt.Sprintf("tcp/batch/w/%d", i)
		size := int64(4096 + i)
		seg, off, err := st.PutBegin(ctx, key, size)
		if err != nil {
			t.Fatalf("PutBegin %s: %v", key, err)
		}
		buf := bufpool.Get(int(size))
		for j := range buf[:size] {
			buf[j] = byte(j + i)
		}
		wt := &writeTask{
			key: key, seg: seg, off: off, size: size,
			jobs: []device.WriteJob{{SegmentID: seg, Off: off, Data: buf[:size], Size: size}},
			done: make(chan error, 1),
		}
		if err := b.submit(wt); err != nil {
			t.Fatalf("submit %s: %v", key, err)
		}
		bufpool.Put(buf)

		got, err := st.ReadAt(ctx, key, 0, size)
		if err != nil {
			t.Fatalf("ReadAt %s: %v", key, err)
		}
		if int64(len(got)) != size {
			t.Fatalf("ReadAt %s len=%d want %d", key, len(got), size)
		}
		for j := range got {
			if got[j] != byte(j+i) {
				t.Fatalf("ReadAt %s 数据不一致 @%d", key, j)
			}
		}
		bufpool.Put(got)
	}
	if d := statWriteItems.Load() - itemsBefore; d < 6 {
		t.Fatalf("statWriteItems delta = %d, want >=6", d)
	}
	if statWriteBatches.Load() == batchesBefore {
		t.Fatal("statWriteBatches 未增长")
	}

	// size==0 任务：无设备写，仅提交映射
	seg0, off0, err := st.PutBegin(ctx, "tcp/batch/w/zero", 0)
	if err != nil {
		t.Fatalf("PutBegin zero: %v", err)
	}
	if err := b.submit(&writeTask{
		key: "tcp/batch/w/zero", seg: seg0, off: off0, size: 0, done: make(chan error, 1),
	}); err != nil {
		t.Fatalf("submit zero: %v", err)
	}
	if sz, err := st.Stat(ctx, "tcp/batch/w/zero"); err != nil || sz != 0 {
		t.Fatalf("Stat(zero) = %d, %v", sz, err)
	}

	// 设备写失败（数据短于声明长度）→ submit 回投错误，且不建映射
	key := "tcp/batch/w/short"
	seg, off, err := st.PutBegin(ctx, key, 4096)
	if err != nil {
		t.Fatalf("PutBegin short: %v", err)
	}
	if err := b.submit(&writeTask{
		key: key, seg: seg, off: off, size: 4096,
		jobs: []device.WriteJob{{SegmentID: seg, Off: off, Data: make([]byte, 8), Size: 4096}},
		done: make(chan error, 1),
	}); err == nil {
		t.Fatal("设备短写应使 submit 报错")
	}
	if _, err := st.Stat(ctx, key); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("失败任务不应建立映射: %v", err)
	}
}

// TestTCPBatchDeleterSubmit 覆盖删流水线的 per-key 结果回投。
func TestTCPBatchDeleterSubmit(t *testing.T) {
	st := tcpNewTestStorage(t)
	ctx := context.Background()

	if d := newBatchDeleter(st, 2, 0); d != nil {
		t.Fatal("batchCap<=0 应关闭删流水线")
	}
	d := newBatchDeleter(st, 0, 8) // workers<1 → 回落 1
	if d == nil || len(d.queues) != 1 || d.batchCap != 8 {
		t.Fatalf("batchDeleter = %+v", d)
	}

	keys := []string{"tcp/batch/d/0", "tcp/batch/d/1", "tcp/batch/d/2"}
	for _, k := range keys {
		if err := st.Put(ctx, k, 4, []byte("data")); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	itemsBefore := statDeleteItems.Load()
	batchesBefore := statDeleteBatches.Load()
	for _, k := range keys {
		if err := d.submit(k); err != nil {
			t.Fatalf("submit %s: %v", k, err)
		}
	}
	if delta := statDeleteItems.Load() - itemsBefore; delta < int64(len(keys)) {
		t.Fatalf("statDeleteItems delta = %d, want >=%d", delta, len(keys))
	}
	if statDeleteBatches.Load() == batchesBefore {
		t.Fatal("statDeleteBatches 未增长")
	}
	for _, k := range keys {
		if _, err := st.Stat(ctx, k); !errors.Is(err, ierr.ErrNotFound) {
			t.Fatalf("Stat(%s) after delete = %v", k, err)
		}
	}
	if err := d.submit("tcp/batch/d/missing"); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("submit(missing) err=%v, want ErrNotFound", err)
	}
}

// TestTCPNewPipelineConfig 覆盖流水线配置的默认值与关闭语义。
func TestTCPNewPipelineConfig(t *testing.T) {
	st := tcpNewTestStorage(t)

	if p := newPipeline(st, PipelineConfig{}); p == nil || p.w != nil || p.d != nil {
		t.Fatalf("默认配置应关闭写/删流水线: %+v", p)
	}
	p := newPipeline(st, PipelineConfig{WriteBatch: 4, WriteWorkers: 2, DeleteBatch: 4, DeleteWorkers: 2})
	if p.w == nil || p.d == nil || len(p.w.queues) != 2 || len(p.d.queues) != 2 {
		t.Fatalf("pipeline = %+v", p)
	}
	if srv := NewServer(st); srv.pipeline == nil || srv.pipeline.w != nil || srv.pipeline.d != nil {
		t.Fatal("NewServer 应默认关闭写/删流水线")
	}
	if srv := NewServerWithOptions(st, PipelineConfig{WriteBatch: 2, DeleteBatch: 2}); srv.pipeline.w == nil || srv.pipeline.d == nil {
		t.Fatal("NewServerWithOptions 应按配置启用流水线")
	}
}

// TestTCPPipelineServerRoundTrip 覆盖线上启用写/删流水线后的真往返（含批量聚合与 per-key 错误）。
func TestTCPPipelineServerRoundTrip(t *testing.T) {
	st := tcpNewTestStorage(t)
	cfg := PipelineConfig{WriteBatch: 4, WriteWorkers: 2, DeleteBatch: 4, DeleteWorkers: 2}
	addr := tcpServe(t, st, &cfg)
	c := tcpDial(t, addr)
	ctx := context.Background()

	writeBefore := statWriteItems.Load()
	delBefore := statDeleteItems.Load()

	for i, size := range []int64{1, 4096, protocol.ChunkSize + 7} {
		key := fmt.Sprintf("tcp/pipe/%d", i)
		payload := make([]byte, size)
		for j := range payload {
			payload[j] = byte(j*3 + i)
		}
		if err := c.Put(ctx, key, size, payload); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
		got, rel, err := c.Get(ctx, key, 0, size)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("Get %s: len=%d err=%v", key, len(got), err)
		}
		rel()
		if sz, err := c.Stat(ctx, key); err != nil || sz != size {
			t.Fatalf("Stat %s = %d, %v", key, sz, err)
		}
		if err := c.Delete(ctx, key); err != nil {
			t.Fatalf("Delete %s: %v", key, err)
		}
	}

	// 删不存在的 key：per-key 错误经流水线回投为 ErrNotFound
	if err := c.Delete(ctx, "tcp/pipe/missing"); !errors.Is(err, ierr.ErrNotFound) {
		t.Fatalf("Delete(missing) err=%v, want ErrNotFound", err)
	}
	if d := statWriteItems.Load() - writeBefore; d < 3 {
		t.Fatalf("写流水线未处理任务: delta=%d", d)
	}
	if d := statDeleteItems.Load() - delBefore; d < 4 {
		t.Fatalf("删流水线未处理任务: delta=%d", d)
	}
}
