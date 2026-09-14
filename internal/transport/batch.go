// 写/删批处理流水线（batchWriter / batchDeleter）：与读路径的 shmBatchReader 对称，
// 把「逐对象串行写/删」改造为「队列 + worker 池 + 批量提交」：
//   - 写：各流 goroutine 把完整对象（PutData 帧攒齐、PutBegin 已分配位置）投递到写队列，
//     worker 池 drain 取批（清空当前队列，至多 batchCap 个；队空才阻塞），
//     批内一次 device.AppendBatch（单次 io_submit 排空全部数据帧）→ 一次 BatchPutCommit
//     （单个 Pebble Batch 提交全部映射），再逐任务回投 done，由流 goroutine 写 OpResp 帧。
//   - 删：各流 goroutine 把一元 key 投递到删队列，worker 池 drain 取批后一次
//     BatchDelete（批量查 + 单个 Pebble Batch 删除），再逐任务回投 per-key 结果。
//
// 保序：同一流（同一 shmipc.Stream / rpc streamID）的请求由该流 goroutine 串行投递并串行
// 等 done，天然保序；跨流之间由 rr 轮转分发做负载均衡。worker 内存中取批、无攒批等待，
// CPU 空闲时天然退化为 batch(1) 单条处理，避免尾延迟。
//
// worker 生命周期：随进程起（newBatchWriter/newBatchDeleter 启动 goroutine）、随进程止
// （与 shmBatchReader.run 一致，不做主动关闭；Server 停机路径不依赖队列排空）。
package transport

import (
	"context"
	"sync/atomic"

	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/internal/storage"
)

// PipelineConfig 服务端读/写/删批处理流水线配置（-batch / -write-batch / -del-batch 映射）。
// 各字段 ≤0 表示对应流水线关闭（退化为逐请求串行处理，保持旧行为）。
type PipelineConfig struct {
	// 读（现有 shmBatchReader，仅 shm 路径使用）。
	ReadBatch    int
	ReadWorkers  int
	// 写（batchWriter）。
	WriteBatch    int
	WriteWorkers  int
	// 删（batchDeleter）。
	DeleteBatch    int
	DeleteWorkers  int
}

// pipeline 组装写/删两条流水线（读由 shmServer.batched 独立管理）。
type pipeline struct {
	w *batchWriter
	d *batchDeleter
}

// newPipeline 构建写/删流水线；对应 batch ≤0 时为 nil（调用方走旧路径）。
func newPipeline(st *storage.Storage, cfg PipelineConfig) *pipeline {
	return &pipeline{
		w: newBatchWriter(st, cfg.WriteWorkers, cfg.WriteBatch),
		d: newBatchDeleter(st, cfg.DeleteWorkers, cfg.DeleteBatch),
	}
}

// writeTask 写流水线的一个待批任务：把对象 key（数据已由流 goroutine 攒齐到 jobs，
// 段内位置 PutBegin 已分配）写入设备并提交映射。Data 切片须由调用方持有到 submit 返回
// （submit 同步等待 AppendBatch + BatchPutCommit 完成）。shm 路径的 Data 直引共享内存，
// submit 完成后由流 goroutine 统一 ReleasePreviousRead 归还环形缓冲。
type writeTask struct {
	key  string
	seg  int64
	off  int64
	size int64
	jobs []device.WriteJob // 该对象全部数据帧（off 连续递增，4K 对齐）
	done chan error
}

// batchWriter 写流水线协调器：多个 worker 各跑一个 run goroutine，任务按 rr 轮转分发
// 各 worker 独立取批提交（并行批提交，避免单 worker 串行化整机写并发）。
type batchWriter struct {
	st       *storage.Storage
	batchCap int
	queues   []chan *writeTask
	rr       atomic.Uint64
}

// newBatchWriter 构建写流水线并启动 worker 池。batchCap≤0 返回 nil（关闭）。
func newBatchWriter(st *storage.Storage, workers, batchCap int) *batchWriter {
	if batchCap <= 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	b := &batchWriter{
		st:       st,
		batchCap: batchCap,
		queues:   make([]chan *writeTask, workers),
	}
	for i := range b.queues {
		b.queues[i] = make(chan *writeTask, batchCap*2)
		go b.run(b.queues[i])
	}
	return b
}

