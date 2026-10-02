// Sub-command taihu web 是集群管理 Web 页面（HTTP API + go:embed 前端）。
//
// 服务端以 net/http 提供 /api/* JSON 端点与内嵌静态页，能力实现 webService 复用
// cmd 包既有的 CLI helper（listAllInstances/dialInstance/pingInstance/probeSegments/
// resolveKeyTarget/targetName 等），语义与各 CLI 子命令一致：
//
//	cluster list|status|index|purge、client list|info、instance segments、
//	key put|get|delete|stat|meta|list、version。
package cmd

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/rpcclient"
	"github.com/liucxer/taihu/internal/version"
	internalweb "github.com/liucxer/taihu/internal/web"
)

// webCmd 启动集群管理 Web 页面。
var webCmd = &cobra.Command{
	Use:   "web",
	Short: "启动集群管理 Web 页面（HTTP API + 内嵌前端）",
	Long: `taihu web：启动集群管理 Web 页面。

必传参数：
  -pd <PD...>                  TiKV PD 地址（根级参数，逗号分隔；实例/索引/客户端注册区所在）

可选参数：
  --listen <host:port>         HTTP 监听地址（默认 0.0.0.0:18080）

功能覆盖 CLI 的 cluster/client/instance/key/version 命令：
  /api/cluster/list|status|index|purge   集群实例/健康/索引/清空
  /api/client/list|info                  SDK 客户端清单与环境
  /api/instance/segments                 实例 segment 汇总/明细
  /api/key/put|get|delete|stat|meta|list 对象读写删查
  /api/version                           版本

说明：
  未配置 -pd 时降级启动：仅静态页 / version / client info 可用（KV 相关端点报错）；
  危险操作（key delete、cluster purge）需二次确认（confirm=true），与 CLI --confirm 语义一致。`,
	Example: `  # 默认监听 0.0.0.0:18080
  taihu --pd 100.71.128.11:2379 web

  # 自定义监听地址
  taihu --pd 100.71.128.11:2379 web --listen 127.0.0.1:8080`,
	RunE: func(cmd *cobra.Command, args []string) error {
		listen, _ := cmd.Flags().GetString("listen")
		ctx := context.Background()

		// 注册区 KV：启动建一次复用（-pd 为空降级；连接失败报错退出）。
		var kv cluster.KV
		if global.pd != "" {
			var err error
			kv, err = connectKV(ctx)
			if err != nil {
				return err
			}
		}
		defer func() {
			if kv != nil {
				_ = kv.Close()
			}
		}()

		srv := internalweb.New(newWebService(kv))
		ln, err := net.Listen("tcp", listen)
		if err != nil {
			return fmt.Errorf("web listen %s: %w", listen, err)
		}
		httpSrv := &http.Server{Handler: srv.Handler()}

		// 优雅停机：收到 SIGINT/SIGTERM 关闭 HTTP 监听。
		go func() {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			<-sig
			log.Println("taihu web shutting down...")
			_ = httpSrv.Close()
		}()

		log.Printf("taihu web listening on %s (pd=%q)", listen, global.pd)
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	},
}

func init() {
	webCmd.Flags().String("listen", "0.0.0.0:18080", "HTTP 监听地址")
	rootCmd.AddCommand(webCmd)
}

// webService 实现 internalweb.Service，复用 cmd 包未导出的 CLI helper。
type webService struct {
	kv         cluster.KV // 启动时建立的注册区 KV（-pd 为空时为 nil）
	clientName string     // 本客户端标识（同机判定标注）
}

func newWebService(kv cluster.KV) *webService {
	return &webService{kv: kv, clientName: global.clientName}
}

// errPD 是 KV 类端点缺 -pd 的统一错误。
func errPD() error {
	return fmt.Errorf("-pd required: TiKV PD 地址列表（实例/索引/客户端注册区所在）")
}

func (w *webService) Version(ctx context.Context) (internalweb.VersionInfo, error) {
	return internalweb.VersionInfo{Version: version.String(), GoVersion: runtime.Version()}, nil
}

