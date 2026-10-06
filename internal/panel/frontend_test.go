package panel

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// TestAppJSTopLevelSmoke app.js 顶层求值冒烟（v1.11.3/1.11.4 两连炸后补的运行时闸门）：
// node + DOM 桩执行 app.js（含按 hash 落到各视图的 go() 顶层调用），抓 TDZ/
// ReferenceError 类运行时错误——Go 侧 frontend_test 不执行 JS，语法层检查对此全盲。
// 无 node 的环境跳过（CI/精简机不受影响）；harness 与 app.js 同判（app.js 顶层
// start() 的 setInterval 会让 node 事件循环不退出，故成功路径显式 exit(0)）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS smoke skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: process.env.SMOKE_HASH || '#taskscenter' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, URLSearchParams, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	hf, err := os.CreateTemp(t.TempDir(), "smoke-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	for _, hash := range []string{"#taskscenter", "#accounts", "#usage", "#models", "#config", "#logs", "#packages"} {
		cmd := exec.Command(node, hf.Name(), "app.js")
		cmd.Dir = "." // 测试工作目录 = internal/panel
		cmd.Env = append(os.Environ(), "SMOKE_HASH="+hash)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("app.js 顶层求值 %s 崩溃: %v\n%s", hash, err, out)
		}
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js smoke %s 未通过:\n%s", hash, out)
		}
	}
}

// 积分扣除维度的格式必须稳定，且缺样本/缺匹配 Token 时不能伪造比例。
func TestAppJSCreditDimensionFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
	const start = src.indexOf('function trimFixed');
const end = src.indexOf('function usStat');
if (start < 0 || end < 0) throw new Error('credit helpers not found');
const ctx = { Number, String, RegExp };
vm.createContext(ctx);
vm.runInContext(
  src.slice(start, end) +
  '\nthis.fmtCredit=fmtCredit; this.fmtCreditRatio=fmtCreditRatio; this.fmtModelRate=fmtModelRate;',
  ctx
);
process.stdout.write(JSON.stringify({
  credit: ctx.fmtCredit(1.25),
  zero: ctx.fmtCredit(0),
  hundred: ctx.fmtCredit(100),
  ratio: ctx.fmtCreditRatio(12.5, 2, 400),
  noSamples: ctx.fmtCreditRatio(12.5, 0, 400),
  noTokens: ctx.fmtCreditRatio(12.5, 2, 0),
  rate: ctx.fmtModelRate('0.5'),
  noRate: ctx.fmtModelRate(''),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"credit":"1.25","zero":"0","hundred":"100","ratio":"12.5 / 1M","noSamples":"—","noTokens":"—","rate":"x0.5","noRate":"—"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit formatting=%s want %s", out, want)
	}
}

// 账号表「今日用量」的格式契约：单位大写 K/M/B、小数固定两位、且不再拼 "tok" 后缀
// （7.1mtok → 7.10M）。1000 以下保持精确整数，0 就是 "0" 而不是 "0.00"——
// 用量列只显示数字+单位，量纲说明交给悬浮提示。
func TestAppJSFormatTokenCount(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; token count formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function formatTokenCount');
const end = src.indexOf('function formatLatency');
if (start < 0 || end < 0) throw new Error('token count formatter not found');
const ctx = { Number, String, Math };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.fmt=formatTokenCount;', ctx);
process.stdout.write(JSON.stringify({
  example: ctx.fmt(7100000),
  large: ctx.fmt(18700000),
  zero: ctx.fmt(0),
  below1k: ctx.fmt(320),
  kilo: ctx.fmt(1500),
  carry: ctx.fmt(999999),
  billion: ctx.fmt(1234567890),
  negative: ctx.fmt(-1),
  missing: ctx.fmt(null),
  blank: ctx.fmt(''),
}));`
	f, err := os.CreateTemp(t.TempDir(), "token-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("token count formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"example":"7.10M","large":"18.70M","zero":"0","below1k":"320",` +
		`"kilo":"1.50K","carry":"1.00M","billion":"1.23B","negative":"—","missing":"—","blank":"—"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("token count formatting=%s want %s", out, want)
	}

	// 用量单元格里不得再出现 "tok" 小字：格式必须整体由 formatTokenCount 决定。
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"todayTokUnit", "<em>tok</em>"} {
		if strings.Contains(string(js), banned) {
			t.Errorf("app.js 用量列仍带 tok 后缀（发现 %q）", banned)
		}
	}
}

// 模型限流时间必须同时支持上游 reset_at、网关 until 和无重置时间三种形态。
func TestAppJSRateLimitMeta(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; rate limit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function dur(');
const end = src.indexOf('function formatTokenCount');
if (start < 0 || end < 0) throw new Error('rate limit helpers not found');
const ctx = { Date, Number, String, Math };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.rateLimitMeta=rateLimitMeta;', ctx);
const now = new Date(2026, 8, 28, 14, 0, 0).getTime();
const reset = new Date(2026, 8, 28, 16, 0, 0).getTime();
const until = new Date(2026, 8, 28, 15, 0, 0).getTime();
const rate = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit', reset_at: new Date(reset).toISOString(), until: new Date(until).toISOString() }, now);
const unavailable = ctx.rateLimitMeta({ model: 'missing', kind: 'model_unavailable', until: new Date(until).toISOString() }, now);
const unknown = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit' }, now);
process.stdout.write(JSON.stringify({
  rate: rate.detail,
  unavailable: unavailable.detail,
  unknown: unknown.detail,
}));`
	f, err := os.CreateTemp(t.TempDir(), "rate-limit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("rate-limit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"rate":"预计 2026-09-28 16:00 解封（剩余 2时00分） · 网关最快 1时00分 后重试","unavailable":"预计 1时00分 后重试","unknown":"预计解封时间未知"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("rate-limit formatting=%s want %s", out, want)
	}
}

// 请求指标滚动行必须紧凑、可读，并对失败结果使用日志高亮。
func TestAppJSRequestLogFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request log formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const reqStart = src.indexOf('function requestLogText');
const reqEnd = src.indexOf('function fmtBytes');
if ([escStart, escEnd, fmtStart, fmtEnd, reqStart, reqEnd].some(v => v < 0)) throw new Error('request log helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(reqStart, reqEnd) +
  '\nthis.requestLogText=requestLogText; this.requestLogLine=requestLogLine;',
  ctx
);
const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const good = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: '账号(uid8)', duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12, request_id: 'req-1' };
const bad = { ...good, status: 500, outcome: 'http_error', request_id: 'req-2' };
process.stdout.write(JSON.stringify({
  good: ctx.requestLogText(good),
  goodLine: ctx.requestLogLine(good),
  badLine: ctx.requestLogLine(bad),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-log-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request log formatting node test failed: %v\n%s", err, out)
	}
	const text = "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 1.25s | 2.3k tok | 0.12 credit | req-1"
	want := `{"good":` + strconv.Quote(text) +
		`,"goodLine":` + strconv.Quote(`<span class="ln">`+text+`</span>`) +
		`,"badLine":` + strconv.Quote(`<span class="ln e">14:05:06 | 500 HTTP 错误 | glm-5.3 | 账号(uid8) | 1.25s | 2.3k tok | 0.12 credit | req-2</span>`) + `}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request log formatting=%s want %s", out, want)
	}
}

// 同到期时间按面额降序；其余未用完包与零/负余额包分别聚合。
func TestAppJSDetailGroups(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; detail groups test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_DEFAULT_DETAIL_LIMIT');
const end = src.indexOf('function renderPackages');
if (start < 0 || end < 0) throw new Error('detail group functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.pkDetailGroups = pkDetailGroups; this.pkDetailLimit = pkDetailLimit;', ctx);
const input = [
  { id: 'small-late', size: 100, remain: 1, expires_at: 400 },
  { id: 'zero-early-b', size: 200, remain: 0, expires_at: 200 },
  { id: 'small-early', size: 100, remain: 2, expires_at: 200 },
  { id: 'large-unknown', size: 300, remain: 3, end_time: '' },
  { id: 'zero-early-a', size: 200, remain: -1, expires_at: 200 },
  { id: 'small-unknown', size: 100, remain: 1, end_time: '' },
  { id: 'large-early', size: 300, remain: 4, expires_at: 200 },
  { id: 'zero-late', size: 300, remain: 0, expires_at: 300 },
];
const before = input.map(p => p.id).join(',');
const out = ctx.pkDetailGroups(input, 2);
process.stdout.write(JSON.stringify({
  visible: out.visible.map(p => p.id),
  rest: out.rest.map(p => p.id),
  used: out.used.map(p => p.id),
  restSize: out.restSize,
  restRemain: out.restRemain,
  usedSize: out.usedSize,
  defaultLimit: ctx.pkDetailLimit({}),
  configuredLimit: ctx.pkDetailLimit({ panel: { package_detail_limit: 7 } }),
  unchanged: input.map(p => p.id).join(',') === before,
}));`
	f, err := os.CreateTemp(t.TempDir(), "detail-groups-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("detail groups node test failed: %v\n%s", err, out)
	}
	const want = `{"visible":["large-early","small-early"],"rest":["small-late","large-unknown","small-unknown"],"used":["zero-early-b","zero-early-a","zero-late"],"restSize":500,"restRemain":5,"usedSize":700,"defaultLimit":5,"configuredLimit":7,"unchanged":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("detail groups=%s want %s", out, want)
	}
}

// 精确剩余天数聚合、账号内按总余额钳制、无到期批次不进入图表。
func TestAppJSExpirySummary(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry summary test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_ACCOUNT_COLORS');
const end = src.indexOf('function renderExpiryDistribution');
if (start < 0 || end < 0) throw new Error('expiry summary functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.summarizeCreditDays = summarizeCreditDays; this.pkAccountColorMap = pkAccountColorMap;', ctx);
const day = 86400000, now = 100000;
const out = ctx.summarizeCreditDays([
  { uid: 'a', remain: 100, packages: [
    { name: 'soon-a', remain: 30, expires_at: now + day },
    { name: 'later', remain: 70, expires_at: now + 7 * day },
  ] },
  { uid: 'b', remain: 55, packages: [
    { name: 'soon-b', remain: 20, expires_at: now + day },
    { name: 'unknown', remain: 5, end_time: '' },
  ] },
  { uid: 'err', error: 'offline' },
], now);
process.stdout.write(JSON.stringify({
  rows: out.rows.map(row => ({ days: row.days, credits: row.credits })),
  accountCount: out.accountCount,
  unavailable: out.unavailable,
  colorA: ctx.pkAccountColorMap([{ uid: 'b' }, { uid: 'a' }]).get('a'),
  colorB: ctx.pkAccountColorMap([{ uid: 'a' }, { uid: 'b' }]).get('b'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry summary node test failed: %v\n%s", err, out)
	}
	const want = `{"rows":[{"days":1,"credits":50},{"days":7,"credits":70}],"accountCount":3,"unavailable":1,"colorA":"var(--chart-1)","colorB":"var(--chart-2)"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry summary=%s want %s", out, want)
	}
}

// TestIndexLightThemeDefault 面板默认浅色：浅色是主路径，深色为次要模式。
// 根元素硬编码 data-theme 决定首帧外观（CSP 禁内联脚本，无法在 <head> 提前读
// localStorage），若这里回落到 dark，浅色用户每次刷新都会看到一帧深色。
func TestIndexLightThemeDefault(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `<html lang="zh-CN" data-theme="light">`) {
		t.Error(`index.html must default to data-theme="light"`)
	}
}

// TestIndexNoStaticInlineStyle 静态内联样式禁令：颜色/间距/字号等一律走 token 与
// class，只允许注入 CSS 变量（style="--x:…"）表达动态值（进度条宽度、图段比例）。
// 旧版有 9 处内联 style（含弹层宽度、字号、色值），是"同一件事有 N 种写法"的来源。
func TestIndexNoStaticInlineStyle(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()
	for _, m := range regexp.MustCompile(`style="[^"]*"`).FindAllString(body, -1) {
		if !strings.HasPrefix(m, `style="--`) {
			t.Errorf("index.html has a static inline style (use a class or a CSS variable): %s", m)
		}
	}
}

// TestAppJSNoStaticInlineStyle app.js 生成的 HTML 同样只允许变量注入。
func TestAppJSNoStaticInlineStyle(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?s)style="([^"]*)"`).FindAllStringSubmatch(string(raw), -1) {
		if !strings.HasPrefix(m[1], "--") {
			t.Errorf("app.js has a static inline style (use a class or a CSS variable): %s", m[0])
		}
	}
}

// TestNavTitlesAreFourChars 左侧导航标题统一 4 个字：标题长度一致，7 个条目才能
// 在固定宽度的侧栏里左对齐成一条竖线（长短不一会让文字起始位置参差）。新增视图
// 时若标题不是 4 字，这里会失败——请改标题而不是放宽断言。
func TestNavTitlesAreFourChars(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	re := regexp.MustCompile(`(?s)<a href="#[^"]+" data-view="[^"]+"[^>]*>.*?</svg>\s*([^<]+)</a>`)
	items := re.FindAllStringSubmatch(body, -1)
	if len(items) != 7 {
		t.Fatalf("expected 7 nav items, got %d", len(items))
	}
	for _, m := range items {
		label := strings.TrimSpace(m[1])
		if n := len([]rune(label)); n != 4 {
			t.Errorf("nav title %q is %d chars, want exactly 4", label, n)
		}
	}
}

