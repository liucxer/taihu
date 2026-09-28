// Package web 提供 taihu 集群管理 Web 页面：HTTP API + go:embed 内嵌前端（单二进制）。
//
// 分层：handler 只依赖本包 Service 接口，不感知 CLI / TiKV / rpcclient 的具体实现；
// Service 由 cmd/taihu/cmd 包实现（复用其未导出的 helper：listAllInstances/dialInstance/
// pingInstance/probeSegments/resolveKeyTarget 等）。该切分让 httptest 可用 fake 实现
// 隔离测试 handler 层，命令级测试用真实实现 + cluster.NewMemoryKV。
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"github.com/liucxer/taihu/pkg/ierr"
)

//go:embed static
var staticFS embed.FS

// Server 集群管理页 HTTP 服务。svc 为能力实现（cmd 包注入）。
type Server struct {
	svc     Service
	timeout time.Duration // 单请求上下文超时
}

// New 构造 Server，默认单请求超时 60s：长于 CLI 的 5s，避免 cluster status 等多实例
// 逐段探测（每实例内部已有 3s 超时）被整体上下文过早掐断。
func New(svc Service) *Server {
	return &Server{svc: svc, timeout: 60 * time.Second}
}

// Service 是 Web 后端所需能力，由 cmd 包实现，对应 CLI 各子命令逻辑。
type Service interface {
	Version(ctx context.Context) (VersionInfo, error)
	ClusterList(ctx context.Context) ([]InstanceRow, error)
	ClusterStatus(ctx context.Context) ([]StatusRow, error)
	ClusterIndex(ctx context.Context, prefix string) (IndexResult, error)
	ClusterPurge(ctx context.Context, confirm bool) (PurgeResult, error)
	ClientList(ctx context.Context) (ClientListResult, error)
	ClientInfo(ctx context.Context) (ClientInfoResult, error)
	InstanceSegments(ctx context.Context, instName string, detail bool) ([]SegmentResult, error)
	KeyStat(ctx context.Context, key, addr, instName string) (KeyStatResult, error)
	KeyMeta(ctx context.Context, key, addr, instName string) (KeyMetaResult, error)
	KeyList(ctx context.Context, prefix string, limit int, addr, instName string) (KeyListResult, error)
	KeyGet(ctx context.Context, key string, off, size int64, addr, instName string) ([]byte, func(), error)
	KeyPut(ctx context.Context, key string, data []byte, size int64, addr, instName string) (KeyWriteResult, error)
	KeyDelete(ctx context.Context, key, addr, instName string) (KeyWriteResult, error)
}

// ---------- 出参类型（与 CLI 各命令 JSON 输出等价） ----------

// VersionInfo /api/version。
type VersionInfo struct {
	Version   string `json:"version"`
	GoVersion string `json:"go_version"`
}

// InstanceRow /api/cluster/list 一行。
type InstanceRow struct {
	Name          string `json:"name"`
	Node          string `json:"node"`
	Addr          string `json:"addr"`
	ShmAddr       string `json:"shm_addr,omitempty"`
	Status        string `json:"status"` // online | stale
	Capacity      int64  `json:"capacity"`
	Used          int64  `json:"used"`
	Available     int64  `json:"available"`
	StartTime     int64  `json:"start_time"`
	LastHeartbeat int64  `json:"last_heartbeat"`
	HeartbeatAge  string `json:"heartbeat_age,omitempty"`
}

// StatusRow /api/cluster/status 一行。
type StatusRow struct {
	Name        string `json:"name"`
	TcpRTT      string `json:"tcp_rtt,omitempty"`
	ShmOK       string `json:"shm_ok,omitempty"`
	SegFree     int64  `json:"seg_free,omitempty"`
	SegActive   int64  `json:"seg_active,omitempty"`
	SegFull     int64  `json:"seg_full,omitempty"`
	SegReclaim  int64  `json:"seg_reclaiming,omitempty"`
	CursorSeg   int64  `json:"cursor_seg,omitempty"`
	CursorOff   int64  `json:"cursor_off,omitempty"`
	ObjectCount int64  `json:"object_count,omitempty"`
	Error       string `json:"error,omitempty"`
}

// IndexInstCount 索引归组后一个实例的计数。
type IndexInstCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// IndexResult /api/cluster/index。
type IndexResult struct {
	Prefix    string          `json:"prefix,omitempty"`
	Total     int             `json:"total"`
	Instances []IndexInstCount `json:"instances"`
}

// PurgeCounts purge 预览分类计数。
type PurgeCounts struct {
	Instances int `json:"instances"`
	Index     int `json:"index"`
	Clients   int `json:"clients"`
}

// PurgeResult /api/cluster/purge。
type PurgeResult struct {
	Counts    PurgeCounts `json:"counts"`
	Total     int         `json:"total"`
	Deleted   int         `json:"deleted"`
	Confirmed bool        `json:"confirmed"`
}

// ClientRow /api/client/list 一行。
type ClientRow struct {
	ID            string            `json:"id"`
	Node          string            `json:"node"`
	Host          string            `json:"host,omitempty"`
	Pid           int               `json:"pid"`
	SDKVersion    string            `json:"sdk_version"`
	Addr          string            `json:"addr,omitempty"`
	Status        string            `json:"status"`
	HeartbeatAge  string            `json:"heartbeat_age,omitempty"`
	StartTime     int64             `json:"start_time"`
	LastHeartbeat int64             `json:"last_heartbeat"`
	Extra         map[string]string `json:"extra,omitempty"`
}

