package protocol

import (
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/liucxer/taihu/internal/storage"
)

// assertMappedErr 断言 parse 失败时返回的错误与 MapCode 的映射结果等价。
//
// 不能直接用 == 比较：MapCode 对「有对应 sentinel」的错误码返回库里的哨兵值
// （同一实例，== 成立），但对 CodeInternal / CodeInvalidArgument 走 default 分支
// 返回 errors.New(...) —— 每次调用都是**新实例**，== 恒为假。故分两种断言：
// 哨兵码要求 errors.Is 命中（更强的契约），其余码要求消息一致。
func assertMappedErr(t *testing.T, what string, got error, code ErrCode) {
	t.Helper()
	want := MapCode(code)
	if got == nil {
		t.Fatalf("%s(code=%d) 应返回错误，got nil", what, code)
	}
	switch code {
	case CodeNotFound, CodeInvalidRange, CodeTooLarge, CodeNoSpace:
		if !errors.Is(got, want) {
			t.Fatalf("%s(code=%d) 应返回哨兵错误 %v，got %v", what, code, want, got)
		}
	default:
		if got.Error() != want.Error() {
			t.Fatalf("%s(code=%d) 消息应为 %q，got %q", what, code, want.Error(), got.Error())
		}
	}
}

// --- 编解码往返 ---

func TestPutHeaderRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		key  string
		size int64
	}{
		{"空 key 零长度", "", 0},
		{"单字节 key", "k", 1},
		{"常规", "some/object/key", 4 << 20},
		{"size 取 MaxInt64", "k", math.MaxInt64},
		{"size 为 -1（读至结尾的哨兵值经 uint64 往返）", "k", -1},
		{"key 恰为 MaxKeyLen", strings.Repeat("x", MaxKeyLen), 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, size, err := ParsePutHeader(NewSliceReader(EncodePutHeader(tc.key, tc.size)))
			if err != nil {
				t.Fatalf("ParsePutHeader: %v", err)
			}
			if key != tc.key || size != tc.size {
				t.Fatalf("往返不一致: got (%q, %d), want (%q, %d)", key, size, tc.key, tc.size)
			}
		})
	}
}

func TestGetReqRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		key       string
		off, size int64
	}{
		{"全零", "", 0, 0},
		{"常规", "key-1", 4096, 1 << 20},
		{"off 与 size 取 MaxInt64", "k", math.MaxInt64, math.MaxInt64},
		{"size 为 -1", "k", 0, -1},
		{"key 恰为 MaxKeyLen", strings.Repeat("y", MaxKeyLen), 8, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, off, size, err := ParseGetReq(NewSliceReader(EncodeGetReq(tc.key, tc.off, tc.size)))
			if err != nil {
				t.Fatalf("ParseGetReq: %v", err)
			}
			if key != tc.key || off != tc.off || size != tc.size {
				t.Fatalf("往返不一致: got (%q, %d, %d), want (%q, %d, %d)",
					key, off, size, tc.key, tc.off, tc.size)
			}
		})
	}
}

func TestKeyReqRoundTrip(t *testing.T) {
	for _, key := range []string{"", "k", "a/b/c", strings.Repeat("z", MaxKeyLen)} {
		got, err := ParseKeyReq(NewSliceReader(EncodeKeyReq(key)))
		if err != nil {
			t.Fatalf("ParseKeyReq(%d 字节 key): %v", len(key), err)
		}
		if got != key {
			t.Fatalf("往返不一致: got %q want %q", got, key)
		}
	}
}

func TestPongRoundTrip(t *testing.T) {
	for _, ts := range []int64{0, 1, 1 << 40, math.MaxInt64} {
		got, err := ParsePong(NewSliceReader(EncodePong(CodeOK, ts)))
		if err != nil {
			t.Fatalf("ParsePong(CodeOK, %d): %v", ts, err)
		}
		if got != ts {
			t.Fatalf("往返不一致: got %d want %d", got, ts)
		}
	}
	// 非 0 错误码须还原成对应库错误，而不是被当成时间戳解析（code 占前 4 字节，
	// 若漏判会把错误码当时间戳返回，静默给出错误结果）。
	for _, c := range []ErrCode{CodeNotFound, CodeInvalidRange, CodeTooLarge, CodeNoSpace, CodeInternal, CodeInvalidArgument} {
		_, err := ParsePong(NewSliceReader(EncodePong(c, 123)))
		assertMappedErr(t, "ParsePong", err, c)
	}
}

