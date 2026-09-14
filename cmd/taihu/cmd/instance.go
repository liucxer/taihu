package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/internal/storage"
)

// instanceCmd instance 命令父节点。
var instanceCmd = &cobra.Command{
	Use:   "instance",
	Short: "实例信息：segment 汇总与明细",
	Example: `  # 查询全部在线实例的 segment 汇总
  taihu --pd 100.71.128.11:2379 instance segments`,
}

// segResult JSON 形态的单实例段汇总+明细。
type segResult struct {
	Name     string               `json:"name"`
	Addr     string               `json:"addr"`
	Summary  taihu.SegmentSummary `json:"summary"`
	Segments []taihu.SegmentEntry `json:"segments,omitempty"`
	Error    string               `json:"error,omitempty"`
}

// instanceSegmentsCmd 查询每个实例的 segment 信息（汇总 + -detail 明细）。
var instanceSegmentsCmd = &cobra.Command{
	Use:   "segments",
	Short: "查询实例的 segment 汇总（-detail 出明细）",
	Example: `  # 全部实例 segment 汇总
  taihu --pd 100.71.128.11:2379 instance segments

  # 指定实例 + 每段明细
  taihu --pd 100.71.128.11:2379 instance segments -instance TAIHU-0 -detail`,
	RunE: func(cmd *cobra.Command, args []string) error {
		instName, _ := cmd.Flags().GetString("instance")
		detail, _ := cmd.Flags().GetBool("detail")

		ctx, cancel := ctxWithTimeout(context.Background())
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
		var insts []cluster.InstanceInfo
		if instName != "" {
			inst, err := resolveInstance(all, instName)
			if err != nil {
				return err
			}
			insts = []cluster.InstanceInfo{inst}
		} else {
			insts = all
		}

		results := make([]segResult, 0, len(insts))
		for _, inst := range insts {
			res := segResult{Name: inst.Name, Addr: inst.Addr}
			if !aliveness(inst, time.Now()) {
				res.Error = "stale (no heartbeat)"
			} else {
				sum, entries, err := probeSegments(ctx, inst)
				if err != nil {
					res.Error = err.Error()
				} else {
					res.Summary = sum
					res.Segments = entries
				}
			}
			results = append(results, res)
		}

		if global.json {
			printJSON(results)
			return nil
		}
		for _, res := range results {
			fmt.Printf("%s (%s):\n", res.Name, res.Addr)
			if res.Error != "" {
				fmt.Printf("  ERROR: %s\n", res.Error)
				continue
			}
			fmt.Printf("  total=%d free=%d active=%d full=%d reclaiming=%d\n",
				res.Summary.Total, res.Summary.Free, res.Summary.Active,
				res.Summary.Full, res.Summary.Reclaiming)
			fmt.Printf("  cursor=seg %d off %s  object_count=%d\n",
				res.Summary.CursorSeg, humanBytes(res.Summary.CursorOff), res.Summary.ObjectCount)
			if detail {
				fmt.Printf("  segID  state      alive  reclaim_seq\n")
				for _, e := range res.Segments {
					if e.State == metastore.SegmentStateFree && e.AliveCount == 0 && e.ReclaimSeq == 0 {
						continue // 空 Free 段不展示（默认 2048 段全打太噪）
					}
					fmt.Printf("  %-7d %-9s %-6d %d\n", e.SegmentID, stateName(e.State), e.AliveCount, e.ReclaimSeq)
				}
			}
		}
		return nil
	},
}

func init() {
	instanceCmd.AddCommand(instanceSegmentsCmd)
	instanceSegmentsCmd.Flags().String("instance", "", "只查询指定实例（缺省=全部在线）")
	instanceSegmentsCmd.Flags().Bool("detail", false, "输出每段明细")
}

// stateName 段状态字面名。
func stateName(s metastore.SegmentState) string {
	switch s {
	case metastore.SegmentStateFree:
		return "free"
	case metastore.SegmentStateActive:
		return "active"
	case metastore.SegmentStateFull:
		return "full"
	case metastore.SegmentStateReclaiming:
		return "reclaiming"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}
