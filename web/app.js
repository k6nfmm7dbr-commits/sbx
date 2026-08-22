/* sbx-panel 前端 — 原生 JS + 手绘 SVG，无外部依赖
 * 反应速度优化要点：
 *  1) 拆分接口：高频轮询轻量 /api/live（速率+连接数），重的 /api/summary 只在切范围/低频时拉
 *  2) 实时数字用 requestAnimationFrame 做数值缓动，视觉上连续不跳变
 *  3) hero 迷你曲线本地维护滑动窗口，每帧平滑推进，不等下一次请求
 *  4) 只更新变化的 DOM 文本节点，避免整表 innerHTML 重建导致的闪烁
 */
'use strict';

var TOKEN = new URLSearchParams(location.search).get('token') || '';
var state = { days: 30, sortScope: 'today', nodeId: null, summary: null, live: null, range: '30m' };
var RANGE_LABEL = { '30m': '近 30 分钟', '2h': '近 2 小时', '1d': '近 1 天', '7d': '近 7 天', '30d': '近 30 天' };

function api(path, params) {
  var u = new URL(path, location.origin);
  if (params) Object.keys(params).forEach(function (k) { u.searchParams.set(k, params[k]); });
  if (TOKEN) u.searchParams.set('token', TOKEN);
  return fetch(u, { cache: 'no-store' }).then(function (r) {
    if (r.status === 401) throw new Error('令牌无效，请重新登录');
    if (!r.ok) throw new Error('请求失败 ' + r.status);
    return r.json();
  });
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
function fmtDay(s) { return s ? s.slice(5) : ''; }
function fmtClock(ts) { var d = new Date(ts * 1000); return ('0' + d.getHours()).slice(-2) + ':' + ('0' + d.getMinutes()).slice(-2); }
function fmtDate(ts) { var d = new Date(ts * 1000); return (d.getMonth() + 1) + '/' + d.getDate(); }
function fmtDateTime(ts) { var d = new Date(ts * 1000); return (d.getMonth() + 1) + '/' + d.getDate() + ' ' + ('0' + d.getHours()).slice(-2) + ':00'; }
function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }

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

/* ---------- 数值缓动（让大数字连续变化，不一跳一跳）---------- */
var eased = {};   // id -> {cur, target, fmt}
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
    else e.cur += diff * 0.22;            // 指数逼近，视觉平滑
    setText(id, e.fmt(e.cur));
  }
}

/* ---------- SVG 工具 ---------- */
var NS = 'http://www.w3.org/2000/svg';
function svgEl(name, attrs) {
  var e = document.createElementNS(NS, name);
  if (attrs) for (var k in attrs) e.setAttribute(k, attrs[k]);
  return e;
}
var tipEl = null;
function showTip(evt, html) {
  tipEl = tipEl || document.getElementById('tip');
  tipEl.innerHTML = html; tipEl.classList.add('show');
  var w = tipEl.offsetWidth, h = tipEl.offsetHeight, pad = 12;
  var x = Math.min(Math.max(evt.clientX - w / 2, 8), window.innerWidth - w - 8);
  var y = evt.clientY - h - pad; if (y < 8) y = evt.clientY + pad;
  tipEl.style.left = x + 'px'; tipEl.style.top = y + 'px';
}
function hideTip() { if (tipEl) tipEl.classList.remove('show'); }

function niceMax(v) {
  if (v <= 0) return 1;
  var exp = Math.floor(Math.log(v) / Math.LN10), base = Math.pow(10, exp), f = v / base;
  var step = f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10;
  return step * base;
}

