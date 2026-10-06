package wsx

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// 握手
// ---------------------------------------------------------------------------

// RFC 6455 §1.3 官方向量：这是唯一可以"照抄标准"来证明 accept 算法正确的锚点
// （自己算一遍再自己验一遍的测试等于没测）。
func TestAcceptKeyRFC6455Vector(t *testing.T) {
	const (
		key    = "dGhlIHNhbXBsZSBub25jZQ=="
		accept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	)
	if got := acceptKey(key); got != accept {
		t.Fatalf("acceptKey(%q) = %q, want %q", key, got, accept)
	}
}

// validWSRequest 造一个合法的升级请求；mutate 用来逐项破坏它。
func validWSRequest(t *testing.T) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/panel/api/live", nil)
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")))
	return r
}

// 缺头/版本不对/key 不合法：必须报错，且**不写任何响应**（不写脏头）。
// 用 httptest.ResponseRecorder 还顺带证明了"校验发生在 hijack 之前"——
// 这个 recorder 根本不支持 Hijack，若先劫持就会拿到 ErrNoHijack 而不是 ErrBadHandshake。
func TestUpgradeRejectsBadHandshakeWithoutWritingResponse(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"missing-upgrade-header", func(r *http.Request) { r.Header.Del("Upgrade") }},
		{"upgrade-not-websocket", func(r *http.Request) { r.Header.Set("Upgrade", "h2c") }},
		{"missing-connection-header", func(r *http.Request) { r.Header.Del("Connection") }},
		{"connection-without-upgrade-token", func(r *http.Request) { r.Header.Set("Connection", "keep-alive") }},
		{"wrong-version", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "8") }},
		{"missing-version", func(r *http.Request) { r.Header.Del("Sec-WebSocket-Version") }},
		{"missing-key", func(r *http.Request) { r.Header.Del("Sec-WebSocket-Key") }},
		{"key-not-base64", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "!!!not-base64!!!") }},
		{"key-wrong-length", func(r *http.Request) {
			// 合法 base64，但解出来只有 5 字节（RFC 要求 16 字节随机数）。
			r.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString([]byte("short")))
		}},
		{"post-method", func(r *http.Request) { r.Method = http.MethodPost }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := validWSRequest(t)
			c.mutate(req)
			conn, err := Upgrade(rec, req)
			if conn != nil {
				_ = conn.Close()
				t.Fatal("非法握手不该返回 Conn")
			}
			if !errors.Is(err, ErrBadHandshake) {
				t.Fatalf("err = %v, want ErrBadHandshake", err)
			}
			if errors.Is(err, ErrNoHijack) {
				t.Fatal("校验必须先于 Hijack：非法请求不该走到劫持那一步")
			}
			if rec.Body.Len() != 0 {
				t.Errorf("非法握手不该写响应体，实际写了 %q", rec.Body.String())
			}
			if got := rec.Header().Get("Sec-WebSocket-Accept"); got != "" {
				t.Errorf("非法握手不该带 Sec-WebSocket-Accept，实际 %q", got)
			}
			if rec.Flushed {
				t.Error("非法握手不该 flush 任何东西")
			}
		})
	}
}

// 合法的握手 + 不支持 Hijack 的 ResponseWriter → ErrNoHijack（调用方据此回 501），
// 且同样不写脏头。
func TestUpgradeWithoutHijackSupport(t *testing.T) {
	rec := httptest.NewRecorder()
	conn, err := Upgrade(rec, validWSRequest(t))
	if conn != nil {
		_ = conn.Close()
		t.Fatal("不支持 Hijack 时不该返回 Conn")
	}
	if !errors.Is(err, ErrNoHijack) {
		t.Fatalf("err = %v, want ErrNoHijack", err)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("不该写响应体，实际 %q", rec.Body.String())
	}
}

