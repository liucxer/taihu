package cluster

import (
	"context"
	"testing"

	tikverr "github.com/tikv/client-go/v2/error"
	"github.com/tikv/client-go/v2/txnkv"
)

// 本文件用假实现驱动 TiKVKV 的全部代码路径（单测不连真实 TiKV/PD）：
// 生产侧通过 kv_tikv.go 的 tikvClient/tikvSnapshot/tikvTxn/tikvIterator 最小接口缝隙注入。

// fakeIter 内存快照迭代器，可注入 Next 错误。
type fakeIter struct {
	keys    [][]byte
	vals    [][]byte
	i       int
	nextErr error
	closed  bool
}

func (it *fakeIter) Valid() bool   { return it.i < len(it.keys) }
func (it *fakeIter) Key() []byte   { return it.keys[it.i] }
func (it *fakeIter) Value() []byte { return it.vals[it.i] }
func (it *fakeIter) Close()        { it.closed = true }

func (it *fakeIter) Next() error {
	if it.nextErr != nil {
		return it.nextErr
	}
	it.i++
	return nil
}

// fakeSnapshot 假快照：Iter 按调用顺序弹出预置迭代器（用尽后返回空迭代器）。
type fakeSnapshot struct {
	getVal   []byte
	getErr   error
	batchRes map[string][]byte
	batchErr error
	iters    []*fakeIter
	iterErr  error

	isoLevel  txnkv.IsoLevel
	iterCalls int
}

func (s *fakeSnapshot) Get(context.Context, []byte) ([]byte, error) {
	return s.getVal, s.getErr
}

func (s *fakeSnapshot) BatchGet(context.Context, [][]byte) (map[string][]byte, error) {
	return s.batchRes, s.batchErr
}

func (s *fakeSnapshot) SetIsolationLevel(level txnkv.IsoLevel) { s.isoLevel = level }

func (s *fakeSnapshot) Iter([]byte, []byte) (tikvIterator, error) {
	if s.iterErr != nil {
		return nil, s.iterErr
	}
	i := s.iterCalls
	s.iterCalls++
	if i < len(s.iters) {
		return s.iters[i], nil
	}
	return &fakeIter{}, nil
}

// fakeTxn 假事务：记录 Set/Delete 并支持各步注入错误。
type fakeTxn struct {
	sets      map[string][]byte
	dels      [][]byte
	setErr    error
	delErr    error
	commitErr error
}

func (t *fakeTxn) Set(key, value []byte) error {
	if t.setErr != nil {
		return t.setErr
	}
	if t.sets == nil {
		t.sets = make(map[string][]byte)
	}
	t.sets[string(key)] = append([]byte(nil), value...)
	return nil
}

func (t *fakeTxn) Delete(key []byte) error {
	if t.delErr != nil {
		return t.delErr
	}
	t.dels = append(t.dels, append([]byte(nil), key...))
	return nil
}

func (t *fakeTxn) Commit(context.Context) error { return t.commitErr }

// fakeClient 假 TiKV 客户端。
type fakeClient struct {
	ts       uint64
	tsErr    error
	snap     *fakeSnapshot
	txn      *fakeTxn
	beginErr error
	closeErr error
	closed   bool
}

func (c *fakeClient) GetTimestamp(context.Context) (uint64, error) { return c.ts, c.tsErr }

func (c *fakeClient) GetSnapshot(uint64) tikvSnapshot { return c.snap }

func (c *fakeClient) Begin() (tikvTxn, error) {
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	return c.txn, nil
}

func (c *fakeClient) Close() error {
	c.closed = true
	return c.closeErr
}

// newFakeKV 组装带假客户端的 TiKVKV（测试专用）。
func newFakeKV(c *fakeClient) *TiKVKV { return &TiKVKV{c: c} }

// TestTLSConfigEnabled：三者齐全才启用 TLS。
func TestTLSConfigEnabled(t *testing.T) {
	if (TLSConfig{}).enabled() {
		t.Fatal("empty TLSConfig should be disabled")
	}
	if (TLSConfig{CA: "ca", Cert: "cert"}).enabled() {
		t.Fatal("partial TLSConfig should be disabled")
	}
	if !(TLSConfig{CA: "ca", Cert: "cert", Key: "key"}).enabled() {
		t.Fatal("complete TLSConfig should be enabled")
	}
}

