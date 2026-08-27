/* SBX 流量面板 —— 前端逻辑（无外部依赖） */
'use strict';

var state = { days: 60, nodeId: null, summary: null, live: null, serverOffsetMs: 0, tz: '' };

var inflight = {};
function api(path, params) {
  var key = path + JSON.stringify(params || {});
  if (inflight[key]) return inflight[key];
  var u = new URL(path, location.origin);
  if (params) Object.keys(params).forEach(function (k) { u.searchParams.set(k, params[k]); });
  var req = fetch(u, { cache: 'no-store' }).then(function (r) {
    if (r.status === 401) { location.replace('/login'); throw new Error('未登录'); }
    if (!r.ok) throw new Error('请求失败 ' + r.status);
    return r.json();
  }).finally(function () { delete inflight[key]; });
  inflight[key] = req;
  return req;
}

/* ---------- 格式化 ---------- */
var UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
function fmtBytes(n) {
  n = Number(n) || 0;
  var i = 0, v = n;
  while (v >= 1024 && i < UNITS.length - 1) { v /= 1024; i++; }
  var d = v < 10 && i > 0 ? 2 : (v < 100 && i > 0 ? 1 : 0);
  return v.toFixed(d) + ' ' + UNITS[i];
}
function fmtRate(n) { return fmtBytes(n) + '/s'; }
function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
    return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
  });
}

function toast(msg) {
  var el = document.getElementById('toast');
  el.textContent = msg; el.classList.add('show');
  clearTimeout(toast._t);
  toast._t = setTimeout(function () { el.classList.remove('show'); }, 4000);
}
function setText(id, txt) {
  var el = document.getElementById(id);
  if (el && el.textContent !== txt) el.textContent = txt;
}

/* ---------- 数值缓动（按需 rAF，尊重 prefers-reduced-motion） ---------- */
var reducedMotion = !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
var eased = {};
function easeTo(id, target, fmt) {
  if (reducedMotion) { setText(id, fmt(target)); return; }
  var e = eased[id];
  if (!e) { e = eased[id] = { cur: target, target: target, fmt: fmt }; setText(id, fmt(target)); return; }
  e.target = target; e.fmt = fmt;
  kickEase();
}
function tickEase() {
  var any = false;
  for (var id in eased) {
    var e = eased[id];
    var diff = e.target - e.cur;
    if (Math.abs(diff) < Math.max(1, Math.abs(e.target) * 0.005)) e.cur = e.target;
    else { e.cur += diff * 0.22; any = true; }
    setText(id, e.fmt(e.cur));
  }
  return any;
}
// 按需 rAF 调度：有动画才跑，收敛或页面隐藏即停，不再 25FPS 永久轮询。
var easeRunning = false;
function easeLoop() {
  if (document.hidden) { easeRunning = false; return; }
  if (!tickEase()) { easeRunning = false; return; }
  requestAnimationFrame(easeLoop);
}
function kickEase() {
  if (easeRunning || reducedMotion) return;
  easeRunning = true;
  requestAnimationFrame(easeLoop);
}

/* ---------- 渲染：概览（低频 summary） ---------- */
function renderSummary(s) {
  state.summary = s;
  state.tz = s.tz || state.tz;
  syncServerClock(s.now);
  easeTo('kpi-today-total', s.today.rx + s.today.tx, fmtBytes);
  easeTo('kpi-all-total', s.total.rx + s.total.tx, fmtBytes);
  setText('kpi-nodes', s.nodes.length);
  renderNodeCards(s);
  renderNodeSelect(s);
  if (s.error) toast(s.error);
}

