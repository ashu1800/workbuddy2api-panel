// live.go 面板账号池的实时推送（WebSocket + 变化检测 + 票据鉴权）。
//
// 为什么要有它：面板此前是"前端每 5s 轮询 /panel/api/overview"。轮询有两个固有
// 毛病——变化最多要等一个周期才可见（点了"暂停选号"，按钮状态慢半拍），以及
// **空闲时也在打**：没人看面板时每秒（每 5s）仍要做一次全量 pool.List() +
// usage.Snapshot()。改成服务端推送后：变化 1s 内到达，且"没人订阅"时零开销
//（无订阅者不跑 ticker、无常驻 goroutine）。
//
// 三个关键设计：
//
//  1. **票据鉴权**：浏览器 WebSocket API 不能自定义请求头，Authorization 根本发不
//     出去，所以不能直接复用 Bearer。做法是先带 Bearer 换一张一次性短 TTL 票据，
//     再用 ?ticket= 升级：既不给 URL 留长期凭据（30s 过期 + 用一次即焚，泄露窗口
//     极小，且浏览器历史/日志里留下的也是废票），信任根仍是同一把 api_key。
//
//  2. **逐 key 原始 JSON 比较**：把 overview 载荷拆成 pool 计数 / per-uid 账号 /
//     model_locks 三段再逐 key bytes.Equal，而不是手写"哪些字段要比较"的清单。
//     池状态字段（token_usage / model_costs / rate_limited_models …）还在演进，
//     手写清单必然漏字段：漏了就永远推不到前端，而且**不会报错**。
//
//  3. **排除 uptime_sec**：它每秒都变。若参与比较，每个 tick 都"有变化"，
//     推送就退化成 1s 一次的轮询——正是要摆脱的东西。它只出现在全量 snapshot 里
//     （前端拿它显示运行时长，漂移由每 60 tick 的自愈全量纠正）。
package panel

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/wsx"
)

// 实时推送的全部可调参数（集中在这里，便于对照协议文档核数）。
const (
	// liveTickInterval 变化检测周期。1s：比分秒级轮询更快的感知，又不至于让
	// pool.List() 变成 CPU 热点（哪怕 100 个号，一次比较也是微秒级）。
	liveTickInterval = time.Second
	// liveFullSnapshotEvery 每隔多少个 tick 强制发一次全量 snapshot（自愈/去漂移）：
	// 增量推送一旦因为任何原因与真实状态漂了（丢帧、前端合并出错、被中间设备改包），
	// 60s 内必然被一份全量拉回正确值，不需要重连。
	liveFullSnapshotEvery = 60
	// livePingInterval 服务端主动 PING 的间隔（控制帧，浏览器自动回 PONG）。
	livePingInterval = 25 * time.Second
	// liveIdleTimeout 读空闲上限：超过它没有任何入站帧就判死连接（半开 TCP 的兜底）。
	liveIdleTimeout = 90 * time.Second
	// liveQueueSize 每订阅者的发送队列长度。32 帧 ≈ 32s 的推送量：一个只是"卡一下"
	// 的浏览器能靠它缓冲过来，而真卡死的（连读都不读）会很快填满并被断开。
	liveQueueSize = 32
	// liveMaxSubscribers 订阅者上限。面板是运维工具，16 个标签页已经远超真实用量；
	// 上限的意义是防止连接泄漏（前端 bug 反复重连且不关旧连接）把服务端拖垮。
	liveMaxSubscribers = 16
	// liveTicketTTL 票据有效期：够一次"取票 → 建连"往返（毫秒级），又短到即使
	// 票据被写进访问日志也基本立刻失效。
	liveTicketTTL = 30 * time.Second
)

// errTooManySubscribers 订阅者超限（HTTP 层转 503）。
var errTooManySubscribers = errors.New("too many subscribers")

// ---------------------------------------------------------------------------
// 票据
// ---------------------------------------------------------------------------