// TestTiKVKVGet：TSO 失败、key 不存在、读取失败与命中，并校验快照隔离级别为 RC。
func TestTiKVKVGet(t *testing.T) {
	ctx := context.Background()

	kv := newFakeKV(&fakeClient{tsErr: errInjected})
	if _, err := kv.Get(ctx, []byte("k")); err == nil {
		t.Fatal("Get with TSO error should fail")
	}

	snap := &fakeSnapshot{getErr: tikverr.ErrNotExist}
	kv = newFakeKV(&fakeClient{ts: 7, snap: snap})
	v, err := kv.Get(ctx, []byte("k"))
	if err != nil || v != nil {
		t.Fatalf("Get missing = (%q,%v), want (nil,nil)", v, err)
	}
	if snap.isoLevel != txnkv.RC {
		t.Fatalf("isolation level = %v, want RC", snap.isoLevel)
	}

	kv = newFakeKV(&fakeClient{snap: &fakeSnapshot{getErr: errInjected}})
	if _, err := kv.Get(ctx, []byte("k")); err != errInjected {
		t.Fatalf("Get err = %v, want injected", err)
	}

	kv = newFakeKV(&fakeClient{snap: &fakeSnapshot{getVal: []byte("v")}})
	if v, err := kv.Get(ctx, []byte("k")); err != nil || string(v) != "v" {
		t.Fatalf("Get = (%q,%v), want (v,nil)", v, err)
	}
}

// TestTiKVKVPut：Begin/Set/Commit 各错误分支与成功写入。
func TestTiKVKVPut(t *testing.T) {
	ctx := context.Background()

	kv := newFakeKV(&fakeClient{beginErr: errInjected})
	if err := kv.Put(ctx, []byte("k"), []byte("v")); err != errInjected {
		t.Fatalf("Put begin err = %v", err)
	}

	kv = newFakeKV(&fakeClient{txn: &fakeTxn{setErr: errInjected}})
	if err := kv.Put(ctx, []byte("k"), []byte("v")); err != errInjected {
		t.Fatalf("Put set err = %v", err)
	}

	kv = newFakeKV(&fakeClient{txn: &fakeTxn{commitErr: errInjected}})
	if err := kv.Put(ctx, []byte("k"), []byte("v")); err != errInjected {
		t.Fatalf("Put commit err = %v", err)
	}

	txn := &fakeTxn{sets: map[string][]byte{}}
	kv = newFakeKV(&fakeClient{txn: txn})
	if err := kv.Put(ctx, []byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put = %v", err)
	}
	if string(txn.sets["k"]) != "v" {
		t.Fatalf("Put sets = %v", txn.sets)
	}
}

// TestTiKVKVDelete：Begin/Delete/Commit 各错误分支与成功删除。
func TestTiKVKVDelete(t *testing.T) {
	ctx := context.Background()

	kv := newFakeKV(&fakeClient{beginErr: errInjected})
	if err := kv.Delete(ctx, []byte("k")); err != errInjected {
		t.Fatalf("Delete begin err = %v", err)
	}

	kv = newFakeKV(&fakeClient{txn: &fakeTxn{delErr: errInjected}})
	if err := kv.Delete(ctx, []byte("k")); err != errInjected {
		t.Fatalf("Delete err = %v", err)
	}

	kv = newFakeKV(&fakeClient{txn: &fakeTxn{commitErr: errInjected}})
	if err := kv.Delete(ctx, []byte("k")); err != errInjected {
		t.Fatalf("Delete commit err = %v", err)
	}

	txn := &fakeTxn{}
	kv = newFakeKV(&fakeClient{txn: txn})
	if err := kv.Delete(ctx, []byte("k")); err != nil {
		t.Fatalf("Delete = %v", err)
	}
	if len(txn.dels) != 1 || string(txn.dels[0]) != "k" {
		t.Fatalf("Delete dels = %q", txn.dels)
	}
}

