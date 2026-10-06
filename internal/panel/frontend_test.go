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
  '\nthis.loadUsage=loadUsage; this.loadLogs=loadLogs; this.renderAccounts=renderAccounts; this.renderRequestMetrics=renderRequestMetrics;' +
  '\nthis.reqSetView=reqSetView;',
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
  // 请求记录默认视图是表格（与上游一致），下面断言的是**行式视图**的输出：切到行式
  // 后走的仍是移植前的 requestLogLine，行内容必须逐字不变（默认视图与切换另有
  // TestAppJSRequestTableRender 钉住）。
  sandbox.reqSetView('line');
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
	if !strings.Contains(str("accounts"), `<span class="usage-item usage-total"><b>7.10<span class="usage-unit">M</span></b></span>`) {
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
	if !strings.Contains(got["accounts"], `<span class="usage-item usage-total"><b>7.10<span class="usage-unit">M</span></b></span>`) {
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
		if !strings.Contains(row, `<span class="usage-item usage-total"><b>7.10<span class="usage-unit">M</span></b></span>`) {
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
//   - 请求记录：同一批记录先按默认的表格视图渲染（#reqBody），再切到行式渲染
//     （#reqLogBox，即有来源与无来源两组 .ln 行；表格视图的完整契约见
//     TestAppJSRequestTableRender）；
//   - 用量页：总览统计条固定 6 张卡（0 空白格）、积分扣除统计条含缓存命中率卡（4→5 张）、
//     按账号/按模型/按域三张明细表（含新增的命中率列与空态 colspan）；
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
  // 请求记录的模块级状态（声明在文件顶部，不在本切片里）：表格渲染要读它们。
  reqEntries: [],
  reqFilter: { client_ip: '', user_agent: '', account: '', model: '', outcome: '' },
  reqView: 'table',
};
vm.createContext(ctx);
vm.runInContext(a + '\n' + b + '\n' + c + '\n' + d +
  '\nthis.renderRequestMetrics = renderRequestMetrics;' +
  '\nthis.renderUsage = renderUsage;' +
  '\nthis.renderAccounts = renderAccounts;' +
  '\nthis.tokenChipHTML = tokenChipHTML;\n', ctx);

const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const base = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: '账号(uid8)', duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12 };
const plain = { ...base, request_id: 'req-1' };
const full = { ...base, request_id: 'req-2', client_ip: '203.0.113.7', user_agent: 'curl/8.4.0', cache_hit_tokens: 2257, cache_miss_tokens: 43 };
const metrics = { completed: 2, success_rate: 100, http_success_rate: 100, avg_duration_ms: 1250, in_flight: 0,
  archive: { enabled: true, bytes: 20480 } };
// 默认视图 = 表格：同一批记录先以表格渲染（首屏形态），再切到行式渲染。
// 两个分支的输出分别断言，行式必须与移植前逐字一致。
ctx.renderRequestMetrics(metrics, [plain, full]);
const reqTable = nodes.reqBody.innerHTML;
ctx.reqView = 'line';
ctx.renderRequestMetrics(metrics, [plain, full]);
const reqLogBox = nodes.reqLogBox.innerHTML;
const reqCountWithSource = nodes.reqCount.textContent;
// 开关关闭 / 旧归档：整批记录都没有来源时明确点名（计数条转 .src-off），
// 而不是让人以为解析坏了或筛选失效了。
ctx.renderRequestMetrics({ completed: 1, archive: { enabled: true, bytes: 0 } }, [plain]);
const reqCountNoSource = nodes.reqCount.textContent;
const reqCountNoSourceCls = nodes.reqCount.className;

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
const creditStats = nodes.usCreditStats.innerHTML;
const creditNote = nodes.usCreditNote.textContent;
// 总览统计条必须是 6 张实卡、0 个 .blank：statsHTML 把不足列数的空位补成 6 的倍数，
// 多出第 7 张卡时第二行会只剩 1 张卡 + 5 个空白格，而空白格的 1px 网格线照样可见。
const usStatsCards = (usStats.match(/class="stat\b/g) || []).length;
const usStatsBlanks = (usStats.match(/class="stat blank"/g) || []).length;

// 空态：colspan 必须与表头列数（11 / 8 / 8）一致，否则空表整行错位。
ctx.renderUsage({ totals: {}, by_account: [], by_model: [], by_realm: [], series: [] });
const emptyAcc = nodes.usAccBody.innerHTML;
const emptyModel = nodes.usModelBody.innerHTML;
const emptyRealm = nodes.usRealmBody.innerHTML;
const emptyStats = nodes.usStats.innerHTML;
const emptyCreditStats = nodes.usCreditStats.innerHTML;
const emptyCreditNote = nodes.usCreditNote.textContent;

ctx.renderAccounts([{
  uid: 'uid-0000000000000001', nickname: '号一', credits: 10, credits_total: 100,
  last_success: '2026-09-28T13:00:00Z',
  today: { day: '2026-09-28', requests: 1771, errors: 3, total_tokens: 7100000 },
  token_usage: { request_count: 1771, ok_count: 1768, total_tokens: 18700000, last_latency_ms: 1500 },
}]);
const accounts = nodes.accBody.innerHTML;
process.stdout.write(JSON.stringify({ reqLogBox, reqCountWithSource, reqCountNoSource, reqCountNoSourceCls, reqTable,
  usStats, usAcc, usModel, usRealm,
  creditStats, creditNote, usStatsCards: String(usStatsCards), usStatsBlanks: String(usStatsBlanks),
  chipUnit: ctx.tokenChipHTML('7.10M'), chipPlain: ctx.tokenChipHTML('320'), chipDash: ctx.tokenChipHTML('—'),
  emptyAcc, emptyModel, emptyRealm, emptyStats, emptyCreditStats, emptyCreditNote, accounts }));`
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
	// 行式视图：逐字沿用移植前的行格式（同一批记录、同一条 requestLogLine）。
	for _, frag := range []string{
		`<span class="ln">` + line + ` | req-1</span>`,
		`<span class="ln">` + line + ` | 命中 98.1%（2.3k tok） | src=203.0.113.7 ua=&quot;curl/8.4.0&quot; | req-2</span>`,
	} {
		if !strings.Contains(got["reqLogBox"], frag) {
			t.Errorf("请求记录行缺少片段 %q\n实际：%s", frag, got["reqLogBox"])
		}
	}
	// 表格视图（默认）：两行都在 #reqBody 里，来源列有值/缺值分别呈现。
	if !strings.Contains(got["reqTable"], `<span class="tag ok">200 成功</span>`) ||
		!strings.Contains(got["reqTable"], `<span class="clip ip" title="203.0.113.7">203.0.113.7</span>`) ||
		strings.Count(got["reqTable"], `<tr title="`) != 2 {
		t.Errorf("请求记录表格渲染不对：%s", got["reqTable"])
	}
	if strings.Contains(got["reqCountWithSource"], "来源未记录") {
		t.Errorf("有来源时不该提示「来源未记录」：%s", got["reqCountWithSource"])
	}
	if !strings.Contains(got["reqCountNoSource"], "来源未记录") {
		t.Errorf("整页无来源时应提示「来源未记录」：%s", got["reqCountNoSource"])
	}
	if !strings.Contains(got["reqCountNoSourceCls"], "src-off") {
		t.Errorf("无来源时计数条应转琥珀（.src-off）：%q", got["reqCountNoSourceCls"])
	}

	// 总览统计条必须 6 张实卡、0 个空白格：statsHTML 只把不足列数的空位补成 6 的倍数，
	// 第 7 张卡会让第二行只剩 1 张卡 + 5 个空白格（空白格的 1px 网格线照样可见，看着像
	// 5 张卡没加载出来）。命中率卡因此挂在「积分扣除历史」的统计条上。
	if got["usStatsCards"] != "6" || got["usStatsBlanks"] != "0" {
		t.Errorf("用量总览统计条应为 6 张实卡 + 0 空白格，实际 %s 张 / %s 格：%s",
			got["usStatsCards"], got["usStatsBlanks"], got["usStats"])
	}
	// 统计卡：命中率卡在积分扣除统计条里，无样本时也是 '—'（不是 0%）。
	if !strings.Contains(got["creditStats"], `<div class="stat good"><div class="v">98.1%</div><div class="k">缓存命中率</div></div>`) {
		t.Errorf("积分扣除统计条缺少缓存命中率统计卡：%s", got["creditStats"])
	}
	if !strings.Contains(got["emptyCreditStats"], `<div class="stat "><div class="v">—</div><div class="k">缓存命中率</div></div>`) {
		t.Errorf("无样本时命中率卡应显示 —：%s", got["emptyCreditStats"])
	}
	// 命中率卡的样本集与本条其它卡不同（全部 Token vs 积分匹配到的 Token），有样本时
	// note 要点名口径，无样本时不要多出这句。
	if !strings.Contains(got["creditNote"], "缓存命中率为本窗口全部 Token 口径") {
		t.Errorf("有缓存样本时 note 应点名命中率口径：%s", got["creditNote"])
	}
	if strings.Contains(got["emptyCreditNote"], "缓存命中率为本窗口全部 Token 口径") {
		t.Errorf("无缓存样本时不该出现命中率口径说明：%s", got["emptyCreditNote"])
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
	// 单位字母要求单独包一层 .usage-unit（值/单位之间留 2px 间隔，见 index.html 注释）：
	// 没有单位（不足 1000 的精确整数）与无数据的 '—' 不得凭空多出这个 span，否则 chip
	// 内会多出一个空元素、把内边距撑宽。
	if !strings.Contains(got["accounts"], `<span class="usage-item usage-total"><b>7.10<span class="usage-unit">M</span></b></span>`) {
		t.Errorf("账号表今日用量不再是 7.10M 的 chip：%s", got["accounts"])
	}
	if strings.Contains(got["accounts"], "Mtok") || strings.Contains(got["accounts"], "<em>tok</em>") {
		t.Errorf("账号表用量列又带上了 tok 后缀：%s", got["accounts"])
	}
	if got["chipUnit"] != `7.10<span class="usage-unit">M</span>` {
		t.Errorf("带单位的值应拆出 .usage-unit：%s", got["chipUnit"])
	}
	if got["chipPlain"] != "320" || got["chipDash"] != "—" {
		t.Errorf("无单位/无数据不应插入 .usage-unit：plain=%q dash=%q", got["chipPlain"], got["chipDash"])
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

// TestAppJSUsageSortAndChartRender 用量页本批两项增量的完整闸门（node + DOM 桩真跑一遍）：
//   - (a) 排序纯函数 usSortRows：8 个可排序列 × 升降序，覆盖缺失值（无样本的命中率/延迟/
//     速率恒沉底，升序也不上浮）、零值（0 是合法值，按 0 参与排序）、并列（按原下标稳定）、
//     纯函数性（不改动入参）；
//   - (b) 表头排序交互：初始指示符与 aria-sort、点击后各表首行顺序、再点切换升降序、
//     换列回到降序、三张表状态互相独立、不可排序列点了不做事、键盘 Enter/空格等价、
//     排序不触发新请求（假 fetch 逐字比对 URL 清单）、刷新后排序状态不丢；
//   - (c) 图表增强：均值参考线（虚线，样式在 index.html 的 CSS 里）、均值/峰值标签、
//     柱子悬停类名与单桶明细（<title> 含 prompt/completion/合计/请求数/时间），且仍是
//     --chart-1/--chart-2 平色（本 fork 不搬上游渐变：那需要 style="stop-color:…"，
//     违反静态内联样式禁令）；
//   - (d) 回归：三张表列数 11/8/8（index.html 静态表头与渲染出的行都数）、空态 colspan、
//     总览统计条 6 张实卡 0 空白格、积分扣除统计条 5 张实卡 + 1 个补齐格（含命中率卡）、
//     账号表 7.10M chip（含 .usage-unit）。
//
// 表头的 data-sort 列清单直接从 index.html 里读出来建 DOM 桩，所以指示符断言是「按真实
// 页面的列序」做的，桩不会自己编一套列序。无 node 环境跳过。
func TestAppJSUsageSortAndChartRender(t *testing.T) {
	// 先确认真正下发的那份资产带着排序接线（go:embed 的就是 index.html 本身，但
	// 列数/接线这类回归历来是靠这条路径抓出来的，例如 TestUsageTablesCacheColumnCount）。
	// 这一步不需要 node，所以放在跳过判断之前。
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	served := rec.Body.String()
	for _, frag := range []string{
		`id="usAccTable"`, `id="usModelTable"`, `id="usRealmTable"`,
		`data-sort="total"`, `data-sort="cache"`, `data-sort="latency"`, `data-sort="rate"`,
		`class="sort-ind"`, `aria-sort="none"`,
	} {
		if !strings.Contains(served, frag) {
			t.Errorf("服务端下发的面板缺少 %s", frag)
		}
	}
	if n := strings.Count(served, `data-sort="`); n != 20 {
		t.Errorf("下发的页面里候选排序列应为 20 个（账号 8 + 模型 6 + 域 6），实际 %d", n)
	}
	if n := strings.Count(served, `class="sort-ind"`); n != 20 {
		t.Errorf("排序指示符 span 应为 20 个，实际 %d", n)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; usage sort/chart render test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const page = fs.readFileSync(process.argv[3], 'utf8');

// ── 静态表头：data-sort 列清单与列数都从 index.html 读，桩不另编一套列序 ──
function theadOf(cap) {
  const i = page.indexOf('<caption class="sr-only">' + cap + '</caption>');
  const j = page.indexOf('</thead>', i);
  return i < 0 || j < 0 ? '' : page.slice(i, j);
}
const CAP = { account: '用量统计：按账号', model: '用量统计：按模型', realm: '用量统计：按域' };
function sortKeysOf(dim) {
  return (theadOf(CAP[dim]).match(/data-sort="[^"]+"/g) || []).map(function (s) { return s.slice(11, -1); });
}

// ── 极简 DOM 桩：只实现被测代码用到的那几个 API ──────────────────────
function classSet(store) {
  return {
    add: function (n) { store.add(n); },
    remove: function (n) { store.delete(n); },
    contains: function (n) { return store.has(n); },
    toggle: function (n, f) {
      const on = f === undefined ? !store.has(n) : !!f;
      if (on) store.add(n); else store.delete(n);
      return on;
    },
  };
}
function mkTh(key) {
  const el = {
    dataset: { sort: key }, attrs: {}, ind: { textContent: '' },
    setAttribute: function (k, v) { el.attrs[k] = String(v); },
    getAttribute: function (k) { return Object.prototype.hasOwnProperty.call(el.attrs, k) ? el.attrs[k] : null; },
    querySelector: function (sel) { return sel === '.sort-ind' ? el.ind : null; },
    closest: function (sel) { return sel === 'th[data-sort]' ? el : null; },
  };
  el.classes = new Set();
  el.classList = classSet(el.classes);
  return el;
}
const TH_KEYS = { account: sortKeysOf('account'), model: sortKeysOf('model'), realm: sortKeysOf('realm') };
const TABLE_ID = { account: 'usAccTable', model: 'usModelTable', realm: 'usRealmTable' };
const nodes = {};
function mkTable(keys) {
  const ths = keys.map(mkTh);
  const el = { innerHTML: '', textContent: '', hidden: false, ths: ths, handlers: {},
    addEventListener: function (type, fn) { el.handlers[type] = fn; },
    querySelectorAll: function (sel) { return sel === 'th[data-sort]' ? ths : []; } };
  return el;
}
Object.keys(TABLE_ID).forEach(function (dim) { nodes[TABLE_ID[dim]] = mkTable(TH_KEYS[dim]); });
function el(id) { return nodes[id] || (nodes[id] = { innerHTML: '', textContent: '', hidden: false }); }
function thOf(dim, key) { return nodes[TABLE_ID[dim]].ths[TH_KEYS[dim].indexOf(key)]; }

// 假 fetch：记录 URL。「排序不触发新请求」的断言就是比对这份清单。
const fetchCalls = [];
const FIXTURE = {
  totals: { requests: 70, total_tokens: 348, prompt_tokens: 310, completion_tokens: 33, errors: 4,
    avg_latency_ms: 800, cache_hit_tokens: 96, cache_miss_tokens: 8 },
  // 刻意造出四种边界：零值（丁全 0）、并列（乙/丙 请求与失败都相同）、
  // 缺失（乙/丁 无延迟与速率样本、无缓存样本）、正常值。
  by_account: [
    { key: 'acct-aaa1', extra: '甲', realm: 'cn', requests: 10, errors: 0, prompt_tokens: 10, completion_tokens: 2,
      total_tokens: 12, avg_latency_ms: 900, avg_tokens_per_second: 30, cache_hit_tokens: 95, cache_miss_tokens: 5 },
    { key: 'acct-bbb2', extra: '乙', realm: 'us', requests: 30, errors: 4, prompt_tokens: 300, completion_tokens: 30,
      total_tokens: 330, avg_latency_ms: 0, avg_tokens_per_second: 0, cache_hit_tokens: 0, cache_miss_tokens: 0 },
    { key: 'acct-ccc3', extra: '丙', realm: 'cn', requests: 30, errors: 4, prompt_tokens: 5, completion_tokens: 1,
      total_tokens: 6, avg_latency_ms: 1500, avg_tokens_per_second: 12, cache_hit_tokens: 1, cache_miss_tokens: 3 },
    { key: 'acct-ddd4', extra: '丁', realm: 'us', requests: 0, errors: 0, prompt_tokens: 0, completion_tokens: 0,
      total_tokens: 0, avg_latency_ms: 0, avg_tokens_per_second: 0, cache_hit_tokens: 0, cache_miss_tokens: 0 },
  ],
  // 模型表：命中率 0%（100 miss）仍是合法可比值 → 该列可排序。
  by_model: [
    { key: 'glm-5.3', requests: 3, errors: 1, prompt_tokens: 10, completion_tokens: 2, total_tokens: 12,
      cache_hit_tokens: 0, cache_miss_tokens: 100 },
    { key: 'glm-5.2', requests: 7, errors: 0, prompt_tokens: 20, completion_tokens: 4, total_tokens: 24,
      cache_hit_tokens: 50, cache_miss_tokens: 50 },
    { key: 'glm-4.5', requests: 1, errors: 0, prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
  ],
  // 域表：整张表都没有缓存字段 → 命中率列没有任何可比行，应当不可排序。
  by_realm: [
    { key: 'cn', requests: 3, errors: 0, prompt_tokens: 10, completion_tokens: 2, total_tokens: 12 },
    { key: 'us', requests: 9, errors: 1, prompt_tokens: 30, completion_tokens: 6, total_tokens: 36 },
  ],
  // 图表：峰值在中间（不是最后一根），均值 470 < 峰值 1200。
  series: [
    { t: '2026-09-28T10', prompt_tokens: 100, completion_tokens: 50, total_tokens: 150, requests: 3 },
    { t: '2026-09-28T11', prompt_tokens: 900, completion_tokens: 300, total_tokens: 1200, requests: 9 },
    { t: '2026-09-28T12', prompt_tokens: 50, completion_tokens: 10, total_tokens: 60, requests: 2 },
  ],
  credit_by_account: [], credit_by_model: [],
};
function fakeFetch(url) {
  fetchCalls.push(String(url));
  return Promise.resolve({ ok: true, status: 200, json: function () { return Promise.resolve(FIXTURE); } });
}
async function fakeApi(ep) { const r = await fakeFetch('/panel/api/' + ep); return r.json(); }

const ctx = {
  Date: Date, Number: Number, String: String, Boolean: Boolean, Math: Math, Array: Array, Object: Object,
  JSON: JSON, RegExp: RegExp, Error: Error, isNaN: isNaN, parseInt: parseInt, parseFloat: parseFloat,
  URLSearchParams: URLSearchParams,
  $: el,
  api: fakeApi,
  trangeQuery: function () { return new URLSearchParams('hours=72'); },
  trangeLabel: function () { return '近 3 天'; },
  usageRateWarmAt: 0,
  // 请求记录的模块级状态声明在文件顶部、不在下面这几段切片里。
  reqEntries: [], reqFilter: { client_ip: '', user_agent: '', account: '', model: '', outcome: '' }, reqView: 'table',
};
// 切片：A 转义工具，B 账号表渲染，C 用量页渲染（含本批的排序），D 请求记录渲染
// （usRow 复用的 cacheRateCell 等命中率 helper 落在这一段里，缺了它 renderUsage 会
// 抛 ReferenceError——loadUsage 的 catch 会把它吞成"读取用量失败"，所以下面显式拦一次），
// E 图表（parsePointTime + renderUsageChart，本批改成真跑而不是打桩），
// F warmUsageModelRates + loadUsage（真走一次拉取，好证明排序不再拉），
// G 表头排序的委托绑定（真跑那段顶层绑定，桩表才收得到 click/keydown）。
const a = src.slice(src.indexOf('function esc('), src.indexOf('function veilStack('));
const b = src.slice(src.indexOf('function avgTTFB('), src.indexOf('async function loadOverview('));
const c = src.slice(src.indexOf('function fmtTok('), src.indexOf('function parsePointTime'));
const d = src.slice(src.indexOf('function renderRequestMetrics'), src.indexOf("$('btnLogPin')"));
const e = src.slice(src.indexOf('function parsePointTime'), src.indexOf('function fmtTokTip'));
const f = src.slice(src.indexOf('async function warmUsageModelRates'), src.indexOf("if ($('btnUsage'))"));
const g = src.slice(src.indexOf('// 明细表表头排序'), src.indexOf('/* ── 积分构成'));
if (!a || !b || !c || !d || !e || !f || !g) throw new Error('usage render slice not found in app.js');
vm.createContext(ctx);
vm.runInContext(a + '\n' + b + '\n' + c + '\n' + d + '\n' + e + '\n' + f + '\n' + g +
  '\nthis.renderUsage = renderUsage; this.usSortRows = usSortRows; this.usHeadKey = usHeadKey;' +
  '\nthis.loadUsage = loadUsage; this.renderUsageChart = renderUsageChart;' +
  '\nthis.renderAccounts = renderAccounts; this.tokenChipHTML = tokenChipHTML;', ctx);

const flat = {};
function put(k, v) { flat[k] = String(v); }
function labels(rows) { return rows.map(function (r) { return r.extra || r.key; }).join(','); }
function cellNames(h) { return (h.match(/<td>([^<]*)/g) || []).map(function (s) { return s.slice(4); }).join(','); }
function countOf(s, needle) { return s.split(needle).length - 1; }
function firstRow(h) { return (h.match(/<tr>[\s\S]*?<\/tr>/) || [''])[0]; }
function headState(dim) {
  return TH_KEYS[dim].map(function (k) {
    const th = thOf(dim, k);
    return k + '(sortable=' + (th.classList.contains('sortable') ? 'y' : 'n') +
      ',sorted=' + (th.classList.contains('sorted') ? 'y' : 'n') +
      ',ind=' + (th.querySelector('.sort-ind').textContent || '-') +
      ',aria=' + (th.getAttribute('aria-sort') || '-') + ')';
  }).join(' ');
}
function clickHead(dim, key) {
  const th = thOf(dim, key);
  nodes[TABLE_ID[dim]].handlers.click({
    target: { closest: function (sel) { return sel === 'th[data-sort]' ? th : null; } },
  });
}

(async function () {
  // ── (a) 排序纯函数 ───────────────────────────────────────────────
  const acc = FIXTURE.by_account;
  const before = acc.map(function (r) { return r.key; }).join(',');
  ['requests', 'errors', 'prompt', 'completion', 'total', 'cache', 'latency', 'rate'].forEach(function (key) {
    put('pure.' + key + '_desc', labels(ctx.usSortRows(acc, key, 'desc')));
    put('pure.' + key + '_asc', labels(ctx.usSortRows(acc, key, 'asc')));
  });
  put('pure.none', labels(ctx.usSortRows(acc, null, 'desc')));
  put('pure.unknown', labels(ctx.usSortRows(acc, 'nope', 'asc')));
  put('pure.inputIntact', acc.map(function (r) { return r.key; }).join(',') === before ? 'yes' : 'no');

  // ── 首屏：真走 loadUsage（假 fetch 记 URL）→ renderUsage → 三表 + 图表 ──
  await ctx.loadUsage();
  // loadUsage 的 catch 会把渲染期的异常吞成这一行文案；显式拦一次，否则失败会以
  // "某个 body 是 undefined" 的形式在下面几行才暴露。
  if (nodes.usChart.innerHTML.indexOf('读取用量失败') >= 0) {
    throw new Error('loadUsage 吞掉了渲染异常：' + nodes.usChart.innerHTML);
  }
  put('fetch.afterLoad', fetchCalls.join(' '));
  put('rows.init.account', cellNames(nodes.usAccBody.innerHTML));
  put('rows.init.model', cellNames(nodes.usModelBody.innerHTML));
  put('rows.init.realm', cellNames(nodes.usRealmBody.innerHTML));
  put('raw.init.account', firstRow(nodes.usAccBody.innerHTML));
  put('raw.init.model', firstRow(nodes.usModelBody.innerHTML));
  put('raw.init.realm', firstRow(nodes.usRealmBody.innerHTML));
  put('head.init.account', headState('account'));
  put('head.init.model', headState('model'));
  put('head.init.realm', headState('realm'));
  put('td.account', countOf(firstRow(nodes.usAccBody.innerHTML), '<td'));
  put('td.model', countOf(firstRow(nodes.usModelBody.innerHTML), '<td'));
  put('td.realm', countOf(firstRow(nodes.usRealmBody.innerHTML), '<td'));
  put('stats.cards', countOf(nodes.usStats.innerHTML, 'class="stat'));
  put('stats.blanks', countOf(nodes.usStats.innerHTML, 'class="stat blank"'));
  put('stats.credit.cards', countOf(nodes.usCreditStats.innerHTML, 'class="stat'));
  put('stats.credit.blanks', countOf(nodes.usCreditStats.innerHTML, 'class="stat blank"'));
  put('stats.credit.hasCache', nodes.usCreditStats.innerHTML.indexOf('缓存命中率') >= 0 ? 'yes' : 'no');
  put('chart.svg', nodes.usChart.innerHTML);

  // ── (b) 表头点击 ─────────────────────────────────────────────────
  clickHead('account', 'total');
  put('rows.totalDesc.account', cellNames(nodes.usAccBody.innerHTML));
  put('head.totalDesc.account', headState('account'));
  clickHead('account', 'total');                       // 同一列再点 → 升序
  put('rows.totalAsc.account', cellNames(nodes.usAccBody.innerHTML));
  put('head.totalAsc.account', headState('account'));
  clickHead('account', 'cache');                       // 换列 → 回到降序，旧列复位
  put('rows.cacheDesc.account', cellNames(nodes.usAccBody.innerHTML));
  put('head.cacheDesc.account', headState('account'));
  clickHead('model', 'total');                         // 另一张表：状态独立
  put('rows.modelTotalDesc.model', cellNames(nodes.usModelBody.innerHTML));
  put('rows.modelTotalDesc.account', cellNames(nodes.usAccBody.innerHTML));
  clickHead('realm', 'cache');                         // 不可排序列：点了不做事
  put('rows.realmCacheClick.realm', cellNames(nodes.usRealmBody.innerHTML));
  put('head.realmCacheClick.realm', headState('realm'));

  // 键盘：空格等价于点击，且要挡掉默认滚动
  const thReq = thOf('account', 'requests');
  nodes.usAccTable.handlers.keydown({
    key: ' ', preventDefault: function () { put('kbd.prevented', 'yes'); },
    target: { closest: function (sel) { return sel === 'th[data-sort]' ? thReq : null; } },
  });
  put('rows.kbdSpace.account', cellNames(nodes.usAccBody.innerHTML));
  put('raw.sorted.account', firstRow(nodes.usAccBody.innerHTML));
  put('raw.sorted.model', firstRow(nodes.usModelBody.innerHTML));
  put('raw.sorted.realm', firstRow(nodes.usRealmBody.innerHTML));

  // 排序全程不得新增请求（放在刷新之前取，刷新本来就会重新拉一次）
  put('fetch.afterClicks', fetchCalls.join(' '));

  // 排序后再刷新（重新拉取一次）：排序状态不丢——点了「请求降序」再刷新，看到的还是
  // 降序，而不是悄悄弹回后端顺序。倍率回填有 10 分钟节流，所以这次只多一条 usage。
  await ctx.loadUsage();
  put('rows.refetch.account', cellNames(nodes.usAccBody.innerHTML));
  put('head.refetch.account', headState('account'));
  put('fetch.afterRefetch', fetchCalls.join(' '));

  // ── (c) 图表 ─────────────────────────────────────────────────────
  ctx.renderUsageChart([{ t: '2026-09-28T10', prompt_tokens: 5, completion_tokens: 5, total_tokens: 10, requests: 1 }]);
  put('chart.single', nodes.usChart.innerHTML);        // 只有一个点：均值线与顶框重合，不画
  ctx.renderUsageChart([]);
  put('chart.empty', nodes.usChart.innerHTML);

  // ── (d) 回归 ─────────────────────────────────────────────────────
  ctx.renderUsage({ totals: {}, by_account: [], by_model: [], by_realm: [], series: [] });
  put('empty.account', nodes.usAccBody.innerHTML);
  put('empty.model', nodes.usModelBody.innerHTML);
  put('empty.realm', nodes.usRealmBody.innerHTML);
  put('empty.stats.cards', countOf(nodes.usStats.innerHTML, 'class="stat'));
  put('empty.stats.blanks', countOf(nodes.usStats.innerHTML, 'class="stat blank"'));
  ctx.renderAccounts([{
    uid: 'uid-0000000000000001', nickname: '号一', credits: 10, credits_total: 100,
    last_success: '2026-09-28T13:00:00Z',
    today: { day: '2026-09-28', requests: 1771, errors: 3, total_tokens: 7100000 },
    token_usage: { request_count: 1771, ok_count: 1768, total_tokens: 18700000, last_latency_ms: 1500 },
  }]);
  put('chip', ctx.tokenChipHTML('7.10M'));

  // ── 静态页：列数 / data-sort 清单 / 指示符 / CSS ──────────────────
  ['account', 'model', 'realm'].forEach(function (dim) {
    const head = theadOf(CAP[dim]);
    put('static.' + dim + '.th', (head.match(/<th[ >]/g) || []).length);
    put('static.' + dim + '.sort', countOf(head, 'data-sort="'));
    put('static.' + dim + '.ind', countOf(head, 'class="sort-ind"'));
  });
  put('static.tableIds', ['usAccTable', 'usModelTable', 'usRealmTable'].every(function (id) {
    return page.indexOf('id="' + id + '"') >= 0;
  }) ? 'yes' : 'no');
  put('th.account', TH_KEYS.account.join(','));
  put('th.model', TH_KEYS.model.join(','));
  put('th.realm', TH_KEYS.realm.join(','));
  put('css.avg', (page.match(/\.uschart-body \.avg \{[^}]*\}/) || [''])[0]);
  put('css.tkAvg', (page.match(/\.uschart-body \.tk-avg \{[^}]*\}/) || [''])[0]);
  put('css.tkPeak', (page.match(/\.uschart-body \.tk-peak \{[^}]*\}/) || [''])[0]);
  put('css.hover', (page.match(/\.uschart-body g\.usbar-g:hover rect\.usbar \{[^}]*\}/) || [''])[0]);
  put('css.sortable', (page.match(/#view-usage th\.sortable \{[^}]*\}/) || [''])[0]);
  put('css.sorted', (page.match(/#view-usage th\.sorted \{[^}]*\}/) || [''])[0]);

  console.log(JSON.stringify(flat));
})().catch(function (e) { console.log('HARNESS FAIL: ' + (e && e.stack ? e.stack : e)); process.exit(1); });`
	f, err := os.CreateTemp(t.TempDir(), "usage-sort-chart-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js", "index.html").CombinedOutput()
	if err != nil {
		t.Fatalf("usage sort/chart node test failed: %v\n%s", err, out)
	}
	got := map[string]string{}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("usage sort/chart output is not JSON: %v\n%s", err, out)
	}

	accCols := []string{"requests", "errors", "prompt", "completion", "total", "cache", "latency", "rate"}
	modelCols := []string{"requests", "errors", "prompt", "completion", "total", "cache"}
	// headWant 拼表头状态串：active 列给对应字符与 aria-sort，dead 里的列整列没有可比
	// 值（不可排），其余候选列是未排序的 ⇅。
	headWant := func(cols []string, active, dir string, dead ...string) string {
		deadSet := map[string]bool{}
		for _, d := range dead {
			deadSet[d] = true
		}
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			switch {
			case deadSet[c]:
				parts = append(parts, c+"(sortable=n,sorted=n,ind=-,aria=none)")
			case c == active && dir == "asc":
				parts = append(parts, c+"(sortable=y,sorted=y,ind=▲,aria=ascending)")
			case c == active:
				parts = append(parts, c+"(sortable=y,sorted=y,ind=▼,aria=descending)")
			default:
				parts = append(parts, c+"(sortable=y,sorted=n,ind=⇅,aria=none)")
			}
		}
		return strings.Join(parts, " ")
	}

	want := map[string]string{
		// 静态表头：列数与 data-sort 候选列（列序即真实页面的列序，指示符断言据此比对）。
		"static.account.th": "11", "static.account.sort": "8", "static.account.ind": "8",
		"static.model.th": "8", "static.model.sort": "6", "static.model.ind": "6",
		"static.realm.th": "8", "static.realm.sort": "6", "static.realm.ind": "6",
		"static.tableIds": "yes",
		"th.account":      strings.Join(accCols, ","),
		"th.model":        strings.Join(modelCols, ","),
		"th.realm":        strings.Join(modelCols, ","),
		// 表头初始态：全部候选列 ⇅/none，没有列被标成已排序。
		"head.init.account": headWant(accCols, "", ""),
		"head.init.model":   headWant(modelCols, "", ""),
		"head.init.realm":   headWant(modelCols, "", "", "cache"),
		// 首屏顺序 = 后端顺序（没点过表头不重排）。
		"rows.init.account": "acct-aaa,acct-bbb,acct-ccc,acct-ddd",
		"rows.init.model":   "glm-5.3,glm-5.2,glm-4.5",
		"rows.init.realm":   "cn,us",
		// 点「合计」：首次降序（330 的乙在前），再点升序（0 的丁在前）。
		"rows.totalDesc.account": "acct-bbb,acct-aaa,acct-ccc,acct-ddd",
		"head.totalDesc.account": headWant(accCols, "total", "desc"),
		"rows.totalAsc.account":  "acct-ddd,acct-ccc,acct-aaa,acct-bbb",
		"head.totalAsc.account":  headWant(accCols, "total", "asc"),
		// 换列点命中率：新列回到降序（无样本的乙/丁沉底），旧列复位成 ⇅。
		"rows.cacheDesc.account": "acct-aaa,acct-ccc,acct-bbb,acct-ddd",
		"head.cacheDesc.account": headWant(accCols, "cache", "desc"),
		// 三张表状态互相独立：排模型表不影响已排好的账号表。
		"rows.modelTotalDesc.model":   "glm-5.2,glm-5.3,glm-4.5",
		"rows.modelTotalDesc.account": "acct-aaa,acct-ccc,acct-bbb,acct-ddd",
		// 域表整列没有缓存样本 → 不可排序列点了不做事（顺序与 aria 都不变）。
		"rows.realmCacheClick.realm": "cn,us",
		"head.realmCacheClick.realm": headWant(modelCols, "", "", "cache"),
		// 键盘空格等价于点击：请求数降序（并列的乙/丙保持后端顺序）。
		"kbd.prevented":         "yes",
		"rows.kbdSpace.account": "acct-bbb,acct-ccc,acct-aaa,acct-ddd",
		// 刷新（重新拉取）后排序状态仍在，且带箭头的是同一列。
		"rows.refetch.account": "acct-bbb,acct-ccc,acct-aaa,acct-ddd",
		"head.refetch.account": headWant(accCols, "requests", "desc"),
		// 点表头全程不新增请求：URL 清单与首次加载逐字相同（只有刷新才多一条 usage）。
		"fetch.afterClicks":  "/panel/api/models /panel/api/usage?hours=72",
		"fetch.afterLoad":    "/panel/api/models /panel/api/usage?hours=72",
		"fetch.afterRefetch": "/panel/api/models /panel/api/usage?hours=72 /panel/api/usage?hours=72",
		// 渲染出的行与表头同列数（排序不会掉列）。
		"td.account": "11", "td.model": "8", "td.realm": "8",
		// 统计条：总览 6 张实卡 0 空白格；积分扣除 5 张实卡 + 1 个补齐格（含命中率卡）。
		"stats.cards": "6", "stats.blanks": "0",
		"stats.credit.cards": "6", "stats.credit.blanks": "1", "stats.credit.hasCache": "yes",
		"empty.stats.cards": "6", "empty.stats.blanks": "0",
		// chip 回归：7.10M 的 M 单独包 .usage-unit。
		"chip": `7.10<span class="usage-unit">M</span>`,
	}
	for key, w := range want {
		if got[key] != w {
			t.Errorf("%s = %q\nwant %q", key, got[key], w)
		}
	}

	// (a) 纯函数：8 个可排序列 × 升降序。甲乙丙丁 = 后端顺序，括号里是取值。
	wantPure := map[string]string{
		// 请求数：30/30 并列（乙在丙前）；0（丁）是合法值，升序排最前。
		"pure.requests_desc": "乙,丙,甲,丁",
		"pure.requests_asc":  "丁,甲,乙,丙",
		// 失败数：4/4 与 0/0 两组并列，两个方向都保持后端顺序（甲在丁前）。
		"pure.errors_desc":     "乙,丙,甲,丁",
		"pure.errors_asc":      "甲,丁,乙,丙",
		"pure.prompt_desc":     "乙,甲,丙,丁",
		"pure.prompt_asc":      "丁,丙,甲,乙",
		"pure.completion_desc": "乙,甲,丙,丁",
		"pure.completion_asc":  "丁,丙,甲,乙",
		// 合计 Token 是默认口径：330 > 12 > 6 > 0。
		"pure.total_desc": "乙,甲,丙,丁",
		"pure.total_asc":  "丁,丙,甲,乙",
		// 命中率：甲 95% > 丙 25%；乙/丁 没有缓存样本，升降序都沉底。
		"pure.cache_desc": "甲,丙,乙,丁",
		"pure.cache_asc":  "丙,甲,乙,丁",
		// 延迟 / 速率：0 与缺字段都算「无样本」，同样沉底。
		"pure.latency_desc": "丙,甲,乙,丁",
		"pure.latency_asc":  "甲,丙,乙,丁",
		"pure.rate_desc":    "甲,丙,乙,丁",
		"pure.rate_asc":     "丙,甲,乙,丁",
		// 未排序 / 未知列：逐字保持后端顺序（首屏观感不变）。
		"pure.none":    "甲,乙,丙,丁",
		"pure.unknown": "甲,乙,丙,丁",
		// 纯函数：入参数组不得被就地重排。
		"pure.inputIntact": "yes",
	}
	for key, w := range wantPure {
		if got[key] != w {
			t.Errorf("%s = %q\nwant %q", key, got[key], w)
		}
	}

	// (c) 图表：均值线 + 均值/峰值标签 + 悬停类名 + 单桶明细。
	svg := got["chart.svg"]
	avg := (150 + 1200 + 60) / 3
	for _, frag := range []string{
		`<line class="avg"`,              // 均值参考线（虚线样式在 CSS 里）
		`<text class="tk-avg"`,           // 均值标签
		">均值 " + strconv.Itoa(avg) + "<", // 470
		`<text class="tk-peak"`,          // 峰值标注
		">峰值 1.2k<",                      // 最高桶 1200
		`<g class="usbar-g">`,
		`<rect class="usbar"`,                            // 悬停高亮挂在这个类上
		`fill="var(--chart-1)"`, `fill="var(--chart-2)"`, // 仍是本 fork 的平色 token
		`<title>2026-09-28T11  900 prompt / 300 completion / 合计 1.2k / 9 次</title>`,
		`<title>2026-09-28T10  100 prompt / 50 completion / 合计 150 / 3 次</title>`,
	} {
		if !strings.Contains(svg, frag) {
			t.Errorf("图表缺少片段 %q\n实际：%s", frag, svg)
		}
	}
	if n := strings.Count(svg, `<g class="usbar-g">`); n != 3 {
		t.Errorf("单桶分组应为 3 个，实际 %d：%s", n, svg)
	}
	if n := strings.Count(svg, `<rect class="usbar"`); n != 6 {
		t.Errorf("柱子（prompt+completion 两段 × 3 桶）应为 6 段，实际 %d：%s", n, svg)
	}
	if strings.Contains(svg, "linearGradient") {
		t.Errorf("图表不该用渐变（本 fork 用 --chart-1/2 平色，渐变要 style=… 会破内联样式禁令）：%s", svg)
	}
	// 只有一个点（或所有桶等高）时均值线与顶框重合，不画线也不给标签。
	single := got["chart.single"]
	if strings.Contains(single, `class="avg"`) || strings.Contains(single, "tk-avg") {
		t.Errorf("均值等于峰值时不该画均值线：%s", single)
	}
	if !strings.Contains(single, ">峰值 10<") {
		t.Errorf("单点时峰值标注仍应在：%s", single)
	}
	if !strings.Contains(got["chart.empty"], "暂无用量数据") {
		t.Errorf("空序列应回空态文案：%s", got["chart.empty"])
	}
	// CSS：虚线靠 CSS 表达（不许内联 style），悬停规则只改不透明度、绝不碰 height
	// （SVG2 里 height 是 rect 的 CSS 几何属性，账号池的 .bar{height:4px} 曾把柱子压扁）。
	for key, frag := range map[string]string{
		"css.avg":      "stroke-dasharray: 4 4",
		"css.tkAvg":    "fill: var(--warn)",
		"css.tkPeak":   "font: 600 10px var(--mono)",
		"css.hover":    "opacity: .8",
		"css.sortable": "cursor: pointer",
		"css.sorted":   "color: var(--accent)",
	} {
		if !strings.Contains(got[key], frag) {
			t.Errorf("%s 缺少 %q：%q", key, frag, got[key])
		}
	}
	if strings.Contains(got["css.hover"], "height") {
		t.Errorf("柱子悬停规则不得设置 height（会压扁 rect）：%q", got["css.hover"])
	}

	// (d) 空态 colspan 与静态表头一致。
	for key, cols := range map[string]string{"empty.account": "11", "empty.model": "8", "empty.realm": "8"} {
		if !strings.Contains(got[key], `colspan="`+cols+`" class="empty">暂无数据`) {
			t.Errorf("%s 空态 colspan 应为 %s：%s", key, cols, got[key])
		}
	}

	// 关键渲染结果落日志，便于人眼核对（go test -v）。
	t.Logf("排序前：账号=%s 模型=%s 域=%s", got["rows.init.account"], got["rows.init.model"], got["rows.init.realm"])
	t.Logf("排序前首行：账号 %s", clipLine(got["raw.init.account"], "<tr>", 240))
	t.Logf("排序前首行：模型 %s", clipLine(got["raw.init.model"], "<tr>", 240))
	t.Logf("排序前首行：域 %s", clipLine(got["raw.init.realm"], "<tr>", 240))
	t.Logf("合计降序：%s（表头 %s）", got["rows.totalDesc.account"], got["head.totalDesc.account"])
	t.Logf("合计升序：%s", got["rows.totalAsc.account"])
	t.Logf("命中率降序：%s（旧列复位：%s）", got["rows.cacheDesc.account"], got["head.cacheDesc.account"])
	t.Logf("域表命中率不可排：顺序=%s 表头=%s", got["rows.realmCacheClick.realm"], got["head.realmCacheClick.realm"])
	t.Logf("排序后首行：账号 %s", clipLine(got["raw.sorted.account"], "<tr>", 240))
	t.Logf("排序后首行：模型 %s", clipLine(got["raw.sorted.model"], "<tr>", 240))
	t.Logf("排序后首行：域 %s", clipLine(got["raw.sorted.realm"], "<tr>", 240))
	t.Logf("请求清单：加载后=%s 排序后=%s 刷新后=%s", got["fetch.afterLoad"], got["fetch.afterClicks"], got["fetch.afterRefetch"])
	t.Logf("图表均值线：%s", clipLine(svg, `<line class="avg"`, 130))
	t.Logf("图表均值标签：%s", clipLine(svg, ">均值 ", 130))
	t.Logf("图表峰值标注：%s", clipLine(svg, ">峰值 ", 130))
	t.Logf("单桶明细：%s", clipLine(svg, "<title>2026-09-28T11", 130))
	t.Logf("行内单元格数：账号=%s 模型=%s 域=%s；统计卡 %s 张/%s 空白格；chip=%s",
		got["td.account"], got["td.model"], got["td.realm"], got["stats.cards"], got["stats.blanks"], got["chip"])
}

// clipLine 截取 s 中 needle 开始的至多 max 个字符，仅供测试日志人眼核对。
func clipLine(s, needle string, max int) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return "（缺 " + needle + "）"
	}
	rest := s[i:]
	if len(rest) > max {
		rest = rest[:max] + "…"
	}
	return rest
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
	if !strings.Contains(got.Accounts, `<span class="usage-item usage-total"><b>7.10<span class="usage-unit">M</span></b></span>`) {
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

/* ══════════════════════════════════════════════════════════════════════════
   请求记录表格（表格 / 行式切换 + 筛选栏）
   ══════════════════════════════════════════════════════════════════════════ */

// TestIndexRequestTableView index.html 侧的表格骨架必须真的在页面里：视图分段
// 控件、筛选栏的每个控件、10 列表头与 tbody、行式容器（默认隐藏）。
// app.js 侧全是 `if ($('reqIP')) …` 的守卫写法——宿主被删掉时 JS 不报错、测试
// 全绿，只是筛选栏人间蒸发，故这里逐个钉住。
func TestIndexRequestTableView(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	// 筛选栏：四个文字筛选 + 结果 + 时间范围 + 条数 + 计数 + 重新读取，全在 .fbar 内。
	const fbarStart = `<div class="fbar" id="reqFilterBar">`
	const tblStart = `<div class="tbl-wrap req-wrap" id="reqTableWrap">`
	i, j := strings.Index(body, fbarStart), strings.Index(body, tblStart)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("index.html 缺少筛选栏或表格容器：fbar=%d table=%d", i, j)
	}
	fbar := body[i:j]
	for _, want := range []string{
		`id="reqIP"`, `id="reqUA"`, `id="reqAccount"`, `id="reqModel"`,
		`<select id="reqOutcome" aria-label="按结果筛选">`,
		`<span class="trange" id="reqRange"></span>`,
		`<select id="reqLimit" aria-label="读取条数">`,
		`<span class="note" id="reqCount">—</span>`,
		`<button class="xs" id="btnReqReload">重新读取</button>`,
	} {
		if !strings.Contains(fbar, want) {
			t.Errorf("筛选栏缺少控件 %s\n实际：%s", want, fbar)
		}
	}
	// 结果下拉的取值必须与后端 reqlog 的 outcome 字面量一一对应（不发明取值）。
	for _, want := range []string{
		`<option value="">全部结果</option>`,
		`<option value="success">成功</option>`,
		`<option value="http_error">HTTP 错误</option>`,
		`<option value="stream_error">流错误</option>`,
		`<option value="interrupted">中断</option>`,
	} {
		if !strings.Contains(fbar, want) {
			t.Errorf("结果筛选缺少选项 %s", want)
		}
	}
	// 条数四档，默认 100（与移植前 request_logs?limit=100 的默认一致）。
	for _, want := range []string{
		`<option value="50">最近 50 条</option>`,
		`<option value="100" selected>最近 100 条</option>`,
		`<option value="200">最近 200 条</option>`,
		`<option value="500">最近 500 条</option>`,
	} {
		if !strings.Contains(fbar, want) {
			t.Errorf("条数下拉缺少选项 %s", want)
		}
	}

	// 视图切换与两个容器：默认表格（表格 chip 带 on、行式容器 hidden）。
	for _, want := range []string{
		`<span class="chips" id="reqViews" role="group" aria-label="请求记录视图">`,
		`<button class="xs chip on" data-rv="table">表格</button>`,
		`<button class="xs chip" data-rv="line">行式</button>`,
		`<table class="acc req">`,
		`<tbody id="reqBody"></tbody>`,
		`<pre id="reqLogBox" hidden></pre>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html 缺少请求记录视图元素：%s", want)
		}
	}

	// 表头 10 列：与 app.js 的行内 <td> 数、空态 colspan 三处一致（列数对不上整行错位）。
	head := body[i:strings.Index(body, `<tbody id="reqBody">`)]
	if n := strings.Count(head, "</th>"); n != 10 {
		t.Errorf("请求记录表头应为 10 列，实际 %d", n)
	}
	for _, want := range []string{
		`<th>来源 IP</th>`, `<th>User-Agent</th>`,
		`<th class="num">耗时</th>`, `<th class="num">Token</th>`, `<th class="num">积分</th>`,
		`<th>请求 ID</th>`,
	} {
		if !strings.Contains(head, want) {
			t.Errorf("请求记录表头缺少列 %s", want)
		}
	}

	// 样式：筛选栏 / 表格 / 截断列 / 计数条琥珀，全部走既有 token 与类名体系。
	for _, want := range []string{
		".fbar {", ".fbar > input, .fbar > select", ".acc.req",
		".req-wrap { max-height: 460px; }", ".clip {", ".clip.ip", ".clip.rid",
		".acc.req .muted", ".req-hit {", ".src-off {",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html 缺少请求记录表样式：%s", want)
		}
	}
}

// reqPanelStubJS 是「请求记录表格 / 筛选」用例共用的 node 沙箱骨架（沙箱写法与
// TestAppJSTopLevelSmoke / TestAppJSTrangeRequests 同款）：
//   - DOM 桩按 id 缓存元素，支持 innerHTML / textContent / value / className / hidden /
//     dataset / classList，addEventListener 把 handler 记进 __h——用例可以像真实点击
//     那样派发事件，而不是直接改内部状态；
//   - 假 fetch 记录每条请求路径（含查询串）；REQ_LOGS_FAIL=1 时 request_logs 直接
//     reject，用来走「归档关闭 → 回落内存最近请求」这条唯一需要前端兜底筛选的路径；
//   - localStorage 桩记录写入（断言视图选择持久化）；REQ_VIEW_PRE 预置初值，验证
//     「下次打开记住行式」；
//   - 时钟钉死（2026-09-30T14:30 本地），断言才能落在具体的 from/to 秒数与 14:05:06 上。
//
// 场景脚本接在骨架之后，用骨架里的 runScenario(async () => {…}) 返回待断言的 JSON。
const reqPanelStubJS = `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');

// 假后端：fetched 是 request_logs 回的批次（用例改写），recent 是内存指标里的
// 「最近请求」（归档关闭时前端回落到它）。
let fetched = [];
let recent = [];
let reqLogsFail = process.env.REQ_LOGS_FAIL === '1';
let logsFail = false;
let slow = false;   // 拉取期间才有机会观察加载态（默认 fetch 立刻 resolve）
const urls = [];
const body = p => {
  if (p.startsWith('logs')) return { entries: [] };
  if (p.startsWith('request_metrics')) return { completed: 2, success_rate: 100, http_success_rate: 100,
    avg_duration_ms: 1250, in_flight: 0, archive: { enabled: !reqLogsFail, bytes: 2048 }, recent };
  if (p.startsWith('request_logs')) return { entries: fetched, limit: 100 };
  return {};
};

// localStorage 桩：记下写入的键值；REQ_VIEW_PRE 预置「上次的选择」。
const lsStore = {};
if (process.env.REQ_VIEW_PRE) lsStore['wb2api.reqview'] = process.env.REQ_VIEW_PRE;

// DOM 桩：元素是 Proxy，未实现的成员回落到 inert（跑通整份 app.js 不必实现所有 DOM 方法）。
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; }, apply() { return inert; }, construct() { return inert; }, has() { return true; },
});
const nodes = {}, qcache = {};
const mkEl = key => {
  const classes = new Set();
  const handlers = {};
  const store = {
    key, innerHTML: '', textContent: '', value: '', title: '', hidden: false, disabled: false,
    className: '', dataset: {}, style: {}, children: [], selectedOptions: [], __h: handlers,
    scrollTop: 0, scrollHeight: 0, clientHeight: 0,
    classList: {
      add: c => classes.add(c), remove: c => classes.delete(c),
      toggle: (c, on) => { const w = on === undefined ? !classes.has(c) : !!on; if (w) classes.add(c); else classes.delete(c); return w; },
      contains: c => classes.has(c),
    },
    classes,
    setAttribute() {}, getAttribute: () => null, removeAttribute() {}, hasAttribute: () => false,
    addEventListener(t, fn) { handlers[t] = fn; }, removeEventListener() {},
    appendChild(n) { store.children.push(n); return n; }, remove() {}, focus() {}, blur() {}, click() {},
    closest: () => null, contains: () => false, insertAdjacentHTML() {},
    getElementsByTagName: () => [], querySelectorAll: () => [],
    querySelector: sel => (qcache[key + '|' + sel] = qcache[key + '|' + sel] || mkEl(key + '|' + sel)),
    get firstElementChild() { return store.children[0] || null; },
  };
  return new Proxy(store, { get(t, k) { return k in t ? t[k] : inert; }, set(t, k, v) { t[k] = v; return true; }, has: () => true });
};
const el = id => (nodes[id] = nodes[id] || mkEl(id));
const chipTable = mkEl('chip-table'), chipLine = mkEl('chip-line');
chipTable.dataset.rv = 'table'; chipLine.dataset.rv = 'line';

const RealDate = Date;
const FIXED = RealDate.parse('2026-09-30T14:30:00');
class FakeDate extends RealDate {
  constructor(...a) { if (a.length) { super(...a); } else { super(FIXED); } }
  static now() { return FIXED; }
}
const sandbox = {
  location: { hash: '#logs' },   // 深链：顶层 go() 会同步调进 loadLogs
  history: { replaceState() {} },
  localStorage: {
    getItem: k => (k in lsStore ? lsStore[k] : null),
    setItem: (k, v) => { lsStore[k] = String(v); },
    removeItem: k => { delete lsStore[k]; },
  },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: {
    getElementById: el,
    querySelectorAll: sel => (sel === '#reqViews .chip' ? [chipTable, chipLine] : []),
    querySelector: () => inert, addEventListener() {},
    documentElement: el('documentElement'), head: inert, body: inert, cookie: '',
    createElement: () => mkEl('created'), contains: () => false, activeElement: inert,
  },
  fetch: url => {
    const path = String(url).replace('/panel/api/', '');
    urls.push(path);
    if (logsFail && path.startsWith('logs')) return Promise.reject(new Error('日志接口挂了'));
    if (reqLogsFail && path.startsWith('request_logs')) return Promise.reject(new Error('archive off'));
    if (slow) return new Promise(r => setTimeout(() => r({ status: 200, ok: true, json: () => Promise.resolve(body(path)) }), 40));
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
  '\nthis.loadLogs=loadLogs; this.renderAccounts=renderAccounts; this.renderModelLocks=renderModelLocks;' +
  '\nthis.renderRequestMetrics=renderRequestMetrics; this.reqSetView=reqSetView; this.reqSyncFilter=reqSyncFilter;' +
  '\nthis.reqFilter=reqFilter; this.reqQuery=reqQuery;',
  sandbox, { filename: 'app.js' });

const tick = () => new Promise(r => setTimeout(r, 5));
const sleep = ms => new Promise(r => setTimeout(r, ms));
const reqUrls = () => urls.filter(u => u.startsWith('request_logs'));
const lastReq = () => reqUrls().slice(-1)[0] || '';
const setPreset = (id, v) => { const s = el(id).querySelector('.tr-preset'); s.value = v; s.onchange(); };
// 走控件自己的 click 事件（不是直接改状态）：分段控件有没有真的接上切换函数，
// 只有这条路径能证明。
const clickView = rv => el('reqViews').__h.click({ target: { closest: () => ({ dataset: { rv } }) } });
const rowsOf = html => html.split('<tr ').slice(1).map(s => '<tr ' + s);
const cells = r => (r.replace(/^<tr[^>]*>/, '').replace(/<\/tr>$/, '').match(/<td[^>]*>.*?<\/td>/g) || []);
const chipOn = () => (chipTable.classList.contains('on') ? 'table' : '') + (chipLine.classList.contains('on') ? 'line' : '');
const runScenario = fn => fn().then(
  out => { process.stdout.write(JSON.stringify(out)); process.exit(0); },
  e => { console.log('REQ TABLE FAIL: ' + (e && e.stack ? e.stack : e)); process.exit(1); });
`

// TestAppJSRequestTableRender node + DOM 桩真实渲染请求记录表：(a) 表格行（含来源
// 缺失 / 有来源 / 缓存命中率三态）+ 空态 + 计数条；(c) 视图切换（表格 ↔ 行式，
// 行式输出与既有断言逐字一致、选择写进 localStorage、预置选择能恢复）；(d) 回归
// 红线：账号表用量列、模型锁池表、trange 控件都还在。
func TestAppJSRequestTableRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request table render test skipped")
	}
	script := reqPanelStubJS + `
runScenario(async () => {
  const out = {};
  await tick();                       // 深链 #logs 的首屏加载（默认表格 + ?limit=100）
  out.initView = chipOn();
  out.initUrl = lastReq();
  out.initTableHidden = String(el('reqTableWrap').hidden);
  out.initLineHidden = String(el('reqLogBox').hidden);
  sandbox.reqSetView('table');        // 归一化：下面的渲染断言不依赖预置的视图选择

  const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
  const base = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: '账号(uid8)',
    duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12 };
  const old = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: '账号(uid8)',
    duration_ms: 1250, total_tokens: 2300, request_id: 'req-1' };   // 老归档：无来源/无缓存/无积分观测
  const full = { ...base, request_id: 'req-2', client_ip: '203.0.113.7', user_agent: 'curl/8.4.0',
    cache_hit_tokens: 2257, cache_miss_tokens: 43 };
  const allMiss = { ...base, request_id: 'req-3', outcome: 'stream_error', status: 502,
    cache_hit_tokens: 0, cache_miss_tokens: 2000 };
  const httpErr = { ...base, request_id: 'req-4', outcome: 'http_error', status: 500, credit_known: false };
  const cut = { ...base, request_id: 'req-5', outcome: 'interrupted',
    client_ip: '<b>10.0.0.1</b>', user_agent: 'Mozilla/5.0 Chrome/1.0<b>' };

  fetched = [full, old, allMiss, httpErr, cut];
  await sandbox.loadLogs();
  const rows = rowsOf(el('reqBody').innerHTML);
  out.rowCount = String(rows.length);
  out.row1 = rows[0];
  out.row2 = rows[1];
  out.tags = rows.map(r => (r.match(/<span class="tag [a-z]+">[^<]*<\/span>/) || [''])[0]).join('|');
  out.tokenOld = cells(rows[1])[7];      // 无观测：只有总量
  out.tokenMiss = cells(rows[2])[7];     // 有观测且全未命中：0%（不是「没有样本」）
  out.tokenFull = cells(rows[0])[7];     // 有观测：命中率（复用用量明细表的分档色）
  out.srcOld = cells(rows[1])[4] + cells(rows[1])[5];   // 老归档 → —
  out.srcFull = cells(rows[0])[4] + cells(rows[0])[5];
  out.srcCut = cells(rows[4])[4] + cells(rows[4])[5];   // 客户端可控文本必须转义
  out.creditUnknown = cells(rows[3])[8];
  out.ridCell = cells(rows[0])[9];
  out.count = el('reqCount').textContent;
  out.countCls = el('reqCount').className;

  // 加载态：拉取期间表格铺既有骨架行（skeletonRows，与账号表/任务表同一写法），
  // 不是「正在查询…」一行把表格塌掉。
  fetched = [full];
  slow = true;
  const pending = sandbox.loadLogs();
  out.loadingTable = el('reqBody').innerHTML;
  await pending;
  slow = false;
  out.loadedRows = String(rowsOf(el('reqBody').innerHTML).length);

  // 空态：一整批都没有 → 「暂无请求记录」；有记录但筛掉 → 另一句文案（colspan=10）。
  fetched = [];
  await sandbox.loadLogs();
  out.emptyTable = el('reqBody').innerHTML;
  out.emptyCount = el('reqCount').textContent;

  // 整次拉取失败（logs 接口报错）：表格不能停在加载骨架上，就地收尾成失败文案。
  logsFail = true;
  await sandbox.loadLogs();
  out.errTable = el('reqBody').innerHTML;
  logsFail = false;

  // 归档关闭：request_logs 报错 → 回落内存里的最近请求；那批数据没经过服务端筛选，
  // 前端必须自己兜住（否则筛选栏看起来完全失效）。
  reqLogsFail = true;
  recent = [full, { ...old, request_id: 'req-9', client_ip: '198.51.100.9' }];
  el('reqIP').value = '198.51.100.9';
  sandbox.reqSyncFilter();
  await sandbox.loadLogs();
  out.fallbackRows = String(rowsOf(el('reqBody').innerHTML).length);
  out.fallbackCount = el('reqCount').textContent;
  out.fallbackCls = el('reqCount').className;
  el('reqIP').value = '192.0.2.1';
  sandbox.reqSyncFilter();
  await sandbox.loadLogs();
  out.missTable = el('reqBody').innerHTML;
  out.missCount = el('reqCount').textContent;
  el('reqIP').value = '';
  sandbox.reqSyncFilter();
  reqLogsFail = false;

  // 视图切换：表格 → 行式 → 表格。行式分支走移植前的 requestLogLine，逐字不变
  // （下面两行就是既有断言里的那一对：带来源与缓存的 req-2、无来源的 req-1）。
  fetched = [full, { ...base, request_id: 'req-1' }];
  await sandbox.loadLogs();
  out.tableRows = String(rowsOf(el('reqBody').innerHTML).length);
  out.tableHiddenBefore = String(el('reqTableWrap').hidden);
  clickView('line');
  out.lineRows = el('reqLogBox').innerHTML;
  out.lineHidden = String(el('reqLogBox').hidden);
  out.tableHidden = String(el('reqTableWrap').hidden);
  out.lsView = lsStore['wb2api.reqview'] || '';
  out.chipLine = chipOn();
  clickView('table');
  out.chipBack = chipOn();
  out.lineHiddenBack = String(el('reqLogBox').hidden);
  out.tableHiddenBack = String(el('reqTableWrap').hidden);
  out.lineKept = el('reqLogBox').innerHTML;

  // 回归红线：账号表用量列、模型锁池表、时间范围控件。
  sandbox.renderAccounts([{
    uid: 'uid-0000000000000001', nickname: '号一', credits: 10, credits_total: 100,
    last_success: '2026-09-28T13:00:00Z',
    today: { day: '2026-09-28', requests: 1771, errors: 3, total_tokens: 7100000 },
    token_usage: { request_count: 1771, ok_count: 1768, total_tokens: 18700000, last_latency_ms: 1500 },
  }]);
  out.accounts = el('accBody').innerHTML;
  sandbox.renderModelLocks([{ model: 'glm-5.3', realm: 'cn', total: 5, servable: 0, locked: 5, state: 'locked',
    unlock_at: new Date(FIXED + 3600000).toISOString(), fully_unlock_at: new Date(FIXED + 9000000).toISOString(),
    reason: '上游 429' }]);
  out.locks = el('mlBody').innerHTML;
  out.rangeOptions = String((el('reqRange').innerHTML.match(/<option/g) || []).length);
  return out;
});`
	f, err := os.CreateTemp(t.TempDir(), "req-table-render-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// 行式视图的两行输出：与 TestAppJSRequestLogFormatting 钉的行格式逐字相同
	// （同一批记录、同一条 requestLogLine，只是渲染进 #reqLogBox 而不是表格）。
	const lineHead = "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 1.25s | 2.3k tok | 0.12 credit"
	const lineFull = `<span class="ln">` + lineHead + ` | 命中 98.1%（2.3k tok） | src=203.0.113.7 ua=&quot;curl/8.4.0&quot; | req-2</span>`
	const lineOld = `<span class="ln">` + lineHead + ` | req-1</span>`

	for _, tc := range []struct{ name, pre, initView, initTable, initLine string }{
		{"默认表格", "", "table", "false", "true"},
		{"记住行式", "line", "line", "true", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(node, f.Name(), "app.js")
			cmd.Dir = "."
			cmd.Env = append(os.Environ(), "REQ_VIEW_PRE="+tc.pre)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("请求记录表格渲染失败: %v\n%s", err, out)
			}
			got := map[string]string{}
			if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
				t.Fatalf("请求记录表格渲染输出不是 JSON: %v\n%s", err, out)
			}
			check := func(key, want string) {
				t.Helper()
				if got[key] != want {
					t.Errorf("%s=%q\nwant %q", key, got[key], want)
				}
			}
			// 首屏：默认表格 + 默认条数 100（不放筛选条件、不发 from/to）。
			check("initView", tc.initView)
			check("initUrl", "request_logs?limit=100")
			check("initTableHidden", tc.initTable)
			check("initLineHidden", tc.initLine)

			// 表格行（逐字钉住前两行：时间/结果/模型/账号/来源/耗时/Token/积分/请求 ID）。
			check("rowCount", "5")
			check("row1", `<tr title="14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 1.25s | 2.3k tok | 0.12 credit | 命中 98.1%（2.3k tok） | src=203.0.113.7 ua=&quot;curl/8.4.0&quot; | req-2">`+
				`<td class="num">14:05:06</td><td><span class="tag ok">200 成功</span></td><td>glm-5.3</td><td>账号(uid8)</td>`+
				`<td><span class="clip ip" title="203.0.113.7">203.0.113.7</span></td>`+
				`<td><span class="clip" title="curl/8.4.0">curl/8.4.0</span></td>`+
				`<td class="num">1.25s</td>`+
				`<td class="num">2.3k<span class="req-hit"><span class="c-ok" title="命中 2.3k / 未命中 43 tok">98.1%</span></span></td>`+
				`<td class="num">0.12</td><td><span class="clip rid" title="req-2">req-2</span></td></tr>`)
			check("row2", `<tr title="14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 1.25s | 2.3k tok | credit — | req-1">`+
				`<td class="num">14:05:06</td><td><span class="tag ok">200 成功</span></td><td>glm-5.3</td><td>账号(uid8)</td>`+
				`<td><span class="muted">—</span></td><td><span class="muted">—</span></td>`+
				`<td class="num">1.25s</td><td class="num">2.3k</td>`+
				`<td class="num"><span class="muted">—</span></td><td><span class="clip rid" title="req-1">req-1</span></td></tr>`)

			// 结果列语义色：成功 ok / 流错误·HTTP 错误 bad / 中断 warn（不新起一套徽标）。
			check("tags", `<span class="tag ok">200 成功</span>|<span class="tag ok">200 成功</span>|`+
				`<span class="tag bad">502 流错误</span>|<span class="tag bad">500 HTTP 错误</span>|`+
				`<span class="tag warn">200 中断</span>`)

			// Token 列的缓存命中率三态：无观测（老归档）只有总量；全未命中显示 0%（有样本）；
			// 命中显示 98.1%——三态都复用用量明细表的 cacheRateCell。
			check("tokenOld", `<td class="num">2.3k</td>`)
			check("tokenMiss", `<td class="num">2.3k<span class="req-hit">`+
				`<span class="c-bad" title="命中 0 / 未命中 2.0k tok">0%</span></span></td>`)
			check("tokenFull", `<td class="num">2.3k<span class="req-hit">`+
				`<span class="c-ok" title="命中 2.3k / 未命中 43 tok">98.1%</span></span></td>`)

			// 来源列：老归档显示 — 而不是空白；有值时截断显示、完整值进 title；
			// 客户端可控文本（IP/UA）必须转义。
			check("srcOld", `<td><span class="muted">—</span></td><td><span class="muted">—</span></td>`)
			check("srcFull", `<td><span class="clip ip" title="203.0.113.7">203.0.113.7</span></td>`+
				`<td><span class="clip" title="curl/8.4.0">curl/8.4.0</span></td>`)
			check("srcCut", `<td><span class="clip ip" title="&lt;b&gt;10.0.0.1&lt;/b&gt;">&lt;b&gt;10.0.0.1&lt;/b&gt;</span></td>`+
				`<td><span class="clip" title="Mozilla/5.0 Chrome/1.0&lt;b&gt;">Mozilla/5.0 Chrome/1.0&lt;b&gt;</span></td>`)
			check("creditUnknown", `<td class="num"><span class="muted">—</span></td>`)
			check("ridCell", `<td><span class="clip rid" title="req-2">req-2</span></td>`)
			check("count", "5 条")
			check("countCls", "note")

			// 空态：colspan 必须等于表头列数（10），否则空表整行错位。
			// 加载态：4 行骨架（既有 skeletonRows 写法，colspan=10 铺满整行）。
			if strings.Count(got["loadingTable"], `<span class="skeleton`) != 4 ||
				!strings.Contains(got["loadingTable"], `<td colspan="10">`) {
				t.Errorf("加载态应是 4 行 colspan=10 的骨架行：%s", got["loadingTable"])
			}
			check("loadedRows", "1")
			check("emptyTable", `<tr><td colspan="10" class="empty">暂无请求记录</td></tr>`)
			check("emptyCount", "—")
			// 失败态：就地收尾，不把加载骨架留在屏幕上。
			check("errTable", `<tr><td colspan="10"><div class="empty">日志接口挂了</div></td></tr>`)
			check("fallbackRows", "1")
			check("fallbackCount", "命中 1 / 2 条")
			check("fallbackCls", "note src-off")
			check("missTable", `<tr><td colspan="10" class="empty">没有符合当前筛选条件的请求记录</td></tr>`)
			check("missCount", "命中 0 / 2 条")

			// 视图切换：两个容器互斥、分段控件选中态、选择写进 localStorage。
			check("tableRows", "2")
			check("tableHiddenBefore", "false")
			check("lineRows", lineFull+lineOld)
			check("lineHidden", "false")
			check("tableHidden", "true")
			check("lsView", "line")
			check("chipLine", "line")
			check("chipBack", "table")
			check("lineHiddenBack", "true")
			check("tableHiddenBack", "false")
			check("lineKept", lineFull+lineOld)

			// 回归红线：账号表今日用量、模型锁池表、trange 控件的 7 个预设。
			if !strings.Contains(got["accounts"], `<b>7.10<span class="usage-unit">M</span></b>`) {
				t.Errorf("账号表今日用量不再是 7.10M 的 chip：%s", got["accounts"])
			}
			if !strings.Contains(got["locks"], `<td>glm-5.3</td>`) ||
				!strings.Contains(got["locks"], `<span class="tag bad">整池不可用</span>`) {
				t.Errorf("模型锁池表渲染异常：%s", got["locks"])
			}
			check("rangeOptions", "7")
		})
	}
}

// TestAppJSRequestTableFilters (b) 筛选控件 → 实际请求 URL：逐条驱动真实控件
// （oninput / onchange / 预设切换 / 重新读取），用假 fetch 记录到的查询串断言。
// 参数名与后端 panel.requestLogs 一一对应，前端不发明参数、顺序也固定
// （时间范围 → limit → 筛选条件），默认状态逐字是移植前的 request_logs?limit=100。
func TestAppJSRequestTableFilters(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request table filter test skipped")
	}
	script := reqPanelStubJS + `
runScenario(async () => {
  const out = {};
  await tick();
  out.qOpen = lastReq();              // 深链首屏那次（默认表格 + limit=100）
  urls.length = 0;                    // 之后的断言只看控件驱动的请求
  await sandbox.loadLogs();
  out.qDefault = lastReq();
  // 文字筛选：控件输入 → 防抖 250ms → 重新拉取
  const type = async (id, v) => { el(id).value = v; el(id).oninput(); await sleep(300); return lastReq(); };
  out.qIP = await type('reqIP', '203.0.113.7');
  out.qUA = await type('reqUA', 'curl/8.4.0');
  out.qAccount = await type('reqAccount', 'uid8');
  out.qModel = await type('reqModel', 'glm-5.3');
  // 下拉不防抖：一次点击就是一次明确的查询
  el('reqOutcome').value = 'http_error'; el('reqOutcome').onchange(); await tick();
  out.qOutcome = lastReq();
  el('reqLimit').value = '500'; el('reqLimit').onchange(); await tick();
  out.qLimit = lastReq();
  // 时间范围沿用 trange 控件（预设 → from/to）
  setPreset('reqRange', 'today'); await tick();
  out.qToday = lastReq();
  setPreset('reqRange', '24'); await tick();
  out.q24 = lastReq();
  setPreset('reqRange', '0'); await tick();
  out.qAll = lastReq();
  // 防抖：连打三个字符只发一次请求（每次 loadLogs 自身是 logs + request_metrics +
  // request_logs 三个接口，故只数 request_logs）。
  const before = reqUrls().length;
  for (const v of ['1', '19', '198']) { el('reqIP').value = v; el('reqIP').oninput(); }
  await sleep(300);
  out.debounce = String(reqUrls().length - before);
  out.qDebounced = lastReq();
  // 清空一个字段：该参数从查询串里消失（空值不发，后端把空串当「不筛该字段」）
  out.qClearIP = await type('reqIP', '');
  out.qClearUA = await type('reqUA', '');
  // 重新读取：无条件重发（不依赖筛选变化）
  const n = urls.length;
  el('btnReqReload').onclick(); await tick();
  out.reloadFetched = String(urls.length > n);
  out.reloadUrl = lastReq();
  out.filterState = JSON.stringify(sandbox.reqFilter);
  out.queryString = sandbox.reqQuery().toString();
  return out;
});`
	f, err := os.CreateTemp(t.TempDir(), "req-table-filters-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("请求记录筛选 node 测试失败: %v\n%s", err, out)
	}
	got := map[string]string{}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("请求记录筛选输出不是 JSON: %v\n%s", err, out)
	}
	// 控件按顺序叠加，后面的断言用前面积累出来的查询串（顺序即参数顺序）。
	const flt = "&client_ip=203.0.113.7&user_agent=curl%2F8.4.0&account=uid8&model=glm-5.3&outcome=http_error"
	sec := func(d time.Time) string { return strconv.FormatInt(d.Unix(), 10) }
	// 沙箱时钟钉在 2026-09-30T14:30 本地：断言落在具体的 from 秒数上（否则随真实日期漂）。
	at := func(day, h, m int) time.Time { return time.Date(2026, 9, day, h, m, 0, 0, time.Local) }
	for _, tc := range []struct{ key, want string }{
		// 默认：与移植前逐字一致（全部历史不发 from/to，条数默认 100）
		{"qDefault", "request_logs?limit=100"},
		{"qOpen", "request_logs?limit=100"},
		{"qIP", "request_logs?limit=100&client_ip=203.0.113.7"},
		{"qUA", "request_logs?limit=100&client_ip=203.0.113.7&user_agent=curl%2F8.4.0"},
		{"qAccount", "request_logs?limit=100&client_ip=203.0.113.7&user_agent=curl%2F8.4.0&account=uid8"},
		{"qModel", "request_logs?limit=100&client_ip=203.0.113.7&user_agent=curl%2F8.4.0&account=uid8&model=glm-5.3"},
		{"qOutcome", "request_logs?limit=100" + flt},
		{"qLimit", "request_logs?limit=500" + flt},
		// 「今天」= 浏览器本地 00:00 起（只发 from）；滚动预设折算成 from（归档没有 hours 口径）
		{"qToday", "request_logs?from=" + sec(at(30, 0, 0)) + "&limit=500" + flt},
		{"q24", "request_logs?from=" + sec(at(29, 14, 30)) + "&limit=500" + flt},
		{"qAll", "request_logs?limit=500" + flt},
		{"debounce", "1"},
		{"qDebounced", "request_logs?limit=500&client_ip=198&user_agent=curl%2F8.4.0&account=uid8&model=glm-5.3&outcome=http_error"},
		{"qClearIP", "request_logs?limit=500&user_agent=curl%2F8.4.0&account=uid8&model=glm-5.3&outcome=http_error"},
		{"qClearUA", "request_logs?limit=500&account=uid8&model=glm-5.3&outcome=http_error"},
		{"reloadFetched", "true"},
		{"reloadUrl", "request_logs?limit=500&account=uid8&model=glm-5.3&outcome=http_error"},
		// 前端状态与发出去的查询串同源（模型/账号/结果留着，两个被清空的字段是空串）
		{"filterState", `{"client_ip":"","user_agent":"","account":"uid8","model":"glm-5.3","outcome":"http_error"}`},
		{"queryString", "limit=500&account=uid8&model=glm-5.3&outcome=http_error"},
	} {
		if got[tc.key] != tc.want {
			t.Errorf("%s=%q\nwant %q", tc.key, got[tc.key], tc.want)
		}
	}
}

