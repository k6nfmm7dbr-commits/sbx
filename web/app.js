/* sbx-panel 前端：纯原生 JS + 手绘 SVG，无外部依赖 */
'use strict';

var TOKEN = new URLSearchParams(location.search).get('token') || '';
var state = { days: 30, sortScope: 'today', nodeId: null, summary: null };

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
function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
    return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
  });
}
function toast(msg) {
  var el = document.getElementById('toast');
  el.textContent = msg;
  el.classList.add('show');
  clearTimeout(toast._t);
  toast._t = setTimeout(function () { el.classList.remove('show'); }, 4000);
}

/* ---------- SVG 工具 ---------- */
var NS = 'http://www.w3.org/2000/svg';
function svgEl(name, attrs) {
  var e = document.createElementNS(NS, name);
  if (attrs) Object.keys(attrs).forEach(function (k) { e.setAttribute(k, attrs[k]); });
  return e;
}
var tip = null;
function showTip(evt, html) {
  if (!tip) { tip = document.createElement('div'); tip.className = 'tip'; document.body.appendChild(tip); }
  tip.innerHTML = html;
  tip.classList.add('show');
  var pad = 12, w = tip.offsetWidth, h = tip.offsetHeight;
  var x = Math.min(Math.max(evt.clientX - w / 2, 8), window.innerWidth - w - 8);
  var y = evt.clientY - h - pad;
  if (y < 8) y = evt.clientY + pad;
  tip.style.left = x + 'px';
  tip.style.top = y + 'px';
}
function hideTip() { if (tip) tip.classList.remove('show'); }

function niceMax(v) {
  if (v <= 0) return 1;
  var exp = Math.floor(Math.log(v) / Math.LN10);
  var base = Math.pow(10, exp);
  var f = v / base;
  var step = f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10;
  return step * base;
}

/**
 * 分组柱状图（上传/下载并列）
 * rows: [{label, up, down, title}]
 */
function drawGrouped(host, rows, opts) {
  opts = opts || {};
  host.textContent = '';
  if (!rows.length) {
    host.innerHTML = '<div class="empty">暂无数据</div>';
    return;
  }
  var avail = Math.max(280, host.parentNode.clientWidth || host.clientWidth || 600);
  var slot = opts.slot || 26;
  var padL = 56, padR = 12, padT = 12, padB = 26;
  var W = Math.max(avail, rows.length * slot + padL + padR);
  var H = opts.height || 220;
  var iw = W - padL - padR, ih = H - padT - padB;
  var maxV = niceMax(rows.reduce(function (m, r) { return Math.max(m, r.up, r.down); }, 0));
  var svg = svgEl('svg', {
    viewBox: '0 0 ' + W + ' ' + H, width: W, height: H, role: 'img'
  });
  svg.setAttribute('aria-label', opts.label || '柱状图');

  for (var g = 0; g <= 4; g++) {
    var y = padT + ih - (ih * g / 4);
    svg.appendChild(svgEl('line', { class: 'grid', x1: padL, y1: y, x2: W - padR, y2: y }));
    var t = svgEl('text', { class: 'axis', x: padL - 8, y: y + 3, 'text-anchor': 'end' });
    t.textContent = fmtBytes(maxV * g / 4);
    svg.appendChild(t);
  }

  var slotW = iw / rows.length;
  var barW = Math.max(2, Math.min(11, slotW / 2 - 2));
  var minGap = 46, lastLabelX = -1e9;

  rows.forEach(function (r, i) {
    var cx = padL + slotW * (i + 0.5);
    [['up', r.up, 'bar-up', -1], ['down', r.down, 'bar-down', 1]].forEach(function (p) {
      var h = maxV > 0 ? Math.max(r[p[0]] > 0 ? 1.5 : 0, ih * (p[1] / maxV)) : 0;
      if (h <= 0) return;
      svg.appendChild(svgEl('rect', {
        class: p[2], x: cx + (p[3] < 0 ? -barW - 1 : 1), y: padT + ih - h,
        width: barW, height: h, rx: 2
      }));
    });
    var hit = svgEl('rect', { class: 'hit', x: cx - slotW / 2, y: padT, width: slotW, height: ih });
    var html = '<b>' + esc(r.title || r.label) + '</b><br>'
      + '↑ ' + fmtBytes(r.up) + '<br>↓ ' + fmtBytes(r.down)
      + '<br>合计 ' + fmtBytes(r.up + r.down);
    hit.addEventListener('pointerenter', function (e) { showTip(e, html); });
    hit.addEventListener('pointermove', function (e) { showTip(e, html); });
    hit.addEventListener('pointerleave', hideTip);
    svg.appendChild(hit);

    var isLast = i === rows.length - 1;
    if (cx - lastLabelX >= minGap || (isLast && cx - lastLabelX >= minGap * 0.7)) {
      lastLabelX = cx;
      var lb = svgEl('text', { class: 'axis', x: cx, y: H - 8, 'text-anchor': 'middle' });
      lb.textContent = r.label;
      svg.appendChild(lb);
    }
  });
  host.appendChild(svg);
}