// TestTiKVKVDeleteRange：空区间直接返回、多轮 scan+delete、以及各步错误分支。
func TestTiKVKVDeleteRange(t *testing.T) {
	ctx := context.Background()
	start, end := []byte("a"), []byte("z")

	// 区间内无 key：直接返回 nil。
	kv := newFakeKV(&fakeClient{snap: &fakeSnapshot{iters: []*fakeIter{{}}}})
	if err := kv.DeleteRange(ctx, start, end); err != nil {
		t.Fatalf("DeleteRange empty = %v", err)
	}

	// 两轮删除后第三轮为空：循环两次后收敛。
	txn := &fakeTxn{}
	snap := &fakeSnapshot{iters: []*fakeIter{
		{keys: [][]byte{[]byte("a1")}, vals: [][]byte{[]byte("v")}},
		{keys: [][]byte{[]byte("a2")}, vals: [][]byte{[]byte("v")}},
		{},
	}}
	kv = newFakeKV(&fakeClient{snap: snap, txn: txn})
	if err := kv.DeleteRange(ctx, start, end); err != nil {
		t.Fatalf("DeleteRange = %v", err)
	}
	if len(txn.dels) != 2 || string(txn.dels[0]) != "a1" || string(txn.dels[1]) != "a2" {
		t.Fatalf("DeleteRange dels = %q", txn.dels)
	}

	// scan 失败。
	kv = newFakeKV(&fakeClient{snap: &fakeSnapshot{iterErr: errInjected}})
	if err := kv.DeleteRange(ctx, start, end); err != errInjected {
		t.Fatalf("DeleteRange scan err = %v", err)
	}

	// 每个用例用独立迭代器（fakeIter 会被 Scan 消费，复用会导致第二轮读空）。
	oneIter := func() []*fakeIter {
		return []*fakeIter{{keys: [][]byte{[]byte("a1")}, vals: [][]byte{[]byte("v")}}}
	}
	// Begin 失败。
	kv = newFakeKV(&fakeClient{snap: &fakeSnapshot{iters: oneIter()}, beginErr: errInjected})
	if err := kv.DeleteRange(ctx, start, end); err != errInjected {
		t.Fatalf("DeleteRange begin err = %v", err)
	}
	// 事务内 Delete 失败。
	kv = newFakeKV(&fakeClient{snap: &fakeSnapshot{iters: oneIter()}, txn: &fakeTxn{delErr: errInjected}})
	if err := kv.DeleteRange(ctx, start, end); err != errInjected {
		t.Fatalf("DeleteRange del err = %v", err)
	}
	// Commit 失败。
	kv = newFakeKV(&fakeClient{snap: &fakeSnapshot{iters: oneIter()}, txn: &fakeTxn{commitErr: errInjected}})
	if err := kv.DeleteRange(ctx, start, end); err != errInjected {
		t.Fatalf("DeleteRange commit err = %v", err)
	}
}