/* ---------- 节点卡片 ---------- */
function portText(n) { return n.port != null ? n.port : '—'; }
function quotaLine(n) {
  if (!n.quota_enabled) return '<div class="node-stat"><span>流量配额</span><b>' + fmtBytes(n.quota_used_bytes) + ' / 不限</b></div>';
  var remain = typeof n.quota_remaining_bytes === 'number' ? n.quota_remaining_bytes : Math.max(0, n.quota_limit_bytes - n.quota_used_bytes);
  return '<div class="node-stat"><span>本期用量</span><b>' + fmtBytes(n.quota_used_bytes) + ' / ' + fmtBytes(n.quota_limit_bytes) + '</b><small>剩余 ' + fmtBytes(remain) + '</small></div>';
}
function accessText(n) {
  if (n.access_state === 'quota_blocked') return '已暂停接入';
  if (n.quota_enabled) return '节点可连接';
  return '不限流量';
}
// 下次自动归零剩余时间（由实时接口高频刷新）。
function formatCountdown(secs) {
  if (secs == null || !(secs > 0)) return '—';
  var d = Math.floor(secs / 86400);
  var h = Math.floor((secs % 86400) / 3600);
  var m = Math.floor((secs % 3600) / 60);
  var s = secs % 60;
  if (d > 0) return d + '天 ' + pad2(h) + ':' + pad2(m) + ':' + pad2(s);
  if (h > 0) return pad2(h) + ':' + pad2(m) + ':' + pad2(s);
  return pad2(m) + ':' + pad2(s);
}
function pad2(x) { return (x < 10 ? '0' : '') + x; }
// 用 API 的服务器时钟校准倒计时，避免用户设备时间不准导致剩余时间漂移。
function syncServerClock(serverUnix) {
  if (typeof serverUnix === 'number' && serverUnix > 0) {
    state.serverOffsetMs = serverUnix * 1000 - Date.now();
  }
}
function serverNowUnix() { return Math.floor((Date.now() + state.serverOffsetMs) / 1000); }
// 下次归零绝对时间 + 服务器校准倒计时。
function formatResetFuture(ts) {
  var d = new Date(ts * 1000);
  var secs = ts - serverNowUnix();
  var date;
  try {
    var parts = new Intl.DateTimeFormat('zh-CN', {
      timeZone: state.tz || undefined, month: 'numeric', day: 'numeric',
      hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23'
    }).formatToParts(d);
    var p = {}; parts.forEach(function (x) { if (x.type !== 'literal') p[x.type] = x.value; });
    date = p.month + '月' + p.day + '日 ' + p.hour + ':' + p.minute + ':' + p.second;
  } catch (e) {
    date = (d.getMonth() + 1) + '月' + d.getDate() + '日 ' + pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
  }
  return date + '（剩 ' + formatCountdown(secs) + '）';
}
function resetLine(n) {
  if (!n.quota_enabled || !n.reset_enabled) return '';
  var secs = n.reset_next_at ? (n.reset_next_at - serverNowUnix()) : null;
  var txt = (secs != null && secs <= 0) ? '待执行' : formatCountdown(secs);
  if (n.access_state === 'quota_blocked') return '<div class="node-stat reset-stat blocked"><span>自动恢复</span><b>' + esc(resetRuleOf(n)) + '</b>' +
    '<small data-node-reset="' + esc(n.id) + '">配额归零后恢复 · ' + txt + '</small></div>';
  return '<div class="node-stat reset-stat"><span>自动归零</span><b>' + esc(resetRuleOf(n)) + '</b>' +
    '<small data-node-reset="' + esc(n.id) + '">' + txt + '</small></div>';
}
function resetRuleOf(n) {
  if (!(n && n.reset_day >= 1)) return '未设置';
  return '每月 ' + n.reset_day + ' 日 ' + String(n.reset_time || '00:00:00');
}
/* 每月自动归零计划：D:HH:MM:SS（30 日 / 24 时 / 60 分 / 60 秒）。 */
function parseResetSpec(spec) {
  var m = /^(\d{1,2}):(\d{2}):(\d{2}):(\d{2})$/.exec(String(spec || '').trim());
  if (!m) return null;
  var day = parseInt(m[1], 10), hh = parseInt(m[2], 10);
  var mm = parseInt(m[3], 10), ss = parseInt(m[4], 10);
  if (!(day >= 1 && day <= 30) || hh > 23 || mm > 59 || ss > 59) return null;
  return { day: day, hour: hh, minute: mm, second: ss, time: pad2(hh) + ':' + pad2(mm) + ':' + pad2(ss) };
}
function resetSpecOf(n) {
  if (!(n.reset_day >= 1)) return '';
  var t = String(n.reset_time || '00:00:00').split(':');
  return n.reset_day + ':' + pad2(parseInt(t[0], 10) || 0) + ':' + pad2(parseInt(t[1], 10) || 0) + ':' + pad2(parseInt(t[2], 10) || 0);
}
function friendlyReset(spec) {
  var p = parseResetSpec(spec);
  return p ? (p.day + ' 日 ' + p.time) : '';
}
function applyResetSpec(spec) {
  var p = parseResetSpec(spec);
  var btn = document.getElementById('pol-reset-spec');
  if (btn) btn.setAttribute('data-spec', p ? spec : '');
  setText('pol-reset-spec-value', p ? friendlyReset(spec) : '请选择');
  setText('pol-reset-rule', p ? ('每月 ' + p.day + ' 日 ' + p.time) : '每月 —');
  setText('pol-reset-hint', p ? '按面板时区执行，精确到秒' : '完整支持 30 日 × 24 时 × 60 分 × 60 秒');
}

