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
	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/internal/rpcserver"
	"github.com/liucxer/taihu/internal/transport"
	"github.com/liucxer/taihu/internal/version"
	"github.com/liucxer/taihu/pkg/taihu"
)

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version (commit_date) and exit")
		addr        = flag.String("addr", "", "listen address, concrete ip:port (required)")
		db          = flag.String("db", "", "pebble metadata directory (required)")
		dev         = flag.String("dev", "", "raw device path (required)")
		shm         = flag.String("shm", "", "unix domain socket path for shmipc shared-memory IPC (disabled if empty)")
		pprofAddr   = flag.String("pprof", "", "pprof http listen address (e.g. :6060), disabled if empty")
		// 实例唯一标识：-name（如 TAIHU-0）作为 taihu 实例的唯一标识符，
		// 也是 TiKV 中容量记录（/taihu/capacity/{name}）与集群注册（/taihu/instances/{name}）的 key。
		// -tikv-pd 必填：容量记录/比较（启动读 nvme 容量后写入/比对，不一致拒绝启动）依赖 TiKV。
		name    = flag.String("name", "", "unique instance name, e.g. TAIHU-0 (required)")
		node    = flag.String("node", "", "cluster node id (enables cluster registration)")
		regAddr = flag.String("reg-addr", "", "address advertised to clients, e.g. 127.0.0.1:50051")
		tikvPD  = flag.String("tikv-pd", "", "comma-separated TiKV PD addresses (required)")
		segSize = flag.Int64("seg-size", 0, "segment size bytes (0 = default 8GiB)")
	)
	flag.Parse()
	if *showVersion {
		fmt.Printf("taihu-server %s\n", version.String())
		return
	}
	if *db == "" || *dev == "" {
		fmt.Fprintln(os.Stderr, "taihu-server: -db and -dev are required")
		os.Exit(2)
	}
	if *name == "" || *tikvPD == "" || *addr == "" {
		fmt.Fprintln(os.Stderr, "taihu-server: -name, -tikv-pd and -addr are required")
		os.Exit(2)
	}
	if strings.HasPrefix(*addr, ":") {
		fmt.Fprintf(os.Stderr, "taihu-server: -addr must be a concrete ip:port, got %q\n", *addr)
		os.Exit(2)
	}

	ctx := context.Background()

	// 容量读取 + TiKV 记录/比较：每次启动读取一次 nvme 容量，
	// 与 TiKV 中该实例已有记录比对，不一致（换盘/容量变化）拒绝启动。
	kv, err := cluster.NewTiKVKV(ctx, strings.Split(*tikvPD, ","))
	if err != nil {
		log.Fatalf("tikv connect: %v", err)
	}
	defer kv.Close()

	capacity, err := device.DeviceCapacity(*dev)
	if err != nil {
		log.Fatalf("DeviceCapacity %s: %v", *dev, err)
	}
	if *segSize <= 0 {
		*segSize = layout.DefaultSegmentSizeBytes
	}
	if rec, ok, err := cluster.GetCapacity(ctx, kv, *name); err != nil {
		log.Fatalf("get capacity record: %v", err)
	} else if ok && rec.CapacityBytes != capacity {
		log.Fatalf("instance %s capacity changed: recorded=%d current=%d (disk replaced? deploy as a new instance)",
			*name, rec.CapacityBytes, capacity)
	}
	if err := cluster.PutCapacity(ctx, kv, *name, cluster.CapacityRecord{
		CapacityBytes:    capacity,
		SegmentSizeBytes: *segSize,
		SegmentCount:     capacity / *segSize,
		ListenAddr:       *addr,
		UpdateTime:       time.Now().Unix(),
	}); err != nil {
		log.Fatalf("put capacity record: %v", err)
	}
	log.Printf("capacity %s: %d bytes = %d segments x %d", *name, capacity, capacity/ *segSize, *segSize)

	l := layout.ComputeLayout(capacity, *segSize)
	storage, err := taihu.NewStorage(ctx, *db, *dev, l)
	if err != nil {
		log.Fatalf("NewStorage: %v", err)
	}
	defer storage.Close()

	// 后台段压缩（compaction）：低频搬移高空洞 Full 段存活对象，配合 segment 级 GC 释放空间。
	// 默认配置（60s 周期 / 空洞 80% / 全局水位 80% 触发），随进程生命周期启停。
	compactor := taihu.NewCompactor(storage, taihu.DefaultCompactorConfig())
	compactor.Start()
	defer compactor.Stop()

	// 集群注册：注册 + 1s 心跳 + 停机注销（本地优先发现/索引均依赖该注册区）。
	// kv 已在容量记录阶段创建（-tikv-pd 必填），此处复用。
	var clusterCancel context.CancelFunc
	if *name != "" || *node != "" {
		if *name == "" || *node == "" || *regAddr == "" {
			log.Fatalf("cluster mode requires -name, -node and -reg-addr")
		}
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