func TestMetaRespRoundTrip(t *testing.T) {
	cases := []struct{ segID, off, size int64 }{
		{0, 0, 0},
		{7, 4096, 1 << 20},
		{math.MaxInt64, math.MaxInt64, math.MaxInt64},
	}
	for _, tc := range cases {
		segID, off, size, err := ParseMetaResp(NewSliceReader(EncodeMetaResp(tc.segID, tc.off, tc.size)))
		if err != nil {
			t.Fatalf("ParseMetaResp: %v", err)
		}
		if segID != tc.segID || off != tc.off || size != tc.size {
			t.Fatalf("往返不一致: got (%d,%d,%d) want (%d,%d,%d)",
				segID, off, size, tc.segID, tc.off, tc.size)
		}
	}
}

func TestSegSumRoundTrip(t *testing.T) {
	full := SegmentSummary{
		Total: 1, Free: 2, Active: 3, Full: 4, Reclaiming: 5,
		CursorSeg: 6, CursorOff: 7, SegSize: 8, ObjectCount: 9,
	}
	maxed := SegmentSummary{
		Total: math.MaxInt64, Free: math.MaxInt64, Active: math.MaxInt64, Full: math.MaxInt64,
		Reclaiming: math.MaxInt64, CursorSeg: math.MaxInt64, CursorOff: math.MaxInt64,
		SegSize: math.MaxInt64, ObjectCount: math.MaxInt64,
	}
	for _, want := range []SegmentSummary{{}, full, maxed} {
		got, err := ParseSegSum(NewSliceReader(EncodeSegSum(CodeOK, want)))
		if err != nil {
			t.Fatalf("ParseSegSum(%+v): %v", want, err)
		}
		if got != want {
			t.Fatalf("往返不一致:\n got %+v\nwant %+v", got, want)
		}
	}
	// 非 0 错误码：不解析汇总体，直接映射为库错误。
	for _, c := range []ErrCode{CodeNotFound, CodeInternal, CodeInvalidArgument} {
		_, err := ParseSegSum(NewSliceReader(EncodeSegSum(c, full)))
		assertMappedErr(t, "ParseSegSum", err, c)
	}
}

func TestSegDataRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		entries []SegmentEntry
	}{
		{"空", nil},
		{"单条", []SegmentEntry{{SegmentID: 1, State: 2, AliveCount: 3, ReclaimSeq: 4}}},
		{"多条（含 0 值与极值）", []SegmentEntry{
			{SegmentID: 0, State: 0, AliveCount: 0, ReclaimSeq: 0},
			{SegmentID: math.MaxInt64, State: 255, AliveCount: math.MaxInt64, ReclaimSeq: math.MaxInt64},
			{SegmentID: 42, State: 1, AliveCount: 7, ReclaimSeq: 9},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSegData(NewSliceReader(EncodeSegData(tc.entries)), nil)
			if err != nil {
				t.Fatalf("ParseSegData: %v", err)
			}
			if len(got) != len(tc.entries) {
				t.Fatalf("条数不一致: got %d want %d", len(got), len(tc.entries))
			}
			for i := range got {
				if got[i] != tc.entries[i] {
					t.Fatalf("第 %d 条不一致: got %+v want %+v", i, got[i], tc.entries[i])
				}
			}
		})
	}
}

func TestKeysDataRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		keys []string
	}{
		{"空", nil},
		{"单条", []string{"a"}},
		{"含空串 key", []string{"", "a", ""}},
		{"多条", []string{"k1", "k2", "prefix/3"}},
		{"恰为 MaxKeyLen 的 key", []string{strings.Repeat("w", MaxKeyLen)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseKeysData(NewSliceReader(EncodeKeysData(tc.keys)), nil)
			if err != nil {
				t.Fatalf("ParseKeysData: %v", err)
			}
			if len(got) != len(tc.keys) {
				t.Fatalf("条数不一致: got %d want %d", len(got), len(tc.keys))
			}
			for i := range got {
				if got[i] != tc.keys[i] {
					t.Fatalf("第 %d 条不一致: got %q want %q", i, got[i], tc.keys[i])
				}
			}
		})
	}
}

// --- 追加语义：ParseSegData / ParseKeysData 在给定 out 之后追加 ---