/* ---------- Hero 迷你实时曲线（本地滑动窗口，逐帧平滑）---------- */
var SPARK_N = 80;
var sparkData = [];  // 每项 {up, down}
var sparkTarget = { up: 0, down: 0 };
var sparkInit = false;
function sparkPush(up, down) {
  sparkTarget = { up: up, down: down };
  if (!sparkInit) {   // 首帧：用当前值填满窗口，避免从右下角一小段开始
    sparkInit = true;
    sparkData = [];
    for (var i = 0; i < SPARK_N; i++) sparkData.push({ up: up, down: down });
    drawSpark();
  }
}
function sparkStep() {
  // 平滑逼近目标后压入窗口，让曲线像水流一样推进
  var last = sparkData[sparkData.length - 1] || { up: 0, down: 0 };
  var nu = last.up + (sparkTarget.up - last.up) * 0.25;
  var nd = last.down + (sparkTarget.down - last.down) * 0.25;
  sparkData.push({ up: nu, down: nd });
  while (sparkData.length > SPARK_N) sparkData.shift();
  drawSpark();
}
function drawSpark() {
  var host = document.getElementById('spark');
  if (!host || sparkData.length < 2) return;
  var W = Math.max(300, host.clientWidth || 400), H = 64;
  var maxV = niceMax(sparkData.reduce(function (m, p) { return Math.max(m, p.up, p.down); }, 1));
  var n = sparkData.length, dx = W / (n - 1);
  function line(key, color, fill) {
    var d = '';
    for (var i = 0; i < n; i++) {
      var x = i * dx;
      var y = H - 3 - (H - 6) * (sparkData[i][key] / maxV);
      d += (i ? 'L' : 'M') + x.toFixed(1) + ' ' + y.toFixed(1);
    }
    var parts = '';
    if (fill) parts += '<path d="' + d + 'L' + W + ' ' + H + 'L0 ' + H + 'Z" fill="' + color + '" opacity="0.14"/>';
    parts += '<path d="' + d + '" fill="none" stroke="' + color + '" stroke-width="1.8" stroke-linejoin="round" stroke-linecap="round"/>';
    return parts;
  }
  host.innerHTML = '<svg viewBox="0 0 ' + W + ' ' + H + '" width="' + W + '" height="' + H + '" preserveAspectRatio="none">'
    + line('down', 'var(--down)', true) + line('up', 'var(--up)', true) + '</svg>';
}

/* ---------- 分组柱状图 ---------- */
function drawGrouped(host, rows, opts) {
  opts = opts || {}; host.textContent = '';
  if (!rows.length) { host.innerHTML = '<div class="empty">暂无数据</div>'; return; }
  var avail = Math.max(280, host.parentNode.clientWidth || 600);
  var slot = opts.slot || 24, padL = 54, padR = 12, padT = 12, padB = 26;
  var W = Math.max(avail, rows.length * slot + padL + padR), H = opts.height || 220;
  var iw = W - padL - padR, ih = H - padT - padB;
  var maxV = niceMax(rows.reduce(function (m, r) { return Math.max(m, r.up, r.down); }, 0));
  var svg = svgEl('svg', { viewBox: '0 0 ' + W + ' ' + H, width: W, height: H, role: 'img' });
  for (var g = 0; g <= 4; g++) {
    var y = padT + ih - (ih * g / 4);
    svg.appendChild(svgEl('line', { class: 'grid' + (g === 0 ? ' grid-0' : ''), x1: padL, y1: y, x2: W - padR, y2: y }));
    var t = svgEl('text', { class: 'axis', x: padL - 8, y: y + 3, 'text-anchor': 'end' });
    t.textContent = fmtBytes(maxV * g / 4); svg.appendChild(t);
  }
  var slotW = iw / rows.length, barW = Math.max(2, Math.min(10, slotW / 2 - 2));
  var minGap = 46, lastLabelX = -1e9;
  rows.forEach(function (r, i) {
    var cx = padL + slotW * (i + 0.5);
    [['up', 'bar-up', -1], ['down', 'bar-down', 1]].forEach(function (p) {
      var val = r[p[0]], h = maxV > 0 ? Math.max(val > 0 ? 1.5 : 0, ih * (val / maxV)) : 0;
      if (h <= 0) return;
      svg.appendChild(svgEl('rect', { class: p[1], x: cx + (p[2] < 0 ? -barW - 1 : 1), y: padT + ih - h, width: barW, height: h, rx: 2 }));
    });
    var hit = svgEl('rect', { class: 'hit', x: cx - slotW / 2, y: padT, width: slotW, height: ih });
    var html = '<b>' + esc(r.title || r.label) + '</b><br>↑ ' + fmtBytes(r.up) + '<br>↓ ' + fmtBytes(r.down) + '<br>合计 ' + fmtBytes(r.up + r.down);
    hit.addEventListener('pointerenter', function (e) { showTip(e, html); });
    hit.addEventListener('pointermove', function (e) { showTip(e, html); });
    hit.addEventListener('pointerleave', hideTip);
    svg.appendChild(hit);
    var isLast = i === rows.length - 1;
    if (cx - lastLabelX >= minGap || (isLast && cx - lastLabelX >= minGap * 0.7)) {
      lastLabelX = cx;
      var lb = svgEl('text', { class: 'axis', x: cx, y: H - 8, 'text-anchor': 'middle' });
      lb.textContent = r.label; svg.appendChild(lb);
    }
  });
  host.appendChild(svg);
}

