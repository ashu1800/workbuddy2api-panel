'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_KEY = 'wb2api.key', LS_THEME = 'wb2api.theme';
// 主题以浅色为主：只有用户显式切过深色才用深色（历史 'auto' 与未知值回落浅色）。
let theme = localStorage.getItem(LS_THEME) === 'dark' ? 'dark' : 'light';
let view = 'accounts';
let overviewData = null, cfgLoaded = null;
let logPin = true, loginState = null, loginTimer = null;
let refTimer = null;
// lastFocus 记录弹层打开前的焦点，关闭后归还——键盘用户不会丢失位置。
let lastFocus = null;
// 用量「模型倍率回填」节流时间戳。必须声明在文件顶部：go() 在顶层被调用，落到
// #usage 分支时会同步进入 warmUsageModelRates()，声明在下方就是 TDZ
// ReferenceError（旧版直接刷新用量页即报 "Cannot access 'usageRateWarmAt'
// before initialization"），async 只是把它变成被 catch 吞掉的 rejection。
let usageRateWarmAt = 0;
// 请求记录的视图选择与筛选条件。表格是上游形态（默认），行式是本 fork 移植前的
// 原始流水行——两种都留着，选择记在 localStorage 跨会话记住。
// 与 usageRateWarmAt 同理必须声明在文件顶部：顶层 go() 落到 #logs 时会同步调进
// loadLogs，声明在下方就是 TDZ ReferenceError。
const LS_REQVIEW = 'wb2api.reqview';
let reqView = localStorage.getItem(LS_REQVIEW) === 'line' ? 'line' : 'table';
let reqEntries = [];   // 最近一次拉取到的记录（表格的筛选基数，也是计数条的分母）
let reqFilter = { client_ip: '', user_agent: '', account: '', model: '', outcome: '' };

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 两态翻转（浅/深），默认浅色；点击总是切换可见外观。深色是次要模式：
   所有组件样式先在浅色下定稿，深色只做同一套语义 token 的色阶替换。 */
function effTheme() { return theme === 'dark' ? 'dark' : 'light'; }
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  $('icoTheme').innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  $('btnTheme').title = eff === 'light' ? '切换到深色' : '切换到浅色';
}
$('btnTheme').onclick = () => {
  theme = effTheme() === 'light' ? 'dark' : 'light';
  localStorage.setItem(LS_THEME, theme);
  applyTheme();
};
applyTheme();

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  if (r.status === 401) { openKey(); throw new Error('密钥无效或未填写'); }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  return d;
}
/* toast 提示：容器带 aria-live="polite"（读屏会播报），最多同时 4 条，
   鼠标悬停暂停自动消失（长文案来得及读完）。 */
function toast(msg, cls) {
  const box = $('toasts');
  while (box.children.length >= 4) box.firstElementChild.remove();
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  box.appendChild(el);
  let timer = setTimeout(() => el.remove(), 3600);
  el.addEventListener('mouseenter', () => clearTimeout(timer));
  el.addEventListener('mouseleave', () => { timer = setTimeout(() => el.remove(), 1600); });
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}
function parseAPITime(value) {
  const text = String(value || '');
  if (!text || text.startsWith('0001-')) return 0;
  const ms = Date.parse(text);
  return Number.isFinite(ms) ? ms : 0;
}
function fmtLocalDateTime(ms) {
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}
function rateLimitMeta(row, now) {
  const model = String(row && row.model || '未知模型');
  const kind = String(row && row.kind || 'rate_limit');
  const resetAt = parseAPITime(row && row.reset_at);
  const until = parseAPITime(row && row.until);
  const deadline = resetAt || until;
  const remaining = deadline > now ? Math.round((deadline - now) / 1000) : 0;
  if (kind === 'model_unavailable') {
    return {
      model,
      kind,
      detail: remaining ? '预计 ' + dur(remaining) + ' 后重试' : '等待重新探测',
      title: model + '\n模型当前不可用' + (deadline ? '\n最早重试：' + fmtLocalDateTime(deadline) : ''),
    };
  }
  let detail = resetAt
    ? '预计 ' + fmtLocalDateTime(resetAt) + ' 解封' + (remaining ? '（剩余 ' + dur(remaining) + '）' : '')
    : (until ? '预计 ' + fmtLocalDateTime(until) + ' 恢复（剩余 ' + dur(remaining) + '）' : '预计解封时间未知');
  const title = [model, resetAt ? '上游重置：' + fmtLocalDateTime(resetAt) : '上游重置：时间未知'];
  if (until && resetAt && until < resetAt) {
    detail += ' · 网关最快 ' + dur(Math.max(0, Math.round((until - now) / 1000))) + ' 后重试';
    title.push('网关最早重试：' + fmtLocalDateTime(until));
  }
  return { model, kind, detail, title: title.join('\n') };
}
/* 账号表用量列（今日 chip + 悬浮提示里的累计值）专用：单位一律大写（K/M/B）、
   小数固定两位，量纲由单位字母承载，不再拼 "tok" 后缀——列内数字等宽对齐，
   7.1mtok → 7.10M。1000 以下仍是精确整数（不凑两位小数，免得把 0 写成 0.00、
   把 320 写成 320.00）。 */
function formatTokenCount(tokens) {
  if (tokens == null || tokens === '') return '—';
  const n = Number(tokens);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return String(Math.round(n));
  const units = [['K', 1e3], ['M', 1e6], ['B', 1e9]];
  let unit = units[0];
  for (const candidate of units) {
    if (n >= candidate[1]) unit = candidate;
  }
  let text = (n / unit[1]).toFixed(2);
  // 999999 → 1.00M，而不是 1000.00K；四舍五入后自动升级单位。
  const next = units[units.indexOf(unit) + 1];
  if (next && Number(text) >= 1000) {
    unit = next;
    text = (n / unit[1]).toFixed(2);
  }
  return text + unit[0];
}

function formatLatency(ms) {
  if (ms == null || ms === '') return '—';
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}
function formatRate(rate) {
  if (rate == null || rate === '') return '—';
  const n = Number(rate);
  if (!Number.isFinite(n) || n < 0) return '—';
  return n.toFixed(1) + 'tok/s';
}

/* ── 弹层通用管理 ─────────────────────────────────────────────────── */
/* 四个弹层（任务 / 券码 / 添加账号 / 密钥）共用一套打开-关闭语义：
   打开时聚焦首个可交互控件并记住来源焦点；Esc 关闭（data-lock="1" 的密钥门
   除外——它是鉴权前置，必须由用户显式输入）；Tab 在弹层内循环（焦点陷阱）；
   关闭后焦点归还来源。 */
function veilStack() {
  const list = document.querySelectorAll('.veil.on');
  return list.length ? list[list.length - 1] : null;
}
function veilFocusables(el) {
  return [...el.querySelectorAll('button, input, select, textarea, a[href]')]
    .filter(n => !n.disabled && !n.hidden && n.offsetParent !== null);
}
function openVeil(id) {
  const el = $(id);
  if (!el) return;
  lastFocus = document.activeElement;
  el.classList.add('on');
  // 优先聚焦首个表单控件；纯展示型弹层（任务 / 券码）聚焦弹层本身，而不是随便
  // 挑一个按钮——否则一打开就有一个按钮带焦点环，视线被引到非主操作上。
  const field = el.querySelector('input:not([type="hidden"]), select, textarea');
  const dlg = el.querySelector('.dlg');
  if (dlg && !dlg.hasAttribute('tabindex')) dlg.setAttribute('tabindex', '-1');
  const target = field || dlg || el;
  setTimeout(() => target.focus(), 40);
}
function closeVeil(id) {
  const el = $(id);
  if (!el || el.dataset.lock === '1') return;
  el.classList.remove('on');
  if (lastFocus && document.contains(lastFocus)) { try { lastFocus.focus(); } catch (e) { /* 元素已移除 */ } }
  lastFocus = null;
}
addEventListener('keydown', ev => {
  const veil = veilStack();
  if (!veil) return;
  if (ev.key === 'Escape') {
    if (veil.dataset.lock === '1') return;
    ev.preventDefault();
    closeVeil(veil.id);
    return;
  }
  if (ev.key !== 'Tab') return;
  const items = veilFocusables(veil);
  if (!items.length) return;
  const first = items[0], last = items[items.length - 1];
  if (ev.shiftKey && document.activeElement === first) { ev.preventDefault(); last.focus(); }
  else if (!ev.shiftKey && document.activeElement === last) { ev.preventDefault(); first.focus(); }
});

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() { openVeil('keyVeil'); }
$('btnKey').onclick = async () => {
  const v = $('keyInput').value.trim();
  if (!v) return;
  localStorage.setItem(LS_KEY, v);
  try {
    await api('overview');
    $('keyErr').hidden = true;
    $('keyVeil').classList.remove('on');
    start();
  } catch (e) { $('keyErr').hidden = false; }
};
$('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });
/* #keyInput 现在待在独立 <form id="keyForm"> 里（游离的 password 框会被 Chromium
   合成「用户名 + 密码」凭证表单，把同页第一个文本框当成用户名）。单输入框的
   <form> 在回车时会隐式提交、把整页 GET 刷新一遍（CSP form-action 'none' 只是
   拦下导航，面板状态照样丢），所以拦住 submit。CSP 禁内联 onsubmit，只能在 JS 侧绑。 */
$('keyForm').addEventListener('submit', ev => ev.preventDefault());

/* ── 时间范围控件（用量 / 请求记录共用）──────────────────────────────
   一个控件适配两种宿主：
     · 用量页 rolling=true → 滚动预设发 hours：服务端按整点对齐的滚动窗口与旧
       行为逐位一致；前端自己减 N 小时会多算/少算一个边界桶。
     · 请求记录 rolling=false → 一律折算成 from/to：归档是线性日志，没有「整点
       对齐」这回事，「近 3 天」就是「三天前这一刻起」。
   「今天」与「自定义」在两种宿主下都发浏览器本地时区算好的 from/to。服务端
   时区未必与浏览器一致（容器常挂 TZ=Asia/Shanghai，浏览器可能在任意时区），
   让服务端算「今天」会在跨时区时切错日子。
   「今天」= 本地 00:00 起（自然日口径，本 fork 的既有特性），只发 from，上界
   交给服务端「到现在」：多发一个 to=now 会把正在走的那个小时桶切掉半截。
   旧实现把「今天」换算成 hours=当前小时+1 去套服务端的整点窗口，只有浏览器与
   服务端同时区时才等价；现在起点由浏览器精确给出，那层隐含依赖没有了。

   本段必须放在顶层 go()（路由首次分发）之前求值：深链 #usage / #logs 时 go()
   会同步调进 loadUsage/loadLogs 并读到这里的 const，位置太靠后就是 TDZ
   ReferenceError——首屏该页显示「读取失败」，点导航进去却正常。 */
const TRANGE_PRESETS = [
  ['today', '今天'],
  ['24', '近 24 小时'],
  ['72', '近 3 天'],
  ['168', '近 7 天'],
  ['720', '近 30 天'],
  ['0', '全部历史'],
  ['custom', '自定义…'],
];
/* 两个宿主的默认预设：用量页沿用旧 #usWindow 的「近 3 天」；请求记录默认
   「全部历史」——该页移植前没有任何时间过滤，不发 from/to 才是「默认展示不变」。 */
const TRANGE_DEFAULTS = { usRange: '72', reqRange: '0' };
const trangeStates = new Map(); // hostId → { preset, from: Date|null, to: Date|null }

// dtLocalValue / dtLocalParse 与 <input type=datetime-local> 的取值格式互转
// （YYYY-MM-DDTHH:mm，本地时区；ES 里"带时间的日期串"按本地解析，正是我们要的）。
function dtLocalValue(d) {
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}
function dtLocalParse(s) {
  if (!s) return null;
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d;
}

// trangeMidnight 今天 00:00（浏览器本地时区）——「今天」预设的唯一权威起点。
function trangeMidnight() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

function trangeState(id) {
  if (!trangeStates.has(id)) {
    // 「自定义」的初始值给一段有意义的默认：今天 00:00 → 现在。
    trangeStates.set(id, { preset: TRANGE_DEFAULTS[id] || '72', from: trangeMidnight(), to: new Date() });
  }
  return trangeStates.get(id);
}

// trangeRender 画出控件骨架（幂等：重复调用会保留当前状态）。
function trangeRender(id) {
  const host = $(id);
  if (!host) return;
  const st = trangeState(id);
  const custom = st.preset === 'custom';
  host.innerHTML =
    '<select class="tr-preset" aria-label="时间范围">' +
    TRANGE_PRESETS.map(([v, label]) =>
      '<option value="' + v + '"' + (v === st.preset ? ' selected' : '') + '>' + esc(label) + '</option>').join('') +
    '</select>' +
    '<span class="tr-custom"' + (custom ? '' : ' hidden') + '>' +
    '<input type="datetime-local" class="tr-from" value="' + esc(st.from ? dtLocalValue(st.from) : '') + '" aria-label="起始时间">' +
    '<span class="tr-sep">→</span>' +
    '<input type="datetime-local" class="tr-to" value="' + esc(st.to ? dtLocalValue(st.to) : '') + '" aria-label="结束时间">' +
    '</span>';
  const preset = host.querySelector('.tr-preset');
  if (preset) preset.onchange = () => {
    st.preset = preset.value;
    // 从别的预设切到自定义时，把区间重置为「今天 00:00 → 现在」，
    // 免得用户上次留下的半年区间被无声沿用。
    if (st.preset === 'custom' && (!st.from || !st.to)) { st.from = trangeMidnight(); st.to = new Date(); }
    trangeRender(id);
    trangeEmit(id);
  };
  const fromEl = host.querySelector('.tr-from');
  const toEl = host.querySelector('.tr-to');
  const readCustom = () => {
    st.from = dtLocalParse(fromEl.value);
    st.to = dtLocalParse(toEl.value);
    // 起止颠倒就地标红，且**不**发起查询：否则会把一个 to<from 的空区间当成
    // 有效条件打给服务端，页面上只剩「暂无数据」，看不出是自己填反了。
    // 不静默纠正——用户可能正输到一半（先改起点再改终点）。
    const bad = st.from && st.to && st.from > st.to;
    fromEl.classList.toggle('tr-bad', !!bad);
    toEl.classList.toggle('tr-bad', !!bad);
    if (bad) return;
    trangeEmit(id);
  };
  if (fromEl) fromEl.onchange = readCustom;
  if (toEl) toEl.onchange = readCustom;
}

const trangeHandlers = new Map();
// trangeBind 渲染控件并登记变化回调。**不**在绑定时触发回调：各视图的首次加载
// 由 go() 统一驱动，这里再触发一次会让打开页面时打两遍接口。
function trangeBind(id, onChange) {
  trangeHandlers.set(id, onChange);
  trangeRender(id);
}
function trangeEmit(id) {
  const fn = trangeHandlers.get(id);
  if (fn) fn();
}

// trangeQuery 把当前选择翻译成查询参数。
//   rolling=true  → 滚动预设发 hours（服务端整点对齐）；今天 / 自定义发 from/to
//   rolling=false → 一律发 from/to
// 「全部历史」在用量侧显式发 hours=0：后端把"没给参数"当作默认 72 小时，什么都不
// 发会让「全部历史」静默退化成「近 3 天」；归档侧没有 hours 口径，什么都不发。
function trangeQuery(id, rolling) {
  const st = trangeState(id);
  const q = new URLSearchParams();
  const sec = d => Math.floor(d.getTime() / 1000);
  if (st.preset === 'custom') {
    if (st.from) q.set('from', sec(st.from));
    if (st.to) q.set('to', sec(st.to));
    return q;
  }
  if (st.preset === 'today') {
    q.set('from', sec(trangeMidnight()));
    return q;
  }
  if (st.preset === '0') {
    if (rolling) q.set('hours', '0');
    return q;
  }
  if (rolling) { q.set('hours', st.preset); return q; }
  q.set('from', sec(new Date(Date.now() - Number(st.preset) * 3600 * 1000)));
  return q;
}

// trangeLabel 人读口径，用于「用量总览」这类需要回显当前选择的位置。
// 自定义区间给到分钟（与输入框精度一致）；服务端回显的 window_from/to 优先，
// 这里只是滚动窗口（没有回显）下的兜底。
function trangeLabel(id) {
  const st = trangeState(id);
  const found = TRANGE_PRESETS.find(p => p[0] === st.preset);
  if (st.preset !== 'custom') return found ? found[1] : '';
  if (!st.from && !st.to) return '自定义';
  const f = d => d ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' +
    String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0') : '…';
  return f(st.from) + ' → ' + f(st.to);
}

/* ── 路由 ─────────────────────────────────────────────────────────── */
/* 视图标题：导航与顶栏共用，统一 4 字（导航栏宽度固定 216px，标题长度一致
   才能让 7 个条目左对齐成一条竖线）。 */
const TITLES = { accounts: '账号管理', usage: '用量统计', packages: '积分构成', taskscenter: '任务中心', models: '模型档位', config: '系统配置', logs: '运行日志' };
/* skeletonRow 表格骨架行：加载态给形状不给文字，避免"正在查询…"把表格塌成一行。 */
function skeletonRows(cols, n) {
  const w = ['w-1', 'w-2', 'w-3'];
  let out = '';
  for (let i = 0; i < (n || 3); i++) {
    out += '<tr aria-hidden="true"><td colspan="' + cols + '">' +
      '<span class="skeleton ' + w[i % w.length] + '"></span></td></tr>';
  }
  return out;
}
function go(v) {
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => {
    const on = a.dataset.view === v;
    a.classList.toggle('on', on);
    if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  });
  $('ttl').textContent = TITLES[v];
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  if (v === 'usage') loadUsage();
  if (v === 'packages') loadPackages();
  if (v === 'taskscenter') reattachQueueView();
  // 视图切换后把焦点交给主区：读屏与键盘用户不会滞留在导航里。
  $('main').focus({ preventScroll: true });
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => { e.preventDefault(); go(a.dataset.view); history.replaceState(null, '', '#' + a.dataset.view); });
$('skipLink').onclick = () => setTimeout(() => $('main').focus({ preventScroll: true }), 0);
go((location.hash || '#accounts').slice(1) in TITLES ? (location.hash || '#accounts').slice(1) : 'accounts');