// TestAppJSAvgPerf 首字/推理两列的累计平均换算。后端只暴露原始计数器
// （ttfb_sum_ms / ttfb_count / inference_ms_sum / completion_tokens），
// 除法在前端做——除错不会报错，只会让数字静静偏掉，所以用固定输入钉住。
// 关键口径：推理速度的分子分母必须同一样本集合，否则无 token 的样本会压低速度。
func TestAppJSAvgPerf(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS average check skipped")
	}
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	start := strings.Index(text, "function avgTTFB")
	end := strings.Index(text, "function renderAccounts")
	if start < 0 || end <= start {
		t.Fatal("avgTTFB slice not found in app.js")
	}
	script := text[start:end] + `
const tagOf = o => o.cls + '|' + o.tone + '|' + o.label + '|' + (o.title ? 'T' : '-');
console.log(JSON.stringify({
  ttfb: avgTTFB({ ttfb_sum_ms: 1200, ttfb_count: 3 }),
  ttfbNone: avgTTFB({ ttfb_sum_ms: 0, ttfb_count: 0 }),
  ttfbMissing: avgTTFB({}),
  ttfbNull: avgTTFB(null),
  rate: avgInferenceRate({ inference_tokens_sum: 1000, inference_ms_sum: 2000 }),
  rateNone: avgInferenceRate({ inference_tokens_sum: 1000, inference_ms_sum: 0 }),
  rateMissing: avgInferenceRate({}),
  rateNull: avgInferenceRate(null),
  okRate: okRateOf({ request_count: 1771, ok_count: 1768 }),
  okFull: okRateOf({ request_count: 10, ok_count: 10 }),
  okZero: okRateOf({ request_count: 10, ok_count: 0 }),
  okNoField: okRateOf({ request_count: 10 }),
  okNoAttempts: okRateOf({ request_count: 0, ok_count: 0 }),
  okNull: okRateOf(null),
  todayRate: todayRateOf({ requests: 100, errors: 1 }),
  todayFull: todayRateOf({ requests: 100, errors: 0 }),
  todayAllErr: todayRateOf({ requests: 10, errors: 10 }),
  todayNoTraffic: todayRateOf({ requests: 0, errors: 0 }),
  todayErrExceeds: todayRateOf({ requests: 5, errors: 9 }),
  todayNoErrors: todayRateOf({ requests: 5 }),
  todayNull: todayRateOf(null),
  tagOk: tagOf(statusTagOf({}, '', [])),
  tagOff: tagOf(statusTagOf({ disabled: true }, '', [])),
  tagCool: tagOf(statusTagOf({}, '限流冷却 · 14分02秒', [])),
  tagLimited1: tagOf(statusTagOf({}, '', [{ model: 'm1', kind: 'rate_limit', detail: '预计 X 解封' }])),
  tagLimited2: tagOf(statusTagOf({}, '', [{ model: 'm1', kind: 'rate_limit', detail: 'd1' }, { model: 'm2', kind: 'rate_limit', detail: 'd2' }])),
  tagUnavail: tagOf(statusTagOf({}, '', [{ model: 'm1', kind: 'model_unavailable', detail: '等待重新探测' }])),
  tagMixed: tagOf(statusTagOf({}, '', [{ model: 'm1', kind: 'rate_limit', detail: 'd1' }, { model: 'm2', kind: 'model_unavailable', detail: 'd2' }])),
  tagDisabledWins: tagOf(statusTagOf({ disabled: true }, '', [{ model: 'm1', kind: 'rate_limit', detail: 'd1' }])),
  tagCoolWins: tagOf(statusTagOf({}, '熔断 · 1分', [{ model: 'm1', kind: 'rate_limit', detail: 'd1' }])),
  tagOffReason: tagOf(statusTagOf({ disabled: true, reason: '连续 3 次会话失效' }, '', [])),
  tagRlReason: tagOf(statusTagOf({ reason: 'R' }, '', [{ model: 'm1', kind: 'rate_limit', detail: 'D' }])),
  tagTitle: statusTagOf({}, '', [{ model: 'MODELX', kind: 'rate_limit', detail: 'DETAILY' }]).title.indexOf('MODELX') >= 0
    && statusTagOf({}, '', [{ model: 'MODELX', kind: 'rate_limit', detail: 'DETAILY' }]).title.indexOf('DETAILY') >= 0,
  tagTitleReason: statusTagOf({ disabled: true, reason: 'REASONZ' }, '', []).title.indexOf('REASONZ') >= 0
    && statusTagOf({ reason: 'REASONZ' }, '', [{ model: 'm', kind: 'rate_limit', detail: 'd' }]).title.indexOf('REASONZ') >= 0,
}));`
	f, err := os.CreateTemp(t.TempDir(), "avg-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name()).CombinedOutput()
	if err != nil {
		t.Fatalf("avg perf node check failed: %v\n%s", err, out)
	}
	const want = `{"ttfb":400,"ttfbNone":null,"ttfbMissing":null,"ttfbNull":null,` +
		`"rate":500,"rateNone":null,"rateMissing":null,"rateNull":null,` +
		`"okRate":99.83060417843026,"okFull":100,"okZero":0,"okNoField":null,"okNoAttempts":null,"okNull":null,` +
		`"todayRate":99,"todayFull":100,"todayAllErr":0,"todayNoTraffic":null,` +
		`"todayErrExceeds":0,"todayNoErrors":null,"todayNull":null,` +
		`"tagOk":"|ok|可用|-","tagOff":"off|bad|已禁用|-","tagCool":"cool|warn|限流冷却 · 14分02秒|T",` +
		`"tagLimited1":"rl|warn|限流|T","tagLimited2":"rl|warn|限流 · 2 个模型|T",` +
		`"tagUnavail":"rl|warn|待重探|T","tagMixed":"rl|warn|异常 · 2 个模型|T",` +
		`"tagDisabledWins":"off|bad|已禁用|-","tagCoolWins":"cool|warn|熔断 · 1分|T",` +
		`"tagOffReason":"off|bad|已禁用|T","tagRlReason":"rl|warn|限流|T",` +
		`"tagTitle":true,"tagTitleReason":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("avg perf=%s want %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAccountTableColumnCount 账号表列数必须三处一致：表头 <th>、行内 <td>、
// 空态的 colspan。任何一处不同步，空态或骨架就会整行错位（表格里最显眼、
// 又最容易被后续加列时漏掉的地方）。
func TestAccountTableColumnCount(t *testing.T) {
	const wantCols = 11
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	head := regexp.MustCompile(`(?s)<table class="acc">.*?</thead>`).FindString(body)
	if head == "" {
		t.Fatal("accounts table header not found")
	}
	// `<th[ >]` 排除 <thead
	ths := regexp.MustCompile(`<th[ >]`).FindAllString(head, -1)
	if len(ths) != wantCols {
		t.Errorf("accounts table has %d <th>, want %d", len(ths), wantCols)
	}
	for _, label := range []string{"今日调用", "今日用量", "首字", "推理"} {
		if !strings.Contains(head, ">"+label+"<") {
			t.Errorf("header missing column %q", label)
		}
	}
	if strings.Contains(head, "成功 / 失败") {
		t.Error(`header still has the old "成功 / 失败" column`)
	}

	// 空态 colspan 与表头列数必须一致（在 app.js 里生成）
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`账号池是空的`).FindStringIndex(string(js))
	if m == nil {
		t.Fatal("accounts empty state not found in app.js")
	}
	window := string(js)[maxInt(0, m[0]-160):m[0]]
	cm := regexp.MustCompile(`colspan="(\d+)"`).FindStringSubmatch(window)
	if cm == nil {
		t.Fatal("accounts empty state has no colspan")
	}
	if cm[1] != "11" {
		t.Errorf("accounts empty state colspan=%s, want 11", cm[1])
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestIndexDialogsAccessible 四个弹层必须是可被读屏识别的 dialog，并带标签：
// 重构前它们只是 div + class，键盘用户既看不到焦点也无法用 Esc 退出。
func TestIndexDialogsAccessible(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()
	for _, id := range []string{"taskVeil", "vcVeil", "addVeil", "keyVeil"} {
		marker := regexp.MustCompile(`<div class="veil" id="` + id + `"[^>]*>`).FindString(body)
		if marker == "" {
			t.Errorf("dialog %s not found", id)
			continue
		}
		for _, attr := range []string{`role="dialog"`, `aria-modal="true"`, `aria-labelledby="`} {
			if !strings.Contains(marker, attr) {
				t.Errorf("dialog %s missing %s: %s", id, attr, marker)
			}
		}
	}
}

// TestAppJSTrangeQuery 时间范围控件（trange，本批从上游移植）的预设 → 查询参数映射，
// 以及「今天」的自然日口径：
//   - 「今天」发**浏览器本地时区**的 00:00（服务端时区未必一致），且不带 to
//     ——多发一个 to=now 会把正在走的那个小时桶切掉半截；
//   - 滚动预设（用量页）发 hours，服务端按整点对齐，与旧口径逐位一致；
//   - 「近 N 天」在归档侧（请求记录页）折算成 from：归档是线性日志，没有整点对齐；
//   - 「全部历史」在用量侧必须显式发 hours=0：后端把"没给参数"当作默认 72 小时，
//     什么都不发会让「全部历史」静默退化成「近 3 天」（上游正是在这里漏了）；
//     归档侧没有 hours 口径，什么都不发 = 与移植前的默认展示完全一致。
//
// 时刻与 Date.now 都钉死：自然日口径只差几个小时，漂着测等于没测。
// 旧的 windowHours（today → 当前小时+1 的整点窗口）已随本次移植删除——起点改由
// 浏览器精确给出，不再依赖"面板与服务端同时区"这一隐含前提。
func TestAppJSTrangeQuery(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; trange query check skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const TRANGE_PRESETS');
const end = src.indexOf('function go(v)');
if (start < 0 || end <= start) throw new Error('trange block not found in app.js');
const RealDate = Date;
const FIXED = RealDate.parse('2026-09-30T14:30:00'); // 本地时区 14:30
class FakeDate extends RealDate {
  constructor(...a) { if (a.length) { super(...a); } else { super(FIXED); } }
  static now() { return FIXED; }
}
const host = { innerHTML: '', querySelector: () => null };
const ctx = {
  Date: FakeDate, Number, String, Boolean, Math, Map, Array, Object, JSON, RegExp, Error, isNaN, parseInt, parseFloat,
  URLSearchParams, esc: s => String(s == null ? '' : s),
  $: () => host,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.trangeState=trangeState; this.trangeQuery=trangeQuery; this.trangeLabel=trangeLabel; this.trangeMidnight=trangeMidnight;', ctx);
const q = (preset, rolling) => { ctx.trangeState('t').preset = preset; return ctx.trangeQuery('t', rolling).toString(); };
const custom = (from, to, rolling) => {
  const st = ctx.trangeState('t');
  st.preset = 'custom';
  st.from = from === null ? null : new RealDate(from);
  st.to = to === null ? null : new RealDate(to);
  return ctx.trangeQuery('t', rolling).toString();
};
const m0 = ctx.trangeMidnight();
const label = preset => { ctx.trangeState('t').preset = preset; return ctx.trangeLabel('t'); };
process.stdout.write(JSON.stringify({
  midnightLocal: [m0.getFullYear(), m0.getMonth(), m0.getDate(), m0.getHours(), m0.getMinutes()].join(','),
  today: q('today', true),
  todayLogs: q('today', false),
  roll24: q('24', true),
  roll72: q('72', true),
  roll168: q('168', true),
  roll720: q('720', true),
  allRolling: q('0', true),
  allLogs: q('0', false),
  logs24: q('24', false),
  logs7d: q('168', false),
  customBoth: custom('2026-09-30T09:00:00', '2026-09-30T18:30:00', true),
  customFromOnly: custom('2026-09-30T09:00:00', null, false),
  customToOnly: custom(null, '2026-09-30T18:30:00', false),
  labelToday: label('today'),
  label3d: label('72'),
  labelAll: label('0'),
  labelCustom: (function () { custom('2026-09-30T09:00:00', '2026-09-30T18:30:00', true); return label('custom'); })(),
  labelCustomEmpty: (function () { custom(null, null, true); return label('custom'); })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "trange-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("trange query node check failed: %v\n%s", err, out)
	}
	// 期望值用 Go 的 time.Local 算：node 与 Go 取同一个系统时区。
	local := func(d time.Duration) string {
		return strconv.FormatInt(time.Date(2026, 9, 30, 14, 30, 0, 0, time.Local).Add(d).Unix(), 10)
	}
	sec := func(h, m int) string {
		return strconv.FormatInt(time.Date(2026, 9, 30, h, m, 0, 0, time.Local).Unix(), 10)
	}
	want := `{"midnightLocal":"2026,8,30,0,0",` +
		`"today":"from=` + sec(0, 0) + `",` +
		`"todayLogs":"from=` + sec(0, 0) + `",` +
		`"roll24":"hours=24","roll72":"hours=72","roll168":"hours=168","roll720":"hours=720",` +
		`"allRolling":"hours=0","allLogs":"",` +
		`"logs24":"from=` + local(-24*time.Hour) + `",` +
		`"logs7d":"from=` + local(-168*time.Hour) + `",` +
		`"customBoth":"from=` + sec(9, 0) + `&to=` + sec(18, 30) + `",` +
		`"customFromOnly":"from=` + sec(9, 0) + `",` +
		`"customToOnly":"to=` + sec(18, 30) + `",` +
		`"labelToday":"今天","label3d":"近 3 天","labelAll":"全部历史",` +
		`"labelCustom":"9-30 09:00 → 9-30 18:30","labelCustomEmpty":"自定义"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("trange query=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSTrangeControl 控件本身的渲染契约（node + DOM 桩）：
//   - 七个预设齐全，当前项 selected；非自定义态下起止输入是 hidden（不占位）；
//   - 切到自定义 → 展开起止两个 datetime-local 输入（精度到分钟）；
//   - 起止颠倒 → 两个输入都加 .tr-bad（就地标红）且**不**发查询（把空区间打给
//     服务端只会显示「暂无数据」，看不出是自己填反了）；
//   - 绑定时不触发首次加载（首次加载由 go() 驱动，否则打开页面打两遍接口）。
func TestAppJSTrangeControl(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; trange control check skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const TRANGE_PRESETS');
const end = src.indexOf('function go(v)');
if (start < 0 || end <= start) throw new Error('trange block not found in app.js');
const RealDate = Date;
const FIXED = RealDate.parse('2026-09-30T14:30:00');
class FakeDate extends RealDate {
  constructor(...a) { if (a.length) { super(...a); } else { super(FIXED); } }
  static now() { return FIXED; }
}
// 每个 (宿主, 选择器) 一个稳定 stub：trangeRender 每次重画都会重新 querySelector，
// 同一对象复用才能让测试从"控件自己接上的 onchange"驱动，而不是直接改内部状态。
const qcache = {};
const mkEl = key => {
  const classes = new Set();
  const store = { key, value: '', classes, _onchange: null };
  store.classList = {
    add: c => classes.add(c), remove: c => classes.delete(c),
    toggle: (c, on) => { const want = on === undefined ? !classes.has(c) : !!on; if (want) classes.add(c); else classes.delete(c); return want; },
    contains: c => classes.has(c),
  };
  Object.defineProperty(store, 'onchange', { get: () => store._onchange, set: fn => { store._onchange = fn; } });
  return store;
};
const host = { innerHTML: '', querySelector: sel => (qcache[sel] = qcache[sel] || mkEl(sel)) };
const ctx = {
  Date: FakeDate, Number, String, Boolean, Math, Map, Array, Object, JSON, RegExp, Error, isNaN, parseInt, parseFloat,
  URLSearchParams, esc: s => String(s == null ? '' : s),
  $: () => host,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.trangeState=trangeState; this.trangeQuery=trangeQuery; this.trangeRender=trangeRender; this.trangeBind=trangeBind;', ctx);
let emits = 0;
ctx.trangeBind('t', () => { emits++; });
// innerHTML 是死字符串，真正的 DOM 会把 value 落回 input.value；这里按渲染出的
// HTML 回填一次，后续才能在"用户改过的值"上触发 onchange。
const syncInputs = () => {
  const grab = cls => { const m = new RegExp('class="' + cls + '" value="([^"]*)"').exec(host.innerHTML); return m ? m[1] : ''; };
  qcache['.tr-from'].value = grab('tr-from');
  qcache['.tr-to'].value = grab('tr-to');
};
syncInputs();
const htmlDefault = host.innerHTML;
const emitsOnBind = emits;
const preset = qcache['.tr-preset'];
preset.value = 'custom';
preset.onchange();
syncInputs();
const htmlCustom = host.innerHTML;
const fromEl = qcache['.tr-from'], toEl = qcache['.tr-to'];
const customInit = { from: fromEl.value, to: toEl.value, emits };
fromEl.value = '2026-09-30T18:00';
toEl.value = '2026-09-30T09:00';
fromEl.onchange();
const inverted = { bad: fromEl.classList.contains('tr-bad') && toEl.classList.contains('tr-bad'), emits, query: ctx.trangeQuery('t', true).toString() };
toEl.value = '2026-09-30T19:30';
toEl.onchange();
const fixed = { bad: fromEl.classList.contains('tr-bad') || toEl.classList.contains('tr-bad'), emits, query: ctx.trangeQuery('t', true).toString() };
const presetSel = qcache['.tr-preset'];
presetSel.value = '24';
presetSel.onchange();
const html24 = host.innerHTML;
process.stdout.write(JSON.stringify({ htmlDefault, emitsOnBind, htmlCustom, customInit, inverted, fixed, html24 }));`
	f, err := os.CreateTemp(t.TempDir(), "trange-ctl-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("trange control node check failed: %v\n%s", err, out)
	}
	got := map[string]any{}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("trange control output is not JSON: %v\n%s", err, out)
	}
	str := func(k string) string { s, _ := got[k].(string); return s }
	num := func(k string) float64 { n, _ := got[k].(float64); return n }

	// 默认态：七个预设、当前项 selected（用量页默认「近 3 天」由 TRANGE_DEFAULTS 给）、
	// 自定义输入组 hidden。
	def := str("htmlDefault")
	for _, label := range []string{"今天", "近 24 小时", "近 3 天", "近 7 天", "近 30 天", "全部历史", "自定义…"} {
		if !strings.Contains(def, ">"+label+"</option>") {
			t.Errorf("预设缺少「%s」：%s", label, def)
		}
	}
	if !strings.Contains(def, `<option value="72" selected>近 3 天</option>`) {
		t.Errorf("默认预设不是近 3 天（selected 丢失）：%s", def)
	}
	if !strings.Contains(def, `<span class="tr-custom" hidden>`) {
		t.Errorf("非自定义态下起止输入应 hidden：%s", def)
	}
	if strings.Contains(def, "style=") {
		t.Errorf("控件 HTML 不得带内联样式：%s", def)
	}
	if num("emitsOnBind") != 0 {
		t.Errorf("trangeBind 不应触发首次加载（会打两遍接口），实际触发 %v 次", num("emitsOnBind"))
	}

	// 自定义态：展开起止输入、值 = 今天 00:00 → 现在（本地 14:30），并触发一次查询。
	cus := str("htmlCustom")
	if !strings.Contains(cus, `<span class="tr-custom">`) || strings.Contains(cus, "tr-custom\" hidden") {
		t.Errorf("切到自定义后起止输入未展开：%s", cus)
	}
	if !strings.Contains(cus, `<option value="custom" selected>自定义…</option>`) {
		t.Errorf("自定义项未成为当前项：%s", cus)
	}
	if !strings.Contains(cus, `class="tr-from" value="2026-09-30T00:00"`) ||
		!strings.Contains(cus, `class="tr-to" value="2026-09-30T14:30"`) {
		t.Errorf("自定义初始区间应为「今天 00:00 → 现在」：%s", cus)
	}
	init, _ := got["customInit"].(map[string]any)
	if init == nil || init["from"] != "2026-09-30T00:00" || init["to"] != "2026-09-30T14:30" || init["emits"] != float64(1) {
		t.Errorf("切到自定义后应带默认区间并触发一次查询，实际 %v", got["customInit"])
	}

	// 起止颠倒：就地标红且不发查询。
	inv, _ := got["inverted"].(map[string]any)
	if inv == nil || inv["bad"] != true {
		t.Errorf("起止颠倒时两个输入都应带 tr-bad：%v", got["inverted"])
	}
	if inv != nil && inv["emits"] != float64(1) {
		t.Errorf("起止颠倒不应发起查询（emits 应停在 1），实际 %v", inv["emits"])
	}
	fx, _ := got["fixed"].(map[string]any)
	if fx == nil || fx["bad"] != false || fx["emits"] != float64(2) {
		t.Errorf("改回正序后应清掉 tr-bad 并重新查询：%v", got["fixed"])
	}
	if fx != nil {
		if q, _ := fx["query"].(string); !strings.Contains(q, "from=") || !strings.Contains(q, "to=") {
			t.Errorf("自定义区间的查询串应带 from/to：%v", q)
		}
	}

	// 切回滚动预设：输入组重新收起，且不再有 tr-bad 残留。
	if !strings.Contains(str("html24"), `<span class="tr-custom" hidden>`) ||
		!strings.Contains(str("html24"), `<option value="24" selected>近 24 小时</option>`) {
		t.Errorf("切回滚动预设后应收起自定义输入：%s", str("html24"))
	}
}

// TestAppJSTrangeRequests 端到端：整份 app.js 在 DOM 桩里真实求值（含深链 #usage 的
// 顶层 go()），用假 fetch 记录真正发出的查询串，再通过控件自己的 onchange 逐个切换
// 预设，看 loadUsage / loadLogs 到底请求了什么。
//
// 为什么必须端到端：只求值 trange 那一段时，「onchange 有没有接上加载函数」「查询串
// 有没有真的拼进 URL」「#usNote 有没有回显服务端区间」三件事全都测不到——控件画得
// 再对，没接上线也是死的。同一个沙箱里顺带回归本 fork 的两条红线：账号表今日用量
// 仍是 7.10M 的 chip（不带 tok 后缀）、请求行格式未变。
func TestAppJSTrangeRequests(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; trange request test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const urls = [];
// 假后端：只有显式区间口径才回显 window_from/to（与 internal/usage 的
// SnapshotWindow 一致），滚动窗口不回显——这样 #usNote 的两条分支都能被覆盖。
const body = p => {
  if (p.startsWith('usage')) {
    const d = { totals: {}, by_account: [], by_model: [], by_realm: [], series: [] };
    if (/[?&]from=/.test(p)) { d.window_from = '2026-09-30T00:00:00+08:00'; d.window_to = '2026-09-30T14:30:00+08:00'; }
    return d;
  }
  if (p.startsWith('request_logs')) return { entries: [] };
  // 内存指标里留一条"最近请求"：用来验证时间范围生效时不会回落显示范围外的记录
  // （默认「全部历史」仍走原来的回落逻辑，展示不变）。
  if (p.startsWith('request_metrics')) return { archive: { enabled: true, bytes: 0 }, recent: [{
    time: '2026-09-20T10:00:00.000Z', status: 200, outcome: 'success', model: 'glm-5.3', account: '旧号(uid8)',
    duration_ms: 1000, total_tokens: 100, request_id: 'req-old',
  }] };
  if (p.startsWith('logs')) return { entries: [] };
  return {};
};
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; }, apply() { return inert; }, construct() { return inert; }, has() { return true; },
});
// 时钟钉死：断言落在具体的 from/to 秒数上（否则「今天」会随真实日期漂）。
const RealDate = Date;
const FIXED = RealDate.parse('2026-09-30T14:30:00');
class FakeDate extends RealDate {
  constructor(...a) { if (a.length) { super(...a); } else { super(FIXED); } }
  static now() { return FIXED; }
}
const els = {}, qcache = {};
const mkEl = key => {
  const classes = new Set();
  const store = {
    key, innerHTML: '', textContent: '', value: '', title: '', hidden: false, className: '', disabled: false,
    dataset: {}, style: {}, children: [], selectedOptions: [],
    scrollTop: 0, scrollHeight: 0, clientHeight: 0,
    classList: {
      add: c => classes.add(c), remove: c => classes.delete(c),
      toggle: (c, on) => { const w = on === undefined ? !classes.has(c) : !!on; if (w) classes.add(c); else classes.delete(c); return w; },
      contains: c => classes.has(c),
    },
    classes,
    setAttribute() {}, getAttribute: () => null, removeAttribute() {}, hasAttribute: () => false,
    addEventListener() {}, removeEventListener() {},
    appendChild(n) { store.children.push(n); return n; },
    remove() {}, focus() {}, blur() {}, click() {}, closest: () => null, contains: () => false,
    insertAdjacentHTML() {}, getElementsByTagName: () => [], querySelectorAll: () => [],
    querySelector: sel => (qcache[key + '|' + sel] = qcache[key + '|' + sel] || mkEl(key + '|' + sel)),
    get firstElementChild() { return store.children[0] || null; },
  };
  return new Proxy(store, { get(t, k) { return k in t ? t[k] : inert; }, set(t, k, v) { t[k] = v; return true; }, has: () => true });
};
const el = id => (els[id] = els[id] || mkEl(id));
const sandbox = {
  location: { hash: '#usage' }, // 深链：顶层 go() 会同步调进 loadUsage（TDZ 敏感路径）
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: {
    getElementById: el, querySelectorAll: () => [], querySelector: () => inert, addEventListener() {},
    documentElement: el('documentElement'), head: inert, body: inert, cookie: '',
    createElement: () => mkEl('created'), contains: () => false, activeElement: inert,
  },
  fetch: url => {
    const path = String(url).replace('/panel/api/', '');
    urls.push(path);
    return Promise.resolve({ status: 200, ok: true, json: () => Promise.resolve(body(path)) });
  },
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date: FakeDate, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, URLSearchParams, Symbol, Proxy, Reflect,
};
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src +
  '\nthis.loadUsage=loadUsage; this.loadLogs=loadLogs; this.renderAccounts=renderAccounts; this.renderRequestMetrics=renderRequestMetrics;',
  sandbox, { filename: 'app.js' });

