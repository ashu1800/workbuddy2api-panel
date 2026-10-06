package wsx

// 本文件与 wsx_test.go 的分工：任务书在两处分别写了 ws_test.go 与 wsx_test.go，
// 为免歧义两个都建——**帧编解码**（长度三档边界、掩码、超限、分片、关闭语义）在这里，
// **握手与 Conn 写路径**在 wsx_test.go。
//
// 长度边界是这个文件的核心：125/126 与 65535/65536 是 7 位长度字段的两处"换挡点"，
// 写错任何一处都会表现成"大 payload 静默截断/错位"，而小帧测试永远发现不了。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// maskedFrame 手工构造一个带掩码的客户端帧（模拟浏览器发来的字节）。
// 故意不复用 encodeFrame：测试要能在编码器整段写错时依然给出正确期望值。
func maskedFrame(opcode byte, fin bool, payload []byte, key [4]byte, extBits int) []byte {
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	out := []byte{b0}
	n := len(payload)
	switch extBits {
	case 0:
		out = append(out, 0x80|byte(n))
	case 16:
		out = append(out, 0x80|126, byte(n>>8), byte(n))
	case 64:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		out = append(out, 0x80|127)
		out = append(out, ext[:]...)
	}
	out = append(out, key[:]...)
	for i, b := range payload {
		out = append(out, b^key[i%4])
	}
	return out
}

// connWith 造一个只读的 Conn（读来源是给定字节）。
func connWith(b []byte) *Conn { return &Conn{r: bytes.NewReader(b)} }

// 长度三档边界：编码出的头部字节形态必须与 RFC 6455 §5.2 一致，且编解码往返无损。
func TestFrameLengthBoundaries(t *testing.T) {
	key := [4]byte{0xA1, 0xB2, 0xC3, 0xD4}
	for _, n := range []int{0, 1, 125, 126, 65535, 65536, MaxFramePayload} {
		payload := bytes.Repeat([]byte("x"), n)

		// 1) 服务端方向（不掩码）：头部形态 + DecodeFrame 往返。
		frame := encodeFrame(OpText, payload, false)
		wantExt := 0
		wantLenByte := byte(n)
		switch {
		case n < 126:
		case n <= 0xFFFF:
			wantExt, wantLenByte = 2, 126
		default:
			wantExt, wantLenByte = 8, 127
		}
		if frame[0] != 0x80|OpText {
			t.Fatalf("n=%d: 首字节 = %#x, want 0x81", n, frame[0])
		}
		if frame[1] != wantLenByte {
			t.Fatalf("n=%d: 长度字节 = %d, want %d", n, frame[1], wantLenByte)
		}
		switch wantExt {
		case 2:
			if got := binary.BigEndian.Uint16(frame[2:4]); int(got) != n {
				t.Fatalf("n=%d: 16 位扩展长度 = %d", n, got)
			}
		case 8:
			if got := binary.BigEndian.Uint64(frame[2:10]); int(got) != n {
				t.Fatalf("n=%d: 64 位扩展长度 = %d", n, got)
			}
		}
		if len(frame) != 2+wantExt+n {
			t.Fatalf("n=%d: 帧长 = %d, want %d", n, len(frame), 2+wantExt+n)
		}
		op, got, err := DecodeFrame(bytes.NewReader(frame))
		if err != nil {
			t.Fatalf("n=%d: DecodeFrame: %v", n, err)
		}
		if op != OpText || !bytes.Equal(got, payload) {
			t.Fatalf("n=%d: 往返不一致（op=%#x, len=%d）", n, op, len(got))
		}

		// 2) 客户端方向（带掩码）：走服务端真正的读路径 Conn.ReadFrame（强制掩码）。
		extBits := 0
		switch wantExt {
		case 2:
			extBits = 16
		case 8:
			extBits = 64
		}
		mf := maskedFrame(OpText, true, payload, key, extBits)
		op, got, err = connWith(mf).ReadFrame()
		if err != nil {
			t.Fatalf("n=%d: ReadFrame(掩码帧): %v", n, err)
		}
		if op != OpText || !bytes.Equal(got, payload) {
			t.Fatalf("n=%d: 掩码帧往返不一致（op=%#x, len=%d）", n, op, len(got))
		}
	}
}