// TestTiKVKVScan：limit 截断、快照/迭代器错误、Next 错误与正常区间读取。
func TestTiKVKVScan(t *testing.T) {
	ctx := context.Background()
	start, end := []byte("a"), []byte("z")
	data := &fakeIter{
		keys: [][]byte{[]byte("a1"), []byte("a2"), []byte("a3")},
		vals: [][]byte{[]byte("v1"), []byte("v2"), []byte("v3")},
	}

	kv := newFakeKV(&fakeClient{snap: &fakeSnapshot{iters: []*fakeIter{data}}})
	ks, vs, err := kv.Scan(ctx, start, end, 0)
	if err != nil || len(ks) != 3 || string(vs[2]) != "v3" {
		t.Fatalf("Scan = (%q,%q,%v)", ks, vs, err)
	}
	if !data.closed {
		t.Fatal("iterator not closed")
	}

	kv = newFakeKV(&fakeClient{snap: &fakeSnapshot{iters: []*fakeIter{{
		keys: [][]byte{[]byte("a1"), []byte("a2")},
		vals: [][]byte{[]byte("v1"), []byte("v2")},
	}}}})
	ks, _, err = kv.Scan(ctx, start, end, 1)
	if err != nil || len(ks) != 1 {
		t.Fatalf("Scan limit=1 = (%q,%v)", ks, err)
	}

	kv = newFakeKV(&fakeClient{tsErr: errInjected})
	if _, _, err := kv.Scan(ctx, start, end, 0); err == nil {
		t.Fatal("Scan with TSO error should fail")
	}
	kv = newFakeKV(&fakeClient{snap: &fakeSnapshot{iterErr: errInjected}})
	if _, _, err := kv.Scan(ctx, start, end, 0); err != errInjected {
		t.Fatalf("Scan iter err = %v", err)
	}
	kv = newFakeKV(&fakeClient{snap: &fakeSnapshot{iters: []*fakeIter{{
		keys:    [][]byte{[]byte("a1"), []byte("a2")},
		vals:    [][]byte{[]byte("v1"), []byte("v2")},
		nextErr: errInjected,
	}}}})
	if _, _, err := kv.Scan(ctx, start, end, 0); err != errInjected {
		t.Fatalf("Scan next err = %v", err)
	}
}

// TestTiKVKVBatchPut：Begin/Set/Commit 错误分支与批量写入。
func TestTiKVKVBatchPut(t *testing.T) {
	ctx := context.Background()
	kvs := map[string][]byte{"k1": []byte("v1"), "k2": []byte("v2")}

	kv := newFakeKV(&fakeClient{beginErr: errInjected})
	if err := kv.BatchPut(ctx, kvs); err != errInjected {
		t.Fatalf("BatchPut begin err = %v", err)
	}
	kv = newFakeKV(&fakeClient{txn: &fakeTxn{setErr: errInjected}})
	if err := kv.BatchPut(ctx, kvs); err != errInjected {
		t.Fatalf("BatchPut set err = %v", err)
	}
	kv = newFakeKV(&fakeClient{txn: &fakeTxn{commitErr: errInjected}})
	if err := kv.BatchPut(ctx, kvs); err != errInjected {
		t.Fatalf("BatchPut commit err = %v", err)
	}

	txn := &fakeTxn{sets: map[string][]byte{}}
	kv = newFakeKV(&fakeClient{txn: txn})
	if err := kv.BatchPut(ctx, kvs); err != nil {
		t.Fatalf("BatchPut = %v", err)
	}
	if len(txn.sets) != 2 || string(txn.sets["k2"]) != "v2" {
		t.Fatalf("BatchPut sets = %v", txn.sets)
	}
}

// TestTiKVKVBatchGet：缺失 key 对齐为 nil、快照错误与批量读取错误。
func TestTiKVKVBatchGet(t *testing.T) {
	ctx := context.Background()
	keys := [][]byte{[]byte("k1"), []byte("k2"), []byte("k3")}

	kv := newFakeKV(&fakeClient{snap: &fakeSnapshot{batchRes: map[string][]byte{"k1": []byte("v1"), "k3": []byte("v3")}}})
	vals, err := kv.BatchGet(ctx, keys)
	if err != nil || len(vals) != 3 || string(vals[0]) != "v1" || vals[1] != nil || string(vals[2]) != "v3" {
		t.Fatalf("BatchGet = (%q,%v)", vals, err)
	}

	kv = newFakeKV(&fakeClient{tsErr: errInjected})
	if _, err := kv.BatchGet(ctx, keys); err == nil {
		t.Fatal("BatchGet with TSO error should fail")
	}
	kv = newFakeKV(&fakeClient{snap: &fakeSnapshot{batchErr: errInjected}})
	if _, err := kv.BatchGet(ctx, keys); err != errInjected {
		t.Fatalf("BatchGet err = %v", err)
	}
}

// TestTiKVKVClose：关闭透传（含错误返回）。
func TestTiKVKVClose(t *testing.T) {
	c := &fakeClient{}
	if err := newFakeKV(c).Close(); err != nil || !c.closed {
		t.Fatalf("Close = %v, closed=%v", err, c.closed)
	}
}
