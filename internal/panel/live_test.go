package panel

// live_test.go 账号池实时推送的测试：票据鉴权、hub 变化检测、裸 TCP 端到端集成。
//
// 分工：
//   - 票据与 HTTP 状态码用 httptest.NewRecorder + p.ServeHTTP 直接打（不起服务器，
//     快且确定：不升级的响应根本不需要真连接）；
//   - hub 的变化检测用 net.Pipe 造真连接（net.Pipe 无缓冲，"对端不读"正好是
//     慢客户端的真实语义），并把 ticker 调大以便手动驱动 tick；
//   - 端到端用 httptest.NewServer + 手写裸 TCP WebSocket 客户端（自己算
//     Sec-WebSocket-Accept、自己掩码写 PING），不依赖服务端的写路径。

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/wsx"
)

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// newLiveTestPanel 造一个带真池的面板：apiKey 为空 = 免票据（裸跑口径）。
func newLiveTestPanel(t *testing.T, apiKey string, uids ...string) (*Panel, *pool.Pool) {
	t.Helper()
	pl := pool.New("")
	for _, uid := range uids {
		pl.Add(&auth.Auth{UID: uid, Nickname: "nick-" + uid})
	}
	return New(Config{Version: "test", Pool: pl, APIKey: apiKey}), pl
}

// wsUpgradeHeaders 装上一个合法 WebSocket 升级请求头。
func wsUpgradeHeaders(r *http.Request) {
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")))
}

// liveStatus 用 recorder 直接打一次 /panel/api/live（可选升级头），返回状态码与响应体。
func liveStatus(t *testing.T, p *Panel, query string, upgrade bool) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/live"+query, nil)
	if upgrade {
		wsUpgradeHeaders(req)
	}
	p.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// fetchLiveTicket 走 HTTP 取一张票据。
func fetchLiveTicket(t *testing.T, p *Panel, bearer string) (string, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/live/ticket", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	p.ServeHTTP(rec, req)
	return rec.Body.String(), rec.Code
}

// ---------------------------------------------------------------------------
// 票据
// ---------------------------------------------------------------------------

// 取票端点：必须走 withAuth；签发的票据是 32 字节随机的 base64url（43 字符）、TTL 30s。
func TestLiveTicketIssuedWithTTL(t *testing.T) {
	p, _ := newLiveTestPanel(t, "test-key", "u1")

	if body, code := fetchLiveTicket(t, p, ""); code != http.StatusUnauthorized {
		t.Fatalf("无 key 取票: code=%d body=%s, want 401", code, body)
	}
	body, code := fetchLiveTicket(t, p, "test-key")
	if code != http.StatusOK {
		t.Fatalf("带 key 取票: code=%d body=%s, want 200", code, body)
	}
	var got struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("解析取票响应: %v (%s)", err, body)
	}
	if got.ExpiresIn != 30 {
		t.Errorf("expires_in=%d, want 30", got.ExpiresIn)
	}
	raw, err := base64.RawURLEncoding.DecodeString(got.Ticket)
	if err != nil || len(raw) != 32 {
		t.Errorf("ticket=%q 不是 32 字节 base64url（err=%v, len=%d）", got.Ticket, err, len(raw))
	}
	if n := p.hub.tickets.pending(); n != 1 {
		t.Errorf("未过期票据数=%d, want 1", n)
	}
}

// api_key 为空时取票端点也照常签发（前端拿票动作不需要分叉）。
func TestLiveTicketIssuedWithoutAPIKey(t *testing.T) {
	p, _ := newLiveTestPanel(t, "")
	body, code := fetchLiveTicket(t, p, "")
	if code != http.StatusOK {
		t.Fatalf("免鉴权模式取票: code=%d body=%s, want 200", code, body)
	}
	if !strings.Contains(body, `"ticket":"`) {
		t.Fatalf("响应里没有 ticket: %s", body)
	}
}

// 签发时顺带清扫过期票据：推进假时钟 31s 后再签一张，池里只应剩新的那张。
func TestLiveTicketSweepsExpiredOnIssue(t *testing.T) {
	p, _ := newLiveTestPanel(t, "test-key", "u1")
	base := time.Now()
	p.hub.tickets.setClock(func() time.Time { return base })

	body, code := fetchLiveTicket(t, p, "test-key")
	if code != http.StatusOK {
		t.Fatalf("取票: code=%d", code)
	}
	var first struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal([]byte(body), &first)
	if n := p.hub.tickets.pending(); n != 1 {
		t.Fatalf("签发后 pending=%d, want 1", n)
	}

	// 时钟推到 TTL 之外：旧票据必须先被清扫掉，而不是"过期了还躺在池子里等
	// 被 consume 时才发现"（那样池子会随取票次数无界增长）。
	p.hub.tickets.setClock(func() time.Time { return base.Add(liveTicketTTL + time.Second) })
	if _, code := fetchLiveTicket(t, p, "test-key"); code != http.StatusOK {
		t.Fatalf("第二次取票: code=%d", code)
	}
	if n := p.hub.tickets.pending(); n != 1 {
		t.Fatalf("过期票据未被清扫：pending=%d, want 1", n)
	}
	// 过期票据即便再被拿出来用也必须无效。
	code, resp := liveStatus(t, p, "?ticket="+url.QueryEscape(first.Ticket), true)
	if code != http.StatusUnauthorized {
		t.Fatalf("过期票据: code=%d body=%s, want 401", code, resp)
	}
}

