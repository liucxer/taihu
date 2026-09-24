package storage

import (
	"context"
	"fmt"
	"io"

	"github.com/liucxer/taihu/internal/device"
	"github.com/liucxer/taihu/internal/layout"
	"github.com/liucxer/taihu/internal/metastore"
	"github.com/liucxer/taihu/pkg/ierr"
)

// 读路径：Storage 方法的实现（方法非顶层声明，允许留在本文件；全部 public 顶层
// 声明集中在 storage.go）。对外不要求 off/size 对齐——Storage 层吸收 O_DIRECT 的
// 4K 对齐细节：物理读下/上对齐，请求窗口原址平移或直读。

// Meta 返回 key 当前的对象映射快照（不存在返回 ierr.ErrNotFound）。一次 GET 需要跨多个
// chunk 读取同一 key 时，入口解析一次并全程复用，避免逐 chunk 重解析在多 chunk 响应内
// 混合两个版本（详见 ReadAtMeta）。
func (s *Storage) Meta(ctx context.Context, key string) (metastore.ObjectMeta, error) {
	return s.db.GetMapping(ctx, key)
}

// ReadAt 读取对象内 [off, off+size) 区间的数据并返回（返回值为从 bufpool 取出的池化
// 缓冲或 nil；调用方用毕必须 bufpool.Put(返回值) 归还，否则造成池泄漏）。
//
// 错误与边界：
//   - off < 0 或 off > Size：ierr.ErrInvalidRange；
//   - 请求超出对象结尾：截断到剩余字节，返回的部分不足 size 时附 io.EOF；
//   - off == Size（剩余 0）：返回 (nil, io.EOF)。
func (s *Storage) ReadAt(ctx context.Context, key string, off, size int64) ([]byte, error) {
	meta, err := s.db.GetMapping(ctx, key)
	if err != nil {
		return nil, err
	}
	return s.ReadAtMeta(ctx, meta, off, size)
}

// Stat 返回对象逻辑大小。远程层（taihu-server）Get size=-1 全量读等场景使用。
// key 不存在时返回 ierr.ErrNotFound。
func (s *Storage) Stat(ctx context.Context, key string) (int64, error) {
	meta, err := s.db.GetMapping(ctx, key)
	if err != nil {
		return 0, err
	}
	return meta.Size, nil
}

// readWindow 计算对象内 [off, off+size) 请求窗口对应的段内物理读区间，供所有读路径
// （ReadAtMeta / readAtIntoMeta / BatchRead）共用，收敛「校验 → 截断 → 4K 对齐」逻辑。
// 语义与各方法原实现逐字等价：
//   - off 越界（<0 或 >meta.Size）返回 ierr.ErrInvalidRange；
//   - alignedOff=true 时额外要求 off 4K 对齐（O_DIRECT 直读快路径，skip 恒 0）；
//   - 请求超出对象结尾截断到剩余字节；空窗口（remaining==0）返回 io.EOF。
//
// 返回 relStart（物理读起点，段内偏移）、dlen（向上 4K 对齐的物理读长）、
// want（请求窗口字节数）、skip（请求窗口相对物理起点偏移，供池化缓冲原址平移）。
func readWindow(meta metastore.ObjectMeta, off, size int64, alignedOff bool) (relStart, dlen, want, skip int64, err error) {
	if off < 0 || off > meta.Size || (alignedOff && off%layout.BlockSize != 0) {
		return 0, 0, 0, 0, ierr.ErrInvalidRange
	}
	remaining := meta.Size - off
	want = size
	if want > remaining {
		want = remaining
	}
	if want == 0 {
		return 0, 0, 0, 0, io.EOF
	}
	relStart = meta.Offset + off
	start := relStart
	if !alignedOff {
		start &^= layout.BlockSize - 1
	}
	dlen = layout.Align4k(relStart+want) - start
	skip = relStart - start
	return relStart, dlen, want, skip, nil
}

// ReadAtMeta 与 ReadAt 同语义（含 bufpool 归还契约），区别是映射由调用方以快照形式
// 提供：一次 GET 跨多个 chunk 时入口解析一次 meta 并全程复用，使同一响应严格来自单一
// 版本。调用方负责快照存活 —— 跨 chunk 期间须 RefSegment/UnrefSegment 持有
// meta.SegmentID 引用，防止该段被 GC 回收复用（见 RefSegment）。
func (s *Storage) ReadAtMeta(ctx context.Context, meta metastore.ObjectMeta, off, size int64) ([]byte, error) {
	_, dlen, want, skip, err := readWindow(meta, off, size, false)
	if err != nil {
		return nil, err // ierr.ErrInvalidRange 或 io.EOF（空窗口）
	}

	// 读引用计数：防 GC 在读在途时回收并复用该段（迟到读读错数据）。
	s.db.RefSegment(meta.SegmentID)
	defer s.db.UnrefSegment(meta.SegmentID)

	// 物理读起点 = relStart − skip（readWindow 已下/上 4K 对齐）。
	data, err := s.dev.ReadAt(ctx, meta.SegmentID, meta.Offset+off-skip, dlen)
	if err != nil {
		return nil, err
	}
	if n := int64(len(data)); n < want {
		// 设备不足（对象末尾）：返回已读前缀，调用方按 io.EOF 收尾。
		want = n
	}
	if skip > 0 || want < int64(len(data)) {
		// 非对齐窗口：池化缓冲内原址左移，只保留 [skip, skip+want)。
		n := copy(data, data[skip:skip+want])
		data = data[:n]
	}
	if int64(len(data)) < size {
		return data, io.EOF
	}
	return data, nil
}

