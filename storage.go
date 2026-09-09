package taihu

import (
	"context"
	"io"
	"sync"
)

// Storage 对外 object 存储。数据写底层裸设备，key→位置映射存 RocksDB，内存有界 LRU 缓存。
type Storage struct {
	db    store
	dev   *Device
	cache *metaCache

	// mu 串行化 Put 的「分配游标 + 写设备 + 写元数据」临界区，保证游标单调、互不覆盖。
	mu     sync.Mutex
	curSeg int64
	curOff int64
}

// NewStorage 构建 Storage：打开 pebble、打开裸设备、恢复写游标。
// 返回 (*Storage, error)，与 v1 文档略有出入，便于暴露初始化错误。
func NewStorage(ctx context.Context, rocksdbDir, nvmePath string) (*Storage, error) {
	db, err := openPebbleStore(rocksdbDir)
	if err != nil {
		return nil, err
	}
	dev, err := NewDevice(ctx, nvmePath)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	s := &Storage{
		db:    db,
		dev:   dev,
		cache: &metaCache{},
	}

	cur, found, err := db.GetCursor(ctx)
	if err != nil {
		_ = dev.Close()
		_ = db.Close()
		return nil, err
	}
	if found {
		s.curSeg, s.curOff = cur.SegmentID, cur.Offset
	} else {
		if err := db.PutCursor(ctx, WriteCursor{}); err != nil {
			_ = dev.Close()
			_ = db.Close()
			return nil, err
		}
	}
	return s, nil
}

// LoadCache 预热元数据缓存：全量扫描 mapping 命名空间，把 key→ObjectMeta 载入内存缓存。
// 受同一 LRU 预算约束（超 1GB 自动逐出）。用于 bench / 已知 key 集合场景，
// 消除 Get 的元数据未命中回查，从而测纯设备读写带宽。
func (s *Storage) LoadCache(ctx context.Context) error {
	return s.db.IterMapping(ctx, func(key string, m ObjectMeta) error {
		s.cache.put(key, m)
		return nil
	})
}

// Close 关闭底层设备与 RocksDB。失败时合并返回首个错误。
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
// 顺序追加到当前 segment，写满自动切下一 segment。末尾自动补齐到 4K。
func (s *Storage) Put(ctx context.Context, key string, size int64, in io.Reader) error {
	if size < 0 {
		return ErrInvalidRange
	}
	if size > SegmentSizeBytes {
		return ErrTooLarge
	}
	aligned := align4k(size)

	s.mu.Lock()
	defer s.mu.Unlock()

	// 当前段放不下则切新段（前提：对象不大于段）。提前持久化新游标，避免崩溃回退到满段覆盖。
	if s.curOff+aligned > SegmentSizeBytes {
		s.curSeg++
		if s.curSeg >= SegmentCount {
			return ErrNoSpace
		}
		s.curOff = 0
		if err := s.db.PutCursor(ctx, WriteCursor{SegmentID: s.curSeg, Offset: 0}); err != nil {
			return err
		}
	}

	seg, off := s.curSeg, s.curOff
	if err := s.dev.append(ctx, seg, off, size, in); err != nil {
		return err
	}

	// 顺序保证：先写设备数据，再写元数据，避免出现「有映射无数据」。
	meta := ObjectMeta{SegmentID: seg, Offset: off, Size: size}
	if err := s.db.PutMapping(ctx, key, meta); err != nil {
		return err
	}

	s.curOff = off + aligned
	if err := s.db.PutCursor(ctx, WriteCursor{SegmentID: seg, Offset: s.curOff}); err != nil {
		return err
	}
	s.cache.put(key, meta)
	return nil
}

// Get 读取对象内 [off, off+size) 子区间。命中缓存则免查 pebble。
//
// 对外不要求 off/size 对齐（Storage 层吸收 O_DIRECT 的 4K 对齐细节）：
// 将物理读向下/向上对齐到 4K，仅返回请求的 [off, off+size) 区间；
// off、size 恰为 4K 对齐时对齐段与请求段重合，零额外读取开销。
func (s *Storage) Get(ctx context.Context, key string, off, size int64) (io.ReadCloser, error) {
	meta, ok := s.cache.get(key)
	if !ok {
		var err error
		meta, err = s.db.GetMapping(ctx, key)
		if err != nil {
			return nil, err
		}
		s.cache.put(key, meta)
	}

	if off < 0 || size < 0 || off > meta.Size {
		return nil, ErrInvalidRange
	}
	if size > meta.Size-off {
		size = meta.Size - off
	}

	// 段内请求区间 [relStart, relStart+size)，向下/向上 4K 对齐出物理读区间。
	relStart := meta.Offset + off
	dstart := relStart &^ (BlockSize - 1)
	dlen := align4k(relStart + size) - dstart

	r, err := s.dev.read(ctx, meta.SegmentID, dstart, dlen)
	if err != nil {
		return nil, err
	}
	if skip := relStart - dstart; skip > 0 {
		if _, err := io.CopyN(io.Discard, r, skip); err != nil {
			return nil, err
		}
	}
	return io.NopCloser(io.LimitReader(r, size)), nil
}

// Delete 删除对象的持久化映射与缓存。物理空间回收留待 segment 级 GC。
// key 不存在时返回 ErrNotFound。
func (s *Storage) Delete(ctx context.Context, key string) error {
	if _, err := s.db.GetMapping(ctx, key); err != nil {
		return err
	}
	if err := s.db.DeleteMapping(ctx, key); err != nil {
		return err
	}
	s.cache.del(key)
	return nil
}