/* ---------- 每月归零时间滚轮：日 / 时 / 分 / 秒 ---------- */
var resetPicker = { day: 1, hour: 0, minute: 0, second: 0, built: false, raf: {} };
function buildWheel(col, lo, hi) {
  var sc = document.querySelector('[data-col="' + col + '"]');
  sc.innerHTML = '';
  var frag = document.createDocumentFragment();
  function spacer() { var d = document.createElement('div'); d.className = 'wheel-item spacer'; return d; }
  frag.appendChild(spacer()); frag.appendChild(spacer());
  for (var v = lo; v <= hi; v++) {
    var d = document.createElement('div');
    d.className = 'wheel-item'; d.dataset.v = v;
    d.textContent = pad2(v); frag.appendChild(d);
  }
  frag.appendChild(spacer()); frag.appendChild(spacer());
  sc.appendChild(frag);
  sc.addEventListener('scroll', function () {
    cancelAnimationFrame(resetPicker.raf[col]);
    resetPicker.raf[col] = requestAnimationFrame(function () { onWheelScroll(col); });
  }, { passive: true });
}
function onWheelScroll(col) {
  var sc = document.querySelector('[data-col="' + col + '"]');
  var items = sc.querySelectorAll('.wheel-item[data-v]');
  var idx = Math.max(0, Math.min(items.length - 1, Math.round(sc.scrollTop / 40)));
  resetPicker[col] = parseInt(items[idx].dataset.v, 10);
  for (var i = 0; i < items.length; i++) items[i].classList.toggle('sel', i === idx);
  updatePickerPreview();
}
function positionWheel(col, value) {
  var sc = document.querySelector('[data-col="' + col + '"]');
  var idx = col === 'day' ? value - 1 : value;
  sc.scrollTop = Math.max(0, idx) * 40;
  onWheelScroll(col);
}
function updatePickerPreview() {
  setText('reset-picker-preview', '第 ' + resetPicker.day + ' 日 ' + pad2(resetPicker.hour) + ':' + pad2(resetPicker.minute) + ':' + pad2(resetPicker.second));
}
function openResetPicker() {
  if (!resetPicker.built) {
    buildWheel('day', 1, 30); buildWheel('hour', 0, 23);
    buildWheel('minute', 0, 59); buildWheel('second', 0, 59);
    resetPicker.built = true;
  }
  var spec = document.getElementById('pol-reset-spec').getAttribute('data-spec') || '1:00:00:00';
  var p = parseResetSpec(spec) || { day: 1, hour: 0, minute: 0, second: 0 };
  resetPicker.day = p.day; resetPicker.hour = p.hour;
  resetPicker.minute = p.minute; resetPicker.second = p.second;
  positionWheel('day', p.day); positionWheel('hour', p.hour);
  positionWheel('minute', p.minute); positionWheel('second', p.second);
  updatePickerPreview();
  document.getElementById('reset-picker-mask').classList.add('on');
  document.getElementById('reset-picker').classList.add('on');
  document.body.classList.add('picker-open');
}
function closeResetPicker() {
  document.getElementById('reset-picker-mask').classList.remove('on');
  document.getElementById('reset-picker').classList.remove('on');
  document.body.classList.remove('picker-open');
}
function confirmResetPicker() {
  applyResetSpec(resetPicker.day + ':' + pad2(resetPicker.hour) + ':' + pad2(resetPicker.minute) + ':' + pad2(resetPicker.second));
  closeResetPicker();
}
function nodeStatus(n) {
  if (n.access_state === 'quota_blocked' || n.quota_state === 'exceeded') return '<span class="status-pill danger" data-node-status="' + esc(n.id) + '">已暂停接入</span>';
  if (n.ip_limit_state === 'exceeded') return '<span class="status-pill warn" data-node-status="' + esc(n.id) + '">IP 已达上限</span>';
  return '<span class="status-pill ok" data-node-status="' + esc(n.id) + '">' + accessText(n) + '</span>';
}
function renderNodeCards(s) {
  var host = document.getElementById('node-cards');
  if (!s.nodes.length) { host.innerHTML = '<div class="empty">暂无节点，运行 sbx 菜单添加</div>'; return; }
  host.innerHTML = s.nodes.map(function (n) {
    var total = (n.total && (n.total.rx + n.total.tx)) || 0;
    var ipVal = (n.active_ip_count || 0) + (n.ip_limit_enabled ? ' / ' + n.ip_limit_max : '');
    return '<div class="node-card">' +
      '<div class="node-top">' +
        '<div class="node-title">' +
          '<div class="node-name">' + esc(n.name) + '</div>' +
          '<div class="node-meta-line"><span class="chip">' + esc(n.type || '—') + '</span>' +
            '<span class="port">端口 ' + esc(portText(n)) + '</span></div>' +
        '</div>' +
        '<div class="node-rate">' +
          '<b class="up" data-node-live="' + esc(n.id) + '" data-kind="rate-up">—</b>' +
          '<b class="down" data-node-live="' + esc(n.id) + '" data-kind="rate-down">—</b>' +
        '</div>' +
      '</div>' +
      '<div class="node-stats">' +
        '<div class="node-stat"><span>累计流量</span><b>' + fmtBytes(total) + '</b></div>' +
        quotaLine(n) +
        '<div class="node-stat"><span>节点接入</span><b data-node-access="' + esc(n.id) + '">' + esc(accessText(n)) + '</b></div>' +
        '<div class="node-stat"><span>TCP / UDP</span><b><span data-node-live="' + esc(n.id) + '" data-kind="conns">—</span> / <span data-node-live="' + esc(n.id) + '" data-kind="conns_udp">—</span></b></div>' +
        resetLine(n) +
      '</div>' +
      '<button class="ip-strip" data-view-ips="' + esc(n.id) + '">' +
        '<span class="ip-strip-label">在线 IP</span>' +
        '<span class="ip-strip-val" data-node-ips="' + esc(n.id) + '">' + esc(ipVal) + '</span>' +
        '<span class="ip-strip-arrow">›</span>' +
      '</button>' +
      '<div class="node-foot">' +
        nodeStatus(n) +
        '<div class="node-actions"><button class="mini-btn primary" data-manage="' + esc(n.id) + '">管理</button></div>' +
      '</div>' +
    '</div>';
  }).join('');
  if (state.live) renderLive(state.live);
}