// ticketStore 一次性票据池。
//
// 存票据的 SHA-256 摘要而不是原文：内存被 dump 也拿不到可用票据；且定长摘要把
// 长度差异吸收进摘要，让常量时间比较名副其实——直接比原文时，长度不同会让
// subtle.ConstantTimeCompare 立刻返回 0，白白泄露"长度是否匹配"这一个 bit。
type ticketStore struct {
	mu  sync.Mutex
	m   map[[32]byte]time.Time // 摘要 → 过期时刻
	now func() time.Time       // 可注入时钟（测试用它推进过期，不靠 sleep）
}

func newTicketStore() *ticketStore {
	return &ticketStore{m: map[[32]byte]time.Time{}, now: time.Now}
}

// issue 签发一张新票据，并顺带清扫已过期的。
// 清扫挂在签发路径而不是单独起定时器/goroutine：签发频率天然限制了 map 增长
//（每次取票都会清一遍），无订阅者/无人取票时零开销——与"无订阅者不跑 ticker"
// 同一条设计原则。
func (s *ticketStore) issue() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand 失败是系统级异常；退回时间戳派生（熵更低但仍然一次性+短 TTL）。
		binary.BigEndian.PutUint64(raw[:8], uint64(time.Now().UnixNano()))
	}
	t := base64.RawURLEncoding.EncodeToString(raw[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)
	s.m[sha256.Sum256([]byte(t))] = now.Add(liveTicketTTL)
	return t
}

// consume 校验并**消费**一张票据：有效且未使用过返回 true，用掉即删（一次性）。
// 遍历全部票据做常量时间比较；票据数被 TTL（30s）与签发频率双重限制，是常数级别，
// 不必为了 O(1) 换成"map 直接命中"那种带计时侧信道的查法。
func (s *ticketStore) consume(t string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)
	want := sha256.Sum256([]byte(t))
	ok := 0
	for k, exp := range s.m {
		match := subtle.ConstantTimeCompare(k[:], want[:])
		if match == 1 && exp.After(now) {
			delete(s.m, k)
			ok = 1
		}
	}
	return ok == 1
}

// sweepLocked 清掉已过期票据。调用方必须持有 s.mu。
func (s *ticketStore) sweepLocked(now time.Time) {
	for k, exp := range s.m {
		if !exp.After(now) {
			delete(s.m, k)
		}
	}
}

// pending 当前未过期票据数（测试可见：验证"签发时顺带清扫"确实生效）。
func (s *ticketStore) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// setClock 替换时钟（仅测试用：把 30s TTL 的过期路径变成确定性的）。
func (s *ticketStore) setClock(fn func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = fn
}

// ---------------------------------------------------------------------------
// 状态快照与差分
// ---------------------------------------------------------------------------

// livePoolKeys pool 段参与比较的计数字段。
// 显式列 key（而不是把整份 overview 丢进比较）就是为了把 uptime_sec 排除在外：
// 它是"每秒必变"的字段，一旦进比较，每个 tick 都会产生一次推送。
var livePoolKeys = []string{
	"total", "healthy", "cooling", "disabled", "in_flight_full", "sticky_sessions",
}

// liveAcct 单个账号的快照分片：既有完整对象（snapshot 的整数组 / patch 的 added
// 需要完整对象），也有逐字段的原始 JSON（patch 只发变化的字段）。
type liveAcct struct {
	full   json.RawMessage
	fields map[string]json.RawMessage
}

// liveState 一个 tick 的状态快照。在 hub 里**算一次**给所有订阅者共用。
type liveState struct {
	payload  json.RawMessage            // overview 完整载荷（含 uptime_sec；只进 snapshot）
	pool     map[string]json.RawMessage // 计数：key → 原始值
	accounts map[string]*liveAcct       // uid → 账号分片
	order    []string                   // 账号顺序（uid 列表）
	locks    json.RawMessage            // model_locks 原始 JSON（整体替换语义）
}