const tick = () => new Promise(r => setTimeout(r, 5));
const usageUrls = () => urls.filter(u => u.startsWith('usage?'));
const reqUrls = () => urls.filter(u => u.startsWith('request_logs?'));
// 走控件自己的 onchange（不是直接改内部状态）：预设有没有真的接上加载函数，
// 只有这条路径能证明。
const setPreset = (id, v) => { const s = el(id).querySelector('.tr-preset'); s.value = v; s.onchange(); };
const setCustom = (id, from, to) => {
  const host = el(id);
  const f = host.querySelector('.tr-from'), t = host.querySelector('.tr-to');
  f.value = from; t.value = to;
  f.onchange();
  return t;
};

(async () => {
  await tick();
  // 深链 #usage 首屏：usage 接口必须恰好被请求一次（trangeBind 不得额外触发回调，
  // 否则打开页面就打两遍接口），且默认预设是「近 3 天」。
  const deepLink = usageUrls()[0] || '';
  const openUsage = usageUrls().length;
  urls.length = 0;
  const out = { deepLink, emitsOnOpen: openUsage };
  const usageQ = async preset => {
    setPreset('usRange', preset);
    await sandbox.loadUsage();
    return usageUrls().slice(-1)[0] || '';
  };
  out.qToday = await usageQ('today');
  out.noteToday = els.usNote.textContent;
  out.q24 = await usageQ('24');
  out.note24 = els.usNote.textContent;
  out.q3d = await usageQ('72');
  out.q7d = await usageQ('168');
  out.q30d = await usageQ('720');
  out.qAll = await usageQ('0');

  // 自定义：展开 → 填起止 → 触发控件 onchange。
  setPreset('usRange', 'custom');
  out.htmlCustom = els.usRange.innerHTML;
  const n0 = urls.length;
  setCustom('usRange', '2026-09-30T09:00', '2026-09-30T18:30');
  await tick();
  out.qCustom = usageUrls().slice(-1)[0] || '';
  out.customFetched = urls.length > n0;

  // 自定义区间起止颠倒：就地标红且不发请求。
  const n1 = urls.length;
  const toEl = setCustom('usRange', '2026-09-30T18:00', '2026-09-30T09:00');
  out.badMarked = els.usRange.querySelector('.tr-from').classList.contains('tr-bad') && toEl.classList.contains('tr-bad');
  await tick();
  out.badFetched = urls.length > n1;

  // 请求记录：默认「全部历史」不发 from/to（与移植前逐字一致），选定区间才带。
  const logQ = async preset => {
    if (preset) setPreset('reqRange', preset);
    await sandbox.loadLogs();
    return reqUrls().slice(-1)[0] || '';
  };
  out.qLogDefault = await logQ(null);
  out.logRowsDefault = els.reqLogBox.innerHTML;
  out.qLogToday = await logQ('today');
  out.logRowsToday = els.reqLogBox.innerHTML;
  out.qLog24 = await logQ('24');
  out.qLogAll = await logQ('0');
  out.logRowsAll = els.reqLogBox.innerHTML;
  out.htmlUsage = els.usRange.innerHTML;
  out.htmlLogs = els.reqRange.innerHTML;

  // 回归红线：账号表今日用量列、请求行格式。
  sandbox.renderAccounts([{
    uid: 'uid-0000000000000001', nickname: '号一', credits: 10, credits_total: 100,
    last_success: '2026-09-28T13:00:00Z',
    today: { day: '2026-09-28', requests: 1771, errors: 3, total_tokens: 7100000 },
    token_usage: { request_count: 1771, ok_count: 1768, total_tokens: 18700000, last_latency_ms: 1500 },
  }]);
  out.accounts = els.accBody.innerHTML;
  sandbox.renderRequestMetrics({ completed: 2, success_rate: 100, http_success_rate: 100, avg_duration_ms: 1250, in_flight: 0,
    archive: { enabled: true, bytes: 20480 } }, [{
    time: new Date(2026, 8, 28, 14, 5, 6).toISOString(), status: 200, outcome: 'success', model: 'glm-5.3',
    account: '账号(uid8)', duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12,
    request_id: 'req-2', client_ip: '203.0.113.7', user_agent: 'curl/8.4.0',
    cache_hit_tokens: 2257, cache_miss_tokens: 43,
  }]);
  out.reqLogBox = els.reqLogBox.innerHTML;
  return out;
})().then(out => {
  process.stdout.write(JSON.stringify(out));
  process.exit(0);
}, e => {
  console.log('TRANGE FAIL: ' + (e && e.stack ? e.stack : e));
  process.exit(1);
});`
	f, err := os.CreateTemp(t.TempDir(), "trange-req-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("trange request node test failed: %v\n%s", err, out)
	}
	got := map[string]any{}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("trange request output is not JSON: %v\n%s", err, out)
	}
	str := func(k string) string { s, _ := got[k].(string); return s }
	boolean := func(k string) bool { b, _ := got[k].(bool); return b }
	num := func(k string) float64 { n, _ := got[k].(float64); return n }
	sec := func(h, m int) string {
		return strconv.FormatInt(time.Date(2026, 9, 30, h, m, 0, 0, time.Local).Unix(), 10)
	}

	// 深链首屏：默认「近 3 天」→ hours=72（不是空查询串，也不是 72 小时以外的口径）。
	if str("deepLink") != "usage?hours=72" {
		t.Errorf("深链 #usage 首屏应请求 usage?hours=72，实际 %q", str("deepLink"))
	}
	if num("emitsOnOpen") != 1 {
		t.Errorf("深链打开用量页应恰好请求一次 usage（trangeBind 不得额外触发），实际 %v 次", num("emitsOnOpen"))
	}

	// 用量页：预设 → 查询串。
	for _, c := range []struct{ key, want string }{
		{"qToday", "usage?from=" + sec(0, 0)},
		{"q24", "usage?hours=24"},
		{"q3d", "usage?hours=72"},
		{"q7d", "usage?hours=168"},
		{"q30d", "usage?hours=720"},
		{"qAll", "usage?hours=0"},
		{"qCustom", "usage?from=" + sec(9, 0) + "&to=" + sec(18, 30)},
	} {
		if str(c.key) != c.want {
			t.Errorf("用量页 %s=%q，期望 %q", c.key, str(c.key), c.want)
		}
	}
	// 「今天」不带 to：多发 to=now 会把正在走的那个小时桶切掉半截。
	if strings.Contains(str("qToday"), "to=") || strings.Contains(str("qToday"), "hours=") {
		t.Errorf("「今天」只应发浏览器本地的 from：%q", str("qToday"))
	}
	if !boolean("customFetched") {
		t.Error("自定义区间填完起止后没有发起请求（控件没接上加载函数）")
	}
	if !boolean("badMarked") {
		t.Error("起止颠倒时未标红 tr-bad")
	}
	if boolean("badFetched") {
		t.Error("起止颠倒不应发起请求（空区间会显示成「暂无数据」，看不出是自己填反了）")
	}

	// 区间回显：显式区间用服务端 window_from/to，滚动窗口回落成控件标签。
	if !strings.HasPrefix(str("noteToday"), "2026-09-30 00:00 → 2026-09-30 14:30 · ") {
		t.Errorf("#usNote 未优先回显服务端区间：%q", str("noteToday"))
	}
	if !strings.HasPrefix(str("note24"), "近 24 小时 · ") {
		t.Errorf("滚动窗口下 #usNote 应用控件标签兜底：%q", str("note24"))
	}

	// 请求记录：默认不发 from/to（= 移植前的 ?limit=100），选定区间才带上，且不发 hours。
	if str("qLogDefault") != "request_logs?limit=100" {
		t.Errorf("请求记录默认应是 request_logs?limit=100，实际 %q", str("qLogDefault"))
	}
	if str("qLogToday") != "request_logs?from="+sec(0, 0)+"&limit=100" {
		t.Errorf("请求记录「今天」应带本地 00:00 的 from：%q", str("qLogToday"))
	}
	if !strings.Contains(str("qLog24"), "from=") || strings.Contains(str("qLog24"), "hours=") {
		t.Errorf("请求记录「近 24 小时」应折算成 from（归档没有 hours 口径）：%q", str("qLog24"))
	}
	if str("qLogAll") != "request_logs?limit=100" {
		t.Errorf("请求记录「全部历史」不应带任何时间参数：%q", str("qLogAll"))
	}
	// 归档在区间内没有记录时不回落内存里的"最近请求"——否则选中区间后屏幕上仍是
	// 范围之外的记录；默认（全部历史）保持原来的回落行为不变。
	if !strings.Contains(str("logRowsDefault"), "req-old") {
		t.Errorf("默认（全部历史）应保持原有的回落行为：%s", str("logRowsDefault"))
	}
	if strings.Contains(str("logRowsToday"), "req-old") {
		t.Errorf("选中区间后显示了区间外的内存记录（筛选形同失效）：%s", str("logRowsToday"))
	}
	if !strings.Contains(str("logRowsToday"), "暂无请求记录") {
		t.Errorf("区间内无记录时应显示空态：%s", str("logRowsToday"))
	}
	if !strings.Contains(str("logRowsAll"), "req-old") {
		t.Errorf("切回「全部历史」应恢复回落行为：%s", str("logRowsAll"))
	}

	// 两个宿主的控件 HTML 都画出来了，且带完整预设。
	for key, html := range map[string]string{"htmlUsage": str("htmlUsage"), "htmlLogs": str("htmlLogs")} {
		if !strings.Contains(html, `class="tr-preset"`) || strings.Count(html, "<option") != 7 {
			t.Errorf("%s 控件未渲染出 7 个预设：%s", key, html)
		}
		if strings.Contains(html, "style=") {
			t.Errorf("%s 控件 HTML 不得带内联样式：%s", key, html)
		}
	}
	if !strings.Contains(str("htmlCustom"), `<span class="tr-custom">`) ||
		!strings.Contains(str("htmlCustom"), `type="datetime-local"`) {
		t.Errorf("自定义态未展开起止输入：%s", str("htmlCustom"))
	}

	// 回归红线：账号表「今日用量」仍是 7.10M（大写单位 + 两位小数），请求行格式不变。
	if !strings.Contains(str("accounts"), `<span class="usage-item usage-total"><b>7.10M</b></span>`) {
		t.Errorf("账号表今日用量不再是 7.10M 的 chip：%s", str("accounts"))
	}
	line := `<span class="ln">14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 1.25s | 2.3k tok | 0.12 credit` +
		` | 命中 98.1%（2.3k tok） | src=203.0.113.7 ua=&quot;curl/8.4.0&quot; | req-2</span>`
	if !strings.Contains(str("reqLogBox"), line) {
		t.Errorf("请求行格式变了：%s", str("reqLogBox"))
	}
}

// TestIndexTimeRangeHosts 时间范围控件的两个宿主必须真的在 index.html 里。app.js 侧
// 是 `if ($('usRange')) trangeBind(...)` 的守卫写法：宿主被删掉时 JS 不报错、测试全绿，
// 只是控件人间蒸发——用量页退回"只有刷新按钮"，日志页重新没有任何时间过滤。
func TestIndexTimeRangeHosts(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`<span class="trange" id="usRange"></span>`,
		`<span class="trange" id="reqRange"></span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html 缺少时间范围控件宿主：%s", want)
		}
	}
	// 旧的窗口下拉必须彻底退场：两套窗口口径并存会互相打架（今天到底按谁算）。
	if strings.Contains(body, `id="usWindow"`) {
		t.Error("index.html 仍有旧的 #usWindow 下拉")
	}
	// 控件样式：整组 inline-flex 且允许换行（窄屏自定义态折行，不挤扁标题）、
	// 起止颠倒要有标红规则，否则只是加了类名而看不出错。
	for _, want := range []string{".trange {", ".trange .tr-custom", ".trange .tr-bad"} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html 缺少 .trange 样式：%s", want)
		}
	}
}

