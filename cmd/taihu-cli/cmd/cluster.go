package cmd

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/pkg/taihu"
)

// clusterCmd 集群命令父节点。
var clusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "集群信息：实例列表 / 健康体检 / 索引概览",
}

// clusterListCmd 实例列表。
var clusterListCmd = &cobra.Command{
	Use:   "list",
	Short: "列出注册区全部实例（含心跳超时的僵尸）",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requirePD(); err != nil {
			return err
		}
		ctx, cancel := ctxWithTimeout(cmd.Context())
		defer cancel()

		kv, err := connectKV(ctx)
		if err != nil {
			return err
		}
		defer kv.Close()

		all, err := listAllInstances(ctx, kv)
		if err != nil {
			return fmt.Errorf("list instances: %w", err)
		}

		now := time.Now()
		type row struct {
			Name          string `json:"name"`
			Node          string `json:"node"`
			Addr          string `json:"addr"`
			ShmAddr       string `json:"shm_addr,omitempty"`
			Status        string `json:"status"`
			Capacity      int64  `json:"capacity"`
			Used          int64  `json:"used"`
			Available     int64  `json:"available"`
			StartTime     int64  `json:"start_time"`
			LastHeartbeat int64  `json:"last_heartbeat"`
			HeartbeatAge  string `json:"heartbeat_age,omitempty"`
		}
		rows := make([]row, 0, len(all))
		for _, inst := range all {
			age := time.Since(time.Unix(inst.LastHeartbeat, 0)).Round(time.Second)
			status := "online"
			if !aliveness(inst, now) {
				status = "stale"
			}
			rows = append(rows, row{
				Name: inst.Name, Node: inst.Node, Addr: inst.Addr, ShmAddr: inst.ShmAddr,
				Status: status, Capacity: inst.Capacity, Used: inst.Used, Available: inst.Available,
				StartTime: inst.StartTime, LastHeartbeat: inst.LastHeartbeat, HeartbeatAge: age.String(),
			})
		}

		if global.json {
			printJSON(rows)
			return nil
		}
		if len(rows) == 0 {
			fmt.Println("no instances registered")
			return nil
		}
		fmt.Printf("%-12s %-12s %-20s %-16s %-7s %-10s %-10s %-10s %s\n",
			"NAME", "NODE", "ADDR", "SHM", "STATUS", "CAPACITY", "USED", "AVAILABLE", "HEARTBEAT_AGE")
		for _, r := range rows {
			fmt.Printf("%-12s %-12s %-20s %-16s %-7s %-10s %-10s %-10s %s\n",
				r.Name, r.Node, r.Addr, r.ShmAddr, r.Status,
				humanBytes(r.Capacity), humanBytes(r.Used), humanBytes(r.Available), r.HeartbeatAge)
		}
		return nil
	},
}

// clusterStatusCmd 实例健康/水位/连通性体检。
var clusterStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "逐实例探测连通性(RTT)、段汇总与水位",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requirePD(); err != nil {
			return err
		}
		ctx, cancel := ctxWithTimeout(cmd.Context())
		defer cancel()

		kv, err := connectKV(ctx)
		if err != nil {
			return err
		}
		defer kv.Close()

		all, err := listAllInstances(ctx, kv)
		if err != nil {
			return fmt.Errorf("list instances: %w", err)
		}
		if len(all) == 0 {
			fmt.Println("no instances registered")
			return nil
		}

		type statusRow struct {
			Name        string `json:"name"`
			TcpRTT      string `json:"tcp_rtt,omitempty"`
			ShmOK       string `json:"shm_ok,omitempty"`
			SegFree     int64  `json:"seg_free"`
			SegActive   int64  `json:"seg_active"`
			SegFull     int64  `json:"seg_full"`
			SegReclaim  int64  `json:"seg_reclaiming"`
			CursorSeg   int64  `json:"cursor_seg"`
			CursorOff   int64  `json:"cursor_off"`
			ObjectCount int64  `json:"object_count"`
			Error       string `json:"error,omitempty"`
		}

		rows := make([]statusRow, 0, len(all))
		for _, inst := range all {
			row := statusRow{Name: inst.Name}
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
				if global.clientName != "" && inst.Node == global.clientName {
					// 同机 shm 可达性仅做字段标注（CLI 数据面统一走 TCP）。
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
					row.SegFree = sum.Free
					row.SegActive = sum.Active
					row.SegFull = sum.Full
					row.SegReclaim = sum.Reclaiming
					row.CursorSeg = sum.CursorSeg
					row.CursorOff = sum.CursorOff
					row.ObjectCount = sum.ObjectCount
				}
			}
			rows = append(rows, row)
		}

		if global.json {
			printJSON(rows)
			return nil
		}
		fmt.Printf("%-12s %-10s %-6s %-8s %-9s %-8s %-13s %-16s %-10s %s\n",
			"instance", "tcp_rtt", "shm", "seg_free", "seg_active", "seg_full", "seg_reclaim", "cursor(seg/off)", "objects", "error")
		for _, r := range rows {
			off := ""
			if r.Error == "" {
				off = humanBytes(r.CursorOff)
			}
			fmt.Printf("%-12s %-10s %-6s %-8d %-9d %-8d %-13d %-16s %-10d %s\n",
				r.Name, r.TcpRTT, r.ShmOK, r.SegFree, r.SegActive, r.SegFull, r.SegReclaim,
				fmt.Sprintf("%d/%s", r.CursorSeg, off), r.ObjectCount, r.Error)
		}
		return nil
	},
}