// modelPanelStubJS 是「模型条件查询」用例共用的 node 沙箱骨架（沙箱写法与
// reqPanelStubJS / TestAppJSTopLevelSmoke 同款）：
//   - 真实切片 app.js 的 api / $ / esc / skeletonRows 与整个模型区段；用例只通过控件
//     自己的 oninput / onchange / onclick 驱动（接线断了这条路径就失败），不直接改状态；
//   - 假 fetch 记录每条 /panel/api/… URL：「筛选与排序零新请求」的断言就是比对这份清单；
//   - 上游目录 / 探测结果由用例改写（devModels / devProbes），断言全部落在 mdBody 的
//     innerHTML 与计数条上——那是真实渲染产物，不是内部状态。
const modelPanelStubJS = `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const apiSrc = src.slice(src.indexOf('async function api('), src.indexOf('/* toast 提示'));
const dollarSrc = src.slice(src.indexOf('const $ = id =>'), src.indexOf('/* ── 主题'));
const escSrc = src.slice(src.indexOf('function esc('), src.indexOf('function ago('));
const skelSrc = src.slice(src.indexOf('function skeletonRows('), src.indexOf('function go(v)'));
const mdStart = src.indexOf('/* ── 模型 ──');
const mdEnd = src.indexOf('/* ── 日志（频道');
if (!apiSrc || !dollarSrc || !escSrc || !skelSrc || mdStart < 0 || mdEnd <= mdStart) {
  throw new Error('模型筛选区段或依赖切片未找到');
}
const mdSrc = src.slice(mdStart, mdEnd);

let devModels = [], devProbes = {};
const urls = [];
const body = p => p.startsWith('model_probes') ? { probes: devProbes, exists: true } : { models: devModels };

// DOM 桩：元素是 Proxy，未实现的成员回落到 inert（跑通整段模型代码不必实现所有 DOM 方法）。
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; }, apply() { return inert; }, construct() { return inert; }, has() { return true; },
});
const nodes = {}, qcache = {};
const mkEl = key => {
  const classes = new Set(), handlers = {};
  const store = {
    key, innerHTML: '', textContent: '', value: '', className: '', hidden: false, dataset: {}, children: [], __h: handlers,
    classList: {
      add: c => classes.add(c), remove: c => classes.delete(c),
      toggle: (c, on) => { const w = on === undefined ? !classes.has(c) : !!on; if (w) classes.add(c); else classes.delete(c); return w; },
      contains: c => classes.has(c),
    },
    setAttribute() {}, getAttribute: () => null, addEventListener(t, fn) { handlers[t] = fn; },
    appendChild(n) { store.children.push(n); return n; }, remove() {}, closest: () => null,
    querySelector: sel => (qcache[key + '|' + sel] = qcache[key + '|' + sel] || mkEl(key + '|' + sel)),
    querySelectorAll: () => [],
  };
  return new Proxy(store, { get(t, k) { return k in t ? t[k] : inert; }, set(t, k, v) { t[k] = v; return true; }, has: () => true });
};
const el = id => (nodes[id] = nodes[id] || mkEl(id));

const sandbox = {
  localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
  fetch: url => {
    urls.push(String(url));
    return Promise.resolve({ status: 200, ok: true, json: () => Promise.resolve(body(String(url).replace('/panel/api/', ''))) });
  },
  LS_KEY: 'wb2api.key', openKey: () => {},
  document: { getElementById: el },
  setTimeout, clearTimeout, console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent,
};
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(apiSrc + '\n' + dollarSrc + '\n' + escSrc + '\n' + skelSrc + '\n' + mdSrc +
  '\nthis.loadModels = loadModels; this.mdFilter = () => mdFilter; this.mdAll = () => mdAll;',
  sandbox, { filename: 'app.js' });

const sleep = ms => new Promise(r => setTimeout(r, ms));
const rowsOf = html => html.split('<tr>').slice(1).map(s => '<tr>' + s);
// 模型行唯一标识是首列的 <div class="nm">id</div>（<div class="nm"> 恰好 16 字符）。
const idsOf = html => (html.match(/<div class="nm">[^<]*<\/div>/g) || []).map(s => s.slice(16, -6));
const cells = r => (r.replace(/^<tr[^>]*>/, '').replace(/<\/tr>$/, '').match(/<td[^>]*>.*?<\/td>/g) || []);
const runScenario = fn => fn().then(
  out => { process.stdout.write(JSON.stringify(out)); process.exit(0); },
  e => { console.log('MODEL FILTER FAIL: ' + (e && e.stack ? e.stack : e)); process.exit(1); });
`

