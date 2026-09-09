package taihu

import (
	"context"
	"fmt"

	"github.com/cockroachdb/pebble"
)

// pebbleStore 基于 CockroachDB pebble（纯 Go LSM）实现 store。
//
// pebble 为单 keyspace、无列族；此处用 key 前缀隔离两个逻辑命名空间：
//   - kvPrefixMapping：mapping（用户 key → ObjectMeta）
//   - kvPrefixState：state（cursor / seg/<id>）
//
// 写入默认 Sync，保证「先写设备数据 → 写 mapping → 更新 cursor」的持久化顺序，崩溃后游标不回退。

// 两个逻辑命名空间的前缀。用户 key 会被整体映射到 mapping 前缀之下，
// 因此即便用户 key 恰好以 state 前缀开头也不会与内部键冲突。
const (
	kvPrefixMapping = "m\x00"
	kvPrefixState   = "s\x00"
)

var _ store = (*pebbleStore)(nil)

type pebbleStore struct {
	db *pebble.DB
}

// openPebbleStore 打开 pebble DB。目录不存在时自动创建。
func openPebbleStore(dir string) (*pebbleStore, error) {
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		return nil, fmt.Errorf("taihu: open pebble %q: %w", dir, err)
	}
	return &pebbleStore{db: db}, nil
}

// syncWO 用于需要落盘持久化的写入（mapping / cursor）。
var syncWO = &pebble.WriteOptions{Sync: true}

func keyMapping(key string) []byte {
	b := make([]byte, 0, len(kvPrefixMapping)+len(key))
	b = append(b, kvPrefixMapping...)
	b = append(b, key...)
	return b
}

func keyState(inner []byte) []byte {
	b := make([]byte, 0, len(kvPrefixState)+len(inner))
	b = append(b, kvPrefixState...)
	b = append(b, inner...)
	return b
}

// get 通用读：返回值字节、是否存在。
func (s *pebbleStore) get(key []byte) ([]byte, bool, error) {
	v, closer, err := s.db.Get(key)
	if err != nil {
		if err == pebble.ErrNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	data := make([]byte, len(v))
	copy(data, v)
	_ = closer.Close()
	return data, true, nil
}

func (s *pebbleStore) GetMapping(ctx context.Context, key string) (ObjectMeta, error) {
	v, found, err := s.get(keyMapping(key))
	if err != nil {
		return ObjectMeta{}, err
	}
	if !found {
		return ObjectMeta{}, ErrNotFound
	}
	return decodeObjectMeta(v)
}

func (s *pebbleStore) PutMapping(ctx context.Context, key string, m ObjectMeta) error {
	return s.db.Set(keyMapping(key), m.encode(), syncWO)
}

func (s *pebbleStore) DeleteMapping(ctx context.Context, key string) error {
	return s.db.Delete(keyMapping(key), syncWO)
}

// IterMapping 以 mapping 前缀区间顺序遍历全部 key → ObjectMeta。
func (s *pebbleStore) IterMapping(ctx context.Context, fn func(key string, m ObjectMeta) error) error {
	prefix := []byte(kvPrefixMapping) // "m\x00"
	upper := append([]byte{}, prefix...)
	upper[len(upper)-1]++ // 前缀末字节 +1 → 独占上界，覆盖所有以 prefix 开头的键

	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upper})
	if err != nil {
		return err
	}
	defer it.Close()

	for it.First(); it.Valid(); it.Next() {
		m, err := decodeObjectMeta(it.Value())
		if err != nil {
			return err
		}
		if err := fn(string(it.Key()[len(prefix):]), m); err != nil {
			return err
		}
	}
	return it.Error()
}

func (s *pebbleStore) GetCursor(ctx context.Context) (WriteCursor, bool, error) {
	v, found, err := s.get(keyState([]byte(kvCursorKey)))
	if err != nil || !found {
		return WriteCursor{}, found, err
	}
	c, err := decodeWriteCursor(v)
	return c, true, err
}

func (s *pebbleStore) PutCursor(ctx context.Context, c WriteCursor) error {
	return s.db.Set(keyState([]byte(kvCursorKey)), c.encode(), syncWO)
}

func (s *pebbleStore) GetSegment(ctx context.Context, segmentID int64) (SegmentMeta, bool, error) {
	v, found, err := s.get(keyState(segmentKey(segmentID)))
	if err != nil || !found {
		return SegmentMeta{}, found, err
	}
	m, err := decodeSegmentMeta(v)
	return m, true, err
}

func (s *pebbleStore) PutSegment(ctx context.Context, segmentID int64, m SegmentMeta) error {
	return s.db.Set(keyState(segmentKey(segmentID)), m.encode(), syncWO)
}

// Close 关闭 pebble DB。
func (s *pebbleStore) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}