// 无票/错票/过期票/重放：一律 401；且鉴权必须在 upgrade 之前（连升级头都没有也先 401）。
func TestLiveRejectsInvalidTickets(t *testing.T) {
	p, _ := newLiveTestPanel(t, "test-key", "u1")
	base := time.Now()
	p.hub.tickets.setClock(func() time.Time { return base })

	// 1) 无票据：即便不带任何升级头，也必须是 401（证明鉴权先于握手校验）。
	if code, body := liveStatus(t, p, "", false); code != http.StatusUnauthorized || !strings.Contains(body, "invalid_ticket") {
		t.Fatalf("无票据: code=%d body=%s, want 401 invalid_ticket", code, body)
	}
	// 2) 错票据：同样是 401。
	if code, body := liveStatus(t, p, "?ticket=deadbeef", true); code != http.StatusUnauthorized {
		t.Fatalf("错票据: code=%d body=%s, want 401", code, body)
	}

	// 3) 有效票据：鉴权通过后才会因为握手头缺失而 400（状态码本身就是"过了鉴权"的证据）。
	body, _ := fetchLiveTicket(t, p, "test-key")
	var got struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal([]byte(body), &got)
	if code, resp := liveStatus(t, p, "?ticket="+url.QueryEscape(got.Ticket), false); code != http.StatusBadRequest {
		t.Fatalf("有效票据+缺升级头: code=%d body=%s, want 400（说明已过鉴权）", code, resp)
	}

	// 4) 重放：票据是一次性的，上面那次使用（哪怕最终 400）已经把它烧掉了。
	if code, resp := liveStatus(t, p, "?ticket="+url.QueryEscape(got.Ticket), true); code != http.StatusUnauthorized {
		t.Fatalf("重放已用票据: code=%d body=%s, want 401", code, resp)
	}

	// 5) 过期：新票 + 时钟推进超过 TTL。
	body, _ = fetchLiveTicket(t, p, "test-key")
	_ = json.Unmarshal([]byte(body), &got)
	p.hub.tickets.setClock(func() time.Time { return base.Add(liveTicketTTL + time.Second) })
	if code, resp := liveStatus(t, p, "?ticket="+url.QueryEscape(got.Ticket), true); code != http.StatusUnauthorized {
		t.Fatalf("过期票据: code=%d body=%s, want 401", code, resp)
	}
}

// api_key 为空时免票据：同一条真实 TCP 连接序列可以连续升级两次。
func TestLiveUpgradeWithoutTicketTwiceWhenNoAPIKey(t *testing.T) {
	p, _ := newLiveTestPanel(t, "", "u1")
	srv := httptest.NewServer(p)
	defer srv.Close()

	for i := 0; i < 2; i++ {
		c := dialLive(t, srv, "")
		msg, ok := c.readMsg(3 * time.Second)
		if !ok {
			t.Fatalf("第 %d 次升级：没收到首帧 snapshot", i+1)
		}
		if msg["type"] != "snapshot" {
			t.Fatalf("第 %d 次升级：首帧 type=%v, want snapshot", i+1, msg["type"])
		}
		c.close()
	}
}

// ---------------------------------------------------------------------------
// hub：变化检测
// ---------------------------------------------------------------------------

// 无订阅者时 ticker 根本不跑：3 秒内 tick 计数必须还是 0（零开销的硬证据）。
func TestHubDoesNotTickWithoutSubscribers(t *testing.T) {
	p, _ := newLiveTestPanel(t, "", "u1")
	if n := p.hub.ticksRun(); n != 0 {
		t.Fatalf("初始 tick=%d, want 0", n)
	}
	time.Sleep(3 * time.Second)
	if n := p.hub.ticksRun(); n != 0 {
		t.Fatalf("无订阅者 3s 内 tick=%d, want 0（ticker 不该存在）", n)
	}
	if n := p.hub.subscriberCount(); n != 0 {
		t.Fatalf("订阅者数=%d, want 0", n)
	}
}

