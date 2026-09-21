// Command taihu-example 演示外部业务如何接入 taihu 集群。
//
// taihu 的对外接口只有一个：pkg/taihu-client（唯一对外 SDK，集群模式）。
// 外部业务不能直连某个 taihu server 实例，必须经 TiKV 路由访问集群。
//
// 运行前提：
//  1. 已有 TiKV PD（多个 taihu server 实例以 -pd 注册进同一集群）；
//  2. 本机与集群网络可达（同机实例自动走共享内存，跨节点走 TCP）。
//
// 运行：
//
//	TAIHU_PD=100.71.128.11:2379,100.71.128.12:2379,100.71.128.13:2379 \
//		go run ./examples/taihu-client
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/liucxer/taihu/pkg/taihu-client"
)

// clusterClient 是本示例用到的集群客户端能力集（pkg/taihu-client 的 *Storage 满足）。
// 抽成接口只为让无集群环境下的单测注入假实现；对外契约仍是 taihuclient.NewFromTiKV。
type clusterClient interface {
	CheckPoolIsValid() error
	Put(ctx context.Context, key string, size int64, in []byte) error
	Get(ctx context.Context, key string, off, size int64) ([]byte, func(), error)
	Stat(ctx context.Context, key string) (int64, error)
	Delete(ctx context.Context, key string) error
	Close() error
}

// 编译期断言：真实 SDK 客户端必须满足本示例使用的能力集。
var _ clusterClient = (*taihuclient.Storage)(nil)

// newClusterClient 构造集群客户端（生产恒为 NewFromTiKV；单测可替换注入假实现）。
var newClusterClient = func(ctx context.Context, pd string) (clusterClient, error) {
	return taihuclient.NewFromTiKV(ctx, taihuclient.TiKVOptions{
		PDAddrs:    strings.Split(pd, ","),
		ClientName: "billing-app",
		// 可选参数（不设即默认）：
		//   ClientID:       非空则向注册区注册 SDK 客户端并保活（taihu client list 可见），Close 时注销
		//   Conns:          每地址数据面连接数（默认 1；实例通告多地址时总连接 = 地址数 × Conns，读写均分）
		//   UsageThreshold: 选实例水位阈值百分比（默认 80，超过则跳过该实例）
		//   WriteRouting:   写路由："local"（默认，本地优先）/ "round-robin"（全在线实例轮询）
		//   Source:         回源回调（可选，集群全 miss 时拉远端源并回写缓存）
	})
}

// run 连接集群并跑完示例读写删流程；pd 为 TAIHU_PD（逗号分隔 PD 地址），w 为普通输出。
func run(ctx context.Context, pd string, w io.Writer) error {
	if pd == "" {
		return errors.New("TAIHU_PD 未设置：形如 10.0.0.11:2379,10.0.0.12:2379,10.0.0.13:2379（与 taihu server 的 -pd 一致）")
	}

	// 1. 构建集群客户端：连接 TiKV 注册区，自动启动实例发现/索引后台任务与数据面连接池。
	cli, initErr := newClusterClient(ctx, pd)
	if initErr != nil {
		return fmt.Errorf("连接 taihu 集群失败：%v", initErr)
	}
	defer cli.Close() // 注销 SDK 客户端（若配置了 ClientID）并停止后台心跳

	// 2. 尽早暴露配置错误：挂载/启动时确认集群有在线实例可服务（无实例返回 ErrNoInstances）。
	if poolErr := cli.CheckPoolIsValid(); poolErr != nil {
		return fmt.Errorf("集群无在线实例：%v", poolErr)
	}

	// 3. 写对象：size = 逻辑大小，数据取 in[:size]。
	//    注意：key 不可变，无覆盖写 —— 更新语义 = Delete 后重建。
	const key = "orders/2026-09-14/001"
	data := []byte(`{"order_id":1,"amount":99.9}`)
	if putErr := cli.Put(ctx, key, int64(len(data)), data); putErr != nil {
		return fmt.Errorf("Put 失败：%v", putErr)
	}
	fmt.Fprintf(w, "Put %q size=%d\n", key, len(data))

	// 4. 读对象：Get 返回 (data, release, err)，data 来自内部池化缓冲，
	//    用毕**必须**调用 release() 归还（幂等）。漏归还会耗尽缓冲池、走兜底分配路径。
	out, release, getErr := cli.Get(ctx, key, 0, -1) // size=-1 读至结尾
	if getErr != nil {
		return fmt.Errorf("Get 失败：%v", getErr)
	}
	defer release()
	fmt.Fprintf(w, "Get %q -> %s\n", key, out)

	// 5. Stat：返回对象逻辑大小；区间读：off/size 精确控制（off<0 或 off>size 返回 ErrInvalidRange）。
	if size, statErr := cli.Stat(ctx, key); statErr != nil {
		return fmt.Errorf("Stat 失败：%v", statErr)
	} else {
		fmt.Fprintf(w, "Stat %q size=%d\n", key, size)
	}
	head, releaseHead, headErr := cli.Get(ctx, key, 0, 8) // [0,8)
	if headErr != nil {
		return fmt.Errorf("区间读失败：%v", headErr)
	}
	fmt.Fprintf(w, "Get [0,8) -> %s\n", head)
	releaseHead()

	// 6. 删除与错误判定：以 errors.Is 判错误（ErrNotFound / ErrInvalidRange 等，见 reexport.go）。
	if delErr := cli.Delete(ctx, key); delErr != nil {
		return fmt.Errorf("Delete 失败：%v", delErr)
	}
	if _, _, afterDelErr := cli.Get(ctx, key, 0, -1); afterDelErr != nil {
		if errors.Is(afterDelErr, taihuclient.ErrNotFound) {
			fmt.Fprintln(w, "删除后 Get 预期返回 ErrNotFound")
		} else {
			return fmt.Errorf("删除后 Get 非预期错误：%v", afterDelErr)
		}
	}
	return nil
}

func main() {
	if err := run(context.Background(), os.Getenv("TAIHU_PD"), os.Stdout); err != nil {
		log.Fatal(err)
	}
}
