package taihu

import (
	"github.com/liucxer/taihu/internal/aio"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
)

// 本文件把出现在本包**导出签名**里的 internal 类型 re-export 出去。
//
// 为什么必须补：internal/ 下的类型，外部模块既不能 import 也不能命名。于是
//
//	func NewStorage(..., l layout.Layout, opts ...Option) (*Storage, error)
//
// 这类签名虽然能被调用，但调用方无法声明该类型的变量、写不了字面量、也放不进
// 自己的结构体字段 —— 等于这些 API 对外不可用（实测外部模块报
// `undefined: taihu.ModeIOUring`）。而 Layout 是 NewStorage 的必填参数，
// 缺了它 SDK 的主构造函数实际无法被调用。
//
// 用 type alias（=）而不是自定义新类型：不新建类型、零运行时代价、无需任何转换，
// 只是给同一个类型在本包内起一个外部可引用的名字，**签名与行为完全不变**。
// 与本包 meta.go 的 ObjectMeta、error.go 的错误变量是同一套做法。
//
// 维护约定：凡是本包导出函数签名或导出结构体字段里出现 internal 类型，
// 就到这里补一条 alias，并连带 re-export 调用方解读该类型所需的常量。

// Layout 裸设备物理布局参数（段大小与整盘段数），NewStorage 的入参。
type Layout = layout.Layout

// SegmentState 单个 segment 的生命周期状态。
type SegmentState = metastore.SegmentState

// 段状态常量：调用方据此解读 SegmentStats / SegmentEntry.State。
const (
	SegmentStateFree       = metastore.SegmentStateFree
	SegmentStateActive     = metastore.SegmentStateActive
	SegmentStateFull       = metastore.SegmentStateFull
	SegmentStateReclaiming = metastore.SegmentStateReclaiming
	SegmentStateCompacting = metastore.SegmentStateCompacting
)

// Mode 磁盘异步 IO 后端选择。
type Mode = aio.Mode

// IO 后端常量：WithAIOMode 的取值（ModeIOUring 在内核不支持时启动失败）。
const (
	ModeAuto    = aio.ModeAuto
	ModeLibAIO  = aio.ModeLibAIO
	ModeIOUring = aio.ModeIOUring
)
