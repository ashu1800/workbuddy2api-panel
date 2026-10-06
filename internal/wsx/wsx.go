// Package wsx 极简 WebSocket（RFC 6455）服务端实现：只覆盖管理面板实时推送
// 需要的那一小块——握手、文本帧、PING/PONG/CLOSE 控制帧。
//
// 为什么自己写而不是引第三方库：本仓库的依赖只有 xxhash / go-redis（见 go.mod），
// 面板推送是纯"服务端写、客户端读控制帧"的单向数据流，协议面窄到几百行就能覆盖；
// 为这点需求长期背一条依赖链（升级、CVE、vendor 体积）不划算。代价是必须自己
// 守住 RFC 的硬约束，下面每条都写明了取舍。
//
// 覆盖范围与明确取舍：
//   - 只做服务端一侧：客户端帧必须带掩码（RFC 6455 §5.1 强制），服务端帧不掩码；
//   - 单帧上限 MaxFramePayload（1 MiB）：面板推送的载荷是账号列表 JSON，实测量级是 KB；
//   - 分片消息不重组、直接跳过（见 Conn.ReadFrame 注释里的取舍说明）；
//   - 不做扩展协商：握手不回 Sec-WebSocket-Extensions，浏览器不会启用 permessage-deflate。
package wsx

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// 帧操作码（RFC 6455 §5.2）。
const (
	OpContinuation byte = 0x0
	OpText         byte = 0x1
	OpBinary       byte = 0x2
	OpClose        byte = 0x8
	OpPing         byte = 0x9
	OpPong         byte = 0xA
)

const (
	// MaxFramePayload 单帧载荷上限（1 MiB）。超限的帧在**分配内存之前**就被拒绝，
	// 避免对端只声明一个巨大长度就把服务端内存打爆（见 readFrame）。
	MaxFramePayload = 1 << 20
	// maxControlPayload 控制帧载荷上限：RFC 6455 §5.5 规定 125 字节。
	maxControlPayload = 125
	// WriteTimeout 单次写的超时（含握手响应）。对端 TCP 窗口打满时写会永久阻塞，
	// 没有这条超时，一个卡死的浏览器就能把服务端的写 goroutine 钉住不还。
	WriteTimeout = 10 * time.Second
	// maxFragmentTotal 被跳过的分片消息的累计载荷上限（与单帧上限同口径）。
	maxFragmentTotal = MaxFramePayload
	// closeReasonMax CLOSE 帧里 reason 的最大字节数（125 - 2 字节状态码）。
	closeReasonMax = maxControlPayload - 2
	// wsGUID RFC 6455 §1.3 的握手魔数。
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

var (
	// ErrBadHandshake 握手请求不合法（方法/头/版本/key 不对）。调用方据此回 400，
	// 且因为校验发生在 Hijack 之前，此时连接还完全是 HTTP 语义。
	ErrBadHandshake = errors.New("wsx: bad handshake")
	// ErrNoHijack 底层 ResponseWriter 不支持 Hijack。调用方据此回 501。
	ErrNoHijack = errors.New("wsx: response writer does not support hijack")
	// ErrProtocol 对端违反协议（RSV 位、未知操作码、未掩码的客户端帧、控制帧超长/分片）。
	ErrProtocol = errors.New("wsx: protocol error")
	// ErrTooLarge 帧或消息超过上限。
	ErrTooLarge = errors.New("wsx: frame too large")
	// ErrClosed 本端已发过 CLOSE（或已 Close），不再接受写入。
	ErrClosed = errors.New("wsx: connection closed")
)

// Conn 一条已升级完成的 WebSocket 连接。
//
// 并发约定：写（WriteText/WritePing/WritePong/WriteClose）内部用互斥串行化，
// 可以从多个 goroutine 调用（面板的写 goroutine 与回 PONG 的读循环就是两处）；
// 读（ReadFrame）不是并发安全的，同一时刻只允许一个 goroutine 读。
type Conn struct {
	conn net.Conn
	r    io.Reader // 读来源：Hijack 返回的 bufio.Reader（可能已缓冲客户端预发的帧）

	wmu       sync.Mutex
	closed    atomic.Bool
	closeOnce sync.Once

	writeTimeout time.Duration
}

// NewConn 用一条裸连接构造 Conn（读来源就是这条连接本身，不做额外缓冲）。
// 用途：把 wsx 嵌进别的协议栈，或测试里用 net.Pipe 造一条可控连接。
// 注意：若这条连接上已经被缓冲读取过数据（例如 http.Hijack 返回的 bufio.Reader），
// 必须走 Upgrade，否则缓冲区里的字节会丢。
func NewConn(c net.Conn) *Conn {
	return &Conn{conn: c, r: c, writeTimeout: WriteTimeout}
}

// CheckHandshake 校验 RFC 6455 握手请求，不写任何响应、不劫持连接。
//
// 单独导出是给调用方留出"回 400"的机会：一旦走到 Upgrade 内部才失败，连接可能
// 已经 Hijack（101 写失败那一步），错误就只能记日志、再也回不了 HTTP 语义。
// Upgrade 自己也会再校验一遍——它是公开 API，必须自带防御，不依赖调用方先校验。
func CheckHandshake(r *http.Request) error {
	if r.Method != http.MethodGet {
		return fmt.Errorf("%w: method %s", ErrBadHandshake, r.Method)
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return fmt.Errorf("%w: Upgrade=%q", ErrBadHandshake, r.Header.Get("Upgrade"))
	}
	// Connection 是逗号分隔的 token 列表（可能是 "keep-alive, Upgrade"）。
	if !headerHasToken(r.Header.Get("Connection"), "upgrade") {
		return fmt.Errorf("%w: Connection=%q", ErrBadHandshake, r.Header.Get("Connection"))
	}
	if v := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")); v != "13" {
		return fmt.Errorf("%w: Sec-WebSocket-Version=%q", ErrBadHandshake, v)
	}
	// key 必须是 base64 编码的 16 字节随机数（RFC 6455 §4.1）：只检查"能解码"不够，
	// 长度不对说明对端不是按协议生成的，回 101 会得到一个双方都算不出一致 accept 的连接。
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key")))
	if err != nil || len(raw) != 16 {
		return fmt.Errorf("%w: bad Sec-WebSocket-Key", ErrBadHandshake)
	}
	return nil
}