function renderNodeSelect(s) {
  var sel = document.getElementById('node-select');
  var want = state.nodeId != null ? String(state.nodeId) : (s.nodes.length ? String(s.nodes[0].id) : '');
  var sig = s.nodes.map(function (n) { return n.id + ':' + n.name; }).join('|');
  if (sel._sig !== sig) {
    sel._sig = sig;
    sel.innerHTML = s.nodes.map(function (n) { return '<option value="' + esc(n.id) + '">' + esc(n.name) + '</option>'; }).join('');
  }
  if (want && sel.value !== want) sel.value = want;
  if (state.nodeId == null && want) { state.nodeId = want; loadNodeDaily(); }
}

/* ---------- 渲染：实时（高频 live） ---------- */
function renderLive(v) {
  state.live = v;
  syncServerClock(v.now);
  var healthy = v.healthy, live = v.rate_known !== false;
  setText('status-txt', healthy ? '实时监控中' : (v.error ? '采集异常' : '等待采集'));
  document.getElementById('pulse').className = 'pulse' + (live ? '' : ' stale');

  var rt = v.rate_total || { rx: 0, tx: 0 };
  easeTo('hero-rate', live ? rt.rx + rt.tx : 0, function (n) { return live ? fmtRate(n) : '—'; });
  easeTo('hero-up', live ? rt.rx : 0, function (n) { return live ? fmtRate(n) : '—'; });
  easeTo('hero-down', live ? rt.tx : 0, function (n) { return live ? fmtRate(n) : '—'; });
  setText('kpi-conns', typeof v.conns_total === 'number' ? v.conns_total : '—');
  setText('kpi-conns-udp', typeof v.conns_udp_total === 'number' ? v.conns_udp_total : '—');

  var byId = {};
  (v.nodes || []).forEach(function (n) { byId[n.id] = n; });
  document.querySelectorAll('[data-node-live]').forEach(function (el) {
    var id = el.getAttribute('data-node-live'), n = byId[id];
    if (!n) return;
    var kind = el.getAttribute('data-kind');
    if (kind === 'conns') el.textContent = (typeof n.conns_tcp === 'number') ? n.conns_tcp : '—';
    else if (kind === 'conns_udp') el.textContent = (typeof n.conns_udp === 'number') ? n.conns_udp : '—';
    else if (kind === 'rate-up') el.textContent = live ? '↑ ' + fmtRate(n.rate.rx) : '—';
    else if (kind === 'rate-down') el.textContent = live ? '↓ ' + fmtRate(n.rate.tx) : '—';
  });
  // 在线 IP 数（高频刷新：TCP 断开后立即回落）
  document.querySelectorAll('[data-node-ips]').forEach(function (el) {
    var id = el.getAttribute('data-node-ips'), n = byId[id];
    if (!n) return;
    var val = (typeof n.active_ip_count === 'number') ? n.active_ip_count : 0;
    el.textContent = n.ip_limit_enabled ? (val + ' / ' + n.ip_limit_max) : val;
  });
  // 节点接入状态由后端配额状态权威决定（与 nft 阻断同源）。
  document.querySelectorAll('[data-node-access]').forEach(function (el) {
    var id = el.getAttribute('data-node-access'), n = byId[id];
    if (!n) return;
    el.textContent = n.access_state === 'quota_blocked' ? '已暂停接入' : (n.quota_enabled ? '节点可连接' : '不限流量');
  });
  document.querySelectorAll('[data-node-status]').forEach(function (el) {
    var id = el.getAttribute('data-node-status'), n = byId[id];
    if (!n) return;
    var blocked = n.access_state === 'quota_blocked';
    el.className = 'status-pill ' + (blocked ? 'danger' : 'ok');
    el.textContent = blocked ? '已暂停接入' : (n.quota_enabled ? '节点可连接' : '不限流量');
  });
  // 自动归零倒计时（服务器时钟校准，实时接口高频刷新）
  document.querySelectorAll('[data-node-reset]').forEach(function (el) {
    var id = el.getAttribute('data-node-reset'), n = byId[id];
    if (!n || !n.reset_enabled || !n.reset_next_at) return;
    var secs = n.reset_next_at - serverNowUnix();
    el.textContent = secs <= 0 ? '待执行' : ((n.access_state === 'quota_blocked' ? '归零后恢复 · ' : '剩余 ') + formatCountdown(secs));
  });
}