// 首订阅者必须立刻收到全量 snapshot，且 data 与 /panel/api/overview **同构**
// （逐字段同源，不是"差不多"）。
func TestHubFirstSubscriberGetsOverviewSnapshot(t *testing.T) {
	p, _ := newLiveTestPanel(t, "", "u1", "u2")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	var httpPayload map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &httpPayload); err != nil {
		t.Fatalf("解析 overview: %v", err)
	}

	h := p.hub
	h.tickInterval = time.Hour // 关掉后台 ticker：本次断言只由手动 tick 驱动
	_, peer := attachPipe(t, h)

	msg := peer.next(t, 3*time.Second)
	if msg == nil {
		t.Fatal("首订阅者没收到 snapshot")
	}
	if msg["type"] != "snapshot" {
		t.Fatalf("type=%v, want snapshot", msg["type"])
	}
	boot, _ := msg["boot"].(string)
	if len(boot) != 8 {
		t.Errorf("boot=%q, want 8 位 hex", boot)
	}
	if rev, _ := msg["rev"].(float64); rev != 1 {
		t.Errorf("首个 snapshot 的 rev=%v, want 1", msg["rev"])
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(msg["at"])); err != nil {
		t.Errorf("at=%v 不是 RFC3339: %v", msg["at"], err)
	}

	data, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("data 不是对象: %T", msg["data"])
	}
	// 键集合必须与 HTTP overview 完全一致（多一个少一个都算漂移）。
	for k := range httpPayload {
		if _, ok := data[k]; !ok {
			t.Errorf("snapshot.data 缺少 overview 字段 %q", k)
		}
	}
	for k := range data {
		if _, ok := httpPayload[k]; !ok {
			t.Errorf("snapshot.data 多出 overview 没有的字段 %q", k)
		}
	}
	// accounts 是完整账号数组（含 today），不是 per-uid 分片。
	accts, ok := data["accounts"].([]any)
	if !ok || len(accts) != 2 {
		t.Fatalf("data.accounts = %#v, want 2 个账号", data["accounts"])
	}
	first, _ := accts[0].(map[string]any)
	if first["uid"] == nil || first["today"] == nil {
		t.Fatalf("账号对象缺少 uid/today: %#v", first)
	}
	if _, ok := data["uptime_sec"]; !ok {
		t.Error("全量 snapshot 必须带 uptime_sec（它只在这里出现）")
	}
}

// 只改 uptime_sec 不发帧：这是"排除 uptime_sec"这条设计决策的直接验证。
func TestHubIgnoresUptimeSecChange(t *testing.T) {
	p, _ := newLiveTestPanel(t, "", "u1")
	h := p.hub
	h.tickInterval = time.Hour
	_, peer := attachPipe(t, h)
	if peer.next(t, 3*time.Second) == nil {
		t.Fatal("没收到 snapshot")
	}

	// 手动把基线载荷里的 uptime_sec 改成另一个值——模拟"只跑了时间，什么都没变"。
	// 若 diff 是把整份 overview 拿来比，这一下就会推出一帧（正是要避免的行为）。
	h.mu.Lock()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(h.prev.payload, &m); err != nil {
		h.mu.Unlock()
		t.Fatalf("解析基线载荷: %v", err)
	}
	m["uptime_sec"] = json.RawMessage("999999")
	patched, err := json.Marshal(m)
	if err != nil {
		h.mu.Unlock()
		t.Fatalf("重编码基线载荷: %v", err)
	}
	h.prev.payload = patched
	h.mu.Unlock()

	h.tick()
	peer.assertNoMessage(t, 400*time.Millisecond)
}

// 只有变化的字段进 patch：credits 变 → 只发 credits；其余字段（nickname/uid…）不出现。
func TestHubPatchCarriesOnlyChangedFields(t *testing.T) {
	p, pl := newLiveTestPanel(t, "", "u1", "u2")
	h := p.hub
	h.tickInterval = time.Hour
	_, peer := attachPipe(t, h)
	if peer.next(t, 3*time.Second) == nil {
		t.Fatal("没收到 snapshot")
	}

	// 什么都没变：一个 tick 不该产生任何帧。
	h.tick()
	peer.assertNoMessage(t, 300*time.Millisecond)

	pl.SetCredits("u1", 123, 456)
	h.tick()
	msg := peer.next(t, 3*time.Second)
	if msg == nil {
		t.Fatal("credits 变化后没收到 patch")
	}
	if msg["type"] != "patch" {
		t.Fatalf("type=%v, want patch", msg["type"])
	}
	if rev, _ := msg["rev"].(float64); rev != 2 {
		t.Errorf("patch.rev=%v, want 2（snapshot 之后递增）", msg["rev"])
	}
	accts, ok := msg["accounts"].(map[string]any)
	if !ok {
		t.Fatalf("patch 缺少 accounts: %#v", msg)
	}
	u1, ok := accts["u1"].(map[string]any)
	if !ok {
		t.Fatalf("patch.accounts 缺少 u1: %#v", accts)
	}
	if u1["credits"] != float64(123) {
		t.Errorf("u1.credits=%v, want 123", u1["credits"])
	}
	if _, ok := u1["uid"]; ok {
		t.Error("uid 没变就不该出现在 patch 里（只发变化字段）")
	}
	if _, ok := u1["nickname"]; ok {
		t.Error("nickname 没变就不该出现在 patch 里")
	}
	if _, ok := accts["u2"]; ok {
		t.Error("没变化的 u2 不该出现在 patch.accounts 里")
	}
	// 本地没有全局计数变化（total/healthy 都没动）。
	if _, ok := msg["pool"]; ok {
		t.Errorf("池计数没变却出现在 patch 里: %#v", msg["pool"])
	}
}