// TestAppJSModelFilterAndSort 钉住模型档位页的条件查询（上游 981bbe2 的模型筛选部分）：
// 关键词（空格分词 AND、大小写不敏感、命中 ID/名称/描述/厂商/标签）、域、能力、思考档位、
// 价格五个维度的结果行集合，四种排序，清除筛选回到全量，以及两条行为红线——
// 筛选/排序绝不重新请求上游（假 fetch 清单只该有首屏那两条），空结果走既有 .empty 空态。
//
// 模型行集合用「渲染出来的 id 列表」比对而不是内部数组：筛选对了但渲染没跟上
// （例如 renderModels 忘了重画 tbody）同样要失败。
func TestAppJSModelFilterAndSort(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model filter test skipped")
	}
	script := modelPanelStubJS + `
// 目录桩：五个模型刻意覆盖各维度的边界——无倍率（排尾）、context 并列（验证排序稳定）、
// 无 max_output、只有 promo_label 没有 factor（错峰类算「有优惠」但没有折扣倍率）。
const MODELS = [
  { id: 'cn:glm-5.2', name: 'GLM-5.2 旗舰', vendor: 'Zhipu', description: '国内旗舰对话模型', tags: ['旗舰', '长上下文'],
    is_default: true, supports_tool_call: true, supports_images: true, supports_reasoning: true,
    can_disable_thinking: true, supported_efforts: ['high', 'xhigh'], default_effort: 'high',
    credits: 'x2', promo_factor: 0, promo_credits: 'x0', promo_label: '限时免费', promo_note: '9 月限时',
    context_length: 200000, max_output_tokens: 96000 },
  { id: 'cn:glm-4.5-air', name: 'GLM-4.5 Air', vendor: 'Zhipu', description: '轻量快速', tags: ['轻量'],
    is_default: false, supports_tool_call: true, supports_images: false, supports_reasoning: false,
    can_disable_thinking: false, supported_efforts: [], default_effort: '',
    credits: 'x0.5', promo_factor: 0.5, promo_credits: 'x0.25', promo_label: '夜间五折',
    context_length: 128000, max_output_tokens: 32000 },
  { id: 'global:gpt-5.6-luna', name: 'GPT-5.6 Luna', vendor: 'OpenAI', description: '国际版旗舰', tags: ['实验'],
    is_default: false, supports_tool_call: true, supports_images: true, supports_reasoning: true,
    can_disable_thinking: false, supported_efforts: ['low', 'medium', 'high', 'xhigh', 'max'], default_effort: 'high',
    credits: 'x8.88', promo_label: '错峰优惠',
    context_length: 400000, max_output_tokens: 128000 },
  { id: 'global:deepseek-v4-pro', name: 'DeepSeek V4 Pro', vendor: 'DeepSeek', description: '推理模型', tags: ['推理', '便宜'],
    is_default: false, supports_tool_call: false, supports_images: false, supports_reasoning: true,
    can_disable_thinking: false, supported_efforts: ['low', 'high', 'xhigh'], default_effort: 'high',
    context_length: 128000 },
  { id: 'cn:glm-4.6-vision', name: 'GLM-4.6 视觉版', vendor: 'Zhipu', description: '多模态', tags: ['多模态'],
    is_default: false, supports_tool_call: false, supports_images: true, supports_reasoning: false,
    can_disable_thinking: true, supported_efforts: ['off', 'high'], default_effort: 'high',
    credits: 'x3', context_length: 64000, max_output_tokens: 16000 },
];
// 探测结果只给第一个模型：实测上限标注（钳制告警）必须随筛选后的行一起保留。
const PROBES = { 'cn:glm-5.2': { claimed: 96000, measured: 24000, verdict: 'clamped',
  tested_at: '2026-09-20 10:00:00', note: '截断于 24K' } };

runScenario(async () => {
  const out = {};
  devModels = MODELS; devProbes = PROBES;
  await sandbox.loadModels();
  out.urls = urls.join(' ');
  out.all = idsOf(el('mdBody').innerHTML).join(',');
  out.allCount = el('mdCount').textContent;
  out.allCountCls = el('mdCount').className;
  out.note = el('mdNote').textContent;
  // 本 fork 的七列与信息逐字保留：积分倍率（生效价 + 划线牌价 + 标签）、支持档位含
  // off（可关）、上下文长度、最大输出含实测值与钳制告警。
  const rows = rowsOf(el('mdBody').innerHTML);
  out.rowCols = String((rows[0].match(/<td/g) || []).length);
  out.rowRate = cells(rows[0])[2];
  out.rowEff = cells(rows[0])[4];
  out.rowCtx = cells(rows[0])[5];
  out.rowOut = cells(rows[0])[6];

  // 之后的断言只看控件驱动的变化：这里的基线长度就是「零新请求」的分母。
  const before = urls.length;
  const type = async v => { el('mdQ').value = v; el('mdQ').oninput(); await sleep(150); };
  const pick = (id, v) => { el(id).value = v; el(id).onchange(); };
  const now = () => idsOf(el('mdBody').innerHTML).join(',');

  // 关键词：大小写不敏感；空格分词 AND；ID / 名称 / 描述 / 厂商 / 标签都参与匹配。
  await type('GLM'); out.qUpper = now();
  out.qUpperCount = el('mdCount').textContent;
  out.qUpperCls = el('mdCount').className;
  await type('zhipu 旗舰'); out.qAnd = now();
  await type('视觉'); out.qName = now();
  await type('多模态'); out.qTag = now();
  await type('快速'); out.qDesc = now();
  await type('deepseek'); out.qVendorId = now();
  await type('不存在的模型'); out.qMiss = now();
  out.qMissHtml = el('mdBody').innerHTML;
  out.qMissCount = el('mdCount').textContent;
  out.qMissCls = el('mdCount').className;
  await type('');

  // 域 / 能力 / 思考档位 / 价格：下拉不防抖，一次切换就是一次（纯前端）重画。
  pick('mdRealm', 'cn'); out.realmCn = now();
  pick('mdRealm', 'global'); out.realmGlobal = now();
  pick('mdRealm', '');
  pick('mdCap', 'tool'); out.capTool = now();
  pick('mdCap', 'vision'); out.capVision = now();
  pick('mdCap', 'reasoning'); out.capReasoning = now();
  pick('mdCap', 'default'); out.capDefault = now();
  pick('mdCap', '');
  pick('mdEffort', 'off'); out.effortOff = now();
  pick('mdEffort', 'xhigh'); out.effortXhigh = now();
  pick('mdEffort', 'max'); out.effortMax = now();
  pick('mdEffort', '');
  pick('mdPromo', 'promo'); out.promoAny = now();
  pick('mdPromo', 'free'); out.promoFree = now();
  pick('mdPromo', 'discount'); out.promoDiscount = now();
  pick('mdPromo', '');

  // 条件之间是 AND：域 + 能力 + 排序同时生效。
  pick('mdRealm', 'cn'); pick('mdCap', 'vision'); pick('mdSort', 'rate'); out.combo = now();
  pick('mdRealm', ''); pick('mdCap', '');

  // 排序四键 + 默认档（后端原顺序）。context 一列刻意造了并列（两个 128000）：
  // 稳定排序必须保持它们在目录里的相对顺序，否则「同长度」的展示顺序会随机跳动。
  pick('mdSort', 'rate'); out.sortRate = now();
  pick('mdSort', 'context'); out.sortContext = now();
  pick('mdSort', 'output'); out.sortOutput = now();
  pick('mdSort', 'name'); out.sortName = now();
  pick('mdSort', 'default'); out.sortDefault = now();
  out.fetches = String(urls.length - before);

  // 清除筛选：控件与状态一起复位，行集合回到全量，且不触发请求。
  await type('GLM'); pick('mdSort', 'rate');
  el('mdReset').onclick();
  out.reset = now();
  out.resetQ = el('mdQ').value;
  out.resetSort = el('mdSort').value;
  out.resetRealm = el('mdRealm').value;
  out.resetCount = el('mdCount').textContent;
  out.resetCls = el('mdCount').className;
  out.resetFetches = String(urls.length - before);
  out.filterState = JSON.stringify(sandbox.mdFilter());
  out.mdAllLen = String(sandbox.mdAll().length);

  // 重新获取（用户点「重新获取」）：沿用当前筛选条件，不因为刷新把条件悄悄清掉。
  pick('mdCap', 'vision');
  await sandbox.loadModels();
  out.reloadKept = now();
  out.reloadFetches = String(urls.length - before);

  // 上游未返回模型：走「上游未返回模型」空态，并清掉计数条。
  devModels = [];
  await sandbox.loadModels();
  out.emptyUpstream = el('mdBody').innerHTML;
  out.emptyUpstreamCount = el('mdCount').textContent;
  return out;
});`
	f, err := os.CreateTemp(t.TempDir(), "model-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("模型筛选 node 测试失败: %v\n%s", err, out)
	}
	got := map[string]string{}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("模型筛选输出不是 JSON: %v\n%s", err, out)
	}
	// 目录里的 id 在 Go 侧也留一份名字：断言读起来是「哪些模型」，不是一长串前缀。
	const (
		mGlm   = "cn:glm-5.2"
		mAir   = "cn:glm-4.5-air"
		mLuna  = "global:gpt-5.6-luna"
		mDeep  = "global:deepseek-v4-pro"
		mVis   = "cn:glm-4.6-vision"
		allIDs = mGlm + "," + mAir + "," + mLuna + "," + mDeep + "," + mVis
	)
	rows := func(ids ...string) string { return strings.Join(ids, ",") }
	for _, tc := range []struct{ key, want string }{
		// 首屏：两条请求（目录 + 可选探测），无筛选时计数条不转琥珀。
		{"urls", "/panel/api/models /panel/api/model_probes"},
		{"all", allIDs},
		{"allCount", "5 个模型"},
		{"allCountCls", "note"},
		{"note", "5 个模型 · 已刷新降级缓存 · 1 个有实测上限"},
		{"rowCols", "7"},
		{"rowRate", `<td class="num"><span title="9 月限时" class="help"><b>x0</b> <span class="tag info">限时免费</span> <s class="c-muted fs-12">x2</s></span></td>`},
		{"rowEff", `<td class="efs"><span class="tag info">high</span> <span class="tag info">xhigh</span> <span class="tag info">off（可关）</span></td>`},
		{"rowCtx", `<td class="num">200K</td>`},
		{"rowOut", `<td class="num" title="声称 96K · 实测 24K · 截断于 24K · 探测于 2026-09-20 10:00:00"><span class="c-warn strong">24K ⚠</span><div class="note">钳制 4×</div></td>`},
		// 关键词：大写 GLM 命中三个（id/名称都含）、空格分词 AND、名称/描述/标签/ID 各一遍
		{"qUpper", rows(mGlm, mAir, mVis)},
		{"qUpperCount", "命中 3 / 5 个模型"},
		{"qUpperCls", "note src-off"},
		{"qAnd", rows(mGlm)},       // 厂商 Zhipu AND 标签 旗舰
		{"qName", rows(mVis)},      // 命中名称「GLM-4.6 视觉版」
		{"qTag", rows(mVis)},       // 命中标签「多模态」
		{"qDesc", rows(mAir)},      // 命中描述「轻量快速」
		{"qVendorId", rows(mDeep)}, // 命中 id 与厂商 DeepSeek
		// 空结果：既有 .empty 写法 + 「N / M」计数 + 琥珀提示
		{"qMiss", ""},
		{"qMissHtml", `<tr><td colspan="7"><div class="empty">没有符合当前筛选条件的模型</div></td></tr>`},
		{"qMissCount", "命中 0 / 5 个模型"},
		{"qMissCls", "note src-off"},
		// 域：按 id 前缀（= 调用时该填的 model 值）判定
		{"realmCn", rows(mGlm, mAir, mVis)},
		{"realmGlobal", rows(mLuna, mDeep)},
		// 能力：工具 / 视觉 / 思考 / 默认
		{"capTool", rows(mGlm, mAir, mLuna)},
		{"capVision", rows(mGlm, mLuna, mVis)},
		{"capReasoning", rows(mGlm, mLuna, mDeep)},
		{"capDefault", rows(mGlm)},
		// 思考档位：off = 可关闭思考；其余档位看 supported_efforts
		{"effortOff", rows(mGlm, mVis)},
		{"effortXhigh", rows(mGlm, mLuna, mDeep)},
		{"effortMax", rows(mLuna)},
		// 价格：有优惠（含错峰标签）/ 限时免费（factor 0）/ 打折但非免费
		{"promoAny", rows(mGlm, mAir, mLuna)},
		{"promoFree", rows(mGlm)},
		{"promoDiscount", rows(mAir)},
		// 域 + 能力 + 排序三条件 AND
		{"combo", rows(mGlm, mVis)},
		// 排序四键：倍率升序（无倍率的 mDeep 排尾）、上下文/最大输出降序、ID A→Z
		{"sortRate", rows(mGlm, mAir, mVis, mLuna, mDeep)},
		{"sortContext", rows(mLuna, mGlm, mAir, mDeep, mVis)},
		{"sortOutput", rows(mLuna, mGlm, mAir, mVis, mDeep)},
		{"sortName", rows(mAir, mVis, mGlm, mDeep, mLuna)},
		{"sortDefault", allIDs},
		// 红线：上面所有筛选与排序一次请求都没发（before 之后 urls 长度不变）
		{"fetches", "0"},
		{"reset", allIDs},
		{"resetQ", ""},
		{"resetSort", "default"},
		{"resetRealm", ""},
		{"resetCount", "5 个模型"},
		{"resetCls", "note"},
		{"resetFetches", "0"},
		{"filterState", `{"q":"","realm":"","cap":"","effort":"","promo":"","sort":"default"}`},
		{"mdAllLen", "5"},
		// 重新获取沿用筛选条件：cap=vision 仍然生效，且确实重新拉了目录（2 条请求）
		{"reloadKept", rows(mGlm, mLuna, mVis)},
		{"reloadFetches", "2"},
		{"emptyUpstream", `<tr><td colspan="7"><div class="empty">上游未返回模型</div></td></tr>`},
		{"emptyUpstreamCount", ""},
	} {
		if got[tc.key] != tc.want {
			t.Errorf("%s=%q\nwant %q", tc.key, got[tc.key], tc.want)
		}
	}
}