/* ── 账号池 ───────────────────────────────────────────────────────── */
/* avgTTFB / avgInferenceRate 累计平均在面板侧算：pool.TokenUsage 是要落盘
   state.json 的原始计数器，派生值不进那个结构体。
   · 首字 = ttfb_sum_ms / ttfb_count。样本 = 观测到首字的尝试（流式），
     非流式请求没有首字概念、不计入，所以这个均值天然只反映流式体验。
   · 推理 = 累计 inference_tokens_sum ÷ 累计 inference_ms_sum。分子分母同窗口、
     同条件累计（后端在同一个分支里同时加），分母已扣掉首字等待。
     注意不能用 completion_tokens 当分子：那是账号历史全量，而 ms 只覆盖新样本，
      窗口不一致会把速度放大几十倍（1.13.0 的线上实况：3096–20677 tok/s）。
   两者都返回 null 表示「无样本」，由调用方决定渲染成 —。 */
function avgTTFB(tu) {
  const n = (tu && tu.ttfb_count) || 0;
  return n > 0 ? ((tu.ttfb_sum_ms || 0) / n) : null;
}

function avgInferenceRate(tu) {
  const ms = (tu && tu.inference_ms_sum) || 0;
  return ms > 0 ? ((tu.inference_tokens_sum || 0) * 1000 / ms) : null;
}

/* okRateOf 累计成功率 = ok_count ÷ request_count（百分数）。列里显示的是今日口径，
   这个用于悬浮提示里的累计对照行。后端在同一次加锁内、按同一个
   事件（一次尝试的最终结果）同时累加这两个计数器，故恒有 ok <= req——成功率在结构上
   不可能超过 100%。
   不能用池的 success_count 当分子：它在「上游 2xx 刚开流」时就 +1，比尝试记账早整个
   流式时长，面板轮询必然采到「成功已加、尝试未加」的中间态（线上实况 662次 | 100.15%），
   而且空流/断连/error 帧这些最终失败的调用也已计入其中。
   字段缺失（新面板 + 旧后端）返回 null，由调用方渲染成 —，而不是把 0 当成「零成功」。 */
function okRateOf(tu) {
  const req = (tu && tu.request_count) || 0;
  if (typeof (tu && tu.ok_count) !== 'number' || req <= 0) return null;
  return tu.ok_count / req * 100;
}

/* todayRateOf 今日成功率 = (今日尝试 − 今日失败) ÷ 今日尝试（百分数）。
   两个数来自后端同一次聚合（用量记录器的今天窗口），所以恒有 errors <= requests，
   成功率不可能超过 100%；也正因为同源同窗口，它与「用量统计 → 今天」的失败数一致。
   分母为 0（今天没流量）或字段缺失时返回 null，由调用方渲染成 —。 */
function todayRateOf(td) {
  const req = (td && td.requests) || 0;
  if (!td || typeof td.errors !== 'number' || req <= 0) return null;
  const ok = req - td.errors;
  return (ok > 0 ? ok : 0) / req * 100;
}

/* statusTagOf 账号状态结论（纯函数，便于测试）：优先级 = 账号级状态 > 模型级受限 > 可用。
   账号自身可能既没冷却也没熔断，却已经有模型在限流或待重探——这时只显示「可用」会
   让人以为它完全健康（实测：标签写着可用，下面挂着「预计 10-01 11:14 解封」的琥珀条）。
   状态列只渲染这一个标签：具体原因（哪些模型、何时解封、禁用原因）全部放进 title，
   悬浮才显示——常显的原因会把行高撑得参差不齐，也让「状态」这一列喧宾夺主。
   coolText 由调用方格式化（本函数不依赖 dur）；rlMetas 是 rateLimitMeta 的结果数组。
   优先级：disabled > paused > 账号级冷却 > 模型级受限 > 可用——disabled 与 paused 同时
   置位时只报「已禁用」：禁用是更彻底的结论（保号任务默认也停），显示暂停会让人以为
   点一下「恢复选号」就能出量；paused 又排在冷却之前，因为「我手动按下的暂停」才是这个
   号眼下不出量的原因，只显示倒计时会让人以为等一等就自动回候选（点「解冻」同样不会）。 */
function statusTagOf(s, coolText, rlMetas) {
  const reason = (s && s.reason) ? String(s.reason) : '';
  if (s.disabled) {
    // 已禁用 + 已暂停：暂停标记被结论盖住，但仍是解冻后不出量的原因，放进悬浮提示里，
    // 免得运维解冻完盯着一个照常签到的号找不到「为什么还是选不上」。
    const bits = [];
    if (reason) bits.push(reason);
    if (s.paused) bits.push('同时处于暂停选号状态：解冻后还需点「恢复选号」才会回到候选。');
    return { cls: 'off', tone: 'bad', label: '已禁用', title: bits.join('\n') };
  }
  /* 已暂停选号（paused）：运维主动让位，不是故障。
     配色取舍：不用 bad——红在本 fork 专指需要干预的故障（禁用 / 熔断）；也不用 warn——
     琥珀在这里专指「冷却 / 熔断 / 模型限流」这类只能等它自己恢复的被动异常，运维扫视时
     会条件反射地去排查，而暂停点一下就能改回来。mute 的中性灰正是「人为、无需处理」。
     色条不新起一档：账号表只有 绿(在服务) / 琥珀(暂不服务) / 红(禁用) 三档（index.html
     的 tr.cool / tr.rl / tr.off），暂停属「暂不服务」，故复用 tr.cool 的琥珀条，红的 off
     只留给 disabled。 */
  if (s.paused) {
    return {
      cls: 'cool',
      tone: 'mute',
      label: '已暂停选号',
      title: (reason ? reason + '\n' : '') +
        '暂停期间照常签到 / 活跃上报 / 保活 / 刷新余额，只是退出选号候选。\n点「恢复选号」重新参与选号。',
    };
  }
  if (coolText) {
    return {
      cls: 'cool', tone: 'warn', label: coolText,
      title: (reason ? reason + '\n' : '') + '账号当前不可用，等待冷却/熔断恢复',
    };
  }
  const rl = rlMetas || [];
  if (rl.length) {
    const unavail = rl.filter(m => m.kind === 'model_unavailable').length;
    const limited = rl.length - unavail;
    // 同质时给出具体状态，混杂时统称异常——两者都要求悬浮能看到具体原因
    const label = limited && unavail ? '异常' : (unavail ? '待重探' : '限流');
    const lines = rl.length + ' 个模型受限：\n' +
      rl.map(m => '· ' + m.model + '：' + m.detail).join('\n');
    return {
      cls: 'rl',
      tone: 'warn',
      label: label + (rl.length > 1 ? ' · ' + rl.length + ' 个模型' : ''),
      title: reason ? lines + '\n' + reason : lines,
    };
  }
  return { cls: '', tone: 'ok', label: '可用', title: reason };
}

/* tokenChipHTML 把 formatTokenCount 的结果拆成「数值 + 单位」两段，单位包一层
   .usage-unit 拿到 2px 左间距：单位字母与数字同字号，紧贴时 110.98M 会被读成一个小
   数词，列内扫视只能靠字形差分辨量纲。没有单位的精确整数（320）与无数据的 '—' 原样
   输出，不产生多余元素（多出来的空 span 会让 chip 内边距看着变宽）。返回值是 HTML，
   故带 HTML 后缀，且两段都过 esc()。 */
function tokenChipHTML(value) {
  const m = /^([0-9][0-9.,]*)([KMB])$/.exec(String(value));
  if (!m) return esc(value);
  return esc(m[1]) + '<span class="usage-unit">' + esc(m[2]) + '</span>';
}