/* ---------- 明细表格 ---------- */
var cache = { daily: null, nodeDaily: null };

function renderTable(hostId, rows) {
  var host = document.getElementById(hostId); if (!host) return;
  if (!rows || !rows.length) { host.innerHTML = '<div class="empty">暂无数据</div>'; return; }
  var html = '<div class="table-scroll"><table><thead><tr>' +
    '<th>日期</th><th class="up">上传</th><th class="down">下载</th><th>合计</th>' +
    '</tr></thead><tbody>';
  rows.slice().reverse().forEach(function (r) {
    html += '<tr><td class="date">' + esc(r.day) + '</td>' +
      '<td class="up">' + fmtBytes(r.rx) + '</td>' +
      '<td class="down">' + fmtBytes(r.tx) + '</td>' +
      '<td><b>' + fmtBytes(r.rx + r.tx) + '</b></td></tr>';
  });
  html += '</tbody></table></div>';
  host.innerHTML = html;
}
function drawDaily() { if (cache.daily) renderTable('daily-table', cache.daily); }
function drawNodeDaily() { if (cache.nodeDaily) renderTable('node-daily-table', cache.nodeDaily); }
function loadDaily() {
  return api('/api/daily', { days: 60 }).then(function (d) { cache.daily = d.days; drawDaily(); })
    .catch(function (e) { if (e.message !== '未登录') toast(e.message); });
}
function loadNodeDaily() {
  if (state.nodeId == null) return Promise.resolve();
  return api('/api/daily', { days: 60, scope: 'node:' + state.nodeId })
    .then(function (d) { cache.nodeDaily = d.days; drawNodeDaily(); })
    .catch(function (e) { if (e.message !== '未登录') toast(e.message); });
}