// Upgrade 完成握手并返回可读写的 Conn：校验请求头 → Hijack → 写 101。
// 任何失败都不写"脏头"：校验失败时连接原封不动（调用方还能正常回 400/501）。
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if err := CheckHandshake(r); err != nil {
		return nil, err
	}
	// 用 ResponseController 而不是 w.(http.Hijacker) 断言：面板可能挂在
	// 中间件/观测 wrapper 后面，包装类型往往只实现 ResponseWriter 而把 Hijack
	// "吞掉"，直接类型断言会失败；ResponseController 会沿 Unwrap 链一路找下去
	//（Go 1.20+ 的标准库口径），拿到底层真实能力。
	netConn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoHijack, err)
	}
	// Hijack 之后 http.Server 不再管这条连接，但它读请求头时设的读/写超时还留在
	// 底层 conn 上，必须显式清掉——否则连接会在 ReadTimeout 到点时被莫名掐断
	// （面板是长连接，服务端那些超时语义对它全部失效）。
	_ = netConn.SetDeadline(time.Time{})

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"
	_ = netConn.SetWriteDeadline(time.Now().Add(WriteTimeout))
	if _, err := netConn.Write([]byte(resp)); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("wsx: write handshake response: %w", err)
	}

	var src io.Reader = netConn
	if brw != nil {
		// 客户端可能在收到 101 之前就抢发了帧，这些字节已经在 bufio 缓冲里；
		// 必须从缓冲读，否则第一帧会"凭空消失"。
		src = brw.Reader
	}
	return &Conn{conn: netConn, r: src, writeTimeout: WriteTimeout}, nil
}

// acceptKey 计算 Sec-WebSocket-Accept（RFC 6455 §4.2.2：key + GUID 的 SHA-1 再 base64）。
func acceptKey(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, strings.TrimSpace(key))
	_, _ = io.WriteString(h, wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// headerHasToken 判断逗号分隔的头里是否含某个 token（大小写不敏感）。
func headerHasToken(v, token string) bool {
	for _, part := range strings.Split(v, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// WriteText 写一个文本帧（面板推送的唯一数据形态：JSON 文本）。
func (c *Conn) WriteText(b []byte) error { return c.writeFrame(OpText, b) }

// WritePing 写 PING 控制帧（保活；对端应回 PONG）。
func (c *Conn) WritePing(b []byte) error { return c.writeControl(OpPing, b) }

// WritePong 写 PONG 控制帧（应答对端的 PING）。
func (c *Conn) WritePong(b []byte) error { return c.writeControl(OpPong, b) }

// WriteClose 写 CLOSE 帧（状态码 + 原因）并标记本端不再写。
// 不关闭底层连接：对端的 CLOSE 应答可能还没读到，关闭时机交给调用方。
func (c *Conn) WriteClose(code int, reason string) error {
	p := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(p, uint16(code))
	p = append(p, truncateUTF8(reason, closeReasonMax)...)
	err := c.writeControl(OpClose, p)
	c.closed.Store(true)
	return err
}

// SetReadDeadline 设置读超时。空闲超时的惯用实现是"每次读之前重设一次"，
// 所以这里不做任何内部计时，纯透传（调用方掌握语义：面板用 90s 判死连接）。
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// Close 关闭底层连接（幂等）。不发送 CLOSE 帧——要优雅关闭请先 WriteClose。
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		err = c.conn.Close()
	})
	return err
}