func (w *webService) ClusterList(ctx context.Context) ([]internalweb.InstanceRow, error) {
	if w.kv == nil {
		return nil, errPD()
	}
	all, err := listAllInstances(ctx, w.kv)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	rows := make([]internalweb.InstanceRow, 0, len(all))
	for _, inst := range all {
		status := "online"
		if !aliveness(inst, now) {
			status = "stale"
		}
		rows = append(rows, internalweb.InstanceRow{
			Name: inst.Name, Node: inst.Node, Addr: inst.Addr, ShmAddr: inst.ShmAddr,
			Status: status, Capacity: inst.Capacity, Used: inst.Used, Available: inst.Available,
			StartTime: inst.StartTime, LastHeartbeat: inst.LastHeartbeat,
			HeartbeatAge: time.Since(time.Unix(inst.LastHeartbeat, 0)).Round(time.Second).String(),
		})
	}
	return rows, nil
}

func (w *webService) ClusterStatus(ctx context.Context) ([]internalweb.StatusRow, error) {
	if w.kv == nil {
		return nil, errPD()
	}
	all, err := listAllInstances(ctx, w.kv)
	if err != nil {
		return nil, err
	}
	rows := make([]internalweb.StatusRow, 0, len(all))
	for _, inst := range all {
		row := internalweb.StatusRow{Name: inst.Name}
		if !aliveness(inst, time.Now()) {
			row.Error = "stale (no heartbeat)"
			rows = append(rows, row)
			continue
		}
		st, perr := pingInstance(ctx, inst)
		if perr == nil {
			row.TcpRTT = st.rtt.String()
		}
		if inst.ShmAddr != "" {
			row.ShmOK = "yes"
			if w.clientName != "" && inst.Node == w.clientName {
				row.ShmOK = "open"
			}
		} else {
			row.ShmOK = "-"
		}
		if perr != nil {
			row.Error = perr.Error()
		} else {
			sum, _, err := probeSegments(ctx, inst)
			if err != nil {
				row.Error = err.Error()
			} else {
				row.SegFree, row.SegActive, row.SegFull, row.SegReclaim = sum.Free, sum.Active, sum.Full, sum.Reclaiming
				row.CursorSeg, row.CursorOff, row.ObjectCount = sum.CursorSeg, sum.CursorOff, sum.ObjectCount
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (w *webService) ClusterIndex(ctx context.Context, prefix string) (internalweb.IndexResult, error) {
	if w.kv == nil {
		return internalweb.IndexResult{}, errPD()
	}
	start, end := []byte(cluster.IndexKeyPrefix), []byte(cluster.IndexKeyPrefix+"\xff")
	keys, values, err := w.kv.Scan(ctx, start, end, 10000)
	if err != nil {
		return internalweb.IndexResult{}, err
	}
	byInst := map[string]int{}
	filtered := 0
	for i, k := range keys {
		key := string(k[len(cluster.IndexKeyPrefix):])
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		filtered++
		byInst[string(values[i])]++
	}
	insts := make([]internalweb.IndexInstCount, 0, len(byInst))
	for name, n := range byInst {
		insts = append(insts, internalweb.IndexInstCount{Name: name, Count: n})
	}
	sort.Slice(insts, func(i, j int) bool { return insts[i].Count > insts[j].Count })
	return internalweb.IndexResult{Prefix: prefix, Total: filtered, Instances: insts}, nil
}

func (w *webService) ClusterPurge(ctx context.Context, confirm bool) (internalweb.PurgeResult, error) {
	if w.kv == nil {
		return internalweb.PurgeResult{}, errPD()
	}
	counts := internalweb.PurgeCounts{}
	for _, prefix := range []string{cluster.InstanceKeyPrefix, cluster.IndexKeyPrefix, cluster.ClientKeyPrefix} {
		ks, _, err := w.kv.Scan(ctx, []byte(prefix), []byte(prefix+"\xff"), 10000)
		if err != nil {
			return internalweb.PurgeResult{}, err
		}
		switch prefix {
		case cluster.InstanceKeyPrefix:
			counts.Instances = len(ks)
		case cluster.IndexKeyPrefix:
			counts.Index = len(ks)
		case cluster.ClientKeyPrefix:
			counts.Clients = len(ks)
		}
	}
	total := counts.Instances + counts.Index + counts.Clients
	res := internalweb.PurgeResult{Counts: counts, Total: total, Confirmed: confirm}
	if confirm && total > 0 {
		start, end := cluster.TaihuDataRange()
		if err := w.kv.DeleteRange(ctx, start, end); err != nil {
			return res, err
		}
		res.Deleted = total
	}
	return res, nil
}

func (w *webService) ClientList(ctx context.Context) (internalweb.ClientListResult, error) {
	if w.kv == nil {
		return internalweb.ClientListResult{}, errPD()
	}
	clients, err := cluster.ListClients(ctx, w.kv)
	if err != nil {
		return internalweb.ClientListResult{}, err
	}
	sort.Slice(clients, func(i, j int) bool {
		if clients[i].Node != clients[j].Node {
			return clients[i].Node < clients[j].Node
		}
		return clients[i].ID < clients[j].ID
	})
	now := time.Now()
	res := internalweb.ClientListResult{Rows: make([]internalweb.ClientRow, 0, len(clients))}
	for _, c := range clients {
		status := "online"
		if !c.Aliveness(now, 0) {
			status = "stale"
			res.Stale++
		} else {
			res.Online++
		}
		res.Total++
		res.Rows = append(res.Rows, internalweb.ClientRow{
			ID: c.ID, Node: c.Node, Host: c.Host, Pid: c.Pid, SDKVersion: c.SDKVersion,
			Addr: c.Addr, Status: status,
			HeartbeatAge:  time.Since(time.Unix(c.LastHeartbeat, 0)).Round(time.Second).String(),
			StartTime:     c.StartTime,
			LastHeartbeat: c.LastHeartbeat,
			Extra:         c.Extra,
		})
	}
	return res, nil
}

func (w *webService) ClientInfo(ctx context.Context) (internalweb.ClientInfoResult, error) {
	res := internalweb.ClientInfoResult{
		Version:   version.String(),
		GoVersion: runtime.Version(),
		Config:    map[string]string{"pd": global.pd, "node": global.clientName, "timeout": global.timeout.String()},
	}
	if w.kv == nil {
		res.KVStatus = "unset (-pd 缺省：无集群视角)"
		res.Config["pd"] = "(unset)"
		return res, nil
	}
	res.KVStatus = "tikv txnkv ok"
	res.KVCounts = map[string]int{}
	insts, err := cluster.ListInstances(ctx, w.kv)
	if err == nil {
		res.KVCounts["instances"] = len(insts)
	}
	if clients, err := cluster.ListClients(ctx, w.kv); err == nil {
		res.KVCounts["clients"] = len(clients)
	}
	if _, values, err := w.kv.Scan(ctx, []byte(cluster.IndexKeyPrefix), []byte(cluster.IndexKeyPrefix+"\xff"), 100000); err == nil {
		res.KVCounts["index_entries"] = len(values)
	}
	for _, inst := range insts {
		cr := internalweb.ConnRow{Name: inst.Name, Addr: inst.Addr}
		if inst.ShmAddr != "" {
			cr.Shm = "open"
		} else {
			cr.Shm = "-"
		}
		if w.clientName != "" && inst.Node == w.clientName {
			cr.Local = true
		}
		if pi, err := pingInstance(ctx, inst); err != nil {
			cr.Error = err.Error()
		} else {
			cr.RTT = pi.rtt.String()
		}
		res.Connectivity = append(res.Connectivity, cr)
	}
	return res, nil
}

func (w *webService) InstanceSegments(ctx context.Context, instName string, detail bool) ([]internalweb.SegmentResult, error) {
	if w.kv == nil {
		return nil, errPD()
	}
	all, err := listAllInstances(ctx, w.kv)
	if err != nil {
		return nil, err
	}
	var insts []cluster.InstanceInfo
	if instName != "" {
		inst, err := resolveInstance(all, instName)
		if err != nil {
			return nil, err
		}
		insts = []cluster.InstanceInfo{inst}
	} else {
		insts = all
	}
	results := make([]internalweb.SegmentResult, 0, len(insts))
	for _, inst := range insts {
		res := internalweb.SegmentResult{Name: inst.Name, Addr: inst.Addr}
		if !aliveness(inst, time.Now()) {
			res.Error = "stale (no heartbeat)"
		} else {
			sum, entries, err := probeSegments(ctx, inst)
			if err != nil {
				res.Error = err.Error()
			} else {
				res.Summary = internalweb.SegmentSummary{
					Total: sum.Total, Free: sum.Free, Active: sum.Active, Full: sum.Full,
					Reclaiming: sum.Reclaiming, CursorSeg: sum.CursorSeg, CursorOff: sum.CursorOff,
					SegSize: sum.SegSize, ObjectCount: sum.ObjectCount,
				}
				if detail {
					for _, e := range entries {
						res.Segments = append(res.Segments, internalweb.SegmentEntry{
							SegmentID: e.SegmentID, State: stateName(e.State),
							AliveCount: e.AliveCount, ReclaimSeq: e.ReclaimSeq,
						})
					}
				}
			}
		}
		results = append(results, res)
	}
	return results, nil
}

// prepareStore 解析 key 命令寻址并建立数据面客户端（等价 prepareKeyStore，KV 复用启动连接）。
func (w *webService) prepareStore(ctx context.Context, addr, instName string) (objectStore, *keyTarget, func(), error) {
	if addr == "" && instName == "" && w.kv == nil {
		return nil, nil, nil, errStringfNoTarget()
	}
	var all []cluster.InstanceInfo
	if w.kv != nil {
		var err error
		all, err = listAllInstances(ctx, w.kv)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	store, target, err := resolveKeyTarget(ctx, addr, instName, w.kv, all)
	if err != nil {
		return nil, nil, nil, err
	}
	return store, target, func() { _ = store.Close() }, nil
}

func errStringfNoTarget() error {
	return fmt.Errorf("需要一个目标：-addr ADDR / -instance NAME（配合 -pd）/ -pd（集群路由）")
}

func (w *webService) KeyStat(ctx context.Context, key, addr, instName string) (internalweb.KeyStatResult, error) {
	store, target, cleanup, err := w.prepareStore(ctx, addr, instName)
	if err != nil {
		return internalweb.KeyStatResult{}, err
	}
	defer cleanup()
	sz, err := store.Stat(ctx, key)
	if err != nil {
		return internalweb.KeyStatResult{}, err
	}
	return internalweb.KeyStatResult{Key: key, Size: sz, Instance: targetName(target)}, nil
}

func (w *webService) KeyMeta(ctx context.Context, key, addr, instName string) (internalweb.KeyMetaResult, error) {
	store, _, cleanup, err := w.prepareStore(ctx, addr, instName)
	if err != nil {
		return internalweb.KeyMetaResult{}, err
	}
	defer cleanup()
	rc, ok := store.(*rpcclient.Storage)
	if !ok {
		return internalweb.KeyMetaResult{}, fmt.Errorf("meta 仅支持直连（-addr / -instance），不走集群路由")
	}
	m, err := rc.Meta(ctx, key)
	if err != nil {
		return internalweb.KeyMetaResult{}, err
	}
	return internalweb.KeyMetaResult{Key: key, Size: m.Size, SegmentID: m.SegmentID, Offset: m.Offset}, nil
}

func (w *webService) KeyList(ctx context.Context, prefix string, limit int, addr, instName string) (internalweb.KeyListResult, error) {
	store, _, cleanup, err := w.prepareStore(ctx, addr, instName)
	if err != nil {
		return internalweb.KeyListResult{}, err
	}
	defer cleanup()
	rc, ok := store.(*rpcclient.Storage)
	if !ok {
		return internalweb.KeyListResult{}, fmt.Errorf("list 仅支持直连（-addr / -instance），不走集群路由")
	}
	keys, err := rc.ListKeys(ctx, prefix)
	if err != nil {
		return internalweb.KeyListResult{}, err
	}
	total := len(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	return internalweb.KeyListResult{Prefix: prefix, Total: total, Keys: keys}, nil
}

func (w *webService) KeyGet(ctx context.Context, key string, off, size int64, addr, instName string) ([]byte, func(), error) {
	store, _, cleanup, err := w.prepareStore(ctx, addr, instName)
	if err != nil {
		return nil, nil, err
	}
	data, rel, err := store.Get(ctx, key, off, size)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return data, func() {
		rel()
		cleanup()
	}, nil
}

func (w *webService) KeyPut(ctx context.Context, key string, data []byte, size int64, addr, instName string) (internalweb.KeyWriteResult, error) {
	if size < 0 || size > int64(len(data)) {
		size = int64(len(data))
	}
	store, target, cleanup, err := w.prepareStore(ctx, addr, instName)
	if err != nil {
		return internalweb.KeyWriteResult{}, err
	}
	defer cleanup()
	if err := store.Put(ctx, key, size, data); err != nil {
		return internalweb.KeyWriteResult{}, err
	}
	return internalweb.KeyWriteResult{Key: key, Size: size, Instance: targetName(target)}, nil
}

func (w *webService) KeyDelete(ctx context.Context, key, addr, instName string) (internalweb.KeyWriteResult, error) {
	store, target, cleanup, err := w.prepareStore(ctx, addr, instName)
	if err != nil {
		return internalweb.KeyWriteResult{}, err
	}
	defer cleanup()
	if err := store.Delete(ctx, key); err != nil {
		return internalweb.KeyWriteResult{}, err
	}
	return internalweb.KeyWriteResult{Key: key, Instance: targetName(target)}, nil
}