// submit 投递一个完整对象的写任务并阻塞至完成，返回批处理错误（设备写失败/映射提交失败）。
func (b *batchWriter) submit(t *writeTask) error {
	w := (b.rr.Add(1) - 1) % uint64(len(b.queues))
	b.queues[w] <- t
	return <-t.done
}

// run 单 worker 主循环：drain 取批 → 一次 AppendBatch → 一次 BatchPutCommit → 逐任务回投。
func (b *batchWriter) run(q chan *writeTask) {
	for {
		batch := drain(q, b.batchCap)
		if len(batch) == 0 {
			continue
		}
		statWriteBatches.Add(1)
		statWriteItems.Add(int64(len(batch)))

		total := 0
		for _, t := range batch {
			total += len(t.jobs)
		}
		var err error
		if total > 0 {
			jobs := make([]device.WriteJob, 0, total)
			for _, t := range batch {
				jobs = append(jobs, t.jobs...)
			}
			err = b.st.BatchAppend(context.Background(), jobs)
		}
		if err == nil {
			items := make([]metastore.PutMappingItem, len(batch))
			for i, t := range batch {
				items[i] = metastore.PutMappingItem{
					Key: t.key,
					Meta: metastore.ObjectMeta{
						SegmentID: t.seg,
						Offset:    t.off,
						Size:      t.size,
					},
				}
			}
			err = b.st.BatchPutCommit(context.Background(), items)
		}
		for i := range batch {
			batch[i].done <- err
		}
	}
}

// deleteTask 删流水线的一个待批任务：删除 key 的对象映射，done 回投 per-key 错误。
type deleteTask struct {
	key  string
	done chan error
}

// batchDeleter 删流水线协调器（结构与 batchWriter 对称：rr 轮转 + 多 worker 并行取批）。
type batchDeleter struct {
	st       *storage.Storage
	batchCap int
	queues   []chan *deleteTask
	rr       atomic.Uint64
}

// newBatchDeleter 构建删流水线并启动 worker 池。batchCap≤0 返回 nil（关闭）。
func newBatchDeleter(st *storage.Storage, workers, batchCap int) *batchDeleter {
	if batchCap <= 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	b := &batchDeleter{
		st:       st,
		batchCap: batchCap,
		queues:   make([]chan *deleteTask, workers),
	}
	for i := range b.queues {
		b.queues[i] = make(chan *deleteTask, batchCap*2)
		go b.run(b.queues[i])
	}
	return b
}

// submit 投递一个删除任务并阻塞至完成，返回 per-key 错误（key 不存在为 ierr.ErrNotFound）。
func (b *batchDeleter) submit(key string) error {
	w := (b.rr.Add(1) - 1) % uint64(len(b.queues))
	t := &deleteTask{key: key, done: make(chan error, 1)}
	b.queues[w] <- t
	return <-t.done
}

// run 单 worker 主循环：drain 取批 → 一次 BatchDelete → 逐任务回投 per-key 结果。
func (b *batchDeleter) run(q chan *deleteTask) {
	for {
		batch := drain(q, b.batchCap)
		if len(batch) == 0 {
			continue
		}
		statDeleteBatches.Add(1)
		statDeleteItems.Add(int64(len(batch)))

		keys := make([]string, len(batch))
		for i, t := range batch {
			keys[i] = t.key
		}
		errs, err := b.st.BatchDelete(context.Background(), keys)
		if err != nil {
			for i := range batch {
				batch[i].done <- err
			}
			continue
		}
		for i := range batch {
			batch[i].done <- errs[i]
		}
	}
}

// drain 阻塞取首个任务，随后一次性清空当前队列内全部任务（至多 max 个）返回。
// 满足「不攒批等待：有多少取多少；达到 max 是批量上限而非下限」。max≤0 时只取一个。
func drain[T any](q chan T, max int) []T {
	t := <-q
	batch := []T{t}
	for len(batch) < max {
		select {
		case t := <-q:
			batch = append(batch, t)
		default:
			return batch
		}
	}
	return batch
}