/* ---------- 速率面积图 ---------- */
function drawArea(host, points, opts) {
  opts = opts || {}; host.textContent = '';
  if (points.length < 2) { host.innerHTML = '<div class="empty">采集中，稍候即会出现曲线</div>'; return; }
  var W = Math.max(300, host.parentNode.clientWidth || 700), H = opts.height || 200;
  var padL = 54, padR = 12, padT = 12, padB = 24, iw = W - padL - padR, ih = H - padT - padB;
  var maxV = niceMax(points.reduce(function (m, p) { return Math.max(m, p.rx, p.tx); }, 0));
  var t0 = points[0].ts, t1 = points[points.length - 1].ts, span = Math.max(1, t1 - t0);
  var svg = svgEl('svg', { viewBox: '0 0 ' + W + ' ' + H, width: W, height: H, role: 'img' });
  for (var g = 0; g <= 4; g++) {
    var y = padT + ih - (ih * g / 4);
    svg.appendChild(svgEl('line', { class: 'grid' + (g === 0 ? ' grid-0' : ''), x1: padL, y1: y, x2: W - padR, y2: y }));
    var t = svgEl('text', { class: 'axis', x: padL - 8, y: y + 3, 'text-anchor': 'end' });
    t.textContent = fmtRate(maxV * g / 4); svg.appendChild(t);
  }
  [0, 0.5, 1].forEach(function (f) {
    var x = padL + iw * f;
    var lb = svgEl('text', { class: 'axis', x: x, y: H - 6, 'text-anchor': f === 0 ? 'start' : (f === 1 ? 'end' : 'middle') });
    lb.textContent = (opts.fmtX || fmtClock)(t0 + span * f); svg.appendChild(lb);
  });
  function xOf(p) { return padL + iw * ((p.ts - t0) / span); }
  function yOf(v) { return padT + ih - (maxV > 0 ? ih * (v / maxV) : 0); }
  function path(key, color) {
    var d = '';
    points.forEach(function (p, i) { d += (i ? 'L' : 'M') + xOf(p).toFixed(1) + ' ' + yOf(p[key]).toFixed(1); });
    svg.appendChild(svgEl('path', { d: d + 'L' + (padL + iw) + ' ' + (padT + ih) + 'L' + padL + ' ' + (padT + ih) + 'Z', fill: color, opacity: '0.13', stroke: 'none' }));
    svg.appendChild(svgEl('path', { d: d, fill: 'none', stroke: color, 'stroke-width': 1.8, 'stroke-linejoin': 'round', 'stroke-linecap': 'round' }));
  }
  path('tx', 'var(--down)'); path('rx', 'var(--up)');
  // 悬浮游标
  var cur = svgEl('line', { class: 'cursor-line', x1: 0, y1: padT, x2: 0, y2: padT + ih, opacity: 0 });
  svg.appendChild(cur);
  var overlay = svgEl('rect', { class: 'hit', x: padL, y: padT, width: iw, height: ih });
  overlay.addEventListener('pointermove', function (e) {
    var rect = svg.getBoundingClientRect();
    var rel = (e.clientX - rect.left) / rect.width * W;
    var frac = Math.min(1, Math.max(0, (rel - padL) / iw));
    var idx = Math.round(frac * (points.length - 1));
    var p = points[idx]; if (!p) return;
    cur.setAttribute('x1', xOf(p)); cur.setAttribute('x2', xOf(p)); cur.setAttribute('opacity', 1);
    showTip(e, (opts.fmtX || fmtClock)(p.ts) + '<br>↑ ' + fmtRate(p.rx) + '<br>↓ ' + fmtRate(p.tx));
  });
  overlay.addEventListener('pointerleave', function () { cur.setAttribute('opacity', 0); hideTip(); });
  svg.appendChild(overlay);
  host.appendChild(svg);
}