/* ---------- 事件 ---------- */
document.getElementById('node-select').addEventListener('change', function (e) {
  state.nodeId = e.target.value; loadNodeDaily();
});

/* ---------- 三页底部导航 ---------- */
(function initNav() {
  var valid = { home: 1, daily: 1, node: 1 }, positions = { home: 0, daily: 0, node: 0 };
  var current = (location.hash || '#home').slice(1); if (!valid[current]) current = 'home';
  function show(name, push) {
    if (!valid[name]) name = 'home';
    positions[current] = window.scrollY || 0; current = name;
    document.querySelectorAll('.view').forEach(function (v) { v.classList.toggle('on', v.id === 'view-' + name); });
    document.querySelectorAll('.tab').forEach(function (b) { b.classList.toggle('on', b.dataset.view === name); });
    if (push && location.hash !== '#' + name) history.pushState(null, '', '#' + name);
    requestAnimationFrame(function () { window.scrollTo(0, positions[name] || 0); });
    if (name === 'daily' && !cache.daily) loadDaily();
    if (name === 'node' && !cache.nodeDaily) loadNodeDaily();
  }
  document.querySelectorAll('.tab').forEach(function (b) {
    b.addEventListener('click', function () { show(b.dataset.view, true); });
  });
  window.addEventListener('popstate', function () { show((location.hash || '#home').slice(1), false); });
  show(current, false);
})();

/* ---------- 启动与轮询 ---------- */
function loadSummary() { return api('/api/summary').then(renderSummary).catch(function (e) { if (e.message !== '未登录') toast(e.message); }); }
function loadLive() { return api('/api/live').then(renderLive).catch(function () {}); }

loadSummary().then(function () { loadLive(); loadDaily(); });
setInterval(function () { if (!document.hidden) loadLive(); }, 2000);
setInterval(function () { if (!document.hidden) loadSummary(); }, 8000);
setInterval(function () { if (!document.hidden) { loadDaily(); loadNodeDaily(); } }, 60000);
document.addEventListener('visibilitychange', function () {
  if (!document.hidden) { loadLive(); loadSummary(); }
});

/* ==================== 节点策略管理（Quota / IP Limit） ==================== */
var policyState = { nodeId: null, summaryNode: null };

function showPolicy(nodeId) {
  var n = (state.summary && state.summary.nodes || []).filter(function (x) { return String(x.id) === String(nodeId); })[0];
  if (!n) return;
  policyState.nodeId = String(nodeId);
  policyState.summaryNode = n;
  document.getElementById('drawer-node-name').textContent = n.name;
  document.getElementById('pol-quota-used').textContent = fmtBytes(n.quota_used_bytes || 0);
  document.getElementById('pol-access-state').textContent = accessText(n);
  document.getElementById('pol-ip-active').textContent = (n.active_ip_count || 0);
  document.getElementById('pol-quota-enable').checked = !!n.quota_enabled;
  document.getElementById('pol-ip-enable').checked = !!n.ip_limit_enabled;
  document.getElementById('pol-quota-box').classList.toggle('hidden', !n.quota_enabled);
  document.getElementById('pol-ip-box').classList.toggle('hidden', !n.ip_limit_enabled);
  // 每月自动归零是流量配额内部的计费周期。
  var quotaOn = !!n.quota_enabled;
  var resetOn = quotaOn && !!n.reset_enabled;
  document.getElementById('pol-reset-enable').checked = resetOn;
  document.getElementById('pol-reset-box').classList.toggle('hidden', !resetOn);
  applyResetSpec(resetSpecOf(n));
  document.getElementById('pol-reset-next').textContent = (resetOn && n.reset_next_at)
    ? formatResetFuture(n.reset_next_at) : '—';
  if (n.quota_enabled && n.quota_limit_bytes > 0) {
    var g = n.quota_limit_bytes / (1024 * 1024 * 1024);
    var unit = 'GiB';
    if (g >= 1024 && g % 1024 === 0) { g = g / 1024; unit = 'TiB'; }
    document.getElementById('pol-quota-val').value = g;
    document.getElementById('pol-quota-unit').value = unit;
  } else {
    document.getElementById('pol-quota-val').value = '';
  }
  document.getElementById('pol-ip-max').value = n.ip_limit_max > 0 ? n.ip_limit_max : '';
  hidePolError();
  openDrawer('policy-drawer');
}