// buildState 采样一次当前状态。与 HTTP 轮询同源：直接调 overviewPayload()，
// 保证 WS 推送与 /panel/api/overview 的字段永远一致（两条路径各写一份必然漂移）。
func (h *liveHub) buildState() *liveState {
	raw, err := json.Marshal(h.p.overviewPayload())
	if err != nil {
		// 载荷是 map[string]any + 结构体，正常不可能失败；退化成空态让下个 tick
		// 重试，而不是让整个推送循环崩掉。
		return emptyState()
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return emptyState()
	}
	st := &liveState{
		payload:  raw,
		pool:     make(map[string]json.RawMessage, len(livePoolKeys)),
		accounts: map[string]*liveAcct{},
	}
	for _, k := range livePoolKeys {
		if v, ok := top[k]; ok {
			st.pool[k] = v
		}
	}
	locks := top["model_locks"]
	if len(locks) == 0 {
		locks = json.RawMessage("null")
	}
	st.locks = locks

	var accts []json.RawMessage
	if rawAccts, ok := top["accounts"]; ok {
		_ = json.Unmarshal(rawAccts, &accts)
	}
	for _, one := range accts {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(one, &fields); err != nil {
			continue
		}
		var uid string
		if err := json.Unmarshal(fields["uid"], &uid); err != nil || uid == "" {
			continue
		}
		st.accounts[uid] = &liveAcct{full: one, fields: fields}
		st.order = append(st.order, uid)
	}
	// pool.List() 本来就按 UID 排序，这里再排一次是防御：顺序必须稳定，
	// 否则"顺序变了"会被误报成一次推送（order 也是协议语义的一部分）。
	sort.Strings(st.order)
	return st
}

// emptyState 空状态（仅在理论上不可能的 marshal 失败时使用）。
func emptyState() *liveState {
	return &liveState{
		payload:  json.RawMessage("null"),
		pool:     map[string]json.RawMessage{},
		accounts: map[string]*liveAcct{},
		locks:    json.RawMessage("null"),
	}
}

