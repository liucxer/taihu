package aio

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/liucxer/taihu/internal/ierr"
)

// 本文件是**平台无关**的契约测试（无 build tag，全平台编译运行）——平台入口见
// aio_linux_test.go / aio_darwin_test.go：
//
//   - aio_test.go          本文件：平台无关的契约与选项用例
//   - aio_linux_test.go    Linux 专属（libaio / io_uring / 探测 / 4.19 与 5.10 两份入口）
//   - aio_darwin_test.go   macOS 入口（只有兜底实现，无 O_DIRECT）
//
// 入口文件只负责「本环境跑哪些后端、哪些通道，外加哪些平台事实断言」；契约本体
// （runRingContract）在这里，Ring 的 6 个方法全部覆盖，IO 尺寸覆盖 ioSizeTable 的
// 每一档（4K…4M），且每次都用独立于 ring 的第二个 fd 双向校验数据一致性。
//
// 为什么按平台拆入口：build tag 只能按 GOOS/GOARCH 分，分不出内核版本，所以 4.19 与
// 5.10 两份会同时编译进同一个测试二进制（见 aio_linux_test.go），靠运行期内核门控
// （requireKernel，见 aio_linux_test.go 的测试基建部分）只让匹配的那一份真正跑。

const testChunk = 4096

// ioSizeTable 契约覆盖的 IO 尺寸档位。上限取 4M 是本项目的实际边界：RPC 帧 4MiB
// （internal/transport）、bufpool 最大桶、device 单次读写粒度都在这个量级。
// 全部是 4K 的整数倍 —— O_DIRECT 通道要求长度与偏移都按 4K 对齐。
var ioSizeTable = []int{4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20, 4 << 20}

const (
	// contractDepth 契约主用的队列深度：要装得下 batchCount 的最大批量（64 条）。
	contractDepth = 128
)

// ioTimeout Wait 的上限。正常 IO 远快于它；一旦超时说明请求根本没完成（例如 IOPOLL
// 设备未开队列轮询时请求会停在 iopoll_list 上），要报错而不是把测试挂死。
var ioTimeout = 30 * time.Second

// backend 一个待测的后端实现。new 负责建好队列并绑定目标 fd；后端在当前机器不可用时应
// t.Skipf（带上原因，避免「静默跳过」在日志里看起来和「通过」一样）。
type backend struct {
	name string
	new  func(t *testing.T, fd int, maxEvents int) Ring
	// depthLimit 该后端是否有队列深度上限（满了会返回 ErrFull）。
	// libaio/io_uring 有；非 Linux 兜底实现没有（逐条起 goroutine，永不 ErrFull）。
	depthLimit bool
}

// testChannel 一条文件通道：文件怎么开、缓冲怎么分配。
//
// 为什么要分通道：同一套契约要在「普通缓冲 IO」与「O_DIRECT」两种形态下各跑一遍 ——
// 后者是生产形态（device 层用 O_DIRECT 打开设备），但语言层面没有 O_DIRECT 常量
// （darwin 的 x/sys/unix 里没有该符号），故实现只放在 Linux 侧（aio_linux_test.go 的测试基建部分）。
type testChannel struct {
	name string
	// open 把同一个定长文件打开两次，返回两个独立 fd：ring 提交用一个，
	// 独立校验（不经 ring）用另一个。文件系统不支持该通道时 t.Skipf。
	open func(t *testing.T, size int64) (ring, verify *os.File)
	// buf 按通道要求分配缓冲：O_DIRECT 通道必须 4K 对齐。
	buf func(n int) []byte
}

// plainChannel 普通缓冲 IO 通道，全平台可用。
func plainChannel() testChannel {
	return testChannel{
		name: "plain",
		open: func(t *testing.T, size int64) (ring, verify *os.File) {
			// 两个 fd 必须指向同一个文件：t.TempDir() 每次调用都给一个新目录，
			// 故路径只算一次。
			path := filepath.Join(t.TempDir(), "aio-dev")
			return newTestFileAt(t, path, size), newTestFileAt(t, path, size)
		},
		buf: func(n int) []byte { return make([]byte, n) },
	}
}

// newTestFile 建一个定长临时文件并返回句柄。
func newTestFile(t *testing.T, size int64) *os.File {
	t.Helper()
	return newTestFileAt(t, filepath.Join(t.TempDir(), "aio-dev"), size)
}