function renderAccounts(list) {
  const tb = $('accBody');
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="11"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号</div></td></tr>';
    return;
  }
  // 有总额度（credits_total）→ 进度条按自身 剩余/总额 百分比；旧数据无总额 → 退回池内最高=100%
  const maxCred = Math.max(1, ...list.map(s => s.credits || 0));
  tb.innerHTML = list.map(s => {
    const now = Date.now();
    const bl = (new Date(s.breaker_until || 0) - now) / 1000;
    const dg = (new Date(s.degrade_until || 0) - now) / 1000;
    const cool = Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
    // 模型级受限（限流 / 待重探）。账号自身可能既没冷却也没熔断，却已有模型不可用——
    // 这时只显示「可用」会让人以为它完全健康（实测：标签写着可用，下面挂着
    // 「预计 10-01 11:14 解封」的琥珀条）。
    const rl = Array.isArray(s.rate_limited_models) ? s.rate_limited_models.filter(m => m && m.model) : [];
    const rlMetas = rl.map(m => rateLimitMeta(m, now));
    // 账号级冷却文案（kind 判定沿用原逻辑），与模型级受限一起交给 statusTagOf 定结论
    const coolKind = bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0) ? '熔断'
      : (dg > (s.cool_remaining_sec || 0) ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
    const st = statusTagOf(s, cool > 0 ? coolKind + ' · ' + dur(cool) : '', rlMetas);
    const cls = st.cls;
    // 状态列只放这一个标签：原因（模型限流明细 / 禁用原因）在 title 里，悬浮才显示。
    const tag = '<span class="tag ' + st.tone + '"' +
      (st.title ? ' title="' + esc(st.title) + '"' : '') + '>' + esc(st.label) + '</span>';
    const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
    const cred = s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits));
    const pct = s.credits_total > 0
      ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
      : Math.round((s.credits || 0) / maxCred * 100);
    // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），运维据此
    // 看「为什么总选它」——免费号垄断 / 单价排序一眼可见。
    let credTip = s.credits_total > 0 ? '剩余 ' + s.credits + ' / 总额 ' + s.credits_total + '（' + pct + '%）' : '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool > 0;
    const tu = s.token_usage || {};
    // 「今日调用 / 今日用量 / 成功率」= 自然日口径，取自后端 overview 合并的用量记录器
    // 今天窗口（与「用量统计 → 今天」同源同口径）。累计全量仍在池里，放进悬浮提示对照。
    const td = s.today || {};
    const hasToday = typeof td.requests === 'number';
    const todayReq = td.requests || 0;
    const todayErr = typeof td.errors === 'number' ? td.errors : null;
    const todayTok = formatTokenCount(td.total_tokens);
    const dayLabel = '今天' + (td.day ? '（' + td.day + '）' : '');
    // 累计对照值
    const cumTok = formatTokenCount(tu.total_tokens);
    const cumReq = tu.request_count || 0;
    const cumOk = typeof tu.ok_count === 'number' ? tu.ok_count : null;
    const cumRate = okRateOf(tu);
    const latency = formatLatency(tu.last_latency_ms);
    // 成功率 = (今日尝试 − 今日失败) ÷ 今日尝试（见 todayRateOf 的口径说明）。
    const okRate = todayRateOf(td);
    const rateCls = okRate == null ? 'c-muted' : (okRate >= 99 ? 'c-ok' : (okRate >= 95 ? 'c-warn' : 'c-bad'));
    let callsTitle = hasToday
      ? dayLabel + '尝试 ' + todayReq + ' 次' + (todayErr == null ? '' : ' / 失败 ' + todayErr) +
        '\n成功率 = (尝试 − 失败) ÷ 尝试（同一次聚合，故不会超过 100%）'
      : '今日数据需要 1.13.3 起的后端';
    callsTitle += '\n累计：尝试 ' + cumReq + ' 次' +
      (cumOk == null ? '' : ' / 成功 ' + cumOk + ' / 失败 ' + (cumReq - cumOk)) +
      (cumRate == null ? '' : '（' + cumRate.toFixed(2) + '%）');
    const ttfb = avgTTFB(tu);
    const infer = avgInferenceRate(tu);
    let usageTitle = hasToday
      ? dayLabel + '用量 ' + todayTok + ' token'
      : '今日数据需要 1.13.3 起的后端';
    usageTitle += '\n累计 ' + cumTok + ' token / 尝试 ' + cumReq + ' 次' +
      (latency === '—' ? '' : '\n最近一次总延迟 ' + latency);
    const ttfbTitle = ttfb == null
      ? '暂无首字样本（非流式请求没有首字概念）'
      : '累计平均首字 ' + formatLatency(ttfb) + '（样本 ' + tu.ttfb_count + ' 次）' +
        (tu.last_ttfb_ms ? '\n最近一次 ' + formatLatency(tu.last_ttfb_ms) : '');
    const inferTitle = infer == null
      ? '暂无推理样本'
      : '累计平均推理速度 ' + formatRate(infer) + '（已排除首字等待）' +
        '\n= 累计 completion token ÷ 累计生成耗时';
    return '<tr class="' + cls + '" title="uid: ' + esc(s.uid) + '">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + (s.nickname ? esc(s.nickname) : '<span class="c-muted">未命名</span>') + (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + '</div><div class="id">' + esc(short) + '</div></td>' +
      '<td>' + tag + '</td>' +
      '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar"><i style="--w:' + pct + '%"></i></div></td>' +
      '<td class="num calls-cell" title="' + esc(callsTitle) + '">' +
        '<span class="calls-n"><b>' + (hasToday ? todayReq : '—') + '</b>' + (hasToday ? '<em>次</em>' : '') + '</span>' +
        '<span class="sep">|</span>' +
        '<span class="' + rateCls + '">' + (okRate == null ? '—' : okRate.toFixed(2) + '%') + '</span></td>' +
      '<td class="num">' + (s.in_flight || 0) + '</td>' +
      '<td class="num usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line">' +
        '<span class="usage-item usage-total"><b>' + tokenChipHTML(todayTok) + '</b></span>' +
      '</span></td>' +
      // 首字/推理：无样本时不渲染空 chip（旧版恒渲染会留下一个绿色的「—」）
      '<td class="num ttfb-cell" title="' + esc(ttfbTitle) + '">' +
        (ttfb == null ? '<span class="c-muted">—</span>'
          : '<span class="usage-line"><span class="usage-item"><b>' + formatLatency(ttfb) + '</b></span></span>') + '</td>' +
      '<td class="num speed-cell" title="' + esc(inferTitle) + '">' +
        (infer == null ? '<span class="c-muted">—</span>'
          : '<span class="usage-line"><span class="usage-item"><b>' + formatRate(infer) + '</b></span></span>') + '</td>' +
      '<td class="num c-muted">' + ago(s.last_success) + '</td>' +
      '<td class="acts">' +
        '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '"' + (s.checkin_done ? ' title="今日已签到；点击可重新签到并刷新余额"' : '') + '>' + (s.checkin_done ? '已签' : '签到') + '</button>' +
        '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '">余额</button>' +
        '<button class="xs ghost" data-a="tasks" data-u="' + esc(s.uid) + '">任务</button>' +
        (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '">解冻</button>'
                : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.uid) + '">禁用</button>') +
        /* 暂停 / 恢复选号（上游 paused 合并过来的操作）：「解冻/禁用」旁并列的第二个开关，
           与 disable 正交且不能互相替代——pause 只退出选号，签到 / 活跃上报 / 保活 /
           刷新余额照常；disable 默认连保号任务一起停（include_disabled_in_tasks 才是例外）。
           也不与冷却互斥：冷却中的号同样可以主动让位，已暂停的号在冷却期间也应能立刻恢复
           （意图立即生效，不必等冷却走完），故这一个按钮不看 frozen，只看 paused。
           已禁用账号不渲染它：禁用已含「不参与选号」，两个按钮并列会让人以为效果能叠加，
           也容易误以为禁用号的任务还能靠暂停挽回（本 fork 的 include_disabled_in_tasks
           是全局开关，不是账号级补救）。 */
        (s.disabled ? ''
          : (s.paused
              ? '<button class="xs primary" data-a="resume" data-u="' + esc(s.uid) + '">恢复选号</button>'
              : '<button class="xs ghost" data-a="pause" data-u="' + esc(s.uid) + '" title="退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额">暂停选号</button>')) +
        '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.uid) + '">移除</button>' +
      '</td></tr>';
  }).join('');
}

// renderModelLocks 模型锁池：哪些模型不能用、锁了几个号、还要锁多久。
// 后端 model_locks 已按「整池不可用 → 没号可用 → 部分限流」排好序，这里只做展示。
// 本 fork 的改写：域复用账号表的 .realm-tag、状态复用 .tag 体系（不另起平行徽标），
// 倒计时复用 dur()（绝对时刻进 title——列里塞长日期会把表顶宽），空态复用 .empty 卡。
function renderModelLocks(rows) {
  const tb = $('mlBody');
  if (!tb) return;
  const note = $('mlNote');
  const list = Array.isArray(rows) ? rows.filter(r => r && r.model) : [];
  if (!list.length) {
    // 无锁时不渲染空表：空 tbody 只剩表头，看着像功能坏了，给一句结论。
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">' +
      '<div class="big">所有模型均可选</div>' +
      '当前没有模型级限流 —— 没有任何账号因某个模型被挡</div></td></tr>';
    if (note) note.textContent = '';
    return;
  }
  const STATE = { locked: ['bad', '整池不可用'], starved: ['warn', '没号可用'], partial: ['warn', '部分限流'] };
  // 解锁时刻：列里只放「还剩多久」（倒计时才是运维要判断的量），绝对时刻进 title。
  const left = iso => {
    const ms = parseAPITime(iso);
    if (!ms) return { t: '—', title: '无明确解锁时刻' };
    const sec = Math.max(0, Math.round((ms - Date.now()) / 1000));
    return {
      t: dur(sec),
      title: '解锁于 ' + fmtLocalDateTime(ms) + (sec > 0 ? '（' + dur(sec) + '后）' : '（已到点，等待下一轮探测）'),
    };
  };
  tb.innerHTML = list.map(r => {
    const st = STATE[r.state] || ['mute', esc(r.state || '—')];
    const realm = r.realm === 'global' ? '国际版' : '国内版';
    // 最早解锁兜底用全池解锁：后端两字段同时给出，缺一列也不至于空着。
    const first = left(r.unlock_at || r.fully_unlock_at);
    const full = left(r.fully_unlock_at);
    return '<tr>' +
      '<td>' + esc(r.model) + '</td>' +
      '<td><span class="realm-tag">' + realm + '</span></td>' +
      '<td><span class="tag ' + st[0] + '">' + st[1] + '</span></td>' +
      '<td class="num">' + (r.servable || 0) + ' <span class="c-muted">/</span> ' + (r.total || 0) + '</td>' +
      '<td class="num">' + (r.locked || 0) + '</td>' +
      '<td class="num" title="' + esc(first.title) + '">' + first.t + '</td>' +
      '<td class="num" title="' + esc(full.title) + '">' + full.t + '</td>' +
      '<td class="reason">' + (r.reason ? esc(r.reason) : '<span class="c-muted">—</span>') + '</td>' +
      '</tr>';
  }).join('');
  if (note) {
    const bad = list.filter(r => r.state === 'locked' || r.state === 'starved').length;
    note.textContent = bad ? bad + ' 个模型整池不可用' : list.length + ' 个模型部分限流';
  }
}

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    overviewData = d;
    $('sTotal').textContent = d.total;
    $('sHealthy').textContent = d.healthy;
    $('sCooling').textContent = d.cooling;
    $('sDisabled').textContent = d.disabled;
    const remSum = (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0);
    const totSum = (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0);
    $('sCredits').textContent = totSum > 0 ? remSum + ' / ' + totSum : remSum;
    $('sSticky').textContent = d.sticky_sessions;
    // 版本只在侧栏底栏展示一次：旧版品牌区与底栏重复显示同一版本号，观感冗余。
    // 品牌区改展示部署形态（本地内存 / Redis 镜像），信息不重复。
    $('navSub').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
    $('navVer').textContent = 'v' + d.version;
    $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    const p = $('navPulse');
    p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '';
    const up = Math.floor(d.uptime_sec);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    renderAccounts(d.accounts || []);
    renderModelLocks(d.model_locks);
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

$('accBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  // disable 的确认文案里点名「暂停选号」：两者都让账号退出选号，但暂停可逆、保号任务照常，
  // 正是「只想临时让位」的人要的那个——不问一句，多数人会直接用更重的禁用。
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号，需手动解冻才能恢复。若只是想临时让位、仍要保号，可改用「暂停选号」。确认禁用？')) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'pause') {
      /* 刻意不弹确认（与 disable 不同）：pause 可逆、无损，保号任务照常跑，点错了再点
         「恢复选号」即可；「让某个号临时让位」是高频运维动作，每次都多一次点击只增加
         摩擦。remove / disable 这类不可逆或有副作用的操作才需要确认。 */
      await api('accounts/' + encodeURIComponent(u) + '/pause', { method: 'POST' });
      toast('已暂停选号（签到 / 保活照常）', 'ok');
    } else if (a === 'resume') {
      await api('accounts/' + encodeURIComponent(u) + '/resume', { method: 'POST' });
      toast('已恢复选号', 'ok');
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'remove') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; loadOverview(true); }
});

