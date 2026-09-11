package taihu

import (
	"context"
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
	dir string // pebble 元数据目录（容量上报 statfs 用）
}

// NewStorage 构建 Storage：打开 pebble、打开裸设备。写游标由 db 首次分配时懒加载恢复。
// 返回 (*Storage, error)，与 v1 文档略有出入，便于暴露初始化错误。
func NewStorage(ctx context.Context, rocksdbDir, nvmePath string) (*Storage, error) {
	db, err := metastore.Open(rocksdbDir)
	if err != nil {
		return nil, err
	}
	dev, err := device.NewDevice(ctx, nvmePath)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	s := &Storage{
		db:  db,
		dev: dev,
		dir: rocksdbDir,
	}
	return s, nil
}

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
func (s *Storage) Put(ctx context.Context, key string, size int64, in []byte) error {
	if size < 0 {
		return ErrInvalidRange
	}
	if size > layout.SegmentSizeBytes {
		return ErrTooLarge
	}
	if int64(len(in)) < size {
		return ErrShortWrite
	}

	seg, off, err := s.db.AllocateSegment(size)
	if err != nil {
		return err
	}
	if err := s.dev.Append(ctx, seg, off, size, in); err != nil {
		return err
	}

	// 顺序保证：先写设备数据，再写元数据，避免出现「有映射无数据」。
	meta := ObjectMeta{SegmentID: seg, Offset: off, Size: size}
	if err := s.db.PutMapping(ctx, key, meta); err != nil {
		return err
	}
	return nil
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

// GetDiskCapacity 返回 pebble 元数据目录所在文件系统的容量/可用/已用字节
// （集群注册与心跳上报用，statfs 取 Bavail）。失败时返回 (0,0,0,err)。
func (s *Storage) GetDiskCapacity() (capacity, available, used int64, err error) {
	return diskCapacity(s.dir)
}

// SegmentStats 返回各状态 segment 数量（GC/回收/复用验证用）。
func (s *Storage) SegmentStats() map[metastore.SegmentState]int {
	return s.db.SegmentStats()
}