/* ---------- 渲染：概览（低频 summary）---------- */
function renderSummary(s) {
  state.summary = s;
  setText('meta-backend', (s.backend === 'nft' ? 'nftables' : s.backend) + ' · ' + s.interval + 's');
  setText('meta-clock', s.day + ' ' + (s.tz || ''));
  easeTo('kpi-today-total', s.today.rx + s.today.tx, fmtBytes);
  easeTo('kpi-today-up', s.today.rx, fmtBytes);
  easeTo('kpi-today-down', s.today.tx, fmtBytes);
  easeTo('kpi-all-total', s.total.rx + s.total.tx, fmtBytes);
  easeTo('kpi-all-up', s.total.rx, fmtBytes);
  easeTo('kpi-all-down', s.total.tx, fmtBytes);
  setText('kpi-nodes', s.nodes.length);
  setText('foot-note', '数据来源：内核 ' + (s.backend === 'nft' ? 'nftables' : 'iptables')
    + ' 计数器（精确字节数）· 统计时区 ' + (s.tz || 'local') + (s.error ? ' · 异常：' + s.error : ''));
  if (s.error) toast(s.error);
  renderNodesStatic(s);
  renderNodeSelect(s);
}

/* ---------- 渲染：实时（高频 live）---------- */
function renderLive(v) {
  state.live = v;
  var healthy = v.healthy, live = v.rate_known !== false;
  var dot = document.getElementById('health-dot');
  dot.className = 'dot ' + (healthy ? 'ok' : 'bad');
  setText('status-txt', healthy ? '实时监控中' : (v.error ? '采集异常' : '等待采集'));
  document.getElementById('pulse').className = 'hero-pulse' + (live ? '' : ' stale');

  var rt = v.rate_total || { rx: 0, tx: 0 };
  easeTo('hero-rate', live ? rt.rx + rt.tx : 0, function (n) { return live ? fmtRate(n) : '—'; });
  easeTo('hero-up', live ? rt.rx : 0, function (n) { return live ? fmtRate(n) : '—'; });
  easeTo('hero-down', live ? rt.tx : 0, function (n) { return live ? fmtRate(n) : '—'; });
  setText('kpi-conns', typeof v.conns_total === 'number' ? v.conns_total : '—');
  setText('kpi-conns-udp', typeof v.conns_udp_total === 'number' ? v.conns_udp_total : '—');
  if (live) sparkPush(rt.rx, rt.tx);

  // 把 live 的速率/连接数合并进节点行（增量更新，不重建）
  var byId = {};
  (v.nodes || []).forEach(function (n) { byId[n.id] = n; });
  document.querySelectorAll('[data-node-live]').forEach(function (el) {
    var id = el.getAttribute('data-node-live'), n = byId[id];
    if (!n) return;
    var kind = el.getAttribute('data-kind');
    if (kind === 'conns') el.textContent = (typeof n.conns_tcp === 'number') ? n.conns_tcp : '—';
    else if (kind === 'conns_udp') el.textContent = (typeof n.conns_udp === 'number') ? n.conns_udp : '—';
    else if (kind === 'rate') el.innerHTML = live ? '↑' + fmtRate(n.rate.rx) + '<br>↓' + fmtRate(n.rate.tx) : '—';
    else if (kind === 'rate1') el.textContent = live ? ('↑' + fmtRate(n.rate.rx) + '  ↓' + fmtRate(n.rate.tx)) : '—';
  });
}