$('btnCheckinAll').onclick = async () => {
  try { await api('checkin_all', { method: 'POST' }); toast('全部签到已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnKeepaliveAll').onclick = async () => {
  try { await api('keepalive_all', { method: 'POST' }); toast('全部保活已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnTravelAll').onclick = async () => {
  try { await api('travel_all', { method: 'POST' }); toast('旅行巡检已开始（含领养链路），结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnActivityAll').onclick = async () => {
  try { await api('activity_all', { method: 'POST' }); toast('活跃上报已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /panel/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span class="c-warn strong">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span class="c-ok">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span class="c-muted">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span class="c-muted">?</span><div class="note">未测出' + stale + '</div></td>';
}

/* rateCell 倍率列：牌价 vs 生效价。上游 credits 是牌价（转正后基准倍率），
   modelPromotions 给当前生效折扣（限时免费 factor=0 / 夜间五折 0.5 等）——
   WorkBuddy 客户端显示的正是生效价。有折扣：生效价大字 + 标签 + 划线牌价，
   悬停带时段说明；无 factor 只有标签（错峰类）：牌价 + 标签。 */
function rateCell(m) {
  const tip = m.promo_note ? ' title="' + esc(m.promo_note) + '"' : '';
  if (m.promo_factor != null && m.promo_credits) {
    const base = m.credits ? ' <s class="c-muted fs-12">' + esc(m.credits) + '</s>' : '';
    const label = m.promo_label ? ' <span class="tag info">' + esc(m.promo_label) + '</span>' : '';
    return '<span' + tip + ' class="help"><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
  }
  if (m.promo_label) {
    return '<span' + tip + ' class="help">' + (m.credits ? esc(m.credits) : '—') +
      ' <span class="tag info">' + esc(m.promo_label) + '</span></span>';
  }
  return m.credits ? esc(m.credits) : '—';
}

async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = skeletonRows(7, 4);
  try {
    // 探测数据是可选增强：拉取失败或返回非对象都不影响模型列表本身。
    // （.catch 只兜住 reject；200 + 非对象 body 会原样落到下面，故这里显式判空，
    //   否则一个可选接口的畸形响应会让整张模型表塌成一行报错。）
    const [d, pr] = await Promise.all([api('models'), api('model_probes').catch(() => null)]);
    const list = (d && d.models) || [];
    if (!list.length) { tb.innerHTML = '<tr><td colspan="7"><div class="empty">上游未返回模型</div></td></tr>'; return; }
    const probes = (pr && pr.probes) || {};
    const probeKeys = Object.keys(probes);
    const probeOf = id => probes[id] || probes[probeKeys.find(k => k.endsWith(':' + id))];
    tb.innerHTML = list.map(m => {
      const eff = (m.supported_efforts || []).slice();
      if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
      const effs = eff.length ? eff.map(e => '<span class="tag info">' + esc(e) + '</span>').join(' ')
        : '<span class="c-muted fs-12">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
      // 能力徽标：默认模型 / 工具调用 / 视觉 / 纯推理（上游目录全字段透出，缺失不显示）。
      // 能力是中性元信息，用 mute；只有「需要你关注」的状态才用琥珀。
      const caps = [];
      if (m.is_default) caps.push('<span class="tag ok">默认</span>');
      if (m.supports_tool_call) caps.push('<span class="tag mute">工具</span>');
      if (m.supports_images) caps.push('<span class="tag mute">视觉</span>');
      if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag mute">思考常开</span>');
      const capHtml = caps.length ? '<div class="id mt-1">' + caps.join(' ') + '</div>' : '';
      const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
      return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm">' + esc(m.id) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
        '<td class="num">' + rateCell(m) + '</td>' +
        '<td>' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span class="c-muted">—</span>') + '</td>' +
        '<td class="efs">' + effs + '</td>' +
        '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
        outCell(m, probeOf(m.id)) + '</tr>';
    }).join('');
    const hit = list.filter(m => probeOf(m.id)).length;
    $('mdNote').textContent = list.length + ' 个模型 · 已刷新降级缓存' + (hit ? ' · ' + hit + ' 个有实测上限' : '');
  } catch (e) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
  }
}
$('btnModels').onclick = loadModels;

/* ── 日志（频道：全部/任务/对话/系统） ─────────────────────────────── */
let logCh = 'all';
$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  loadLogs();
});
async function loadLogs() {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  // 时间范围与筛选栏都交给归档侧过滤，不是前端筛已拉取的行：命中的记录可能落在
  // 「最近 N 条」之外，必须让服务端按条件取。
  // 默认「全部历史」+ 空筛选 → 查询串与移植前逐字相同（request_logs?limit=100）。
  const rq = reqQuery();
  reqLoading();
  try {
    const [d, metrics, requestRows] = await Promise.all([
      api('logs'),
      api('request_metrics').catch(() => ({})),
      api('request_logs?' + rq.toString()).catch(() => ({ entries: [] })),
    ]);
    // 时间范围生效时**不**回落到内存里的「最近请求」：那是另一套口径。「区间内没有
    // 记录」是一个真实结果，回落会把范围之外的请求显示出来，看着像筛选没生效。
    // 默认（全部历史，不发 from/to）保持原来的回落行为逐字不变。
    const rangeOn = rq.has('from') || rq.has('to');
    const recent = rangeOn
      ? (requestRows.entries || [])
      : ((requestRows.entries && requestRows.entries.length) ? requestRows.entries : (metrics.recent || []));
    renderRequestMetrics(metrics, recent);
    const entries = (d.entries || []).filter(e => logCh === 'all' || e.ch === logCh);
    box.innerHTML = entries.length
      ? entries.map(e => {
        const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
        const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
        const ch = logCh === 'all' ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>' : '';
        return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
      }).join('')
      : '<span class="c-muted">暂无日志</span>';
    if (logPin && atEnd) box.scrollTop = box.scrollHeight;
    const counts = {};
    for (const e of (d.entries || [])) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : logCh === 'chat' ? '对话' : '系统') + ' ' + entries.length + ' 行';
  } catch (e) {
    /* 概览/密钥门已提示失败原因。表格里若留着加载骨架会一直像「正在加载」，
       这里就地收尾（写法同 loadModels 的失败态）；行式视图不动——它的内容是上一次
       成功渲染的结果，与移植前的表现一致。 */
    if (reqView === 'table') {
      const tb = $('reqBody');
      if (tb) tb.innerHTML = '<tr><td colspan="10"><div class="empty">' + esc(e.message) + '</div></td></tr>';
    }
  }
}

// 请求记录的时间范围控件：任何改动（预设切换 / 自定义起止）都重新拉一次归档。
// 绑定本身不发请求——首次加载由 go() 驱动，否则打开日志页会打两遍接口。
if ($('reqRange')) trangeBind('reqRange', loadLogs);

/* ── 请求记录：筛选栏 + 视图切换（表格 / 行式）──────────────────────
   筛选控件 → 请求参数 → 重新拉取。字段名与 panel.requestLogs 一一对应，
   前端只做「取值 + 拼查询串」，不自己重写一套匹配语义。 */
const REQ_FIELDS = { reqIP: 'client_ip', reqUA: 'user_agent', reqAccount: 'account', reqModel: 'model' };

function reqSyncFilter() {
  for (const [id, key] of Object.entries(REQ_FIELDS)) {
    const el = $(id);
    reqFilter[key] = el ? String(el.value || '').trim() : '';
  }
  const oc = $('reqOutcome');
  if (oc) reqFilter.outcome = oc.value || '';
}

/* reqQuery 把筛选栏 + 时间范围 + 条数拼成请求记录接口的查询串。顺序固定为
   「时间范围 → limit → 筛选条件」：默认（全部历史 + 空筛选）时逐字就是移植前的
   request_logs?limit=100，既有断言（含时间范围那批）钉得住。 */
function reqQuery() {
  const q = trangeQuery('reqRange', false);
  const limit = $('reqLimit') && $('reqLimit').value;
  q.set('limit', limit || '100');
  for (const key of ['client_ip', 'user_agent', 'account', 'model']) {
    if (reqFilter[key]) q.set(key, reqFilter[key]);
  }
  if (reqFilter.outcome) q.set('outcome', reqFilter.outcome);
  return q;
}

/* reqLoading 拉取期间的加载态：表格视图铺既有骨架行（skeletonRows，与账号表/
   任务表同一写法），行式视图什么都不做——那条路径的输出（含空态文案）是逐字节
   钉住的，不能多一行占位。 */
function reqLoading() {
  if (reqView !== 'table') return;
  const tb = $('reqBody');
  if (tb) tb.innerHTML = skeletonRows(10, 4);
}

/* reqPaintView 把分段控件的选中态与两个容器的显隐对齐到 reqView。 */
function reqPaintView() {
  for (const c of document.querySelectorAll('#reqViews .chip')) c.classList.toggle('on', c.dataset.rv === reqView);
  const wrap = $('reqTableWrap'), box = $('reqLogBox');
  if (wrap) wrap.hidden = reqView !== 'table';
  if (box) box.hidden = reqView === 'table';
}

/* reqSetView 切换视图：只重画当前容器，不重拉数据（reqEntries 就是刚拉到的那批）。
   localStorage 读写都可能抛（隐私模式 / 无存储宿主），一律吞掉——存不下只是下次
   打开不记得，不该让切换本身失败。 */
function reqSetView(v) {
  reqView = v === 'line' ? 'line' : 'table';
  try { localStorage.setItem(LS_REQVIEW, reqView); } catch (e) { /* 只影响下次打开 */ }
  reqPaintView();
  renderRequestLog();
}
if ($('reqViews')) $('reqViews').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-rv]');
  if (b) reqSetView(b.dataset.rv);
});

// 文字筛选防抖 250ms：每敲一个字符打一次接口会把归档读穿（UA 这种长串尤其）。
// 下拉（结果 / 条数）不防抖——一次点击就是一次明确的查询。
let reqTimer = null;
function reqOnType() {
  reqSyncFilter();
  clearTimeout(reqTimer);
  reqTimer = setTimeout(loadLogs, 250);
}
for (const id of Object.keys(REQ_FIELDS)) if ($(id)) $(id).oninput = reqOnType;
if ($('reqOutcome')) $('reqOutcome').onchange = () => { reqSyncFilter(); loadLogs(); };
if ($('reqLimit')) $('reqLimit').onchange = loadLogs;
if ($('btnReqReload')) $('btnReqReload').onclick = loadLogs;

// 首屏恢复上次的视图选择（index.html 的默认骨架是表格态，这里只处理「上次选了行式」）。
reqPaintView();

function renderRequestMetrics(m, entries) {
  m = m || {};
  const a = m.archive || {};
  $('reqSummary').textContent =
    '已完成 ' + fmtTok(m.completed) +
    ' · 成功 ' + (m.success_rate == null ? '—' : Number(m.success_rate).toFixed(1) + '%') +
    ' · HTTP ' + (m.http_success_rate == null ? '—' : Number(m.http_success_rate).toFixed(1) + '%') +
    ' · 平均 ' + fmtMs(m.avg_duration_ms) +
    ' · 进行中 ' + String(m.in_flight || 0);
  $('reqNote').textContent = a.enabled
    ? 'JSONL 归档 ' + fmtBytes(a.bytes) + (a.dropped_writes ? ' · 丢弃 ' + a.dropped_writes + ' 条' : '') +
      (a.last_error ? ' · 错误：' + a.last_error : '')
    : '仅内存指标，JSONL 归档已关闭';

  // 记录交给当前视图渲染（表格 / 行式），计数条与来源提示跟着一起更新。
  reqEntries = entries || [];
  renderRequestLog();
}

/* reqMatch 单行匹配：与后端 reqlog.Filter 同一口径——client_ip / user_agent /
   account / model 是大小写不敏感的子串匹配，outcome 精确匹配。
   服务端已经按同一套条件筛过一遍，这里再匹配一次是为了「归档关闭」时前端会回落
   到内存里的最近请求（metrics.recent）——那批数据没经过服务端筛选，不兜一层的话
   筛选栏看起来完全失效（表格里仍是全部记录）。 */
function reqMatch(e, f) {
  f = f || reqFilter;
  if (f.outcome && String(e && e.outcome || '') !== f.outcome) return false;
  for (const key of ['client_ip', 'user_agent', 'account', 'model']) {
    const needle = String(f[key] || '').trim().toLowerCase();
    if (needle && !String((e && e[key]) || '').toLowerCase().includes(needle)) return false;
  }
  return true;
}
function reqList(f) { return reqEntries.filter(e => reqMatch(e, f)); }

/* reqOutcomeTag 结果列：HTTP 状态码 + outcome 标签，色走既有 .tag 语义色
   （成功 ok / 中断 warn / 出错 bad / 无结果 mute），不新起一套徽标。 */
function reqOutcomeTag(e) {
  const outcome = String(e && e.outcome || '');
  const label = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' }[outcome] ||
    outcome || '—';
  const cls = outcome === 'success' ? 'ok' : outcome === 'interrupted' ? 'warn' : outcome ? 'bad' : 'mute';
  // 状态码缺失时不拼出「— —」（上游写法）：两者都没有就只留一个 —。
  const status = e && e.status ? String(e.status) : '';
  return '<span class="tag ' + cls + '">' + esc((status + ' ' + label).trim()) + '</span>';
}

/* reqTokenCell Token 列：总量 + 缓存命中率。命中率只在有观测（命中+未命中>0）时
   出现——老归档行没有 cache_* 字段，凭空补一个「—」会被读成「缓存完全没命中」。
   命中率直接复用用量明细表的 cacheRateCell：同一口径、同一分档色，两处不漂移。 */
function reqTokenCell(e) {
  const total = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  const cell = total ? fmtTok(total) : '<span class="muted">—</span>';
  const observed = Number(e && e.cache_hit_tokens || 0) + Number(e && e.cache_miss_tokens || 0);
  if (!observed) return cell;
  return cell + '<span class="req-hit">' + cacheRateCell(e.cache_hit_tokens, e.cache_miss_tokens) + '</span>';
}

/* reqCreditCell 积分列：credit_known=false（未计费/请求没走完）显示 —。「不知道」
   和「0 分」是两回事，都写成 0.00 会让对账时分不清。 */
function reqCreditCell(e) {
  if (!e || !e.credit_known) return '<span class="muted">—</span>';
  const v = Number(e.credit);
  return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '<span class="muted">—</span>';
}

/* renderRequestTable 渲染请求记录表。来源列是这一版的重点：IP 用等宽字体方便扫，
   UA 单行截断（完整值在 title 里，整行用 requestLogText 作 tooltip，与行式视图
   看到的是同一份文本）。
   老归档（来源功能上线前写入）没有 client_ip/user_agent：显示 — 而不是空白——
   空白会被读成「这一列坏了」。 */
