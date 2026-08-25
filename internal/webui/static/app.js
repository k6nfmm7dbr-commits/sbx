/* SBX 流量面板 —— 前端逻辑（无外部依赖） */
'use strict';

var state = { days: 60, nodeId: null, summary: null, live: null };

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
var UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
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

/* ---------- 数值缓动 ---------- */
var eased = {};
function easeTo(id, target, fmt) {
  var e = eased[id];
  if (!e) { e = eased[id] = { cur: target, target: target, fmt: fmt }; setText(id, fmt(target)); return; }
  e.target = target; e.fmt = fmt;
}
function tickEase() {
  for (var id in eased) {
    var e = eased[id];
    var diff = e.target - e.cur;
    if (Math.abs(diff) < Math.max(1, Math.abs(e.target) * 0.005)) e.cur = e.target;
    else e.cur += diff * 0.22;
    setText(id, e.fmt(e.cur));
  }
}

/* ---------- 渲染：概览（低频 summary） ---------- */
function renderSummary(s) {
  state.summary = s;
  easeTo('kpi-today-total', s.today.rx + s.today.tx, fmtBytes);
  easeTo('kpi-all-total', s.total.rx + s.total.tx, fmtBytes);
  setText('kpi-nodes', s.nodes.length);
  renderNodeCards(s);
  renderNodeSelect(s);
  if (s.error) toast(s.error);
}

/* ---------- 节点卡片 ---------- */
function portText(n) { return n.port != null ? n.port : '—'; }
function renderNodeCards(s) {
  var host = document.getElementById('node-cards');
  if (!s.nodes.length) { host.innerHTML = '<div class="empty">暂无节点，运行 sbx 菜单添加</div>'; return; }
  host.innerHTML = s.nodes.map(function (n) {
    return '<div class="node-card">' +
      '<div class="node-top">' +
        '<div class="node-title">' +
          '<div class="node-name">' + esc(n.name) + '</div>' +
          '<div class="node-meta-line"><span class="chip">' + esc(n.type || '—') + '</span>' +
            '<span class="port">端口 ' + esc(portText(n)) + '</span></div>' +
        '</div>' +
        '<div class="node-rate">' +
          '<b class="up" data-node-live="' + n.id + '" data-kind="rate-up">—</b>' +
          '<b class="down" data-node-live="' + n.id + '" data-kind="rate-down">—</b>' +
        '</div>' +
      '</div>' +
      '<div class="node-stats">' +
        '<div class="node-stat"><span>TCP 连接</span><b data-node-live="' + n.id + '" data-kind="conns">—</b></div>' +
        '<div class="node-stat"><span>UDP 会话</span><b data-node-live="' + n.id + '" data-kind="conns_udp">—</b></div>' +
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
setInterval(tickEase, 40);

function loadSummary() { return api('/api/summary').then(renderSummary).catch(function (e) { if (e.message !== '未登录') toast(e.message); }); }
function loadLive() { return api('/api/live').then(renderLive).catch(function () {}); }

loadSummary().then(function () { loadLive(); loadDaily(); });
setInterval(function () { if (!document.hidden) loadLive(); }, 2000);
setInterval(function () { if (!document.hidden) loadSummary(); }, 8000);
setInterval(function () { if (!document.hidden) { loadDaily(); loadNodeDaily(); } }, 60000);
document.addEventListener('visibilitychange', function () {
  if (!document.hidden) { loadLive(); loadSummary(); }
});