// omitempty 字段消失必须下发显式 null，否则前端会把旧值一直显示下去。
// 典型场景：暂停选号后点"恢复选号"（paused: true → 字段消失）。
func TestHubPatchClearsVanishedFieldsWithNull(t *testing.T) {
	p, pl := newLiveTestPanel(t, "", "u1")
	h := p.hub
	h.tickInterval = time.Hour
	_, peer := attachPipe(t, h)
	if peer.next(t, 3*time.Second) == nil {
		t.Fatal("没收到 snapshot")
	}

	pl.Pause("u1")
	h.tick()
	msg := peer.next(t, 3*time.Second)
	if msg == nil {
		t.Fatal("pause 后没收到 patch")
	}
	u1 := msg["accounts"].(map[string]any)["u1"].(map[string]any)
	if u1["paused"] != true {
		t.Fatalf("patch 里 paused=%v, want true", u1["paused"])
	}

	pl.Resume("u1")
	h.tick()
	msg = peer.next(t, 3*time.Second)
	if msg == nil {
		t.Fatal("resume 后没收到 patch")
	}
	u1 = msg["accounts"].(map[string]any)["u1"].(map[string]any)
	v, ok := u1["paused"]
	if !ok {
		t.Fatal("resume 后必须下发 paused（哪怕值为 null），否则前端永远显示「已暂停」")
	}
	if v != nil {
		t.Fatalf("resume 后 paused=%v, want null（字段已消失）", v)
	}
}

// 账号增/删/顺序变：added 带完整对象（含 today）、removed 带 uid、order 只在真的变了时出现。
func TestHubAccountAddedRemovedAndOrder(t *testing.T) {
	p, pl := newLiveTestPanel(t, "", "aaa", "ccc")
	h := p.hub
	h.tickInterval = time.Hour
	_, peer := attachPipe(t, h)
	if peer.next(t, 3*time.Second) == nil {
		t.Fatal("没收到 snapshot")
	}

	// 新增一个排中间的 uid：added + order + pool.total 三处都要出现。
	pl.Add(&auth.Auth{UID: "bbb", Nickname: "新号"})
	h.tick()
	msg := peer.next(t, 3*time.Second)
	if msg == nil {
		t.Fatal("新增账号后没收到 patch")
	}
	added, ok := msg["added"].([]any)
	if !ok || len(added) != 1 {
		t.Fatalf("added = %#v, want 1 个完整账号", msg["added"])
	}
	acct := added[0].(map[string]any)
	if acct["uid"] != "bbb" || acct["today"] == nil {
		t.Fatalf("added 元素必须是完整账号对象（含 today）: %#v", acct)
	}
	order, _ := msg["order"].([]any)
	if len(order) != 3 || order[0] != "aaa" || order[1] != "bbb" || order[2] != "ccc" {
		t.Fatalf("order = %#v, want [aaa bbb ccc]", msg["order"])
	}
	if pc, ok := msg["pool"].(map[string]any); !ok || pc["total"] != float64(3) {
		t.Fatalf("pool = %#v, want total=3", msg["pool"])
	}

	// 删除：removed + order。
	pl.Remove("aaa")
	h.tick()
	msg = peer.next(t, 3*time.Second)
	if msg == nil {
		t.Fatal("删除账号后没收到 patch")
	}
	removed, _ := msg["removed"].([]any)
	if len(removed) != 1 || removed[0] != "aaa" {
		t.Fatalf("removed = %#v, want [aaa]", msg["removed"])
	}
	order, _ = msg["order"].([]any)
	if len(order) != 2 || order[0] != "bbb" || order[1] != "ccc" {
		t.Fatalf("order = %#v, want [bbb ccc]", msg["order"])
	}

	// 只有顺序变（账号集合不变）：order 单独出现。
	h.mu.Lock()
	h.prev.order = []string{"ccc", "bbb"}
	h.mu.Unlock()
	h.tick()
	msg = peer.next(t, 3*time.Second)
	if msg == nil {
		t.Fatal("顺序变化后没收到 patch")
	}
	if _, ok := msg["order"]; !ok {
		t.Fatalf("顺序变化必须带 order: %#v", msg)
	}
	if _, ok := msg["accounts"]; ok {
		t.Errorf("顺序变化不该带 accounts: %#v", msg["accounts"])
	}
}