/**
 * 速率面积图（上传/下载双线）
 * points: [{ts, rx, tx}]  —— rx=上传, tx=下载, 单位 bytes/s
 */
function drawArea(host, points, opts) {
  opts = opts || {};
  host.textContent = '';
  if (points.length < 2) {
    host.innerHTML = '<div class="empty">采集中，稍候即会出现曲线</div>';
    return;
  }
  var W = Math.max(300, host.parentNode.clientWidth || host.clientWidth || 700);
  var H = opts.height || 190;
  var padL = 56, padR = 12, padT = 12, padB = 24;
  var iw = W - padL - padR, ih = H - padT - padB;
  var maxV = niceMax(points.reduce(function (m, p) { return Math.max(m, p.rx, p.tx); }, 0));
  var t0 = points[0].ts, t1 = points[points.length - 1].ts;
  var span = Math.max(1, t1 - t0);
  var svg = svgEl('svg', { viewBox: '0 0 ' + W + ' ' + H, width: W, height: H, role: 'img' });
  svg.setAttribute('aria-label', opts.label || '速率曲线');

  for (var g = 0; g <= 4; g++) {
    var y = padT + ih - (ih * g / 4);
    svg.appendChild(svgEl('line', { class: 'grid', x1: padL, y1: y, x2: W - padR, y2: y }));
    var t = svgEl('text', { class: 'axis', x: padL - 8, y: y + 3, 'text-anchor': 'end' });
    t.textContent = fmtRate(maxV * g / 4);
    svg.appendChild(t);
  }
  [0, 0.5, 1].forEach(function (f) {
    var ts = t0 + span * f;
    var x = padL + iw * f;
    var lb = svgEl('text', {
      class: 'axis', x: x, y: H - 6,
      'text-anchor': f === 0 ? 'start' : (f === 1 ? 'end' : 'middle')
    });
    lb.textContent = new Date(ts * 1000).toTimeString().slice(0, 5);
    svg.appendChild(lb);
  });

  function path(key, cls, fill) {
    var d = '';
    points.forEach(function (p, i) {
      var x = padL + iw * ((p.ts - t0) / span);
      var y = padT + ih - (maxV > 0 ? ih * (p[key] / maxV) : 0);
      d += (i ? 'L' : 'M') + x.toFixed(1) + ' ' + y.toFixed(1);
    });
    var color = cls === 'up' ? 'var(--up)' : 'var(--down)';
    if (fill) {
      var area = d + 'L' + (padL + iw) + ' ' + (padT + ih) + 'L' + padL + ' ' + (padT + ih) + 'Z';
      svg.appendChild(svgEl('path', { d: area, fill: color, opacity: '0.13', stroke: 'none' }));
    }
    svg.appendChild(svgEl('path', {
      d: d, fill: 'none', stroke: color, 'stroke-width': 1.8,
      'stroke-linejoin': 'round', 'stroke-linecap': 'round'
    }));
  }
  path('tx', 'down', true);
  path('rx', 'up', true);
  host.appendChild(svg);
}

/* ---------- 渲染 ---------- */
function renderSummary(s) {
  state.summary = s;
  var dot = document.getElementById('health-dot');
  dot.className = 'dot ' + (s.healthy ? 'ok' : 'bad');
  dot.title = s.healthy ? '采集正常' : (s.error || '采集异常');

  document.getElementById('meta-backend').textContent =
    (s.backend === 'nft' ? 'nftables' : s.backend) + ' · ' + s.interval + 's';
  document.getElementById('meta-clock').textContent = s.day + ' ' + (s.tz || '');

  var td = s.today, tt = s.total, rt = s.rate_total;
  var live = s.rate_known !== false;
  document.getElementById('kpi-today-total').textContent = fmtBytes(td.rx + td.tx);
  document.getElementById('kpi-today-up').textContent = fmtBytes(td.rx);
  document.getElementById('kpi-today-down').textContent = fmtBytes(td.tx);
  document.getElementById('kpi-all-total').textContent = fmtBytes(tt.rx + tt.tx);
  document.getElementById('kpi-all-up').textContent = fmtBytes(tt.rx);
  document.getElementById('kpi-all-down').textContent = fmtBytes(tt.tx);
  document.getElementById('kpi-rate').textContent = live ? fmtRate(rt.rx + rt.tx) : '—';
  document.getElementById('kpi-rate-up').textContent = live ? fmtRate(rt.rx) : '—';
  document.getElementById('kpi-rate-down').textContent = live ? fmtRate(rt.tx) : '—';
  document.getElementById('kpi-nodes').textContent = s.nodes.length;
  document.getElementById('kpi-day').textContent = '统计日期 ' + s.day;

  var lag = s.last_sample ? Math.max(0, s.now - s.last_sample) : -1;
  document.getElementById('live-hint').textContent =
    lag < 0 ? '尚未采集' : ('最后采样 ' + lag + ' 秒前');
  document.getElementById('foot-note').textContent =
    '数据来源：内核 ' + (s.backend === 'nft' ? 'nftables' : 'iptables')
    + ' 计数器（精确字节数）· 统计时区 ' + (s.tz || 'local')
    + (s.error ? ' · 异常：' + s.error : '');
  if (s.error) toast(s.error);

  renderNodes(s);
  renderNodeSelect(s);
}

