package taihu

import (
	"context"
	"fmt"
	"io"

	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
)

// Storage 对外 object 存储。数据写底层裸设备，key→位置映射经由 metastore（pebble）持久化，
// 其内部自带元数据加速缓存；Storage 不感知缓存细节。
//
// Storage 不持有写游标状态；「申请写位置」下沉到 db（metastore.Store.AllocateSegment），
// 其内部锁只覆盖廉价的游标分配，真正的设备写在此锁外执行 → 多 Put 可并发写不同偏移。
type Storage struct {
	db  metastore.Store
	dev *device.Device

	// 设备物理布局（启动时按读取的真实 nvme 容量计算注入，供对象上限/容量上报）。
	layout layout.Layout
}

// NewStorage 构建 Storage：打开 pebble、打开裸设备。l 为设备物理布局（段大小/段数），
// 由启动时读取的真实设备容量经 layout.ComputeLayout 计算。写游标由 db 首次分配时懒加载恢复。
// 返回 (*Storage, error)，与 v1 文档略有出入，便于暴露初始化错误。
func NewStorage(ctx context.Context, rocksdbDir, nvmePath string, l layout.Layout) (*Storage, error) {
	db, err := metastore.Open(rocksdbDir, l)
	if err != nil {
		return nil, err
	}
	dev, err := device.NewDevice(ctx, nvmePath, l.SegmentSizeBytes)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	s := &Storage{
		db:     db,
		dev:    dev,
		layout: l,
	}
	return s, nil
}

// MaxObjectSize 单对象大小上限（= 单段大小，Put/传输层校验用）。
func (s *Storage) MaxObjectSize() int64 { return s.layout.SegmentSizeBytes }

// Layout 返回设备物理布局（段大小/段数，管理/容量上报用）。
func (s *Storage) Layout() layout.Layout { return s.layout }

// LoadCache 预热 store（metastore）内部的元数据加速缓存。用于 bench / 已知 key 集合场景，
// 消除 GetMapping 的未命中回查，从而测纯设备读写带宽。
func (s *Storage) LoadCache(ctx context.Context) error {
	return s.db.LoadCache(ctx)
}

// Close 关闭底层设备与 pebble。失败时合并返回首个错误。
func (s *Storage) Close() error {
	var err error
	if s.dev != nil {
		err = s.dev.Close()
	}
	if s.db != nil {
		if e := s.db.Close(); err == nil {
			err = e
		}
	}
	return err
}

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
		return 0, 0, ErrInvalidRange
	}
	if size > s.layout.SegmentSizeBytes {
		return 0, 0, ErrTooLarge
	}
	return s.db.AllocateSegment(size)
}

// PutAppend 直写一段数据到段内 off 处。data 首地址 4K 对齐时零拷贝直写设备
// （O_DIRECT 直写调用方缓冲），非对齐/尾段由 device.Append 内部对齐缓冲兜底。
// 分段调用方须保证 off 递增（off + 已写字节数）且各段相邻。
func (s *Storage) PutAppend(ctx context.Context, segmentID, off, size int64, data []byte) error {
	if int64(len(data)) < size {
		return ErrShortWrite
	}
	return s.dev.Append(ctx, segmentID, off, size, data)
}

// PutCommit 建立 key→(segmentID, off, size) 映射。顺序保证：先写设备数据，再写元数据，
// 避免出现「有映射无数据」。
func (s *Storage) PutCommit(ctx context.Context, key string, segmentID, off, size int64) error {
	meta := ObjectMeta{SegmentID: segmentID, Offset: off, Size: size}
	return s.db.PutMapping(ctx, key, meta)
}