// model_locks 变化：整体替换（从 null 变成清单、再变回 null）。
func TestHubModelLocksReplacement(t *testing.T) {
	p, pl := newLiveTestPanel(t, "", "u1")
	h := p.hub
	h.tickInterval = time.Hour
	_, peer := attachPipe(t, h)
	if peer.next(t, 3*time.Second) == nil {
		t.Fatal("没收到 snapshot")
	}

	// 用 11102 负缓存造锁（它有配对的清除入口；6004 的冷却只能等上游重置墙钟到期）。
	pl.BlockModelBackoff("u1", "model-x", "11102 model not found")
	h.tick()
	msg := peer.next(t, 3*time.Second)
	if msg == nil {
		t.Fatal("模型锁变化后没收到 patch")
	}
	locks, ok := msg["model_locks"].([]any)
	if !ok || len(locks) != 1 {
		t.Fatalf("model_locks = %#v, want 1 行", msg["model_locks"])
	}
	row := locks[0].(map[string]any)
	if row["model"] != "model-x" || row["locked"] != float64(1) {
		t.Fatalf("model_locks 行内容不对: %#v", row)
	}

	// 解锁：整体替换回 null。
	pl.BlockModelClear("u1", "model-x")
	h.tick()
	msg = peer.next(t, 3*time.Second)
	if msg == nil {
		t.Fatal("解锁后没收到 patch")
	}
	if v, ok := msg["model_locks"]; !ok || v != nil {
		t.Fatalf("解锁后 model_locks = %#v, want 显式 null", msg["model_locks"])
	}
}

// 每 60 个 tick 一次全量 snapshot（自愈/去漂移）：第 60 个 tick 即使什么都没变也要发全量。
func TestHubPeriodicFullSnapshot(t *testing.T) {
	p, _ := newLiveTestPanel(t, "", "u1")
	h := p.hub
	h.tickInterval = time.Hour
	_, peer := attachPipe(t, h)
	if peer.next(t, 3*time.Second) == nil {
		t.Fatal("没收到 snapshot")
	}
	// 手动跑到第 liveFullSnapshotEvery 个 tick（前面 59 个都不该发帧）。
	for i := 1; i < liveFullSnapshotEvery; i++ {
		h.tick()
	}
	peer.assertNoMessage(t, 300*time.Millisecond)
	h.tick() // 第 60 个
	msg := peer.next(t, 3*time.Second)
	if msg == nil {
		t.Fatal("第 60 个 tick 必须发一次全量 snapshot")
	}
	if msg["type"] != "snapshot" {
		t.Fatalf("第 60 个 tick 的 type=%v, want snapshot", msg["type"])
	}
	data, _ := msg["data"].(map[string]any)
	if data == nil || data["accounts"] == nil {
		t.Fatalf("全量 snapshot 缺 data.accounts: %#v", msg["data"])
	}
}

// 慢客户端：队列满即断开（byе + close 1001），且不拖累其他订阅者。
func TestHubDisconnectsSlowSubscriber(t *testing.T) {
	p, _ := newLiveTestPanel(t, "", "u1")
	h := p.hub
	h.tickInterval = time.Hour

	// 快订阅者：正常读。慢订阅者：net.Pipe 对端完全不读（真连接、真阻塞）。
	_, fast := attachPipe(t, h)
	if fast.next(t, 3*time.Second) == nil {
		t.Fatal("快订阅者没收到 snapshot")
	}
	slowConn, slowPeer := net.Pipe()
	defer slowPeer.Close()
	slow := h.attach(wsx.NewConn(slowConn))
	go slow.writeLoop()
	defer h.detach(slow)

	// 一帧一帧地灌：每帧都等快订阅者收完再发下一帧，保证队列被填满的只有慢的那个
	//（快订阅者队列占用始终 ≤1）。第一帧会被慢订阅者的写 goroutine 取走并卡在写超时上，
	// 之后队列填满，再入队就触发断开。
	msg := []byte(`{"type":"patch","rev":9}`)
	for i := 0; i < liveQueueSize+4; i++ {
		h.broadcastForTest(msg)
		if _, ok := fast.readMsg(2 * time.Second); !ok {
			t.Fatalf("第 %d 帧：快订阅者被慢客户端拖累了", i+1)
		}
	}
	select {
	case <-slow.done:
	case <-time.After(2 * time.Second):
		t.Fatal("队列填满后慢订阅者没有被断开")
	}
	if slow.closeReason() != "slow_consumer" {
		t.Errorf("断开原因=%q, want slow_consumer", slow.closeReason())
	}
	// 快订阅者不受影响：还能继续收到帧。
	h.broadcastForTest([]byte(`{"type":"patch","rev":10}`))
	got := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m, ok := fast.readMsg(deadline.Sub(time.Now()))
		if !ok {
			break
		}
		got++
		if m["rev"] == float64(10) {
			return
		}
	}
	t.Fatalf("快订阅者没能收到后续帧（收到 %d 帧）", got)
}