function renderRequestTable(list) {
  const tb = $('reqBody');
  if (!tb) return;
  const rows = list || reqList();
  tb.innerHTML = rows.map(e => {
    const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
    const ip = (e && e.client_ip) || '';
    const ua = (e && e.user_agent) || '';
    const rid = (e && e.request_id) || '';
    return '<tr title="' + esc(requestLogText(e)) + '">' +
      '<td class="num">' + esc(when) + '</td>' +
      '<td>' + reqOutcomeTag(e) + '</td>' +
      '<td>' + esc((e && e.model) || '—') + '</td>' +
      '<td>' + esc((e && e.account) || '—') + '</td>' +
      '<td>' + (ip ? '<span class="clip ip" title="' + esc(ip) + '">' + esc(ip) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td>' + (ua ? '<span class="clip" title="' + esc(ua) + '">' + esc(ua) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td class="num">' + fmtMs(e && e.duration_ms) + '</td>' +
      '<td class="num">' + reqTokenCell(e) + '</td>' +
      '<td class="num">' + reqCreditCell(e) + '</td>' +
      '<td>' + (rid ? '<span class="clip rid" title="' + esc(rid) + '">' + esc(rid) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="10" class="empty">' +
    (reqEntries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录') + '</td></tr>';
}

/* reqFiltered 当前是否带着筛选条件发过查询。服务端按条件取回的批次已经只剩命中
   项，本地匹配不会再去掉任何行——计数条必须靠这个标志说明「这不是全部」。 */
function reqFiltered() {
  return ['client_ip', 'user_agent', 'account', 'model', 'outcome'].some(k => reqFilter[k]);
}

/* renderRequestCount 计数条（筛选栏右侧）：命中数 / 总数。同一批记录里一条来源
   都没有时（开关关闭，或读到来源功能上线前写入的归档）在计数后点名「来源未记录」
   并转琥珀（.src-off）——否则表格里那两列整片 — 会被当成解析坏了。 */
function renderRequestCount(list) {
  const el = $('reqCount');
  if (!el) return;
  const total = reqEntries.length, shown = (list || []).length;
  const localFiltered = shown !== total;
  const queryFiltered = reqFiltered();
  const hasSource = reqEntries.some(e => e && (e.client_ip || e.user_agent));
  el.textContent = !total ? '—'
    : (localFiltered ? '命中 ' + shown + ' / ' + total + ' 条'
      : queryFiltered ? '命中 ' + shown + ' 条' : total + ' 条') + (hasSource ? '' : ' · 来源未记录');
  el.className = (total > 0 && (localFiltered || queryFiltered || !hasSource)) ? 'note src-off' : 'note';
}

/* renderRequestLog 按当前视图渲染请求记录：表格（.acc.req 的 tbody）或行式
   （移植前的 .ln 流水行）。两个容器各写各的，切换视图不重拉数据；非当前视图的
   容器不渲染——500 行拼两遍 DOM 是白费。行式分支保持移植前的表达式（含空态
   文案）逐字节不变，那是被测试钉住的输出。 */
function renderRequestLog() {
  const list = reqList();
  renderRequestCount(list);
  if (reqView === 'line') {
    const box = $('reqLogBox');
    if (box) box.innerHTML = list.map(requestLogLine).join('') ||
      '<span class="c-muted">暂无请求记录</span>';
    return;
  }
  renderRequestTable(list);
}

/* requestLogText 生成请求记录行（本 fork 的行式日志，渲染进 #reqLogBox 的 .ln 行）。
   字段顺序是本 fork 的既有约定（请求 ID 收尾），本批只加两段，且都要求「有观测才
   显示」：缓存命中率（token 口径指标，紧跟 credit）与调用来源（沿用 stdout 流水行
   的 `src=… ua=…` 写法）。两者都没有的老归档行因此保持原样，不会凭空多出
   「命中 —」「src=- ua=-」这类看着像坏了的占位。 */
function requestLogText(e) {
  const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
  const outcomeLabel = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' };
  const token = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  let credit = 'credit —';
  if (e && e.credit_known) {
    const value = Number(e.credit);
    if (Number.isFinite(value)) credit = String(Number(value.toFixed(2))) + ' credit';
  }
  const out = [
    when,
    String(e && e.status || '—') + ' ' + (outcomeLabel[e && e.outcome] || (e && e.outcome) || '—'),
    e && e.model || '—',
    e && e.account || '—',
    fmtMs(e && e.duration_ms),
    fmtTok(token) + ' tok',
    credit,
  ];
  const rate = cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens);
  if (rate !== '—') out.push('命中 ' + rate + '（' + fmtTok(e && e.cache_hit_tokens) + ' tok）');
  const src = requestSourceText(e);
  if (src) out.push(src);
  out.push(e && e.request_id || '—');
  return out.join(' | ');
}

/* requestSourceText 调用来源片段（客户端 IP + User-Agent 短标签）。
   与 internal/server/logging.go 的 stdout 流水行同款：两者都没采集时返回空串（不追加，
   旧行格式不变）；只缺一侧时该侧写 `-`，绝不拿别的字段顶替。IP 用裸值便于 grep，
   UA 加引号（短标签里可能含空格）。 */
function requestSourceText(e) {
  const ip = String(e && e.client_ip || '').trim();
  const ua = shortUA(e && e.user_agent);
  if (!ip && !ua) return '';
  return 'src=' + (ip || '-') + ' ua=' + (ua ? '"' + ua + '"' : '-');
}

/* cacheRateText 前缀缓存命中率（上游 prompt cache，issue #92）。命中率 = 命中 ÷
   （命中 + 未命中）；无观测（两者都是 0/缺字段）返回 '—' 而不是 '0%'——把「没有
   样本」显示成 0% 会被读成「缓存完全失效」，那是误导。统计卡、明细表、请求行共用
   这一个口径，避免三处各算一遍后漂移。自包含（不依赖 trimFixed），便于切片测试。 */
function cacheRateText(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '—';
  return String(Math.round(h / total * 1000) / 10) + '%';
}

/* cacheRateTone 命中率健康度分档：≥90% 绿 / 80–90% 琥珀 / <80% 红 / 无样本无色。
   上游用内联 style 上色，本 fork 禁静态内联样式，这里只回档位名，由调用方接既有
   语义色类（卡片 .stat.good|warn|bad、表格 .c-ok|warn|bad）。 */
function cacheRateTone(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '';
  const pct = h / total * 100;
  return pct >= 90 ? 'ok' : (pct >= 80 ? 'warn' : 'bad');
}

/* cacheRateCell 明细表里的命中率单元格：绝对值（命中/未命中 tok）进 title，供逐项
   核对——只有百分比时无法区分「1k 命中 90%」和「1M 命中 90%」。 */
function cacheRateCell(hit, miss) {
  const tone = cacheRateTone(hit, miss);
  if (!tone) return '<span class="c-muted">—</span>';
  return '<span class="c-' + tone + '" title="命中 ' + fmtTok(hit) + ' / 未命中 ' + fmtTok(miss) + ' tok">' +
    cacheRateText(hit, miss) + '</span>';
}

/* cacheRateStat 用量总览的命中率统计卡：与明细表共用 tone 口径，只把档位映射到
   .stat 的语义色类（卡片用的是 good，不是 ok）。 */
function cacheRateStat(hit, miss) {
  const tone = cacheRateTone(hit, miss);
  return usStat(cacheRateText(hit, miss), '缓存命中率', tone === 'ok' ? 'good' : tone);
}

/* shortUA 是后端 internal/logfmt.ShortUA 的等价实现。面板从 reqlog 拿到的是完整 UA
   （最长 200 字节），浏览器 UA 动辄 120+ 字符，直接铺进行式日志会把整行挤爆；而
   「Mozilla/5.0 (Windows NT …) AppleWebKit/537.36 …」里对「这是什么客户端」有用的
   只有 Chrome/120.0.0.0。取第一个 name/version 形态且 name 不是渲染引擎的 token；
   没有就回落整串截断。口径与 stdout 流水行的 ua= 一致，两处看到的标签才不打架。 */
const SHORT_UA_MAX = 40;
const UA_ENGINES = {
  mozilla: 1, applewebkit: 1, gecko: 1, khtml: 1, like: 1, safari: 1, compatible: 1, msie: 1, trident: 1,
};
function shortUA(ua) {
  const s = String(ua == null ? '' : ua).trim();
  if (!s) return '';
  for (const raw of s.split(/\s+/)) {
    const tok = raw.replace(/^[(),;]+/, '').replace(/[(),;]+$/, '');
    const slash = tok.indexOf('/');
    if (slash <= 0) continue;
    if (UA_ENGINES[tok.slice(0, slash).toLowerCase()]) continue;
    return truncBytes(tok, SHORT_UA_MAX);
  }
  return truncBytes(s, SHORT_UA_MAX);
}

/* truncBytes 按 UTF-8 字节上限截断（对齐后端 logfmt.Truncate：切点落在多字节字符
   中间时整个让出该字符）。UA 里混中文时按字符数截会超字节上限，按字节截又会切出
   半个字符，故逐个码点累加。 */
function truncBytes(s, n) {
  let used = 0, out = '';
  for (const ch of String(s)) {
    const b = utf8Len(ch);
    if (used + b > n) break;
    used += b; out += ch;
  }
  return out;
}
function utf8Len(ch) {
  const c = ch.codePointAt(0);
  if (c < 0x80) return 1;
  if (c < 0x800) return 2;
  if (c < 0x10000) return 3;
  return 4;
}

function requestLogLine(e) {
  const outcome = String(e && e.outcome || '');
  const cls = outcome === 'http_error' || outcome === 'stream_error' ? ' e'
    : outcome === 'interrupted' ? ' w' : '';
  return '<span class="ln' + cls + '">' + esc(requestLogText(e)) + '</span>';
}

function fmtBytes(bytes) {
  const n = Number(bytes || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}
$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  listen: ['listen'], api_key: ['api_key'],
  package_detail_limit: ['panel', 'package_detail_limit'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'], growth_hours: ['schedule', 'growth_hours'], growth_enabled: ['schedule', 'growth_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  // 保号类四任务（签到 / 活跃上报 / 保活 / 余额刷新）是否覆盖已禁用账号：默认 false
  // 保持「禁用即跳过」既有语义，故勾选框空态与后端缺省一致，不需要额外默认值代码。
  include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  // 积分保底：0 = 关闭（上游与后端共同的缺省），正整数 = 余额低于该值时对实测
  // 收费模型（tier 2）不出票。表单留空 = 沿用现值（collectConfig 跳过发送）。
  credit_floor: ['pool', 'credit_floor'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  cost_explore_interval: ['pool', 'cost_explore_interval'],
  prefer_expiring: ['pool', 'prefer_expiring'], expiring_soon: ['pool', 'expiring_soon'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  // 调用来源（客户端 IP / UA）开关：比 token 计数敏感，共享部署可一键关掉。
  // 后端缺省 true、热生效（无需重启），面板只负责回填/保存这一个布尔值。
  request_client_info: ['logging', 'request_client_info'],
};
function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else el.value = v == null ? '' : v;
    }
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      if (raw === '') v = undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'expiring_soon', 'ttl'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('btnCfgReload').onclick = loadConfig;
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
    const n = (r.restart_required || []).length;
    toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    const k = $('cfgKey').value.trim();
    if (k) localStorage.setItem(LS_KEY, k);
    loadConfig();
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
function openAdd() {
  openVeil('addVeil');
  // 重置到登录标签
  switchAddTab('login');
  $('addPick').hidden = false;
  $('addLoad').hidden = true; $('addReady').hidden = true;
  $('addDone').hidden = true; $('addErr').hidden = true;
  $('importDone').hidden = true; $('importErr').hidden = true;
  $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
  $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
  stopPoll();
}
function switchAddTab(tab) {
  document.querySelectorAll('#addTabs .tab').forEach(b => {
    const on = b.dataset.tab === tab;
    b.classList.toggle('on', on);
    b.setAttribute('aria-selected', on ? 'true' : 'false');
  });
  $('addTabLogin').hidden = tab !== 'login';
  $('addTabImport').hidden = tab !== 'import';
}
document.querySelectorAll('#addTabs .tab').forEach(b => {
  b.onclick = () => switchAddTab(b.dataset.tab);
});
function startAddLogin() {
  const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
  $('btnStartLogin').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('login/start', { method: 'POST', body: JSON.stringify({ realm }) }).then(r => {
    loginState = r.state;
    $('addUrl').textContent = r.url;
    $('addPick').hidden = true; // 选域锁定（会话已按该域发起）
    $('addLoad').hidden = true; $('addReady').hidden = false;
    $('btnStartLogin').hidden = true;
    $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
    loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnStartLogin').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    if (r.done) {
      stopPoll();
      $('addReady').hidden = true;
      $('addDone').hidden = false;
      $('addDone').textContent = '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中';
      setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
    }
  } catch (e) {
    stopPoll();
    $('addReady').hidden = true;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message + '（关闭后重新添加）';
  }
}
function closeAdd() { stopPoll(); loginState = null; closeVeil('addVeil'); }
$('btnCloseAdd').onclick = closeAdd;
$('btnStartLogin').onclick = startAddLogin;
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => navigator.clipboard.writeText($('addUrl').textContent)
  .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));
$('importFile').onchange = async () => {
  const file = $('importFile').files[0];
  if (!file) return;
  $('importDone').hidden = true; $('importErr').hidden = true;
  const fd = new FormData();
  fd.append('file', file);
  const h = {};
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  try {
    const r = await fetch('/panel/api/import/cockpit', { method: 'POST', body: fd, headers: h });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
    $('importDone').hidden = false;
    $('importDone').textContent = '导入完成：成功 ' + d.imported + ' 个' + (d.skipped ? '，跳过 ' + d.skipped + ' 个' : '');
    if (d.errors && d.errors.length) {
      console.warn('import errors:', d.errors);
    }
    loadOverview(true);
  } catch (e) {
    $('importErr').hidden = false;
    $('importErr').textContent = '导入失败：' + e.message;
  }
  $('importFile').value = '';
};

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;
$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  b.disabled = true; b.textContent = '刷新中…';
  try {
    await api('balance_all', { method: 'POST' });
    await loadOverview(true);
    toast('余额已从上游刷新', 'ok');
  } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
  finally { b.disabled = false; b.textContent = '刷新'; }
  if (view === 'logs') loadLogs();
};

/* ── 轮询 ─────────────────────────────────────────────────────────── */
function refreshVisible() {
  if (view === 'accounts') loadOverview(true);
  else if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') reattachQueueView();
}
function start() {
  loadOverview(true);
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, 5000);
  checkAuthGate();
}
async function checkAuthGate() {
  try { await api('overview'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}
start();

/* ── 积分任务 ─────────────────────────────────────────────────────── */
let taskUID = null;

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）',
  'school_season': '校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_1': '小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_2': '小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）',
  'Sequential_Tasks_3': '小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）',
  'Sequential_Tasks_4': '小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_5': '小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_6': '小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖',
  'Sequential_Tasks_7': '体验灵感功能（预留，疑 PC 口径）：accept → 灵感事件组（PC+mp 双形态）→ 领奖（判据待解锁验证）'
};

function openTasks(uid) {
  taskUID = uid;
  $('taskWho').textContent = uid.slice(0, 16);
  openVeil('taskVeil');
  $('btnTaskReload').hidden = false;
  loadTasks();
}
function closeTasks() { closeVeil('taskVeil'); taskUID = null; }
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = loadTasks;

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
$('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAcceptAll');
  btn.disabled = true; btn.textContent = '接受中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
    } else {
      toast(n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部接受'; loadTasks(); }
};

// 一键完成全部可自动任务（耗时较长：含真实对话，逐项回读验证）。
$('btnTaskAutoAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAutoAll');
  if (!confirm('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1-2 分钟（含真实对话），确认继续？')) return;
  btn.disabled = true; btn.textContent = '执行中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all', { method: 'POST' });
    const okN = (r.results || []).filter(x => x.status === 'done').length;
    const skipN = (r.results || []).filter(x => x.status === 'skipped').length;
    const errN = (r.results || []).filter(x => x.status === 'error').length;
    toast(`执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`, errN ? 'err' : 'ok');
    console.log('auto_all results:', r.results);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '一键完成可自动任务'; loadTasks(); }
};

async function loadTasks() {
  if (!taskUID) return;
  const st = $('taskState'), tb = $('taskTable');
  // 加载态直接用骨架表格占位（旧版是"查询中"一行 + 空表，切换时会有跳动）。
  st.hidden = true;
  st.className = 'state';
  tb.hidden = false;
  $('taskBody').innerHTML = skeletonRows(6, 5);
  try {
    const d = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks');
    const list = d.tasks || [];
    if (!list.length) {
      tb.hidden = true;
      st.hidden = false;
      st.className = 'state';
      st.textContent = '该账号暂无任务';
      return;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    $('taskBody').innerHTML = list.map(t => {
      // 进度：current 可能缺失（0 或被上游省略）——用 ?? 兜底，避免渲染成 "undefined / N"
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push('+' + t.credit + ' 分');
      if (t.energy) parts.push('+' + t.energy + ' 能');
      if (t.reward_buddy) parts.push('Buddy');
      const reward = parts.length ? parts.join(' ') : '—';
      const badge = t.claimed ? '<span class="tag ok">已领取</span>'
        : t.claimable ? '<span class="tag warn">可领取</span>'
        : t.locked ? '<span class="tag mute">未解锁</span>'
        : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>'
        : '<span class="tag mute">未接受</span>';
      const acted = t.claimed || t.locked ? ''
        : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
        : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">一键完成</button>'
        : t.accept_status === 'accepted' ? ''
        : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">接受</button>';
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(t.task_code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
        '<td class="num">' + esc(prog) + '</td>' +
        '<td class="num">' + esc(reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="acts">' + acted + '</td></tr>';
    }).join('');
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    tb.hidden = true;
    st.hidden = false;
    st.className = 'state err';
    st.textContent = e.message;
  }
}

$('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || !taskUID) return;
  const kind = b.dataset.t, code = b.dataset.c;
  b.disabled = true;
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto', {
        method: 'POST', body: JSON.stringify({ task_code: code })
      });
      if (r.skipped) {
        toast(r.message || '已跳过', 'ok');
      } else {
        const advanced = r.progress_before !== r.progress_after;
        let msg = r.message || '已执行';
        if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
        if (r.claimed) msg += '，奖励已自动到账';
        else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
        else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
        toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
      }
      loadOverview(true);
    } else {
      const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      await api(path, { method: 'POST', body: JSON.stringify(body) });
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      if (kind === 'claim') loadOverview(true);
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { loadTasks(); }
});

/* ── 任务中心：全账号扫描 / 执行队列 ──────────────────────────────── */
/* 说明：早期版本的「开学季状态卡」相关实现（staskHTML / LUCK_SVG /
   SCHOOL_TITLES 与配套 .srow/.stask 样式）已随该视图下线成为死代码，
   2026-10 前端重构时一并移除；活动已于 2026-09-24 结束。 */