// ReadFrame 读下一个"可交付"的帧：控制帧，或一个已结束（FIN=1）的数据帧。
//
// 分片（FIN=0）的取舍——**跳过并丢弃整条分片消息**，继续读下一帧，不返回给调用方、
// 也不做重组。理由：本协议是纯服务端→客户端方向，客户端只回控制帧，浏览器/前端
// 永远不会分片发送数据；为一条用不到的能力维护重组缓冲、总长度上限、continuation
// 乱序这些边界是纯负债。代价写在这里：若将来允许客户端上行数据，必须在 skipFragments
// 处补真正的重组，否则上行数据会被静默丢弃（不会报错，只是"发不进去"）。
//
// 跳过期间的控制帧同样被丢弃：服务端每 25s 主动 PING，丢一两个 PING/PONG 不影响
// 保活判断（读超时仍按调用方设的 deadline 生效），而把它们插回返回值需要额外的
// 待读队列，同样是划不来的复杂度。
//
// 返回值语义区分得很清楚：
//   - 收到 CLOSE 帧：返回 (OpClose, payload, nil)——这是"对端正常关闭"，不是错误；
//   - 对端直接关掉 TCP：返回 (0, nil, io.EOF)；
//   - 协议违规/超限：返回包装了 ErrProtocol/ErrTooLarge 的错误；
//   - 超时：返回 net.Error（Timeout()==true）。
func (c *Conn) ReadFrame() (byte, []byte, error) {
	for {
		fin, op, payload, err := readFrame(c.r, true)
		if err != nil {
			return 0, nil, err
		}
		if op >= 0x8 || fin {
			return op, payload, nil
		}
		if err := c.skipFragments(len(payload)); err != nil {
			return 0, nil, err
		}
	}
}

// skipFragments 丢弃一条分片消息的剩余帧（直到 FIN=1 的数据帧）。
// initial 是首片已读到的载荷长度，计入总上限——上限的语义要是"整条分片消息
// ≤ maxFragmentTotal"，否则对端只要把每片都卡在单帧上限内，就能拼出无限长的消息。
func (c *Conn) skipFragments(initial int) error {
	total := initial
	for {
		fin, op, payload, err := readFrame(c.r, true)
		if err != nil {
			return err
		}
		if op < 0x8 {
			total += len(payload)
			if total > maxFragmentTotal {
				return fmt.Errorf("%w: fragmented message exceeds %d bytes", ErrTooLarge, maxFragmentTotal)
			}
		}
		if fin && op < 0x8 {
			return nil
		}
	}
}

// DecodeFrame 从一个裸流里读一个帧，返回操作码与**已解掩码**的载荷；掩码/非掩码都接受。
// 给客户端一侧用（含测试里的裸 TCP 客户端）：服务端读路径必须用 Conn.ReadFrame，
// 它额外强制"客户端帧必须带掩码"，否则会在 RFC 上留一个"未掩码帧被接受"的口子。
//
// 实现只用 io.ReadFull 精确读取、不做额外缓冲：调用方传 net.Conn 时不会因为
// 一次性多读一个 TCP 段而把后续帧吞进缓冲区。
func DecodeFrame(r io.Reader) (byte, []byte, error) {
	_, op, payload, err := readFrame(r, false)
	if err != nil {
		return 0, nil, err
	}
	return op, payload, nil
}

// writeFrame 编码并写出一个帧（服务端方向：不掩码）。
func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	if c.closed.Load() {
		return ErrClosed
	}
	frame := encodeFrame(opcode, payload, false)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// 锁内再判一次：WriteClose 可能与并发写竞争，避免"CLOSE 之后还写出数据帧"。
	if c.closed.Load() {
		return ErrClosed
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
		return fmt.Errorf("wsx: set write deadline: %w", err)
	}
	if _, err := c.conn.Write(frame); err != nil {
		return err
	}
	return nil
}

// writeControl 写控制帧：先卡 125 字节上限（RFC 强约束，超了必须拒绝而不是截断）。
func (c *Conn) writeControl(opcode byte, payload []byte) error {
	if len(payload) > maxControlPayload {
		return fmt.Errorf("%w: control frame payload %d > %d", ErrTooLarge, len(payload), maxControlPayload)
	}
	return c.writeFrame(opcode, payload)
}

