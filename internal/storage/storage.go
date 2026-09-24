// Package storage 对象存储核心：编排 metastore（元数据持久化）+ device（裸盘 O_DIRECT），
// 对上层（transport / cmd / cluster）暴露对象读写与容量管理。Storage 不感知 pebble 键格式
// 与 O_DIRECT 对齐之外的细节：对齐与段快照语义在本层吸收。
//
// 本文件是包内**唯一导出文件**：全部 public 顶层声明（Storage 类型、BatchReadBlock/
// BatchedReadResult/Compactor/CompactorConfig/Option、NewStorage/NewCompactor/
// DefaultCompactorConfig/WithAIOMode/WithAIOIOPoll）集中于此；
// 其余文件（write.go / read.go / compact.go / admin.go / options.go）只保留 Storage 方法
// 实现与私有辅助。新增对外符号一律收敛到本文件，避免导出面散落。
package storage

import (
	"context"
	"sync"
	"time"

	"github.com/liucxer/taihu/internal/aio"
	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
)

// Storage 对外 object 存储。数据写底层裸设备，key→位置映射经由 metastore（pebble）持久化，
// 其内部自带元数据加速缓存；Storage 不感知缓存细节。
//
// Storage 不持有写游标状态；「申请写位置」下沉到 db（metastore.Store.AllocateSegment），
// 其内部锁只覆盖廉价的游标分配，真正的设备写在此锁外执行 → 多 Put 可并发写不同偏移。
type Storage struct {
	db  metastore.Store
	dev *device.Device

	// 设备物理布局（启动时按读取的真实 nvme 容量计算注入，供对象上限/容量上报）。
	layout layout.Layout
}

// NewStorage 构建 Storage：打开 pebble、打开裸设备。l 为设备物理布局（段大小/段数），
// 由启动时读取的真实设备容量经 layout.ComputeLayout 计算。写游标由 db 首次分配时懒加载恢复。
// 磁盘异步 IO 后端由 opts 选择（默认 auto：支持 io_uring 则用）。
// 返回 (*Storage, error)，与 v1 文档略有出入，便于暴露初始化错误。
func NewStorage(ctx context.Context, rocksdbDir, nvmePath string, l layout.Layout, opts ...Option) (*Storage, error) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}

	db, err := metastore.Open(rocksdbDir, l)
	if err != nil {
		return nil, err
	}
	dev, err := device.NewDevice(ctx, nvmePath, l.SegmentSizeBytes,
		device.WithAIOMode(o.aioMode), device.WithAIOIOPoll(o.aioIOPoll))
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	s := &Storage{
		db:     db,
		dev:    dev,
		layout: l,
	}
	return s, nil
}

// MaxObjectSize 单对象大小上限（= 单段大小，Put/传输层校验用）。
func (s *Storage) MaxObjectSize() int64 { return s.layout.SegmentSizeBytes }

// LoadCache 预热 store（metastore）内部的元数据加速缓存。用于 bench / 已知 key 集合场景，
// 消除 GetMapping 的未命中回查，从而测纯设备读写带宽。
func (s *Storage) LoadCache(ctx context.Context) error {
	return s.db.LoadCache(ctx)
}

// Close 关闭底层设备与 pebble。失败时合并返回首个错误。
func (s *Storage) Close() error {
	var err error
	if s.dev != nil {
		err = s.dev.Close()
	}
	if s.db != nil {
		if e := s.db.Close(); err == nil {
			err = e
		}
	}
	return err
}

// RefSegment / UnrefSegment 透出段读引用：GET 级快照须在整段读取期间持有快照段引用，
// 否则覆盖写/删除后该段存活计数归零转 Reclaiming 并被 GC 复用，迟到的 chunk 会读到
// 他人数据（比版本混合更严重的静默损坏）。
func (s *Storage) RefSegment(segmentID int64)   { s.db.RefSegment(segmentID) }
func (s *Storage) UnrefSegment(segmentID int64) { s.db.UnrefSegment(segmentID) }

// BatchReadBlock 批读的一个对象块（直读快路径前置：Off/Size 4K 对齐，Size 为整块）。
type BatchReadBlock struct {
	Key  string
	Off  int64
	Size int64
	Dst  []byte // 4K 对齐，cap ≥ align4K(Size)（请求窗口 + 尾部对齐余量）
}

