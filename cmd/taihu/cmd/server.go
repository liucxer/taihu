// Sub-command taihu server 是 taihu 远程访问层的 netpoll 服务端（设计文档_v3 §4，替代 gRPC）。
//
// 单 Storage 实例（-db pebble 目录 + -dev 裸设备），对外暴露 4 个 RPC：
// Put（client 流式上传）、Get（server 流式下发，支持 off/size 区间）、Delete、Stat。
// 具体 RPC 实现见 internal/transport；本文件只做参数解析、资源初始化和生命周期管理。
package cmd

import (
	"context"
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

	"github.com/spf13/cobra"

	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/internal/storage"
	"github.com/liucxer/taihu/internal/transport"
)

// 端口自动分配区间：RPC 与 pprof 均在此区间内抢占未使用端口（互不相同）。
const (
	portRangeStart = 50000
	portRangeEnd   = 51000
)

// serverCmd 启动 taihu 对象服务端（daemon）。
var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "启动 taihu 对象服务端（daemon）",
	Long: `taihu server：启动单 Storage 实例的 netpoll 服务端，对外暴露 Put/Get/Delete/Stat 四个 RPC。

必传参数：
  -listen <IP1,IP2,...>      监听 IP 列表（逗号分隔；支持 bond0/1/2 多网卡，同一端口在全部 IP 绑定）
  -db <dir>                  pebble 元数据目录
  -dev <path>                裸设备路径
  -server-name <NAME>        实例唯一标识（如 TAIHU-0；也是 TiKV 容量记录与集群注册的 key）
  -pd <PD...>                TiKV PD 地址（逗号分隔；容量记录/比较、集群注册依赖）

说明：
  端口自动分配于 [50000,51000]；通告地址 = -listen 首个 IP + RPC 端口；共享内存（shmipc）
  unix socket 固定 /dev/<server-name>，与 TCP 监听并行（默认开启）；pprof 监听 0.0.0.0，
  端口同样自动分配；段大小内置写死 8GiB，不允许命令行覆盖。`,
	Example: `  # 启动单实例服务端（双网卡监听，端口自动分配）
  taihu server -listen 10.0.0.1,10.0.0.2 -db /mnt/db -dev /dev/nvme0n1 -server-name TAIHU-0 -pd 100.71.128.11:2379`,
	RunE: func(cmd *cobra.Command, args []string) error {
		listen, _ := cmd.Flags().GetString("listen")
		db, _ := cmd.Flags().GetString("db")
		dev, _ := cmd.Flags().GetString("dev")
		serverName, _ := cmd.Flags().GetString("server-name")
		writeBatch, _ := cmd.Flags().GetInt("write-batch")
		writeWorkers, _ := cmd.Flags().GetInt("write-workers")
		delBatch, _ := cmd.Flags().GetInt("del-batch")
		delWorkers, _ := cmd.Flags().GetInt("del-workers")

		if db == "" || dev == "" {
			return fmt.Errorf("-db and -dev are required")
		}
		if serverName == "" || global.pd == "" {
			return fmt.Errorf("-server-name and -pd are required")
		}
		if listen == "" {
			return fmt.Errorf("-listen is required (comma-separated listen IPs)")
		}

		// 解析并去重监听 IP。
		seen := make(map[string]struct{})
		var ips []string
		for _, s := range strings.Split(listen, ",") {
			ip := strings.TrimSpace(s)
			if net.ParseIP(ip) == nil {
				return fmt.Errorf("invalid listen IP %q", s)
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
		rpcListeners, rpcPort, err := listenMultiPort(ips)
		if err != nil {
			return err
		}
		advAddr := net.JoinHostPort(ips[0], strconv.Itoa(rpcPort))
		pprofLn, pprofPort, err := pickPort(map[int]struct{}{rpcPort: {}})
		if err != nil {
			return err
		}

		// 容量读取 + TiKV 记录/比较：每次启动读取一次 nvme 容量，
		// 与 TiKV 中该实例已有记录比对，不一致（换盘/容量变化）拒绝启动。
		segSize := layout.DefaultSegmentSizeBytes // 段大小内置写死（8GiB），不允许命令行覆盖
		kv, err := cluster.NewTiKVKV(ctx, strings.Split(global.pd, ","), cluster.TLSConfig{
			CA: global.tikvCA, Cert: global.tikvCert, Key: global.tikvKey,
		})
		if err != nil {
			return fmt.Errorf("tikv connect: %w", err)
		}
		defer kv.Close()

		capacity, err := device.DeviceCapacity(dev)
		if err != nil {
			return fmt.Errorf("DeviceCapacity %s: %w", dev, err)
		}
		if rec, ok, err := cluster.GetCapacity(ctx, kv, serverName); err != nil {
			return fmt.Errorf("get capacity record: %w", err)
		} else if ok && rec.CapacityBytes != capacity {
			return fmt.Errorf("instance %s capacity changed: recorded=%d current=%d (disk replaced? deploy as a new instance)",
				serverName, rec.CapacityBytes, capacity)
		}
		if err := cluster.PutCapacity(ctx, kv, serverName, cluster.CapacityRecord{
			CapacityBytes:    capacity,
			SegmentSizeBytes: segSize,
			SegmentCount:     capacity / segSize,
			ListenAddr:       advAddr,
			UpdateTime:       time.Now().Unix(),
		}); err != nil {
			return fmt.Errorf("put capacity record: %w", err)
		}
		log.Printf("capacity %s: %d bytes = %d segments x %d", serverName, capacity, capacity/segSize, segSize)

		l := layout.ComputeLayout(capacity, segSize)
		aioOpts, err := aioOptions()
		if err != nil {
			return err
		}
		st, err := storage.NewStorage(ctx, db, dev, l, aioOpts...)
		if err != nil {
			return fmt.Errorf("NewStorage: %w", err)
		}
		defer st.Close()

		// 后台段压缩（compaction）：低频搬移高空洞 Full 段存活对象，配合 segment 级 GC 释放空间。
		// 默认配置（60s 周期 / 空洞 80% / 全局水位 80% 触发），随进程生命周期启停。
		compactor := storage.NewCompactor(st, storage.DefaultCompactorConfig())
		compactor.Start()
		defer compactor.Stop()

		// 集群注册：注册 + 1s 心跳 + 停机注销（本地优先发现/索引均依赖该注册区）。
		// Hostname（主机名）供 SDK 判断"客户端是否与服务端同机"——同机走 shm、否则走 TCP。
		hostname, hnerr := os.Hostname()
		if hnerr != nil {
			hostname = ""
		}
		shmPath := "/dev/" + serverName
		info := &cluster.InstanceInfo{
			Name:      serverName,
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
			cap, avail, used, err := st.GetDiskCapacity()
			if err != nil {
				log.Printf("GetDiskCapacity: %v", err)
			} else {
				n.Capacity, n.Available, n.Used = cap, avail, used
			}
			return &n
		}
		if err := cluster.Register(context.Background(), kv, refresh()); err != nil {
			return fmt.Errorf("cluster register: %w", err)
		}
		// defer 兜住所有 return 路径（含 shm 启动失败等中途错误）：否则心跳 ctx 只在
		// 收到信号时被取消，错误退出会让心跳 goroutine 泄漏到进程结束（vet lostcancel）。
		hctx, clusterCancel := context.WithCancel(context.Background())
		defer clusterCancel()
		go cluster.RunHeartbeat(hctx, kv, refresh, time.Second)
		log.Printf("cluster registered name=%s node=%s hostname=%s addr=%s kv=%T", serverName, hostname, hostname, advAddr, kv)

		gs := transport.NewServerWithOptions(st, transport.PipelineConfig{
			WriteBatch:    writeBatch,
			WriteWorkers:  writeWorkers,
			DeleteBatch:   delBatch,
			DeleteWorkers: delWorkers,
		})

		// 同机共享内存 IPC（shmipc）：unix socket 固定 /dev/<server-name>，与 TCP 监听并行（默认开启）。
		// -batch>0 时启用"多 stream 多 worker"批读（对齐整块 4MiB 直读聚合 io_submit）；
		// -write-batch>0 时启用整对象攒批写（一次 AppendBatch + BatchPutCommit）；
		// -del-batch>0 时启用批量删（一次 BatchDelete）。
		// 非 Linux 平台 shmipc 不可用（且 /dev 不可写），跳过该数据面而非启动失败 ——
		// TCP 服务照常，使服务端能在 macOS 上跑起来做本机联调。
		batchTarget, _ := cmd.Flags().GetInt("batch")
		batchWorkers, _ := cmd.Flags().GetInt("batch-workers")
		if transport.ShmSupported() {
			shmCloser, err := transport.ServeShmWithConfig(st, shmPath, transport.PipelineConfig{
				ReadBatch:     batchTarget,
				ReadWorkers:   batchWorkers,
				WriteBatch:    writeBatch,
				WriteWorkers:  writeWorkers,
				DeleteBatch:   delBatch,
				DeleteWorkers: delWorkers,
			})
			if err != nil {
				return fmt.Errorf("serve shm %s: %w", shmPath, err)
			}
			defer shmCloser.Close()
		} else {
			log.Printf("taihu: shmipc 仅 Linux 支持，本平台跳过（%s 未创建）；仅提供 TCP 服务", shmPath)
		}

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
				io4M, ioOther, b4M, bOther := st.IOStats()
				log.Printf("[stat] disk-io 4MiB=%d other=%d bytes4MiB=%d bytesOther=%d", io4M, ioOther, b4M, bOther)
				log.Printf("[stat] segments free=%d active=%d full=%d reclaiming=%d",
					st.SegmentStats()[metastore.SegmentStateFree],
					st.SegmentStats()[metastore.SegmentStateActive],
					st.SegmentStats()[metastore.SegmentStateFull],
					st.SegmentStats()[metastore.SegmentStateReclaiming])
				log.Printf("[stat] %s", transport.StatsString())
			}
		}()

		go func() {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			<-sig
			log.Println("shutting down...")
			// 先注销集群注册（避免残留僵尸实例），再停数据面。
			// clusterCancel 已由上面的 defer 保证调用，这里提前调一次让心跳先停。
			clusterCancel()
			_ = cluster.Unregister(context.Background(), kv, serverName)
			gs.GracefulStop()
		}()

		log.Printf("taihu server listening on %v (rpcPort=%d pprofPort=%d db=%s dev=%s shm=%s)",
			ips, rpcPort, pprofPort, db, dev, shmPath)
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
		return nil
	},
}