/* ── 精简 QR 编码器（券码二维码用）────────────────────────────────────
   规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响
   可扫描性；已用 python qrcode 库对多输入多版本做逐像素交叉验证（强制 byte
   模式 + mask 0，5/5 全部 diff=0）。面板 CSP 只允许 self，外链 QR 服务不可用。 */
// qr_gen.js —— 精简 QR 编码器（浏览器用 + node 可跑交叉验证）
// 规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
// 完整性说明：规范允许编码器任选掩码（解码器按格式信息位自行去掩码），
// 固定掩码不影响可扫描性；券码为短文本，v1-5（26 字节起）绰绰有余。

// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1）：[数据码字数, 纠错码字数]，ECC L 单块
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 8 位计数 + 数据 + 终止符 + 0xEC/0x11 填充）
function qrDataCodewords(text, dataCap) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);            // byte 模式
  push(bytes.length, 8); // v1-9 计数 8 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 ≈ 2 码字头 + 文本长度，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
  if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
  const [dataCap, ecCap] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+，跳过与定位重叠处）
  const align = QR_ALIGN[ver - 1] || [];
  for (const ar of align) for (const ac of align) {
    if (F[ar][ac]) continue;
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);


  // 数据码字 + 纠错码字 → 位流
  const dcw = qrDataCodewords(text, dataCap);
  const cw = dcw.concat(rsRem(dcw, ecCap));
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG（quiet zone 4 模块）。白底由 CSS 提供（.vc-qr svg），
// 保证深色主题下二维码仍是白底黑块——扫描器需要足够对比度。
function qrSVG(M, px) {
  const n = M.length, q = 4, total = n + q * 2;
  let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img">';
  for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
    if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
  return s + '</svg>';
}

/* ── 开学季券码查询（弹窗，仿活动页 #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API 只在 secure context（https/localhost）可用，
   远程 http 面板会拿不到 navigator.clipboard → 降级 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

function vcCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return '<div class="vc' + (expired ? ' expired' : '') + '">' +
    '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || '券') + '</span>' +
    (expired ? '<span class="tag bad">已过期</span>' : '<span class="tag ok">可使用</span>') + '</div>' +
    '<div class="meta">' +
      (v.valid_to ? '有效期至 ' + esc(v.valid_to) : '长期有效') +
      (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' 抽中' : '') +
    '</div>' +
    '<div class="sep"></div>' +
    '<div class="ft"><span class="lab">券码</span><code>' + esc(v.code || '-') + '</code>' +
    '<span class="acts">' +
      (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">二维码</button>' : '') +
      '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">复制</button>' +
    '</span></div>' +
    '</div>';
}

async function loadSchoolVouchers() {
  const body = $('vcBody');
  openVeil('vcVeil');
  body.innerHTML = '<div class="state"><span class="dots">查询中</span></div>';
  $('vcNote').textContent = '';
  try {
    const d = await api('school/vouchers');
    const arr = d.accounts || [];
    const ok = arr.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
      '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
      '<span>' + a.vouchers.length + ' 张</span></div>' +
      a.vouchers.map(vcCard).join('')
    ).join('') || '<div class="empty"><div class="big">还没有抽到券</div>开学季活动期间的抽奖奖品会显示在这里。</div>';
    $('vcNote').textContent = total ? total + ' 张券 · ' + ok.filter(a => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
    const errs = arr.filter(a => a.error);
    if (errs.length) {
      body.insertAdjacentHTML('beforeend', '<div class="note c-warn mt-2">查询失败：' +
        errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(a.error) + '）').join('、') + '</div>');
    }
    body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
      try { await copyText(b.dataset.copy); toast('券码已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择券码', 'err'); }
    });
    // 二维码：券码本体编码为 QR（到店出示扫描），点击切换显示/隐藏
    body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
      const card = b.closest('.vc');
      const old = card.querySelector('.vc-qr');
      if (old) { old.remove(); return; }
      const box = document.createElement('div');
      box.className = 'vc-qr';
      try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
      catch (e) { box.innerHTML = '<span class="note">二维码生成失败：' + esc(e.message) + '</span>'; }
      card.appendChild(box);
    });
  } catch (e) {
    body.innerHTML = '<div class="state err">' + esc(e.message) + '</div>';
  }
}
$('btnSchoolVouchers').onclick = loadSchoolVouchers;
$('btnVcClose').onclick = () => closeVeil('vcVeil');
$('btnVcRefresh').onclick = loadSchoolVouchers;

/* 成长任务队列。lastQueueSeq 记录本页启动过的队列代次：执行结束后的残留 items
   （running=false 但 seq 停在旧值）不再回写视图——否则扫描结果 3 秒后被上一轮
   队列状态覆盖。 */
let queueTimer = null, lastQueueSeq = 0;
const GROWTH_TITLES = {}; // code → 展示名（扫描时从任务列表带出）
$('btnScanAll').onclick = async () => {
  const b = $('btnScanAll');
  // 停掉队列轮询：显式扫描 = 切到待办视图。否则在途队列的下一 tick 会把扫描
  // 结果冲掉重渲染回队列视图（服务端执行不受影响，只是不再实时回写本视图）。
  if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }
  b.disabled = true; b.textContent = '扫描中…';
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    renderQueue(groupItems(d), null, '没有待办任务', '全部账号的成长任务与开学季活动都已完成，明日再来。');
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '扫描待办'; }
};
$('btnRunQueue').onclick = async () => {
  const conc = Number($('qcConc').value) || 1;
  if (!confirm('扫描全部账号待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
  const b = $('btnRunQueue');
  b.disabled = true; b.textContent = '启动中…';
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
    if (!r.started) { toast(r.message || '没有待办任务', 'ok'); return; }
    lastQueueSeq = r.seq || 0;
    toast('队列已启动：' + r.total + ' 项（并发 ' + conc + '）', 'ok');
    startQueuePolling();
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '执行全部待办'; }
};
// 扫描结果 → 分组条目（无执行状态）
function groupItems(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
    }
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}
const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };
function qrowHTML(it) {
  const isSchool = it.kind === 'school';
  const title = isSchool ? '开学季闭环' : (GROWTH_TITLES[it.code] || it.code);
  const dotCls = it.status === 'scan' ? 'wait' : it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
  const stWord = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  return '<div class="qrow" title="' + esc(it.message || '') + '">' +
    '<span class="code">' + esc(it.code) + '</span>' +
    '<span class="name"><span class="t">' + esc(title) + '</span>' + (isSchool ? '<span class="tag mute">开学季</span>' : '') + '</span>' +
    '<span class="prog">' + esc(it.prog || '') + '</span>' +
    '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
    '<span class="msg">' + esc(it.message || '') + '</span>' +
    '</div>';
}
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
  const empty = $('tcEmpty'), list = $('qcList');
  if (!groups.length) {
    empty.hidden = false;
    if (emptyTitle) empty.querySelector('.t').textContent = emptyTitle;
    if (emptyDesc) empty.querySelector('.d').textContent = emptyDesc;
    list.innerHTML = '';
    $('qProg').hidden = true; $('qcSummary').textContent = '';
    return;
  }
  empty.hidden = true;
  let total = 0;
  list.innerHTML = groups.map(g => {
    total += g.rows.length;
    return '<div class="qgroup"><header><span class="nm">' + esc(g.nick || g.uid.slice(0, 12)) + '</span><span class="cnt">' + g.rows.length + ' 项待办</span></header>' +
      g.rows.map(qrowHTML).join('') + '</div>';
  }).join('');
  $('qcSummary').textContent = total + ' 项';
  updateProgress(progress);
}
function updateProgress(q) {
  if (!q || !q.items) { $('qProg').hidden = true; return; }
  const total = q.items.length;
  const done = q.items.filter(it => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length;
  $('qProg').hidden = false;
  $('qBarFill').style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  $('qProgText').textContent = (q.running ? '执行中 ' : '已结束 ') + done + ' / ' + total;
}
// 队列状态 → 分组（执行时轮询）
function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({
      kind: it.kind, code: it.code,
      prog: it.kind === 'school' ? '—' : '',
      status: it.status, message: it.message,
    });
  }
  return Array.from(by.values());
}
function startQueuePolling() {
  if (queueTimer) clearInterval(queueTimer);
  queueTimer = setInterval(async () => {
    let q;
    try { q = await api('tasks/queue'); } catch (e) { return; }
    if (!q.started) return;
    // 只渲染本页启动过的那轮队列（刷新页面后不再接管旧队列）。
    if (lastQueueSeq && q.seq !== lastQueueSeq) return;
    if (q.running) {
      renderQueue(groupsFromQueue(q.items || []), q);
      return;
    }
    // 结束：终态只渲染这一次，随即停表。此后残留的 items（running=false）不再
    // 回写视图——曾把用户刚点开的「扫描待办」结果在下一个 tick 冲掉。
    renderQueue(groupsFromQueue(q.items || []), q);
    clearInterval(queueTimer); queueTimer = null;
    toast('任务队列执行结束', 'ok');
  }, 3000);
}
// reattachQueueView 切回任务中心视图时恢复队列进度：仅当本页启动的队列仍在
// 执行才重新开轮询（残留态/别页队列不接管——视图不被旧结果冲掉）。
function reattachQueueView() {
  // 全程异步：go() 在顶层（app.js ~143 行）被调用时，本文件下方 let/const
  //（queueTimer/lastQueueSeq 等）尚未初始化——同步读取即 TDZ ReferenceError
  // 使整个脚本中断。await 之后才碰它们（旧 pollQueueOnce 正是靠开头的 await
  // 侥幸安全）。queueTimer 的"已在跑"判定也挪到 await 后，语义不变。
  (async () => {
    try {
      const q = await api('tasks/queue');
      if (queueTimer) return; // 轮询已在跑（跨视图不中断）
      if (q.started && q.running && (!lastQueueSeq || q.seq === lastQueueSeq)) startQueuePolling();
    } catch (e) { /* 静默 */ }
  })();
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，二十行足够。 */

function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }
function trimFixed(s) {
  if (!String(s).includes('.')) return String(s);
  return String(s).replace(/0+$/, '').replace(/\.$/, '');
}
function fmtCredit(n) {
  const v = Number(n || 0);
  if (!Number.isFinite(v)) return '—';
  return trimFixed(v.toFixed(2));
}
function fmtCreditRatio(v, samples, tokens) {
  if (!samples || !tokens) return '—';
  const n = Number(v || 0);
  if (!Number.isFinite(n)) return '—';
  return trimFixed(n.toFixed(4)) + ' / 1M';
}
function fmtModelRate(rate) {
  const s = String(rate || '').trim();
  return s ? 'x' + s : '—';
}

function usStat(v, k, cls) {
  return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
         '</div><div class="k">' + esc(k) + '</div></div>';
}

/* statsHTML 补齐统计条到 6 的整数倍：统计条用「gap + 容器底色」画 1px 网格，
   不足列数的空位会透出底色形成灰块。补 .blank 格（同面底色）让网格始终填满；
   6 的倍数在三个断点（6/3/2 列）下都能整除，不会出现半行空位。 */
function statsHTML(items) {
  const out = items.slice();
  while (out.length % 6 !== 0) out.push('<div class="stat blank" aria-hidden="true"></div>');
  return out.join('');
}

function usBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-wrapbar">' +
    '<span class="bar bar-p" style="--w:' + (pp * 0.8).toFixed(1) + 'px" title="prompt"></span>' +
    '<span class="bar bar-c" style="--w:' + Math.max(2, pc * 0.8).toFixed(1) + 'px" title="completion"></span>' +
    '</span>';
}

/* usRow 生成一行。mid 是插在「名称」之后、请求数之前的额外单元格（如「域」列）。
   withPerf 控制延迟/速率两列；列开关显式传入，避免调用方改动后与表头错列。
   缓存命中率紧跟在「合计」之后：它是 token 口径的构成指标，与三张表都同序，
   加列时表头（index.html）、本函数、空态 colspan 三处必须一起改。 */
