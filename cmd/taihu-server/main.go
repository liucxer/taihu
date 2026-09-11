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
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/liucxer/taihu/internal/cluster"
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
		shm       = flag.String("shm", "", "unix domain socket path for shmipc shared-memory IPC (disabled if empty)")
		pprofAddr = flag.String("pprof", "", "pprof http listen address (e.g. :6060), disabled if empty")
		// 集群注册（缓存场景：首写本地 + 索引锚定 + 回源兜底）：
		// -name/-node 齐备即启用；-reg-addr 为对外通告地址（客户端直连用）；
		// -tikv-pd 指定 PD 地址列表（逗号分隔），缺省回落内存 KV（单机开发/测试）。
		name    = flag.String("name", "", "cluster instance name (enables cluster registration)")
		node    = flag.String("node", "", "cluster node id (enables cluster registration)")
		regAddr = flag.String("reg-addr", "", "address advertised to clients, e.g. 127.0.0.1:50051")
		tikvPD  = flag.String("tikv-pd", "", "comma-separated TiKV PD addresses (empty -> in-memory KV)")
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

	// 集群注册：注册 + 1s 心跳 + 停机注销（本地优先发现/索引均依赖该注册区）。
	var (
		kv            cluster.KV
		clusterCancel context.CancelFunc
	)
	if *name != "" || *node != "" {
		if *name == "" || *node == "" || *regAddr == "" {
			log.Fatalf("cluster mode requires -name, -node and -reg-addr")
		}
		if *tikvPD != "" {
			kv, err = cluster.NewTiKVKV(context.Background(), strings.Split(*tikvPD, ","))
			if err != nil {
				log.Fatalf("tikv connect: %v", err)
			}
		} else {
			log.Printf("cluster: no -tikv-pd, using in-memory KV")
			kv = cluster.NewMemoryKV()
		}
		defer kv.Close()

		info := &cluster.InstanceInfo{
			Name:      *name,
			Node:      *node,
			Addr:      *regAddr,
			ShmAddr:   *shm, // 同机共享内存 unix socket（-shm）；空=未开放 shm
			StartTime: time.Now().Unix(),
		}
		// 心跳刷新动态字段（容量/可用/已用），StartTime 保持注册时刻。
		refresh := func() *cluster.InstanceInfo {
			n := *info
			cap, avail, used, err := storage.GetDiskCapacity()
			if err != nil {
				log.Printf("GetDiskCapacity: %v", err)
			} else {
				n.Capacity, n.Available, n.Used = cap, avail, used
			}
			return &n
		}
		if err := cluster.Register(context.Background(), kv, refresh()); err != nil {
			log.Fatalf("cluster register: %v", err)
		}
		var hctx context.Context
		hctx, clusterCancel = context.WithCancel(context.Background())
		go cluster.RunHeartbeat(hctx, kv, refresh, time.Second)
		log.Printf("cluster registered name=%s node=%s addr=%s kv=%T", *name, *node, *regAddr, kv)
	}

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}

	gs := rpcserver.New(storage)

	// 可选的同机共享内存 IPC（shmipc）：-shm 指定 unix socket 路径，与 TCP 监听并行。
	var shmCloser io.Closer
	if *shm != "" {
		shmCloser, err = transport.ServeShm(storage, *shm)
		if err != nil {
			log.Fatalf("serve shm %s: %v", *shm, err)
		}
		defer shmCloser.Close()
	}

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
		// 先注销集群注册（避免残留僵尸实例），再停数据面。
		if clusterCancel != nil {
			clusterCancel()
			_ = cluster.Unregister(context.Background(), kv, *name)
		}
		gs.GracefulStop()
	}()

	log.Printf("taihu-server listening on %s (db=%s dev=%s shm=%s)", *addr, *db, *dev, *shm)
	if err := gs.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
