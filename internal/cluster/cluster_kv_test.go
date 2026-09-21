package cluster

import (
	"context"
	"errors"
	"testing"
	"time"
)

// errInjected 注入到 faultKV 的错误，便于错误分支断言。
var errInjected = errors.New("injected kv failure")

// faultKV 可注入错误的 KV 桩（嵌入 MemoryKV 复用成功路径），用于覆盖错误分支。
type faultKV struct {
	MemoryKV
	err error
}

func (f *faultKV) Put(context.Context, []byte, []byte) error { return f.err }
func (f *faultKV) Get(context.Context, []byte) ([]byte, error) {
	return nil, f.err
}
func (f *faultKV) Delete(context.Context, []byte) error { return f.err }
func (f *faultKV) Scan(context.Context, []byte, []byte, int) ([][]byte, [][]byte, error) {
	return nil, nil, f.err
}

// TestMemoryKVDeleteRangeAndClose：DeleteRange 区间删除、空区间(全量)语义与 Close。
func TestMemoryKVDeleteRangeAndClose(t *testing.T) {
	kv := NewMemoryKV()
	ctx := context.Background()
	if err := kv.BatchPut(ctx, map[string][]byte{
		"/taihu/instances/a": []byte("a"),
		"/taihu/instances/b": []byte("b"),
		"/taihu/clients/c":   []byte("c"),
	}); err != nil {
		t.Fatal(err)
	}
	start, end := InstanceScanRange()
	if err := kv.DeleteRange(ctx, start, end); err != nil {
		t.Fatal(err)
	}
	if v, _ := kv.Get(ctx, []byte("/taihu/instances/a")); v != nil {
		t.Fatal("instances not deleted by range")
	}
	if v, _ := kv.Get(ctx, []byte("/taihu/clients/c")); v == nil {
		t.Fatal("out-of-range key deleted")
	}

	// 空 start/end：匹配全部 key。
	if err := kv.DeleteRange(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	keys, _, err := kv.Scan(ctx, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("after full DeleteRange keys = %q, want empty", keys)
	}
	if err := kv.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
}

// TestTaihuDataRangeAndKeys：命名空间 key 编码与全量清理区间。
func TestTaihuDataRangeAndKeys(t *testing.T) {
	start, end := TaihuDataRange()
	if string(start) != UserDataPrefix || string(end) != UserDataPrefix+"\xff" {
		t.Fatalf("TaihuDataRange = (%q,%q)", start, end)
	}
	if got := string(IndexKey("k1")); got != IndexKeyPrefix+"k1" {
		t.Fatalf("IndexKey = %q", got)
	}
	if got := string(ClientKey("id1")); got != ClientKeyPrefix+"id1" {
		t.Fatalf("ClientKey = %q", got)
	}
}

// TestAlivenessDefaultTimeout：timeout<=0 时按默认 5s 判定（实例与客户端语义一致）。
func TestAlivenessDefaultTimeout(t *testing.T) {
	now := time.Now()
	inst := &InstanceInfo{LastHeartbeat: now.Unix()}
	if !inst.Aliveness(now, 0) {
		t.Fatal("fresh instance heartbeat should be alive with default timeout")
	}
	staleInst := &InstanceInfo{LastHeartbeat: now.Add(-time.Minute).Unix()}
	if staleInst.Aliveness(now, 0) {
		t.Fatal("stale instance heartbeat should be offline with default timeout")
	}
	cli := &ClientInfo{LastHeartbeat: now.Unix()}
	if !cli.Aliveness(now, 0) {
		t.Fatal("fresh client heartbeat should be alive with default timeout")
	}
	staleCli := &ClientInfo{LastHeartbeat: now.Add(-time.Minute).Unix()}
	if staleCli.Aliveness(now, 0) {
		t.Fatal("stale client heartbeat should be offline with default timeout")
	}
}

// TestClientRegisterListGetUnregister：客户端注册/列表/读取/注销往返与脏数据跳过。
func TestClientRegisterListGetUnregister(t *testing.T) {
	kv := NewMemoryKV()
	ctx := context.Background()
	start, end := ClientScanRange()
	if string(start) != ClientKeyPrefix || string(end) != ClientKeyPrefix+"\xff" {
		t.Fatalf("ClientScanRange = (%q,%q)", start, end)
	}
	now := time.Now().Unix()
	for _, id := range []string{"c1", "c2"} {
		info := &ClientInfo{ID: id, Node: "n1", Pid: 42, SDKVersion: "v1", StartTime: now, LastHeartbeat: now}
		if err := RegisterClient(ctx, kv, info); err != nil {
			t.Fatal(err)
		}
	}
	// 脏数据（非 JSON）应被 ListClients 跳过。
	if err := kv.Put(ctx, ClientKey("dirty"), []byte("{not-json")); err != nil {
		t.Fatal(err)
	}
	all, err := ListClients(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListClients = %+v, want 2 (dirty skipped)", all)
	}

	got, err := GetClient(ctx, kv, "c1")
	if err != nil || got == nil || got.Pid != 42 {
		t.Fatalf("GetClient(c1) = %+v, %v", got, err)
	}
	if got, err := GetClient(ctx, kv, "missing"); err != nil || got != nil {
		t.Fatalf("GetClient(missing) = %+v, %v, want (nil, nil)", got, err)
	}
	if _, err := GetClient(ctx, kv, "dirty"); err == nil {
		t.Fatal("GetClient(dirty) should return decode error")
	}

	// 幂等覆盖写。
	info := &ClientInfo{ID: "c1", Pid: 43, LastHeartbeat: now}
	if err := RegisterClient(ctx, kv, info); err != nil {
		t.Fatal(err)
	}
	if got, _ := GetClient(ctx, kv, "c1"); got == nil || got.Pid != 43 {
		t.Fatalf("re-register not applied: %+v", got)
	}
	if err := UnregisterClient(ctx, kv, "c1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := GetClient(ctx, kv, "c1"); got != nil {
		t.Fatalf("after unregister: %+v", got)
	}
}

// TestClientErrors：客户端注册/列表/读取各错误分支经注入 KV 覆盖。
func TestClientErrors(t *testing.T) {
	ctx := context.Background()
	kv := &faultKV{err: errInjected}
	if err := RegisterClient(ctx, kv, &ClientInfo{ID: "x"}); err != errInjected {
		t.Fatalf("RegisterClient err = %v", err)
	}
	if err := UnregisterClient(ctx, kv, "x"); err != errInjected {
		t.Fatalf("UnregisterClient err = %v", err)
	}
	if _, err := ListClients(ctx, kv); err != errInjected {
		t.Fatalf("ListClients err = %v", err)
	}
	if _, err := GetClient(ctx, kv, "x"); err != errInjected {
		t.Fatalf("GetClient err = %v", err)
	}
}

// TestRunClientHeartbeat：心跳周期写入（含 interval<=0 默认值）与 ctx 取消退出。
func TestRunClientHeartbeat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kv := NewMemoryKV()

	// interval<=0 且 ctx 已取消：立即返回（覆盖默认 interval 分支）。
	dead, deadCancel := context.WithCancel(ctx)
	deadCancel()
	RunClientHeartbeat(dead, kv, func() *ClientInfo { return &ClientInfo{ID: "hb"} }, 0)

	done := make(chan struct{})
	go func() {
		RunClientHeartbeat(ctx, kv, func() *ClientInfo { return &ClientInfo{ID: "hb", Pid: 7} }, 10*time.Millisecond)
		close(done)
	}()
	waitFor(t, func() bool {
		got, err := GetClient(ctx, kv, "hb")
		return err == nil && got != nil && got.Pid == 7 && got.LastHeartbeat > 0
	}, "client heartbeat not written")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunClientHeartbeat did not exit on ctx cancel")
	}
}