// 第 17 个订阅者被拒（单元口径：名额预留 + 上限）。
func TestHubSubscriberLimitReserve(t *testing.T) {
	p, _ := newLiveTestPanel(t, "", "u1")
	h := p.hub
	for i := 0; i < liveMaxSubscribers; i++ {
		if err := h.reserve(); err != nil {
			t.Fatalf("第 %d 个名额被拒: %v", i+1, err)
		}
	}
	if err := h.reserve(); err != errTooManySubscribers {
		t.Fatalf("第 %d 个名额: err=%v, want errTooManySubscribers", liveMaxSubscribers+1, err)
	}
	h.release()
	if err := h.reserve(); err != nil {
		t.Fatalf("释放一个名额后应当能再占: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 端到端（httptest.NewServer + 手写裸 TCP 客户端）
// ---------------------------------------------------------------------------

// 不支持 Hijack 的 ResponseWriter → 501（连接还是普通 HTTP 连接，能干净地回状态码）。
func TestLiveUpgradeUnsupportedHijack(t *testing.T) {
	p, _ := newLiveTestPanel(t, "", "u1") // 免票据：直接走到升级那一步
	rec := &noHijackWriter{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/panel/api/live", nil)
	wsUpgradeHeaders(req)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d body=%s, want 501", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ws_unsupported") {
		t.Fatalf("响应体=%s, want ws_unsupported", rec.Body.String())
	}
	// 名额必须被归还（否则重试会把 16 个名额耗光）。
	if p.hub.subscriberCount() != 0 || p.hub.pending != 0 {
		t.Fatalf("升级失败后名额没归还: subs=%d pending=%d", p.hub.subscriberCount(), p.hub.pending)
	}
}

// noHijackWriter 只实现 ResponseWriter（刻意不实现 Hijacker），模拟被中间件
// 包装掉 Hijack 的环境。
type noHijackWriter struct {
	*httptest.ResponseRecorder
}

// 端到端主链路：pause / 改余额 → 2s 内收到含该 uid 的 patch；两个客户端都收到同一次变化；
// 连接跨越 11s 依然存活（旧实现是 5s 轮询 + 服务端超时，这里要证明长连接不被掐断）。
func TestLiveIntegrationPatchAndKeepAlive(t *testing.T) {
	p, pl := newLiveTestPanel(t, "live-key", "u1", "u2")
	srv := httptest.NewServer(p)
	defer srv.Close()

	ticket := mustTicket(t, srv, "live-key")
	c1 := dialLive(t, srv, ticket)
	start := time.Now()

	snap := c1.waitMsg(3*time.Second, func(m map[string]any) bool { return m["type"] == "snapshot" })
	if snap == nil {
		t.Fatal("c1 没收到 snapshot")
	}

	// 1) pause → patch（含 uid + paused:true）
	pl.Pause("u1")
	patch := c1.waitMsg(2*time.Second, func(m map[string]any) bool {
		return m["type"] == "patch" && msgAccount(m, "u1") != nil
	})
	if patch == nil {
		t.Fatal("pause 后 2s 内没收到含 u1 的 patch")
	}
	if got := msgAccount(patch, "u1")["paused"]; got != true {
		t.Fatalf("pause patch 缺 paused:true，实际 %#v", msgAccount(patch, "u1"))
	}

	// 2) 改余额 → patch（绝对新值）
	pl.SetCredits("u1", 4242, 9999)
	patch = c1.waitMsg(2*time.Second, func(m map[string]any) bool {
		a := msgAccount(m, "u1")
		return m["type"] == "patch" && a != nil && a["credits"] == float64(4242)
	})
	if patch == nil {
		t.Fatal("SetCredits 后 2s 内没收到 credits=4242 的 patch")
	}

	// 3) 第二个客户端：同一次变化两个客户端都要收到。
	c2 := dialLive(t, srv, mustTicket(t, srv, "live-key"))
	if c2.waitMsg(3*time.Second, func(m map[string]any) bool { return m["type"] == "snapshot" }) == nil {
		t.Fatal("c2 没收到 snapshot")
	}
	pl.SetCredits("u2", 777, 777)
	for i, c := range []*liveClient{c1, c2} {
		got := c.waitMsg(3*time.Second, func(m map[string]any) bool {
			a := msgAccount(m, "u2")
			return m["type"] == "patch" && a != nil && a["credits"] == float64(777)
		})
		if got == nil {
			t.Fatalf("第 %d 个客户端没收到 u2 的余额变化", i+1)
		}
	}

	// 4) 跨过 11s：连接必须还活着（PING → PONG），而不是被某个服务端超时掐断。
	if wait := 11*time.Second - time.Since(start); wait > 0 {
		time.Sleep(wait)
	}
	if !c1.pingPong(2 * time.Second) {
		t.Fatal("连接在 11s 后已死（PING 没换回 PONG）")
	}
	if c1.sawClose {
		t.Fatal("连接在 11s 内收到了 CLOSE/bye")
	}
	c1.close()
	c2.close()
}

// 第 17 个订阅者 → 503（真连接、真上限）。
func TestLiveTooManySubscribersOverHTTP(t *testing.T) {
	p, _ := newLiveTestPanel(t, "", "u1")
	srv := httptest.NewServer(p)
	defer srv.Close()

	clients := make([]*liveClient, 0, liveMaxSubscribers)
	defer func() {
		for _, c := range clients {
			c.close()
		}
	}()
	for i := 0; i < liveMaxSubscribers; i++ {
		c := dialLive(t, srv, "")
		if c.waitMsg(3*time.Second, func(m map[string]any) bool { return m["type"] == "snapshot" }) == nil {
			t.Fatalf("第 %d 个订阅者没收到 snapshot", i+1)
		}
		clients = append(clients, c)
	}
	if n := p.hub.subscriberCount(); n != liveMaxSubscribers {
		t.Fatalf("订阅者数=%d, want %d", n, liveMaxSubscribers)
	}

	// 第 17 个：必须回 503，且不能升级成功。
	status, head, body, conn := dialLiveRaw(t, srv, "")
	if conn != nil {
		defer conn.Close()
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("第 17 个订阅者: status=%d head=%q body=%q, want 503", status, head, body)
	}
	if !strings.Contains(body, "too_many_subscribers") {
		t.Fatalf("第 17 个订阅者的响应体=%q, want too_many_subscribers", body)
	}
}

// ---------------------------------------------------------------------------
// 裸 TCP WebSocket 客户端（测试自备，不复用服务端实现）
// ---------------------------------------------------------------------------

type liveClient struct {
	t        *testing.T
	conn     net.Conn
	sawClose bool
}

func (c *liveClient) close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

// readHead 逐字节读到空行，随后剩下的字节仍留在连接里（不会吞掉第一帧）。
func readHead(t *testing.T, r io.Reader) string {
	t.Helper()
	var sb strings.Builder
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, one); err != nil {
			t.Fatalf("读响应头: %v (已读 %q)", err, sb.String())
		}
		sb.WriteByte(one[0])
		s := sb.String()
		if strings.HasSuffix(s, "\r\n\r\n") || strings.HasSuffix(s, "\n\n") {
			return s
		}
	}
}

// dialLiveRaw 发起一次真实升级请求，返回状态码/响应头/响应体（以及 101 时的连接）。
func dialLiveRaw(t *testing.T, srv *httptest.Server, ticket string) (int, string, string, net.Conn) {
	t.Helper()
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	path := "/panel/api/live"
	if ticket != "" {
		path += "?ticket=" + url.QueryEscape(ticket)
	}
	var keyRaw [16]byte
	_, _ = rand.Read(keyRaw[:])
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(keyRaw[:]) + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		_ = conn.Close()
		t.Fatalf("写升级请求: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	head := readHead(t, conn)
	status := 0
	if _, err := fmt.Sscanf(head, "HTTP/1.1 %d", &status); err != nil {
		_ = conn.Close()
		t.Fatalf("响应状态行不可解析: %q", head)
	}
	if status != http.StatusSwitchingProtocols {
		// 非 101：按 Content-Length 精确读响应体——服务端会保持连接（HTTP/1.1 默认
		// keep-alive），等 EOF 会白等一个读超时。
		body := ""
		if i := strings.Index(head, "\r\n\r\n"); i >= 0 {
			body = head[i+4:]
		}
		if want := contentLength(head); want > len(body) {
			buf := make([]byte, want-len(body))
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := io.ReadFull(conn, buf); err == nil {
				body += string(buf)
			}
		}
		_ = conn.Close()
		return status, head, body, nil
	}
	if !strings.Contains(head, "Sec-WebSocket-Accept: ") {
		_ = conn.Close()
		t.Fatalf("101 响应缺 Sec-WebSocket-Accept: %q", head)
	}
	return status, head, "", conn
}

// contentLength 从响应头里取出 Content-Length（取不到返回 0）。
func contentLength(head string) int {
	for _, line := range strings.Split(head, "\r\n") {
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, _ := strconv.Atoi(strings.TrimSpace(value))
			return n
		}
	}
	return 0
}

// dialLive 发起升级并断言 101。
func dialLive(t *testing.T, srv *httptest.Server, ticket string) *liveClient {
	t.Helper()
	status, head, body, conn := dialLiveRaw(t, srv, ticket)
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("升级失败: status=%d head=%q body=%q", status, head, body)
	}
	return &liveClient{t: t, conn: conn}
}