/* ---------- 节点表：静态部分（流量/占比，随 summary 重建）---------- */
function renderNodesStatic(s) {
  var tbody = document.getElementById('node-tbody'), cards = document.getElementById('node-cards');
  var key = state.sortScope;
  var nodes = s.nodes.slice().sort(function (a, b) { return (b[key].rx + b[key].tx) - (a[key].rx + a[key].tx); });
  var maxSum = nodes.reduce(function (m, n) { return Math.max(m, n[key].rx + n[key].tx); }, 0) || 1;
  var grandSum = nodes.reduce(function (t, n) { return t + n[key].rx + n[key].tx; }, 0) || 1;
  if (!nodes.length) {
    tbody.innerHTML = '<tr><td colspan="13" class="empty">还没有节点，先用 sbx 菜单添加一个</td></tr>';
    cards.innerHTML = '<div class="empty">还没有节点，先用 sbx 菜单添加一个</div>';
    return;
  }
  function portText(n) { return n.ports ? (n.port ? n.port + ',' + n.ports : n.ports) : (n.port || '—'); }
  // 占比条：该节点用量 ÷ 所有节点合计（纯分布对比，与流量限额无关）
  // 无流量时不画条；只有一个节点时百分比恒为 100% 没有信息量，改标"唯一节点"
  function shareHTML(n, inCard) {
    var sum = n[key].rx + n[key].tx;
    if (sum <= 0) return '<span class="pct">—</span>';
    var w = (sum / maxSum * 100).toFixed(1);
    var pct = nodes.length === 1 ? '唯一节点' : (sum / grandSum * 100).toFixed(1) + '%';
    var title = nodes.length === 1 ? '当前只有一个节点，全部流量都来自它'
      : ('占所有节点' + (key === 'today' ? '今日' : '累计') + '合计的 ' + (sum / grandSum * 100).toFixed(1) + '%');
    return '<div class="bar"' + (inCard ? ' style="flex:1"' : '') + ' title="' + title + '"><i style="width:' + w + '%"></i></div>'
      + '<span class="pct" title="' + title + '">' + pct + '</span>';
  }

  tbody.innerHTML = nodes.map(function (n) {
    var td = n.today, tt = n.total, sum = n[key].rx + n[key].tx;
    return '<tr>'
      + '<td><div class="node-name">' + esc(n.name) + '</div></td>'
      + '<td><span class="chip">' + esc(n.type || '—') + '</span></td>'
      + '<td class="num">' + esc(portText(n)) + '</td>'
      + '<td class="num"><b class="conns" data-node-live="' + n.id + '" data-kind="conns">—</b></td>'
      + '<td class="num"><b class="conns-udp" data-node-live="' + n.id + '" data-kind="conns_udp">—</b></td>'
      + '<td class="num live" data-node-live="' + n.id + '" data-kind="rate">—</td>'
      + '<td class="num up">' + fmtBytes(td.rx) + '</td>'
      + '<td class="num down">' + fmtBytes(td.tx) + '</td>'
      + '<td class="num"><b>' + fmtBytes(td.rx + td.tx) + '</b></td>'
      + '<td class="num up">' + fmtBytes(tt.rx) + '</td>'
      + '<td class="num down">' + fmtBytes(tt.tx) + '</td>'
      + '<td class="num"><b>' + fmtBytes(tt.rx + tt.tx) + '</b></td>'
      + '<td>' + shareHTML(n, false) + '</td>'
      + '</tr>';
  }).join('');

  cards.innerHTML = nodes.map(function (n) {
    var td = n.today, tt = n.total, sum = n[key].rx + n[key].tx;
    return '<div class="ncard">'
      + '<div class="ncard-top"><span class="ncard-name">' + esc(n.name) + '</span>'
      + '<span class="chip">' + esc(n.type || '—') + '</span></div>'
      + '<div class="ncard-meta"><span>端口 ' + esc(portText(n)) + '</span>'
      + '<span>TCP <b class="conns" data-node-live="' + n.id + '" data-kind="conns">—</b></span>'
      + '<span>UDP <b class="conns-udp" data-node-live="' + n.id + '" data-kind="conns_udp">—</b></span></div>'
      + '<div class="ncard-live"><span class="live" data-node-live="' + n.id + '" data-kind="rate1">—</span></div>'
      + '<div class="ncard-grid">'
      + '<div><div class="ncell-label">今日合计</div><div class="ncell-value">' + fmtBytes(td.rx + td.tx) + '</div>'
      + '<div class="ncell-sub"><span class="up">↑' + fmtBytes(td.rx) + '</span> <span class="down">↓' + fmtBytes(td.tx) + '</span></div></div>'
      + '<div><div class="ncell-label">累计合计</div><div class="ncell-value">' + fmtBytes(tt.rx + tt.tx) + '</div>'
      + '<div class="ncell-sub"><span class="up">↑' + fmtBytes(tt.rx) + '</span> <span class="down">↓' + fmtBytes(tt.tx) + '</span></div></div>'
      + '</div>'
      + (nodes.length > 1 || sum > 0
          ? '<div class="ncard-bar">' + shareHTML(n, true) + '</div>' : '')
      + '</div>';
  }).join('');

  if (state.live) renderLive(state.live);   // 立即补上实时列
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

/* ---------- 数据加载 ---------- */
var cache = { daily: null, series: null, nodeDaily: null };

function loadSummary() { return api('/api/summary').then(renderSummary).catch(function (e) { toast(e.message); }); }
function loadLive() { return api('/api/live').then(renderLive).catch(function () {}); }

function drawDaily() {
  if (!cache.daily) return;
  drawGrouped(document.getElementById('chart-daily'), cache.daily.map(function (r) {
    return { label: fmtDay(r.day), title: r.day, up: r.rx, down: r.tx };
  }), { label: '每日流量', height: 230 });
}
function drawSeries() {
  if (!cache.series) return;
  var d = cache.series, bucket = d.bucket || ((state.summary && state.summary.interval) || 5);
  var fmtX = (state.range === '7d' || state.range === '30d') ? fmtDate : (state.range === '1d' ? fmtDateTime : fmtClock);
  drawArea(document.getElementById('chart-live'), d.points.map(function (p) {
    return { ts: p.ts, rx: p.rx / bucket, tx: p.tx / bucket };
  }), { label: '速率曲线', fmtX: fmtX });
}
function drawNodeDaily() {
  if (!cache.nodeDaily) return;
  drawGrouped(document.getElementById('chart-node'), cache.nodeDaily.map(function (r) {
    return { label: fmtDay(r.day), title: r.day, up: r.rx, down: r.tx };
  }), { label: '单节点每日流量', height: 200 });
}
function loadDaily() { return api('/api/daily', { days: state.days }).then(function (d) { cache.daily = d.days; drawDaily(); }).catch(function (e) { toast(e.message); }); }
function loadSeries() {
  return api('/api/series', { range: state.range }).then(function (d) {
    cache.series = d; drawSeries();
    var n = (d.points || []).length;
    setText('live-hint', RANGE_LABEL[state.range] + ' · '
      + (state.range === '30m' || state.range === '2h' ? '每点 ' + (d.bucket || 5) + ' 秒'
        : (d.bucket >= 86400 ? '每点 1 天' : '每点 1 小时')) + '（' + n + ' 点）');
  }).catch(function (e) { toast(e.message); });
}
function loadNodeDaily() {
  if (state.nodeId == null) return Promise.resolve();
  return api('/api/daily', { days: state.days, scope: 'node:' + state.nodeId })
    .then(function (d) { cache.nodeDaily = d.days; drawNodeDaily(); }).catch(function (e) { toast(e.message); });
}

/* ---------- 事件 ---------- */
function bindSeg(attr, cb) {
  document.querySelectorAll('.seg [data-' + attr + ']').forEach(function (b) {
    b.addEventListener('click', function () {
      var group = b.parentNode;
      group.querySelectorAll('[data-' + attr + ']').forEach(function (x) { x.classList.remove('on'); });
      b.classList.add('on'); cb(b.dataset[attr]);
    });
  });
}
bindSeg('days', function (v) { state.days = Number(v); loadDaily(); loadNodeDaily(); });
bindSeg('scope', function (v) { state.sortScope = v; if (state.summary) renderNodesStatic(state.summary); });
bindSeg('range', function (v) { state.range = v; loadSeries(); });
document.getElementById('node-select').addEventListener('change', function (e) { state.nodeId = e.target.value; loadNodeDaily(); });
if (TOKEN) document.getElementById('csv-link').href = '/api/export?token=' + encodeURIComponent(TOKEN);

var reflowTimer;
window.addEventListener('resize', function () {
  clearTimeout(reflowTimer);
  reflowTimer = setTimeout(function () { drawDaily(); drawSeries(); drawNodeDaily(); drawSpark(); }, 160);
});

/* ---------- 启动与轮询 ---------- */
setInterval(tickEase, 40);               // 数字缓动循环（setInterval 比 rAF 在后台更可靠）
setInterval(sparkStep, 250);             // hero 迷你曲线每 250ms 平滑推进一帧

loadSummary().then(function () { loadLive(); loadSeries(); loadDaily(); });

// 实时数据：2 秒一次（轻量 /api/live）
setInterval(function () { if (!document.hidden) loadLive(); }, 2000);
// 概览+节点表：10 秒一次（较重的 /api/summary）
setInterval(function () { if (!document.hidden) loadSummary(); }, 10000);
// 短范围速率曲线：10 秒刷新；长范围随每日刷新
setInterval(function () {
  if (document.hidden) return;
  if (state.range === '30m' || state.range === '2h') loadSeries();
}, 10000);
// 每日图 + 长范围曲线：60 秒
setInterval(function () {
  if (document.hidden) return;
  loadDaily(); loadNodeDaily();
  if (state.range === '1d' || state.range === '7d' || state.range === '30d') loadSeries();
}, 60000);
// 页面重新可见时立即刷新一次
document.addEventListener('visibilitychange', function () {
  if (!document.hidden) { loadLive(); loadSummary(); }
});