// newTestFileAt 按给定路径建定长文件并返回句柄。同一路径可以开成多个独立 fd
// （plainChannel 就是这样给「ring 提交」与「独立校验」各拿一个 fd 的）。
func newTestFileAt(t *testing.T, path string, size int64) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open temp file: %v", err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// pattern 生成确定性数据块。
func pattern(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

// sizeName 把尺寸档位格式化成子测试名（4K/16K/…/4M）。
func sizeName(size int) string {
	if size%(1<<20) == 0 {
		return fmt.Sprintf("%dM", size>>20)
	}
	return fmt.Sprintf("%dK", size>>10)
}

// batchCount 该尺寸下批量用例的条目数：总量压在 4M 上下、条数不超过 64，
// 既覆盖「一次提交多条」，又不让 4M 档吃掉过多内存与时间。
func batchCount(size int) int {
	n := (4 << 20) / size
	if n < 2 {
		n = 2
	}
	if n > 64 {
		n = 64
	}
	return n
}

// ── 契约本体 ─────────────────────────────────────────────────────────

// runRingContract 对单个后端 + 单条文件通道跑完整契约。子测试名即覆盖点，
// `go test -v` 的输出可以直接当覆盖清单读。
func runRingContract(t *testing.T, b backend, ch testChannel) {
	t.Run("roundtrip", func(t *testing.T) { contractRoundTrip(t, b, ch) })
	t.Run("batch", func(t *testing.T) { contractBatch(t, b, ch) })
	t.Run("inflight_mixed", func(t *testing.T) { contractInflightMixed(t, b, ch) })
	t.Run("wait_semantics", func(t *testing.T) { contractWaitSemantics(t, b, ch) })
	t.Run("read_beyond_eof", func(t *testing.T) { contractReadBeyondEOF(t, b, ch) })
	t.Run("submit_after_close", func(t *testing.T) { contractSubmitAfterClose(t, b, ch) })
	t.Run("fd_survives_gc", func(t *testing.T) { contractFdSurvivesGC(t, b, ch) })
	if b.depthLimit {
		t.Run("queue_full", func(t *testing.T) { contractQueueFull(t, b, ch) })
	}
}

// contractRoundTrip 覆盖 SubmitWrite / SubmitRead / Wait：每档尺寸都双向校验 ——
// ring 写 → 独立 fd（不经 ring）读回比对；独立 fd 写 → ring 读回比对。
func contractRoundTrip(t *testing.T, b backend, ch testChannel) {
	for _, size := range ioSizeTable {
		t.Run(sizeName(size), func(t *testing.T) {
			f, vf := ch.open(t, int64(size))
			r := b.new(t, int(f.Fd()), contractDepth)
			defer closeRing(t, r)

			// 方向一：ring 写 → 独立读校验
			want := pattern(0x5A, size)
			wbuf := ch.buf(size)
			copy(wbuf, want)
			if ev := ringWrite(t, r, wbuf, 0); ev.Res != int64(size) {
				t.Fatalf("写完成字节数 = %d, want %d", ev.Res, size)
			}
			got := ch.buf(size)
			fileReadAt(t, vf, got, 0)
			if !bytes.Equal(got, want) {
				t.Fatalf("ring 写 → 独立读回 数据不一致（size=%s）", sizeName(size))
			}

			// 方向二：独立写 → ring 读校验（换一个 pattern，两方向不互相掩盖）
			want2 := pattern(0xF0, size)
			wbuf2 := ch.buf(size)
			copy(wbuf2, want2)
			fileWriteAt(t, vf, wbuf2, 0)
			rbuf := ch.buf(size)
			if ev := ringRead(t, r, rbuf, 0); ev.Res != int64(size) {
				t.Fatalf("读完成字节数 = %d, want %d", ev.Res, size)
			}
			if !bytes.Equal(rbuf, want2) {
				t.Fatalf("独立写 → ring 读回 数据不一致（size=%s）", sizeName(size))
			}
		})
	}
}

// contractBatch 覆盖 SubmitWriteBatch / SubmitReadBatch：每档尺寸一次提交多条
// （同一 fd），校验序号连续不重叠、每条完成字节数，以及与独立通道的双向一致性。
func contractBatch(t *testing.T, b backend, ch testChannel) {
	for _, size := range ioSizeTable {
		t.Run(sizeName(size), func(t *testing.T) {
			n := batchCount(size)
			f, vf := ch.open(t, int64(n)*int64(size))
			r := b.new(t, int(f.Fd()), contractDepth)
			defer closeRing(t, r)

			// 方向一：SubmitWriteBatch → Wait → 独立读校验
			want := make([][]byte, n)
			bufs := make([][]byte, n)
			specs := make([]WriteSpec, n)
			for i := range specs {
				want[i] = pattern(byte(i+1), size)
				bufs[i] = ch.buf(size)
				copy(bufs[i], want[i])
				specs[i] = WriteSpec{Buf: bufs[i], Off: int64(i) * int64(size)}
			}
			// 批量入口允许部分排队（submitted < len(specs)：内核提交队列截断），按 Ring
			// 文档的约定把未排队部分追加提交，直到全部排入；每条的序号按「调用内
			// firstSeq+i」关联，跨轮不得重复。
			seqOf := make([]uint64, n)
			for done := 0; done < n; {
				first, submitted, err := r.SubmitWriteBatch(specs[done:])
				if err != nil {
					t.Fatalf("SubmitWriteBatch（第 %d/%d 条起）: %v", done, n, err)
				}
				if submitted == 0 {
					t.Fatalf("SubmitWriteBatch 排队 0 条却未报错（第 %d/%d 条起）", done, n)
				}
				for i := 0; i < submitted; i++ {
					seqOf[done+i] = first + uint64(i)
				}
				done += submitted
			}
			evs, err := r.Wait(n, n, &ioTimeout)
			if err != nil {
				t.Fatalf("Wait(写批次): %v", err)
			}
			assertBatchEvents(t, evs, seqOf, size, "写")
			runtime.KeepAlive(bufs) // 缓冲须存活到事件被取回

			got := ch.buf(size)
			for i := 0; i < n; i++ {
				fileReadAt(t, vf, got, int64(i)*int64(size))
				if !bytes.Equal(got, want[i]) {
					t.Fatalf("写批次第 %d/%d 块数据不一致（size=%s）", i+1, n, sizeName(size))
				}
			}

			// 方向二：独立写 → SubmitReadBatch → Wait → 比对
			wantR := make([][]byte, n)
			ob := ch.buf(size)
			for i := range wantR {
				wantR[i] = pattern(byte(0x80+i), size)
				copy(ob, wantR[i])
				fileWriteAt(t, vf, ob, int64(i)*int64(size))
			}
			rbufs := make([][]byte, n)
			rspecs := make([]ReadSpec, n)
			for i := range rspecs {
				rbufs[i] = ch.buf(size)
				rspecs[i] = ReadSpec{Buf: rbufs[i], Off: int64(i) * int64(size)}
			}
			// 读方向同样按「追加提交未排队部分」处理，序号逐条记录。
			rseqOf := make([]uint64, n)
			var firstR uint64
			for done := 0; done < n; {
				first, submitted, errR := r.SubmitReadBatch(rspecs[done:])
				if errR != nil {
					t.Fatalf("SubmitReadBatch（第 %d/%d 条起）: %v", done, n, errR)
				}
				if submitted == 0 {
					t.Fatalf("SubmitReadBatch 排队 0 条却未报错（第 %d/%d 条起）", done, n)
				}
				if done == 0 {
					firstR = first
				}
				for i := 0; i < submitted; i++ {
					rseqOf[done+i] = first + uint64(i)
				}
				done += submitted
			}
			if firstR <= seqOf[n-1] {
				t.Fatalf("读批次首序号 %d 未接在写批次末序号 %d 之后（序号重叠）", firstR, seqOf[n-1])
			}
			evs, err = r.Wait(n, n, &ioTimeout)
			if err != nil {
				t.Fatalf("Wait(读批次): %v", err)
			}
			assertBatchEvents(t, evs, rseqOf, size, "读")
			runtime.KeepAlive(rbufs)
			for i := range rbufs {
				if !bytes.Equal(rbufs[i], wantR[i]) {
					t.Fatalf("读批次第 %d/%d 块数据不一致（size=%s）", i+1, n, sizeName(size))
				}
			}
		})
	}
}

// assertBatchEvents 校验一批完成事件：条数正确、序号与 seqOf 一一对应（不重不漏）、
// 每条完成字节数等于 size。
func assertBatchEvents(t *testing.T, evs []Event, seqOf []uint64, size int, what string) {
	t.Helper()
	n := len(seqOf)
	if len(evs) != n {
		t.Fatalf("%s批次取回 %d 个事件, want %d", what, len(evs), n)
	}
	res := make(map[uint64]int64, n)
	for _, ev := range evs {
		if _, dup := res[ev.Data]; dup {
			t.Fatalf("%s批次序号 %d 被取回两次", what, ev.Data)
		}
		res[ev.Data] = ev.Res
	}
	for i, seq := range seqOf {
		got, ok := res[seq]
		if !ok {
			t.Fatalf("%s批次序号 %d（第 %d 条）未取回", what, seq, i)
		}
		if got != int64(size) {
			t.Fatalf("%s批次序号 %d 完成字节数 = %d, want %d", what, seq, got, size)
		}
	}
}

// contractInflightMixed 多请求同时在途、且各请求尺寸互不相同：校验完成事件靠
// Event.Data 关联（Linux 上完成顺序不保证与提交顺序一致），以及混合尺寸下每条的
// 字节数与内容都对得上。
func contractInflightMixed(t *testing.T, b backend, ch testChannel) {
	offs := make([]int64, len(ioSizeTable))
	var total int64
	for i, size := range ioSizeTable {
		offs[i] = total
		total += int64(size)
	}
	f, vf := ch.open(t, total)
	r := b.new(t, int(f.Fd()), contractDepth)
	defer closeRing(t, r)

	// 写：把各档尺寸全部发出后再统一收割。
	want := make([][]byte, len(ioSizeTable))
	bufs := make([][]byte, len(ioSizeTable))
	seqs := make([]uint64, len(ioSizeTable))
	for i, size := range ioSizeTable {
		want[i] = pattern(byte(0x10+i), size)
		bufs[i] = ch.buf(size)
		copy(bufs[i], want[i])
		seq, err := r.SubmitWrite(bufs[i], offs[i])
		if err != nil {
			t.Fatalf("SubmitWrite(size=%s): %v", sizeName(size), err)
		}
		seqs[i] = seq
	}
	evs, err := r.Wait(len(ioSizeTable), len(ioSizeTable), &ioTimeout)
	if err != nil {
		t.Fatalf("Wait(混合尺寸写): %v", err)
	}
	if len(evs) != len(ioSizeTable) {
		t.Fatalf("混合尺寸写取回 %d 个事件, want %d", len(evs), len(ioSizeTable))
	}
	res := make(map[uint64]int64, len(evs))
	for _, ev := range evs {
		res[ev.Data] = ev.Res
	}
	for i, size := range ioSizeTable {
		got, ok := res[seqs[i]]
		if !ok {
			t.Fatalf("size=%s 的写（序号 %d）未取回", sizeName(size), seqs[i])
		}
		if got != int64(size) {
			t.Fatalf("size=%s 的写完成字节数 = %d, want %d", sizeName(size), got, size)
		}
	}
	runtime.KeepAlive(bufs)

	// 独立读回整段，逐档比对。
	for i, size := range ioSizeTable {
		got := ch.buf(size)
		fileReadAt(t, vf, got, offs[i])
		if !bytes.Equal(got, want[i]) {
			t.Fatalf("size=%s 的写数据与独立读回不一致", sizeName(size))
		}
	}

	// 读：同样多请求在途。
	rbufs := make([][]byte, len(ioSizeTable))
	rseqs := make([]uint64, len(ioSizeTable))
	for i, size := range ioSizeTable {
		rbufs[i] = ch.buf(size)
		seq, err := r.SubmitRead(rbufs[i], offs[i])
		if err != nil {
			t.Fatalf("SubmitRead(size=%s): %v", sizeName(size), err)
		}
		rseqs[i] = seq
	}
	evs, err = r.Wait(len(ioSizeTable), len(ioSizeTable), &ioTimeout)
	if err != nil || len(evs) != len(ioSizeTable) {
		t.Fatalf("Wait(混合尺寸读): 取回 %d 个事件, err=%v, want %d, nil",
			len(evs), err, len(ioSizeTable))
	}
	res = make(map[uint64]int64, len(evs))
	for _, ev := range evs {
		res[ev.Data] = ev.Res
	}
	for i, size := range ioSizeTable {
		if got := res[rseqs[i]]; got != int64(size) {
			t.Fatalf("size=%s 的读完成字节数 = %d, want %d", sizeName(size), got, size)
		}
		if !bytes.Equal(rbufs[i], want[i]) {
			t.Fatalf("size=%s 的读回数据不一致", sizeName(size))
		}
	}
	runtime.KeepAlive(rbufs)
}

// contractWaitSemantics 覆盖 Wait 的三种出口：按 max 截断、max<=0 立即返回、超时
// （零超时与到期超时都必须返回 ErrTimeout，不得提前返回，也不得挂死）。
func contractWaitSemantics(t *testing.T, b backend, ch testChannel) {
	const n = 4
	f, _ := ch.open(t, int64(n)*int64(testChunk))
	r := b.new(t, int(f.Fd()), contractDepth)
	defer closeRing(t, r)

	bufs := make([][]byte, n)
	for i := 0; i < n; i++ {
		bufs[i] = ch.buf(testChunk)
		if _, err := r.SubmitRead(bufs[i], int64(i)*int64(testChunk)); err != nil {
			t.Fatalf("SubmitRead#%d: %v", i, err)
		}
	}
	runtime.KeepAlive(bufs)

	// max 截断：单次取回不得超过 max。这里不要求「每次正好取回两条」——
	// 内核在满足 min 后唤醒，那一刻可用事件数未必正好等于 max。
	seen := make(map[uint64]bool, n)
	reaped := 0
	for round := 1; reaped < n; round++ {
		if round > n+1 {
			t.Fatalf("多轮 Wait 后仍有事件未取回（已取 %d/%d）", reaped, n)
		}
		evs, err := r.Wait(1, 2, &ioTimeout)
		if err != nil {
			t.Fatalf("Wait(1,2) 第 %d 轮: %v", round, err)
		}
		if len(evs) == 0 {
			t.Fatalf("Wait(1,2) 第 %d 轮未取回事件（min=1）", round)
		}
		if len(evs) > 2 {
			t.Fatalf("Wait(1,2) 第 %d 轮取回 %d 个事件，超过 max=2", round, len(evs))
		}
		for _, ev := range evs {
			if seen[ev.Data] {
				t.Fatalf("序号 %d 被取回两次", ev.Data)
			}
			seen[ev.Data] = true
			if ev.Res != testChunk {
				t.Fatalf("seq=%d 完成字节数 = %d, want %d", ev.Data, ev.Res, testChunk)
			}
			reaped++
		}
	}

	// max<=0：直接返回空，不进内核。
	if evs, err := r.Wait(1, 0, nil); err != nil || len(evs) != 0 {
		t.Fatalf("Wait(max=0) = (%d 个事件, %v), want (0, nil)", len(evs), err)
	}

	// 零超时：纯 deadline 判定，必须立刻 ErrTimeout 且不带事件。
	zero := time.Duration(0)
	if evs, err := r.Wait(1, 4, &zero); err != ierr.ErrTimeout || len(evs) != 0 {
		t.Fatalf("Wait(零超时) = (%d 个事件, %v), want (0, ErrTimeout)", len(evs), err)
	}

	// 到期超时：同样无事件，且不得提前返回（此时已在途请求为 0）。
	d := 50 * time.Millisecond
	start := time.Now()
	evs, err := r.Wait(1, 4, &d)
	elapsed := time.Since(start)
	if err != ierr.ErrTimeout {
		t.Fatalf("Wait(到期超时) err=%v, want ErrTimeout（取回 %d 个事件）", err, len(evs))
	}
	if len(evs) != 0 {
		t.Fatalf("无在途请求时不该有事件，却取回 %d 个", len(evs))
	}
	if elapsed < d/2 {
		t.Fatalf("Wait 提前返回: %v < %v", elapsed, d/2)
	}
}

// contractReadBeyondEOF 越过文件末尾的读返回 0 字节，而不是错误。
func contractReadBeyondEOF(t *testing.T, b backend, ch testChannel) {
	f, _ := ch.open(t, testChunk)
	r := b.new(t, int(f.Fd()), 4)
	defer closeRing(t, r)

	ev := ringRead(t, r, ch.buf(testChunk), 2*testChunk)
	if ev.Res != 0 {
		t.Fatalf("越界读完成字节数 = %d, want 0", ev.Res)
	}
}

// contractSubmitAfterClose Close 之后四个提交入口都必须报错（具体 errno 各后端不同：
// libaio 走 io_submit 的 EINVAL、io_uring 与兜底在入口拦下返回 EBADF），且 Close 幂等。
func contractSubmitAfterClose(t *testing.T, b backend, ch testChannel) {
	f, _ := ch.open(t, testChunk)
	r := b.new(t, int(f.Fd()), 4)
	buf := ch.buf(testChunk)

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("二次 Close 应幂等: %v", err)
	}
	if _, err := r.SubmitRead(buf, 0); err == nil {
		t.Error("Close 后 SubmitRead 应报错")
	}
	if _, err := r.SubmitWrite(buf, 0); err == nil {
		t.Error("Close 后 SubmitWrite 应报错")
	}
	if _, _, err := r.SubmitReadBatch([]ReadSpec{{Buf: buf}}); err == nil {
		t.Error("Close 后 SubmitReadBatch 应报错")
	}
	if _, _, err := r.SubmitWriteBatch([]WriteSpec{{Buf: buf}}); err == nil {
		t.Error("Close 后 SubmitWriteBatch 应报错")
	}
}

