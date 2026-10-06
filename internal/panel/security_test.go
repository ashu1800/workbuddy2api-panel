package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestPanel() *Panel {
	// 启用鉴权：未带 key 的请求一律 401，不进入依赖 Pool/Upstream 的 handler。
	return New(Config{Version: "test", APIKey: "test-key"})
}

// 面板安全响应头必须覆盖：页面、静态脚本、鉴权失败响应。
func TestSecurityHeadersOnAllPanelResponses(t *testing.T) {
	p := newTestPanel()
	paths := []struct{ method, path string }{
		{"GET", "/panel/"},
		{"GET", "/panel/app.js"},
		{"GET", "/panel/api/overview"},      // 401（未提供 key）
		{"POST", "/panel/api/config"},       // 401
		{"GET", "/panel/api/live"},          // 401（无票据，鉴权在升级之前）
		{"GET", "/panel/api/live/ticket"},   // 401（未提供 key）
		{"GET", "/panel/api/nonexistent"},
	}
	for _, c := range paths {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		h := rec.Header()
		if got := h.Get("Content-Security-Policy"); got == "" {
			t.Errorf("%s %s: missing CSP", c.method, c.path)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options=%q", c.method, c.path, h.Get("X-Content-Type-Options"))
		}
		if h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s %s: X-Frame-Options=%q", c.method, c.path, h.Get("X-Frame-Options"))
		}
		if h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: Referrer-Policy=%q", c.method, c.path, h.Get("Referrer-Policy"))
		}
	}
}

// CSP 必须禁止内联脚本与 iframe 嵌套（严格策略的核心约束）。
func TestCSPDisallowsInlineScriptAndFraming(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	csp := rec.Header().Get("Content-Security-Policy")

	for _, must := range []string{
		"script-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"default-src 'none'",
	} {
		if !strings.Contains(csp, must) {
			t.Errorf("CSP missing %q; got: %s", must, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("CSP must not allow unsafe-inline scripts; got: %s", csp)
	}
}

// 页面必须引用外部脚本（内联脚本会被上面的 CSP 拦掉，页面将完全不可用）。
func TestIndexReferencesExternalScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `<script src="app.js"></script>`) {
		t.Error("index.html must load app.js externally (inline script is blocked by CSP)")
	}
	// 反例保护：出现内联 <script>...</script> 内容块即为回归
	if strings.Contains(body, "<script>\n") || strings.Contains(body, "<script> ") {
		t.Error("index.html still contains an inline <script> block; CSP would block it")
	}
}

// app.js 必须能作为同源脚本取到且类型正确（否则页面白屏）。
func TestAppScriptServed(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type=%q want javascript", ct)
	}
	if !strings.Contains(rec.Body.String(), "'use strict'") {
		t.Error("app.js body looks wrong")
	}
}

// UID 白名单：拒绝路径穿越与异常字符，放行真实 UUID 形态。
func TestValidUID(t *testing.T) {
	ok := []string{
		"248890d9-bb26-4131-87a7-4ec74d472344",
		"abc_123-XYZ",
		"a",
	}
	bad := []string{
		"",
		"../../evil",
		"x/../../y",
		`..\..\evil`,
		"a/b",
		"a\\b",
		"uid with space",
		"uid\nnewline",
		"uid\x00null",
		"café",
		strings.Repeat("a", 65), // 超长
	}
	for _, u := range ok {
		if !validUID(u) {
			t.Errorf("validUID(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if validUID(u) {
			t.Errorf("validUID(%q) = true, want false", u)
		}
	}
}

// 未带密钥的 API 请求必须 401；携带正确密钥则通过鉴权层（不再是 401）。
func TestAuthLayerBehavior(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code=%d want 401", rec.Code)
	}
	// 用不存在的路由验证"带正确 key 已过鉴权"（避免触碰依赖 nil 的 handler）。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/panel/api/nonexistent", nil)
	req2.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusUnauthorized {
		t.Error("valid key must pass the auth layer")
	}
}

// checkSecurityHeaders 断言统一安全响应头齐全（供新增端点复用）。
func checkSecurityHeaders(t *testing.T, h http.Header, label string) {
	t.Helper()
	if got := h.Get("Content-Security-Policy"); got == "" {
		t.Errorf("%s: missing CSP", label)
	}
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options=%q", label, h.Get("X-Content-Type-Options"))
	}
	if h.Get("X-Frame-Options") != "DENY" {
		t.Errorf("%s: X-Frame-Options=%q", label, h.Get("X-Frame-Options"))
	}
	if h.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("%s: Referrer-Policy=%q", label, h.Get("Referrer-Policy"))
	}
}

// 实时推送端点（live.go）：升级端点无票据必须 401 且带全套安全头（浏览器里
// 直接打开 /panel/api/live 看到的是 JSON，同样要吃 CSP）；取票端点走 withAuth，
// 无 key 401、带 key 200 且真的给出票据。
func TestLiveEndpointsAuthAndSecurityHeaders(t *testing.T) {
	p := newTestPanel() // APIKey = "test-key"，Pool 为 nil（401 路径不会碰池）

	// 1) 无票据升级 → 401 invalid_ticket（且不能升级：没有 101 的任何痕迹）。
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/live", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("live 无票据: code=%d want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_ticket") {
		t.Errorf("live 无票据响应体=%q，want invalid_ticket", rec.Body.String())
	}
	if rec.Header().Get("Sec-WebSocket-Accept") != "" {
		t.Error("鉴权失败不该带回任何升级相关响应头")
	}
	checkSecurityHeaders(t, rec.Header(), "GET /panel/api/live")

	// 2) 错票据同样 401（票据是唯一的通行凭证）。
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/live?ticket=nope", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("live 错票据: code=%d want 401", rec.Code)
	}
	checkSecurityHeaders(t, rec.Header(), "GET /panel/api/live?ticket=nope")

	// 3) 取票端点：无 key 401 + 安全头。
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/live/ticket", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("ticket 无 key: code=%d want 401", rec.Code)
	}
	checkSecurityHeaders(t, rec.Header(), "GET /panel/api/live/ticket (401)")

	// 4) 取票端点：带 key 200，且返回票据与 TTL。
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/live/ticket", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ticket 带 key: code=%d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析取票响应: %v", err)
	}
	if got.Ticket == "" {
		t.Error("取票响应没有 ticket")
	}
	if got.ExpiresIn != 30 {
		t.Errorf("expires_in=%d want 30", got.ExpiresIn)
	}
	checkSecurityHeaders(t, rec.Header(), "GET /panel/api/live/ticket (200)")
}
