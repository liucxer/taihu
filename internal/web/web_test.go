// handler 层用例：fake Service + httptest，隔离实现，覆盖各路由 happy path、
// purge/delete 二次确认、key get 流式字节、未知 /api 404、静态页与错误码映射。
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/liucxer/taihu/pkg/ierr"
)

// fakeService 实现 Service：可配置返回值与错误，并记录调用参数供断言。
type fakeService struct {
	version    VersionInfo
	instances  []InstanceRow
	status     []StatusRow
	index      IndexResult
	purge      PurgeResult
	clients    ClientListResult
	clientInfo ClientInfoResult
	segments   []SegmentResult
	stat       KeyStatResult
	meta       KeyMetaResult
	klist      KeyListResult
	getData    []byte
	putRes     KeyWriteResult
	delRes     KeyWriteResult

	listErr  error
	purgeErr error
	statErr  error
	getErr   error
	putErr   error
	delErr   error

	gotConfirm  bool
	gotKey      string
	gotPutData  []byte
	gotPutSize  int64
	gotGetOff   int64
	gotGetSize  int64
	gotPrefix   string
	gotLimit    int
	gotInstName string
}

func (f *fakeService) Version(context.Context) (VersionInfo, error) { return f.version, nil }

func (f *fakeService) ClusterList(context.Context) ([]InstanceRow, error) {
	return f.instances, f.listErr
}

func (f *fakeService) ClusterStatus(context.Context) ([]StatusRow, error) { return f.status, nil }

func (f *fakeService) ClusterIndex(_ context.Context, prefix string) (IndexResult, error) {
	f.gotPrefix = prefix
	return f.index, nil
}

func (f *fakeService) ClusterPurge(_ context.Context, confirm bool) (PurgeResult, error) {
	f.gotConfirm = confirm
	return f.purge, f.purgeErr
}

func (f *fakeService) ClientList(context.Context) (ClientListResult, error) { return f.clients, nil }

func (f *fakeService) ClientInfo(context.Context) (ClientInfoResult, error) { return f.clientInfo, nil }

func (f *fakeService) InstanceSegments(_ context.Context, instName string, _ bool) ([]SegmentResult, error) {
	f.gotInstName = instName
	return f.segments, nil
}

func (f *fakeService) KeyStat(_ context.Context, key, _, _ string) (KeyStatResult, error) {
	f.gotKey = key
	return f.stat, f.statErr
}

func (f *fakeService) KeyMeta(context.Context, string, string, string) (KeyMetaResult, error) {
	return f.meta, nil
}

func (f *fakeService) KeyList(_ context.Context, prefix string, limit int, _, _ string) (KeyListResult, error) {
	f.gotPrefix = prefix
	f.gotLimit = limit
	return f.klist, nil
}

func (f *fakeService) KeyGet(_ context.Context, key string, off, size int64, _, _ string) ([]byte, func(), error) {
	f.gotKey = key
	f.gotGetOff = off
	f.gotGetSize = size
	if f.getErr != nil {
		return nil, nil, f.getErr
	}
	return f.getData, func() {}, nil
}

func (f *fakeService) KeyPut(_ context.Context, key string, data []byte, size int64, _, _ string) (KeyWriteResult, error) {
	f.gotKey = key
	f.gotPutData = data
	f.gotPutSize = size
	return f.putRes, f.putErr
}

func (f *fakeService) KeyDelete(_ context.Context, key, _, _ string) (KeyWriteResult, error) {
	f.gotKey = key
	return f.delRes, f.delErr
}

var _ Service = (*fakeService)(nil)

// doReq 发起请求并返回响应记录器。
func doReq(t *testing.T, h http.Handler, method, path string, body io.Reader, ct string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func newTestServer() (*Server, *fakeService) {
	f := &fakeService{}
	return New(f), f
}

func TestWebVersion(t *testing.T) {
	s, f := newTestServer()
	f.version = VersionInfo{Version: "v1.0.3", GoVersion: "go1.25"}
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/version", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body=%s", rr.Code, rr.Body.String())
	}
	var v VersionInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Version != "v1.0.3" || v.GoVersion != "go1.25" {
		t.Fatalf("version = %+v", v)
	}
}

