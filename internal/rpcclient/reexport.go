package rpcclient

import (
	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/pkg/ierr"
)

// 本文件把出现在本包**导出签名**里的 internal 类型 re-export 出去。
//
// 为什么必须补：internal/ 下的类型，外部模块既不能 import 也不能命名。于是
//
//	func (s *Storage) Meta(ctx, key) (metastore.ObjectMeta, error)
//	func (s *Storage) Segments(ctx) (metastore.SegmentSummary, []metastore.SegmentEntry, error)
//
// 这类签名虽然能被调用，但调用方无法声明该类型的变量、写不了字面量、也放不进自己的
// 结构体字段 —— 等于这些 API 对外不可用（曾实测外部模块报 `undefined: taihu.Layout`）。
//
// 用 type alias（=）而不是自定义新类型：不新建类型、零运行时代价、无需任何转换，
// 只是给同一个类型在本包内起一个外部可引用的名字，**签名与行为完全不变**。
//
// 维护约定：凡是本包导出函数签名或导出结构体字段里出现 internal 类型，就到这里补一条
// alias，并连带 re-export 调用方解读该类型所需的常量（只给类型不给常量，调用方仍然
// 用不了 —— 例如 map[SegmentState]int 没法写 key）。签名里出现新 internal 类型而这里
// 忘了补时**编译不会报错**，只是包对外悄悄不可用，故 internal/rpcclient/api_test.go 用
// 外部测试包逐个命名这些类型，作为编译期的兜底。

// 客户端可见的库错误（唯一定义在 pkg/ierr；此处 re-export 以便外部
// errors.Is(err, rpcclient.ErrNotFound) 判断）。
//
// 不含 ierr.ErrConflict：那是 compaction 内部的 CAS 控制信号，不是客户端会收到的错误。
var (
	// ErrNotFound 表示 key 不存在。
	ErrNotFound = ierr.ErrNotFound
	// ErrInvalidRange 表示读写范围非法（off<0 或 size<0）。
	ErrInvalidRange = ierr.ErrInvalidRange
	// ErrTooLarge 表示对象超过单 segment 上限（不允许跨段）。
	ErrTooLarge = ierr.ErrTooLarge
	// ErrNoSpace 表示无空闲 segment 可写。
	ErrNoSpace = ierr.ErrNoSpace
	// ErrShortWrite 表示设备/传输实际写入字节数少于期望。
	ErrShortWrite = ierr.ErrShortWrite
)

// ObjectMeta 对象落盘元数据（段号/段内偏移/逻辑大小）。
type ObjectMeta = metastore.ObjectMeta

// SegmentEntry 单个 segment 的状态明细。
type SegmentEntry = metastore.SegmentEntry

// SegmentSummary 实例段汇总与写游标。
type SegmentSummary = metastore.SegmentSummary

// SegmentState 单个 segment 的生命周期状态。
type SegmentState = metastore.SegmentState

// 段状态常量：调用方据此解读 SegmentEntry.State / SegmentSummary 的各计数字段。
const (
	SegmentStateFree       = metastore.SegmentStateFree
	SegmentStateActive     = metastore.SegmentStateActive
	SegmentStateFull       = metastore.SegmentStateFull
	SegmentStateReclaiming = metastore.SegmentStateReclaiming
	SegmentStateCompacting = metastore.SegmentStateCompacting
)
