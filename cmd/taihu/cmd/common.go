package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/pkg/rpcclient"
)

// 共享 helper：KV 连接、实例枚举/寻址、数据面拨号、输出格式化。

// requirePD 校验 -pd 已配置；集群类命令入口调用。
func requirePD() error {
	if global.pd == "" {
		return fmt.Errorf("-pd required: TiKV PD 地址列表（实例/索引/客户端注册区所在），例如 -pd 10.0.0.10:2379,10.0.0.11:2379")
	}
	return nil
}

// connectKV 连接注册区 KV（TiKV TxnKV）。client-go 对不可达 PD 的发现/重试不服从
// ctx，故包一层 goroutine + select 兜底：超过 ctx（-timeout）立即报错退出。
func connectKV(ctx context.Context) (cluster.KV, error) {
	type result struct {
		kv  cluster.KV
		err error
	}
	ch := make(chan result, 1)
	go func() {
		kv, err := cluster.NewTiKVKV(ctx, splitCSV(global.pd), cluster.TLSConfig{
			CA: global.tikvCA, Cert: global.tikvCert, Key: global.tikvKey,
		})
		ch <- result{kv, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("tikv connect: %w", r.err)
		}
		return r.kv, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("tikv connect: %w (pd %s)", ctx.Err(), global.pd)
	}
}

// splitCSV 拆逗号分隔列表（忽略空段）。
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// listAllInstances 枚举注册区全部实例（含心跳超时，状态由调用方判定）。
func listAllInstances(ctx context.Context, kv cluster.KV) ([]cluster.InstanceInfo, error) {
	all, err := cluster.ListInstances(ctx, kv)
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Node != all[j].Node {
			return all[i].Node < all[j].Node
		}
		return all[i].Name < all[j].Name
	})
	return all, nil
}

// aliveness 按心跳超时判定在线（与 InstanceInfo.Aliveness 一致，默认 5s）。
func aliveness(inst cluster.InstanceInfo, now time.Time) bool {
	return inst.Aliveness(now, 0)
}

// resolveInstance 按名字寻址实例；name 为空返回 err。
func resolveInstance(all []cluster.InstanceInfo, name string) (cluster.InstanceInfo, error) {
	for _, inst := range all {
		if inst.Name == name {
			return inst, nil
		}
	}
	return cluster.InstanceInfo{}, fmt.Errorf("instance %q not found", name)
}

// dialInstance 建立到实例数据面的 TCP 连接（CLI 跨节点运行，统一走 TCP；
// 同机 shm 优先留给 SDK 数据面，CLI 不感知）。
func dialInstance(ctx context.Context, inst cluster.InstanceInfo) (*rpcclient.Storage, error) {
	if inst.Addr == "" {
		return nil, fmt.Errorf("instance %s has no addr", inst.Name)
	}
	return rpcclient.DialPool(ctx, inst.Addr, 1)
}

// printJSON 机器可读输出（-json）。
func printJSON(v interface{}) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "json:", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}

// humanBytes 字节数转人类可读（GB/MB/KB，2 位小数）。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