// clusterIndexCmd key→实例 索引概览。
var clusterIndexCmd = &cobra.Command{
	Use:   "index",
	Short: "扫描 /taihu/index/ 索引区，按实例归组计数",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requirePD(); err != nil {
			return err
		}
		prefix := cmd.Flag("prefix").Value.String()
		ctx, cancel := ctxWithTimeout(cmd.Context())
		defer cancel()

		kv, err := connectKV(ctx)
		if err != nil {
			return err
		}
		defer kv.Close()

		start, end := []byte(cluster.IndexKeyPrefix), []byte(cluster.IndexKeyPrefix+"\xff")
		// TiKV rawkv Scan 受 MaxRawKVScanLimit(10240) 上限约束，取 10000。
		keys, values, err := kv.Scan(ctx, start, end, 10000)
		if err != nil {
			return fmt.Errorf("index scan: %w", err)
		}

		byInst := map[string]int{}
		filtered := 0
		for i, k := range keys {
			key := string(k[len(cluster.IndexKeyPrefix):])
			if prefix != "" && len(key) < len(prefix) || (len(key) >= len(prefix) && key[:len(prefix)] != prefix) {
				continue
			}
			filtered++
			byInst[string(values[i])]++
		}
		if prefix != "" {
			fmt.Printf("index entries matching %q: %d\n", prefix, filtered)
		} else {
			fmt.Printf("index entries: %d\n", filtered)
		}
		if global.json {
			printJSON(byInst)
			return nil
		}
		// 按实例数降序输出。
		type idxKV struct {
			name string
			n    int
		}
		insts := make([]idxKV, 0, len(byInst))
		for name, n := range byInst {
			insts = append(insts, idxKV{name, n})
		}
		sort.Slice(insts, func(i, j int) bool { return insts[i].n > insts[j].n })
		for _, it := range insts {
			fmt.Printf("  -> %s: %d\n", it.name, it.n)
		}
		return nil
	},
}

// pingInfo 一次连通性探测结果。
type pingInfo struct {
	rtt time.Duration
}

// pingInstance 对实例做 Dial+Ping 探测。
func pingInstance(ctx context.Context, inst cluster.InstanceInfo) (pingInfo, error) {
	c, err := dialInstance(ctx, inst)
	if err != nil {
		return pingInfo{}, err
	}
	defer c.Close()
	ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rtt, _, err := c.Ping(ctx2)
	if err != nil {
		return pingInfo{}, err
	}
	return pingInfo{rtt: rtt}, nil
}

// probeSegments 对实例拉取段汇总（明细可空）。
func probeSegments(ctx context.Context, inst cluster.InstanceInfo) (taihu.SegmentSummary, []taihu.SegmentEntry, error) {
	c, err := dialInstance(ctx, inst)
	if err != nil {
		return taihu.SegmentSummary{}, nil, err
	}
	defer c.Close()
	ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.Segments(ctx2)
}

func init() {
	clusterCmd.AddCommand(clusterListCmd, clusterStatusCmd, clusterIndexCmd)
	clusterIndexCmd.Flags().String("prefix", "", "只统计 key 前缀匹配的索引条目")
}