// TestAppJSModelLocks 「模型锁池」表（从上游移植）的渲染契约：
//   - 有锁 → 每个 (域, 模型) 一行：可选/总数、锁定账号数、最早与全池解锁倒计时、
//     限流原因齐全；状态走本 fork 既有的 .tag 体系、域走 .realm-tag，不另起徽标；
//   - 无锁（null / []）→ 「所有模型均可选」空态，而不是只剩表头的空表；
//   - 同一沙箱内顺带回归账号表「今日用量」列：chip 里仍是 7.10M，不带 tok 后缀。
//
// 倒计时是相对量，脚本把 Date.now 钉死，断言才能落在具体文案上（否则
// 「1时00分」会随测试耗时漂成「59分59秒」）。无 node 环境跳过。
func TestAppJSModelLocks(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model locks render test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
// 两段纯函数切片（各自的下界都是下一个函数声明，中间没有顶层副作用语句——
// 整段 [esc … loadOverview) 会带上顶层的 addEventListener，在沙箱里跑不起来）：
//   A = 格式化/转义工具，B = 统计口径 + renderAccounts + renderModelLocks。
const a = src.slice(src.indexOf('function esc('), src.indexOf('function veilStack('));
const b = src.slice(src.indexOf('function avgTTFB('), src.indexOf('async function loadOverview('));
if (!a || !b) throw new Error('render slice not found in app.js');
const code = a + '\n' + b;
const RealDate = Date;
const FIXED = RealDate.parse('2026-09-28T14:00:00Z');
class FakeDate extends RealDate {
  constructor(...a) { if (a.length) { super(...a); } else { super(FIXED); } }
  static now() { return FIXED; }
}
const nodes = {};
const el = id => (nodes[id] = nodes[id] || { innerHTML: '', textContent: '' });
const ctx = {
  Date: FakeDate, Number, String, Boolean, Math, Array, Object, JSON, RegExp, Error, isNaN, parseInt, parseFloat,
  $: el,
};
vm.createContext(ctx);
vm.runInContext(code + '\nthis.renderAccounts = renderAccounts; this.renderModelLocks = renderModelLocks;', ctx);
const at = s => new RealDate(FIXED + s * 1000).toISOString();
ctx.renderModelLocks([
  { model: 'glm-5.3', realm: 'cn', total: 5, servable: 0, locked: 5, state: 'locked',
    unlock_at: at(3600), fully_unlock_at: at(9000), reason: '上游 <429> 额度不足 & 稍后重试' },
  { model: 'gpt-5.2', realm: 'global', total: 3, servable: 2, locked: 1, state: 'partial',
    unlock_at: at(1800), fully_unlock_at: at(1800) },
  { model: 'x-preview', realm: 'cn', total: 2, servable: 0, locked: 1, state: 'starved',
    unlock_at: null, fully_unlock_at: null },
]);
const locks = nodes.mlBody.innerHTML;
const note = nodes.mlNote.textContent;
ctx.renderModelLocks(null);
const emptyNull = nodes.mlBody.innerHTML;
const emptyNote = nodes.mlNote.textContent;
ctx.renderModelLocks([]);
const emptyArr = nodes.mlBody.innerHTML;
ctx.renderAccounts([{
  uid: 'uid-0000000000000001', nickname: '号一', credits: 10, credits_total: 100,
  last_success: '2026-09-28T13:00:00Z',
  today: { day: '2026-09-28', requests: 1771, errors: 3, total_tokens: 7100000 },
  token_usage: { request_count: 1771, ok_count: 1768, total_tokens: 18700000, last_latency_ms: 1500 },
}]);
const accounts = nodes.accBody.innerHTML;
process.stdout.write(JSON.stringify({ locks, note, emptyNull, emptyNote, emptyArr, accounts }));`
	f, err := os.CreateTemp(t.TempDir(), "model-locks-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model locks node render failed: %v\n%s", err, out)
	}
	got := map[string]string{}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("model locks render output is not JSON: %v\n%s", err, out)
	}

	// 有锁：一行一模型，八列缺一不可（上游信息量不缩水）。
	for _, frag := range []string{
		`<td>glm-5.3</td><td><span class="realm-tag">国内版</span></td><td><span class="tag bad">整池不可用</span></td><td class="num">0 <span class="c-muted">/</span> 5</td><td class="num">5</td>`,
		`>1时00分</td>`, // 最早解锁倒计时
		`>2时30分</td>`, // 全池解锁倒计时
		`（1时00分后）`,    // 绝对时刻 + 相对量进 title
		`<td>gpt-5.2</td><td><span class="realm-tag">国际版</span></td><td><span class="tag warn">部分限流</span></td><td class="num">2 <span class="c-muted">/</span> 3</td><td class="num">1</td>`,
		`>30分00秒</td>`,
		`<span class="tag warn">没号可用</span>`,
		`<td class="num" title="无明确解锁时刻">—</td>`, // 无解锁时刻不编造倒计时
		`<td class="reason">上游 &lt;429&gt; 额度不足 &amp; 稍后重试</td>`,
		`<td class="reason"><span class="c-muted">—</span></td>`,
	} {
		if !strings.Contains(got["locks"], frag) {
			t.Errorf("模型锁池缺少片段 %q\n实际：%s", frag, got["locks"])
		}
	}
	if strings.Contains(got["locks"], "<429>") {
		t.Errorf("限流原因未转义：%s", got["locks"])
	}
	if got["note"] != "2 个模型整池不可用" {
		t.Errorf("模型锁池表头摘要=%q want %q", got["note"], "2 个模型整池不可用")
	}

	// 无锁：可读空态，不渲染空表；null 与 [] 表现一致。
	for _, empty := range []string{got["emptyNull"], got["emptyArr"]} {
		if !strings.Contains(empty, `colspan="8"`) || !strings.Contains(empty, "所有模型均可选") {
			t.Errorf("模型锁池空态缺少 colspan/文案：%s", empty)
		}
		if strings.Contains(empty, `<span class="tag`) {
			t.Errorf("模型锁池空态不应渲染数据行：%s", empty)
		}
	}
	if got["emptyNote"] != "" {
		t.Errorf("无锁时表头摘要应为空，实际 %q", got["emptyNote"])
	}
	if got["emptyNull"] != got["emptyArr"] {
		t.Errorf("model_locks 为 null 与 [] 的空态必须一致：\n%s\n%s", got["emptyNull"], got["emptyArr"])
	}

	// 回归：账号表「今日用量」chip 仍是 7.10M（大写单位 + 两位小数），且不带 tok 后缀。
	if !strings.Contains(got["accounts"], `<span class="usage-item usage-total"><b>7.10M</b></span>`) {
		t.Errorf("账号表今日用量不再是 7.10M 的 chip：%s", got["accounts"])
	}
	if strings.Contains(got["accounts"], "Mtok") || strings.Contains(got["accounts"], "<em>tok</em>") {
		t.Errorf("账号表用量列又带上了 tok 后缀：%s", got["accounts"])
	}

	// 空态 colspan 必须与 index.html 里模型锁池表头的列数一致（漏加列时最易错位）。
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	head := regexp.MustCompile(`(?s)<table class="acc ml">.*?</thead>`).FindString(rec.Body.String())
	if head == "" {
		t.Fatal("index.html 缺少模型锁池表（table.acc.ml）")
	}
	if ths := regexp.MustCompile(`<th[ >]`).FindAllString(head, -1); len(ths) != 8 {
		t.Errorf("模型锁池表有 %d 个 <th>，空态 colspan=8", len(ths))
	}
}

// pausedActionResultJS 是 DOM 桩里一次按钮点击的结果：假 fetch 记下的请求、toast 文案、
// 点击后（finally 里 loadOverview 重渲染过）同一行的按钮，以及这次点击弹了几次 confirm。
type pausedActionResultJS struct {
	Calls []struct {
		URL    string `json:"url"`
		Method string `json:"method"`
	} `json:"calls"`
	Toasts       []string `json:"toasts"`
	ButtonAfter  string   `json:"buttonAfter"`
	ConfirmCount int      `json:"confirmCount"`
}