// diffRawMaps 逐 key 比较两份原始 JSON，返回"值变了 / 新增 / 消失"的 key → 新值。
//
// 为什么用 bytes.Equal 比原始字节、而不是反序列化后比语义：JSON 里 1 与 1.0 语义
// 相同但字节不同，会多推一次——而"多推一次绝对值"永远安全（前端应用是幂等的），
// "漏推一次"才是 bug。逐 key 的字节比较还有一个决定性好处：不依赖任何字段清单，
// 池以后加字段（比如新的 token 统计）会自动被推送覆盖。
//
// 字段**消失**（omitempty 字段在新状态里被省略）时下发显式 null，而不是干脆不发：
// 例如 paused 从 true 变 false、rate_limited_models 清空——若不发，前端会把旧值
// 一直显示下去（点了"恢复选号"却还挂着"已暂停"），直到 60s 后的自愈全量才纠正。
// 前端约定：值为 null = 该字段已消失，应删除/置空本地字段（不要当 undefined 直接
// 解引用嵌套对象，例如 token_usage 的 null）。
func diffRawMaps(prev, cur map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range cur {
		old, ok := prev[k]
		if !ok || !bytes.Equal(old, v) {
			out[k] = v
		}
	}
	for k := range prev {
		if _, ok := cur[k]; !ok {
			out[k] = json.RawMessage("null")
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// diffLive 把两份状态之差整理成 patch 载荷（只放确实变化的段）。
// changed=false 表示"这一秒什么都没变"，调用方据此不发帧——这是"实时推送"相对
// 轮询省下来的那部分带宽/CPU。
func diffLive(prev, cur *liveState) (map[string]any, bool) {
	patch := map[string]any{}
	changed := false

	if pc := diffRawMaps(prev.pool, cur.pool); len(pc) > 0 {
		patch["pool"] = pc
		changed = true
	}

	acctChanges := map[string]map[string]json.RawMessage{}
	var addedUIDs []string
	var removed []string
	for uid, a := range cur.accounts {
		old, ok := prev.accounts[uid]
		if !ok {
			addedUIDs = append(addedUIDs, uid)
			continue
		}
		if f := diffRawMaps(old.fields, a.fields); len(f) > 0 {
			acctChanges[uid] = f
		}
	}
	for uid := range prev.accounts {
		if _, ok := cur.accounts[uid]; !ok {
			removed = append(removed, uid)
		}
	}
	// 排序：map 遍历顺序随机，输出必须稳定（便于测试与人工比对抓包）。
	sort.Strings(addedUIDs)
	sort.Strings(removed)
	if len(addedUIDs) > 0 {
		added := make([]json.RawMessage, 0, len(addedUIDs))
		for _, uid := range addedUIDs {
			added = append(added, cur.accounts[uid].full)
		}
		patch["added"] = added
		changed = true
	}
	if len(acctChanges) > 0 {
		patch["accounts"] = acctChanges
		changed = true
	}
	if len(removed) > 0 {
		patch["removed"] = removed
		changed = true
	}
	if !liveEqualStrings(prev.order, cur.order) {
		patch["order"] = cur.order
		changed = true
	}
	if !bytes.Equal(prev.locks, cur.locks) {
		patch["model_locks"] = cur.locks
		changed = true
	}
	return patch, changed
}

// liveEqualStrings 比较两个字符串切片是否逐元素相等。
// （名字带 live 前缀：包内已有测试辅助函数叫 equalStrings，不能重名。）
func liveEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 订阅者
// ---------------------------------------------------------------------------

// liveSub 一个订阅者：发送队列 + 写 goroutine + 读循环。
type liveSub struct {
	conn *wsx.Conn
	send chan []byte
	done chan struct{}
	once sync.Once

	mu     sync.Mutex
	reason string
}

// enqueue 投递一帧。队列满 = 慢客户端 → 立刻断开，绝不阻塞：
// 阻塞会把一个卡死的浏览器变成所有订阅者（乃至整个 tick 循环）的停顿。
func (s *liveSub) enqueue(msg []byte) {
	if len(msg) == 0 {
		return
	}
	select {
	case <-s.done:
		// 已经在关闭流程里：静默丢弃。若在这里再报一次，慢客户端被断开后的
		// 每个 tick 都会再刷一条日志（断开要等写超时，最长 10s）。
		return
	default:
	}
	select {
	case s.send <- msg:
	default:
		log.Printf("panel: live 订阅者发送队列已满（%d 帧），判定为慢客户端并断开", liveQueueSize)
		s.shutdown("slow_consumer")
	}
}

// shutdown 请求断开：记录首个原因并关闭 done（幂等）。
// 为什么这里不直接写帧：写入必须与 tick 侧解耦。若在 tick 里同步写"bye"，
// 一个慢客户端会把 tick 卡住整个写超时（10s），全体订阅者一起挨饿。
func (s *liveSub) shutdown(reason string) {
	s.mu.Lock()
	if s.reason == "" {
		s.reason = reason
	}
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
}

func (s *liveSub) closeReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason == "" {
		return "closed"
	}
	return s.reason
}

// writeLoop 写循环：这条连接上唯一主动写数据的 goroutine（读循环回 PONG 是唯一
// 例外，wsx.Conn 内部用互斥把并发写串行化，两者可以共存）。
// 队列 + 独立 goroutine 的意义是把"客户端读得慢"与"服务端 tick"解耦。
func (s *liveSub) writeLoop() {
	defer func() { _ = s.conn.Close() }()
	ping := time.NewTicker(livePingInterval)
	defer ping.Stop()
	for {
		select {
		case msg := <-s.send:
			if err := s.conn.WriteText(msg); err != nil {
				s.shutdown("write_error")
				return
			}
		case <-ping.C:
			// 心跳：25s 一个 PING 控制帧。浏览器收到会自动回 PONG，
			// 读循环那边的"读空闲 90s"因此被刷新——两者配合才能区分
			// "空闲但活着"与"TCP 半开、其实已经死了"。
			if err := s.conn.WritePing(nil); err != nil {
				s.shutdown("write_error")
				return
			}
		case <-s.done:
			reason := s.closeReason()
			if b, err := json.Marshal(map[string]any{"type": "bye", "reason": reason}); err == nil {
				_ = s.conn.WriteText(b) // 尽力而为：对端可能已经走了
			}
			_ = s.conn.WriteClose(1001, reason)
			return
		}
	}
}

// readLoop 读循环。本协议是"服务端推、客户端只回控制帧"，所以：
//   - PING → 回 PONG（RFC 要求对端可据此确认链路）；
//   - PONG → 唯一作用是刷新下面的读 deadline；
//   - CLOSE → 回 CLOSE 并退出（对端正常关闭）；
//   - 文本/二进制帧一律忽略（前向兼容：将来前端要上行什么，服务端不会因为
//     不认识就断连——增加一条上行命令不应该需要先改服务端）。
//
// 读空闲超过 liveIdleTimeout 判死连接，由"每次读之前重设 deadline"实现。
func (s *liveSub) readLoop() {
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(liveIdleTimeout))
		op, payload, err := s.conn.ReadFrame()
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				// 对端直接关了 TCP：最常见的收场，不打日志。
				s.shutdown("peer_eof")
			case isTimeoutErr(err):
				log.Printf("panel: live 订阅者读空闲超过 %s，判定死连接并断开", liveIdleTimeout)
				s.shutdown("idle_timeout")
			default:
				log.Printf("panel: live 订阅者读循环结束: %v", err)
				s.shutdown("read_error")
			}
			return
		}
		switch op {
		case wsx.OpPing:
			_ = s.conn.WritePong(payload)
		case wsx.OpClose:
			_ = s.conn.WriteClose(1000, "") // 回 CLOSE（RFC 6455 §5.5.1）
			s.shutdown("peer_close")
			return
		case wsx.OpPong:
			// 空分支：PONG 的全部意义就是刷新上面的读 deadline。
		default:
			// 文本/二进制：忽略（前向兼容）。
		}
	}
}