function openDrawer(id) {
  document.getElementById('drawer-mask').classList.add('on');
  document.getElementById(id).classList.add('on');
}
function closeDrawer(id) {
  document.getElementById('drawer-mask').classList.remove('on');
  document.getElementById(id).classList.remove('on');
}
function hidePolError() { document.getElementById('pol-error').classList.add('hidden'); }
function showPolError(msg) {
  var el = document.getElementById('pol-error');
  el.textContent = msg; el.classList.remove('hidden');
}

function unitToBytes(val, unit) {
  var n = Number(val);
  if (!(n > 0)) return 0;
  var mult = unit === 'TiB' ? 1024 * 1024 * 1024 * 1024 : 1024 * 1024 * 1024;
  return Math.round(n * mult);
}

function savePolicy() {
  var quotaOn = document.getElementById('pol-quota-enable').checked;
  var ipOn = document.getElementById('pol-ip-enable').checked;
  var quotaVal = document.getElementById('pol-quota-val').value;
  var quotaUnit = document.getElementById('pol-quota-unit').value;
  var ipMax = document.getElementById('pol-ip-max').value;
  // 定时重置依附于流量配额：配额没开时强制关闭并清语义，不发送开启。
  var resetOn = quotaOn && document.getElementById('pol-reset-enable').checked;
  var parsed = parseResetSpec(document.getElementById('pol-reset-spec').getAttribute('data-spec'));
  if (resetOn && !parsed) {
    showPolError('请设置完整的每月归零时间（日、时、分、秒）');
    return;
  }

  var body = {
    quota_enabled: quotaOn,
    quota_limit_bytes: quotaOn ? unitToBytes(quotaVal, quotaUnit) : 0,
    ip_limit_enabled: ipOn,
    ip_limit_max: ipOn ? Number(ipMax) : 0,
    reset_enabled: resetOn,
    reset_day: parsed ? parsed.day : 0,
    reset_time: parsed ? parsed.time : '00:00:00'
  };
  if (quotaOn && body.quota_limit_bytes <= 0) { showPolError('流量额度必须大于 0'); return; }
  if (ipOn && !(body.ip_limit_max >= 1)) { showPolError('最大 IP 数必须 ≥ 1'); return; }

  var btn = document.getElementById('pol-save');
  btn.disabled = true; btn.textContent = '保存中…';
  fetch('/api/nodes/' + policyState.nodeId + '/policy', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  }).then(function (r) {
    if (r.status === 401) { location.replace('/login'); throw new Error('未登录'); }
    return r.json().then(function (d) { if (!r.ok) throw new Error(d.error || ('请求失败 ' + r.status)); return d; });
  }).then(function () {
    btn.disabled = false; btn.textContent = '保存修改';
    closeDrawer('policy-drawer');
    toast('已保存'); loadSummary();
  }).catch(function (e) {
    btn.disabled = false; btn.textContent = '保存修改';
    if (e.message !== '未登录') showPolError(e.message);
  });
}

