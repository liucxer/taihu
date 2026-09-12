// rawkv-scan: 用 RawKV 客户端扫描/删除 TiKV 中指定前缀的 key（默认 /taihu/）。
// 用于清理 rawkv 明文 key 残留（TxnKV 客户端无法解码这些 region 边界）。
//
// 用法:
//   rawkv-scan -pd 100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379
//   rawkv-scan -pd <PD> -delete        # 真正删除匹配的 key
//   rawkv-scan -pd <PD> -prefix /taihu/ -limit 200
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/tikv/client-go/v2/rawkv"
	"github.com/tikv/client-go/v2/config"
)

func main() {
	pd := flag.String("pd", "", "PD 地址列表，逗号分隔")
	prefix := flag.String("prefix", "/taihu/", "扫描前缀")
	limit := flag.Int("limit", 100, "最多打印 key 数（0=不限）")
	del := flag.Bool("delete", false, "是否删除匹配的 key")
	flag.Parse()

	if *pd == "" {
		fmt.Println("usage: rawkv-scan -pd <PD> [-delete] [-prefix /taihu/] [-limit N]")
		os.Exit(1)
	}

	ctx := context.Background()
	cli, err := rawkv.NewClientWithOpts(ctx, strings.Split(*pd, ","),
		rawkv.WithSecurity(config.Security{}))
	if err != nil {
		fmt.Printf("connect failed: %v\n", err)
		os.Exit(1)
	}
	defer cli.Close()

	start := []byte(*prefix)
	end := nextKey(start)

	var total, deleted int
	scanStart := start
	for {
		keys, _, err := cli.Scan(ctx, scanStart, end, 1024)
		if err != nil {
			fmt.Printf("scan error at %q: %v\n", scanStart, err)
			break
		}
		if len(keys) == 0 {
			break
		}
		for _, k := range keys {
			total++
			if *limit > 0 && total <= *limit {
				fmt.Printf("%s\n", k)
			}
			if *del {
				if err := cli.Delete(ctx, k); err != nil {
					fmt.Printf("delete %q failed: %v\n", k, err)
					continue
				}
				deleted++
			}
		}
		if len(keys) < 1024 {
			break
		}
		scanStart = append(append([]byte{}, keys[len(keys)-1]...), 0)
	}

	fmt.Printf("total=%d deleted=%d prefix=%s\n", total, deleted, *prefix)
}

// nextKey 返回 key 的后继（字典序下最小的大于 key 的字节串）。
func nextKey(key []byte) []byte {
	next := append([]byte{}, key...)
	for i := len(next) - 1; i >= 0; i-- {
		if next[i] != 0xff {
			next[i]++
			return next[:i+1]
		}
		next = next[:i]
	}
	return nil
}