// BatchedReadResult 单块批读结果。
type BatchedReadResult struct {
	N   int64 // 请求窗口读入字节数（读到对象末尾不足 Size 时截断）
	Err error // io.EOF 表示读到对象/设备末尾短读；其余为映射/设备错误
}

// CompactorConfig 后台段压缩（compaction）配置。
// 对应设计文档《segment 级 Compaction（数据迁移）设计方案》。
type CompactorConfig struct {
	Interval        time.Duration // 扫描周期
	HoleThreshold   float64       // 单段空洞率阈值：≥ 该值的 Full 段入候选
	ForceWatermark  float64       // 全局水位：已用段占比 ≥ 该值强制压缩（不等单段阈值）
	MaxMovePerRound int           // 每轮搬移对象数上限（限速，防冲击稳态带宽）
}

// DefaultCompactorConfig 返回默认配置：60s 低频扫描、空洞 80% 或段水位 80% 触发、每轮 ≤512 对象。
func DefaultCompactorConfig() CompactorConfig {
	return CompactorConfig{
		Interval:        time.Minute,
		HoleThreshold:   0.8,
		ForceWatermark:  0.8,
		MaxMovePerRound: 512,
	}
}

// Compactor 后台段压缩器：把高空洞 Full 段（含中断未完成的 Compacting 段）的存活对象
// 搬移到新位置（可动用预留缓冲段），搬空后旧段计数归零自动转 Reclaiming，
// 由现有后台 GC 回收入池复用。与 GC 为两条独立惰性链，互不冲突。
// 实现（run/compactOnce/moveObject）见 compact.go。
type Compactor struct {
	st   *Storage
	cfg  CompactorConfig
	stop chan struct{}
	wg   sync.WaitGroup
}

// NewCompactor 构造压缩器（不启动；调用 Start/Stop 管理生命周期）。
func NewCompactor(st *Storage, cfg CompactorConfig) *Compactor {
	return &Compactor{st: st, cfg: cfg, stop: make(chan struct{})}
}

// Option 是 NewStorage 的可选参数（变参选项）。新增配置项时在此扩展，
// 既有调用点无需改动签名。具体选项字段见 options.go。
type Option func(*options)

// WithAIOMode 指定底层磁盘异步 IO 后端：aio.ModeAuto / ModeLibAIO / ModeIOUring。
// 由命令行 -io-uring=auto|on|off 映射而来（on→ModeIOUring，off→ModeLibAIO）。
// ModeIOUring 在内核不支持时启动失败（不静默降级）。
func WithAIOMode(m aio.Mode) Option {
	return func(o *options) { o.aioMode = m }
}

// WithAIOIOPoll 启用 io_uring 的 IOPOLL 模式（仅 io_uring 后端生效）。默认关闭：
// 它需要块设备队列开启轮询，且完成靠内核忙等推进（占一个核），收益需实测。
func WithAIOIOPoll(on bool) Option {
	return func(o *options) { o.aioIOPoll = on }
}

// IOStats 返回底层设备磁盘 IO 尺寸统计（4MiB 整块 vs 其他）。压测/验证用。
func (s *Storage) IOStats() (io4M, ioOther, bytes4M, bytesOther int64) {
	return s.dev.Stats()
}

// GetDiskCapacity 返回整盘容量/可用/已用字节（集群注册与心跳上报用）。
// Capacity 为布局总容量（segmentSize×segmentCount，由启动时读取的真实 nvme 容量计算），
// Used 为已写物理字节（metastore 按段状态统计），Available = Capacity - Used。
func (s *Storage) GetDiskCapacity() (capacity, available, used int64, err error) {
	capacity = s.layout.SegmentSizeBytes * s.layout.SegmentCount
	used = s.db.UsedBytes()
	available = capacity - used
	return capacity, available, used, nil
}

// SegmentStats 返回各状态 segment 数量（GC/回收/复用验证用）。
func (s *Storage) SegmentStats() map[metastore.SegmentState]int {
	return s.db.SegmentStats()
}