function usRow(name, sub, a, mid, withPerf) {
  return '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>' + esc(name) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    (mid || '') +
    '<td class="num">' + fmtTok(a.requests) + '</td>' +
    '<td class="num">' + (a.errors ? '<span class="c-warn">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
    '<td class="num">' + fmtTok(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.total_tokens) + '</td>' +
    '<td class="num">' + cacheRateCell(a.cache_hit_tokens, a.cache_miss_tokens) + '</td>' +
    (withPerf
      ? '<td class="num">' + fmtMs(a.avg_latency_ms) + '</td>' +
        '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '</tr>';
}

function renderUsage(d) {
  const t = d.totals || {};
  // 统计条固定 6 张卡：statsHTML 把空位补成 6 的倍数，靠「gap + 容器底色」画的 1px 网格
  // 才能始终排满；曾把缓存命中率当第 7 张卡塞进来，结果第二行只剩 1 张卡 + 5 个空格，
  // 空格之间的格线照样可见，看着像 5 张卡没加载出来。命中率改挂到「积分扣除历史」的
  // 统计条（4→5 张，仍是一行），口径说明也写在那条 note 里。
  $('usStats').innerHTML = statsHTML([
    usStat(fmtTok(t.requests), '请求数'),
    usStat(fmtTok(t.total_tokens), '总 token'),
    usStat(fmtTok(t.prompt_tokens), 'prompt'),
    usStat(fmtTok(t.completion_tokens), 'completion'),
    usStat(t.errors ? String(t.errors) : '0', '失败尝试', t.errors ? 'warn' : ''),
    usStat(fmtMs(t.avg_latency_ms), '平均延迟'),
  ]);

  // 卡片、三张表与时序图全部按所选窗口统计（切窗口数字随之变化）；
  // 「全部历史」含 90 天前折叠出的日桶。这里标注当前口径与数据起点：
  // 服务端回显的 window_from/window_to 优先——「今天」与自定义区间都由浏览器算
  // 好发过去，回显是"服务端真的按这一段统计"的唯一凭据；滚动窗口没有回显，
  // 回落成控件自己的标签。
  const rangeEcho = d.window_from
    ? String(d.window_from).replace('T', ' ').slice(0, 16) +
      (d.window_to ? ' → ' + String(d.window_to).replace('T', ' ').slice(0, 16) : ' → 现在')
    : '';
  const winLabel = trangeLabel('usRange');
  $('usNote').textContent =
    (rangeEcho || winLabel ? (rangeEcho || winLabel) + ' · ' : '') +
    (d.buckets || 0) + ' 个分桶' +
    (d.since ? ' · 数据自 ' + d.since.replace('T', ' ') : '') +
    (d.file_bytes ? ' · 文件 ' + (d.file_bytes / 1024).toFixed(1) + ' KB' : '');

  $('usAccBody').innerHTML = (d.by_account || []).map(x =>
    usRow(x.key.slice(0, 8), x.extra || '', x,
      '<td class="num">' + esc(x.realm || '') + '</td>', true)
  ).join('') || '<tr><td colspan="11" class="empty">暂无数据</td></tr>';

  $('usModelBody').innerHTML = (d.by_model || []).map(x =>
    usRow(x.key, '', x, '', false)).join('') || '<tr><td colspan="8" class="empty">暂无数据</td></tr>';

  $('usRealmBody').innerHTML = (d.by_realm || []).map(x =>
    usRow(x.key, '', x, '', false)).join('') || '<tr><td colspan="8" class="empty">暂无数据</td></tr>';

  renderCreditDimensions(d);
  renderUsageChart(d.series || []);
}

function renderCreditDimensions(d) {
  const t = d.totals || {};
  const accounts = d.credit_by_account || [];
  const models = d.credit_by_model || [];
  // 缓存命中率（无样本时 cacheRateStat 自己回 '—'）：放在这条统计条里而不是「用量总览」，
  // 是为了守住上面那条 6 张卡的不变量；放在这里也更贴语义——命中缓存省下的正是积分。
  $('usCreditStats').innerHTML = statsHTML([
    usStat(fmtCredit(t.credits), '扣除积分'),
    usStat(fmtTok(t.credit_tokens), '匹配 Token'),
    usStat(fmtCreditRatio(t.credits_per_1m_tokens, t.credit_samples, t.credit_tokens), '平均积分 / 1M Token'),
    usStat(String(t.credit_samples || 0), '有效积分样本'),
    cacheRateStat(t.cache_hit_tokens, t.cache_miss_tokens),
  ]);

  const cacheTotal = Number(t.cache_hit_tokens || 0) + Number(t.cache_miss_tokens || 0);
  $('usCreditNote').textContent =
    accounts.length + ' 个账号 · ' + models.length + ' 个模型倍率分组 · 仅统计与积分同时观测到的 Token' +
    // 命中率卡的样本集与上表不同（全部 Token，不只积分匹配到的），note 里点名，
    // 免得把它当成「这张表的口径」。
    (cacheTotal > 0 ? ' · 缓存命中率为本窗口全部 Token 口径' : '');

  $('usCreditAccBody').innerHTML = accounts.map(row => {
    const uid = String(row.key || '');
    const account = row.nickname || uid.slice(0, 8) || '—';
    return '<tr>' +
      '<td class="mark" aria-hidden="true"></td>' +
      '<td>' + esc(account) + '<div class="note">' + esc(row.realm || '') + ' · ' + esc(uid.slice(0, 8)) + '</div></td>' +
      '<td class="num">' + fmtTok(row.requests) + '</td>' +
      '<td class="num">' + fmtCredit(row.credits) + '</td>' +
      '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
      '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="6" class="empty">暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。</td></tr>';

  $('usCreditModelBody').innerHTML = models.map(row =>
    '<tr>' +
      '<td class="mark" aria-hidden="true"></td>' +
      '<td>' + esc(row.key || '—') + '</td>' +
      '<td>' + esc(fmtModelRate(row.rate)) + '</td>' +
      '<td class="num">' + fmtTok(row.requests) + '</td>' +
      '<td class="num">' + fmtCredit(row.credits) + '</td>' +
      '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
      '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
    '</tr>'
  ).join('') || '<tr><td colspan="7" class="empty">暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。</td></tr>';
}

/* renderUsageChart 画堆叠柱状图。
 *
 * x 轴是**真实时间轴**，不是按序号等距。这一点很重要：数据里存在 1 小时的
 * 间隔，也存在 6~8 小时的断档（没请求的时段不产生桶），等距排布会把 8 小时
 * 画得和 1 小时一样宽，让「什么时候用的」完全失真。
 *
 * 另外不再用 preserveAspectRatio="none"：那会把 760 宽的 viewBox 横向拉伸到
 * 容器宽度，柱子和文字都变形。改为固定比例、按容器宽度自适应高度。
 *
 * 时间轴用本地时间解析（后端返回的就是本地时区），day 点按当天 00:00 参与定位，
 * 与 hour 点在同一个连续轴上——日桶本来就是他那天所有小时的聚合。
 */

/* parsePointTime 把后端的 t 解析成毫秒时间戳。 */
function parsePointTime(p) {
  // hour: "2026-09-16T13"  day: "2026-09-16"
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

function renderUsageChart(series) {
  const host = $('usChart');

  // 丢掉时间解析不出来的点，而不是让 NaN 传染整张图。
  const pts = [];
  for (const p of series) {
    const t = parsePointTime(p);
    if (t === null) continue;
    const pt = Number(p.prompt_tokens || 0);
    const ct = Number(p.completion_tokens || 0);
    pts.push({ t, scope: p.scope, raw: p.t, pt, ct, tt: Number(p.total_tokens || 0) || (pt + ct),
               req: p.requests || 0 });
  }
  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>';
    return;
  }

  const W = 760, H = 180, PL = 52, PR = 12, PT = 12, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB;

  const t0 = pts[0].t;
  const t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);

  const max = Math.max(1, ...pts.map(p => p.tt));

  // 柱宽取「最小真实间隔」的 70%，并夹在合理区间内——窗口拉到 30 天时柱子会
  // 变细，但不会细到看不见。
  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const slot = iw * (minGap / span);
  const bw = Math.max(1.5, Math.min(30, slot * 0.7));

  const xOf = t => PL + (t - t0) / span * iw;

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" ' +
            'preserveAspectRatio="xMidYMid meet">';

  // y 轴网格 + 刻度
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
           '" text-anchor="end">' + fmtTok(max * i / 4) + '</text>';
  }

  // 柱子
  for (const p of pts) {
    const cx = xOf(p.t);
    const x = cx - bw / 2;
    const hTot = ih * (p.tt / max);
    const hP = p.tt ? hTot * (p.pt / p.tt) : 0;
    const hC = Math.max(p.tt && p.ct ? 1 : 0, hTot - hP);
    const yBase = PT + ih;
    if (hP > 0) out += '<rect x="' + x.toFixed(2) + '" y="' + (yBase - hP).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hP.toFixed(2) +
      '" fill="var(--chart-1)" rx="1.5"/>';
    if (hC > 0) out += '<rect x="' + x.toFixed(2) + '" y="' + (yBase - hP - hC).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hC.toFixed(2) +
      '" fill="var(--chart-2)" rx="1.5"/>';
    out += '<title>' + esc(p.raw) + '  ' + fmtTok(p.pt) + ' prompt / ' +
           fmtTok(p.ct) + ' completion / ' + p.req + ' 次</title>';
  }

  // x 轴基线画在柱子之后，避免压在柱底
  out += '<line class="ax" x1="' + PL + '" y1="' + (PT + ih) + '" x2="' + (W - PR) +
         '" y2="' + (PT + ih) + '"/>';

  // x 轴刻度：按真实时间等距取 6 个位置，取该位置**最近的实际柱子**做标签，
  // 所以标签永远落在有数据的点上，不会指到空档里。同时按像素间距去重——
  // 相邻目标点吸附到同一簇柱子时（如 17:00 / 18:00），两个标签会叠在一起。
  const TICKS = Math.min(6, pts.length);
  const usedLabel = new Set();
  const MIN_LABEL_GAP = 46;
  let lastLabelX = -Infinity;
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i].t - target);
      if (d < best) { best = d; bi = i; }
    }
    if (usedLabel.has(bi)) continue;
    const p = pts[bi];
    const cx = xOf(p.t);
    if (cx - lastLabelX < MIN_LABEL_GAP) continue;
    usedLabel.add(bi);
    lastLabelX = cx;
    const d = new Date(p.t);
    const lab = p.scope === 'day'
      ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
      : String(d.getHours()).padStart(2, '0') + ':00';
    // 首尾标签靠边对齐，避免被裁掉
    const anchor = cx < PL + 14 ? 'start' : (cx > W - PR - 14 ? 'end' : 'middle');
    out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
           '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' + esc(lab) + '</text>';
  }

  // 跨天时补一条日期分隔线，让「日界」在长窗口里可见
  let prevDay = null;
  for (const p of pts) {
    const d = new Date(p.t).getDate();
    if (prevDay !== null && d !== prevDay) {
      const x = xOf(p.t).toFixed(1);
      out += '<line class="gl dim" x1="' + x + '" y1="' + PT + '" x2="' + x + '" y2="' +
             (PT + ih) + '"/>';
    }
    prevDay = d;
  }

  out += '</svg>';
  host.innerHTML = out;
}

function fmtTokTip(v) { return fmtTok(v); }

/* 倍率回填节流：usageRateWarmAt 声明在文件顶部（TDZ 修复，见状态区注释）。 */
async function warmUsageModelRates() {
  if (Date.now() - usageRateWarmAt < 10 * 60 * 1000) return;
  try {
    await api('models');
  } catch (e) {
    // 倍率回填是可选增强；失败不阻塞用量统计，10 分钟后再试。
  }
  usageRateWarmAt = Date.now();
}

async function loadUsage() {
  try {
    await warmUsageModelRates();
    const d = await api('usage?' + trangeQuery('usRange', true).toString());
    renderUsage(d);
  } catch (e) {
    $('usChart').innerHTML = '<div class="us-empty">读取用量失败：' + esc(e.message) + '</div>';
  }
}

if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
// 时间范围控件（替换旧的 #usWindow 下拉）：预设或自定义起止一变就重拉用量。
if ($('usRange')) trangeBind('usRange', loadUsage);

/* ── 积分构成 ─────────────────────────────────────────────────────── */
/* 一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
   「个人体验版」…），面额从 6 到 1500 不等，且**按次发放**。所以两个任务完成度
   完全一致的账号，余额可能差上千——差别只在包里。这里把逐包明细摊开，并给每个
   包名一个稳定配色，跨账号对比时同色即同类。 */

/* 图表配色统一走 CSS 变量（--chart-1..10）：浅/深两套在 index.html 的 token
   层定义，前端不再持有裸色值——深色主题下的图表颜色随主题自动适配。
   旧版这里是 12 个硬编码 hex，其中第 2 色 #25b08b 在白底上仅 2.77:1，不达
   「图形元素 ≥3:1」，重构时整组替换（frontend_test 的断言同步更新）。 */
const PK_COLORS = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)',
                   'var(--chart-5)', 'var(--chart-6)', 'var(--chart-7)', 'var(--chart-8)',
                   'var(--chart-9)', 'var(--chart-10)'];
// 与 PK_COLORS 同值但独立声明：frontend_test 会从本行切到 renderExpiryDistribution
// 单独求值，引用外部常量会让该切片在隔离上下文里 ReferenceError。
const PK_ACCOUNT_COLORS = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)',
                           'var(--chart-5)', 'var(--chart-6)', 'var(--chart-7)', 'var(--chart-8)',
                           'var(--chart-9)', 'var(--chart-10)'];

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }

// pkAccountColorMap 按 UID 稳定分配颜色：排序后分配，账号刷新/重排不会换色。
function pkAccountColorMap(list) {
  const uids = (list || [])
    .filter(a => a && !a.error && a.uid)
    .map(a => String(a.uid))
    .sort();
  const colors = new Map();
  uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]));
  return colors;
}

/* pkBySource 把包按名称归并，得到「来源 → 面额/余额/个数」。这是对比的关键视图：
   两个号的差异一定体现在某几个来源的面额上。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs) {
    // 分组键用 code + name，而不是只 name：上游给「首登赠送」和普通活动包用了
    // **同一个 PackageName 和同一个 PackageCode**，只按 name 会把两类混成一类，
    // 那正是当初「两个号为何差 1500」看不出来的原因。这里至少把 code 带进键里，
    // 并在卡片上显示最早的发放时间。
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

const PK_DEFAULT_DETAIL_LIMIT = 5;

function pkDetailLimitValue(raw) {
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : PK_DEFAULT_DETAIL_LIMIT;
}

function pkDetailLimit(cfg) {
  return pkDetailLimitValue(cfg && cfg.panel && cfg.panel.package_detail_limit);
}

const PK_DAY_MS = 24 * 3600 * 1000;

function pkExpiryMs(p) {
  const raw = Number(p && p.expires_at);
  if (Number.isFinite(raw) && raw > 0) return raw;
  const text = String((p && p.end_time) || '').trim();
  if (!text) return null;
  let iso = text.includes('T') ? text : text.replace(' ', 'T');
  if (!/(?:Z|[+-]\d\d:\d\d)$/.test(iso)) iso += '+08:00';
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}

/* 逐包明细的排序规则（选择持久化在 localStorage，跨会话记住，与上游同款）：
     end_asc   到期升序（默认）——快过期的包排最前，提醒优先消耗；上游没下发
               end_time 的包没有可比的日期，统一垫底，不掺进日期序里；
     size_desc 面额降序——看「钱从哪来」，同面额按到期升序收尾。
   上游把整表排完就完事；本 fork 的明细表还有「前 N 条 + 折叠」，所以规则同时
   决定折叠线：选谁，前 N 条就按谁挑——否则「按面额降序」看到的仍是最早到期的
   那 5 条，切换只换了个顺序，大额包依旧藏在折叠里。 */
const PK_SORT_DEFAULT = 'end_asc';
const PK_SORT_LABELS = { end_asc: '按到期升序 · 近的在前', size_desc: '按面额降序 · 大的在前' };
const PK_SORT_SHORT = { end_asc: '按到期升序', size_desc: '按面额降序' };
const LS_PK_SORT = 'pkSortMode';
let pkSortMode = PK_SORT_DEFAULT;

// pkSizeOf 面额取数：非数字一律当 0，别让 NaN 把比较器搅成不稳定序。
function pkSizeOf(p) {
  const n = Number(p && p.size);
  return Number.isFinite(n) ? n : 0;
}

// pkDetailCompare 到期升序：无到期垫底，同到期按面额降序。
function pkDetailCompare(a, b) {
  const ea = pkExpiryMs(a), eb = pkExpiryMs(b);
  if (ea == null && eb != null) return 1;
  if (ea != null && eb == null) return -1;
  if (ea != null && eb != null && ea !== eb) return ea - eb;
  return pkSizeOf(b) - pkSizeOf(a);
}

// 面额降序：同面额回落到期升序。pkDetailCompare 本身就是「到期升序 + 无到期垫底
// + 同到期按面额降序」，直接复用，不维护第二份日期比较。
function pkSizeDescCompare(a, b) {
  const d = pkSizeOf(b) - pkSizeOf(a);
  return d !== 0 ? d : pkDetailCompare(a, b);
}

function pkSortCompare(mode) {
  return mode === 'size_desc' ? pkSizeDescCompare : pkDetailCompare;
}

