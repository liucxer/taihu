package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/pkg/rpcclient"
	"github.com/liucxer/taihu/pkg/rpccluster"
)

// keyCmd key 操作父节点。
var keyCmd = &cobra.Command{
	Use:   "key",
	Short: "key 操作：put / get / delete / stat / meta / list",
}

// keyTarget 本次 key 命令命中的目标实例信息（nil=集群路由模式）。
type keyTarget struct {
	inst *cluster.InstanceInfo
}

// resolveKeyTarget 解析 -addr / -instance / -pd 三类寻址。
// 返回 (store, target, err)：cluster 模式 target=nil。
func resolveKeyTarget(ctx context.Context, addr, instName string, kv cluster.KV, all []cluster.InstanceInfo) (objectStore, *keyTarget, error) {
	if addr != "" {
		c, err := rpcclient.DialPool(ctx, addr, 1)
		if err != nil {
			return nil, nil, fmt.Errorf("dial %s: %w", addr, err)
		}
		return c, &keyTarget{inst: &cluster.InstanceInfo{Name: addr, Addr: addr}}, nil
	}
	if instName != "" {
		if kv == nil {
			return nil, nil, fmt.Errorf("-instance requires -pd")
		}
		inst, err := resolveInstance(all, instName)
		if err != nil {
			return nil, nil, err
		}
		c, err := dialInstance(ctx, inst)
		if err != nil {
			return nil, nil, err
		}
		return c, &keyTarget{inst: &inst}, nil
	}
	// 集群路由模式（rpccluster）：put 选实例、get/delete/stat 按索引定位。
	store, err := rpccluster.NewCluster(rpccluster.ClusterConfig{KV: kv, ClientName: global.clientName})
	if err != nil {
		return nil, nil, err
	}
	return store, nil, nil
}

// objectStore 数据面统一接口（rpcclient.Storage / rpccluster.Storage 均满足）。
type objectStore interface {
	Put(ctx context.Context, key string, size int64, in []byte) error
	Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error)
	Delete(ctx context.Context, key string) error
	Stat(ctx context.Context, key string) (int64, error)
	Close() error
}