// TestAppJSCollectConfigClearable 钉住 collectConfig 的空串语义（上游 10e17ef）。
//
// 覆盖型字段（user_agent / prompt_file）空串必须照发：mergeConfigMaps 是深合并，
// 未提交的键原样保留，漏发会让面板显示「已保存」而 config.json 里的值没变——
// 「清空 = 回落默认」这条路径就彻底没了。
//
// 反面同样重要：其余文本字段空串仍然不下发（表单里没填的框 = 没改，不是「请清空」）；
// api_key 刻意不在 CLEARABLE_CFG 里——清空它等于关掉整个鉴权。
func TestAppJSCollectConfigClearable(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; collectConfig test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const CFG_MAP');
const end = src.indexOf('/* Go 时长字段即时校验');
if (start < 0 || end < 0 || end < start) throw new Error('collectConfig region not found');
const mk = v => ({ type: 'text', value: v });
const cfgForm = { elements: {
  listen: mk(''),
  api_key: mk(''),
  user_agent: mk(''),
  prompt_file: mk(''),
  prompt_mode: mk(''),
  checkin_hours: mk(''),
}};
const ctx = {
  Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams, Set,
  document: { getElementById: id => (id === 'cfgForm' ? cfgForm : null) },
  $: id => (id === 'cfgForm' ? cfgForm : null),
};
vm.createContext(ctx);
// const 声明只活在脚本文法作用域里，不会挂到 context 全局上，故显式导出这两个符号。
vm.runInContext(src.slice(start, end) +
  '\nthis.collectConfig = collectConfig; this.CLEARABLE_CFG = CLEARABLE_CFG;', ctx);
const out = ctx.collectConfig();
const has = (o, k) => Object.prototype.hasOwnProperty.call(o || {}, k);
process.stdout.write(JSON.stringify([
  has(out.upstream, 'user_agent'), (out.upstream || {}).user_agent,
  has(out.prompt, 'file'), (out.prompt || {}).file,
  has(out.prompt, 'mode'),
  has(out, 'listen'),
  has(out, 'api_key'),
  has(out.schedule, 'checkin_hours'),
  ctx.CLEARABLE_CFG ? ctx.CLEARABLE_CFG.has('api_key') : 'no-set'
]));`
	f, err := os.CreateTemp(t.TempDir(), "cfgc-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("collectConfig node test failed: %v\n%s", err, out)
	}
	// [user_agent 已发, 其值 "", prompt.file 已发, 其值 "", prompt.mode 未发,
	//  listen 未发, api_key 未发, checkin_hours 未发, CLEARABLE_CFG 不含 api_key]
	const want = `[true,"",true,"",false,false,false,false,false]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("collectConfig=%s want %s", strings.TrimSpace(string(out)), want)
	}
}