// readAtInto 读取对象内 [off, off+size) 区间的数据，直接 DMA 进调用方 dst
// （服务端 shm 直读共享内存用：免 bufpool→共享内存 memcpy）。无外部调用，私有
// （shm 直读走 readAtIntoMeta 快照路径）。
//
// 要求：
//   - off 4K 对齐（O_DIRECT 直读快路径，skip==0；非对齐 off 由调用方回退 ReadAt+拷贝）；
//   - dst 首地址 4K 对齐（bufAligned）且 cap ≥ align4K(want)（物理读区间含尾部对齐余量）。
//
// 返回实际读入的请求窗口字节数（读到对象末尾不足 size 时截断；remaining==0 返回
// (0, io.EOF)）。dst 中 [0, 返回 n) 为有效数据。
func (s *Storage) readAtInto(ctx context.Context, key string, off, size int64, dst []byte) (int64, error) {
	meta, err := s.db.GetMapping(ctx, key)
	if err != nil {
		return 0, err
	}
	return s.ReadAtIntoMeta(ctx, meta, off, size, dst)
}

// ReadAtIntoMeta 与 ReadAtInto 同语义，映射由调用方以快照形式提供（快照存活责任同
// ReadAtMeta：跨 chunk 期间须持有 meta.SegmentID 段引用）。
func (s *Storage) ReadAtIntoMeta(ctx context.Context, meta metastore.ObjectMeta, off, size int64, dst []byte) (int64, error) {
	// off 4K 对齐 + meta.Offset 4K 对齐（写入保证）→ skip==0，dstart = relStart。
	relStart, dlen, want, _, err := readWindow(meta, off, size, true)
	if err != nil {
		return 0, err // ierr.ErrInvalidRange 或 io.EOF（空窗口）
	}
	if int64(len(dst)) < dlen {
		return 0, fmt.Errorf("taihu: readinto dst %d < dlen %d", len(dst), dlen)
	}

	// 读引用计数：防 GC 在读在途时回收并复用该段（迟到读读错数据）。
	s.db.RefSegment(meta.SegmentID)
	defer s.db.UnrefSegment(meta.SegmentID)

	n, err := s.dev.ReadAtInto(ctx, meta.SegmentID, relStart, dlen, dst[:dlen])
	if err != nil {
		return 0, err
	}
	if n < want {
		// 设备不足（对象末尾）：返回已读前缀，调用方按 EOF 收尾。
		want = n
	}
	if want < size {
		return want, io.EOF
	}
	return want, nil
}

// BatchRead 一次性批读多个对象块：把各块解析为设备直读 job 后，交给 device 以一次
// io_submit 批量提交（摊薄系统调用），完成后再返回各块结果。逐块语义与 ReadAtInto
// 完全等价（cost 短读截断 + io.EOF）。供服务端"多 stream 一 worker 聚合读"使用。
func (s *Storage) BatchRead(ctx context.Context, blocks []BatchReadBlock) ([]BatchedReadResult, error) {
	res := make([]BatchedReadResult, len(blocks))
	if len(blocks) == 0 {
		return res, nil
	}
	// Phase 1：批量查对象映射（单快照 + 单迭代，摊薄 N 次 pebble Get）→ 全量校验并
	// 构建段内物理区间 device.ReadJob。此阶段不持读引用——任一块校验失败即返回，
	// 不会遗留未释放的 Ref（防引用泄漏抢占 GC 回收）。
	keys := make([]string, len(blocks))
	for i := range blocks {
		keys[i] = blocks[i].Key
	}
	metas, err := s.db.BatchGetMapping(ctx, keys)
	if err != nil {
		return res, err
	}
	devJobs := make([]device.ReadJob, len(blocks))
	refed := make([]bool, len(blocks))
	wants := make([]int64, len(blocks))
	for i := range blocks {
		b := &blocks[i]
		meta := metas[i]
		relStart, dlen, want, _, err := readWindow(meta, b.Off, b.Size, true)
		if err != nil {
			if err == io.EOF { // 空窗口：该块置 EOF，不构建设备 job
				res[i] = BatchedReadResult{N: 0, Err: io.EOF}
				continue
			}
			return res, err // ierr.ErrInvalidRange（off 越界/未对齐）
		}
		if int64(len(b.Dst)) < dlen {
			return res, fmt.Errorf("taihu: batchread dst %d < dlen %d", len(b.Dst), dlen)
		}
		devJobs[i] = device.ReadJob{SegmentID: meta.SegmentID, Off: relStart, Buf: b.Dst[:dlen], Size: dlen}
		wants[i] = want
		refed[i] = true // Phase 2 中对该段持读引用
	}
	// 整批都无有效 job 时直接返回（已逐块置 EOF）。
	any := false
	for i := range devJobs {
		if refed[i] {
			any = true
			break
		}
	}
	if !any {
		return res, nil
	}
	// Phase 2：统一持段读引用 → 一次设备批提交 → defer 统一释放（任何错误路径都不泄漏）。
	for i := range blocks {
		if refed[i] {
			s.db.RefSegment(devJobs[i].SegmentID)
		}
	}
	defer func() {
		for i := range blocks {
			if refed[i] {
				s.db.UnrefSegment(devJobs[i].SegmentID)
			}
		}
	}()
	ns, err := s.dev.ReadAtIntoBatch(ctx, devJobs)
	if err != nil {
		return res, err
	}
	for i := range blocks {
		if refed[i] {
			n := ns[i]
			want := wants[i]
			if n < want {
				want = n
			}
			if want < blocks[i].Size {
				res[i] = BatchedReadResult{N: want, Err: io.EOF}
			} else {
				res[i] = BatchedReadResult{N: want}
			}
		}
	}
	return res, nil
}