// readFrame 读一个帧。requireMask=true 时强制掩码（服务端读客户端帧的口径）。
//
// 内存安全的关键点：载荷长度在**分配之前**就被上限卡掉——对端只要声明 2^62 字节，
// 若先 make 再校验，服务端当场 OOM。所以顺序是：解长度 → 校验上限 → 才分配。
func readFrame(r io.Reader, requireMask bool) (fin bool, opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return false, 0, nil, err // io.EOF / io.ErrUnexpectedEOF / net 超时都原样透出
	}
	fin = hdr[0]&0x80 != 0
	if hdr[0]&0x70 != 0 {
		// RSV1-3：本实现不协商任何扩展，置位即违规（浏览器不会发）。
		return false, 0, nil, fmt.Errorf("%w: reserved bits set", ErrProtocol)
	}
	opcode = hdr[0] & 0x0F
	if !validOpcode(opcode) {
		return false, 0, nil, fmt.Errorf("%w: unknown opcode 0x%x", ErrProtocol, opcode)
	}
	masked := hdr[1]&0x80 != 0
	n := int64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		n = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		u := binary.BigEndian.Uint64(ext[:])
		// RFC 6455 §5.2：最高位必须为 0（长度不能超过 2^63-1）。
		if u > 1<<62 {
			return false, 0, nil, fmt.Errorf("%w: invalid 64-bit length", ErrProtocol)
		}
		n = int64(u)
	}
	if opcode >= 0x8 {
		// 控制帧必须"不可分片 + ≤125 字节"（RFC 6455 §5.5）。
		if !fin {
			return false, 0, nil, fmt.Errorf("%w: fragmented control frame", ErrProtocol)
		}
		if n > maxControlPayload {
			return false, 0, nil, fmt.Errorf("%w: control frame payload %d > %d", ErrProtocol, n, maxControlPayload)
		}
	}
	if n > MaxFramePayload {
		return false, 0, nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, n)
	}
	if requireMask && !masked {
		return false, 0, nil, fmt.Errorf("%w: client frame must be masked", ErrProtocol)
	}
	var key [4]byte
	if masked {
		if _, err = io.ReadFull(r, key[:]); err != nil {
			return false, 0, nil, err
		}
	}
	if n > 0 {
		payload = make([]byte, n)
		if _, err = io.ReadFull(r, payload); err != nil {
			return false, 0, nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= key[i%4]
			}
		}
	}
	return fin, opcode, payload, nil
}

// encodeFrame 编码一个帧。masked=true 时生成客户端方向的掩码帧（测试构造客户端
// 请求用它；服务端恒为 false）。
//
// 长度编码（RFC 6455 §5.2）：≤125 直接放 7 位；126..65535 用 7 位 + 2 字节；
// 更大用 7 位 + 8 字节。三档边界（125/126/65535/65536）都有测试钉住。
func encodeFrame(opcode byte, payload []byte, masked bool) []byte {
	n := len(payload)
	head := 2
	switch {
	case n < 126:
	case n <= 0xFFFF:
		head += 2
	default:
		head += 8
	}
	if masked {
		head += 4
	}
	buf := make([]byte, head+n)
	buf[0] = 0x80 | opcode // FIN=1：本实现只发完整消息
	i := 2
	switch {
	case n < 126:
		buf[1] = byte(n)
	case n <= 0xFFFF:
		buf[1] = 126
		binary.BigEndian.PutUint16(buf[2:4], uint16(n))
		i = 4
	default:
		buf[1] = 127
		binary.BigEndian.PutUint64(buf[2:10], uint64(n))
		i = 10
	}
	if !masked {
		copy(buf[i:], payload)
		return buf
	}
	buf[1] |= 0x80
	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		// crypto/rand 不可用是系统级故障；掩码的作用只是防中间代理缓存污染，
		// 退化成全零掩码仍然是一个合法帧，比整条连接建不起来强。
		key = [4]byte{}
	}
	copy(buf[i:i+4], key[:])
	i += 4
	for j, b := range payload {
		buf[i+j] = b ^ key[j%4]
	}
	return buf
}

// validOpcode 判断操作码是否是 RFC 6455 定义过的（0x3-0x7、0xB-0xF 保留未用）。
func validOpcode(op byte) bool {
	switch op {
	case OpContinuation, OpText, OpBinary, OpClose, OpPing, OpPong:
		return true
	}
	return false
}

// truncateUTF8 把 s 截到不超过 max 字节，且不切断 UTF-8 编码点。
// 为什么在意：CLOSE 帧的 reason 按 RFC 必须是合法 UTF-8，直接在字节层截断
// 中文原因会产出半个字符，严格的客户端会判定为协议错误。
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	b := []byte(s)[:max]
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size > 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return string(b)
}