// TestIndexModelFilterBar 模型页筛选栏的静态结构（不依赖 node）：
//   - 复用既有 .fbar 体系（第 7 批为请求记录引入），不新造一套筛选栏样式；
//   - 五个筛选 + 排序 + 计数 + 清除筛选的控件 id / option 取值必须与应用层判定一致
//     （值拼错不报错，只是该筛选项永远筛不出东西）；
//   - 模型表仍是本 fork 的七列，表格高度预算把新增的筛选栏算进去。
func TestIndexModelFilterBar(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	const fbarStart = `<div class="fbar" id="mdFilterBar">`
	const tbodyStart = `<tbody id="mdBody"></tbody>`
	viewAt := strings.Index(body, `<section class="view" id="view-models"`)
	i, j := strings.Index(body, fbarStart), strings.Index(body, tbodyStart)
	if viewAt < 0 || i < 0 || j < 0 || j < i || i < viewAt {
		t.Fatalf("index.html 缺少模型筛选栏或表格：view=%d fbar=%d tbody=%d", viewAt, i, j)
	}
	fbar := body[i:j]
	for _, want := range []string{
		`<input id="mdQ" type="search"`,
		`<select id="mdRealm" class="xs"`,
		`<select id="mdCap" class="xs"`,
		`<select id="mdEffort" class="xs"`,
		`<select id="mdPromo" class="xs"`,
		`<select id="mdSort" class="xs"`,
		`<span class="grow"></span>`,
		`<span class="note" id="mdCount"></span>`,
		`<button class="xs" id="mdReset"`,
	} {
		if !strings.Contains(fbar, want) {
			t.Errorf("模型筛选栏缺少控件 %s\n实际：%s", want, fbar)
		}
	}
	// 各维度取值必须与应用层判定字面量一一对应（不发明取值）。
	for _, want := range []string{
		`<option value="">全部域</option>`, `<option value="cn">`, `<option value="global">`,
		`<option value="tool">`, `<option value="vision">`, `<option value="reasoning">`, `<option value="default">`,
		`<option value="off">`, `<option value="minimal">`, `<option value="low">`, `<option value="medium">`,
		`<option value="high">`, `<option value="xhigh">`, `<option value="max">`,
		`<option value="promo">`, `<option value="free">`, `<option value="discount">`,
		`<option value="default">上游默认顺序</option>`, `<option value="rate">`, `<option value="context">`,
		`<option value="output">`, `<option value="name">`,
	} {
		if !strings.Contains(fbar, want) {
			t.Errorf("模型筛选缺少选项 %s", want)
		}
	}
	// 表头仍是七列：筛选不该动模型表的信息集（列数对不上整行错位）。
	head := body[viewAt:j]
	if n := strings.Count(head, "</th>"); n != 7 {
		t.Errorf("模型表头应为 7 列，实际 %d", n)
	}
	// 样式与高度预算：.fbar 体系复用 + 表格高度把新增的 43px 筛选栏算进去。
	for _, want := range []string{
		".fbar {", ".fbar > input, .fbar > select", "select.xs",
		"#view-models .tbl-wrap { max-height: max(240px, calc(100dvh - 239px)); }",
		".empty {", ".src-off {",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html 缺少模型筛选相关样式：%s", want)
		}
	}
}

