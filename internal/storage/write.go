package storage

import (
	"context"

	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/pkg/ierr"
)

// 写路径：Storage 方法的实现（方法非顶层声明，允许留在本文件；全部 public 顶层
// 声明集中在 storage.go —— 见该文件包注释）。存储顺序保证：先写设备数据、再写映射，
// 避免出现「有映射无数据」；分配与设备写解耦，并发 Put 可写不同偏移。

// Put 写入对象。size 为对象逻辑长度，in 提供数据（使用 in[:size] 的前 size 字节，
// in 不足 size 字节时报错）。先从 db 原子申请写位置（段满自动滚动），
// 再写设备数据，最后写映射。设备写位于分配锁之外，并发 Put 可写不同偏移。
// 等价于 PutBegin + PutAppend + PutCommit（供一次性写调用方；分段直写走三个方法）。
func (s *Storage) Put(ctx context.Context, key string, size int64, in []byte) error {
	seg, off, err := s.PutBegin(ctx, key, size)
	if err != nil {
		return err
	}
	if size > 0 {
		if err := s.PutAppend(ctx, seg, off, size, in); err != nil {
			return err
		}
	}
	return s.PutCommit(ctx, key, seg, off, size)
}

// PutBegin 校验 size 并原子分配段游标（返回 4K 对齐 off），开始分段写。
// key 仅语义占位（分配不依赖 key，映射在 PutCommit 建立）。
func (s *Storage) PutBegin(ctx context.Context, key string, size int64) (segmentID, off int64, err error) {
	if size < 0 {
		return 0, 0, ierr.ErrInvalidRange
	}
	if size > s.layout.SegmentSizeBytes {
		return 0, 0, ierr.ErrTooLarge
	}
	return s.db.AllocateSegment(size)
}

// PutAppend 直写一段数据到段内 off 处。data 首地址 4K 对齐时零拷贝直写设备
// （O_DIRECT 直写调用方缓冲），非对齐/尾段由 device.Append 内部对齐缓冲兜底。
// 分段调用方须保证 off 递增（off + 已写字节数）且各段相邻。
func (s *Storage) PutAppend(ctx context.Context, segmentID, off, size int64, data []byte) error {
	if int64(len(data)) < size {
		return ierr.ErrShortWrite
	}
	return s.dev.Append(ctx, segmentID, off, size, data)
}

// PutCommit 建立 key→(segmentID, off, size) 映射。顺序保证：先写设备数据，再写元数据，
// 避免出现「有映射无数据」。
func (s *Storage) PutCommit(ctx context.Context, key string, segmentID, off, size int64) error {
	meta := metastore.ObjectMeta{SegmentID: segmentID, Offset: off, Size: size}
	return s.db.PutMapping(ctx, key, meta)
}

// putItem 批量写的一个条目：把 Data 的前 Size 字节写入 Key。仅内部 batchPut 使用
// （无外部调用，默认私有）。
type putItem struct {
	Key  string
	Size int64
	Data []byte
}

// batchPut 批量写对象（等价于多次 Put 的批量版，供写流水线并发排空）：
// 先批量申请写位置（单次游标持久化）→ 一次设备批量写（单次 io_submit）→
// 一次 Pebble Batch 提交全部映射。任一项校验失败或设备写失败时整体返回错误
// （与 Put 语义一致：有数据落盘但无映射的孤儿块由 segment GC 兜底回收）。
// 无外部调用，私有。
func (s *Storage) batchPut(ctx context.Context, items []putItem) error {
	if len(items) == 0 {
		return nil
	}
	sizes := make([]int64, len(items))
	for i := range items {
		it := &items[i]
		if it.Size < 0 {
			return ierr.ErrInvalidRange
		}
		if it.Size > s.layout.SegmentSizeBytes {
			return ierr.ErrTooLarge
		}
		if int64(len(it.Data)) < it.Size {
			return ierr.ErrShortWrite
		}
		sizes[i] = it.Size
	}

	res, err := s.db.AllocateSegmentBatch(sizes)
	if err != nil {
		return err
	}
	jobs := make([]device.WriteJob, 0, len(items))
	for i := range items {
		if items[i].Size == 0 {
			continue
		}
		jobs = append(jobs, device.WriteJob{
			SegmentID: res[i].SegmentID,
			Off:       res[i].Offset,
			Data:      items[i].Data[:items[i].Size],
			Size:      items[i].Size,
		})
	}
	if len(jobs) > 0 {
		if err := s.dev.AppendBatch(ctx, jobs); err != nil {
			return err
		}
	}
	comm := make([]metastore.PutMappingItem, len(items))
	for i := range items {
		comm[i] = metastore.PutMappingItem{
			Key: items[i].Key,
			Meta: metastore.ObjectMeta{
				SegmentID: res[i].SegmentID,
				Offset:    res[i].Offset,
				Size:      items[i].Size,
			},
		}
	}
	return s.db.BatchPutMapping(ctx, comm)
}

// BatchAppend 批量设备写：Data[:Size] 写段内 Off 处（4K 对齐，末尾补零）。
// 供 shm 写流水线按帧批量排空数据段（不涉及分配/元数据）。
func (s *Storage) BatchAppend(ctx context.Context, jobs []device.WriteJob) error {
	return s.dev.AppendBatch(ctx, jobs)
}

// BatchPutCommit 批量建立 key→(segmentID,off,size) 映射（单个 Pebble Batch 原子提交）。
// 「先写设备数据、再写元数据」的顺序由调用方保证（本方法只做元数据批量提交）。
func (s *Storage) BatchPutCommit(ctx context.Context, items []metastore.PutMappingItem) error {
	return s.db.BatchPutMapping(ctx, items)
}

// BatchDelete 批量删除对象映射。返回 per-key 错误（key 不存在为 ierr.ErrNotFound）
// 与整体存储错误；物理空间回收留待 segment 级 GC。
func (s *Storage) BatchDelete(ctx context.Context, keys []string) ([]error, error) {
	return s.db.BatchDeleteMapping(ctx, keys)
}

// Delete 删除对象的持久化映射。缓存失效由 store 内部处理。物理空间回收留待 segment 级 GC。
// key 不存在时返回 ierr.ErrNotFound（metastore.DeleteMapping 内部已查映射并透传，无需外层预检）。
func (s *Storage) Delete(ctx context.Context, key string) error {
	return s.db.DeleteMapping(ctx, key)
}
