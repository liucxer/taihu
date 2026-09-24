package metastore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/cockroachdb/pebble"

	"github.com/liucxer/taihu/pkg/ierr"
)

// 底层存储（CockroachDB Pebble）操作的对象化封装。
//
// pebble 为单 keyspace、无列族，本包用 key 前缀隔离两个逻辑命名空间：
//   - mapping 命名空间（m\x00...）：用户 key → ObjectMeta
//   - state 命名空间（s\x00...）：cursor 与 seg/<id> 段状态
//
// 本文件把散落各处的「前缀 + 裸 key 拼接 + db.Set/Get/NewIter」写法收敛为三个
// 私有 model：mapping / segment / cursor 每类记录一个结构体，各自封装键编解码
// 与所需的 set / get / delete（含批量变体）与迭代方法，呈面向对象形态。
// 事务（WriteBatch）与 CAS 提交的编排仍在顶层（pebbleStore / segmentManager /
// allocator）负责，不在此下沉。
//
// 磁盘键格式与 v1 完全兼容（旧数据可继续读），仅操作形态从散落函数收敛为对象。

// 两个逻辑命名空间的前缀。用户 key 会被整体映射到 mapping 前缀之下，
// 因此即便用户 key 恰好以 state 前缀开头也不会与内部键冲突。
const (
	kvPrefixMapping = "m\x00"
	kvPrefixState   = "s\x00"
)

// state 命名空间中的内部关键字。
const (
	kvCursorKey     = "cursor"
	kvSegmentPrefix = "seg/"
)

// keyMapping 编码 mapping 键：m\x00 + 用户 key（model 内部与测试共用）。
func keyMapping(key string) []byte {
	b := make([]byte, 0, len(kvPrefixMapping)+len(key))
	b = append(b, kvPrefixMapping...)
	b = append(b, key...)
	return b
}

// keyState 编码 state 键：s\x00 + 内部关键字。
func keyState(inner []byte) []byte {
	b := make([]byte, 0, len(kvPrefixState)+len(inner))
	b = append(b, kvPrefixState...)
	b = append(b, inner...)
	return b
}

// segmentKey 编码段号关键字：seg/ + 小端 8 字节段号（state 命名空间内部关键字）。
func segmentKey(segmentID int64) []byte {
	b := make([]byte, 0, len(kvSegmentPrefix)+8)
	b = append(b, kvSegmentPrefix...)
	b = binary.LittleEndian.AppendUint64(b, uint64(segmentID))
	return b
}

// appendUpper 返回前缀的独占上界（末字节 +1），用于迭代区间 [prefix, upper)。
func appendUpper(prefix []byte) []byte {
	u := append([]byte{}, prefix...)
	u[len(u)-1]++
	return u
}

// mappingModel mapping 命名空间（用户 key → ObjectMeta）的操作对象。
type mappingModel struct {
	db *pebble.DB
}

// key 编码：m\x00 + 用户 key。
func (m *mappingModel) key(userKey string) []byte { return keyMapping(userKey) }

// get 读单条映射：存在返回 (meta, true, nil)；不存在返回 found=false；值损坏返回解码错误。
func (m *mappingModel) get(userKey string) (ObjectMeta, bool, error) {
	v, closer, err := m.db.Get(keyMapping(userKey))
	if err != nil {
		if err == pebble.ErrNotFound {
			return ObjectMeta{}, false, nil
		}
		return ObjectMeta{}, false, err
	}
	defer closer.Close()
	meta, err := decodeObjectMeta(v)
	return meta, true, err
}

// set 覆盖写单条映射（同步落盘）。
func (m *mappingModel) set(userKey string, meta ObjectMeta) error {
	return m.db.Set(keyMapping(userKey), meta.encode(), syncWO)
}

// setBatch 将单条映射写入 batch（原子提交由调用方负责）。
func (m *mappingModel) setBatch(b *pebble.Batch, userKey string, meta ObjectMeta) {
	b.Set(keyMapping(userKey), meta.encode(), nil)
}

// delBatch 将单条映射删除写入 batch。
func (m *mappingModel) delBatch(b *pebble.Batch, userKey string) {
	b.Delete(keyMapping(userKey), nil)
}