// pkSortLoad 读回上次选择：只认白名单里的值（localStorage 里可能存着改版前的废弃
// 值），读写都可能抛（隐私模式 / 无 localStorage 的宿主），一律回落默认值——一个
// 排序偏好不值得让整个面板白屏。
function pkSortLoad() {
  try {
    const v = localStorage.getItem(LS_PK_SORT);
    if (typeof v === 'string' && Object.prototype.hasOwnProperty.call(PK_SORT_LABELS, v)) return v;
    if (v) localStorage.removeItem(LS_PK_SORT);
  } catch (e) { /* 读不到就用默认值 */ }
  return PK_SORT_DEFAULT;
}
function pkSortSave(mode) {
  try { localStorage.setItem(LS_PK_SORT, mode); } catch (e) { /* 存不下只影响下次打开 */ }
}

// pkDetailGroups 只服务单账号逐包明细：正余额包先按当前排序规则挑选展示项，
// 其余正余额包与已用完包分别折叠。
function pkDetailGroups(packs, limit, mode) {
  const cmp = pkSortCompare(mode);
  const active = [], used = [];
  let usedSize = 0, restSize = 0, restRemain = 0;
  for (const p of packs || []) {
    const remain = Number(p && p.remain);
    if (remain > 0) {
      active.push(p);
      continue;
    }
    used.push(p);
    const size = Number(p && p.size);
    if (Number.isFinite(size)) usedSize += size;
  }
  active.sort(cmp);
  used.sort(cmp);
  const visible = active.slice(0, pkDetailLimitValue(limit));
  const rest = active.slice(visible.length);
  for (const p of rest) {
    const size = Number(p && p.size);
    if (Number.isFinite(size)) restSize += size;
    const remain = Number(p && p.remain);
    if (Number.isFinite(remain)) restRemain += remain;
  }
  return { visible, rest, used, restSize, restRemain, usedSize };
}

function pkCreditOpacity(days) {
  if (days == null || !Number.isFinite(Number(days))) return 1;
  return 0.25 + 0.75 * Math.max(0, Math.min(29, Number(days) - 1)) / 29;
}

function pkExpiryText(expiresAt) {
  if (!expiresAt) return '无到期时间';
  const diff = expiresAt - Date.now();
  if (diff <= 0) return '已到期';
  const minutes = Math.max(1, Math.ceil(diff / 60000));
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟';
  const hours = Math.ceil(diff / 3600000);
  if (hours < 24) return '剩余 ' + hours + ' 小时';
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天';
}

function pkExpiryDateTime(expiresAt) {
  if (!expiresAt) return '—';
  return new Date(expiresAt).toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai', hour12: false,
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  });
}

function pkAccountSegments(a, now) {
  let balance = Math.max(0, Number(a.remain || 0));
  const out = [];
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0);
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
    const amount = Math.min(balance, remain);
    const expiresAt = pkExpiryMs(p);
    out.push({
      amount,
      expiresAt,
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      uid: String(a.uid || ''),
      accountName: a.nickname || String(a.uid || '').slice(0, 8) || '未命名账号',
    });
    balance -= amount;
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1;
    if (x.expiresAt != null && y.expiresAt == null) return -1;
    return (x.expiresAt || 0) - (y.expiresAt || 0);
  });
}

// summarizeCreditDays 对齐 WorkDaddy：按精确剩余天数逐行聚合，无有效到期时间的余额
// 不进入图表，也不猜测到期日。账号内先按总余额约束逐包金额，避免上游重复记录膨胀。
function summarizeCreditDays(list, now) {
  const buckets = new Map();
  let unavailable = 0;
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) {
      unavailable++;
      continue;
    }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null) continue;
      let row = buckets.get(segment.days);
      if (!row) {
        row = { days: segment.days, credits: 0, segments: [] };
        buckets.set(segment.days, row);
      }
      row.credits += segment.amount;
      row.segments.push(segment);
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.days - b.days);
  for (const row of rows) {
    row.segments.sort((a, b) =>
      (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
      a.accountName.localeCompare(b.accountName) ||
      a.source.localeCompare(b.source));
  }
  return { rows, accountCount: (list || []).length, unavailable };
}

function renderExpiryDistribution(list, now) {
  const summary = summarizeCreditDays(list, now);
  const colors = pkAccountColorMap(list);
  const rows = summary.rows.map(row => {
    const total = row.credits || 1;
    const nodes = row.segments.map(segment => {
      const color = colors.get(segment.uid) || 'var(--accent)';
      const title = segment.source + '\n' + fmtTok(segment.amount) + ' 积分\n到期时间 ' +
        pkExpiryDateTime(segment.expiresAt) + '（' + pkExpiryText(segment.expiresAt) + '）\n' +
        segment.accountName;
      return '<span class="pk-expiry-seg" style="--seg-color:' + color +
        ';--seg-op:' + pkCreditOpacity(segment.days).toFixed(5) +
        ';--seg-flex:' + Math.max(0.008, segment.amount / total).toFixed(4) +
        ' 1 0" title="' + esc(title) + '" aria-label="' + esc(title) + '"></span>';
    }).join('');
    return '<div class="pk-expiry-row"><span>' + esc(row.days === 0 ? '已到期' : row.days + ' 天') +
      '</span><div class="pk-expiry-track">' + nodes + '</div><b>' + esc(fmtTok(row.credits)) +
      '</b></div>';
  }).join('');
  const foot = summary.accountCount + ' 个账号' +
    (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '');
  const legend = (list || []).filter(a =>
    a && !a.error && a.uid && pkAccountSegments(a, now).some(s => s.days != null)
  ).map(a => '<span><i style="--seg-color:' + (colors.get(String(a.uid)) || 'var(--chart-1)') +
    '"></i>' + esc(a.nickname || String(a.uid).slice(0, 8)) + '</span>').join('');
  const hdr = '<div class="pk-expiry-hdr"><span>剩余天数</span><span class="ta-c">各账号该批剩余</span><b>剩余积分</b></div>';
  $('pkExpiry').innerHTML = (rows
    ? hdr + '<div class="pk-expiry-chart">' + rows + '</div>'
    : '<div class="pk-expiry-empty">暂无可汇总积分</div>') +
    (legend ? '<div class="pk-expiry-legend">' + legend + '</div>' : '') +
    '<div class="pk-expiry-foot">' + esc(foot) + '</div>';
}

function renderPackages(d, detailLimit) {
  const list = (d.accounts || []);
  const now = Date.now();
  const expiryColors = pkAccountColorMap(list);
  renderExpiryDistribution(list, now);
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    return;
  }

  // 包名 → 稳定色号（跨账号一致，方便肉眼对齐）
  const names = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  names.sort((x, y) => {
    const sz = n => Math.max(...list.map(a => {
      const f = pkBySource(a.packages || []).find(s => s.key === n);
      return f ? f.size : 0;
    }));
    return sz(y) - sz(x);
  });
  const colorOf = n => pkColor(names.indexOf(n));
  // 键 → 展示名，供卡片与明细表共用（同一来源必然同色同名）。
  const labelOf = {};
  for (const a of list) for (const s of pkBySource(a.packages || [])) labelOf[s.key] = s;

  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  $('pkSummary').innerHTML = list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' +
        esc((a.nickname || a.uid.slice(0, 8))) + '</span>' +
        '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
        '<div class="err">查询失败：' + esc(a.error) + '</div></div>';
    }
    const srcs = pkBySource(a.packages || []);
    const total = Math.max(1, Number(a.size || 0));
    const bar = srcs.map(s =>
      '<i style="--w:' + (s.size / total * 100).toFixed(2) + '%;--seg-color:' +
      colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
    ).join('');
    const legend = srcs.map(s =>
      '<span><i style="--seg-color:' + colorOf(s.key) + '"></i>' +
      esc(s.name.replace(/^CodeBuddy/, '')) + ' x' + s.n + ' · ' + fmtTok(s.size) +
      (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span>'
    ).join('');
    const expiry = pkAccountSegments(a, now);
    const expiryTotal = Math.max(1, expiry.reduce((sum, s) => sum + s.amount, 0));
    const expiryColor = expiryColors.get(String(a.uid)) || 'var(--chart-1)';
    const expiryBar = expiry.length ? '<div class="expirybar" role="img" aria-label="积分到期分布">' +
      expiry.map(s => {
        const title = s.source + '\n' + fmtTok(s.amount) + ' 积分\n到期时间 ' +
          pkExpiryDateTime(s.expiresAt) + '（' + pkExpiryText(s.expiresAt) + '）';
        return '<i style="--seg-color:' + expiryColor +
          ';--seg-op:' + pkCreditOpacity(s.days).toFixed(5) +
          ';--seg-flex:' + Math.max(0.008, s.amount / expiryTotal).toFixed(4) +
          ' 1 0" title="' + esc(title) + '"></i>';
      }).join('') + '</div>' : '';
    return '<div class="pk-card">' +
      '<div class="who"><span class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
      '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
      '<div class="big">' + fmtTok(a.remain) + '</div>' +
      '<div class="sub">共 ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
      ' 个包 · 占最高 ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div>' +
      '<div class="mixbar">' + bar + '</div>' +
      expiryBar +
      '<div class="pk-legend">' + legend + '</div>' +
      '</div>';
  }).join('');

  $('pkNote').textContent = list.length + ' 个账号 · 实时查询上游';

  // 逐包明细：每个账号一个表；排序规则由视图顶部的选择器决定（默认到期近的在前）
  $('pkDetail').innerHTML = list.map(a => {
    if (a.error) return '';
    const groups = pkDetailGroups(a.packages || [], detailLimit, pkSortMode);
    const rowOf = (p, rowGroup) => {
      const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
      const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
                  (p.package_code || '').replace(/^TCACA_/, '');
      return '<tr' + (rowGroup ? ' class="pk-hidden-row pk-' + rowGroup +
        '-row" data-pk-row="' + rowGroup + '" hidden' : '') +
        '><td class="mark" aria-hidden="true"><i style="--seg-color:' +
        colorOf(k) + '"></i></td>' +
      '<td>' + esc(p.name || '(未命名)') +
        (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
      '<td class="num">' + fmtTok(p.size) + '</td>' +
      '<td class="num">' + fmtTok(p.remain) + '</td>' +
      '<td class="num">' + fmtTok(p.used) + '</td>' +
      '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
      '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
      '</tr>';
    };
    const groupSummary = (group, label, count, size, remain) =>
      '<tr class="pk-group-summary"><td colspan="7"><button type="button" class="pk-group-toggle"' +
      ' data-pk-group="' + group + '" data-count="' + count + '" data-size="' + size +
      '" data-remain="' + remain + '" aria-expanded="false">' + label + '，展开</button></td></tr>';
    const rows = groups.visible.map(p => rowOf(p, '')).join('');
    const restSummary = groups.rest.length
      ? groupSummary('rest', '其余未用完 ' + groups.rest.length + ' 个包（面额合计 ' +
          fmtTok(groups.restSize) + ' · 剩余 ' + fmtTok(groups.restRemain) + '）',
          groups.rest.length, groups.restSize, groups.restRemain) +
        groups.rest.map(p => rowOf(p, 'rest')).join('')
      : '';
    const usedSummary = groups.used.length
      ? groupSummary('used', '已用完 ' + groups.used.length + ' 个包（面额合计 ' +
          fmtTok(groups.usedSize) + '）', groups.used.length, groups.usedSize, 0) +
        groups.used.map(p => rowOf(p, 'used')).join('')
      : '';
    return '<div class="box"><header><h3>' +
      esc(a.nickname || a.uid.slice(0, 8)) + ' · ' + esc(a.realm || '') +
      '</h3><span class="grow"></span><span class="note">余额 ' + fmtTok(a.remain) +
      ' / 总额 ' + fmtTok(a.size) + ' · 可用 ' + (groups.visible.length + groups.rest.length) + ' 个包' +
      (groups.used.length ? ' / 已用完 ' + groups.used.length + ' 个' : '') +
      ' · ' + (PK_SORT_SHORT[pkSortMode] || '') + '展示前 ' + pkDetailLimitValue(detailLimit) + ' 条</span>' +
      '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
      '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
      '<th class="num">发放</th><th class="num">到期</th>' +
      '</tr></thead><tbody>' + rows + restSummary + usedSummary + '</tbody></table></div></div>';
  }).join('');
}

if ($('pkDetail')) $('pkDetail').addEventListener('click', ev => {
  const btn = ev.target.closest('button[data-pk-group]');
  if (!btn) return;
  const body = btn.closest('tbody');
  if (!body) return;
  const group = btn.dataset.pkGroup;
  const expanded = btn.getAttribute('aria-expanded') === 'true';
  body.querySelectorAll('tr[data-pk-row="' + group + '"]').forEach(row => { row.hidden = expanded; });
  const count = btn.dataset.count || '0';
  const size = btn.dataset.size || '0';
  const remain = btn.dataset.remain || '0';
  btn.setAttribute('aria-expanded', String(!expanded));
  if (group === 'rest') {
    btn.textContent = expanded
      ? '其余未用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + ' · 剩余 ' +
        fmtTok(remain) + '），展开'
      : '收起其余未用完 ' + count + ' 个包';
  } else {
    btn.textContent = expanded
      ? '已用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + '），展开'
      : '收起已用完 ' + count + ' 个包';
  }
});

/* 最近一次拉到的积分构成数据与生效的明细条数：切换排序规则时只重排内存数据，
   不重新向上游请求（逐包明细是逐账号实时查询，刷新代价高）。 */
let lastPackages = null, lastPackagesLimit;

/* 排序规则控件：恢复上次选择（localStorage 跨会话记住）并绑定切换。 */
if ($('pkSort')) {
  pkSortMode = pkSortLoad();
  $('pkSort').value = pkSortMode;
  $('pkSort').onchange = () => {
    const v = $('pkSort').value;
    pkSortMode = Object.prototype.hasOwnProperty.call(PK_SORT_LABELS, v) ? v : PK_SORT_DEFAULT;
    pkSortSave(pkSortMode);
    if (lastPackages) renderPackages(lastPackages, lastPackagesLimit);
  };
}

async function loadPackages() {
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('pkDetail').innerHTML = '';
  $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">查询中…</div>';
  try {
    const [d, c] = await Promise.all([
      api('packages'),
      api('config').catch(() => null),
    ]);
    lastPackages = d;                            // 缓存供排序切换即时重排
    lastPackagesLimit = pkDetailLimit(c && c.config);
    renderPackages(lastPackages, lastPackagesLimit);
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
    $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">读取失败：' + esc(e.message) + '</div>';
  }
}

if ($('btnPk')) $('btnPk').onclick = loadPackages;