// isTimeoutErr 判断错误是不是超时（net.Error 的 Timeout 口径）。
func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ---------------------------------------------------------------------------
// Hub
// ---------------------------------------------------------------------------

// liveHub 实时推送中枢：订阅者集合 + 状态基线 + ticker 生命周期。
//
// ticker 只在"有订阅者"时存在：首个订阅者启动、最后一个停止。无订阅者时
// 没有常驻 goroutine、没有定时器、没有采集——面板没人看的时候零开销。
type liveHub struct {
	p    *Panel
	boot string // 进程标识（8 hex）：前端用它区分"服务端重启过"，rev 只在本进程内单调

	mu      sync.Mutex
	subs    map[*liveSub]struct{}
	pending int // 已预占但还没 attach 的名额（并发 upgrade 之间不能超卖）
	prev    *liveState
	rev     uint64
	running bool
	stopCh  chan struct{}

	ticks atomic.Int64 // 已跑过的 tick 数（测试可见：验证"无订阅者不跑"）

	// tickInterval 变化检测周期。生产恒为 liveTickInterval；单元测试把它调大，
	// 好让断言的时序只由手动 h.tick() 驱动（否则后台 ticker 会在断言窗口里插一帧）。
	tickInterval time.Duration

	tickets *ticketStore
}

// newLiveHub 构造 hub。boot 取 4 字节随机数的 hex（8 字符）：进程重启后前端能
// 一眼看出 rev 断了是"服务端重启"而不是"丢了帧"。
func newLiveHub(p *Panel) *liveHub {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		binary.BigEndian.PutUint32(raw[:], uint32(time.Now().UnixNano()))
	}
	return &liveHub{
		p:            p,
		boot:         hex8(raw),
		subs:         map[*liveSub]struct{}{},
		tickInterval: liveTickInterval,
		tickets:      newTicketStore(),
	}
}

// hex8 把 4 字节编码成 8 位小写十六进制。
func hex8(b [4]byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 8)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0F]
	}
	return string(out)
}

// reserve 预占一个订阅者名额（升级前调用）。
// 必须把 pending 也算进上限：16 个连接同时握手时，若只数已 attach 的订阅者，
// 它们会同时通过检查、一起挤进第 17 个。
func (h *liveHub) reserve() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs)+h.pending >= liveMaxSubscribers {
		return errTooManySubscribers
	}
	h.pending++
	return nil
}

// release 归还预占名额（升级失败时）。
func (h *liveHub) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending > 0 {
		h.pending--
	}
}