// ClientListResult /api/client/list。
type ClientListResult struct {
	Rows   []ClientRow `json:"rows"`
	Total  int         `json:"total"`
	Online int         `json:"online"`
	Stale  int         `json:"stale"`
}

// ConnRow 连通性一行。
type ConnRow struct {
	Name  string `json:"name"`
	Addr  string `json:"addr"`
	Shm   string `json:"shm,omitempty"`
	RTT   string `json:"rtt,omitempty"`
	Error string `json:"error,omitempty"`
	Local bool   `json:"local,omitempty"`
}

// ClientInfoResult /api/client/info。
type ClientInfoResult struct {
	Version      string            `json:"version"`
	GoVersion    string            `json:"go_version"`
	Config       map[string]string `json:"config"`
	KVStatus     string            `json:"kv_status"`
	KVCounts     map[string]int    `json:"kv_counts,omitempty"`
	Connectivity []ConnRow         `json:"connectivity,omitempty"`
}

// SegmentSummary 实例段汇总（等价 rpcclient.SegmentSummary 的 JSON 形态）。
type SegmentSummary struct {
	Total       int64 `json:"total"`
	Free        int64 `json:"free"`
	Active      int64 `json:"active"`
	Full        int64 `json:"full"`
	Reclaiming  int64 `json:"reclaiming"`
	CursorSeg   int64 `json:"cursor_seg"`
	CursorOff   int64 `json:"cursor_off"`
	SegSize     int64 `json:"seg_size"`
	ObjectCount int64 `json:"object_count"`
}

// SegmentEntry 单个 segment 状态（detail=1 时返回）。
type SegmentEntry struct {
	SegmentID  int64  `json:"segment_id"`
	State      string `json:"state"` // free|active|full|reclaiming|compacting
	AliveCount int64  `json:"alive_count"`
	ReclaimSeq int64  `json:"reclaim_seq"`
}

// SegmentResult /api/instance/segments 单实例结果。
type SegmentResult struct {
	Name     string         `json:"name"`
	Addr     string         `json:"addr"`
	Summary  SegmentSummary `json:"summary"`
	Segments []SegmentEntry `json:"segments,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// KeyWriteResult key put/delete 结果。
type KeyWriteResult struct {
	Key      string `json:"key"`
	Size     int64  `json:"size,omitempty"`
	Instance string `json:"instance"`
}

// KeyStatResult /api/key/stat。
type KeyStatResult struct {
	Key      string `json:"key"`
	Size     int64  `json:"size"`
	Instance string `json:"instance"`
}

// KeyMetaResult /api/key/meta。
type KeyMetaResult struct {
	Key       string `json:"key"`
	Size      int64  `json:"size"`
	SegmentID int64  `json:"segment_id"`
	Offset    int64  `json:"offset"`
}

// KeyListResult /api/key/list。
type KeyListResult struct {
	Prefix string   `json:"prefix,omitempty"`
	Total  int      `json:"total"`
	Keys   []string `json:"keys"`
}

// Handler 返回装配好的 HTTP handler（Go 1.22+ 方法路由）。
func (s *Server) Handler() http.Handler {
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(fmt.Sprintf("embed static: %v", err)) // go:embed 目录必然存在
	}
	mux := http.NewServeMux()
	// "/" 全方法注册：静态页兜底。Go 1.22+ ServeMux 下若用 "GET /"，
	// 会与 "/api/"（匹配更多方法但路径更具体）冲突 panic，必须用无方法限定。
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.HandleFunc("GET /api/cluster/list", s.handleClusterList)
	mux.HandleFunc("GET /api/cluster/status", s.handleClusterStatus)
	mux.HandleFunc("GET /api/cluster/index", s.handleClusterIndex)
	mux.HandleFunc("POST /api/cluster/purge", s.handleClusterPurge)
	mux.HandleFunc("GET /api/client/list", s.handleClientList)
	mux.HandleFunc("GET /api/client/info", s.handleClientInfo)
	mux.HandleFunc("GET /api/instance/segments", s.handleInstanceSegments)
	mux.HandleFunc("GET /api/key/stat", s.handleKeyStat)
	mux.HandleFunc("GET /api/key/meta", s.handleKeyMeta)
	mux.HandleFunc("GET /api/key/list", s.handleKeyList)
	mux.HandleFunc("GET /api/key/get", s.handleKeyGet)
	mux.HandleFunc("POST /api/key/put", s.handleKeyPut)
	mux.HandleFunc("DELETE /api/key/delete", s.handleKeyDelete)
	mux.HandleFunc("/api/", s.handleAPINotFound) // 未知 /api 路径统一 JSON 404
	return mux
}

// ---------- 输出与错误辅助 ----------

// writeJSON 统一 JSON 输出。
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr 统一错误输出：{"error":"..."}。
func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// errStringf 生成错误（handler 内联小文案用）。
func errStringf(format string, args ...interface{}) error {
	return fmt.Errorf(format, args...)
}

// apiStatus 把库错误映射为 HTTP 状态码。
func apiStatus(err error) int {
	switch {
	case errors.Is(err, ierr.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ierr.ErrInvalidRange):
		return http.StatusBadRequest
	case errors.Is(err, ierr.ErrNoSpace), errors.Is(err, ierr.ErrTooLarge), errors.Is(err, ierr.ErrShortWrite):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusBadGateway
	}
}