// contractFdSurvivesGC 回归：后端不得持有「用完即丢、却会关闭调用方 fd」的包装对象。
//
// 背景见 aio_fallback_other.go 里关于 os.NewFile finalizer 的注释：macOS 兜底实现曾把 fd 包成
// os.NewFile，包装对象成垃圾后 GC 会 close 掉**调用方持有的同一个 fd** —— 轻则随机
// EBADF，重则 kevent 报 EBADF 触发 runtime fatal（netpoll failed）整进程退出。
func contractFdSurvivesGC(t *testing.T, b backend, ch testChannel) {
	f, vf := ch.open(t, 2*int64(testChunk))
	r := b.new(t, int(f.Fd()), 8)
	defer closeRing(t, r)

	want := pattern(0x33, testChunk)
	wbuf := ch.buf(testChunk)
	copy(wbuf, want)

	roundTrip := func() {
		t.Helper()
		if ev := ringWrite(t, r, wbuf, 0); ev.Res != testChunk {
			t.Fatalf("写完成字节数 = %d, want %d", ev.Res, testChunk)
		}
		got := ch.buf(testChunk)
		fileReadAt(t, vf, got, 0)
		if !bytes.Equal(got, want) {
			t.Fatal("往返数据不一致")
		}
	}

	roundTrip()
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	roundTrip() // fd 必须仍然有效

	// 直接查原症状：fd 被别人的 finalizer 关掉时，这里会报 EBADF。
	if err := f.Close(); err != nil {
		t.Fatalf("GC 后 Close: %v（fd 被后端包装对象的 finalizer 关闭了？）", err)
	}
}