// TestAppJSPausedAccountsControl 上游 paused（暂停选号：退出选号候选，但签到 / 活跃上报 /
// 保活 / 刷新余额照常）在本 fork 面板侧的入口。后端 POST accounts/{uid}/pause|resume 早已
// 随上游合并进来，缺的是「用户点得到」，所以这一批全部是前端契约：
//   - (a) 状态标签四态组合：正常 / paused / disabled / disabled+paused，disabled 优先；
//     paused 用 mute 中性色（运维主动让位，不是故障，别占用告警色），色条复用 tr.cool 的琥珀档
//     （账号表只有 绿在服务 / 琥珀暂不服务 / 红禁用 三档，红的 off 只留给 disabled）；
//     悬浮提示必须写清「照常签到 / 活跃上报 / 保活 / 刷新余额」。
//   - (b) 行内按钮：「解冻 / 禁用」旁并列的暂停选号 / 恢复选号；已禁用账号两者都不给——
//     禁用已含「不参与选号」，并列会让人以为效果能叠加。
//   - (c) disable 的确认文案点名「暂停选号」：不可逆的禁用 vs 可逆的暂停，别让人走错门。
//   - (d) 点击后真的发出 POST accounts/{uid}/pause|resume（假 fetch 记录），且 pause 不弹确认。
//   - (e) 回归：账号表用量列仍是 7.10M 的 chip、行仍是 11 列。
//
// 沙箱与 TestAppJSTopLevelSmoke 同源（真跑整个 app.js），只是把 DOM 换成一堆会记录
// innerHTML / addEventListener 的桩对象，再用桩事件触发 #accBody 的点击处理器。
// 无 node 时跳过（与其余前端测试一致，不阻塞无 Node 的构建机）。
func TestAppJSPausedAccountsControl(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; paused accounts control test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
// DOM 桩：每个 id 一个对象；addEventListener 把处理器留在 _h 上，测试用桩事件直接触发点击。
const nodes = {};
function mkEl(id) {
  return {
    id, innerHTML: '', textContent: '', title: '', value: '', checked: false, disabled: false,
    className: '', hidden: false, dataset: {}, style: {}, children: [], firstElementChild: null,
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    addEventListener(ev, fn) { (this._h || (this._h = {}))[ev] = fn; },
    removeEventListener() {},
    appendChild(c) { this.children.push(c); if (!this.firstElementChild) this.firstElementChild = c; return c; },
    remove() {}, focus() {}, blur() {}, click() {},
    setAttribute() {}, removeAttribute() {}, getAttribute() { return null; },
    querySelector() { return null; }, querySelectorAll() { return []; }, closest() { return null; },
    replaceChildren() { this.children = []; this.firstElementChild = null; },
  };
}
const $ = id => (nodes[id] || (nodes[id] = mkEl(id)));
const calls = [], confirms = [];
const state = { accounts: [], confirmReturn: true };
const tick = () => new Promise(r => setTimeout(r, 0));
function account(uid, extra) {
  return Object.assign({
    uid, nickname: '号 ' + uid, credits: 10, credits_total: 100,
    last_success: '2026-09-28T13:00:00Z',
    today: { day: '2026-09-28', requests: 1771, errors: 3, total_tokens: 7100000 },
    token_usage: { request_count: 1771, ok_count: 1768, total_tokens: 18700000, last_latency_ms: 1500 },
  }, extra || {});
}
state.accounts = [account('uid-normal'), account('uid-paused', { paused: true }),
  account('uid-disabled', { disabled: true }), account('uid-both', { disabled: true, paused: true }),
  account('uid-cooling', { cool_remaining_sec: 300 }),
  account('uid-paused-cool', { paused: true, cool_remaining_sec: 300 })];
const sandbox = {
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp,
  Error, TypeError, isNaN, isFinite, parseInt, parseFloat, encodeURIComponent, decodeURIComponent,
  URL, URLSearchParams, Symbol, Proxy, Reflect,
  setInterval, clearInterval, setTimeout, clearTimeout,
  location: { hash: '#accounts' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  addEventListener() {}, removeEventListener() {},
  confirm: m => { confirms.push(String(m)); return state.confirmReturn; },
  alert() {}, open() {},
  document: {
    documentElement: mkEl('html'), head: mkEl('head'), body: mkEl('body'),
    getElementById: $, querySelector: () => null, querySelectorAll: () => [],
    createElement: () => mkEl('created'), contains: () => false, execCommand: () => true,
  },
  // 假 fetch：记录 URL / 方法，并按 URL 真的改掉池状态——这样 finally 里的 loadOverview
  // 重渲染拿到的是「服务端已生效」的数据，能验证点击后同一行的按钮真的翻面。
  fetch: async (url, opts) => {
    const u = String(url);
    calls.push({ url: u, method: (opts && opts.method) || 'GET' });
    state.accounts.forEach(a => {
      const hit = u.indexOf(encodeURIComponent(a.uid)) >= 0;
      if (hit && u.indexOf('/pause') >= 0) a.paused = true;
      if (hit && u.indexOf('/resume') >= 0) a.paused = false;
      if (hit && u.indexOf('/disable') >= 0) a.disabled = true;
    });
    const body = u.indexOf('overview') >= 0
      ? { total: 4, healthy: 0, cooling: 0, disabled: 2, sticky_sessions: 0, version: '9.9.9',
          uptime_sec: 0, redis_mode: 'local', accounts: state.accounts, model_locks: [] }
      : {};
    return { status: 200, ok: true, json: async () => body };
  },
};
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src + '\nthis.__statusTagOf = statusTagOf;', sandbox, { filename: 'app.js' });
(async () => {
  await tick();
  // rowOf 每次都重新切当前 innerHTML：点击后 finally 里的 loadOverview 会整表重渲染，
  // 缓存的 rows 数组会停在点击前的那一版（按钮「翻面」就验证不出来了）。
  const rowOf = uid => (nodes.accBody.innerHTML.split('<tr class=').slice(1)
    .map(s => '<tr class=' + s).find(r => r.indexOf('title="uid: ' + uid + '"') >= 0) || '');
  const tagOf = o => o.cls + '|' + o.tone + '|' + o.label + '|' + (o.title ? 'T' : '-');
  const at = i => state.accounts[i];
  const out = {
    tags: {
      normal: tagOf(sandbox.__statusTagOf(at(0), '', [])),
      paused: tagOf(sandbox.__statusTagOf(at(1), '', [])),
      disabled: tagOf(sandbox.__statusTagOf(at(2), '', [])),
      both: tagOf(sandbox.__statusTagOf(at(3), '', [])),
    },
    pausedTitle: sandbox.__statusTagOf(at(1), '', []).title,
    bothTitle: sandbox.__statusTagOf(at(3), '', []).title,
    rows: { normal: rowOf('uid-normal'), paused: rowOf('uid-paused'),
            disabled: rowOf('uid-disabled'), both: rowOf('uid-both'),
            cooling: rowOf('uid-cooling'), pausedCool: rowOf('uid-paused-cool') },
    usageTokSuffix: nodes.accBody.innerHTML.indexOf('<em>tok</em>') >= 0,
    pause: null, resume: null, disable: null,
  };
  const click = nodes.accBody._h.click;
  const btn = (a, u) => ({ target: { closest: () => ({ dataset: { a: a, u: u }, disabled: false }) } });
  const acts = { pause: ['pause', 'uid-normal'], resume: ['resume', 'uid-paused'] };
  for (const key of Object.keys(acts)) {
    calls.length = 0; confirms.length = 0;
    await click(btn(acts[key][0], acts[key][1]));
    await tick(); await tick(); // finally 里的 loadOverview 没有被 await：等它把行重渲染出来
    const row = rowOf(acts[key][1]);
    out[key] = {
      calls: calls.slice(), toasts: nodes.toasts.children.map(c => c.textContent), confirmCount: confirms.length,
      buttonAfter: row.indexOf('data-a="resume"') >= 0 ? 'resume' : (row.indexOf('data-a="pause"') >= 0 ? 'pause' : 'none'),
    };
  }
  const disableCalls = () => calls.filter(c => c.url.indexOf('/disable') >= 0).length;
  calls.length = 0; confirms.length = 0;
  state.confirmReturn = false;
  await click(btn('disable', 'uid-normal'));
  await tick();
  const cancelled = disableCalls(), confirmText = confirms[0] || '';
  calls.length = 0; confirms.length = 0;
  state.confirmReturn = true;
  await click(btn('disable', 'uid-normal'));
  await tick();
  out.disable = { confirm: confirmText, cancelled: cancelled, confirmed: disableCalls() };
  process.stdout.write(JSON.stringify(out));
  process.exit(0);
})();
`
	f, err := os.CreateTemp(t.TempDir(), "paused-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cmd := exec.Command(node, f.Name(), "app.js")
	cmd.Dir = "." // 测试工作目录 = internal/panel
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("paused accounts node render failed: %v\n%s", err, out)
	}
	var got struct {
		Tags        map[string]string `json:"tags"`
		PausedTitle string            `json:"pausedTitle"`
		BothTitle   string            `json:"bothTitle"`
		Rows        map[string]string `json:"rows"`
		UsageTok    bool              `json:"usageTokSuffix"`
		Pause       pausedActionResultJS
		Resume      pausedActionResultJS
		Disable     struct {
			Confirm   string `json:"confirm"`
			Cancelled int    `json:"cancelled"`
			Confirmed int    `json:"confirmed"`
		} `json:"disable"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("paused accounts output is not JSON: %v\n%s", err, out)
	}

	// (a) 四种组合的状态标签：disabled 优先于 paused；paused 既不占 bad（故障红）也不占
	// warn（冷却/限流的琥珀），用中性 mute；色条走 tr.cool（琥珀）而不是 off（红）。
	for _, tc := range []struct{ name, want string }{
		{"normal", "|ok|可用|-"},
		{"paused", "cool|mute|已暂停选号|T"},
		{"disabled", "off|bad|已禁用|-"},
		{"both", "off|bad|已禁用|T"}, // disabled+paused：结论取已禁用，暂停只进悬浮提示
	} {
		if got.Tags[tc.name] != tc.want {
			t.Errorf("statusTagOf(%s)=%q want %q", tc.name, got.Tags[tc.name], tc.want)
		}
	}
	for _, want := range []string{"照常签到", "活跃上报", "保活", "刷新余额", "恢复选号"} {
		if !strings.Contains(got.PausedTitle, want) {
			t.Errorf("已暂停选号的悬浮提示缺 %q：%s", want, got.PausedTitle)
		}
	}
	if !strings.Contains(got.BothTitle, "恢复选号") {
		t.Errorf("disabled+paused 的悬浮提示要提醒解冻后仍需恢复选号：%s", got.BothTitle)
	}

	// (b) 行内按钮 + 行 HTML（标签、按钮、色条 class 一起钉）。
	rowWant := map[string][]string{
		"normal": {`<tr class=""`, `<span class="tag ok"`, `>可用</span>`,
			`<button class="xs ghost" data-a="pause" data-u="uid-normal" title="退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额">暂停选号</button>`,
			`<button class="xs ghost" data-a="disable"`},
		"paused": {`<tr class="cool"`, `<span class="tag mute"`, `>已暂停选号</span>`,
			`<button class="xs primary" data-a="resume" data-u="uid-paused">恢复选号</button>`,
			`<button class="xs ghost" data-a="disable"`},
		"disabled": {`<tr class="off"`, `<span class="tag bad"`, `>已禁用</span>`,
			`<button class="xs primary" data-a="revive"`},
		"both": {`<tr class="off"`, `<span class="tag bad"`, `>已禁用</span>`,
			`<button class="xs primary" data-a="revive"`},
	}
	for name, frags := range rowWant {
		if got.Rows[name] == "" {
			t.Fatalf("%s 行没渲染出来", name)
		}
		for _, frag := range frags {
			if !strings.Contains(got.Rows[name], frag) {
				t.Errorf("%s 行缺片段 %q\n实际：%s", name, frag, got.Rows[name])
			}
		}
	}
	// 已禁用账号不得出现暂停/恢复/禁用三者中的任何一个（禁用已含不参与选号，并列即误导）。
	for _, name := range []string{"disabled", "both"} {
		for _, bad := range []string{`data-a="pause"`, `data-a="resume"`, `data-a="disable"`} {
			if strings.Contains(got.Rows[name], bad) {
				t.Errorf("%s 行不该有 %s（已禁用账号不给暂停/禁用入口）\n实际：%s", name, bad, got.Rows[name])
			}
		}
	}
	if strings.Contains(got.Rows["paused"], `data-a="pause"`) {
		t.Errorf("已暂停的行应给「恢复选号」而不是「暂停选号」：%s", got.Rows["paused"])
	}
	if strings.Contains(got.Rows["normal"], `data-a="resume"`) {
		t.Errorf("未暂停的行不该出现「恢复选号」：%s", got.Rows["normal"])
	}
	// 暂停与冷却正交：本 fork 在这里刻意比上游多给一个入口——上游把暂停/恢复挂在
	// frozen（禁用或冷却）的 else 分支里，冷却中的号只能「解冻」，既没法主动让位，
	// 已暂停的号也得等冷却走完才能恢复。这里让两个按钮各管各的状态。
	if !strings.Contains(got.Rows["cooling"], `<tr class="cool"`) ||
		!strings.Contains(got.Rows["cooling"], `data-a="revive"`) ||
		!strings.Contains(got.Rows["cooling"], `data-a="pause"`) {
		t.Errorf("冷却中的号应同时给出「解冻」与「暂停选号」：%s", got.Rows["cooling"])
	}
	if !strings.Contains(got.Rows["pausedCool"], `>已暂停选号</span>`) ||
		!strings.Contains(got.Rows["pausedCool"], `data-a="revive"`) ||
		!strings.Contains(got.Rows["pausedCool"], `data-a="resume"`) {
		t.Errorf("暂停 + 冷却的号：标签取 paused，且「解冻」「恢复选号」都要在：%s", got.Rows["pausedCool"])
	}

	// (e) 回归：用量列仍是 7.10M 的 chip（不带 tok 后缀），行仍是 11 列。
	for name, row := range got.Rows {
		if !strings.Contains(row, `<span class="usage-item usage-total"><b>7.10M</b></span>`) {
			t.Errorf("%s 行今日用量不再是 7.10M 的 chip：%s", name, row)
		}
		if n := strings.Count(row, "<td"); n != 11 {
			t.Errorf("%s 行有 %d 个 <td>，账号表是 11 列", name, n)
		}
	}
	if got.UsageTok {
		t.Error("账号表用量列又带上了 tok 后缀")
	}

	// (d) 点击后真的发出请求：URL + 方法都要对，且重渲染后同一行的按钮翻面。
	for _, tc := range []struct {
		name   string
		res    pausedActionResultJS
		url    string
		button string
		toast  string
	}{
		{"pause", got.Pause, "/panel/api/accounts/uid-normal/pause", "resume", "已暂停选号"},
		{"resume", got.Resume, "/panel/api/accounts/uid-paused/resume", "pause", "已恢复选号"},
	} {
		if len(tc.res.Calls) == 0 {
			t.Fatalf("%s 点击没有发出任何请求", tc.name)
		}
		if tc.res.Calls[0].URL != tc.url || tc.res.Calls[0].Method != "POST" {
			t.Errorf("%s 点击发出 %s %s，want POST %s", tc.name, tc.res.Calls[0].Method, tc.res.Calls[0].URL, tc.url)
		}
		if tc.res.ButtonAfter != tc.button {
			t.Errorf("%s 后同一行按钮变成 %q，want %q", tc.name, tc.res.ButtonAfter, tc.button)
		}
		if n := len(tc.res.Toasts); n == 0 || !strings.Contains(tc.res.Toasts[n-1], tc.toast) {
			t.Errorf("%s 的 toast 文案缺 %q：%v", tc.name, tc.toast, tc.res.Toasts)
		}
	}
	// pause 不弹确认（可逆、无损、保号任务照常）：与 disable 的差别就在这里。
	if got.Pause.ConfirmCount != 0 {
		t.Errorf("pause 不该弹 confirm（可逆无损，上游也不弹），实际弹了 %d 次", got.Pause.ConfirmCount)
	}

	// (c) disable 的 confirm 文案必须把「暂停选号」这条更轻的路指出来；取消时不得发请求。
	if !strings.Contains(got.Disable.Confirm, "暂停选号") {
		t.Errorf("disable 的确认文案没提到「暂停选号」：%s", got.Disable.Confirm)
	}
	if got.Disable.Cancelled != 0 {
		t.Errorf("confirm 取消后仍发出了 %d 次 disable 请求", got.Disable.Cancelled)
	}
	if got.Disable.Confirmed != 1 {
		t.Errorf("confirm 确认后 disable 请求数=%d，want 1", got.Disable.Confirmed)
	}

	// 色条不是随便挑的 class：账号表只有三档（index.html 的 tr.cool / tr.rl / tr.off），
	// 把「暂停 → tr.cool → 琥珀条」这条链的另一端（CSS）也钉住。
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		`.acc tbody tr.cool .mark i { background: var(--warn-mark); }`,
		`.acc tbody tr.off .mark i { background: var(--bad-mark); }`,
	} {
		if !strings.Contains(string(html), rule) {
			t.Errorf("index.html 缺少色条规则 %q（行 class 是色条的唯一来源）", rule)
		}
	}
	// 配置页 include_disabled_in_tasks 的说明要指向账号行的「暂停选号」按钮：那个开关是
	// 全局的（且面向「确实要禁用」的场景），单号临时让位不该为了它改全局配置。
	if !strings.Contains(string(html), `账号行的「<b>暂停选号</b>」按钮`) {
		t.Error("include_disabled_in_tasks 的说明没指向账号行的「暂停选号」按钮")
	}
}

