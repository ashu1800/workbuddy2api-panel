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