// ReadAt 读取对象内 [off, off+size) 区间的数据并返回（返回值为从 bufpool 取出的池化
// 缓冲或 nil；调用方用毕必须 bufpool.Put(返回值) 归还，否则造成池泄漏）。
//
// 对外不要求 off/size 对齐（Storage 层吸收 O_DIRECT 的 4K 对齐细节）：
// 将物理读向下/向上对齐到 4K，再取回请求窗口。对齐区间（skip==0 且 size 为 4K 倍数）
// 零拷贝直读，非对齐区间在池化缓冲内原址平移一次。
//
// 错误与边界：
//   - off < 0 或 off > Size：ErrInvalidRange；
//   - 请求超出对象结尾：截断到剩余字节，返回的部分不足 size 时附 io.EOF；
//   - off == Size（剩余 0）：返回 (nil, io.EOF)。
func (s *Storage) ReadAt(ctx context.Context, key string, off, size int64) ([]byte, error) {
	meta, err := s.db.GetMapping(ctx, key)
	if err != nil {
		return nil, err
	}
	if off < 0 || off > meta.Size {
		return nil, ErrInvalidRange
	}
	remaining := meta.Size - off
	want := size
	if want > remaining {
		want = remaining
	}
	if want == 0 {
		return nil, io.EOF
	}

	// 段内物理读区间 [dstart, dstart+dlen)，向下/向上 4K 对齐。
	relStart := meta.Offset + off
	dstart := relStart &^ (layout.BlockSize - 1)
	dlen := layout.Align4k(relStart+want) - dstart
	skip := relStart - dstart

	// 读引用计数：防 GC 在读在途时回收并复用该段（迟到读读错数据）。
	s.db.RefSegment(meta.SegmentID)
	defer s.db.UnrefSegment(meta.SegmentID)

	data, err := s.dev.ReadAt(ctx, meta.SegmentID, dstart, dlen)
	if err != nil {
		return nil, err
	}
	if n := int64(len(data)); n < want {
		// 设备不足（对象末尾）：返回已读前缀，调用方按 io.EOF 收尾。
		want = n
	}
	if skip > 0 || want < int64(len(data)) {
		// 非对齐窗口：池化缓冲内原址左移，只保留 [skip, skip+want)。
		n := copy(data, data[skip:skip+want])
		data = data[:n]
	}
	if int64(len(data)) < size {
		return data, io.EOF
	}
	return data, nil
}

// ReadAtInto 读取对象内 [off, off+size) 区间的数据，直接 DMA 进调用方 dst
// （服务端 shm 直读共享内存用：免 bufpool→共享内存 memcpy）。
//
// 要求：
//   - off 4K 对齐（O_DIRECT 直读快路径，skip==0；非对齐 off 由调用方回退 ReadAt+拷贝）；
//   - dst 首地址 4K 对齐（bufAligned）且 cap ≥ align4K(want)（物理读区间含尾部对齐余量）。
//
// 返回实际读入的请求窗口字节数（读到对象末尾不足 size 时截断；remaining==0 返回
// (0, io.EOF)）。dst 中 [0, 返回 n) 为有效数据。
func (s *Storage) ReadAtInto(ctx context.Context, key string, off, size int64, dst []byte) (int64, error) {
	meta, err := s.db.GetMapping(ctx, key)
	if err != nil {
		return 0, err
	}
	if off < 0 || off > meta.Size || off%layout.BlockSize != 0 {
		return 0, ErrInvalidRange
	}
	remaining := meta.Size - off
	want := size
	if want > remaining {
		want = remaining
	}
	if want == 0 {
		return 0, io.EOF
	}

	// 段内物理读区间：off 4K 对齐 + meta.Offset 4K 对齐（写入保证）→ skip==0，
	// dstart = relStart 4K 对齐，dlen 向上对齐到 4K。
	relStart := meta.Offset + off
	dlen := layout.Align4k(relStart+want) - relStart
	if int64(len(dst)) < dlen {
		return 0, fmt.Errorf("taihu: readinto dst %d < dlen %d", len(dst), dlen)
	}

	// 读引用计数：防 GC 在读在途时回收并复用该段（迟到读读错数据）。
	s.db.RefSegment(meta.SegmentID)
	defer s.db.UnrefSegment(meta.SegmentID)

	n, err := s.dev.ReadAtInto(ctx, meta.SegmentID, relStart, dlen, dst[:dlen])
	if err != nil {
		return 0, err
	}
	if n < want {
		// 设备不足（对象末尾）：返回已读前缀，调用方按 EOF 收尾。
		want = n
	}
	if want < size {
		return want, io.EOF
	}
	return want, nil
}

// BatchReadBlock 批读的一个对象块（直读快路径前置：Off/Size 4K 对齐，Size 为整块）。
type BatchReadBlock struct {
	Key  string
	Off  int64
	Size int64
	Dst  []byte // 4K 对齐，cap ≥ align4K(Size)（请求窗口 + 尾部对齐余量）
}

// BatchedReadResult 单块批读结果。
type BatchedReadResult struct {
	N   int64 // 请求窗口读入字节数（读到对象末尾不足 Size 时截断）
	Err error // io.EOF 表示读到对象/设备末尾短读；其余为映射/设备错误
}