// attach 登记订阅者，并把当前状态作为首个 snapshot 入队（必须让新客户端先拿到
// 全量：增量是相对上一份状态算的，没有基线就无从应用）。
func (h *liveHub) attach(c *wsx.Conn) *liveSub {
	sub := &liveSub{conn: c, send: make(chan []byte, liveQueueSize), done: make(chan struct{})}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending > 0 {
		h.pending--
	}
	if len(h.subs) == 0 {
		// 从"无订阅者"回到"有订阅者"：重算基线。无订阅者期间 ticker 不跑，
		// 旧基线可能已经陈旧很久（尤其是 uptime_sec）；重建一次，比让新客户端
		// 先看到一个陈旧快照、再等 60s 的自愈全量纠正要划算。
		//
		// rev 也必须前进（代码审查 D1）：这一帧 snapshot 携带的是**新**状态，而 rev
		// 的语义是"服务端已广播状态的版本"。若沿用旧 rev，刚重连的客户端（它的
		// liveRev 还停在断开前的值）会把这帧当重放丢掉——无订阅者期间发生的变化就
		// 再也补不上（增量已随基线一起前进），只能干等 60s 的自愈全量，而前端此时
		// 仍显示绿色「实时」徽标。旧 rev 号在这条路径上没有任何用处，递增是零成本。
		h.prev = h.buildState()
		h.rev++
	}
	h.subs[sub] = struct{}{}
	if !h.running {
		h.running = true
		h.ticks.Store(0)
		h.stopCh = make(chan struct{})
		go h.loop(h.stopCh)
	}
	// snapshot 与 subs 登记在同一把锁里完成：保证新订阅者的队列里
	// "snapshot 一定在它可能收到的第一个 patch 之前"。
	sub.enqueue(h.snapshotLocked(time.Now()))
	return sub
}

// detach 注销订阅者；最后一个离开时停掉 ticker（回到零开销）。
func (h *liveHub) detach(s *liveSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	if len(h.subs) == 0 && h.running {
		h.running = false
		close(h.stopCh)
	}
}

// loop 推送循环（仅在有订阅者时存活）。
func (h *liveHub) loop(stop <-chan struct{}) {
	t := time.NewTicker(h.tickInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			h.tick()
		}
	}
}

// tick 一个变化检测周期：采样一次 → 与上一份逐 key 比较 → 只把变化推给订阅者。
func (h *liveHub) tick() {
	n := h.ticks.Add(1)
	st := h.buildState() // 在锁外采样：不能占着 hub 锁做池遍历

	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.prev
	h.prev = st

	var msg []byte
	if prev == nil || n%liveFullSnapshotEvery == 0 {
		// 全量：首个 tick（没有基线）与每 60 个 tick（自愈）各一次。
		h.rev++
		msg = marshalLive(map[string]any{
			"type": "snapshot", "boot": h.boot, "rev": h.rev,
			"at": time.Now().Format(time.RFC3339), "data": json.RawMessage(st.payload),
		})
	} else {
		patch, changed := diffLive(prev, st)
		if !changed {
			return // 这一秒什么都没变：不发帧（这正是相对轮询省下来的部分）
		}
		h.rev++
		patch["type"] = "patch"
		patch["boot"] = h.boot
		patch["rev"] = h.rev
		patch["at"] = time.Now().Format(time.RFC3339)
		msg = marshalLive(patch)
	}
	if len(msg) == 0 {
		return
	}
	for s := range h.subs {
		s.enqueue(msg)
	}
}

// snapshotLocked 构造一条全量 snapshot 消息（用当前基线的载荷）。调用方必须持有 h.mu。
func (h *liveHub) snapshotLocked(now time.Time) []byte {
	data := h.prev.payload
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	return marshalLive(map[string]any{
		"type": "snapshot", "boot": h.boot, "rev": h.rev,
		"at": now.Format(time.RFC3339), "data": data,
	})
}

// marshalLive 序列化一条推送消息；失败返回 nil（调用方跳过，不推半个 JSON）。
func marshalLive(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("panel: live 消息序列化失败: %v", err)
		return nil
	}
	return b
}

// ticksRun 已跑过的 tick 数（测试可见）。
func (h *liveHub) ticksRun() int64 { return h.ticks.Load() }