// 真实 TCP 上跑完整握手：101 + 官方向量的 accept，随后服务端写文本帧、客户端按
// 协议解出来。这是"服务端写 → 客户端解"这条主链路的端到端证据。
func TestUpgradeOverTCPThenWriteTextAndClose(t *testing.T) {
	const text = `{"type":"snapshot","rev":1}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Upgrade(w, r)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		if err := conn.WriteText([]byte(text)); err != nil {
			t.Errorf("WriteText: %v", err)
			return
		}
		if err := conn.WriteClose(1001, "done"); err != nil {
			t.Errorf("WriteClose: %v", err)
		}
	}))
	defer srv.Close()

	nc, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer nc.Close()

	// Connection 故意写成 token 列表，验证逗号分隔解析。
	req := "GET /panel/api/live HTTP/1.1\r\nHost: " + strings.TrimPrefix(srv.URL, "http://") + "\r\n" +
		"Upgrade: websocket\r\nConnection: keep-alive, Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(nc, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	head := readHTTPHead(t, nc)

	if !strings.HasPrefix(head, "HTTP/1.1 101 ") {
		t.Fatalf("状态行不是 101:\n%s", head)
	}
	if !strings.Contains(head, "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=") {
		t.Fatalf("accept 与 RFC 官方向量不一致:\n%s", head)
	}
	if !strings.Contains(strings.ToLower(head), "upgrade: websocket") {
		t.Fatalf("101 缺少 Upgrade: websocket:\n%s", head)
	}

	op, payload, err := DecodeFrame(nc)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if op != OpText {
		t.Fatalf("opcode = 0x%x, want OpText", op)
	}
	if string(payload) != text {
		t.Fatalf("payload = %q, want %q", payload, text)
	}

	// CLOSE 帧：状态码 1001 + 原因。
	op, payload, err = DecodeFrame(nc)
	if err != nil {
		t.Fatalf("DecodeFrame(close): %v", err)
	}
	if op != OpClose {
		t.Fatalf("opcode = 0x%x, want OpClose", op)
	}
	if len(payload) < 2 || int(payload[0])<<8|int(payload[1]) != 1001 {
		t.Fatalf("close payload = %v, want code 1001", payload)
	}
}

// 服务端帧必须是"不掩码 + FIN=1"的最小形态：直接对字节拍板，
// 不依赖我们自己写的解码器（解码器错了两边一起错就测不出来）。
func TestServerFrameOnWireBytes(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	conn := NewConn(a)

	go func() {
		_ = conn.WriteText([]byte("hello"))
		_ = conn.WritePing(nil)
	}()

	raw := make([]byte, 2+5)
	if _, err := io.ReadFull(b, raw); err != nil {
		t.Fatalf("read raw frame: %v", err)
	}
	want := []byte{0x81, 0x05, 'h', 'e', 'l', 'l', 'o'}
	if !bytes.Equal(raw, want) {
		t.Fatalf("文本帧字节 = % x, want % x", raw, want)
	}
	ping := make([]byte, 2)
	if _, err := io.ReadFull(b, ping); err != nil {
		t.Fatalf("read ping frame: %v", err)
	}
	if !bytes.Equal(ping, []byte{0x89, 0x00}) {
		t.Fatalf("PING 帧字节 = % x, want 89 00", ping)
	}
}

// writeClose 之后本端不再接受任何写入（避免 CLOSE 之后还冒数据帧）。
func TestWriteCloseThenWriteFails(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	conn := NewConn(a)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(io.Discard, b)
	}()

	if err := conn.WriteClose(1000, "bye"); err != nil {
		t.Fatalf("WriteClose: %v", err)
	}
	if err := conn.WriteText([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("CLOSE 之后的写应返回 ErrClosed，实际 %v", err)
	}
	if err := conn.WritePing(nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("CLOSE 之后的 PING 应返回 ErrClosed，实际 %v", err)
	}
	_ = conn.Close()
	<-drained
}

// CLOSE 的 reason 不能把 UTF-8 字符切一半（严格客户端会判协议错误）。
func TestWriteCloseTruncatesReasonOnRuneBoundary(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	conn := NewConn(a)

	type result struct {
		op      byte
		payload []byte
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		op, p, err := DecodeFrame(b)
		ch <- result{op, p, err}
	}()
	// 200 个汉字远超 123 字节的容量，必然触发截断。
	reason := strings.Repeat("原", 100)
	if err := conn.WriteClose(1001, reason); err != nil {
		t.Fatalf("WriteClose: %v", err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatalf("DecodeFrame: %v", got.err)
	}
	if got.op != OpClose {
		t.Fatalf("opcode = 0x%x, want OpClose", got.op)
	}
	if len(got.payload) > maxControlPayload {
		t.Fatalf("close payload = %d 字节, 超过 %d", len(got.payload), maxControlPayload)
	}
	if !utf8.Valid(got.payload[2:]) {
		t.Fatalf("reason 被切成了非法 UTF-8: %q", got.payload[2:])
	}
}

// readHTTPHead 逐字节读到空行为止：绝不预读，避免把紧随其后的帧字节吞进缓冲
// （bufio 的最小缓冲是 16 字节，一样会多读——所以这里连 bufio 都不用）。
func readHTTPHead(t *testing.T, r io.Reader) string {
	t.Helper()
	var sb strings.Builder
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, one); err != nil {
			t.Fatalf("read response head: %v (got %q)", err, sb.String())
		}
		sb.WriteByte(one[0])
		s := sb.String()
		if strings.HasSuffix(s, "\r\n\r\n") || strings.HasSuffix(s, "\n\n") {
			return s
		}
	}
}
