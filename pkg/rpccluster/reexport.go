package rpccluster

import (
	"github.com/liucxer/taihu/internal/cluster"
	"github.com/liucxer/taihu/pkg/rpcclient"
)

// 本文件把出现在本包**导出签名**里的 internal 类型 re-export 出去。
//
// internal/ 下的类型外部模块无法命名，会让下面这些签名对外不可用：
//
//	func NewInstanceRegistry(kv cluster.KV, ...) *InstanceRegistry
//	func (r *InstanceRegistry) Lookup(name string) (cluster.InstanceInfo, bool)
//	type ClusterConfig struct { KV cluster.KV }
//
// type alias（=）不新建类型、零运行时代价，只是让同一类型有一个外部可引用的名字。
// 详细说明见 pkg/rpcclient/reexport.go。维护约定同：签名里出现 internal 类型就补一条。

// KV 集群注册与索引的最小存储抽象（可替换：内存 KV / TiKV TxnKV）。
type KV = cluster.KV

// InstanceInfo 实例注册信息（实例发现与选路的返回类型）。
type InstanceInfo = cluster.InstanceInfo

// 客户端可见的库错误。本包的 Storage 把底层错误原样透出（不做包装），故只 import
// 本包的调用方也需要能命名它们才能做 errors.Is / 相等判断。
// 定义在 internal/ierr，pkg/rpcclient 已 re-export，此处再指一层以保持单一来源。
var (
	// ErrNotFound 表示 key 不存在。
	ErrNotFound = rpcclient.ErrNotFound
	// ErrInvalidRange 表示读写范围非法（off<0 或 size<0）。
	ErrInvalidRange = rpcclient.ErrInvalidRange
	// ErrTooLarge 表示对象超过单 segment 上限（不允许跨段）。
	ErrTooLarge = rpcclient.ErrTooLarge
	// ErrNoSpace 表示无空闲 segment 可写。
	ErrNoSpace = rpcclient.ErrNoSpace
	// ErrShortWrite 表示设备/传输实际写入字节数少于期望。
	ErrShortWrite = rpcclient.ErrShortWrite
)
