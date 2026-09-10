package rpcclient

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	"github.com/liucxer/taihu/internal/rpcserver"
	"github.com/liucxer/taihu/pkg/taihu"
)

// newBufnetServer 起一个跑在 bufconn 上的本地 taihu-server，返回 listener 和关闭函数。
func newBufnetServer(t *testing.T) (*bufconn.Listener, func()) {
	t.Helper()
	dir := t.TempDir()
	devPath := filepath.Join(dir, "nvme.img")
	f, err := os.Create(devPath)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	_ = f.Close()

	storage, err := taihu.NewStorage(context.Background(), filepath.Join(dir, "meta"), devPath)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	gs := rpcserver.New(storage)
	lis := bufconn.Listen(2 << 20)
	go func() {
		_ = gs.Serve(lis)
	}()
	return lis, func() {
		gs.Stop()
		_ = storage.Close()
		_ = lis.Close()
	}
}

func TestDialPoolRoundTrip(t *testing.T) {
	lis, cleanup := newBufnetServer(t)
	defer cleanup()

	s, err := DialPoolWithOptions(context.Background(), "passthrough:///bufnet", 4,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}))
	if err != nil {
		t.Fatalf("DialPool: %v", err)
	}
	defer s.Close()

	// 4 连接 × 多对象交错读写，覆盖 round-robin 分发到不同连接。
	payload := bytes.Repeat([]byte("taihu-pool-smoke"), 4096) // ~64KiB
	if os.Getenv("BIG4M") != "" {
		payload = make([]byte, 4194304)
		for i := range payload {
			payload[i] = byte(i)
		}
	}
	for i := 0; i < 32; i++ {
		key := "pool/obj"
		key += string(rune('0' + i%10))
		key = key + "/" + string(rune('a'+i%26))
		key = "pool/" + string(rune('a'+i%26)) + string(rune('0'+i%10))
		if err := s.Put(context.Background(), key, int64(len(payload)), payload); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
		// Get 读回，验证 round-trip 数据一致。
		got, err := s.Get(context.Background(), key, 0, int64(len(payload)))
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("Get mismatch key=%s got=%dB want=%dB", key, len(got), len(payload))
		}
		sz, err := s.Stat(context.Background(), key)
		if err != nil || sz != int64(len(payload)) {
			t.Fatalf("Stat %s: sz=%d err=%v", key, sz, err)
		}
		if err := s.Delete(context.Background(), key); err != nil {
			t.Fatalf("Delete %s: %v", key, err)
		}
	}
}