function resetQuota() {
  if (!window.confirm('确认重置该节点当前额度使用量？\n历史累计流量不会删除。')) return;
  var btn = document.getElementById('pol-quota-reset');
  btn.disabled = true;
  fetch('/api/nodes/' + policyState.nodeId + '/quota/reset', { method: 'POST' })
    .then(function (r) {
      if (r.status === 401) { location.replace('/login'); throw new Error('未登录'); }
      return r.json().then(function (d) { if (!r.ok) throw new Error(d.error || ('请求失败 ' + r.status)); return d; });
    })
    .then(function (d) {
      btn.disabled = false;
      document.getElementById('pol-quota-used').textContent = fmtBytes(d.quota_used_bytes || 0);
      toast('已重置'); loadSummary();
    })
    .catch(function (e) {
      btn.disabled = false;
      if (e.message !== '未登录') showPolError(e.message);
    });
}

function showActiveIPs(nodeId) {
  var id = nodeId != null ? String(nodeId) : policyState.nodeId;
  var name = '';
  var found = (state.summary && state.summary.nodes || []).filter(function (x) { return String(x.id) === id; })[0];
  if (found) name = found.name;
  else if (policyState.summaryNode && String(policyState.summaryNode.id) === id) name = policyState.summaryNode.name;
  document.getElementById('ips-node-name').textContent = name;
  var list = document.getElementById('ips-list');
  list.innerHTML = '<div class="empty">加载中…</div>';
  openDrawer('ips-drawer');
  fetch('/api/nodes/' + id + '/active-ips')
    .then(function (r) {
      if (r.status === 401) { location.replace('/login'); throw new Error('未登录'); }
      return r.json().then(function (d) { if (!r.ok) throw new Error(d.error || '请求失败'); return d; });
    })
    .then(function (d) {
      var ips = d.ips || [];
      if (!ips.length) { list.innerHTML = '<div class="empty">暂无在线 IP</div>'; return; }
      list.innerHTML = ips.map(function (ip) {
        var v6 = ip.indexOf(':') >= 0;
        return '<div class="ip-item"><span class="ip-addr">' + esc(ip) + '</span>' +
          '<span class="ip-tag">' + (v6 ? 'IPv6' : 'IPv4') + '</span></div>';
      }).join('');
    })
    .catch(function (e) { if (e.message !== '未登录') { list.innerHTML = '<div class="empty">加载失败</div>'; toast(e.message); } });
}

/* 事件委托：节点卡片上的在线 IP 条 + 管理按钮（动态渲染） */
document.getElementById('node-cards').addEventListener('click', function (e) {
  var ips = e.target.closest('[data-view-ips]');
  if (ips) { showActiveIPs(ips.getAttribute('data-view-ips')); return; }
  var mg = e.target.closest('[data-manage]');
  if (mg) { showPolicy(mg.getAttribute('data-manage')); return; }
});
document.getElementById('drawer-close').addEventListener('click', function () { closeDrawer('policy-drawer'); });
document.getElementById('drawer-mask').addEventListener('click', function () { closeDrawer('policy-drawer'); closeDrawer('ips-drawer'); });
document.getElementById('pol-cancel').addEventListener('click', function () { closeDrawer('policy-drawer'); });
document.getElementById('pol-save').addEventListener('click', savePolicy);
document.getElementById('pol-quota-reset').addEventListener('click', resetQuota);
document.getElementById('ips-close').addEventListener('click', function () { closeDrawer('ips-drawer'); });
document.getElementById('pol-quota-enable').addEventListener('change', function (e) {
  document.getElementById('pol-quota-box').classList.toggle('hidden', !e.target.checked);
  if (!e.target.checked) {
    document.getElementById('pol-reset-enable').checked = false;
    document.getElementById('pol-reset-box').classList.add('hidden');
  }
});
document.getElementById('pol-ip-enable').addEventListener('change', function (e) {
  document.getElementById('pol-ip-box').classList.toggle('hidden', !e.target.checked);
});
document.getElementById('pol-reset-enable').addEventListener('change', function (e) {
  document.getElementById('pol-reset-box').classList.toggle('hidden', !e.target.checked);
});
document.getElementById('pol-reset-spec').addEventListener('click', openResetPicker);
document.getElementById('reset-picker-ok').addEventListener('click', confirmResetPicker);
document.getElementById('reset-picker-cancel').addEventListener('click', closeResetPicker);
document.getElementById('reset-picker-mask').addEventListener('click', closeResetPicker);