function renderNodes(s) {
  var tbody = document.getElementById('node-tbody');
  var cards = document.getElementById('node-cards');
  var key = state.sortScope;
  var live = s.rate_known !== false;
  var nodes = s.nodes.slice().sort(function (a, b) {
    return (b[key].rx + b[key].tx) - (a[key].rx + a[key].tx);
  });
  var maxSum = nodes.reduce(function (m, n) {
    return Math.max(m, n[key].rx + n[key].tx);
  }, 0) || 1;
  var grandSum = nodes.reduce(function (t, n) { return t + n[key].rx + n[key].tx; }, 0) || 1;

  if (!nodes.length) {
    tbody.innerHTML = '<tr><td colspan="11" class="empty">还没有节点，先用 sbx 菜单添加一个</td></tr>';
    cards.innerHTML = '<div class="empty">还没有节点，先用 sbx 菜单添加一个</div>';
    return;
  }

  function portText(n) {
    return n.ports ? (n.port ? n.port + ',' + n.ports : n.ports) : (n.port || '—');
  }

  tbody.innerHTML = nodes.map(function (n) {
    var td = n.today, tt = n.total, rate = n.rate || { rx: 0, tx: 0 };
    var sum = n[key].rx + n[key].tx;
    return '<tr>'
      + '<td><div class="node-name">' + esc(n.name) + '</div></td>'
      + '<td><span class="chip">' + esc(n.type || '—') + '</span></td>'
      + '<td class="num">' + esc(portText(n)) + '</td>'
      + '<td class="num up">' + fmtBytes(td.rx) + '</td>'
      + '<td class="num down">' + fmtBytes(td.tx) + '</td>'
      + '<td class="num"><b>' + fmtBytes(td.rx + td.tx) + '</b></td>'
      + '<td class="num up">' + fmtBytes(tt.rx) + '</td>'
      + '<td class="num down">' + fmtBytes(tt.tx) + '</td>'
      + '<td class="num"><b>' + fmtBytes(tt.rx + tt.tx) + '</b></td>'
      + '<td class="num live">' + (live ? '↑' + fmtRate(rate.rx) + '<br>↓' + fmtRate(rate.tx) : '—') + '</td>'
      + '<td><div class="bar"><i style="width:' + (sum / maxSum * 100).toFixed(1) + '%"></i></div>'
      + '<span class="pct">' + (sum / grandSum * 100).toFixed(1) + '%</span></td>'
      + '</tr>';
  }).join('');

  cards.innerHTML = nodes.map(function (n) {
    var td = n.today, tt = n.total, rate = n.rate || { rx: 0, tx: 0 };
    var sum = n[key].rx + n[key].tx;
    return '<div class="ncard">'
      + '<div class="ncard-top"><span class="ncard-name">' + esc(n.name) + '</span>'
      + '<span class="chip">' + esc(n.type || '—') + '</span></div>'
      + '<div class="ncard-port">端口 ' + esc(portText(n)) + '</div>'
      + '<div class="ncard-grid" style="margin-top:9px">'
      + '<div><div class="ncell-label">今日合计</div>'
      + '<div class="ncell-value">' + fmtBytes(td.rx + td.tx) + '</div>'
      + '<div class="ncell-sub"><span class="up">↑' + fmtBytes(td.rx) + '</span> '
      + '<span class="down">↓' + fmtBytes(td.tx) + '</span></div></div>'
      + '<div><div class="ncell-label">累计合计</div>'
      + '<div class="ncell-value">' + fmtBytes(tt.rx + tt.tx) + '</div>'
      + '<div class="ncell-sub"><span class="up">↑' + fmtBytes(tt.rx) + '</span> '
      + '<span class="down">↓' + fmtBytes(tt.tx) + '</span></div></div>'
      + '</div>'
      + '<div class="ncard-bar"><div class="bar" style="flex:1">'
      + '<i style="width:' + (sum / maxSum * 100).toFixed(1) + '%"></i></div>'
      + '<span class="pct">' + (sum / grandSum * 100).toFixed(1) + '%</span></div>'
      + '<div class="ncell-sub" style="margin-top:6px">实时 '
      + (live ? '↑' + fmtRate(rate.rx) + ' ↓' + fmtRate(rate.tx) : '—') + '</div>'
      + '</div>';
  }).join('');
}