func init() {
	f := serverCmd.Flags()
	f.String("listen", "", "comma-separated listen IPs, e.g. 10.0.0.1,10.0.0.2 (required)")
	f.String("db", "", "pebble metadata directory (required)")
	f.String("dev", "", "raw device path (required)")
	f.String("server-name", "", "unique server name, e.g. TAIHU-0 (required)")
	f.Int("batch", 0, "shm 批读批量：>0 启用\"多 stream 多 worker\"聚合批读（一次 io_submit 提交多个任务）；0 关闭")
	f.Int("batch-workers", 8, "shm 批读 worker 池大小（并行批提交，K×batch 即整机在途批读数）")
	f.Int("write-batch", 0, "写流水线批量：>0 启用整对象攒批写（一次 AppendBatch 排空 + 一次 BatchPutCommit）；0 关闭（逐请求串行）")
	f.Int("write-workers", 4, "写流水线 worker 池大小（并行批提交;K×batch 即整机在途写对象数）")
	f.Int("del-batch", 0, "删流水线批量：>0 启用批量删（一次 BatchDelete,per-key 结果独立）；0 关闭（逐请求串行）")
	f.Int("del-workers", 2, "删流水线 worker 池大小")
}

// listenMultiPort 在 -listen 指定的一批 IP 上抢占同一未使用 TCP 端口（[50000,51000]），
// 返回全部 listener 与端口号。任一 IP 上该端口被占用则关闭已建 listener 并整体换下一个端口重试。
func listenMultiPort(ips []string) ([]net.Listener, int, error) {
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
			return lns, port, nil
		}
	}
	return nil, 0, fmt.Errorf("no free port on %v in [%d,%d]", ips, portRangeStart, portRangeEnd)
}

// pickPort 抢占 [portRangeStart, portRangeEnd] 中第一个未占用（且未被 exclude 排除）的 TCP 端口，
// 返回已绑定的监听器与端口号（监听 0.0.0.0，绑定后由调用方 Serve）。区间耗尽则返回错误。
func pickPort(exclude map[int]struct{}) (*net.TCPListener, int, error) {
	for port := portRangeStart; port <= portRangeEnd; port++ {
		if _, ok := exclude[port]; ok {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue // 端口被占用 → 试下一个
		}
		return ln.(*net.TCPListener), port, nil
	}
	return nil, 0, fmt.Errorf("no free port in [%d,%d]", portRangeStart, portRangeEnd)
}