// 掩码解码：载荷里必须逐字节异或回原文（含跨 4 字节掩码键的循环）。
func TestReadFrameUnmasksClientPayload(t *testing.T) {
	payload := []byte("the quick brown fox jumps over 13 lazy dogs")
	key := [4]byte{0x37, 0xFA, 0x21, 0x3D}
	frame := maskedFrame(OpText, true, payload, key, 0)

	// 先自证"线上字节确实被掩码了"，否则这个测试会退化成恒真。
	if bytes.Contains(frame[6:], payload) {
		t.Fatal("构造的帧里出现了明文载荷，掩码没生效")
	}
	op, got, err := connWith(frame).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if op != OpText {
		t.Fatalf("opcode = %#x, want OpText", op)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("解出的载荷 = %q, want %q", got, payload)
	}
}

// 客户端帧必须带掩码（RFC 6455 §5.1）：不带就是协议错误，绝不能"宽容地收下"。
func TestReadFrameRejectsUnmaskedClientFrame(t *testing.T) {
	frame := encodeFrame(OpText, []byte("hi"), false)
	_, _, err := connWith(frame).ReadFrame()
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}

// 控制帧载荷 >125 → 报错（RFC 6455 §5.5）。
func TestReadFrameRejectsOversizedControlFrame(t *testing.T) {
	var key [4]byte
	big := bytes.Repeat([]byte("p"), 126)
	_, _, err := connWith(maskedFrame(OpPing, true, big, key, 16)).ReadFrame()
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("126 字节的 PING: err = %v, want ErrProtocol", err)
	}
	// 125 字节仍要能收下（边界另一侧）。
	ok := bytes.Repeat([]byte("p"), 125)
	op, got, err := connWith(maskedFrame(OpPing, true, ok, key, 0)).ReadFrame()
	if err != nil {
		t.Fatalf("125 字节的 PING 不该报错: %v", err)
	}
	if op != OpPing || !bytes.Equal(got, ok) {
		t.Fatalf("125 字节 PING 往返不一致（op=%#x, len=%d）", op, len(got))
	}
}

// 控制帧不允许分片（FIN=0）→ 协议错误。
func TestReadFrameRejectsFragmentedControlFrame(t *testing.T) {
	var key [4]byte
	_, _, err := connWith(maskedFrame(OpPing, false, []byte("x"), key, 0)).ReadFrame()
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}

// 超过 1 MiB 的帧必须在**分配内存之前**被拒绝：这里只给头部（声明长度 1MiB+1，
// 后面一个字节都没有），若实现先 make 再读，就会卡在 ReadFull 上而不是立刻报错。
func TestReadFrameRejectsOversizeFrameBeforeAllocating(t *testing.T) {
	var key [4]byte
	over := MaxFramePayload + 1
	frame := []byte{0x81, 0x80 | 127}
	var ext [8]byte
	binary.BigEndian.PutUint64(ext[:], uint64(over))
	frame = append(frame, ext[:]...)
	frame = append(frame, key[:]...)
	// 故意不追加任何载荷字节。
	_, _, err := connWith(frame).ReadFrame()
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	// 正好 1 MiB 是允许的（边界另一侧）。
	exact := bytes.Repeat([]byte("y"), MaxFramePayload)
	if _, got, err := connWith(encodeFrame(OpBinary, exact, true)).ReadFrame(); err != nil {
		t.Fatalf("1 MiB 整不该报错: %v", err)
	} else if len(got) != MaxFramePayload {
		t.Fatalf("1 MiB 整解出 %d 字节", len(got))
	}
}