// minInt / maxInt 是本文件里既有 maxInt 的补充（截断取尾片段时用）。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestAppJSExpiryBatchMath 到期批次的纯逻辑（node 沙箱，无 DOM）：
//
//	· expBatches 按到期日归并——同日多包求和，顺带带上该批面额与包数；
//	· 只算 remain>0 且有到期时间的包（没余额 / 长期包的到期没有意义）；
//	· expDaysLeft 今天=0、未来为正、已过期为负（调用方负责过滤，函数本身不藏）；
//	· expDayWord 的「今天/明天/N 天后」文案。
//
// 用例只由「年-月-日」构造日期（本地自然日），跑在哪个时区都成立。
func TestAppJSExpiryBatchMath(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry batch math test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function expBatches(packs)');
const end = src.indexOf('function renderExpiry(d)');
if (start < 0 || end <= start) throw new Error('expBatches/expDaysLeft slice not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.expBatches = expBatches; this.expDaysLeft = expDaysLeft; this.expDayWord = expDayWord;', ctx);
const today = new Date(2026, 9, 1); // 2026-10-01 00:00 本地
const day = n => {
  const t = new Date(2026, 9, 1 + n);
  return t.getFullYear() + '-' + String(t.getMonth() + 1).padStart(2, '0') + '-' +
    String(t.getDate()).padStart(2, '0');
};
const batches = ctx.expBatches([
  { name: 'a', remain: 60, size: 100, end_time: day(0) + 'T18:00:00+08:00' },
  { name: 'b', remain: 40, size: 50, end_time: day(0) + 'T23:00:00+08:00' },
  { name: 'c', remain: 30, size: 30, end_time: day(3) + ' 00:00:00' },
  { name: 'expired', remain: 5, size: 5, end_time: day(-2) + 'T00:00:00+08:00' },
  { name: 'no-end', remain: 99, size: 99 },
  { name: 'zero', remain: 0, size: 40, end_time: day(1) + 'T00:00:00+08:00' },
]);
process.stdout.write(JSON.stringify({
  batches,
  today: ctx.expDaysLeft(day(0), today),
  in3: ctx.expDaysLeft(day(3), today),
  past: ctx.expDaysLeft(day(-2), today),
  empty: ctx.expBatches([]),
  nullish: ctx.expBatches(null),
  word: [ctx.expDayWord(-1), ctx.expDayWord(0), ctx.expDayWord(1), ctx.expDayWord(5)],
}));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-math-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry batch math node test failed: %v\n%s", err, out)
	}
	// 2026-10-01 今天；+3 天 = 10-04；-2 天 = 09-29（跨月，日期串由本地日历算出）。
	// 排序按日期串升序：已过的批次排最前（函数不做过滤，过滤是渲染层的事）。
	const want = `{"batches":[{"date":"2026-09-29","remain":5,"size":5,"n":1},` +
		`{"date":"2026-10-01","remain":100,"size":150,"n":2},` +
		`{"date":"2026-10-04","remain":30,"size":30,"n":1}],` +
		`"today":0,"in3":3,"past":-2,"empty":[],"nullish":[],` +
		`"word":["已过期","今天到期","明天到期","5 天后到期"]}`
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("expiry batch math=%s\nwant %s", got, want)
	}
}

