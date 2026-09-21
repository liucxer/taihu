package metastore

import "testing"

// TestCodecRoundTripAndDecodeErrors：编解码往返与长度非法时的错误分支。
func TestCodecRoundTripAndDecodeErrors(t *testing.T) {
	om := ObjectMeta{SegmentID: 3, Offset: 4096, Size: 100}
	got, err := decodeObjectMeta(om.encode())
	if err != nil || got != om {
		t.Fatalf("ObjectMeta roundtrip = (%+v,%v), want %+v", got, err, om)
	}
	if _, err := decodeObjectMeta([]byte{metaVersion, 1}); err == nil {
		t.Fatal("decodeObjectMeta with short value should fail")
	}

	wc := WriteCursor{SegmentID: 2, Offset: 8192}
	gotC, err := decodeWriteCursor(wc.encode())
	if err != nil || gotC != wc {
		t.Fatalf("WriteCursor roundtrip = (%+v,%v), want %+v", gotC, err, wc)
	}
	if _, err := decodeWriteCursor(nil); err == nil {
		t.Fatal("decodeWriteCursor with empty value should fail")
	}

	sm := SegmentMeta{State: SegmentStateCompacting, AliveCount: 7, ReclaimSeq: 2}
	gotS, err := decodeSegmentMeta(sm.encode())
	if err != nil || gotS != sm {
		t.Fatalf("SegmentMeta roundtrip = (%+v,%v), want %+v", gotS, err, sm)
	}
	if _, err := decodeSegmentMeta([]byte{metaVersion}); err == nil {
		t.Fatal("decodeSegmentMeta with short value should fail")
	}

	// segmentKey：前缀 + 小端 8 字节段号。
	key := segmentKey(1)
	if len(key) != len(kvSegmentPrefix)+8 {
		t.Fatalf("segmentKey len = %d", len(key))
	}
	if string(key[:len(kvSegmentPrefix)]) != kvSegmentPrefix {
		t.Fatalf("segmentKey prefix = %q", key)
	}
}

// TestAppendUpper：返回前缀独占上界且不修改入参。
func TestAppendUpper(t *testing.T) {
	prefix := []byte("m\x00")
	upper := appendUpper(prefix)
	if string(upper) != "m\x01" {
		t.Fatalf("appendUpper = %q, want m\\x01", upper)
	}
	if string(prefix) != "m\x00" {
		t.Fatalf("appendUpper mutated input: %q", prefix)
	}
}