// iter 以映射前缀区间顺序遍历全部 key → ObjectMeta，回调错误原样返回。
func (m *mappingModel) iter(fn func(key string, meta ObjectMeta) error) error {
	prefix := []byte(kvPrefixMapping)
	it, err := m.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: appendUpper(prefix)})
	if err != nil {
		return err
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		meta, err := decodeObjectMeta(it.Value())
		if err != nil {
			return err
		}
		if err := fn(string(it.Key()[len(prefix):]), meta); err != nil {
			return err
		}
	}
	return it.Error()
}

// getMany 有序批量读：keys 排序后单迭代器逐个 SeekGE（摊薄 N 次独立 Get）。
// 返回 key → ObjectMeta；任一 key 缺失整体返回 ierr.ErrNotFound（与逐条 get 语义一致）。
func (m *mappingModel) getMany(keys []string) (map[string]ObjectMeta, error) {
	if len(keys) == 0 {
		return map[string]ObjectMeta{}, nil
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	prefix := []byte(kvPrefixMapping)
	it, err := m.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: appendUpper(prefix)})
	if err != nil {
		return nil, err
	}
	defer it.Close()
	out := make(map[string]ObjectMeta, len(sorted))
	for _, k := range sorted {
		it.SeekGE(keyMapping(k))
		if !it.Valid() || !bytes.Equal(it.Key()[len(prefix):], []byte(k)) {
			return nil, ierr.ErrNotFound
		}
		meta, err := decodeObjectMeta(it.Value())
		if err != nil {
			return nil, err
		}
		out[k] = meta
	}
	return out, nil
}

// segmentModel state 命名空间中段状态（seg/<id>）的操作对象。
type segmentModel struct {
	db *pebble.DB
}

// key 编码：s\x00seg/ + 小端 8 字节段号。
func (m *segmentModel) key(segmentID int64) []byte { return keyState(segmentKey(segmentID)) }

// get 读单条段状态：存在返回 (meta, true, nil)；不存在返回 found=false。
func (m *segmentModel) get(segmentID int64) (SegmentMeta, bool, error) {
	v, closer, err := m.db.Get(keyState(segmentKey(segmentID)))
	if err != nil {
		if err == pebble.ErrNotFound {
			return SegmentMeta{}, false, nil
		}
		return SegmentMeta{}, false, err
	}
	defer closer.Close()
	meta, err := decodeSegmentMeta(v)
	return meta, true, err
}

// set 覆盖写单条段状态（同步落盘）。
func (m *segmentModel) set(segmentID int64, meta SegmentMeta) error {
	return m.db.Set(keyState(segmentKey(segmentID)), meta.encode(), syncWO)
}

// setBatch 将单条段状态写入 batch（原子提交由调用方负责）。
func (m *segmentModel) setBatch(b *pebble.Batch, segmentID int64, meta SegmentMeta) {
	b.Set(keyState(segmentKey(segmentID)), meta.encode(), nil)
}

// iter 顺序遍历全部段状态记录（s\x00seg/ 区间），回调错误原样返回。
// 段号从小端 8 字节解析；键长非法或值损坏返回错误。
func (m *segmentModel) iter(fn func(segmentID int64, meta SegmentMeta) error) error {
	prefix := keyState([]byte(kvSegmentPrefix))
	it, err := m.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: appendUpper(prefix)})
	if err != nil {
		return err
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		if len(it.Key()) != len(prefix)+8 {
			return fmt.Errorf("taihu: bad segment key %q", it.Key())
		}
		id := int64(binary.LittleEndian.Uint64(it.Key()[len(prefix):]))
		meta, err := decodeSegmentMeta(it.Value())
		if err != nil {
			return err
		}
		if err := fn(id, meta); err != nil {
			return err
		}
	}
	return it.Error()
}

// cursorModel state 命名空间中顺序写游标（cursor）的操作对象，全局唯一。
type cursorModel struct {
	db *pebble.DB
}

// key 编码：s\x00cursor。
func (c *cursorModel) key() []byte { return keyState([]byte(kvCursorKey)) }

// set 持久化游标（同步落盘）。
func (c *cursorModel) set(pos writeCursor) error {
	return c.db.Set(c.key(), pos.encode(), syncWO)
}

// get 读游标：存在返回 (pos, true, nil)；不存在（首次使用）返回 found=false。
func (c *cursorModel) get() (writeCursor, bool, error) {
	v, closer, err := c.db.Get(c.key())
	if err != nil {
		if err == pebble.ErrNotFound {
			return writeCursor{}, false, nil
		}
		return writeCursor{}, false, err
	}
	defer closer.Close()
	pos, err := decodeWriteCursor(v)
	return pos, true, err
}
