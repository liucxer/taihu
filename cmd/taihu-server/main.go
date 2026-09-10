// Command taihu-server 是 taihu 远程访问层的 netpoll 服务端（设计文档_v3 §4，替代 gRPC）。
//
// 单 Storage 实例（-db pebble 目录 + -dev 裸设备），对外暴露 4 个 RPC：
// Put（client 流式上传）、Get（server 流式下发，支持 off/size 区间）、Delete、Stat。
// 具体 RPC 实现见 internal/rpcserver；本文件只做参数解析、资源初始化和生命周期管理。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/internal/rpcserver"
	"github.com/liucxer/taihu/internal/transport"
	"github.com/liucxer/taihu/pkg/taihu"
)

func main() {
	var (
		addr      = flag.String("addr", ":50051", "listen address")
		db        = flag.String("db", "", "pebble metadata directory (required)")
		dev       = flag.String("dev", "", "raw device path (required)")
		pprofAddr = flag.String("pprof", "", "pprof http listen address (e.g. :6060), disabled if empty")
	)
	flag.Parse()
	if *db == "" || *dev == "" {
		fmt.Fprintln(os.Stderr, "taihu-server: -db and -dev are required")
		os.Exit(2)
	}

	storage, err := taihu.NewStorage(context.Background(), *db, *dev)
	if err != nil {
		log.Fatalf("NewStorage: %v", err)
	}
	defer storage.Close()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}

	gs := rpcserver.New(storage)

	// 可选的 pprof 端点（仅压测/排查用，默认关闭），例：-pprof :6060
	if *pprofAddr != "" {
		go func() {
			log.Printf("pprof listening on %s", *pprofAddr)
			if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
				log.Printf("pprof serve: %v", err)
			}
		}()
		// 链路尺寸统计：5s 一次打到 stderr（验证磁盘/回帧是否整块 4MiB）。
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for range t.C {
				io4M, ioOther, b4M, bOther := storage.IOStats()
				log.Printf("[stat] disk-io 4MiB=%d other=%d bytes4MiB=%d bytesOther=%d", io4M, ioOther, b4M, bOther)
				log.Printf("[stat] segments free=%d active=%d full=%d reclaiming=%d",
					storage.SegmentStats()[metastore.SegmentStateFree],
					storage.SegmentStats()[metastore.SegmentStateActive],
					storage.SegmentStats()[metastore.SegmentStateFull],
					storage.SegmentStats()[metastore.SegmentStateReclaiming])
				log.Printf("[stat] %s", transport.StatsString())
			}
		}()
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutting down...")
		gs.GracefulStop()
	}()

	log.Printf("taihu-server listening on %s (db=%s dev=%s)", *addr, *db, *dev)
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
