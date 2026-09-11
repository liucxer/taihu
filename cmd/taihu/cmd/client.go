package cmd

import (
	"fmt"
	"runtime"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/version"
)

// clientCmd client 命令父节点。
var clientCmd = &cobra.Command{
	Use:   "client",
	Short: "客户端信息：SDK 注册客户端清单 / 本工具环境信息",
}

// clientListCmd SDK 注册客户端清单（心跳存活状态）。
var clientListCmd = &cobra.Command{
	Use:   "list",
	Short: "列出注册的 SDK 客户端（/taihu/clients/）",
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

		clients, err := cluster.ListClients(ctx, kv)
		if err != nil {
			return fmt.Errorf("list clients: %w", err)
		}
		sort.Slice(clients, func(i, j int) bool {
			if clients[i].Node != clients[j].Node {
				return clients[i].Node < clients[j].Node
			}
			return clients[i].ID < clients[j].ID
		})

		now := time.Now()
		type row struct {
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
		rows := make([]row, 0, len(clients))
		online, stale := 0, 0
		for _, c := range clients {
			status := "online"
			if !c.Aliveness(now, 0) {
				status = "stale"
				stale++
			} else {
				online++
			}
			rows = append(rows, row{
				ID: c.ID, Node: c.Node, Host: c.Host, Pid: c.Pid, SDKVersion: c.SDKVersion,
				Addr: c.Addr, Status: status,
				HeartbeatAge: time.Since(time.Unix(c.LastHeartbeat, 0)).Round(time.Second).String(),
				StartTime:    c.StartTime, LastHeartbeat: c.LastHeartbeat, Extra: c.Extra,
			})
		}

		if global.json {
			printJSON(rows)
			return nil
		}
		if len(rows) == 0 {
			fmt.Println("no clients registered")
			return nil
		}
		fmt.Printf("%-18s %-12s %-12s %-7s %-16s %-7s %s\n",
			"ID", "NODE", "HOST", "PID", "SDK_VER", "STATUS", "HEARTBEAT_AGE")
		for _, r := range rows {
			fmt.Printf("%-18s %-12s %-12s %-7d %-16s %-7s %s\n",
				r.ID, r.Node, r.Host, r.Pid, r.SDKVersion, r.Status, r.HeartbeatAge)
		}
		fmt.Printf("%d registered, %d online, %d stale\n", len(rows), online, stale)
		return nil
	},
}

// clientInfoCmd 本工具自身/环境信息。
var clientInfoCmd = &cobra.Command{
	Use:   "info",
	Short: "显示工具版本、配置、KV 后端状态与实例连通性",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := ctxWithTimeout(cmd.Context())
		defer cancel()

		type connRow struct {
			Name  string `json:"name"`
			Addr  string `json:"addr"`
			Shm   string `json:"shm,omitempty"`
			RTT   string `json:"rtt,omitempty"`
			Error string `json:"error,omitempty"`
			Local bool   `json:"local,omitempty"`
		}
		info := struct {
			Version      string            `json:"version"`
			GoVersion    string            `json:"go_version"`
			Config       map[string]string `json:"config"`
			KVStatus     string            `json:"kv_status"`
			KVCounts     map[string]int    `json:"kv_counts,omitempty"`
			Connectivity []connRow         `json:"connectivity,omitempty"`
		}{
			Version:   version.String(),
			GoVersion: runtime.Version(),
			Config: map[string]string{
				"pd": global.pd, "node": global.node, "timeout": global.timeout.String(),
			},
		}

		if global.pd == "" {
			info.KVStatus = "unset (-pd 缺省：无集群视角)"
			info.Config["pd"] = "(unset)"
		} else {
			kv, err := connectKV(ctx)
			if err != nil {
				info.KVStatus = "error: " + err.Error()
			} else {
				defer kv.Close()
				info.KVStatus = "tikv rawkv ok"
				info.KVCounts = map[string]int{}
				insts, err := cluster.ListInstances(ctx, kv)
				if err == nil {
					info.KVCounts["instances"] = len(insts)
				}
				clients, err := cluster.ListClients(ctx, kv)
				if err == nil {
					info.KVCounts["clients"] = len(clients)
				}
				_, values, err := kv.Scan(ctx, []byte(cluster.IndexKeyPrefix), []byte(cluster.IndexKeyPrefix+"\xff"), 100000)
				if err == nil {
					info.KVCounts["index_entries"] = len(values)
				}
				// 连通性表：逐实例 Dial+Ping。
				for _, inst := range insts {
					cr := connRow{Name: inst.Name, Addr: inst.Addr}
					if inst.ShmAddr != "" {
						cr.Shm = "open"
					} else {
						cr.Shm = "-"
					}
					if global.node != "" && inst.Node == global.node {
						cr.Local = true
					}
					if pi, err := pingInstance(ctx, inst); err != nil {
						cr.Error = err.Error()
					} else {
						cr.RTT = pi.rtt.String()
					}
					info.Connectivity = append(info.Connectivity, cr)
				}
			}
		}

		if global.json {
			printJSON(info)
			return nil
		}
		fmt.Printf("taihu %s (%s)\n", info.Version, info.GoVersion)
		fmt.Printf("config: pd=%s node=%s timeout=%s\n", global.pd, global.node, global.timeout)
		fmt.Printf("kv backend: %s", info.KVStatus)
		if len(info.KVCounts) > 0 {
			fmt.Printf(" (instances: %d, clients: %d, index entries: %d)",
				info.KVCounts["instances"], info.KVCounts["clients"], info.KVCounts["index_entries"])
		}
		fmt.Println()
		if len(info.Connectivity) > 0 {
			fmt.Println("connectivity:")
			for _, cr := range info.Connectivity {
				local := ""
				if cr.Local {
					local = " (LOCAL)"
				}
				if cr.Error != "" {
					fmt.Printf("  %-12s err: %s%s\n", cr.Name, cr.Error, local)
					continue
				}
				fmt.Printf("  %-12s tcp %s (shm %s)%s\n", cr.Name, cr.RTT, cr.Shm, local)
			}
		}
		return nil
	},
}

func init() {
	clientCmd.AddCommand(clientListCmd, clientInfoCmd)
}