// RSV 位与未知操作码都是协议错误（本实现不协商扩展，置位即违规）。
func TestReadFrameRejectsRSVBitsAndUnknownOpcode(t *testing.T) {
	var key [4]byte
	// 0xC1 = FIN + RSV1 + OpText
	rsv := append([]byte{0xC1, 0x80 | 1}, key[:]...)
	rsv = append(rsv, 'x'^key[0])
	if _, _, err := connWith(rsv).ReadFrame(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("RSV1: err = %v, want ErrProtocol", err)
	}
	// 0x83 = FIN + 保留操作码 0x3
	bad := append([]byte{0x83, 0x80 | 1}, key[:]...)
	bad = append(bad, 'x'^key[0])
	if _, _, err := connWith(bad).ReadFrame(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("未知操作码: err = %v, want ErrProtocol", err)
	}
}

// 对端正常关闭 vs 错误：CLOSE 帧是"正常关闭"（返回帧本身，不是 error），
// 之后对端关掉 TCP 才是 io.EOF。
func TestReadFrameDistinguishesPeerCloseFromError(t *testing.T) {
	var key [4]byte
	closePayload := []byte{0x03, 0xE8} // 1000
	frame := maskedFrame(OpClose, true, closePayload, key, 0)
	r := bytes.NewReader(frame)
	c := &Conn{r: r}

	op, payload, err := c.ReadFrame()
	if err != nil {
		t.Fatalf("收到 CLOSE 不该是错误: %v", err)
	}
	if op != OpClose || !bytes.Equal(payload, closePayload) {
		t.Fatalf("CLOSE 帧解析错误: op=%#x payload=%v", op, payload)
	}
	// 流已尽 = 对端直接关了 TCP。
	if _, _, err := c.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("流结束时 err = %v, want io.EOF", err)
	}
}

// 分片消息按文档约定"忽略并跳过"：跳过整条消息后，下一帧正常交付。
func TestFragmentedMessageIsSkipped(t *testing.T) {
	var key [4]byte
	var buf bytes.Buffer
	buf.Write(maskedFrame(OpText, false, []byte("hel"), key, 0))            // 首片（FIN=0）
	buf.Write(maskedFrame(OpContinuation, false, []byte("lo "), key, 0))    // 中间片
	buf.Write(maskedFrame(OpContinuation, true, []byte("world"), key, 0))   // 收尾片
	buf.Write(maskedFrame(OpText, true, []byte(`{"ok":true}`), key, 0))     // 之后的正常帧

	op, payload, err := connWith(buf.Bytes()).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if op != OpText || string(payload) != `{"ok":true}` {
		t.Fatalf("分片消息应被跳过、下一帧应正常交付；实际 op=%#x payload=%q", op, payload)
	}
}

// 分片消息累计超过上限 → 拒绝（防止用无数小片绕开单帧上限）。
func TestFragmentedMessageOverLimitRejected(t *testing.T) {
	var key [4]byte
	var buf bytes.Buffer
	chunk := bytes.Repeat([]byte("z"), 256*1024)
	// 5 × 256 KiB = 1.25 MiB > 1 MiB 上限（含首片）。
	for i := 0; i < 4; i++ {
		buf.Write(maskedFrame(OpText, false, chunk, key, 64))
	}
	buf.Write(maskedFrame(OpContinuation, true, chunk, key, 64))
	if _, _, err := connWith(buf.Bytes()).ReadFrame(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// 写超时：对端一直不读（net.Pipe 无缓冲）时，写必须在 WriteTimeout 内返回错误，
// 而不是永久阻塞把调用方钉死。
func TestWriteTimesOutWhenPeerStopsReading(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := NewConn(a)
	c.writeTimeout = 120 * time.Millisecond // 测试里压缩到毫秒级

	done := make(chan error, 1)
	go func() { done <- c.WriteText([]byte("blocking")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("对端不读时写应当超时报错")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("写没有在写超时内返回（WriteTimeout 未生效）")
	}
}