func TestWebClusterList(t *testing.T) {
	s, f := newTestServer()
	f.instances = []InstanceRow{{Name: "TAIHU-0", Node: "n11", Status: "online"}}
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/cluster/list", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	var rows []InstanceRow
	if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "TAIHU-0" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestWebClusterStatus(t *testing.T) {
	s, f := newTestServer()
	f.status = []StatusRow{{Name: "TAIHU-0", SegFree: 3, Error: ""}}
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/cluster/status", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	var rows []StatusRow
	_ = json.Unmarshal(rr.Body.Bytes(), &rows)
	if len(rows) != 1 || rows[0].SegFree != 3 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestWebClusterIndex(t *testing.T) {
	s, f := newTestServer()
	f.index = IndexResult{Prefix: "a", Total: 3,
		Instances: []IndexInstCount{{Name: "TAIHU-0", Count: 3}}}
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/cluster/index?prefix=a", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if f.gotPrefix != "a" {
		t.Fatalf("prefix = %q", f.gotPrefix)
	}
	var res IndexResult
	_ = json.Unmarshal(rr.Body.Bytes(), &res)
	if res.Total != 3 {
		t.Fatalf("res = %+v", res)
	}
}

func TestWebPurgeNoConfirmPreview(t *testing.T) {
	s, f := newTestServer()
	f.purge = PurgeResult{Counts: PurgeCounts{Instances: 2}, Total: 2, Confirmed: false}
	rr := doReq(t, s.Handler(), http.MethodPost, "/api/cluster/purge", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body=%s", rr.Code, rr.Body.String())
	}
	if f.gotConfirm {
		t.Fatal("无 confirm 时应传 false")
	}
	var res PurgeResult
	_ = json.Unmarshal(rr.Body.Bytes(), &res)
	if res.Confirmed || res.Deleted != 0 {
		t.Fatalf("预览应 Confirmed=false Deleted=0，实际 %+v", res)
	}
}

func TestWebPurgeConfirmQuery(t *testing.T) {
	s, f := newTestServer()
	f.purge = PurgeResult{Counts: PurgeCounts{Instances: 2, Index: 1, Clients: 1},
		Total: 4, Confirmed: true, Deleted: 4}
	rr := doReq(t, s.Handler(), http.MethodPost, "/api/cluster/purge?confirm=true", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !f.gotConfirm {
		t.Fatal("confirm=true 应传给 Service")
	}
	var res PurgeResult
	_ = json.Unmarshal(rr.Body.Bytes(), &res)
	if !res.Confirmed || res.Deleted != 4 {
		t.Fatalf("res = %+v", res)
	}
}

func TestWebPurgeConfirmBody(t *testing.T) {
	s, f := newTestServer()
	f.purge = PurgeResult{Confirmed: true}
	rr := doReq(t, s.Handler(), http.MethodPost, "/api/cluster/purge",
		strings.NewReader(`{"confirm":true}`), "application/json")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !f.gotConfirm {
		t.Fatal("JSON body confirm=true 应传给 Service")
	}
}

func TestWebPurgeBadConfirm(t *testing.T) {
	s, _ := newTestServer()
	rr := doReq(t, s.Handler(), http.MethodPost, "/api/cluster/purge?confirm=banana", nil, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rr.Code)
	}
}

func TestWebClientList(t *testing.T) {
	s, f := newTestServer()
	f.clients = ClientListResult{Rows: []ClientRow{{ID: "c1", Node: "n11", Status: "online"}},
		Total: 1, Online: 1}
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/client/list", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	var res ClientListResult
	_ = json.Unmarshal(rr.Body.Bytes(), &res)
	if res.Total != 1 || res.Online != 1 {
		t.Fatalf("res = %+v", res)
	}
}

func TestWebClientInfo(t *testing.T) {
	s, f := newTestServer()
	f.clientInfo = ClientInfoResult{Version: "v1", KVStatus: "tikv txnkv ok"}
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/client/info", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	var res ClientInfoResult
	_ = json.Unmarshal(rr.Body.Bytes(), &res)
	if res.KVStatus != "tikv txnkv ok" {
		t.Fatalf("res = %+v", res)
	}
}

func TestWebInstanceSegments(t *testing.T) {
	s, f := newTestServer()
	f.segments = []SegmentResult{{Name: "TAIHU-0", Summary: SegmentSummary{Total: 2048}}}
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/instance/segments?instance=TAIHU-0&detail=1", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if f.gotInstName != "TAIHU-0" {
		t.Fatalf("instance = %q", f.gotInstName)
	}
}

func TestWebKeyStat(t *testing.T) {
	s, f := newTestServer()
	f.stat = KeyStatResult{Key: "hello", Size: 5, Instance: "direct"}
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/key/stat?key=hello", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if f.gotKey != "hello" {
		t.Fatalf("key = %q", f.gotKey)
	}
	var res KeyStatResult
	_ = json.Unmarshal(rr.Body.Bytes(), &res)
	if res.Size != 5 {
		t.Fatalf("res = %+v", res)
	}
}

func TestWebKeyStatNoKey(t *testing.T) {
	s, _ := newTestServer()
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/key/stat", nil, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rr.Code)
	}
}

func TestWebKeyMeta(t *testing.T) {
	s, f := newTestServer()
	f.meta = KeyMetaResult{Key: "hello", Size: 5, SegmentID: 1, Offset: 2}
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/key/meta?key=hello", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
}