// writeClientFrame 写一个**带掩码**的客户端帧（自行实现，不借服务端代码）。
func writeClientFrame(conn net.Conn, opcode byte, payload []byte) error {
	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	buf := []byte{0x80 | opcode}
	n := len(payload)
	if n < 126 {
		buf = append(buf, 0x80|byte(n))
	} else {
		buf = append(buf, 0x80|126, byte(n>>8), byte(n))
	}
	buf = append(buf, key[:]...)
	for i, b := range payload {
		buf = append(buf, b^key[i%4])
	}
	_, err := conn.Write(buf)
	return err
}

// readMsg 读下一条文本消息（自动回 PONG 应答服务端 PING）；超时/关闭返回 false。
func (c *liveClient) readMsg(timeout time.Duration) (map[string]any, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if !time.Now().Before(deadline) {
			return nil, false
		}
		_ = c.conn.SetReadDeadline(deadline)
		op, payload, err := wsx.DecodeFrame(c.conn)
		if err != nil {
			return nil, false
		}
		switch op {
		case wsx.OpText:
			var m map[string]any
			if err := json.Unmarshal(payload, &m); err != nil {
				c.t.Fatalf("服务端发来的不是 JSON: %v (%q)", err, payload)
			}
			return m, true
		case wsx.OpPing:
			_ = writeClientFrame(c.conn, wsx.OpPong, payload)
		case wsx.OpClose:
			c.sawClose = true
			return nil, false
		}
	}
}

