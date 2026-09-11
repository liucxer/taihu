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
	"strconv"
	"strings"
	"sync"
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

// 端口自动分配区间：RPC 与 pprof 均在此区间内抢占未使用端口（互不相同）。
const (
	portRangeStart = 50000
	portRangeEnd   = 51000
)

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version (commit_date) and exit")
		// RPC 监听 IP 列表（逗号分隔）：支持 bond0/bond1/bond2 等多网卡 IP，客户端
		// 可通过任一 IP 连接；同一端口在所有 IP 上分别绑定。端口自动分配（不传）。
		listen = flag.String("listen", "", "comma-separated listen IPs, e.g. 10.0.0.1,10.0.0.2 (required)")
		db     = flag.String("db", "", "pebble metadata directory (required)")
		dev    = flag.String("dev", "", "raw device path (required)")
		// 实例唯一标识：-server-name（如 TAIHU-0）作为 taihu 实例的唯一标识符，
		// 也是 TiKV 中容量记录（/taihu/capacity/{server-name}）与集群注册
		// （/taihu/instances/{server-name}）的 key。
		// -tikv-pd 必填：容量记录/比较（启动读 nvme 容量后写入/比对，不一致拒绝启动）依赖 TiKV。
		// 对外通告地址取 -listen 首个 IP + RPC 端口；共享内存（shmipc）默认开启，
		// unix socket 路径固定为 /dev/<server-name>；节点标识与本机 Hostname 取 os.Hostname()；
		// pprof 监听所有 IP（0.0.0.0），端口同样自动分配。
		serverName = flag.String("server-name", "", "unique server name, e.g. TAIHU-0 (required)")
		tikvPD     = flag.String("tikv-pd", "", "comma-separated TiKV PD addresses (required)")
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
	if *serverName == "" || *tikvPD == "" {
		fmt.Fprintln(os.Stderr, "taihu-server: -server-name and -tikv-pd are required")
		os.Exit(2)
	}
	if *listen == "" {
		fmt.Fprintln(os.Stderr, "taihu-server: -listen is required (comma-separated listen IPs)")
		os.Exit(2)
	}

	// 解析并去重监听 IP。
	seen := make(map[string]struct{})
	var ips []string
	for _, s := range strings.Split(*listen, ",") {
		ip := strings.TrimSpace(s)
		if net.ParseIP(ip) == nil {
			fmt.Fprintf(os.Stderr, "taihu-server: invalid listen IP %q\n", s)
			os.Exit(2)
		}
		if _, dup := seen[ip]; dup {
			continue
		}
		seen[ip] = struct{}{}
		ips = append(ips, ip)
	}

	ctx := context.Background()

	// RPC：-listen 每个 IP 绑定同一未使用端口；通告地址 = 首个 IP:端口。
	// pprof：监听 0.0.0.0（所有 IP），端口在 [50000,51000] 内抢占，与 RPC 端口互斥。
	rpcListeners, rpcPort := listenMultiPort(ips)
	advAddr := net.JoinHostPort(ips[0], strconv.Itoa(rpcPort))
	pprofLn, pprofPort := pickPort(map[int]struct{}{rpcPort: {}})

	// 容量读取 + TiKV 记录/比较：每次启动读取一次 nvme 容量，
	// 与 TiKV 中该实例已有记录比对，不一致（换盘/容量变化）拒绝启动。
	segSize := layout.DefaultSegmentSizeBytes // 段大小内置写死（8GiB），不允许命令行覆盖
	kv, err := cluster.NewTiKVKV(ctx, strings.Split(*tikvPD, ","))
	if err != nil {
		log.Fatalf("tikv connect: %v", err)
	}
	defer kv.Close()

	capacity, err := device.DeviceCapacity(*dev)
	if err != nil {
		log.Fatalf("DeviceCapacity %s: %v", *dev, err)
	}
	if rec, ok, err := cluster.GetCapacity(ctx, kv, *serverName); err != nil {
		log.Fatalf("get capacity record: %v", err)
	} else if ok && rec.CapacityBytes != capacity {
		log.Fatalf("instance %s capacity changed: recorded=%d current=%d (disk replaced? deploy as a new instance)",
			*serverName, rec.CapacityBytes, capacity)
	}
	if err := cluster.PutCapacity(ctx, kv, *serverName, cluster.CapacityRecord{
		CapacityBytes:    capacity,
		SegmentSizeBytes: segSize,
		SegmentCount:     capacity / segSize,
		ListenAddr:       advAddr,
		UpdateTime:       time.Now().Unix(),
	}); err != nil {
		log.Fatalf("put capacity record: %v", err)
	}
	log.Printf("capacity %s: %d bytes = %d segments x %d", *serverName, capacity, capacity/segSize, segSize)

	l := layout.ComputeLayout(capacity, segSize)
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
	// Hostname（主机名）供 SDK 判断"客户端是否与服务端同机"——同机走 shm、否则走 TCP。
	hostname, hnerr := os.Hostname()
	if hnerr != nil {
		hostname = ""
	}
	shmPath := "/dev/" + *serverName
	var clusterCancel context.CancelFunc
	info := &cluster.InstanceInfo{
		Name:      *serverName,
		Node:      hostname,
		Hostname:  hostname,
		Addr:      advAddr,
		ShmAddr:   shmPath,
		StartTime: time.Now().Unix(),
	}
	// 完整 TCP 地址列表（各 IP 同端口）：供客户端按多 IP 均分建立数据面连接。
	// 旧客户端忽略 addrs 字段，仍以 Addr（首个 IP）直连，向后兼容。
	for _, ip := range ips {
		info.Addrs = append(info.Addrs, net.JoinHostPort(ip, strconv.Itoa(rpcPort)))
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
	log.Printf("cluster registered name=%s node=%s hostname=%s addr=%s kv=%T", *serverName, hostname, hostname, advAddr, kv)

	gs := rpcserver.New(storage)

	// 同机共享内存 IPC（shmipc）：unix socket 固定 /dev/<server-name>，与 TCP 监听并行（默认开启）。
	shmCloser, err := transport.ServeShm(storage, shmPath)
	if err != nil {
		log.Fatalf("serve shm %s: %v", shmPath, err)
	}
	defer shmCloser.Close()

	// pprof 端点：监听所有 IP（0.0.0.0），端口自动分配（[50000,51000] 内未使用端口）；
	// 附带 5s 一次的链路尺寸统计日志。
	go func() {
		log.Printf("pprof listening on :%d", pprofPort)
		if err := http.Serve(pprofLn, nil); err != nil && err != http.ErrServerClosed {
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

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutting down...")
		// 先注销集群注册（避免残留僵尸实例），再停数据面。
		if clusterCancel != nil {
			clusterCancel()
			_ = cluster.Unregister(context.Background(), kv, *serverName)
		}
		gs.GracefulStop()
	}()

	log.Printf("taihu-server listening on %v (rpcPort=%d pprofPort=%d db=%s dev=%s shm=%s)",
		ips, rpcPort, pprofPort, *db, *dev, shmPath)
	// 每个监听 IP 一个 Serve goroutine；全部退出后主流程结束。
	var serveWG sync.WaitGroup
	for _, ln := range rpcListeners {
		serveWG.Add(1)
		go func(ln net.Listener) {
			defer serveWG.Done()
			if err := gs.Serve(ln); err != nil {
				log.Printf("serve %s: %v", ln.Addr(), err)
			}
		}(ln)
	}
	serveWG.Wait()
}

// listenMultiPort 在 -listen 指定的一批 IP 上抢占同一未使用 TCP 端口（[50000,51000]），
// 返回全部 listener 与端口号。任一 IP 上该端口被占用则关闭已建 listener 并整体换下一个端口重试。
func listenMultiPort(ips []string) ([]net.Listener, int) {
	for port := portRangeStart; port <= portRangeEnd; port++ {
		var lns []net.Listener
		ok := true
		for _, ip := range ips {
			ln, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
			if err != nil {
				ok = false
				for _, l := range lns {
					_ = l.Close()
				}
				break
			}
			lns = append(lns, ln)
		}
		if ok {
			return lns, port
		}
	}
	log.Fatalf("taihu-server: no free port on %v in [%d,%d]", ips, portRangeStart, portRangeEnd)
	return nil, 0
}

// pickPort 抢占 [portRangeStart, portRangeEnd] 中第一个未占用（且未被 exclude 排除）的 TCP 端口，
// 返回已绑定的监听器与端口号（监听 0.0.0.0，绑定后由调用方 Serve）。区间耗尽则终止进程。
func pickPort(exclude map[int]struct{}) (*net.TCPListener, int) {
	for port := portRangeStart; port <= portRangeEnd; port++ {
		if _, ok := exclude[port]; ok {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue // 端口被占用 → 试下一个
		}
		return ln.(*net.TCPListener), port
	}
	log.Fatalf("taihu-server: no free port in [%d,%d]", portRangeStart, portRangeEnd)
	return nil, 0
}