// prepareKeyStore 打开 KV（如需要）并解析目标，返回 store 与实例寻址信息。
func prepareKeyStore(ctx context.Context) (objectStore, *keyTarget, func(), error) {
	addr, _ := keyCmd.Flags().GetString("addr")
	instName, _ := keyCmd.Flags().GetString("instance")
	if addr == "" && instName == "" && global.pd == "" {
		return nil, nil, nil, fmt.Errorf("需要一个目标：-addr ADDR / -instance NAME（配合 -pd）/ -pd（集群路由）")
	}
	var (
		kv  cluster.KV
		all []cluster.InstanceInfo
	)
	if global.pd != "" {
		var err error
		kv, err = connectKV(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		all, err = listAllInstances(ctx, kv)
		if err != nil {
			_ = kv.Close()
			return nil, nil, nil, fmt.Errorf("list instances: %w", err)
		}
	}
	store, target, err := resolveKeyTarget(ctx, addr, instName, kv, all)
	if err != nil {
		if kv != nil {
			_ = kv.Close()
		}
		return nil, nil, nil, err
	}
	cleanup := func() {
		_ = store.Close()
		if kv != nil {
			_ = kv.Close()
		}
	}
	return store, target, cleanup, nil
}

// keyPutCmd 上传对象（缺省 stdin / -file）。
var keyPutCmd = &cobra.Command{
	Use:   "put",
	Short: "上传对象（缺省 stdin / -file）",
	RunE: func(cmd *cobra.Command, args []string) error {
		key, _ := cmd.Flags().GetString("key")
		if key == "" {
			return fmt.Errorf("-key required")
		}
		file, _ := cmd.Flags().GetString("file")
		size, _ := cmd.Flags().GetInt64("size")

		ctx, cancel := ctxWithTimeout(context.Background())
		defer cancel()
		store, target, cleanup, err := prepareKeyStore(ctx)
		if err != nil {
			return err
		}
		defer cleanup()

		var in io.Reader
		if file != "" {
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			defer f.Close()
			in = f
		} else {
			in = os.Stdin
		}
		data, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		if size < 0 || size > int64(len(data)) {
			size = int64(len(data))
		}
		if err := store.Put(ctx, key, size, data); err != nil {
			return err
		}
		if global.json {
			printJSON(map[string]interface{}{"key": key, "size": size, "instance": targetName(target)})
			return nil
		}
		fmt.Printf("put %q ok (%d bytes) -> %s\n", key, size, targetName(target))
		return nil
	},
}

// keyGetCmd 下载（-off/-size 区间，缺省 stdout / -file）。
var keyGetCmd = &cobra.Command{
	Use:   "get",
	Short: "下载对象（size=-1 全量，缺省 stdout / -file）",
	RunE: func(cmd *cobra.Command, args []string) error {
		key, _ := cmd.Flags().GetString("key")
		if key == "" {
			return fmt.Errorf("-key required")
		}
		off, _ := cmd.Flags().GetInt64("off")
		size, _ := cmd.Flags().GetInt64("size")
		file, _ := cmd.Flags().GetString("file")
		if global.json && file == "" {
			return fmt.Errorf("get -json 需配合 -file（JSON 元信息与二进制数据不能混用 stdout）")
		}

		ctx, cancel := ctxWithTimeout(context.Background())
		defer cancel()
		store, target, cleanup, err := prepareKeyStore(ctx)
		if err != nil {
			return err
		}
		defer cleanup()

		data, rel, err := store.Get(ctx, key, off, size)
		if err != nil {
			return err
		}
		if data != nil {
			defer rel()
		}

		var out io.Writer
		if file != "" {
			f, err := os.Create(file)
			if err != nil {
				return err
			}
			defer f.Close()
			out = f
		} else {
			out = os.Stdout
		}
		n, werr := out.Write(data)
		if werr != nil {
			return werr
		}
		if file != "" || global.json {
			printJSON(map[string]interface{}{"key": key, "bytes": n, "instance": targetName(target)})
		} else {
			fmt.Fprintf(os.Stderr, "get %q: %d bytes\n", key, n)
		}
		return nil
	},
}

// keyDeleteCmd 删除对象。
var keyDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "删除对象",
	RunE: func(cmd *cobra.Command, args []string) error {
		key, _ := cmd.Flags().GetString("key")
		if key == "" {
			return fmt.Errorf("-key required")
		}
		ctx, cancel := ctxWithTimeout(context.Background())
		defer cancel()
		store, target, cleanup, err := prepareKeyStore(ctx)
		if err != nil {
			return err
		}
		defer cleanup()

		if err := store.Delete(ctx, key); err != nil {
			return err
		}
		if global.json {
			printJSON(map[string]interface{}{"key": key, "instance": targetName(target)})
			return nil
		}
		fmt.Printf("delete %q ok -> %s\n", key, targetName(target))
		return nil
	},
}

// keyStatCmd 对象逻辑大小。
var keyStatCmd = &cobra.Command{
	Use:   "stat",
	Short: "返回对象逻辑大小",
	RunE: func(cmd *cobra.Command, args []string) error {
		key, _ := cmd.Flags().GetString("key")
		if key == "" {
			return fmt.Errorf("-key required")
		}
		ctx, cancel := ctxWithTimeout(context.Background())
		defer cancel()
		store, target, cleanup, err := prepareKeyStore(ctx)
		if err != nil {
			return err
		}
		defer cleanup()

		sz, err := store.Stat(ctx, key)
		if err != nil {
			return err
		}
		if global.json {
			printJSON(map[string]interface{}{"key": key, "size": sz, "instance": targetName(target)})
			return nil
		}
		fmt.Printf("stat %q: size=%d (%s) -> %s\n", key, sz, humanBytes(sz), targetName(target))
		return nil
	},
}