// TestAppJSExpiryCardRender 到期卡片的三态渲染（node + DOM 桩）：
// 有将到期批次 / 无将到期（只余长期包与已过期包）/ 无数据，外加「全查挂」这一态。
// 断言口径都在卡片 HTML 自身：剩余天数文案、面额、剩余、涉及账号数、图例配色、
// FEFO 结论，以及缓存年龄的两条分支（实时 / N 分钟前）。
// 缓存时间戳用桩注入（setAge），年龄文案的两条分支才能被精确钉住。
func TestAppJSExpiryCardRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry card render test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
// 只取纯渲染链路：格式化/转义 + 账号配色与到期工具 + 到期批次聚合 + renderExpiry。
// （renderExpiry 的切片要用带参数的签名定位——'function renderExpiry' 会先命中
//  更早出现的 renderExpiryDistribution。）
const slices = [
  src.slice(src.indexOf('function esc('), src.indexOf('function veilStack(')),
  src.slice(src.indexOf('function fmtTok('), src.indexOf('function usStat(')),
  src.slice(src.indexOf('const PK_ACCOUNT_COLORS'), src.indexOf('function renderExpiryDistribution')),
  // 缓存变量声明在路由区，这里补一份等价桩：年龄文案要有确定的输入。
  'let lastPackages = null, lastPackagesLimit; let lastPackagesAt = 0; const EXP_FRESH_MS = 2 * 60 * 1000;',
  src.slice(src.indexOf('function expBatches(packs)'), src.indexOf('function renderExpiry(d)')),
  src.slice(src.indexOf('function renderExpiry(d)'), src.indexOf('async function loadExpiry(force)')),
];
if (slices.some(s => !s)) throw new Error('expiry render slice not found');
const RealDate = Date;
const FIXED = RealDate.parse('2026-10-01T09:00:00'); // 本地 09:00 → 今天 = 10-01
class FakeDate extends RealDate {
  constructor(...a) { if (a.length) { super(...a); } else { super(FIXED); } }
  static now() { return FIXED; }
}
const day = n => {
  const t = new RealDate(2026, 9, 1 + n);
  return t.getFullYear() + '-' + String(t.getMonth() + 1).padStart(2, '0') + '-' +
    String(t.getDate()).padStart(2, '0');
};
const PACKS = { accounts: [
  { uid: 'uid-a', nickname: '号一', realm: 'cn', remain: 150, size: 250, packages: [
    { name: '裂变包', remain: 60, size: 100, end_time: day(0) + 'T18:00:00+08:00' },
    { name: '裂变包', remain: 40, size: 100, end_time: day(0) + 'T23:00:00+08:00' },
    { name: '长期包', remain: 50, size: 50 },
    { name: '已用完', remain: 0, size: 90, end_time: day(1) + 'T00:00:00+08:00' },
    { name: '小包', remain: 20, size: 20, end_time: day(3) + 'T00:00:00+08:00' },
    { name: '中包', remain: 12, size: 12, end_time: day(5) + 'T00:00:00+08:00' },
    { name: '远包', remain: 8, size: 8, end_time: day(20) + 'T00:00:00+08:00' },
  ] },
  { uid: 'uid-b', nickname: '号二', realm: 'global', remain: 35, size: 35, packages: [
    { name: '拉新包', remain: 30, size: 30, end_time: day(0) + 'T20:00:00+08:00' },
    { name: '过期包', remain: 5, size: 5, end_time: day(-3) + 'T00:00:00+08:00' },
  ] },
  { uid: 'uid-c', nickname: '号三', error: 'token expired' },
  { uid: 'uid-d', nickname: '号四<script>', realm: 'cn', remain: 3, size: 3, packages: [
    { name: '活动包', remain: 3, size: 3, end_time: day(5) + 'T00:00:00+08:00' },
  ] },
] };
const NO_EXPIRY = { accounts: [
  { uid: 'uid-a', nickname: '号一', packages: [
    { name: '长期包', remain: 10, size: 10 },
    { name: '过期包', remain: 7, size: 7, end_time: day(-1) + 'T00:00:00+08:00' },
  ] },
] };
const ALL_FAILED = { accounts: [{ uid: 'uid-x', nickname: '号X', error: 'boom' }] };
const nodes = {};
const el = id => (nodes[id] = nodes[id] || { innerHTML: '', textContent: '', hidden: false, children: [] });
const ctx = {
  $: el, Date: FakeDate, Number, String, Boolean, Math, Array, Object, Map, Set, JSON, RegExp, Error,
  isNaN, isFinite, parseInt, parseFloat,
};
vm.createContext(ctx);
vm.runInContext(slices.join('\n') + '\nthis.renderExpiry = renderExpiry;' +
  '\nthis.setAge = ms => { lastPackagesAt = ms; };', ctx);
el('expBox').hidden = true; // 渲染必须把卡片显示出来（不是靠初始值偶然为 false）
ctx.setAge(FIXED);
ctx.renderExpiry(PACKS);
const live = { list: nodes.expList.innerHTML, note: nodes.expNote.textContent, hidden: nodes.expBox.hidden };
ctx.setAge(FIXED - 7 * 60000);
ctx.renderExpiry(PACKS);
const stale = { list: nodes.expList.innerHTML, note: nodes.expNote.textContent };
ctx.setAge(FIXED);
ctx.renderExpiry(NO_EXPIRY);
const noExpiry = { list: nodes.expList.innerHTML, note: nodes.expNote.textContent };
ctx.renderExpiry(ALL_FAILED);
const allFailed = { list: nodes.expList.innerHTML, note: nodes.expNote.textContent };
ctx.renderExpiry({});
const noData = { list: nodes.expList.innerHTML, note: nodes.expNote.textContent };
ctx.renderExpiry(null);
const nullish = { list: nodes.expList.innerHTML };
process.stdout.write(JSON.stringify({ live, stale, noExpiry, allFailed, noData, nullish }));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-card-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry card render node test failed: %v\n%s", err, out)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("expiry card render output is not JSON: %v\n%s", err, out)
	}
	str := func(section, key string) string {
		s, _ := got[section][key].(string)
		return s
	}
	boolean := func(section, key string) bool {
		b, _ := got[section][key].(bool)
		return b
	}

	// ── 态一：有将到期批次（按到期日聚合，最近到期在最上）──────────────
	live := str("live", "list")
	for _, want := range []string{
		`<span class="pk-expiry-days is-bad">今天到期</span>`, // 0 天不是「0 天后」
		`<span class="pk-expiry-days is-bad">3 天后到期</span>`,
		`<span class="pk-expiry-days is-warn">5 天后到期</span>`,
		`<span class="pk-expiry-days is-ok">20 天后到期</span>`,
		`<span>10-01</span>`,
		`>剩余天数<`, `>到期日<`, `>面额<`, `>剩余积分<`, `>账号数<`,
		`<b>230</b><b>130</b><b>2</b>`, // 最近到期那行：面额 230 / 剩余 130 / 2 个账号
		`<b>15</b><b>15</b><b>2</b>`,   // 5 天后那行：号一 12 + 号四 3 聚合成一批
		`FEFO 结论`, `先消耗 <b>2026-10-01</b>`, `日均需耗 ≥<b>130</b>`,
		`7 天内合计 <b>165</b> 积分`, // 130（今天）+ 20（3 天）+ 15（5 天）
		`共 4 个账号 / 1 个查询失败`, `另有 1 个批次已过期（不计入）`,
		`--seg-color:var(--chart-1)`, `--seg-color:var(--chart-2)`, `--seg-color:var(--chart-3)`,
	} {
		if !strings.Contains(live, want) {
			t.Errorf("到期卡片（有将到期批次）缺少 %q", want)
		}
	}
	if n := strings.Count(live, `class="pk-expiry-row"`); n != 4 {
		t.Errorf("到期行数 = %d，期望 4（10-01 / 10-04 / 10-06 / 10-21）", n)
	}
	if n := strings.Count(live, `class="pk-expiry-seg"`); n != 6 {
		t.Errorf("轨道段数 = %d，期望 6（各行按账号构成拆段）", n)
	}
	// FEFO：先到期的行必须排在前面。
	if i, j := strings.Index(live, "今天到期"), strings.Index(live, "3 天后到期"); i < 0 || j < 0 || i > j {
		t.Errorf("到期行未按到期日升序（FEFO 顺序）排列：idx(今天)=%d idx(3 天后)=%d", i, j)
	}
	if strings.Contains(live, "号四<script>") || !strings.Contains(live, "号四&lt;script&gt;") {
		t.Error("账号昵称里的 HTML 未被转义（图例/title 直接拼进了卡片）")
	}
	if strings.Contains(live, `class="pk-expiry-row"`) && !strings.Contains(live, `class="pk-expiry-foot"`) {
		t.Error("有数据时脚注（FEFO 结论）必须存在")
	}
	// 卡片 HTML 不得带静态内联样式（只允许注入 CSS 变量）。
	for _, m := range regexp.MustCompile(`style="([^"]*)"`).FindAllStringSubmatch(live, -1) {
		if !strings.HasPrefix(m[1], "--") {
			t.Errorf("到期卡片 HTML 有静态内联样式：%s", m[0])
		}
	}
	if boolean("live", "hidden") {
		t.Error("渲染后卡片必须显示（#expBox 的 hidden 应被摘掉）")
	}
	if !strings.Contains(str("live", "note"), "4 个账号 · 实时查询上游") {
		t.Errorf("新鲜数据应标注「实时查询上游」，实际 %q", str("live", "note"))
	}
	// ── 缓存年龄：走缓存时标注数据年龄（阈值 EXP_FRESH_MS = 2 分钟）────────
	if !strings.Contains(str("stale", "note"), "4 个账号 · 7 分钟前的数据，可点「检查」刷新") {
		t.Errorf("旧数据未标注年龄：%q", str("stale", "note"))
	}
	if str("stale", "list") != live {
		t.Error("同一份缓存的卡片内容应与实时渲染逐字一致（年龄只影响 #expNote）")
	}

	// ── 态二：没有将到期的批次（只有长期包 + 已过期包）─────────────────
	noExpiry := str("noExpiry", "list")
	for _, want := range []string{
		`<div class="pk-expiry-empty">没有即将到期的积分批次：1 个账号的积分都不会自动作废</div>`,
		`FEFO 结论：当前没有会到期的积分批次`,
		`另有 1 个批次已过期（不计入）`,
	} {
		if !strings.Contains(noExpiry, want) {
			t.Errorf("到期卡片（无将到期）缺少 %q：%s", want, noExpiry)
		}
	}
	// 空态不许摆空表头。
	if strings.Contains(noExpiry, `class="pk-expiry-row"`) || strings.Contains(noExpiry, `>剩余天数<`) {
		t.Errorf("无将到期时不得渲染空表：%s", noExpiry)
	}
	if strings.Contains(noExpiry, "7 天内合计") {
		t.Errorf("没有将到期批次时不该出现「7 天内合计」：%s", noExpiry)
	}

	// ── 态三 / 态四：全部查询失败 / 无数据 ─────────────────────────────
	if !strings.Contains(str("allFailed", "list"), "1 个账号全部查询失败，暂时判断不得到期批次") {
		t.Errorf("全查挂时的文案不可读：%s", str("allFailed", "list"))
	}
	for key, list := range map[string]string{"noData": str("noData", "list"), "nullish": str("nullish", "list")} {
		if !strings.Contains(list, `<div class="pk-expiry-empty">没有账号</div>`) {
			t.Errorf("%s 未给出「没有账号」空态：%s", key, list)
		}
	}
	if !strings.Contains(str("noData", "note"), "0 个账号 · 实时查询上游") {
		t.Errorf("无数据时 #expNote 应给出 0 个账号：%q", str("noData", "note"))
	}
}

