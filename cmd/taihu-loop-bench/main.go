// taihu-loop-bench 专测 loopback 裸 TCP 带宽上限。
//
// socket 函数与 taihu 传输层完全一致：netpoll 事件驱动（epoll）+ readv 收流 / sendmsg 推流，
// 不引入 gRPC/帧协议开销，数据纯流式，用于标定同机 loopback 的真实线速上限。
//
// 用法：
//
//	server: taihu-loop-bench -role server -addr 127.0.0.1:7788 -dir push
//	client: taihu-loop-bench -role client -addr 127.0.0.1:7788 -conns 8 -secs 10 -chunk 4194304 -dir push
//
// -dir push  = server->client 推流（对应读路径）；-dir pull = client->server 推流（对应写路径）。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/cloudwego/netpoll"
)

var (
	role  = flag.String("role", "client", "server|client")
	addr  = flag.String("addr", "127.0.0.1:7788", "listen or dial address")
	conns = flag.Int("conns", 8, "number of connections (client)")
	secs  = flag.Int("secs", 10, "duration in seconds (client)")
	chunk = flag.Int("chunk", 4<<20, "chunk size in bytes")
	dir   = flag.String("dir", "push", "data direction: push(server->client)|pull(client->server)")
)

var (
	sendBytes atomic.Int64
	recvBytes atomic.Int64
)

func main() {
	flag.Parse()
	if *role == "server" {
		if err := runServer(); err != nil {
			fmt.Fprintln(os.Stderr, "server:", err)
			os.Exit(1)
		}
		return
	}
	if err := runClient(); err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		os.Exit(1)
	}
}

// pushLoop 持续 Malloc(chunk)+Flush 推流（netpoll nocopy 写，无用户态拷贝），断开即退出。
func pushLoop(conn netpoll.Connection) {
	w := conn.Writer()
	// 预热一次填充模式，内容无关紧要
	if buf, err := w.Malloc(*chunk); err == nil {
		for i := range buf {
			buf[i] = byte(i)
		}
		if err := w.Flush(); err != nil {
			return
		}
		sendBytes.Add(int64(*chunk))
	} else {
		return
	}
	for {
		if _, err := w.Malloc(*chunk); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
		sendBytes.Add(int64(*chunk))
	}
}

// recvLoop 持续 Next(chunk)+Release 收流（netpoll nocopy 读，零拷贝），断开即退出。
func recvLoop(conn netpoll.Connection) {
	r := conn.Reader()
	for {
		if _, err := r.Next(*chunk); err != nil {
			return
		}
		if err := r.Release(); err != nil {
			return
		}
		recvBytes.Add(int64(*chunk))
	}
}

func runServer() error {
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	fmt.Printf("server listen %s dir=%s chunk=%d\n", *addr, *dir, *chunk)

	evl, err := netpoll.NewEventLoop(func(ctx context.Context, conn netpoll.Connection) error {
		return nil // push 模式服务端不回读；pull 模式数据到达由 OnConnect 起的 recvLoop 消化
	}, netpoll.WithOnConnect(func(ctx context.Context, conn netpoll.Connection) context.Context {
		if *dir == "push" {
			go pushLoop(conn)
		} else {
			go recvLoop(conn)
		}
		return ctx
	}))
	if err != nil {
		return err
	}

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			s := sendBytes.Swap(0)
			r := recvBytes.Swap(0)
			fmt.Printf("[server] send=%.0f MiB/s recv=%.0f MiB/s\n",
				float64(s)/1024/1024, float64(r)/1024/1024)
		}
	}()

	return evl.Serve(ln)
}

func runClient() error {
	fmt.Printf("client dial %s conns=%d secs=%d chunk=%d dir=%s\n", *addr, *conns, *secs, *chunk, *dir)
	cc := make([]netpoll.Connection, 0, *conns)
	for i := 0; i < *conns; i++ {
		conn, err := netpoll.DialConnection("tcp", *addr, 5*time.Second)
		if err != nil {
			return fmt.Errorf("dial %d: %w", i, err)
		}
		cc = append(cc, conn)
		if *dir == "pull" {
			go pushLoop(conn) // client 推流
		} else {
			go recvLoop(conn)
		}
	}

	start := time.Now()
	var prev int64
	var peak float64
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for i := 0; i <= *secs; i++ {
		<-t.C
		cur := recvBytes.Load()
		rate := float64(cur-prev) / 1024 / 1024
		if rate > peak {
			peak = rate
		}
		prev = cur
		fmt.Printf("[client] t=%ds rate=%.0f MiB/s total=%.2f GiB\n", i+1, rate,
			float64(cur)/1024/1024/1024)
	}
	elapsed := time.Since(start).Seconds()
	total := recvBytes.Load()
	avg := float64(total) / 1024 / 1024 / elapsed
	for _, c := range cc {
		c.Close()
	}
	fmt.Printf("RESULT avg=%.1f MiB/s peak=%.0f MiB/s total=%.2f GiB elapsed=%.1fs\n",
		avg, peak, float64(total)/1024/1024/1024, elapsed)
	return nil
}