// TestAppJSRequestLogSourceAndCache 请求行的「调用来源 + 缓存命中率」两段增量（本批从
// 上游移植，形态适配本 fork 的行式日志）：
//   - 来源段与 stdout 流水行同款（`src=IP ua="客户端标签"`），且**只有采集到才追加**：
//     两者皆空时整段不出现，功能上线前写入的老归档行保持原格式（不凭空多出占位）；
//     只缺一侧时该侧写 `-`，不拿别的字段顶替。
//   - UA 走后端 internal/logfmt.ShortUA 的等价实现：跳过 Mozilla/AppleWebKit 这类渲染
//     引擎 token 取 Chrome/120.0.0.0；没有 name/version 就回落整串；一律截到 40 字节
//     （UA 是客户端可控自由文本，不设上限会把整行挤爆）。
//   - 命中率只在有观测（命中+未命中 > 0）时出现，并带命中 token 绝对值——只有百分比
//     区分不出「2k 命中 98%」和「2M 命中 98%」。
func TestAppJSRequestLogSourceAndCache(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request source formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const reqStart = src.indexOf('function requestLogText');
const reqEnd = src.indexOf('function fmtBytes');
if ([escStart, escEnd, fmtStart, fmtEnd, reqStart, reqEnd].some(v => v < 0)) throw new Error('request log helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(reqStart, reqEnd) +
  '\nthis.requestLogText=requestLogText; this.requestLogLine=requestLogLine; this.shortUA=shortUA;',
  ctx
);
const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const base = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: '账号(uid8)', duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12 };
const plain = { ...base, request_id: 'req-1' };
const full = { ...base, request_id: 'req-2', client_ip: '203.0.113.7', cache_hit_tokens: 2257, cache_miss_tokens: 43,
  user_agent: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36' };
const missOnly = { ...base, request_id: 'req-3', user_agent: 'curl/8.4.0', cache_hit_tokens: 0, cache_miss_tokens: 2000 };
const ipOnly = { ...base, request_id: 'req-4', client_ip: '198.51.100.9' };
const productOnly = { ...base, request_id: 'req-5', user_agent: 'node' };
const enginesOnly = { ...base, request_id: 'req-6', user_agent: 'Mozilla/5.0 AppleWebKit/537.36' };
const longUA = { ...base, request_id: 'req-7', user_agent: 'VeryLongClientNameThatKeepsGoingAndGoing/1.2.3' };
const cjkUA = { ...base, request_id: 'req-8', user_agent: 'Mozilla/5.0 中文客户端名称特别长特别长特别长特别长/1.2.3' };
const htmlUA = { ...base, request_id: 'req-9', client_ip: '<b>10.0.0.1</b>', user_agent: 'Mozilla/5.0 Chrome/1.0<b>' };
process.stdout.write(JSON.stringify({
  plain: ctx.requestLogText(plain),
  fullLine: ctx.requestLogLine(full),
  fullUA: ctx.shortUA(full.user_agent),
  missOnly: ctx.requestLogText(missOnly),
  ipOnly: ctx.requestLogText(ipOnly),
  productOnly: ctx.requestLogText(productOnly),
  enginesOnly: ctx.requestLogText(enginesOnly),
  longUA: ctx.requestLogText(longUA),
  cjkUA: ctx.requestLogText(cjkUA),
  htmlUA: ctx.requestLogLine(htmlUA),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-source-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request source formatting node test failed: %v\n%s", err, out)
	}
	got := map[string]string{}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("request source formatting output is not JSON: %v\n%s", err, out)
	}
	const head = "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 1.25s | 2.3k tok | 0.12 credit"
	for _, tc := range []struct{ key, want string }{
		// 无来源无命中观测：与旧格式逐字节一致（本批是纯增量）。
		{"plain", head + " | req-1"},
		{"fullUA", "Chrome/120.0.0.0"},
		{"fullLine", `<span class="ln">` + head + ` | 命中 98.1%（2.3k tok） | src=203.0.113.7 ua=&quot;Chrome/120.0.0.0&quot; | req-2</span>`},
		// 命中 0 但确有观测（整段未命中）：显示 0%，而不是被当成「没样本」吞掉。
		{"missOnly", head + ` | 命中 0%（0 tok） | src=- ua="curl/8.4.0" | req-3`},
		{"ipOnly", head + " | src=198.51.100.9 ua=- | req-4"},
		{"productOnly", head + ` | src=- ua="node" | req-5`},
		{"enginesOnly", head + ` | src=- ua="Mozilla/5.0 AppleWebKit/537.36" | req-6`},
		{"longUA", head + ` | src=- ua="VeryLongClientNameThatKeepsGoingAndGoing" | req-7`},
		// 中文 UA 按 UTF-8 字节截断（对齐后端 logfmt.Truncate 的 rune 边界回退）：
		// 13 个汉字 = 39 字节，第 14 个会越过 40 字节上限，整个让出（不切出半个字符）。
		{"cjkUA", head + ` | src=- ua="中文客户端名称特别长特别长" | req-8`},
		// 来源与正文同走 esc：UA 是客户端可控文本，不能让它闭合属性/注入标签。
		{"htmlUA", `<span class="ln">` + head + ` | src=&lt;b&gt;10.0.0.1&lt;/b&gt; ua=&quot;Chrome/1.0&lt;b&gt;&quot; | req-9</span>`},
	} {
		if got[tc.key] != tc.want {
			t.Errorf("%s=%q\nwant %q", tc.key, got[tc.key], tc.want)
		}
	}
}

// TestAppJSCacheRateFormatting 缓存命中率三种呈现（统计卡 / 明细表单元格 / 请求行）必须
// 同一口径：命中率 = 命中 ÷（命中 + 未命中），无样本回 '—'——显示成 0% 会被读成「缓存
// 完全失效」，那是误导。分档 ≥90% 绿 / 80–90% 琥珀 / <80% 红，全部走 index.html 既有
// 语义色类（.stat.good|warn|bad、.c-ok|warn|bad）：上游用内联 style 上色，本 fork 的
// TestAppJSNoStaticInlineStyle 禁止这种写法，故改为类名映射。
func TestAppJSCacheRateFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cache rate formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const usStart = src.indexOf('function fmtTok(');
const usEnd = src.indexOf('function parsePointTime');
const reqStart = src.indexOf('function requestLogText');
const reqEnd = src.indexOf('function fmtBytes');
if ([escStart, escEnd, usStart, usEnd, reqStart, reqEnd].some(v => v < 0)) throw new Error('cache rate helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(usStart, usEnd) + src.slice(reqStart, reqEnd) +
  '\nthis.cacheRateText=cacheRateText; this.cacheRateCell=cacheRateCell; this.cacheRateStat=cacheRateStat;',
  ctx
);
process.stdout.write(JSON.stringify({
  text: ctx.cacheRateText(2257, 43),
  noSamples: ctx.cacheRateText(0, 0),
  missing: ctx.cacheRateText(undefined, null),
  allMiss: ctx.cacheRateText(0, 2000),
  allHit: ctx.cacheRateText(100, 0),
  boundary90: ctx.cacheRateText(900, 100),
  boundary80: ctx.cacheRateText(80, 20),
  rounded: ctx.cacheRateText(1, 2),
  cellNone: ctx.cacheRateCell(0, 0),
  cellOk: ctx.cacheRateCell(95, 5),
  cellWarn: ctx.cacheRateCell(85, 15),
  cellBad: ctx.cacheRateCell(50, 50),
  cellBig: ctx.cacheRateCell(2257, 43),
  statOk: ctx.cacheRateStat(95, 5),
  statNone: ctx.cacheRateStat(0, 0),
}));`
	f, err := os.CreateTemp(t.TempDir(), "cache-rate-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("cache rate formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"text":"98.1%","noSamples":"—","missing":"—","allMiss":"0%","allHit":"100%",` +
		`"boundary90":"90%","boundary80":"80%","rounded":"33.3%",` +
		`"cellNone":"<span class=\"c-muted\">—</span>",` +
		`"cellOk":"<span class=\"c-ok\" title=\"命中 95 / 未命中 5 tok\">95%</span>",` +
		`"cellWarn":"<span class=\"c-warn\" title=\"命中 85 / 未命中 15 tok\">85%</span>",` +
		`"cellBad":"<span class=\"c-bad\" title=\"命中 50 / 未命中 50 tok\">50%</span>",` +
		`"cellBig":"<span class=\"c-ok\" title=\"命中 2.3k / 未命中 43 tok\">98.1%</span>",` +
		`"statOk":"<div class=\"stat good\"><div class=\"v\">95%</div><div class=\"k\">缓存命中率</div></div>",` +
		`"statNone":"<div class=\"stat \"><div class=\"v\">—</div><div class=\"k\">缓存命中率</div></div>"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("cache rate formatting=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSUsageCacheRender node + DOM 桩真实渲染一次（沙箱写法同 TestAppJSTopLevelSmoke）：
//   - 请求记录行：有来源与无来源两组，分别渲染成 .ln 行；
//   - 用量页：总览统计卡（含新增的缓存命中率卡）、按账号/按模型/按域三张明细表（含新增
//     的命中率列与空态 colspan）；
//   - 回归红线：账号表「今日用量」仍是 7.10M（大写单位 + 两位小数，无 tok 后缀）。
//
// 列位置也一起钉住：命中率列插在「合计」与「均延迟」之间，三张表同序——只断言列数
// 发现不了「列数对但插错位置」。
func TestAppJSUsageCacheRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; usage cache render test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
// 四段纯声明切片（各自下界都是下一个函数声明，中间没有顶层副作用语句）：
//   A = 转义/格式化工具，B = 统计口径 + renderAccounts，C = 用量页渲染，
//   D = 请求记录渲染（renderRequestMetrics + requestLogText + 缓存/来源 helper）。
const a = src.slice(src.indexOf('function esc('), src.indexOf('function veilStack('));
const b = src.slice(src.indexOf('function avgTTFB('), src.indexOf('async function loadOverview('));
const c = src.slice(src.indexOf('function fmtTok('), src.indexOf('function parsePointTime'));
const d = src.slice(src.indexOf('function renderRequestMetrics'), src.indexOf("$('btnLogPin')"));
if (!a || !b || !c || !d) throw new Error('render slice not found in app.js');
const nodes = {};
const el = id => (nodes[id] = nodes[id] || { innerHTML: '', textContent: '', selectedOptions: [] });
const ctx = {
  Date, Number, String, Boolean, Math, Array, Object, JSON, RegExp, Error, isNaN, parseInt, parseFloat,
  $: el,
  renderUsageChart: () => { el('usChart').innerHTML = 'CHART'; },
  // renderUsage 把窗口标签写进 #usNote（本段只关心卡片/表格，标签另有 trange 测试钉住）。
  trangeLabel: () => '近 3 天',
};
vm.createContext(ctx);
vm.runInContext(a + '\n' + b + '\n' + c + '\n' + d +
  '\nthis.renderRequestMetrics = renderRequestMetrics;' +
  '\nthis.renderUsage = renderUsage;' +
  '\nthis.renderAccounts = renderAccounts;\n', ctx);

const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const base = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: '账号(uid8)', duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12 };
const plain = { ...base, request_id: 'req-1' };
const full = { ...base, request_id: 'req-2', client_ip: '203.0.113.7', user_agent: 'curl/8.4.0', cache_hit_tokens: 2257, cache_miss_tokens: 43 };
ctx.renderRequestMetrics({ completed: 2, success_rate: 100, http_success_rate: 100, avg_duration_ms: 1250, in_flight: 0,
  archive: { enabled: true, bytes: 20480 } }, [plain, full]);
const reqLogBox = nodes.reqLogBox.innerHTML;
const reqNoteWithSource = nodes.reqNote.textContent;
// 开关关闭 / 旧归档：整页无来源时明确点名，而不是让人以为解析坏了。
ctx.renderRequestMetrics({ completed: 1, archive: { enabled: true, bytes: 0 } }, [plain]);
const reqNoteNoSource = nodes.reqNote.textContent;

ctx.renderUsage({
  totals: { requests: 1200, total_tokens: 1200000, prompt_tokens: 900000, completion_tokens: 300000, errors: 0,
    avg_latency_ms: 1300, cache_hit_tokens: 2257, cache_miss_tokens: 43 },
  by_account: [{ key: 'uid-0000000000000001', extra: '号一', realm: 'cn', requests: 12, errors: 0,
    prompt_tokens: 100, completion_tokens: 20, total_tokens: 120, avg_latency_ms: 900, avg_tokens_per_second: 30,
    cache_hit_tokens: 95, cache_miss_tokens: 5 }],
  by_model: [{ key: 'glm-5.3', requests: 3, errors: 1, prompt_tokens: 10, completion_tokens: 2, total_tokens: 12,
    cache_hit_tokens: 0, cache_miss_tokens: 100 }],
  by_realm: [{ key: 'cn', requests: 3, prompt_tokens: 10, completion_tokens: 2, total_tokens: 12 }],
  credit_by_account: [], credit_by_model: [], series: [],
});
const usStats = nodes.usStats.innerHTML;
const usAcc = nodes.usAccBody.innerHTML;
const usModel = nodes.usModelBody.innerHTML;
const usRealm = nodes.usRealmBody.innerHTML;

// 空态：colspan 必须与表头列数（11 / 8 / 8）一致，否则空表整行错位。
ctx.renderUsage({ totals: {}, by_account: [], by_model: [], by_realm: [], series: [] });
const emptyAcc = nodes.usAccBody.innerHTML;
const emptyModel = nodes.usModelBody.innerHTML;
const emptyRealm = nodes.usRealmBody.innerHTML;
const emptyStats = nodes.usStats.innerHTML;

ctx.renderAccounts([{
  uid: 'uid-0000000000000001', nickname: '号一', credits: 10, credits_total: 100,
  last_success: '2026-09-28T13:00:00Z',
  today: { day: '2026-09-28', requests: 1771, errors: 3, total_tokens: 7100000 },
  token_usage: { request_count: 1771, ok_count: 1768, total_tokens: 18700000, last_latency_ms: 1500 },
}]);
const accounts = nodes.accBody.innerHTML;
process.stdout.write(JSON.stringify({ reqLogBox, reqNoteWithSource, reqNoteNoSource, usStats, usAcc, usModel, usRealm,
  emptyAcc, emptyModel, emptyRealm, emptyStats, accounts }));`
	f, err := os.CreateTemp(t.TempDir(), "usage-cache-render-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("usage cache render node test failed: %v\n%s", err, out)
	}
	got := map[string]string{}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("usage cache render output is not JSON: %v\n%s", err, out)
	}
	const line = "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 1.25s | 2.3k tok | 0.12 credit"
	for _, frag := range []string{
		`<span class="ln">` + line + ` | req-1</span>`,
		`<span class="ln">` + line + ` | 命中 98.1%（2.3k tok） | src=203.0.113.7 ua=&quot;curl/8.4.0&quot; | req-2</span>`,
	} {
		if !strings.Contains(got["reqLogBox"], frag) {
			t.Errorf("请求记录行缺少片段 %q\n实际：%s", frag, got["reqLogBox"])
		}
	}
	if strings.Contains(got["reqNoteWithSource"], "来源未记录") {
		t.Errorf("有来源时不该提示「来源未记录」：%s", got["reqNoteWithSource"])
	}
	if !strings.Contains(got["reqNoteNoSource"], "来源未记录") {
		t.Errorf("整页无来源时应提示「来源未记录」：%s", got["reqNoteNoSource"])
	}

	// 统计卡：命中率卡在总览统计条里，无样本时也是 '—'（不是 0%）。
	if !strings.Contains(got["usStats"], `<div class="stat good"><div class="v">98.1%</div><div class="k">缓存命中率</div></div>`) {
		t.Errorf("用量总览缺少缓存命中率统计卡：%s", got["usStats"])
	}
	if !strings.Contains(got["emptyStats"], `<div class="stat "><div class="v">—</div><div class="k">缓存命中率</div></div>`) {
		t.Errorf("无样本时命中率卡应显示 —：%s", got["emptyStats"])
	}

	// 明细表：命中率列在「合计」与「均延迟」之间（三张表同序），绝对值进 title。
	if !strings.Contains(got["usAcc"],
		`<td class="num">120</td><td class="num"><span class="c-ok" title="命中 95 / 未命中 5 tok">95%</span></td><td class="num">900ms</td><td class="num">30.0 tok/s</td>`) {
		t.Errorf("按账号表的命中率列位置/内容不对：%s", got["usAcc"])
	}
	if !strings.Contains(got["usModel"], `<td class="num">12</td><td class="num"><span class="c-bad" title="命中 0 / 未命中 100 tok">0%</span></td></tr>`) {
		t.Errorf("按模型表的命中率列位置/内容不对：%s", got["usModel"])
	}
	if !strings.Contains(got["usRealm"], `<td class="num">12</td><td class="num"><span class="c-muted">—</span></td></tr>`) {
		t.Errorf("按域表缺观测时应显示 —：%s", got["usRealm"])
	}
	for key, want := range map[string]string{"emptyAcc": "11", "emptyModel": "8", "emptyRealm": "8"} {
		if !strings.Contains(got[key], `colspan="`+want+`"`) || !strings.Contains(got[key], "暂无数据") {
			t.Errorf("%s 空态 colspan 应为 %s：%s", key, want, got[key])
		}
	}

	// 回归红线：账号表「今日用量」仍是 7.10M（大写单位 + 两位小数），且不带 tok 后缀。
	if !strings.Contains(got["accounts"], `<span class="usage-item usage-total"><b>7.10M</b></span>`) {
		t.Errorf("账号表今日用量不再是 7.10M 的 chip：%s", got["accounts"])
	}
	if strings.Contains(got["accounts"], "Mtok") || strings.Contains(got["accounts"], "<em>tok</em>") {
		t.Errorf("账号表用量列又带上了 tok 后缀：%s", got["accounts"])
	}
}