function renderNodeSelect(s) {
  var sel = document.getElementById('node-select');
  var want = state.nodeId != null ? String(state.nodeId)
    : (s.nodes.length ? String(s.nodes[0].id) : '');
  var sig = s.nodes.map(function (n) { return n.id + ':' + n.name; }).join('|');
  if (sel._sig !== sig) {
    sel._sig = sig;
    sel.innerHTML = s.nodes.map(function (n) {
      return '<option value="' + esc(n.id) + '">' + esc(n.name) + '</option>';
    }).join('');
  }
  if (want && sel.value !== want) sel.value = want;
  if (state.nodeId == null && want) { state.nodeId = want; loadNodeDaily(); }
}

/* ---------- 数据加载 ---------- */
var cache = { daily: null, series: null, nodeDaily: null };

function loadSummary() {
  return api('/api/summary').then(renderSummary).catch(function (e) { toast(e.message); });
}
function drawDaily() {
  if (!cache.daily) return;
  drawGrouped(document.getElementById('chart-daily'), cache.daily.map(function (r) {
    return { label: fmtDay(r.day), title: r.day, up: r.rx, down: r.tx };
  }), { label: '每日流量柱状图', height: 230 });
}
function drawSeries() {
  if (!cache.series) return;
  var iv = (state.summary && state.summary.interval) || 5;
  drawArea(document.getElementById('chart-live'), cache.series.map(function (p) {
    return { ts: p.ts, rx: p.rx / iv, tx: p.tx / iv };
  }), { label: '实时速率曲线' });
}
function drawNodeDaily() {
  if (!cache.nodeDaily) return;
  drawGrouped(document.getElementById('chart-node'), cache.nodeDaily.map(function (r) {
    return { label: fmtDay(r.day), title: r.day, up: r.rx, down: r.tx };
  }), { label: '单节点每日流量', height: 200 });
}
function loadDaily() {
  return api('/api/daily', { days: state.days }).then(function (d) {
    cache.daily = d.days;
    drawDaily();
  }).catch(function (e) { toast(e.message); });
}
function loadSeries() {
  return api('/api/series', { minutes: 30 }).then(function (d) {
    cache.series = d.series;
    drawSeries();
  }).catch(function (e) { toast(e.message); });
}
function loadNodeDaily() {
  if (state.nodeId == null) return Promise.resolve();
  return api('/api/daily', { days: state.days, scope: 'node:' + state.nodeId })
    .then(function (d) {
      cache.nodeDaily = d.days;
      drawNodeDaily();
    }).catch(function (e) { toast(e.message); });
}

/* ---------- 事件 ---------- */
document.querySelectorAll('.seg [data-days]').forEach(function (b) {
  b.addEventListener('click', function () {
    document.querySelectorAll('.seg [data-days]').forEach(function (x) { x.classList.remove('on'); });
    b.classList.add('on');
    state.days = Number(b.dataset.days);
    loadDaily(); loadNodeDaily();
  });
});
document.querySelectorAll('.seg [data-scope]').forEach(function (b) {
  b.addEventListener('click', function () {
    document.querySelectorAll('.seg [data-scope]').forEach(function (x) { x.classList.remove('on'); });
    b.classList.add('on');
    state.sortScope = b.dataset.scope;
    if (state.summary) renderNodes(state.summary);
  });
});
document.getElementById('node-select').addEventListener('change', function (e) {
  state.nodeId = e.target.value;
  loadNodeDaily();
});
if (TOKEN) document.getElementById('csv-link').href = '/api/export?token=' + encodeURIComponent(TOKEN);

/* ---------- 启动 ---------- */
var reflowTimer;
window.addEventListener('resize', function () {
  clearTimeout(reflowTimer);
  reflowTimer = setTimeout(function () { drawDaily(); drawSeries(); drawNodeDaily(); }, 180);
});

loadSummary().then(function () { loadDaily(); loadSeries(); });
setInterval(function () {
  if (document.hidden) return;
  loadSummary(); loadSeries();
}, 5000);
setInterval(function () {
  if (document.hidden) return;
  loadDaily(); loadNodeDaily();
}, 60000);
