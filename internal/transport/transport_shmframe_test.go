//go:build linux

// shm 帧原语（shm_frame_linux.go）单元测试：shmReadFrame 是纯函数（仅依赖
// shmipc.BufferReader 接口），故用一个内存假 reader 覆盖正常帧、数据帧跳 pad、
// 以及各类截断/越界/畸形长度输入，无需真实共享内存。
package transport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/liucxer/taihu/pkg/ierr"
	"github.com/liucxer/taihu/third_party/shmipc-go"

	"github.com/liucxer/taihu/internal/transport/protocol"
)

// shmFakeReader 是 shmipc.BufferReader 的最小内存实现：按字节切片顺序消费，
// ReleasePreviousRead/Peek/Discard 只实现语义上必要的行为。
type shmFakeReader struct {
	data []byte
	pos  int
}

var _ shmipc.BufferReader = (*shmFakeReader)(nil)

func (r *shmFakeReader) Len() int { return len(r.data) - r.pos }

func (r *shmFakeReader) ReadByte() (byte, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	b := r.data[r.pos]
	r.pos++
	return b, nil
}

func (r *shmFakeReader) ReadBytes(size int) ([]byte, error) {
	if size < 0 || r.pos+size > len(r.data) {
		return nil, io.ErrUnexpectedEOF
	}
	b := r.data[r.pos : r.pos+size]
	r.pos += size
	return b, nil
}

func (r *shmFakeReader) Peek(size int) ([]byte, error) {
	if size < 0 || r.pos+size > len(r.data) {
		return nil, io.ErrUnexpectedEOF
	}
	return r.data[r.pos : r.pos+size], nil
}

func (r *shmFakeReader) Discard(size int) (int, error) {
	if size < 0 || r.pos+size > len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	r.pos += size
	return size, nil
}

func (r *shmFakeReader) ReleasePreviousRead() {}

func (r *shmFakeReader) ReadString(size int) (string, error) {
	b, err := r.ReadBytes(size)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// shmBuildFrame 按 shm 线路格式拼一帧：[4B len][1B op][数据帧 pad][payload]。
// len 字段 = op(1) + payload 长度；数据帧在 op 后带 ShmDataPad-5 的 4K 对齐 pad。
func shmBuildFrame(op protocol.OpCode, payload []byte) []byte {
	n := shmOpLen + len(payload)
	buf := make([]byte, 0, shmLenPrefixLen+n+protocol.ShmDataPad)
	var lp [shmLenPrefixLen]byte
	binary.BigEndian.PutUint32(lp[:], uint32(n))
	buf = append(buf, lp[:]...)
	buf = append(buf, byte(op))
	if op == protocol.OpGetData || op == protocol.OpGetDataFinal || op == protocol.OpPutData {
		buf = append(buf, make([]byte, protocol.ShmDataPad-shmLenPrefixLen-shmOpLen)...)
	}
	buf = append(buf, payload...)
	return buf
}

// TestShmReadFrameControl 覆盖控制帧读取：无 pad，负载零拷贝返回，读位恰好到帧尾。
func TestShmReadFrameControl(t *testing.T) {
	payload := protocol.EncodeKeyReq("some-key")
	r := &shmFakeReader{data: shmBuildFrame(protocol.OpStatReq, payload)}

	op, got, err := shmReadFrame(r)
	if err != nil {
		t.Fatalf("shmReadFrame: %v", err)
	}
	if op != protocol.OpStatReq {
		t.Fatalf("op = 0x%x, want 0x%x", byte(op), byte(protocol.OpStatReq))
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %x, want %x", got, payload)
	}
	if r.Len() != 0 {
		t.Fatalf("帧未读尽：剩 %d 字节", r.Len())
	}
}

// TestShmReadFrameDataSkipsPad 覆盖带 pad 的数据帧：跳过后取到 payload，len 字段含 pad。
func TestShmReadFrameDataSkipsPad(t *testing.T) {
	payload := []byte("0123456789")
	for _, op := range []protocol.OpCode{protocol.OpGetData, protocol.OpGetDataFinal, protocol.OpPutData} {
		r := &shmFakeReader{data: shmBuildFrame(op, payload)}
		gotOp, got, err := shmReadFrame(r)
		if err != nil {
			t.Fatalf("op 0x%x: shmReadFrame: %v", byte(op), err)
		}
		if gotOp != op {
			t.Fatalf("op = 0x%x, want 0x%x", byte(gotOp), byte(op))
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("op 0x%x: payload = %q, want %q", byte(op), got, payload)
		}
		if r.Len() != 0 {
			t.Fatalf("op 0x%x: 帧未读尽：剩 %d 字节", byte(op), r.Len())
		}
	}
}

// TestShmReadFrameBadInput 覆盖 shmReadFrame 的全部失败分支：
// 长度前缀不足、n<1、n 超上限、op 缺失、pad 缺失、负载缺失。
func TestShmReadFrameBadInput(t *testing.T) {
	overMax := make([]byte, shmLenPrefixLen)
	binary.BigEndian.PutUint32(overMax, uint32(shmMaxFrameSize+1))

	cases := []struct {
		name       string
		data       []byte
		wantBadFrm bool
	}{
		{"empty", nil, false},
		{"short-len-prefix", []byte{0, 0}, false},
		{"zero-len", []byte{0, 0, 0, 0}, true},
		{"len-over-max", overMax, true},
		{"missing-op", []byte{0, 0, 0, 5}, false},
		{"missing-pad", []byte{0, 0, 0, 5, byte(protocol.OpGetData)}, false},
		{"missing-payload", []byte{0, 0, 0, 5, byte(protocol.OpStatReq)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &shmFakeReader{data: tc.data}
			op, payload, err := shmReadFrame(r)
			if err == nil {
				t.Fatalf("shmReadFrame 应报错，得到 op=0x%x payload=%x", byte(op), payload)
			}
			if tc.wantBadFrm && !errors.Is(err, ierr.ErrShmBadFrame) {
				t.Fatalf("err = %v, want ierr.ErrShmBadFrame", err)
			}
			if !tc.wantBadFrm && errors.Is(err, ierr.ErrShmBadFrame) {
				t.Fatalf("err = %v, 不应为 ierr.ErrShmBadFrame", err)
			}
		})
	}
}

// TestShmFrameConstants 固化帧布局不变量：长度前缀 4B、op 1B、负载上限 = op + ChunkSize。
func TestShmFrameConstants(t *testing.T) {
	if shmLenPrefixLen != 4 || shmOpLen != 1 {
		t.Fatalf("帧头 = %d+%d, want 4+1", shmLenPrefixLen, shmOpLen)
	}
	if shmMaxFrameSize != shmOpLen+protocol.ChunkSize {
		t.Fatalf("shmMaxFrameSize = %d, want %d", shmMaxFrameSize, shmOpLen+protocol.ChunkSize)
	}
	if protocol.ShmDataPad <= shmLenPrefixLen+shmOpLen {
		t.Fatalf("ShmDataPad = %d 应大于帧头 %d", protocol.ShmDataPad, shmLenPrefixLen+shmOpLen)
	}
}