func TestParseAppendsToOut(t *testing.T) {
	pre := []SegmentEntry{{SegmentID: 100, State: 1, AliveCount: 1, ReclaimSeq: 1}}
	got, err := ParseSegData(NewSliceReader(EncodeSegData([]SegmentEntry{{SegmentID: 200}})), pre)
	if err != nil {
		t.Fatalf("ParseSegData: %v", err)
	}
	if len(got) != 2 || got[0].SegmentID != 100 || got[1].SegmentID != 200 {
		t.Fatalf("应在 out 之后追加，got %+v", got)
	}

	gotKeys, err := ParseKeysData(NewSliceReader(EncodeKeysData([]string{"b"})), []string{"a"})
	if err != nil {
		t.Fatalf("ParseKeysData: %v", err)
	}
	if len(gotKeys) != 2 || gotKeys[0] != "a" || gotKeys[1] != "b" {
		t.Fatalf("应在 out 之后追加，got %v", gotKeys)
	}
}

// --- 畸形输入：截断到任意前缀都不得 panic ---

// parsers 覆盖全部 Parse* 入口。每个输入是从一段合法编码逐步截断出来的前缀。
func parsers() []struct {
	name   string
	valid  []byte
	expect func(t *testing.T, err error)
} {
	// 截断到某前缀时，唯一允许的结果是「返回错误」——因为下面每个 valid 编码
	// 的每个真前缀都不构成一段自洽的更短帧（计数/长度字段都已写成非 0 或尚有定长字段缺失）。
	allErr := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("截断输入应返回错误，却解析成功了")
		}
	}
	return []struct {
		name   string
		valid  []byte
		expect func(t *testing.T, err error)
	}{
		{"PutHeader", EncodePutHeader("abc", 9), allErr},
		{"GetReq", EncodeGetReq("abc", 1, 2), allErr},
		{"KeyReq", EncodeKeyReq("abc"), allErr},
		{"Pong", EncodePong(CodeOK, 12345), allErr},
		{"MetaResp", EncodeMetaResp(1, 2, 3), allErr},
		{"SegSum", EncodeSegSum(CodeOK, SegmentSummary{Total: 1}), allErr},
		{"SegData", EncodeSegData([]SegmentEntry{{SegmentID: 1}, {SegmentID: 2}}), allErr},
		{"KeysData", EncodeKeysData([]string{"aa", "bb"}), allErr},
	}
}

func TestParseTruncatedInputNeverPanics(t *testing.T) {
	for _, p := range parsers() {
		for n := 0; n < len(p.valid); n++ {
			prefix := p.valid[:n]
			t.Run(p.name+"/prefix"+strconv.Itoa(n), func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("截断到 %d 字节时 panic: %v", n, r)
					}
				}()
				p.expect(t, parseBy(p.name, prefix))
			})
		}
	}
}

// TestParseShortByOneByte 单独把「短 1 字节」点出来：这是最容易漏的边界。
func TestParseShortByOneByte(t *testing.T) {
	for _, p := range parsers() {
		short := p.valid[:len(p.valid)-1]
		t.Run(p.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("短 1 字节时 panic: %v", r)
				}
			}()
			p.expect(t, parseBy(p.name, short))
		})
	}
}

// TestParseFullInputSucceeds 反证上面用到的 valid 编码本身是自洽的：
// 完整输入必须解析成功，否则「截断报错」这个断言就没有意义。
func TestParseFullInputSucceeds(t *testing.T) {
	for _, p := range parsers() {
		t.Run(p.name, func(t *testing.T) {
			if err := parseBy(p.name, p.valid); err != nil {
				t.Fatalf("完整合法编码应解析成功，却报 %v", err)
			}
		})
	}
}

func parseBy(name string, b []byte) error {
	r := NewSliceReader(b)
	switch name {
	case "PutHeader":
		_, _, err := ParsePutHeader(r)
		return err
	case "GetReq":
		_, _, _, err := ParseGetReq(r)
		return err
	case "KeyReq":
		_, err := ParseKeyReq(r)
		return err
	case "Pong":
		_, err := ParsePong(r)
		return err
	case "MetaResp":
		_, _, _, err := ParseMetaResp(r)
		return err
	case "SegSum":
		_, err := ParseSegSum(r)
		return err
	case "SegData":
		_, err := ParseSegData(r, nil)
		return err
	case "KeysData":
		_, err := ParseKeysData(r, nil)
		return err
	}
	panic("unknown parser " + name)
}

// --- 超长 key 必须被拒（防畸形长度字段放大内存） ---