// keyMetaCmd 对象落盘元数据（段/偏移/大小）。要求直连 -addr / -instance。
var keyMetaCmd = &cobra.Command{
	Use:   "meta",
	Short: "返回对象落盘元数据 seg/off/size（需 -addr / -instance 直连）",
	RunE: func(cmd *cobra.Command, args []string) error {
		key, _ := cmd.Flags().GetString("key")
		if key == "" {
			return fmt.Errorf("-key required")
		}
		ctx, cancel := ctxWithTimeout(context.Background())
		defer cancel()
		store, _, cleanup, err := prepareKeyStore(ctx)
		if err != nil {
			return err
		}
		defer cleanup()
		rc, ok := store.(*rpcclient.Storage)
		if !ok {
			return fmt.Errorf("meta 仅支持直连（-addr / -instance），不走集群路由")
		}
		m, err := rc.Meta(ctx, key)
		if err != nil {
			return err
		}
		if global.json {
			printJSON(map[string]interface{}{
				"key": key, "size": m.Size, "segment_id": m.SegmentID, "offset": m.Offset,
			})
			return nil
		}
		fmt.Printf("key %s size=%d seg=%d off=%d\n", key, m.Size, m.SegmentID, m.Offset)
		return nil
	},
}

// keyListCmd 按前缀枚举 key（需直连）。
var keyListCmd = &cobra.Command{
	Use:   "list",
	Short: "按前缀枚举 key（需 -addr / -instance 直连；prefix 缺省=全部）",
	RunE: func(cmd *cobra.Command, args []string) error {
		prefix, _ := cmd.Flags().GetString("prefix")
		limit, _ := cmd.Flags().GetInt("limit")

		ctx, cancel := ctxWithTimeout(context.Background())
		defer cancel()
		store, _, cleanup, err := prepareKeyStore(ctx)
		if err != nil {
			return err
		}
		defer cleanup()
		rc, ok := store.(*rpcclient.Storage)
		if !ok {
			return fmt.Errorf("list 仅支持直连（-addr / -instance），不走集群路由")
		}
		keys, err := rc.ListKeys(ctx, prefix)
		if err != nil {
			return err
		}
		if limit > 0 && len(keys) > limit {
			keys = keys[:limit]
		}
		if global.json {
			printJSON(map[string]interface{}{"prefix": prefix, "total": len(keys), "keys": keys})
			return nil
		}
		for _, k := range keys {
			fmt.Println(k)
		}
		return nil
	},
}

// targetName 输出目标名：直连=实例名/addr，集群=instance (routed)。
func targetName(t *keyTarget) string {
	if t == nil {
		return "cluster-routed"
	}
	if t.inst != nil {
		return t.inst.Name
	}
	return "direct"
}

func init() {
	keyCmd.AddCommand(keyPutCmd, keyGetCmd, keyDeleteCmd, keyStatCmd, keyMetaCmd, keyListCmd)
	pf := keyCmd.PersistentFlags()
	pf.String("addr", "", "直连实例地址（如 10.0.0.1:50051）")
	pf.String("instance", "", "按注册名寻址实例（配合 -pd）")
	keyPutCmd.Flags().String("key", "", "object key")
	keyPutCmd.Flags().String("file", "", "输入文件（缺省 stdin）")
	keyPutCmd.Flags().Int64("size", -1, "对象逻辑大小（缺省=输入长度）")
	keyGetCmd.Flags().String("key", "", "object key")
	keyGetCmd.Flags().String("file", "", "输出文件（缺省 stdout）")
	keyGetCmd.Flags().Int64("off", 0, "读取起始偏移")
	keyGetCmd.Flags().Int64("size", -1, "读取长度（-1=至结尾）")
	keyDeleteCmd.Flags().String("key", "", "object key")
	keyStatCmd.Flags().String("key", "", "object key")
	keyMetaCmd.Flags().String("key", "", "object key")
	keyListCmd.Flags().String("prefix", "", "key 前缀（缺省=全部）")
	keyListCmd.Flags().Int("limit", 0, "返回条数上限（0=不限）")
}