// waitMsg 一直读，直到出现满足条件的消息（或超时）。
func (c *liveClient) waitMsg(timeout time.Duration, pred func(map[string]any) bool) map[string]any {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		m, ok := c.readMsg(remaining)
		if !ok {
			return nil
		}
		if pred(m) {
			return m
		}
	}
}

// pingPong 发一个客户端 PING，等回 PONG（证明链路双向且没被超时掐断）。
func (c *liveClient) pingPong(timeout time.Duration) bool {
	if err := writeClientFrame(c.conn, wsx.OpPing, []byte("probe")); err != nil {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		if !time.Now().Before(deadline) {
			return false
		}
		_ = c.conn.SetReadDeadline(deadline)
		op, payload, err := wsx.DecodeFrame(c.conn)
		if err != nil {
			return false
		}
		switch op {
		case wsx.OpPong:
			return string(payload) == "probe"
		case wsx.OpText:
			// 中间的 patch 不感兴趣，继续读。
		case wsx.OpPing:
			_ = writeClientFrame(c.conn, wsx.OpPong, payload)
		case wsx.OpClose:
			c.sawClose = true
			return false
		}
	}
}

// mustTicket 通过 HTTP 取一张票据（走真实服务器，顺带验证取票端点的线上形态）。
func mustTicket(t *testing.T, srv *httptest.Server, key string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/panel/api/live/ticket", nil)
	if err != nil {
		t.Fatalf("构造取票请求: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("取票: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("取票: status=%d", resp.StatusCode)
	}
	var got struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("解析取票响应: %v", err)
	}
	if got.Ticket == "" {
		t.Fatal("票据为空")
	}
	return got.Ticket
}

// msgAccount 取出 patch 消息里某个 uid 的变化字段；没有则返回 nil。
func msgAccount(m map[string]any, uid string) map[string]any {
	accts, ok := m["accounts"].(map[string]any)
	if !ok {
		return nil
	}
	a, _ := accts[uid].(map[string]any)
	return a
}

// ---------------------------------------------------------------------------
// net.Pipe 订阅者（hub 单元测试用）
// ---------------------------------------------------------------------------

// pipePeer 是 net.Pipe 的另一端：后台 goroutine 把文本帧投进 msgs。
type pipePeer struct {
	conn net.Conn
	msgs chan []byte
}

// attachPipe 在 hub 上挂一个真连接订阅者（对端由一个 goroutine 持续读帧：
// net.Pipe 无缓冲，没人读的话服务端第一次写就会阻塞在写超时上）。
func attachPipe(t *testing.T, h *liveHub) (*liveSub, *pipePeer) {
	t.Helper()
	a, b := net.Pipe()
	sub := h.attach(wsx.NewConn(a))
	peer := &pipePeer{conn: b, msgs: make(chan []byte, 128)}
	go func() {
		defer close(peer.msgs)
		for {
			op, p, err := wsx.DecodeFrame(b)
			if err != nil {
				return
			}
			if op == wsx.OpText {
				peer.msgs <- append([]byte(nil), p...)
			}
		}
	}()
	go sub.writeLoop()
	t.Cleanup(func() {
		sub.shutdown("test_cleanup")
		h.detach(sub)
		_ = b.Close()
	})
	return sub, peer
}

// readMsg 读一条文本消息。
func (p *pipePeer) readMsg(timeout time.Duration) (map[string]any, bool) {
	select {
	case raw, ok := <-p.msgs:
		if !ok {
			return nil, false
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, false
		}
		return m, true
	case <-time.After(timeout):
		return nil, false
	}
}

// next 读下一条消息；超时返回 nil。
func (p *pipePeer) next(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	m, _ := p.readMsg(timeout)
	return m
}

// assertNoMessage 断言在给定时长内**没有**收到任何消息。
func (p *pipePeer) assertNoMessage(t *testing.T, d time.Duration) {
	t.Helper()
	if m, ok := p.readMsg(d); ok {
		t.Fatalf("本不该收到推送，却收到了: %#v", m)
	}
}

// broadcastForTest 直接给所有订阅者投一帧（慢客户端测试用它代替真 tick：
// 队列策略与 tick 无关，没必要等几十秒攒 tick）。
func (h *liveHub) broadcastForTest(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		s.enqueue(msg)
	}
}