func TestParseRejectsOverlongKey(t *testing.T) {
	long := strings.Repeat("x", MaxKeyLen+1)

	if _, _, err := ParsePutHeader(NewSliceReader(EncodePutHeader(long, 0))); err == nil {
		t.Fatal("ParsePutHeader 应拒绝超过 MaxKeyLen 的 key")
	}
	if _, _, _, err := ParseGetReq(NewSliceReader(EncodeGetReq(long, 0, 0))); err == nil {
		t.Fatal("ParseGetReq 应拒绝超过 MaxKeyLen 的 key")
	}
	if _, err := ParseKeyReq(NewSliceReader(EncodeKeyReq(long))); err == nil {
		t.Fatal("ParseKeyReq 应拒绝超过 MaxKeyLen 的 key")
	}
	if _, err := ParseKeysData(NewSliceReader(EncodeKeysData([]string{long})), nil); err == nil {
		t.Fatal("ParseKeysData 应拒绝超过 MaxKeyLen 的 key")
	}
}

// TestParseHugeDeclaredKeyLenDoesNotAllocate 断言超长 key 的拒绝发生在**读取之前**：
// 声明一个 4GiB 的 keyLen 但只提供极少字节，必须立刻报 key too long / EOF，
// 而不是尝试分配 4GiB（内存放大攻击面）。
func TestParseHugeDeclaredKeyLenDoesNotAllocate(t *testing.T) {
	// keyLen = 0xFFFFFFFF，后面什么都没有。
	payload := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := ParseKeyReq(NewSliceReader(payload)); err == nil {
		t.Fatal("声明 4GiB keyLen 应被拒")
	}
	if _, _, err := ParsePutHeader(NewSliceReader(payload)); err == nil {
		t.Fatal("声明 4GiB keyLen 应被拒")
	}
}

// --- 错误码映射往返 ---

func TestMapStorageErrAndMapCodeRoundTrip(t *testing.T) {
	mapped := []error{
		taihu.ErrNotFound,
		taihu.ErrInvalidRange,
		taihu.ErrTooLarge,
		taihu.ErrNoSpace,
	}
	for _, err := range mapped {
		code := MapStorageErr(err)
		if code == CodeOK {
			t.Fatalf("%v 不应映射为 CodeOK", err)
		}
		got := MapCode(code)
		if !errors.Is(got, err) {
			t.Fatalf("往返不一致: %v -> code %d -> %v", err, code, got)
		}
		// 包一层的错误也要能识别（errors.Is 语义）。
		if wrapped := MapStorageErr(errors.Join(errors.New("ctx"), err)); wrapped != code {
			t.Fatalf("包装后的 %v 应仍映射为 %d，got %d", err, code, wrapped)
		}
	}
}

func TestMapStorageErrDefaultIsInternal(t *testing.T) {
	if got := MapStorageErr(errors.New("boom")); got != CodeInternal {
		t.Fatalf("未映射错误应返回 CodeInternal，got %d", got)
	}
}

func TestMapCode(t *testing.T) {
	if err := MapCode(CodeOK); err != nil {
		t.Fatalf("CodeOK 应映射为 nil，got %v", err)
	}
	if err := MapCode(CodeInvalidArgument); err == nil || !strings.Contains(err.Error(), "invalid argument") {
		t.Fatalf("CodeInvalidArgument 应有明确错误，got %v", err)
	}
	// 未知码走 default 分支，不得 panic，也不得返回 nil。
	if err := MapCode(ErrCode(9999)); err == nil {
		t.Fatal("未知错误码应返回非 nil 错误")
	}
}

func TestEncCode(t *testing.T) {
	for _, c := range []ErrCode{CodeOK, CodeNotFound, CodeInvalidRange, CodeTooLarge, CodeNoSpace, CodeInternal, CodeInvalidArgument} {
		p := EncCode(c)
		if len(p) != 4 {
			t.Fatalf("EncCode 应为 4 字节，got %d", len(p))
		}
		got, err := ReadU32(NewSliceReader(p))
		if err != nil {
			t.Fatalf("ReadU32(EncCode(%d)): %v", c, err)
		}
		if ErrCode(got) != c {
			t.Fatalf("往返不一致: got %d want %d", got, c)
		}
	}
}

// --- ByteReader / SliceReader ---

func TestSliceReaderNext(t *testing.T) {
	buf := []byte{1, 2, 3, 4, 5}
	r := NewSliceReader(buf)

	if r.Len() != 5 {
		t.Fatalf("初始 Len 应为 5，got %d", r.Len())
	}
	// Next 返回零拷贝引用：改动返回值应反映到底层。
	got, err := r.Next(2)
	if err != nil {
		t.Fatalf("Next(2): %v", err)
	}
	if &got[0] != &buf[0] {
		t.Fatal("Next 应返回底层切片的引用（零拷贝）")
	}
	if r.Len() != 3 {
		t.Fatalf("消费 2 字节后 Len 应为 3，got %d", r.Len())
	}
	// 恰好读完剩余。
	if _, err := r.Next(3); err != nil {
		t.Fatalf("Next(3) 读尽剩余应成功: %v", err)
	}
	if r.Len() != 0 {
		t.Fatalf("读尽后 Len 应为 0，got %d", r.Len())
	}
	// 越界读。
	if _, err := r.Next(1); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("越界读应返回 io.ErrUnexpectedEOF，got %v", err)
	}
	// 边界：Next(0) 应成功（剩余为 0）。
	if b, err := r.Next(0); err != nil || len(b) != 0 {
		t.Fatalf("Next(0) 应成功且返回空切片，got (%v, %v)", b, err)
	}
}