func TestWebKeyListLimit(t *testing.T) {
	s, f := newTestServer()
	f.klist = KeyListResult{Prefix: "a", Total: 2, Keys: []string{"a1", "a2"}}
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/key/list?prefix=a&limit=1", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if f.gotPrefix != "a" || f.gotLimit != 1 {
		t.Fatalf("prefix/limit = %q/%d", f.gotPrefix, f.gotLimit)
	}
}

func TestWebKeyGetStream(t *testing.T) {
	s, f := newTestServer()
	f.getData = []byte("hello")
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/key/get?key=hello&off=1&size=3", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if got := rr.Body.String(); got != "hello" {
		t.Fatalf("data = %q", got)
	}
	if f.gotGetOff != 1 || f.gotGetSize != 3 {
		t.Fatalf("off/size = %d/%d", f.gotGetOff, f.gotGetSize)
	}
}

func TestWebKeyGetNoKey(t *testing.T) {
	s, _ := newTestServer()
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/key/get", nil, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rr.Code)
	}
}

func TestWebKeyGetNotFound(t *testing.T) {
	s, f := newTestServer()
	f.getErr = ierr.ErrNotFound
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/key/get?key=nope", nil, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rr.Code)
	}
}

func TestWebKeyPutMultipart(t *testing.T) {
	s, f := newTestServer()
	f.putRes = KeyWriteResult{Key: "hello", Size: 5, Instance: "direct"}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("key", "hello")
	_ = mw.WriteField("size", "5")
	fw, err := mw.CreateFormFile("file", "hello")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("hello-taihu"))
	_ = mw.Close()
	rr := doReq(t, s.Handler(), http.MethodPost, "/api/key/put", &buf, mw.FormDataContentType())
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(f.gotPutData, []byte("hello-taihu")) {
		t.Fatalf("data = %q", f.gotPutData)
	}
	if f.gotPutSize != 5 {
		t.Fatalf("size = %d", f.gotPutSize)
	}
	var res KeyWriteResult
	_ = json.Unmarshal(rr.Body.Bytes(), &res)
	if res.Size != 5 || res.Key != "hello" {
		t.Fatalf("res = %+v", res)
	}
}

func TestWebKeyPutNoFile(t *testing.T) {
	s, _ := newTestServer()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("key", "hello")
	_ = mw.Close()
	rr := doReq(t, s.Handler(), http.MethodPost, "/api/key/put", &buf, mw.FormDataContentType())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rr.Code)
	}
}

func TestWebKeyDeleteRequiresConfirm(t *testing.T) {
	s, _ := newTestServer()
	rr := doReq(t, s.Handler(), http.MethodDelete, "/api/key/delete?key=hello", nil, "")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422", rr.Code)
	}
}

func TestWebKeyDeleteConfirmed(t *testing.T) {
	s, f := newTestServer()
	f.delRes = KeyWriteResult{Key: "hello", Instance: "direct"}
	rr := doReq(t, s.Handler(), http.MethodDelete, "/api/key/delete?key=hello&confirm=true", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body=%s", rr.Code, rr.Body.String())
	}
	if f.gotKey != "hello" {
		t.Fatalf("key = %q", f.gotKey)
	}
}

func TestWebAPINotFound(t *testing.T) {
	s, _ := newTestServer()
	rr := doReq(t, s.Handler(), http.MethodGet, "/api/nope", nil, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not found") {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestWebStaticIndex(t *testing.T) {
	s, _ := newTestServer()
	rr := doReq(t, s.Handler(), http.MethodGet, "/", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("index code = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "taihu 集群管理") {
		t.Fatalf("index 缺标题标记，body 前 200 字符：%s", rr.Body.String()[:200])
	}
	rr2 := doReq(t, s.Handler(), http.MethodGet, "/app.js", nil, "")
	if rr2.Code != http.StatusOK {
		t.Fatalf("app.js code = %d", rr2.Code)
	}
}

func TestWebErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{ierr.ErrNotFound, http.StatusNotFound},
		{ierr.ErrInvalidRange, http.StatusBadRequest},
		{ierr.ErrNoSpace, http.StatusUnprocessableEntity},
		{ierr.ErrTooLarge, http.StatusUnprocessableEntity},
		{ierr.ErrShortWrite, http.StatusUnprocessableEntity},
		{fmt.Errorf("boom"), http.StatusBadGateway},
	}
	for _, c := range cases {
		s, f := newTestServer()
		f.listErr = c.err
		rr := doReq(t, s.Handler(), http.MethodGet, "/api/cluster/list", nil, "")
		if rr.Code != c.want {
			t.Fatalf("err=%v: code=%d, want %d", c.err, rr.Code, c.want)
		}
	}
}
