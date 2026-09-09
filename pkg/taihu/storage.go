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

// Put 写入对象。size 由调用方提供（逻辑长度），不必预读 in 求得。
// 先从 db 原子申请写位置（段满自动滚动），再写设备数据，最后写映射。
// 设备写位于分配锁之外，并发 Put 可写不同偏移。
func (s *Storage) Put(ctx context.Context, key string, size int64, in io.Reader) error {
	if size < 0 {
		return ErrInvalidRange
	}
	if size > layout.SegmentSizeBytes {
		return ErrTooLarge
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

// Get 读取对象内 [off, off+size) 子区间。元数据由 store 内部缓存加速，命中免查 pebble。
// 越界（off<0、size<0、off>Size、off+size>Size）返回 ErrInvalidRange。
//
// 对外不要求 off/size 对齐（Storage 层吸收 O_DIRECT 的 4K 对齐细节）：
// 将物理读向下/向上对齐到 4K，仅返回请求的 [off, off+size) 区间；
// off、size 恰为 4K 对齐时对齐段与请求段重合，零额外读取开销。
func (s *Storage) Get(ctx context.Context, key string, off, size int64) (io.ReadCloser, error) {
	meta, err := s.db.GetMapping(ctx, key)
	if err != nil {
		return nil, err
	}

	if off < 0 || size < 0 || off > meta.Size || off+size > meta.Size {
		return nil, ErrInvalidRange
	}

	// 段内请求区间 [relStart, relStart+size)，向下/向上 4K 对齐出物理读区间。
	relStart := meta.Offset + off
	dstart := relStart &^ (layout.BlockSize - 1)
	dlen := layout.Align4k(relStart+size) - dstart

	r, err := s.dev.Read(ctx, meta.SegmentID, dstart, dlen)
	if err != nil {
		return nil, err
	}
	if skip := relStart - dstart; skip > 0 {
		if _, err := io.CopyN(io.Discard, r, skip); err != nil {
			_ = r.Close() // 归还池化缓冲
			return nil, err
		}
	}
	// 包装 LimitReader 同时透传 Close：Linux 下 Close 归还池化对齐缓冲，调用方必须 Close。
	return &limitReadCloser{Reader: io.LimitReader(r, size), c: r}, nil
}

// limitReadCloser 将 LimitReader 包装为 ReadCloser，Close 透传给内部实现（归还池缓冲）。
type limitReadCloser struct {
	io.Reader
	c io.Closer
}

func (l *limitReadCloser) Close() error { return l.c.Close() }

// Delete 删除对象的持久化映射。缓存失效由 store 内部处理。物理空间回收留待 segment 级 GC。
// key 不存在时返回 ErrNotFound。
func (s *Storage) Delete(ctx context.Context, key string) error {
	if _, err := s.db.GetMapping(ctx, key); err != nil {
		return err
	}
	return s.db.DeleteMapping(ctx, key)
}