// TestUsageTablesCacheColumnCount 用量明细表的列数必须三处一致：index.html 的表头 <th>、
// app.js usRow 的行内 <td>、空态 colspan。本批给「按账号 / 按模型 / 按域」各加了一列缓存
// 命中率，漏改任何一处都会让整行错位（表格里最显眼、加列时最易漏的地方），故用固定列数钉住。
func TestUsageTablesCacheColumnCount(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()
	for _, tc := range []struct {
		caption string
		cols    int
	}{
		{"用量统计：按账号", 11},
		{"用量统计：按模型", 8},
		{"用量统计：按域", 8},
	} {
		re := regexp.MustCompile(`(?s)<caption class="sr-only">` + regexp.QuoteMeta(tc.caption) + `</caption>.*?</thead>`)
		head := re.FindString(body)
		if head == "" {
			t.Errorf("index.html 缺少用量表（%s）", tc.caption)
			continue
		}
		// `<th[ >]` 排除 <thead
		if ths := regexp.MustCompile(`<th[ >]`).FindAllString(head, -1); len(ths) != tc.cols {
			t.Errorf("%s 有 %d 个 <th>，行内 <td> 与空态 colspan 都是 %d", tc.caption, len(ths), tc.cols)
		}
		if !strings.Contains(head, ">缓存命中率<") {
			t.Errorf("%s 缺少「缓存命中率」表头", tc.caption)
		}
	}

	// app.js 侧：三张表的空态 colspan 必须与表头一致（顺序同 renderUsage：账号/模型/域）。
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	empty := regexp.MustCompile(`colspan="(\d+)" class="empty">暂无数据`).FindAllStringSubmatch(string(js), -1)
	if len(empty) != 3 {
		t.Fatalf("app.js 用量空态有 %d 处，want 3（按账号/按模型/按域）", len(empty))
	}
	for i, want := range []string{"11", "8", "8"} {
		if empty[i][1] != want {
			t.Errorf("第 %d 张用量表空态 colspan=%s，want %s", i+1, empty[i][1], want)
		}
	}
}

// TestIndexConfigRequestClientInfo 面板侧的 logging.request_client_info 开关必须三处对齐：
// 勾选框、CFG_MAP 路径、后端字段名。任一处拼错都不会报错——勾选框照常渲染，保存时被静默
// 忽略（collectConfig 找不到表单元素就 continue），配置看着改了其实没变，故用断言钉住。
func TestIndexConfigRequestClientInfo(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	start := strings.Index(body, `<form id="cfgForm">`)
	end := strings.Index(body, `</form>`)
	if start < 0 || end <= start {
		t.Fatal("index.html 缺少配置表单 #cfgForm")
	}
	form := body[start:end]
	at := strings.Index(form, `name="request_client_info"`)
	if at < 0 {
		t.Fatal(`配置表单缺少 request_client_info 勾选框（保存时会被静默忽略）`)
	}
	// 沿用既有 .switch 控件（滑块 + 标签），不新造控件样式。
	before := form[maxInt(0, at-400):at]
	if !strings.Contains(before, `class="switch"`) || !strings.Contains(before, `type="checkbox"`) {
		t.Errorf("request_client_info 未使用既有 .switch 复选框控件：%s", before)
	}

	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), `request_client_info: ['logging', 'request_client_info']`) {
		t.Error("CFG_MAP 缺少 request_client_info → logging.request_client_info 的映射（回填/保存都会失效）")
	}
}

// cfgFormFields 从 index.html 的 <form id="cfgForm"> 里抽出所有 name 控件（名字 → 所在片段），
// 以及表单整体的 HTML。多张测试要按同一口径切片，抽成一处免得各写各的。
func cfgFormFields(t *testing.T, body string) (map[string]string, string) {
	t.Helper()
	start := strings.Index(body, `<form id="cfgForm">`)
	if start < 0 {
		t.Fatal("index.html 缺少配置表单 #cfgForm")
	}
	rest := body[start:]
	end := strings.Index(rest, `</form>`)
	if end < 0 {
		t.Fatal("配置表单 #cfgForm 未闭合")
	}
	form := rest[:end]
	fields := map[string]string{}
	for _, m := range regexp.MustCompile(`name="([a-z_0-9]+)"`).FindAllStringSubmatchIndex(form, -1) {
		name := form[m[2]:m[3]]
		// 控件片段取到所在标签结束：断言「是不是 .switch / type=number / min=0」要看标签本身。
		tagStart := strings.LastIndex(form[:m[0]], "<")
		tagEnd := strings.Index(form[m[1]:], ">")
		if tagStart < 0 || tagEnd < 0 {
			continue
		}
		fields[name] = form[tagStart : m[1]+tagEnd+1]
	}
	return fields, form
}

// cfgMapKeys 抽出 app.js 里 CFG_MAP 的全部键名与源码块。CFG_MAP 是「表单名 → 配置路径」
// 的唯一映射表：漏一个键，回填与保存都会静默空转。
func cfgMapKeys(t *testing.T, src string) (map[string]bool, string) {
	t.Helper()
	at := strings.Index(src, "const CFG_MAP = {")
	if at < 0 {
		t.Fatal("app.js 缺少 CFG_MAP")
	}
	block := src[at:]
	close := strings.Index(block, "\n};")
	if close < 0 {
		t.Fatal("CFG_MAP 未以行首 }; 收尾（切片口径失效）")
	}
	block = block[:close]
	keys := map[string]bool{}
	// 不能按行首匹配：CFG_MAP 里多个键写在同一行（`a: [...], b: [...]`），
	// 按「前面是行首或分隔符」判定才不漏。
	for _, m := range regexp.MustCompile(`(?:^|[\s,{])([a-z_0-9]+):\s*\[`).FindAllStringSubmatch(block, -1) {
		keys[m[1]] = true
	}
	return keys, block
}

// TestConfigFormMatchesCFGMap 钉住「配置表单控件 ↔ CFG_MAP ↔ 后端配置键名」三处一致
// （上游 7339a3c 的等价移植，按本 fork 的表单结构与键集重写）。
//
// 为什么需要：这套映射断掉时没有任何编译期或运行期报错——表单里多一个字段会被保存时
// 静默丢弃（collectConfig 只遍历 CFG_MAP），CFG_MAP 多一个键则回填/保存都是空转，
// 两者都只能靠人点开配置页发现。logging.request_client_info 就这样丢过一次。
// 纯 Go 读文件，不需要 node。
func TestConfigFormMatchesCFGMap(t *testing.T) {
	jsBytes, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	keys, mapBlock := cfgMapKeys(t, js)
	if len(keys) < 20 {
		t.Fatalf("CFG_MAP 只解析出 %d 个键（本 fork 40+ 个），切片或正则口径已经失效", len(keys))
	}
	fields, _ := cfgFormFields(t, body)
	if len(fields) == 0 {
		t.Fatal("未从配置表单解析出任何 name 字段")
	}

	// formOnly 是「只在表单里、刻意不走 CFG_MAP」的白名单。本 fork 现状为空集——
	// api_key 这类字段其实都走 CFG_MAP。留这个表是为了让例外显式化：真出现这种
	// 字段时请连同理由登记，而不是放宽断言。
	formOnly := map[string]string{}
	for name := range fields {
		if keys[name] {
			continue
		}
		if why, ok := formOnly[name]; ok {
			t.Logf("表单字段 %q 按白名单跳过 CFG_MAP 检查：%s", name, why)
			continue
		}
		t.Errorf("表单字段 %q 在 CFG_MAP 里没有条目（保存时会被静默丢弃；若确实不该走 CFG_MAP，请登记进 formOnly 并写明理由）", name)
	}
	for name := range keys {
		if _, ok := fields[name]; !ok {
			t.Errorf("CFG_MAP 键 %q 在配置表单里没有同名控件（回填/保存都是空转）", name)
		}
	}

	// 显式点名三个「后端有配置项、面板必须能在线改」的键：路径写错（拼成别的节名）
	// 时上面的双向检查仍然会通过，只有这条能发现。
	for _, want := range []string{
		`request_client_info: ['logging', 'request_client_info']`,
		`include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks']`,
		`credit_floor: ['pool', 'credit_floor']`,
	} {
		if !strings.Contains(mapBlock, want) {
			t.Errorf("CFG_MAP 缺条目 %s（回填/保存都会失效）", want)
		}
	}

	// 第三条腿：CFG_MAP 路径的最后一段必须真的存在于后端配置结构体的 json tag 里
	// （否则面板存下去的键后端不认识，落盘后静默丢弃）。只对上面点名的键查，
	// 因为路径中间段（schedule/pool/logging）与结构体嵌套的对应关系留给后端测试。
	cfgGo := filepath.Join("..", "..", "cmd", "server", "config.go")
	raw, err := os.ReadFile(cfgGo)
	if err != nil {
		t.Logf("跳过后端键名核对（读不到 %s：%v）", cfgGo, err)
		return
	}
	for _, name := range []string{"request_client_info", "include_disabled_in_tasks", "credit_floor"} {
		if !strings.Contains(string(raw), `json:"`+name+`"`) {
			t.Errorf("后端配置结构体里没有 json:%q（面板保存的 %s 会被静默丢弃）", name, name)
		}
	}
}

// TestIndexConfigCreditFloorAndIncludeDisabled 本批新增的两个配置控件（后端早已支持，
// 面板此前缺字段）：控件类型、min、说明文字与「默认值」都要写清——这两个键都是
// 「静默生效」型：填错了不报错，只是选号/定时任务的行为与预期不同。
func TestIndexConfigCreditFloorAndIncludeDisabled(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	fields, form := cfgFormFields(t, rec.Body.String())

	// include_disabled_in_tasks：布尔开关，沿用既有 .switch 复选框。
	tag, ok := fields["include_disabled_in_tasks"]
	if !ok {
		t.Fatal("配置表单缺少 include_disabled_in_tasks 勾选框（保存时会被静默忽略）")
	}
	if !strings.Contains(tag, `type="checkbox"`) {
		t.Errorf("include_disabled_in_tasks 不是 checkbox：%s", tag)
	}
	if at := strings.Index(form, `name="include_disabled_in_tasks"`); at < 0 ||
		!strings.Contains(form[maxInt(0, at-300):at], `class="switch"`) {
		t.Error("include_disabled_in_tasks 未使用既有 .switch 控件（不新造控件样式）")
	}
	for _, want := range []string{"默认关闭", "已禁用账号依旧不参与选号"} {
		if !strings.Contains(form, want) {
			t.Errorf("include_disabled_in_tasks 说明文字缺 %q（默认值与「禁用只关选号」的边界必须写清）", want)
		}
	}

	// credit_floor：数字输入，min=0；0 = 关闭、正整数 = 低于该值不出票。
	tag, ok = fields["credit_floor"]
	if !ok {
		t.Fatal("配置表单缺少 credit_floor 输入框（保存时会被静默忽略）")
	}
	for _, want := range []string{`type="number"`, `min="0"`} {
		if !strings.Contains(tag, want) {
			t.Errorf("credit_floor 输入框缺 %s：%s", want, tag)
		}
	}
	if at := strings.Index(form, `name="credit_floor"`); at < 0 ||
		!strings.Contains(form[maxInt(0, at-300):at], `class="fld"`) {
		t.Error("credit_floor 未使用既有 label.fld 字段控件")
	}
	for _, want := range []string{"0 = 关闭（默认", "低于该值不出票"} {
		if !strings.Contains(form, want) {
			t.Errorf("credit_floor 说明文字缺 %q（0 与正整数的语义必须写清）", want)
		}
	}
}

// TestIndexKeyInputOwnForm #keyInput 必须待在独立 <form class="form-bare"> 里（上游
// 20bde29）：游离的 password 输入框会被 Chromium 凭空合成「用户名 + 密码」凭证表单，
// 并在同页抓第一个文本输入框当用户名，于是聚焦那个输入框就弹「保存的密码」下拉。
//
// 与上游的差异：上游用 style="display:contents" + onsubmit="return false"，
// 本 fork 两样都不能写（静态内联样式禁令 / CSP script-src 'self' 拦内联处理器），
// 改为 .form-bare 工具类 + app.js 里绑 submit 拦截——这里把两条替换都钉住。
func TestIndexKeyInputOwnForm(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	at := strings.Index(body, `<form id="keyForm" class="form-bare">`)
	if at < 0 {
		t.Fatal(`index.html 缺少 <form id="keyForm" class="form-bare">（keyInput 又变成游离密码框了）`)
	}
	// form 标签本身不得带 style：display:contents 由 .form-bare 承担。
	tagEnd := strings.Index(body[at:], ">")
	if tagEnd < 0 {
		t.Fatal("#keyForm 标签未闭合")
	}
	if tag := body[at : at+tagEnd+1]; strings.Contains(tag, "style=") || regexp.MustCompile(`\son[a-z]+=`).MatchString(tag) {
		t.Errorf("#keyForm 用了内联 style 或内联事件处理器（CSP 拦截 / 静态内联样式禁令）：%s", tag)
	}
	closeAt := strings.Index(body[at:], "</form>")
	if closeAt < 0 {
		t.Fatal("#keyForm 未闭合")
	}
	inner := body[at : at+closeAt]
	if !strings.Contains(inner, `id="keyInput"`) {
		t.Fatalf("#keyInput 不在 #keyForm 内（Chromium 仍会合成凭证表单）：%s", inner)
	}
	if !strings.Contains(inner, `autocomplete="off"`) {
		t.Errorf("keyInput 未设 autocomplete=\"off\"（密码管理器仍会接手机器生成的凭证表单）：%s", inner)
	}
	// 全局：index.html 不得出现任何内联事件处理器（CSP script-src 'self' 下它们静默失效，
	// 表现是「按钮点了没反应」）。上游 20bde29 的 onsubmit="return false" 正属此列。
	if m := regexp.MustCompile(`\son[a-z]+="`).FindAllString(body, -1); len(m) != 0 {
		t.Errorf("index.html 出现内联事件处理器（CSP 下静默失效）：%v", m)
	}
	if !strings.Contains(body, `.form-bare { display: contents; }`) {
		t.Error("缺少 .form-bare { display: contents; } 样式（form 会生成盒子、布局被撑开）")
	}

	// app.js 侧：submit 必须被拦住（单输入框 form 回车会隐式提交、把整页 GET 刷新一遍），
	// 且既有密钥门逻辑（点击 / 回车 → #btnKey）不能被动掉。
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	submitAt := strings.Index(src, `$('keyForm').addEventListener('submit'`)
	if submitAt < 0 {
		t.Fatal("app.js 未给 #keyForm 绑 submit 拦截（回车会隐式提交并刷新页面）")
	}
	if tail := src[submitAt:]; !strings.Contains(tail[:minInt(len(tail), 200)], "preventDefault") {
		t.Error("#keyForm 的 submit 处理器没有 preventDefault")
	}
	for _, want := range []string{
		`$('btnKey').onclick`,
		`$('keyInput').addEventListener('keydown'`,
		`if (e.key === 'Enter') $('btnKey').click();`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("既有密钥门逻辑被破坏，app.js 里找不到 %s", want)
		}
	}
}

// TestIndexPackagesSortControl 积分构成逐包明细的排序切换控件（上游 6bc832c，本 fork
// 保留了「前 N 条 + 折叠」，只加控件与排序口径）：select 必须落在 #view-packages 内，
// 两个 option 的 value 要与 app.js 的 PK_SORT_LABELS 键一致——拼错不会报错，
// 只会让控件切了个寂寞（渲染端认不出这个值，回落默认）。
func TestIndexPackagesSortControl(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	viewAt := strings.Index(body, `id="view-packages"`)
	nextView := strings.Index(body, `id="view-usage"`)
	selAt := strings.Index(body, `<select id="pkSort" class="xs"`)
	if viewAt < 0 || nextView <= viewAt {
		t.Fatal("index.html 视图段落结构变了（找不到 #view-packages / #view-usage）")
	}
	if selAt < 0 || selAt < viewAt || selAt > nextView {
		t.Fatalf("#pkSort 排序选择器不在 #view-packages 视图内（selAt=%d）", selAt)
	}
	head := body[selAt:]
	if end := strings.Index(head, "</select>"); end > 0 {
		head = head[:end]
	}
	for _, want := range []string{`value="end_asc" selected`, `value="size_desc"`} {
		if !strings.Contains(head, want) {
			t.Errorf("#pkSort 缺少 option %s：%s", want, head)
		}
	}

	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{
		`const PK_SORT_LABELS = { end_asc:`, // 两种规则的显示名（与 option 文案同源）
		`size_desc:`,
		`const LS_PK_SORT = 'pkSortMode';`,
		`function pkSortLoad()`,
		`function pkSortSave(mode)`,
		`$('pkSort').onchange`,
		`pkDetailGroups(a.packages || [], detailLimit, pkSortMode)`, // 渲染端真的用上了当前规则
		`let lastPackages = null, lastPackagesLimit;`,               // 切换只重排内存数据，不重新请求上游
		`data-pk-group="`,                                           // 折叠/展开行为必须保留
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js 缺少排序切换的相关实现：%s", want)
		}
	}
}