// subscriberCount 当前订阅者数（测试可见）。
func (h *liveHub) subscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// currentRev 当前状态版本号（测试可见）。
func (h *liveHub) currentRev() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rev
}

// ---------------------------------------------------------------------------
// HTTP 端点
// ---------------------------------------------------------------------------

// wsSameOrigin 判断浏览器上报的 Origin 是否与本次请求同源（Host 相等）。
// 没有 Origin 头时放行：跨站 WebSocket 读取的前提是"由访客浏览器自动发起"，运维脚本 /
// 裸 socket 客户端本来就不带 Origin，拦下来只会误伤；上报了 Origin 却不解析的，判为不同源。
func wsSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// liveTicket 签发一次性实时推送票据（走 withAuth：必须先有 api_key）。
// api_key 为空（未启用鉴权）时也照常签发——前端拿票的动作不需要分叉，
// 升级侧只是忽略票据而已。
func (p *Panel) liveTicket(w http.ResponseWriter, r *http.Request) {
	t := p.hub.tickets.issue()
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":     t,
		"expires_in": int(liveTicketTTL / time.Second),
	})
}

// live 实时推送端点：GET /panel/api/live?ticket=… + WebSocket 升级。
//
// 走裸 withAuth 之外的自定义鉴权（票据），因为浏览器 WebSocket 发不了
// Authorization 头；票据的信任根仍然是同一把 api_key（换票那一步走 withAuth）。
func (p *Panel) live(w http.ResponseWriter, r *http.Request) {
	// 1) 鉴权在 upgrade **之前**完成。顺序很重要：先鉴权再谈握手，未授权者
	//    连"请求头对不对"都不该被回答（也不该占用订阅者名额/连接资源）。
	//    api_key 为空时免票据，与面板其它接口同口径（裸跑本机/私网）。
	if p.apiKey() != "" && !p.hub.tickets.consume(r.URL.Query().Get("ticket")) {
		writeErr(w, http.StatusUnauthorized, "invalid_ticket")
		return
	}

	// 1.5) 跨站读取防护（代码审查 F2）：WebSocket **不受 CORS 约束**，而 panel 的普通
	//      API 受——所以无鉴权部署（api_key 为空，README 允许的私网/本机裸跑）下，
	//      任何站点都能借访客的浏览器连上这条推送，把账号池（uid/昵称/积分/在途…）
	//      一路读走，而 fetch 读不到。故此时要求 Origin 与 Host 同源；带鉴权时票据
	//      本身已证明身份，不再校验 Origin，免得反代改写 Host/Origin 时误伤——
	//      降级路径是 5s 轮询，失败也不会让人看不到数据。
	if p.apiKey() == "" && !wsSameOrigin(r) {
		writeErr(w, http.StatusForbidden, "bad_origin")
		return
	}

	// 2) 握手头部校验。此刻还没 Hijack，失败能干净地按 HTTP 语义回 400。
	//   （wsx.Upgrade 内部会再校验一次——它是公开 API，自带防御不依赖调用方。）
	if err := wsx.CheckHandshake(r); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_handshake")
		return
	}

	// 3) 名额在 Hijack 之前预占：超限时连接还是普通 HTTP 连接，能回完整的 503，
	//    而不是"升级到一半再断"。
	if err := p.hub.reserve(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "too_many_subscribers")
		return
	}

	conn, err := wsx.Upgrade(w, r)
	if err != nil {
		p.hub.release()
		if errors.Is(err, wsx.ErrNoHijack) {
			writeErr(w, http.StatusNotImplemented, "ws_unsupported")
			return
		}
		// 握手校验已过，走到这里只可能是"劫持成功但 101 写失败"——连接已经不在
		// HTTP 语义里，回不了任何响应，只能记日志。
		log.Printf("panel: live 升级失败: %v", err)
		return
	}

	sub := p.hub.attach(conn)
	defer p.hub.detach(sub)
	go sub.writeLoop()
	sub.readLoop() // 阻塞到连接结束
	// 读循环已退出（对端关闭/空闲超时/协议错误）：唤醒写循环发 bye + close(1001)。
	sub.shutdown("conn_closed")
}
