// Command taihu-cluster-check 集群路由验证工具（临时诊断程序，不提交）。
//
// 验证设计文档"首写本地 + 索引锚定 + 回源兜底"路由：
//   - mode=write：用 rpccluster 写 N 个 key，随后直连各实例 Stat 统计 key 分布，
//     验证"本地优先"（同 node 实例承接全部写入）；
//   - mode=read：按 key 逐个走 本地直查 → 索引定位远端 → 回源 三阶段，
//     统计各阶段命中（验证读路由与回源兜底）；
//   - mode=index：扫描 TiKV /taihu/index/ 前缀，验证异步索引落盘条数。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/pkg/rpcclient"
	"github.com/liucxer/taihu/pkg/rpccluster"
	"github.com/liucxer/taihu/pkg/taihu"
)

func main() {
	var (
		pd      = flag.String("pd", "", "comma-separated TiKV PD addrs")
		node    = flag.String("node", "", "this client node id")
		mode    = flag.String("mode", "", "write|read|index")
		prefix  = flag.String("prefix", "rk", "key prefix")
		count   = flag.Int("count", 100, "key count")
		size    = flag.Int("size", 1024*1024, "object size bytes")
		remote  = flag.Bool("remote", false, "read: force skip local (simulate remote node read)")
		noCache = flag.Bool("no-source", false, "read: disable source fallback")
	)
	flag.Parse()

	if *pd == "" || *node == "" {
		fmt.Fprintln(os.Stderr, "-pd and -node required")
		os.Exit(2)
	}

	ctx := context.Background()
	kv, err := cluster.NewTiKVKV(ctx, splitCSV(*pd))
	if err != nil {
		fmt.Fprintf(os.Stderr, "tikv: %v\n", err)
		os.Exit(1)
	}
	defer kv.Close()

	// 注册快照（供直连各实例 Stat 用）。
	all, err := cluster.ListInstances(ctx, kv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list instances: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("instances: %d\n", len(all))
	for _, inst := range all {
		fmt.Printf("  %-12s node=%-15s addr=%-20s used=%d cap=%d\n", inst.Name, inst.Node, inst.Addr, inst.Used, inst.Capacity)
	}

	sourceGetter := func(ctx context.Context, key string) ([]byte, error) {
		return []byte("SOURCE_DATA_" + key), nil
	}

	store, err := rpccluster.NewCluster(rpccluster.ClusterConfig{
		KV:    kv,
		Node:  *node,
		Source: sourceGetter,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	keys := make([]string, 0, *count)
	for i := 0; i < *count; i++ {
		keys = append(keys, fmt.Sprintf("%s-%04d", *prefix, i))
	}

	payload := make([]byte, *size)
	for i := range payload {
		payload[i] = byte(i)
	}

	switch *mode {
	case "write":
		start := time.Now()
		for _, k := range keys {
			if err := store.Put(ctx, k, int64(len(payload)), payload); err != nil {
				fmt.Fprintf(os.Stderr, "put %s: %v\n", k, err)
				os.Exit(1)
			}
		}
		fmt.Printf("write %d keys x %dB done in %v (%.1f ops/s)\n", *count, *size, time.Since(start), float64(*count)/time.Since(start).Seconds())

		// 直连各实例统计 key 分布（验证本地优先）。
		dist := map[string]int{}
		for _, inst := range all {
			c, err := rpcclient.DialPool(ctx, inst.Addr, 1)
			if err != nil {
				fmt.Printf("  dial %s: %v\n", inst.Name, err)
				continue
			}
			n := 0
			for _, k := range keys {
				if _, err := c.Stat(ctx, k); err == nil {
					n++
				}
			}
			dist[inst.Name] = n
			_ = c.Close()
		}
		fmt.Println("key distribution by instance (local-first expected: all on node", *node + "):")
		for _, inst := range all {
			mark := " (LOCAL)" 
			if inst.Node != *node {
				mark = ""
			}
			fmt.Printf("  %-12s %d keys%s\n", inst.Name, dist[inst.Name], mark)
		}

	case "read":
		// 显式三阶段统计（不依赖 store.Get 的本地优先内部路径）：
		//   localHit: 直连本地实例读命中（本地直查，优先 shm）
		//   remoteHit: KV 索引定位 → 目标实例读命中（索引锚定/远端读，TCP）
		//   sourceHit: 全 miss → 回源回调（回源兜底）
		var localHit, remoteHit, sourceHit, miss int
		start := time.Now()
		for _, k := range keys {
			// 阶段1：本地实例直查
			found := false
			if !*remote {
				for _, inst := range all {
					if inst.Node != *node {
						continue
					}
					c, err := dialFor(inst, *node)
					if err != nil {
						continue
					}
					_, _, err = c.Get(ctx, k, 0, -1)
					_ = c.Close()
					if err == nil {
						localHit++
						found = true
						break
					}
					if err != taihu.ErrNotFound {
						break
					}
				}
			}
			if found {
				continue
			}
			// 阶段2：索引定位 → 目标实例读
			if v, err := kv.Get(ctx, cluster.IndexKey(k)); err == nil && len(v) > 0 {
				target := string(v)
				for _, inst := range all {
					if inst.Name == target {
						c, err := dialFor(inst, *node)
						if err == nil {
							_, _, err = c.Get(ctx, k, 0, -1)
							_ = c.Close()
						}
						if err == nil {
							remoteHit++
							found = true
						}
						break
					}
				}
			}
			if found {
				continue
			}
			// 阶段3：回源兜底
			if !*noCache {
				data, err := sourceGetter(ctx, k)
				if err == nil && len(data) > 0 {
					sourceHit++
					continue
				}
			}
			miss++
		}
		fmt.Printf("read %d keys in %v: localHit=%d remoteHit=%d sourceHit=%d miss=%d\n",
			*count, time.Since(start), localHit, remoteHit, sourceHit, miss)

	case "index":
		start, end := []byte(cluster.IndexKeyPrefix), []byte(cluster.IndexKeyPrefix+"\xff")
		keys, values, err := kv.Scan(ctx, start, end, 10000)
		if err != nil {
			fmt.Fprintf(os.Stderr, "index scan: %v\n", err)
			os.Exit(1)
		}
		byInst := map[string]int{}
		for _, v := range values {
			byInst[string(v)]++
		}
		fmt.Printf("index entries: %d (expected %d)\n", len(keys), *count)
		for name, n := range byInst {
			fmt.Printf("  -> %s: %d\n", name, n)
		}

	default:
		fmt.Fprintln(os.Stderr, "unknown mode", *mode)
		os.Exit(2)
	}
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// dialFor 按实例选择数据面连接：同 node 且开放 shm 时走共享内存（零拷贝），
// 否则（跨节点/未开放 shm）走 netpoll TCP。返回连接供单次使用，需调用方 Close。
func dialFor(inst cluster.InstanceInfo, node string) (*rpcclient.Storage, error) {
	if inst.Node == node && inst.ShmAddr != "" {
		if c, err := rpcclient.DialShm(context.Background(), inst.ShmAddr); err == nil {
			return c, nil
		}
	}
	return rpcclient.DialPool(context.Background(), inst.Addr, 1)
}