func TestSliceReaderReadString(t *testing.T) {
	r := NewSliceReader([]byte("hello world"))
	s, err := r.ReadString(5)
	if err != nil {
		t.Fatalf("ReadString(5): %v", err)
	}
	if s != "hello" {
		t.Fatalf("got %q want %q", s, "hello")
	}
	if r.Len() != 6 {
		t.Fatalf("Len 应为 6，got %d", r.Len())
	}
	if _, err := r.ReadString(7); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("越界 ReadString 应返回 io.ErrUnexpectedEOF，got %v", err)
	}
}

func TestReadU32U64ShortInput(t *testing.T) {
	for n := 0; n < 4; n++ {
		if _, err := ReadU32(NewSliceReader(make([]byte, n))); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("ReadU32 于 %d 字节应返回 io.ErrUnexpectedEOF，got %v", n, err)
		}
	}
	for n := 0; n < 8; n++ {
		if _, err := ReadU64(NewSliceReader(make([]byte, n))); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("ReadU64 于 %d 字节应返回 io.ErrUnexpectedEOF，got %v", n, err)
		}
	}
}

// TestWireConstantsAreConsistent 钉住几个互相派生的常量关系：
// 改动其一却忘了另一处，是这类协议常量最典型的回归。
func TestWireConstantsAreConsistent(t *testing.T) {
	if MaxFrameTotal != FrameHeaderLen+ChunkSize {
		t.Fatalf("MaxFrameTotal(%d) != FrameHeaderLen(%d)+ChunkSize(%d)",
			MaxFrameTotal, FrameHeaderLen, ChunkSize)
	}
	if InputNodeSize != 4+MaxFrameTotal {
		t.Fatalf("InputNodeSize(%d) != 4+MaxFrameTotal(%d)", InputNodeSize, MaxFrameTotal)
	}
	if ShmSliceSize%4096 != 0 {
		t.Fatalf("ShmSliceSize(%d) 未 4K 对齐（shm 直读切片要求）", ShmSliceSize)
	}
	if ShmSliceSize < ChunkSize+8*1024 {
		t.Fatalf("ShmSliceSize(%d) 不足以容纳 [pad+帧头+ChunkSize+对齐余量]", ShmSliceSize)
	}
	if SegItemLen != 8+1+8+8 {
		t.Fatalf("SegItemLen(%d) 与 wire 布局不符", SegItemLen)
	}
	// OpGetDataFinal 由 OpGetData 置高位派生，须与 OpGetData 同为数据帧语义。
	if OpGetDataFinal != OpGetData|0x80 {
		t.Fatalf("OpGetDataFinal(%#x) != OpGetData|0x80", OpGetDataFinal)
	}
	// 操作码不得重复（除派生的 final 位）。
	seen := map[OpCode]string{}
	for name, op := range map[string]OpCode{
		"OpPutHeader": OpPutHeader, "OpPutData": OpPutData, "OpPutEnd": OpPutEnd,
		"OpResp": OpResp, "OpGetReq": OpGetReq, "OpGetData": OpGetData,
		"OpGetEnd": OpGetEnd, "OpGetErr": OpGetErr, "OpDelReq": OpDelReq,
		"OpStatReq": OpStatReq, "OpStatResp": OpStatResp, "OpGetDataFinal": OpGetDataFinal,
		"OpPing": OpPing, "OpPong": OpPong, "OpMetaReq": OpMetaReq, "OpMetaResp": OpMetaResp,
		"OpSegReq": OpSegReq, "OpSegSum": OpSegSum, "OpSegData": OpSegData,
		"OpSegEnd": OpSegEnd, "OpKeysReq": OpKeysReq, "OpKeysData": OpKeysData,
	} {
		if prev, dup := seen[op]; dup {
			t.Fatalf("操作码重复: %s 与 %s 同为 %#x", name, prev, op)
		}
		seen[op] = name
	}
}