// TestAppJSConfigFormRoundTrip 配置页两个新字段的真实回填与保存（node + DOM 桩）：
// 只造这两个键的控件，跑 loadConfig（后端 → 控件）与 collectConfig（控件 → 后端嵌套路径）。
// 桩里 input.value 走 getter/setter 强制成字符串 —— 真实 DOM 的 input.value 永远是
// string，loadConfig 会把数字直接赋进去，不模拟这一步 collectConfig 的 .trim() 会假失败。
func TestAppJSConfigFormRoundTrip(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; config round-trip test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const CFG_MAP = {');
const end = src.indexOf('/* Go 时长字段即时校验');
if (start < 0 || end < 0) throw new Error('CFG_MAP slice not found');
// DOM 桩：input.value 永远按字符串存（同真实 DOM），checkbox 用 checked。
function inputStub(type) {
  return {
    type,
    checked: false,
    _v: '',
    get value() { return this._v; },
    set value(v) { this._v = v == null ? '' : String(v); },
  };
}
const checkEl = inputStub('checkbox');
const numEl = inputStub('number');
const keyEl = inputStub('password');
const nodes = { cfgPath: { textContent: '' }, cfgNote: { textContent: '' } };
nodes.cfgForm = { elements: { include_disabled_in_tasks: checkEl, credit_floor: numEl, api_key: keyEl } };
let payload = {};
const ctx = {
  Number, String, Object, Array, JSON, Promise, console,
  api: async () => ({ config: payload, path: '/etc/workbuddy/config.json' }),
  toast: m => { ctx.toasts.push(String(m)); },
  markDurationFields: () => {},
  $: id => nodes[id] || (nodes[id] = { textContent: '', innerHTML: '' }),
  toasts: [],
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.loadConfig = loadConfig; this.collectConfig = collectConfig; this.CFG_MAP = CFG_MAP;', ctx);
(async () => {
  // 1) 后端下发了这两个键：控件要被回填，保存要原样写回嵌套路径。
  payload = { schedule: { include_disabled_in_tasks: true }, pool: { credit_floor: 100 } };
  await ctx.loadConfig();
  const filled = { checked: checkEl.checked, credit: numEl.value, path: nodes.cfgPath.textContent };
  const saved = ctx.collectConfig();
  // 2) 老配置里没有这两个键（后端缺省 false / 0）：控件回落默认态，留空 = 不发送。
  payload = {};
  await ctx.loadConfig();
  const fallback = { checked: checkEl.checked, credit: numEl.value };
  const savedEmpty = ctx.collectConfig();
  process.stdout.write(JSON.stringify({
    filled, saved, fallback, savedEmpty,
    mapped: [ctx.CFG_MAP.include_disabled_in_tasks, ctx.CFG_MAP.credit_floor],
    toasts: ctx.toasts,
  }));
})();`
	f, err := os.CreateTemp(t.TempDir(), "cfg-roundtrip-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("config round-trip node test failed: %v\n%s", err, out)
	}
	got := map[string]json.RawMessage{}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("config round-trip output is not JSON: %v\n%s", err, out)
	}
	check := func(key, want string) {
		t.Helper()
		if strings.TrimSpace(string(got[key])) != want {
			t.Errorf("%s=%s want %s", key, got[key], want)
		}
	}
	// 回填：勾选框 checked、数字框变字符串 "100"（DOM 语义），配置文件路径照旧回显。
	check("filled", `{"checked":true,"credit":"100","path":"/etc/workbuddy/config.json"}`)
	// 保存：collectConfig 产出后端认识的嵌套路径；数字走 Number 而不是字符串。
	check("saved", `{"schedule":{"include_disabled_in_tasks":true},"pool":{"credit_floor":100}}`)
	// 缺省：缺键时勾选框回落 false、数字框为空。勾选框**总是**回传当前状态（false 也发，
	// 语义就是「关」——这是本 fork 既有口径）；数字框留空 = 沿用现值，不发送。
	check("fallback", `{"checked":false,"credit":""}`)
	check("savedEmpty", `{"schedule":{"include_disabled_in_tasks":false}}`)
	// CFG_MAP 的路径就是回填/保存用的那条路径（[节, 键]）。
	check("mapped", `[["schedule","include_disabled_in_tasks"],["pool","credit_floor"]]`)
	check("toasts", `[]`)
}

// TestAppJSPackagesSortOrder 积分构成逐包明细的排序切换（node + DOM 桩真实渲染）：
//   - 默认 end_asc：第一行是最早到期的包；无到期时间的包垫底，不掺进日期序；
//   - 切到 size_desc：第一行变成面额最大的包，且**不用重新请求上游**（onchange 重排内存数据）；
//   - 两种规则下「前 N 条 + 其余折叠」的既有行为都不变（折叠行 hidden + 摘要按钮计数）；
//   - 顺带回归账号表「今日用量」列：仍是 7.10M 的 chip（大写单位 + 两位小数，无 tok 后缀）。
func TestAppJSPackagesSortOrder(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; packages sort render skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
// 四段互不重叠的纯函数切片（各自下界都是下一个函数的声明，段内没有顶层副作用，
// 除了 pk 段末尾的 #pkSort 绑定——那正是被测的控件接线）：
//   A 转义/格式化工具，B 账号表渲染，C fmtTok，D 积分构成整段（含排序、折叠与 loadPackages）。
const a = src.slice(src.indexOf('function esc('), src.indexOf('function veilStack('));
const b = src.slice(src.indexOf('function avgTTFB('), src.indexOf('async function loadOverview('));
const c = src.slice(src.indexOf('function fmtTok('), src.indexOf('function fmtMs('));
const d = src.slice(src.indexOf('const PK_COLORS'), src.indexOf("if ($('btnPk')) $('btnPk').onclick"));
if (!a || !b || !c || !d) throw new Error('render slices not found in app.js');
const nodes = {};
const el = id => (nodes[id] = nodes[id] || { innerHTML: '', textContent: '', value: '', addEventListener() {} });
const storage = {};
const apiCalls = [];
const ctx = {
  Date, Number, String, Boolean, Math, Array, Object, JSON, RegExp, Error, isNaN, parseInt, parseFloat,
  Map, Set, Promise, console,
  $: el,
  localStorage: {
    getItem: k => (k in storage ? storage[k] : null),
    setItem: (k, v) => { storage[k] = String(v); },
    removeItem: k => { delete storage[k]; },
  },
  // 上游桩：packages 返回逐包数据，config 返回明细条数上限 2（走 loadPackages 的真实路径）。
  api: async path => {
    apiCalls.push(path);
    if (path === 'packages') return { accounts: [acct] };
    return { config: { panel: { package_detail_limit: 2 } } };
  },
};
vm.createContext(ctx);
vm.runInContext(a + '\n' + b + '\n' + c + '\n' + d +
  '\nthis.renderAccounts = renderAccounts; this.renderPackages = renderPackages; this.loadPackages = loadPackages;', ctx);
const DAY = 86400000, NOW = Date.now();
const pkg = (name, size, remain, expiresAt) => ({
  name, package_code: 'C-' + name, size, remain, used: size - remain,
  expires_at: expiresAt || undefined, end_time: expiresAt ? new Date(expiresAt).toISOString().slice(0, 10) : '',
});
const acct = {
  uid: 'uid-0000000000000001', nickname: '号一', realm: 'cn', remain: 6, size: 1250,
  last_success: '2026-09-28T13:00:00Z',
  today: { day: '2026-09-28', requests: 1771, errors: 3, total_tokens: 7100000 },
  token_usage: { request_count: 1771, ok_count: 1768, total_tokens: 18700000, last_latency_ms: 1500 },
  packages: [
    pkg('大额晚到期', 500, 5, NOW + 30 * DAY),
    pkg('小额快到期', 100, 1, NOW + 1 * DAY),
    pkg('中额无到期', 400, 4, 0),
    pkg('小额稍晚', 50, 2, NOW + 5 * DAY),
    pkg('已用完', 200, 0, NOW + 2 * DAY),
  ],
};
ctx.renderAccounts([acct]);
const accounts = nodes.accBody.innerHTML;
// 折叠行（data-pk-row）与摘要按钮（pk-group-summary）不算可见数据行；
// 只认以 <tr 开头的片段，避开 </tbody> 后那段尾巴。
const rowsOf = html => html.split('<tbody>')[1].split('</tbody>')[0].split('</tr>')
  .map(s => s.trim()).filter(s => s.startsWith('<tr'));
const nameOf = row => { const m = row.match(/<td>([^<]*)/); return m ? m[1] : ''; };
const visibleOf = html => rowsOf(html).filter(r => !/data-pk-row=|pk-group-summary/.test(r)).map(nameOf);
const foldedOf = html => rowsOf(html).filter(r => /data-pk-row="rest"/.test(r)).map(nameOf);
const acctNoteOf = html => { const m = html.match(/<span class="note">([^<]*)<\/span>/); return m ? m[1] : ''; };
(async () => {
  await ctx.loadPackages();                      // 真实路径：拉数据 → 按配置条数渲染
  const endAsc = {
    visible: visibleOf(nodes.pkDetail.innerHTML),
    folded: foldedOf(nodes.pkDetail.innerHTML),
    acctNote: acctNoteOf(nodes.pkDetail.innerHTML),
    hidden: /data-pk-row="rest" hidden/.test(nodes.pkDetail.innerHTML),
    restBtn: (nodes.pkDetail.innerHTML.match(/其余未用完 \d+ 个包（面额合计 [^）]*）/) || [''])[0],
  };
  const callsAfterLoad = apiCalls.slice();
  // 走真实控件接线：把 select 的值改成 size_desc 再触发 onchange（等价于用户操作）。
  nodes.pkSort.value = 'size_desc';
  nodes.pkSort.onchange();
  const sizeDesc = {
    visible: visibleOf(nodes.pkDetail.innerHTML),
    folded: foldedOf(nodes.pkDetail.innerHTML),
    acctNote: acctNoteOf(nodes.pkDetail.innerHTML),
    hidden: /data-pk-row="rest" hidden/.test(nodes.pkDetail.innerHTML),
    stored: storage.pkSortMode,
  };
  // 折叠/展开按钮仍在（既有行为不能被排序开关挤掉）。
  const hasToggle = /data-pk-group="rest"/.test(nodes.pkDetail.innerHTML);
  process.stdout.write(JSON.stringify({
    accounts, endAsc, sizeDesc, hasToggle,
    callsAfterLoad, callsAfterSwitch: apiCalls,
  }));
})();`
	f, err := os.CreateTemp(t.TempDir(), "pk-sort-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("packages sort render failed: %v\n%s", err, out)
	}
	var got struct {
		Accounts string `json:"accounts"`
		EndAsc   struct {
			Visible  []string `json:"visible"`
			Folded   []string `json:"folded"`
			AcctNote string   `json:"acctNote"`
			Hidden   bool     `json:"hidden"`
			RestBtn  string   `json:"restBtn"`
		} `json:"endAsc"`
		SizeDesc struct {
			Visible  []string `json:"visible"`
			Folded   []string `json:"folded"`
			AcctNote string   `json:"acctNote"`
			Hidden   bool     `json:"hidden"`
			Stored   string   `json:"stored"`
		} `json:"sizeDesc"`
		HasToggle      bool     `json:"hasToggle"`
		CallsAfterLoad []string `json:"callsAfterLoad"`
		CallsAfterSort []string `json:"callsAfterSwitch"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("packages sort render output is not JSON: %v\n%s", err, out)
	}

	// 默认按到期升序：最早的在前，无到期垫底（第 3 位之后），无到期的包不进前 2 条。
	if want := []string{"小额快到期", "小额稍晚"}; !equalStrings(got.EndAsc.Visible, want) {
		t.Errorf("end_asc 可见行=%v want %v", got.EndAsc.Visible, want)
	}
	if want := []string{"大额晚到期", "中额无到期"}; !equalStrings(got.EndAsc.Folded, want) {
		t.Errorf("end_asc 折叠行=%v want %v（无到期包必须垫底，不掺进日期序）", got.EndAsc.Folded, want)
	}

	// 切到面额降序：第一行换成面额最大的包。
	if want := []string{"大额晚到期", "中额无到期"}; !equalStrings(got.SizeDesc.Visible, want) {
		t.Errorf("size_desc 可见行=%v want %v", got.SizeDesc.Visible, want)
	}
	if want := []string{"小额快到期", "小额稍晚"}; !equalStrings(got.SizeDesc.Folded, want) {
		t.Errorf("size_desc 折叠行=%v want %v", got.SizeDesc.Folded, want)
	}
	if got.SizeDesc.Stored != "size_desc" {
		t.Errorf("切换后的选择未持久化（localStorage pkSortMode=%q）", got.SizeDesc.Stored)
	}
	// 切换只重排内存数据：loadPackages 打过 packages+config 两次请求，onchange 后不许再多。
	if want := []string{"packages", "config"}; !equalStrings(got.CallsAfterLoad, want) {
		t.Errorf("loadPackages 请求序列=%v want %v", got.CallsAfterLoad, want)
	}
	if !equalStrings(got.CallsAfterSort, got.CallsAfterLoad) {
		t.Errorf("切换排序后又打了上游：%v → %v（应只重排内存数据）", got.CallsAfterLoad, got.CallsAfterSort)
	}

	// 既有折叠行为不变：折叠行带 hidden、摘要按钮还在且计数按规则重算。
	for _, tc := range []struct {
		name string
		got  bool
	}{{"end_asc", got.EndAsc.Hidden}, {"size_desc", got.SizeDesc.Hidden}} {
		if !tc.got {
			t.Errorf("%s 折叠行丢失 hidden（折叠/展开行为被破坏）", tc.name)
		}
	}
	if !got.HasToggle {
		t.Error("折叠摘要按钮 data-pk-group=\"rest\" 消失（既有展开行为被破坏）")
	}
	if !strings.Contains(got.EndAsc.RestBtn, "其余未用完 2 个包") {
		t.Errorf("end_asc 折叠摘要=%q，应说出折叠了几个包", got.EndAsc.RestBtn)
	}
	// 每个账号表头上的规则说明也要跟着切换（那是用户唯一能确认「当前按什么排」的地方）。
	if !strings.Contains(got.EndAsc.AcctNote, "按到期升序展示前 2 条") {
		t.Errorf("end_asc 账号表头注=%q", got.EndAsc.AcctNote)
	}
	if !strings.Contains(got.SizeDesc.AcctNote, "按面额降序展示前 2 条") {
		t.Errorf("size_desc 账号表头注=%q", got.SizeDesc.AcctNote)
	}

	// 回归红线：账号表「今日用量」仍是 7.10M（大写单位 + 两位小数），且不带 tok 后缀。
	if !strings.Contains(got.Accounts, `<span class="usage-item usage-total"><b>7.10M</b></span>`) {
		t.Errorf("账号表今日用量不再是 7.10M 的 chip：%s", got.Accounts)
	}
	if strings.Contains(got.Accounts, "Mtok") || strings.Contains(got.Accounts, "<em>tok</em>") {
		t.Errorf("账号表用量列又带上了 tok 后缀：%s", got.Accounts)
	}
}

// equalStrings 比对两个字符串切片（顺序敏感）。
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// minInt / maxInt 是本文件里既有 maxInt 的补充（截断取尾片段时用）。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