// BatchRead 一次性批读多个对象块：把各块解析为设备直读 job 后，交给 device 以一次
// io_submit 批量提交（摊薄系统调用），完成后再返回各块结果。逐块语义与 ReadAtInto
// 完全等价（cost 短读截断 + io.EOF）。供服务端"多 stream 一 worker 聚合读"使用。
func (s *Storage) BatchRead(ctx context.Context, blocks []BatchReadBlock) ([]BatchedReadResult, error) {
	res := make([]BatchedReadResult, len(blocks))
	if len(blocks) == 0 {
		return res, nil
	}
	// Phase 1：逐块解析对象映射 → 段内物理区间，构建 device.ReadJob；持段读引用。
	devJobs := make([]device.ReadJob, len(blocks))
	refed := make([]bool, len(blocks))
	wants := make([]int64, len(blocks))
	for i := range blocks {
		b := &blocks[i]
		meta, err := s.db.GetMapping(ctx, b.Key)
		if err != nil {
			return res, err
		}
		if b.Off < 0 || b.Off > meta.Size || b.Off%layout.BlockSize != 0 {
			return res, ErrInvalidRange
		}
		remaining := meta.Size - b.Off
		want := b.Size
		if want > remaining {
			want = remaining
		}
		if want == 0 {
			res[i] = BatchedReadResult{N: 0, Err: io.EOF}
			continue
		}
		relStart := meta.Offset + b.Off
		dlen := layout.Align4k(relStart+want) - relStart
		if int64(len(b.Dst)) < dlen {
			return res, fmt.Errorf("taihu: batchread dst %d < dlen %d", len(b.Dst), dlen)
		}
		s.db.RefSegment(meta.SegmentID)
		refed[i] = true
		devJobs[i] = device.ReadJob{SegmentID: meta.SegmentID, Off: relStart, Buf: b.Dst[:dlen], Size: dlen}
		wants[i] = want
	}
	// 整批都无有效 job 时直接返回（已逐块置 EOF）。
	any := false
	for i := range devJobs {
		if refed[i] {
			any = true
			break
		}
	}
	// Phase 2：一次设备批提交。
	if any {
		if ns, err := s.dev.ReadAtIntoBatch(ctx, devJobs); err != nil {
			for i := range blocks {
				if refed[i] {
					s.db.UnrefSegment(devJobs[i].SegmentID)
				}
			}
			return res, err
		} else {
			for i := range blocks {
				if refed[i] {
					n := ns[i]
					want := wants[i]
					if n < want {
						want = n
					}
					if want < blocks[i].Size {
						res[i] = BatchedReadResult{N: want, Err: io.EOF}
					} else {
						res[i] = BatchedReadResult{N: want}
					}
				}
			}
		}
	}
	// Phase 3：释放所有段读引用。
	for i := range blocks {
		if refed[i] {
			s.db.UnrefSegment(devJobs[i].SegmentID)
		}
	}
	return res, nil
}

// Delete 删除对象的持久化映射。缓存失效由 store 内部处理。物理空间回收留待 segment 级 GC。
// key 不存在时返回 ErrNotFound。
func (s *Storage) Delete(ctx context.Context, key string) error {
	if _, err := s.db.GetMapping(ctx, key); err != nil {
		return err
	}
	return s.db.DeleteMapping(ctx, key)
}

// Stat 返回对象逻辑大小。远程层（taihu-server）Get size=-1 全量读等场景使用。
// key 不存在时返回 ErrNotFound。
func (s *Storage) Stat(ctx context.Context, key string) (int64, error) {
	meta, err := s.db.GetMapping(ctx, key)
	if err != nil {
		return 0, err
	}
	return meta.Size, nil
}

// IOStats 返回底层设备磁盘 IO 尺寸统计（4MiB 整块 vs 其他）。压测/验证用。
func (s *Storage) IOStats() (io4M, ioOther, bytes4M, bytesOther int64) {
	return s.dev.Stats()
}

// GetDiskCapacity 返回整盘容量/可用/已用字节（集群注册与心跳上报用）。
// Capacity 为布局总容量（segmentSize×segmentCount，由启动时读取的真实 nvme 容量计算），
// Used 为已写物理字节（metastore 按段状态统计），Available = Capacity - Used。
func (s *Storage) GetDiskCapacity() (capacity, available, used int64, err error) {
	capacity = s.layout.SegmentSizeBytes * s.layout.SegmentCount
	used = s.db.UsedBytes()
	available = capacity - used
	return capacity, available, used, nil
}

// SegmentStats 返回各状态 segment 数量（GC/回收/复用验证用）。
func (s *Storage) SegmentStats() map[metastore.SegmentState]int {
	return s.db.SegmentStats()
}