// contractQueueFull 覆盖队列占满后的 ErrFull 与「Wait 回收后可重试」—— 设备层正是靠
// 这条语义在 ErrFull 上重试（见 internal/device）。
//
// 深度上限是各后端自己的账：io_uring 是软件在途计数（只有收割才还），libaio 是内核
// ctx 容量（完成即还）。所以这里不去断言「第 depth+1 条必然 ErrFull」—— 完成快的请求
// 可能已经腾出容量；改为「填到报满为止」，再校验可恢复性。容量始终占不满则跳过并说明。
func contractQueueFull(t *testing.T, b backend, ch testChannel) {
	const (
		depth = 4   // 建环深度：越小越容易占满
		cap   = 512 // 填充次数上限：仍未报满说明本后端容量未被占满
	)
	f, _ := ch.open(t, int64(depth)*int64(testChunk))
	r := b.new(t, int(f.Fd()), depth)
	defer closeRing(t, r)

	bufs := make([][]byte, 0, cap)
	pending := 0
	var fullErr error
	for i := 0; i < cap; i++ {
		buf := ch.buf(testChunk)
		if _, err := r.SubmitRead(buf, int64(i%depth)*int64(testChunk)); err != nil {
			fullErr = err
			break
		}
		bufs = append(bufs, buf)
		pending++
	}
	if fullErr == nil {
		t.Skipf("连续提交 %d 条仍未报满，跳过（本后端在该负载下容量未被占满）", cap)
	}
	if fullErr != ierr.ErrFull {
		t.Fatalf("队列占满时 SubmitRead err=%v, want ErrFull", fullErr)
	}

	// 批量入口同样受深度上限约束：只能是 ErrFull 或成功，不得报别的错。
	if _, n, err := r.SubmitReadBatch([]ReadSpec{{Buf: ch.buf(testChunk), Off: 0}}); err != nil && err != ierr.ErrFull {
		t.Fatalf("占满时 SubmitReadBatch err=%v, want ErrFull 或 nil", err)
	} else {
		pending += n
	}

	// Wait 回收后必须能重新提交（设备层据此重试）。
	evs, err := r.Wait(1, cap, &ioTimeout)
	if err != nil {
		t.Fatalf("占满后 Wait: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("占满后 Wait 未取回任何事件")
	}
	pending -= len(evs)
	if _, err := r.SubmitRead(ch.buf(testChunk), 0); err != nil {
		t.Fatalf("Wait 回收 %d 条后 SubmitRead 仍失败: %v", len(evs), err)
	}
	pending++

	// 收尾：全部收割。不能在还有在途请求时 Close —— io_uring 下内核仍可能往这些缓冲
	// 写，而缓冲在 Go 堆上，Close 后随时可能被回收，等于让内核踩内存。
	for round := 1; pending > 0; round++ {
		if round > cap {
			t.Fatalf("收尾 Wait 未收敛，仍有 %d 条在途", pending)
		}
		evs, err := r.Wait(1, pending, &ioTimeout)
		if err != nil {
			t.Fatalf("收尾 Wait（剩 %d 条在途）: %v", pending, err)
		}
		pending -= len(evs)
	}
	runtime.KeepAlive(bufs)
}

// ── 契约内部的小工具 ─────────────────────────────────────────────────

// ringWrite 用 ring 把 data 写到 off 并等待其完成，返回完成事件（Data 即提交序号）。
func ringWrite(t *testing.T, r Ring, data []byte, off int64) Event {
	t.Helper()
	seq, err := r.SubmitWrite(data, off)
	if err != nil {
		t.Fatalf("SubmitWrite(off=%d, len=%d): %v", off, len(data), err)
	}
	evs, err := r.Wait(1, 1, &ioTimeout)
	if err != nil {
		t.Fatalf("Wait(写 off=%d, len=%d): %v", off, len(data), err)
	}
	if len(evs) != 1 || evs[0].Data != seq {
		t.Fatalf("写完成事件 = %+v, 提交序号 %d", evs, seq)
	}
	return evs[0]
}

// ringRead 用 ring 从 off 读 len(buf) 字节到 buf 并等待其完成，返回完成事件。
func ringRead(t *testing.T, r Ring, buf []byte, off int64) Event {
	t.Helper()
	seq, err := r.SubmitRead(buf, off)
	if err != nil {
		t.Fatalf("SubmitRead(off=%d, len=%d): %v", off, len(buf), err)
	}
	evs, err := r.Wait(1, 1, &ioTimeout)
	if err != nil {
		t.Fatalf("Wait(读 off=%d, len=%d): %v", off, len(buf), err)
	}
	if len(evs) != 1 || evs[0].Data != seq {
		t.Fatalf("读完成事件 = %+v, 提交序号 %d", evs, seq)
	}
	return evs[0]
}

// fileWriteAt 在「独立校验通道」的 fd 上同步写（绕过 ring）。用 unix.Pwrite 而不是
// os.File.WriteAt：语义与内核原语一一对应，不受 *os.File 的包装行为影响。
func fileWriteAt(t *testing.T, f *os.File, data []byte, off int64) {
	t.Helper()
	fd := int(f.Fd())
	for done := 0; done < len(data); {
		n, err := unix.Pwrite(fd, data[done:], off+int64(done))
		if err != nil {
			t.Fatalf("校验通道 pwrite(off=%d): %v", off+int64(done), err)
		}
		if n == 0 {
			t.Fatalf("校验通道 pwrite 在 off=%d 处只写进 0 字节（已写 %d/%d）",
				off+int64(done), done, len(data))
		}
		done += n
	}
}

// fileReadAt 在「独立校验通道」的 fd 上同步读满 buf，读不满即失败（用于校验，
// 短读或提前到 EOF 都说明数据没落全）。
func fileReadAt(t *testing.T, f *os.File, buf []byte, off int64) {
	t.Helper()
	fd := int(f.Fd())
	for done := 0; done < len(buf); {
		n, err := unix.Pread(fd, buf[done:], off+int64(done))
		if err != nil {
			t.Fatalf("校验通道 pread(off=%d): %v", off+int64(done), err)
		}
		if n == 0 {
			t.Fatalf("校验通道 pread 在 off=%d 处提前到 EOF（已读 %d/%d）",
				off+int64(done), done, len(buf))
		}
		done += n
	}
}

// closeRing 关闭队列并校验 errno。
func closeRing(t *testing.T, r Ring) {
	t.Helper()
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestNewWithOptionsIOPollPrecondition 校验 IOPOLL 前置条件（块设备队列轮询）由
// NewWithOptions 自行判定，且「是否校验」只取决于最终会建哪个后端 —— 每条用例都
// 用同一个不存在的设备路径，区别只在 Mode 与 devPath 参数，故错误只可能来自这里的校验。
func TestNewWithOptionsIOPollPrecondition(t *testing.T) {
	const noDev = "/dev/taihu-no-such-block-device"

	// 强制 io_uring + IOPoll：校验先于建环，必报错（Linux 读 sysfs 失败；非 Linux 恒不支持）。
	if r, err := NewWithOptions(Options{Mode: ModeIOUring, MaxEvents: 4, IOPoll: true}, noDev); err == nil {
		_ = r.Close()
		t.Error("ModeIOUring + IOPoll 应校验并拒绝该设备")
	}

	// 强制 libaio + IOPoll：IOPoll 对 libaio 无意义，不得校验，应正常建出队列。
	r, err := NewWithOptions(Options{Mode: ModeLibAIO, MaxEvents: 4, IOPoll: true}, noDev)
	if err != nil {
		t.Fatalf("ModeLibAIO 不应被 IOPOLL 前置校验拦下: %v", err)
	}
	_ = r.Close()

	// auto（env 拨到 off → libaio）+ IOPoll：同上下，env 覆盖须先于校验生效。
	t.Setenv(envMode, "off")
	r2, err := NewWithOptions(Options{Mode: ModeAuto, MaxEvents: 4, IOPoll: true}, noDev)
	if err != nil {
		t.Fatalf("env 覆盖到 libaio 后不应被 IOPOLL 前置校验拦下: %v", err)
	}
	_ = r2.Close()

	// devPath 为空：跳过校验（校验需要设备上下文），不得因缺路径误报。
	t.Setenv(envMode, "off")
	r3, err := NewWithOptions(Options{Mode: ModeAuto, MaxEvents: 4, IOPoll: true}, "")
	if err != nil {
		t.Fatalf("devPath 为空应跳过校验: %v", err)
	}
	_ = r3.Close()
}

// TestParseMode 覆盖命令行取值解析：大小写/空白归一化、三个合法取值与非法取值。
func TestParseMode(t *testing.T) {
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeAuto, false},
		{"auto", ModeAuto, false},
		{"AUTO", ModeAuto, false},
		{"  Auto\t", ModeAuto, false},
		{"on", ModeIOUring, false},
		{"ON", ModeIOUring, false},
		{" off ", ModeLibAIO, false},
		{"Off", ModeLibAIO, false},
		{"bogus", ModeAuto, true},
		{"1", ModeAuto, true},
	}
	for _, c := range cases {
		got, err := ParseMode(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseMode(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMode(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMode(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestNewWithOptionsInvalidMaxEvents MaxEvents 越界时三个后端取值都必须报错，不得静默建出队列。
func TestNewWithOptionsInvalidMaxEvents(t *testing.T) {
	for _, m := range []Mode{ModeLibAIO, ModeIOUring, ModeAuto} {
		for _, n := range []int{0, -1, 1<<16 + 1} {
			r, err := NewWithOptions(Options{Mode: m, MaxEvents: n}, "")
			if err == nil {
				_ = r.Close()
				t.Errorf("NewWithOptions({Mode: %d, MaxEvents: %d}) 应报错", int(m), n)
			}
		}
	}
}