// TestRegisterErrorAndDirtyPaths：实例注册各错误分支与脏数据跳过。
func TestRegisterErrorAndDirtyPaths(t *testing.T) {
	ctx := context.Background()
	kv := &faultKV{err: errInjected}
	if err := Register(ctx, kv, &InstanceInfo{Name: "n"}); err != errInjected {
		t.Fatalf("Register err = %v", err)
	}
	if err := Unregister(ctx, kv, "n"); err != errInjected {
		t.Fatalf("Unregister err = %v", err)
	}
	if _, err := ListInstances(ctx, kv); err != errInjected {
		t.Fatalf("ListInstances err = %v", err)
	}
	if _, err := GetInstance(ctx, kv, "n"); err != errInjected {
		t.Fatalf("GetInstance err = %v", err)
	}

	mem := NewMemoryKV()
	if err := mem.Put(ctx, InstanceKey("dirty"), []byte("not-json")); err != nil {
		t.Fatal(err)
	}
	all, err := ListInstances(ctx, mem)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("ListInstances = %+v, want empty (dirty skipped)", all)
	}
	if _, err := GetInstance(ctx, mem, "dirty"); err == nil {
		t.Fatal("GetInstance(dirty) should return decode error")
	}
}

// TestRunHeartbeat：实例心跳周期写入（含 interval<=0 默认值）与 ctx 取消退出。
func TestRunHeartbeat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kv := NewMemoryKV()

	dead, deadCancel := context.WithCancel(ctx)
	deadCancel()
	RunHeartbeat(dead, kv, func() *InstanceInfo { return &InstanceInfo{Name: "hb"} }, 0)

	done := make(chan struct{})
	go func() {
		RunHeartbeat(ctx, kv, func() *InstanceInfo {
			return &InstanceInfo{Name: "hb", Node: "node1", Capacity: 100, Used: 3}
		}, 10*time.Millisecond)
		close(done)
	}()
	waitFor(t, func() bool {
		got, err := GetInstance(ctx, kv, "hb")
		return err == nil && got != nil && got.Used == 3 && got.LastHeartbeat > 0
	}, "instance heartbeat not written")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunHeartbeat did not exit on ctx cancel")
	}
}

// TestCapacityErrors：容量记录解码失败与 KV 错误分支。
func TestCapacityErrors(t *testing.T) {
	ctx := context.Background()
	kv := NewMemoryKV()
	if err := kv.Put(ctx, CapacityKey("bad"), []byte("not-json")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := GetCapacity(ctx, kv, "bad"); ok || err == nil {
		t.Fatalf("GetCapacity(bad) = ok=%v err=%v, want (false, err)", ok, err)
	}
	fault := &faultKV{err: errInjected}
	if _, _, err := GetCapacity(ctx, fault, "x"); err != errInjected {
		t.Fatalf("GetCapacity err = %v", err)
	}
	if err := PutCapacity(ctx, fault, "x", CapacityRecord{}); err != errInjected {
		t.Fatalf("PutCapacity err = %v", err)
	}
}

// waitFor 轮询等待条件成立（上限 2s），避免固定 sleep 造成的抖动。
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}