// TestAppJSExpiryFreshness 到期卡片的取数策略（node + DOM 桩，假 fetch 记录 URL 与次数）：
//  1. 深链 #accounts 首屏取一次 packages（顶层 go() → loadExpiry 的路径必须通）；
//  2. 新鲜窗口（EXP_FRESH_MS = 2 分钟，取自上游 acb3830）内切回视图 / 再调 loadExpiry
//     都不重复请求；
//  3. 超过窗口重新取；force（「检查」按钮）即使新鲜也重取；
//  4. 在途去重：连点两次只打一遍上游。
//
// 同一沙箱顺带回归本 fork 的三条红线：账号表 7.10M chip、模型锁池表、trange 宿主
// （7 个预设仍在，且默认仍是「近 3 天」）。
func TestAppJSExpiryFreshness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry freshness test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const urls = [];
// 时钟钉死且可推前：新鲜度判断要用它推进（否则要么立刻过期要么永远新鲜）。
const RealDate = Date;
const FIXED = RealDate.parse('2026-10-01T09:00:00');
let NOW = FIXED;
class FakeDate extends RealDate {
  constructor(...a) { if (a.length) { super(...a); } else { super(NOW); } }
  static now() { return NOW; }
}
const day = n => {
  const t = new RealDate(2026, 9, 1 + n);
  return t.getFullYear() + '-' + String(t.getMonth() + 1).padStart(2, '0') + '-' +
    String(t.getDate()).padStart(2, '0');
};
const body = p => {
  if (p === 'packages') {
    if (failNext) return { __status: 500, error: '上游超时' };
    return { accounts: [
      { uid: 'uid-a', nickname: '号一', realm: 'cn', remain: 60, size: 100, packages: [
        { name: '裂变包', remain: 60, size: 100, end_time: day(0) + 'T18:00:00+08:00' },
      ] },
    ] };
  }
  if (p.startsWith('usage')) return { totals: {}, by_account: [], by_model: [], by_realm: [], series: [] };
  if (p.startsWith('request_logs')) return { entries: [] };
  if (p.startsWith('logs')) return { entries: [] };
  return {};
};
let failNext = false;
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; }, apply() { return inert; }, construct() { return inert; }, has() { return true; },
});
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
  location: { hash: '#accounts' }, // 深链：顶层 go() 会同步调进 loadExpiry（TDZ 敏感路径）
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
    const d = body(path);
    // 与 api() 同判：!ok 时抛 d.error，失败路径才能被真的走到。
    if (d && d.__status) return Promise.resolve({ status: d.__status, ok: false, json: () => Promise.resolve(d) });
    return Promise.resolve({ status: 200, ok: true, json: () => Promise.resolve(d) });
  },
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date: FakeDate, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, URLSearchParams, Symbol, Proxy, Reflect,
};
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src +
  '\nthis.loadExpiry = loadExpiry; this.go = go; this.EXP_FRESH_MS = EXP_FRESH_MS;' +
  '\nthis.renderAccounts = renderAccounts; this.renderModelLocks = renderModelLocks;',
  sandbox, { filename: 'app.js' });
const tick = () => new Promise(r => setTimeout(r, 5));
const pkCount = () => urls.filter(u => u === 'packages').length;

(async () => {
  await tick();
  const out = {};
  out.freshMs = sandbox.EXP_FRESH_MS;
  out.deepLinkCount = pkCount();                       // 进 #accounts 取一次
  out.deepLinkNote = els.expNote.textContent;
  out.deepLinkList = els.expList.innerHTML;

  const before = pkCount();
  sandbox.go('accounts');                              // 切回同一视图：走缓存
  await tick();
  out.reuseOnEnter = pkCount() === before;
  await sandbox.loadExpiry();                          // 显式调用同样复用
  out.reuseExplicit = pkCount() === before;

  // 「积分构成」视图取到的数据同样进这份缓存：切到账号管理不再打第二遍
  //（列表页自己仍然实时取数——那里是主视图，不做缓存拦截）。
  sandbox.go('packages');
  await tick();
  const afterPackages = pkCount() - before;
  sandbox.go('accounts');
  await tick();
  out.crossViewPackagesFetch = afterPackages;
  out.crossViewReuse = pkCount() - before === afterPackages;

  const beforeStale = pkCount();
  NOW += sandbox.EXP_FRESH_MS + 1000;                  // 越过新鲜窗口 → 自动重取
  await sandbox.loadExpiry();
  out.staleRefetch = pkCount() - beforeStale;

  const beforeForce = pkCount();
  await sandbox.loadExpiry(true);                      // force：新鲜也重取
  out.forceRefetch = pkCount() - beforeForce;

  const beforeDup = pkCount();
  await Promise.all([sandbox.loadExpiry(true), sandbox.loadExpiry(true)]);
  out.dupRefetch = pkCount() - beforeDup;

  // 取数失败：旧数据继续显示，note 说明失败（首屏失败则不留「查询中…」占位）。
  failNext = true;
  const beforeFail = pkCount();
  await sandbox.loadExpiry(true);
  out.failRefetch = pkCount() - beforeFail;
  out.failNote = els.expNote.textContent;
  out.failKeepsData = els.expList.innerHTML.indexOf('pk-expiry-row') >= 0;
  failNext = false;

  // 回归红线：账号表 chip / 模型锁池 / trange 宿主。
  const at = s => new RealDate(NOW + s * 1000).toISOString();
  sandbox.renderAccounts([{
    uid: 'uid-0000000000000001', nickname: '号一', credits: 10, credits_total: 100,
    last_success: '2026-09-28T13:00:00Z',
    today: { day: '2026-09-28', requests: 1771, errors: 3, total_tokens: 7100000 },
    token_usage: { request_count: 1771, ok_count: 1768, total_tokens: 18700000, last_latency_ms: 1500 },
  }]);
  out.accounts = els.accBody.innerHTML;
  sandbox.renderModelLocks([
    { model: 'glm-5.3', realm: 'cn', total: 5, servable: 0, locked: 5, state: 'locked',
      unlock_at: at(3600), fully_unlock_at: at(9000), reason: '上游 <429> 额度不足 & 稍后重试' },
  ]);
  out.locks = els.mlBody.innerHTML;
  out.lockNote = els.mlNote.textContent;
  sandbox.go('usage');
  await tick();
  out.usRange = els.usRange.innerHTML;
  out.usageUrl = urls.filter(u => u.startsWith('usage?')).slice(-1)[0] || '';
  return out;
})().then(out => {
  process.stdout.write(JSON.stringify(out));
  process.exit(0);
}, e => {
  console.log('EXPIRY FRESHNESS FAIL: ' + (e && e.stack ? e.stack : e));
  process.exit(1);
});`
	f, err := os.CreateTemp(t.TempDir(), "expiry-fresh-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry freshness node test failed: %v\n%s", err, out)
	}
	got := map[string]any{}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("expiry freshness output is not JSON: %v\n%s", err, out)
	}
	str := func(k string) string { s, _ := got[k].(string); return s }
	boolean := func(k string) bool { b, _ := got[k].(bool); return b }
	num := func(k string) float64 { n, _ := got[k].(float64); return n }

	if num("freshMs") != 120000 {
		t.Errorf("EXP_FRESH_MS = %v，期望 120000（2 分钟，取自上游 acb3830）", num("freshMs"))
	}
	if num("deepLinkCount") != 1 {
		t.Errorf("深链 #accounts 首屏应恰好请求一次 packages，实际 %v 次", num("deepLinkCount"))
	}
	if !strings.Contains(str("deepLinkList"), "今天到期") {
		t.Errorf("首屏卡片未渲染出到期批次：%s", str("deepLinkList"))
	}
	if !strings.Contains(str("deepLinkNote"), "1 个账号 · 实时查询上游") {
		t.Errorf("首屏 #expNote 应说明数据是实时的：%q", str("deepLinkNote"))
	}
	if !boolean("reuseOnEnter") {
		t.Error("新鲜窗口内切回账号管理视图不应重复请求上游")
	}
	if !boolean("reuseExplicit") {
		t.Error("新鲜窗口内再调 loadExpiry() 不应重复请求上游")
	}
	if num("staleRefetch") != 1 {
		t.Errorf("越过 %vms 新鲜窗口后应重取一次，实际 %v 次", num("freshMs"), num("staleRefetch"))
	}
	if num("forceRefetch") != 1 {
		t.Errorf("force（「检查」按钮）应忽略缓存重取一次，实际 %v 次", num("forceRefetch"))
	}
	if num("dupRefetch") != 1 {
		t.Errorf("在途去重失效：连点两次发了 %v 次请求", num("dupRefetch"))
	}
	if num("crossViewPackagesFetch") != 1 {
		t.Errorf("「积分构成」视图进视图应取一次数，实际 %v 次", num("crossViewPackagesFetch"))
	}
	if !boolean("crossViewReuse") {
		t.Error("从「积分构成」切到账号管理时未复用刚取到的数据（lastPackagesAt 没盖时间戳）")
	}

	// 取数失败：旧数据保留 + note 说明；首屏失败不留「查询中…」占位。
	if num("failRefetch") != 1 {
		t.Errorf("force 失败时也应发出一次请求，实际 %v 次", num("failRefetch"))
	}
	if !strings.HasPrefix(str("failNote"), "查询失败：上游超时") {
		t.Errorf("失败后 #expNote 应说明原因，实际 %q", str("failNote"))
	}
	if !boolean("failKeepsData") {
		t.Errorf("一次取数失败不应把已有卡片清空（应继续显示上一次的数据）")
	}

	// 回归红线。
	if !strings.Contains(str("accounts"), `<span class="usage-item usage-total"><b>7.10<span class="usage-unit">M</span></b></span>`) {
		t.Errorf("账号表今日用量不再是 7.10M 的 chip：%s", str("accounts"))
	}
	if !strings.Contains(str("locks"), "glm-5.3") || !strings.Contains(str("locks"), `整池不可用`) ||
		!strings.Contains(str("locks"), `上游 &lt;429&gt; 额度不足 &amp; 稍后重试`) {
		t.Errorf("模型锁池表渲染异常：%s（note=%q）", str("locks"), str("lockNote"))
	}
	if !strings.Contains(str("usRange"), `class="tr-preset"`) || strings.Count(str("usRange"), "<option") != 7 {
		t.Errorf("时间范围控件未渲染出 7 个预设：%s", str("usRange"))
	}
	if str("usageUrl") != "usage?hours=72" {
		t.Errorf("默认时间范围应仍是「近 3 天」：%q", str("usageUrl"))
	}
}

// TestIndexExpiryCardHost 到期卡片的宿主与位置：卡片必须在 #view-accounts 内、
// 账号表之前（上游位置），不新增导航项；四个挂钩 id 缺一个都会让卡片人间蒸发
// （app.js 侧是 if ($('expBox')) 守卫写法，删掉宿主不会报错、测试也不会红）。
// 另钉住 app.js 的缓存声明顺序：顶层 go() 会同步读 lastPackages，声明挪到文件
// 底部就是 TDZ ReferenceError（整页白屏，Go 侧其它测试全绿）。
func TestIndexExpiryCardHost(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	viewAt := strings.Index(body, `<section class="view" id="view-accounts">`)
	nextView := strings.Index(body, `id="view-taskscenter"`)
	cardAt := strings.Index(body, `<div class="box" id="expBox" hidden>`)
	tableAt := strings.Index(body, `<tbody id="accBody">`)
	if viewAt < 0 || nextView <= viewAt || cardAt < 0 || tableAt < 0 {
		t.Fatalf("index.html 结构变了：view=%d next=%d card=%d table=%d", viewAt, nextView, cardAt, tableAt)
	}
	if cardAt < viewAt || cardAt > nextView {
		t.Errorf("到期卡片不在 #view-accounts 内（card=%d view=%d next=%d）", cardAt, viewAt, nextView)
	}
	if tableAt < cardAt {
		t.Error("到期卡片应在账号表上方（与上游同位置：统计条之下、账号池之上）")
	}
	card := body[cardAt:nextView]
	for _, want := range []string{
		`<h3>积分到期提醒<span class="hint">`,
		`FEFO`, // 结论口径写进 hint，卡片里也能看到
		`<span class="note" id="expNote"></span>`,
		`<div id="expList"></div>`,
		`<button class="xs" id="btnExp"`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("到期卡片缺少 %s", want)
		}
	}
	for _, m := range regexp.MustCompile(`style="[^"]*"`).FindAllString(card, -1) {
		if !strings.HasPrefix(m, `style="--`) {
			t.Errorf("到期卡片有静态内联样式：%s", m)
		}
	}

	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	declAt := strings.Index(src, "let lastPackages = null, lastPackagesLimit;")
	goAt := strings.Index(src, "function go(v) {")
	if declAt < 0 || goAt < 0 || declAt > goAt {
		t.Fatalf("积分包缓存的声明必须在 go() 之前（decl=%d go=%d）：顶层 go() 会同步读它，挪到后面就是 TDZ", declAt, goAt)
	}
	for _, want := range []string{
		`if (v === 'accounts') loadExpiry();`,
		`let lastPackagesAt = 0;`,
		`const EXP_FRESH_MS = 2 * 60 * 1000;`,
		`function expBatches(packs)`,
		`function expDaysLeft(dateStr, today)`,
		`function renderExpiry(d)`,
		`async function loadExpiry(force)`,
		`if ($('btnExp')) $('btnExp').onclick = () => loadExpiry(true);`,
		`lastPackagesAt = Date.now();`,                    // 两条取数路径都要盖时间戳
		`pkAccountColorMap(list)`,                         // 账号配色与积分构成共用
		`class="pk-expiry-empty"`,                         // 空态复用既有样式
		`'<div class="pk-expiry-empty">读取失败：' + esc(msg)`, // 首屏失败不留「查询中…」占位
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js 缺少到期卡片的相关实现：%s", want)
		}
	}
}
