/* SBX 流量面板 —— 前端逻辑（无外部依赖） */
'use strict';

var state = { days: 180, nodeId: null, summary: null, live: null };
var activeView = 'home';

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
var lastPolicyErr = '';
function renderSummary(s) {
  state.summary = s;
  easeTo('kpi-today-total', s.today.rx + s.today.tx, fmtBytes);
  easeTo('kpi-all-total', s.total.rx + s.total.tx, fmtBytes);
  setText('kpi-nodes', s.nodes.length);
  if (activeView === 'home') {
    renderNodeCards(s);
    if (state.live) renderLive(state.live);
  }
  renderNodeSelect(s);
  if (s.error) toast(s.error);
  // 策略 enforcement 错误（如 nft 规则应用失败）：变化时提醒一次，
  // 不每 8s 重复弹——它是稳态条件而非瞬时事件。
  var pe = s.policy_error || '';
  if (pe && pe !== lastPolicyErr) toast(pe);
  lastPolicyErr = pe;
}

/* ---------- 节点卡片 ---------- */
function portText(n) { return n.port != null ? n.port : '—'; }
function rateText(n) {
  return n.rate_limit_enabled && n.rate_limit_mbps > 0 ? (n.rate_limit_mbps + ' Mbps') : '不限';
}
function nodeStatus(n) {
  if (n.paused) return '<span class="status-pill paused">已暂停</span>';
  if (n.ip_limit_state === 'exceeded') return '<span class="status-pill warn">IP 已达上限</span>';
  return '<span class="status-pill ok">正常</span>';
}
function renderNodeCards(s) {
  var host = document.getElementById('node-cards');
  if (!host) return;
  var nodes = s.nodes || [];
  var signature = JSON.stringify(nodes.map(function (n) { return [n.id, n.name, n.type, n.port]; }));
  if (host._nodeStructure !== signature) {
    host._nodeStructure = signature;
    if (!nodes.length) {
      host.innerHTML = '<div class="empty">暂无节点，运行 sbx 菜单添加</div>';
      return;
    }
    host.innerHTML = nodes.map(function (n) {
      var id = esc(n.id);
      return '<div class="node-card' + (n.paused ? ' paused' : '') + '">' +
        '<div class="node-top"><div class="node-title">' +
          '<div class="node-name"><span data-node-name></span><span class="node-paused-tag hidden" data-node-paused>暂停</span></div>' +
          '<div class="node-meta-line"><span class="chip">' + esc(n.type || '—') + '</span><span class="port">端口 ' + esc(portText(n)) + '</span></div>' +
        '</div><div class="node-rate">' +
          '<b class="up" data-node-live="' + id + '" data-kind="rate-up">—</b>' +
          '<b class="down" data-node-live="' + id + '" data-kind="rate-down">—</b>' +
        '</div></div>' +
        '<div class="node-stats">' +
          '<div class="node-stat"><span>累计 / 今日</span><b><span data-node-total></span><span class="sep">/</span><span data-node-today></span></b></div>' +
          '<div class="node-stat"><span>限速</span><b data-node-rate></b></div>' +
          '<div class="node-stat"><span>TCP / UDP</span><b><i data-node-live="' + id + '" data-kind="conns">—</i><span class="sep">/</span><i data-node-live="' + id + '" data-kind="conns_udp">—</i></b></div>' +
        '</div>' +
        '<button class="ip-strip" data-view-ips="' + id + '"><span class="ip-strip-label">在线 IP</span>' +
          '<span class="ip-strip-val" data-node-ips="' + id + '">—</span><span class="ip-strip-arrow">›</span></button>' +
        '<div class="node-foot"><div data-node-status></div><div class="node-actions"><button class="mini-btn primary" data-manage="' + id + '">管理</button></div></div>' +
      '</div>';
    }).join('');
  }
  for (var i = 0; i < nodes.length; i++) {
    var n = nodes[i], card = host.children[i];
    if (!card) continue;
    var total = (n.total && (n.total.rx + n.total.tx)) || 0;
    var today = (n.today && (n.today.rx + n.today.tx)) || 0;
    card.classList.toggle('paused', !!n.paused);
    card.querySelector('[data-node-name]').textContent = n.name || '';
    card.querySelector('[data-node-paused]').classList.toggle('hidden', !n.paused);
    card.querySelector('[data-node-total]').textContent = fmtBytes(total);
    card.querySelector('[data-node-today]').textContent = fmtBytes(today);
    card.querySelector('[data-node-rate]').textContent = rateText(n);
    card.querySelector('[data-node-status]').innerHTML = nodeStatus(n);
    var ipVal = (n.active_ip_count || 0) + (n.ip_limit_enabled ? ' / ' + n.ip_limit_max : '');
    card.querySelector('[data-node-ips]').textContent = ipVal;
  }
}

function renderNodeSelect(s) {
  var sel = document.getElementById('node-select');
  var nodes = s.nodes || [];
  var history = s.history_nodes || [];
  var previous = state.nodeId == null ? '' : String(state.nodeId);
  // 已删除节点的历史同样可选：删除节点不再清空流量记录，那份数据必须看得到，
  // 否则"保留"只是躺在库里而已。
  var exists = nodes.some(function (n) { return String(n.id) === previous; }) ||
               history.some(function (h) { return String(h.id) === previous; });
  var fallback = nodes.length ? String(nodes[0].id) : (history.length ? String(history[0].id) : '');
  var want = exists ? previous : fallback;
  var sig = nodes.map(function (n) { return n.id + ':' + n.name; }).join('|') +
            '#' + history.map(function (h) { return h.id + ':' + h.name; }).join('|');
  if (sel._sig !== sig) {
    sel._sig = sig;
    var html = nodes.map(function (n) { return '<option value="' + esc(n.id) + '">' + esc(n.name) + '</option>'; }).join('');
    if (history.length) {
      html += '<optgroup label="历史节点（已删除）">' + history.map(function (h) {
        return '<option value="' + esc(h.id) + '">' + esc(h.name) + '</option>';
      }).join('') + '</optgroup>';
    }
    sel.innerHTML = html;
  }
  if (want && sel.value !== want) sel.value = want;
  state.nodeId = want || null;
  if (previous !== want && want && activeView === 'node') loadNodeDaily();
}

/* ---------- 渲染：实时（高频 live） ---------- */
function renderLive(v) {
  state.live = v;
  if (activeView !== 'home') return;
  var live = v.rate_known !== false;

  var rt = v.rate_total || { rx: 0, tx: 0 };
  easeTo('hero-rate', live ? rt.rx + rt.tx : 0, function (n) { return live ? fmtRate(n) : '—'; });
  easeTo('hero-up', live ? rt.rx : 0, function (n) { return live ? fmtRate(n) : '—'; });
  easeTo('hero-down', live ? rt.tx : 0, function (n) { return live ? fmtRate(n) : '—'; });
  setText('kpi-conns', typeof v.conns_total === 'number' ? v.conns_total : '—');
  setText('kpi-conns-udp', typeof v.conns_udp_total === 'number' ? v.conns_udp_total : '—');

  var byId = {};
  (v.nodes || []).forEach(function (n) { byId[n.id] = n; });
  var pausedById = {};
  if (state.summary && state.summary.nodes) {
    state.summary.nodes.forEach(function (n) { if (n.paused) pausedById[String(n.id)] = true; });
  }
  document.querySelectorAll('[data-node-live]').forEach(function (el) {
    var id = el.getAttribute('data-node-live'), n = byId[id];
    if (!n) return;
    var kind = el.getAttribute('data-kind');
    var paused = !!n.paused || !!pausedById[String(id)];
    if (kind === 'conns') el.textContent = (typeof n.conns_tcp === 'number') ? n.conns_tcp : '—';
    else if (kind === 'conns_udp') el.textContent = (typeof n.conns_udp === 'number') ? n.conns_udp : '—';
    else if (kind === 'rate-up') el.textContent = live && !paused ? '↑ ' + fmtRate(n.rate.rx) : '—';
    else if (kind === 'rate-down') el.textContent = live && !paused ? '↓ ' + fmtRate(n.rate.tx) : '—';
  });
  // 在线 IP 数（高频刷新：TCP 断开后立即回落）
  document.querySelectorAll('[data-node-ips]').forEach(function (el) {
    var id = el.getAttribute('data-node-ips'), n = byId[id];
    if (!n) return;
    var val = (typeof n.active_ip_count === 'number') ? n.active_ip_count : 0;
    el.textContent = n.ip_limit_enabled ? (val + ' / ' + n.ip_limit_max) : val;
  });
}

/* ---------- 明细表格 ---------- */
var cache = { daily: null, dailyFetchedAt: 0, nodeDaily: Object.create(null) };
var dailyRequest = 0;
var dailyEpoch = 0;
var nodeDailyRequest = 0;
var nodeDailyEpoch = 0;

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

/* ---------- 节点详情：用日历选日期 ---------- */
// 节点有 180 天记录，平铺成表格既长又看不出哪天有流量，所以换成日历：
// 有记录的日期可点，选中后上面的汇总与下面的访问域名一起切到那一天。
var calState = { nodeId: null, day: null, y: 0, m: 0 };

function pad2(n) { return (n < 10 ? '0' : '') + n; }
function isoToday() {
  var d = new Date();
  return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
}

// resetCalendar 在切换到另一个节点时清掉选中日期，避免把上一个节点的
// 日期带过来（那个日期在新节点上可能根本没有记录）。
function resetCalendar(id) {
  if (calState.nodeId === id) return;
  calState.nodeId = id;
  calState.day = null;
  calState.y = 0;
  calState.m = 0;
}

function nodeDayRows() {
  var entry = cache.nodeDaily[String(state.nodeId)];
  var byDay = {};
  ((entry && entry.days) || []).forEach(function (r) { byDay[r.day] = r; });
  return byDay;
}

function drawNodeDaily() {
  var host = document.getElementById('node-cal');
  var sum = document.getElementById('node-day-summary');
  if (!host) return;
  if (state.nodeId == null) {
    host.innerHTML = '';
    if (sum) sum.innerHTML = '';
    return;
  }
  var entry = cache.nodeDaily[String(state.nodeId)];
  if (!entry) { host.innerHTML = '<div class="empty">正在加载…</div>'; return; }

  resetCalendar(String(state.nodeId));
  var byDay = nodeDayRows();
  var days = entry.days || [];
  var today = isoToday();
  // 默认选今天；今天没有记录就退到最近有记录的一天。
  if (!calState.day) {
    calState.day = byDay[today] ? today : (days.length ? days[days.length - 1].day : today);
  }
  var sel = calState.day;
  if (!calState.y) {
    var parts = sel.split('-');
    calState.y = Number(parts[0]);
    calState.m = Number(parts[1]) - 1;
  }
  var y = calState.y, m = calState.m;
  var startDow = new Date(y, m, 1).getDay();
  var dayCount = new Date(y, m + 1, 0).getDate();

  var cells = '';
  for (var i = 0; i < startDow; i++) cells += '<span class="cal-cell pad"></span>';
  for (var d = 1; d <= dayCount; d++) {
    var iso = y + '-' + pad2(m + 1) + '-' + pad2(d);
    var row = byDay[iso];
    var cls = 'cal-cell' + (row ? ' has' : '') + (iso === sel ? ' sel' : '') + (iso === today ? ' today' : '');
    cells += '<button type="button" class="' + cls + '" data-day="' + iso + '"' +
      (row ? '' : ' disabled') + '><b>' + d + '</b>' + (row ? '<i class="dot"></i>' : '') + '</button>';
  }

  host.innerHTML =
    '<div class="cal-head">' +
      '<button type="button" class="cal-nav" data-cal="-1" aria-label="上个月">‹</button>' +
      '<span class="cal-title">' + y + ' 年 ' + (m + 1) + ' 月</span>' +
      '<button type="button" class="cal-nav" data-cal="1" aria-label="下个月">›</button>' +
    '</div>' +
    '<div class="cal-week"><span>日</span><span>一</span><span>二</span><span>三</span>' +
      '<span>四</span><span>五</span><span>六</span></div>' +
    '<div class="cal-grid">' + cells + '</div>';

  var cur = byDay[sel];
  if (sum) {
    sum.innerHTML = '<span class="ds-date">' + esc(sel) + (sel === today ? '（今天）' : '') + '</span>' +
      (cur
        ? '<span class="ds-item"><small>上传</small><b class="up">' + fmtBytes(cur.rx) + '</b></span>' +
          '<span class="ds-item"><small>下载</small><b class="down">' + fmtBytes(cur.tx) + '</b></span>' +
          '<span class="ds-item"><small>合计</small><b>' + fmtBytes(cur.rx + cur.tx) + '</b></span>'
        : '<span class="ds-none">该日没有流量记录</span>');
  }
}

function bindCalendar() {
  var host = document.getElementById('node-cal');
  if (!host || host.dataset.bound) return;
  host.dataset.bound = '1';
  host.addEventListener('click', function (e) {
    var nav = e.target && e.target.closest ? e.target.closest('[data-cal]') : null;
    if (nav) {
      var d = new Date(calState.y, calState.m + Number(nav.getAttribute('data-cal')), 1);
      calState.y = d.getFullYear();
      calState.m = d.getMonth();
      drawNodeDaily();
      return;
    }
    var cell = e.target && e.target.closest ? e.target.closest('[data-day]') : null;
    if (!cell || cell.disabled) return;
    var day = cell.getAttribute('data-day');
    if (day === calState.day) return;
    calState.day = day;
    drawNodeDaily();
    loadNodeDomains(true);  // 域名跟着切到选中那天
  });
}
function loadDaily(force) {
  if (!force && cache.daily && Date.now() - cache.dailyFetchedAt < 60000) {
    if (activeView === 'daily') drawDaily();
    return Promise.resolve();
  }
  var request = ++dailyRequest, epoch = dailyEpoch;
  return api('/api/daily', { days: 180 }).then(function (d) {
    if (epoch !== dailyEpoch) return;
    cache.daily = d.days || [];
    cache.dailyFetchedAt = Date.now();
    if (activeView === 'daily' && request === dailyRequest) drawDaily();
  }).catch(function (e) { if (e.message !== '未登录') toast(e.message); });
}
function loadNodeDaily(force) {
  if (state.nodeId == null) return Promise.resolve();
  var id = String(state.nodeId), cached = cache.nodeDaily[id];
  resetCalendar(id);
  if (!force && cached) {
    drawNodeDaily();
    // 日期要先确定下来，域名查询才知道该查哪一天。
    loadNodeDomains(false);
    if (Date.now() - cached.fetchedAt < 60000) return Promise.resolve();
  }
  var request = ++nodeDailyRequest, epoch = nodeDailyEpoch;
  if (!cached) {
    var host = document.getElementById('node-cal');
    if (host) host.innerHTML = '<div class="empty">正在加载节点趋势…</div>';
  }
  return api('/api/daily', { days: 180, scope: 'node:' + id })
    .then(function (d) {
      if (epoch !== nodeDailyEpoch) return;
      cache.nodeDaily[id] = { days: d.days || [], fetchedAt: Date.now() };
      if (activeView === 'node' && String(state.nodeId) === id && request === nodeDailyRequest) {
        drawNodeDaily();
        loadNodeDomains(force);
      }
    })
    .catch(function (e) { if (e.message !== '未登录') toast(e.message); });
}

/* ---------- 节点配置页：新增 / 查看分享 / 编辑 / 删除 ---------- */
var manageNodes = [];
var editingNode = null;

function loadManageNodes() {
  return api('/api/nodes').then(function (d) {
    manageNodes = d.nodes || [];
    renderManageNodes();
  }).catch(function (e) {
    if (e.message !== '未登录') toast(e.message);
  });
}

function refreshAfterNodeMutation() {
  cache.daily = null;
  cache.dailyFetchedAt = 0;
  cache.nodeDaily = Object.create(null);
  dailyRequest++;
  dailyEpoch++;
  nodeDailyRequest++;
  nodeDailyEpoch++;
  return Promise.all([loadManageNodes(), loadSummary()]).then(function () {
    if (activeView === 'daily') return loadDaily();
    if (activeView === 'node') return loadNodeDaily(true);
  });
}

function renderManageNodes() {
  var host = document.getElementById('node-manage-list');
  if (!host) return;
  if (!manageNodes.length) {
    host.innerHTML = '<div class="empty">暂无节点，点击“添加节点”创建</div>';
    return;
  }
  var conflicts = manageNodes.filter(function (n) {
    return Array.isArray(n.port_conflict_with) && n.port_conflict_with.length > 0;
  });
  var warning = conflicts.length
    ? '<div class="err node-conflict-notice">发现历史重复监听端口，防火墙变更会保持 fail-closed。请编辑冲突节点改为不同端口，或删除多余节点；修复前不能新增或分享冲突节点。</div>'
    : '';
  var cards = manageNodes.map(function (n) {
    var protocol = n.protocol || n.type;
    var meta = [n.type || protocol, '端口 ' + n.port];
    if (n.paused) meta.push('已暂停');
    if (Array.isArray(n.allow) && n.allow.length) meta.push('白名单 ' + n.allow.length + ' 项');
    var peers = Array.isArray(n.port_conflict_with) ? n.port_conflict_with : [];
    if (peers.length) meta.push('端口重复，与节点 #' + peers.join(', #') + ' 冲突');
    var checkbox = manageSelectMode
      ? '<input type="checkbox" class="node-pick" data-node-pick="' + esc(n.id) + '"' +
        (manageSelected[String(n.id)] ? ' checked' : '') + ' aria-label="选择 ' + esc(n.name) + '">'
      : '';
    return '<article class="manage-node' + (manageSelectMode ? ' selectable' : '') +
      (n.paused ? ' paused' : '') + (peers.length ? ' conflict' : '') +
      (manageSelected[String(n.id)] ? ' picked' : '') + '">' +
      checkbox +
      '<div class="manage-node-main"><strong>' + esc(n.name) + '</strong>' +
      '<span>' + esc(meta.join(' · ')) + '</span></div>' +
      '<div class="manage-node-actions">' +
      '<button type="button" class="mini-btn" data-node-links="' + esc(n.id) + '"' + (peers.length ? ' disabled title="请先解决端口冲突"' : '') + '>分享</button>' +
      '<button type="button" class="mini-btn" data-node-edit="' + esc(n.id) + '">编辑</button>' +
      '<button type="button" class="mini-btn danger" data-node-delete="' + esc(n.id) + '">删除</button>' +
      '</div></article>';
  }).join('');
  host.innerHTML = warning + cards;
  var delBtn = document.getElementById('node-batch-delete');
  if (delBtn) {
    var count = Object.keys(manageSelected).length;
    delBtn.classList.toggle('hidden', !manageSelectMode || count === 0);
    if (!pendingBatchDelete) delBtn.textContent = '删除所选（' + count + '）';
  }
}

/* ---- 批量管理模式：勾选节点后一次删除。默认关闭。 ---- */
var manageSelectMode = false;
var manageSelected = {};
// 批量删除的两步确认状态：第一次点击只进入「待确认」，5 秒内再点才真正删除。
// 不用 window.confirm：部分手机浏览器/内置 WebView 会直接屏蔽它，
// 用户点了没反应，看起来就像「删了也没确认」。
var pendingBatchDelete = null;

function resetBatchDeleteConfirm() {
  if (pendingBatchDelete && pendingBatchDelete.timer) clearTimeout(pendingBatchDelete.timer);
  pendingBatchDelete = null;
  var btn = document.getElementById('node-batch-delete');
  if (!btn) return;
  btn.classList.remove('confirming');
  btn.textContent = '删除所选（' + Object.keys(manageSelected).length + '）';
}

function setManageSelectMode(on) {
  manageSelectMode = on;
  if (!on) manageSelected = {};
  resetBatchDeleteConfirm();
  document.getElementById('node-batch-manage').textContent = on ? '退出批量管理' : '批量管理';
  document.getElementById('node-batch-manage').classList.toggle('on', on);
  document.getElementById('node-batch-delete').classList.toggle('hidden', true);
  renderManageNodes();
}

function findManageNode(id) {
  return manageNodes.filter(function (n) { return String(n.id) === String(id); })[0] || null;
}

function updateNodeFormFields() {
  var protocol = editingNode ? editingNode.protocol : document.getElementById('node-form-type').value;
  document.querySelectorAll('.node-form-sni').forEach(function (el) {
    el.classList.toggle('hidden', ['vless', 'trojan', 'anytls'].indexOf(protocol) < 0);
  });
  document.querySelectorAll('.node-form-ss').forEach(function (el) { el.classList.toggle('hidden', protocol !== 'shadowsocks'); });
  document.querySelectorAll('.node-form-snell').forEach(function (el) { el.classList.toggle('hidden', protocol !== 'snell'); });
  document.querySelectorAll('.node-form-psk').forEach(function (el) { el.classList.toggle('hidden', !editingNode || protocol !== 'snell'); });
  var secretNote = document.getElementById('node-secret-note');
  // 加密算法已在编辑模式下锁定（换算法请删节点重建），「切换重生成密钥」的路径
  // 已不存在，特殊提示一并移除；编辑非 SS 协议时整条隐藏。
  secretNote.textContent = '节点密钥和密码由服务器安全生成，面板不会显示或传输这些密钥。';
  secretNote.classList.toggle('hidden', !!editingNode && protocol !== 'shadowsocks');
}

function openNodeEditor(id) {
  editingNode = id == null ? null : findManageNode(id);
  if (id != null && !editingNode) { toast('节点不存在或列表尚未加载'); return; }
  document.getElementById('node-editor-title').textContent = editingNode ? '编辑节点' : '添加节点';
  document.getElementById('node-editor-kicker').textContent = editingNode ? '修改节点配置' : '创建新节点';
  document.getElementById('node-editor-type-section').classList.toggle('hidden', !!editingNode);
  document.getElementById('node-form-type').value = editingNode ? (editingNode.protocol || 'vless') : 'vless';
  document.getElementById('node-form-name').value = editingNode ? editingNode.name : '';
  document.getElementById('node-form-name').disabled = !!editingNode;
  document.getElementById('node-form-port').value = editingNode ? editingNode.port : '443';
  document.getElementById('node-form-sni').value = editingNode ? (editingNode.sni || '') : '';
  var methodSel = document.getElementById('node-form-method');
  methodSel.value = editingNode ? (editingNode.method || '2022-blake3-aes-128-gcm') : '2022-blake3-aes-128-gcm';
  var versionSel = document.getElementById('node-form-version');
  versionSel.value = editingNode ? String(editingNode.version || 5) : '5';
  // 加密算法与 Snell 版本都不允许在编辑时改：换算法必须连带换凭据，
  // 而旧凭据早就分发到客户端了，改了会表现为「节点突然连不上」且查不出原因。
  methodSel.disabled = !!editingNode;
  versionSel.disabled = !!editingNode;
  setFieldNote(methodSel, editingNode ? '加密算法（不可修改，换算法请删除后重建）' : '加密算法');
  setFieldNote(versionSel, editingNode ? 'Snell 版本（不可修改）' : 'Snell 版本');
  document.getElementById('node-form-psk').value = '';
  document.getElementById('node-form-error').classList.add('hidden');
  var allow = (editingNode && Array.isArray(editingNode.allow)) ? editingNode.allow : [];
  document.getElementById('node-form-allow-on').checked = allow.length > 0;
  document.getElementById('node-form-allow-body').classList.toggle('hidden', allow.length === 0);
  renderAllowPicker(allow);
  var btn = document.getElementById('node-form-save');
  btn.disabled = false; btn.textContent = editingNode ? '保存修改' : '添加节点';
  updateNodeFormFields();
  openDrawer('node-editor-drawer');
}

function nodeRequest(path, method, body) {
  return fetch(path, {
    method: method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined
  }).then(function (r) {
    if (r.status === 401) { location.replace('/login'); throw new Error('未登录'); }
    return r.json().then(function (d) { if (!r.ok) throw new Error(d.error || ('请求失败 ' + r.status)); return d; });
  });
}

// setFieldNote 改字段上方那行小标题的文案。
//
// 编辑模式下有几个字段被锁死（加密算法、Snell 版本），光靠 disabled 用户只会
// 觉得「点了没反应」，得直接说明为什么。
function setFieldNote(el, text) {
  if (!el || !el.closest) return;
  var label = el.closest('label');
  var note = label ? label.querySelector('small') : null;
  if (note) note.textContent = text;
}

function saveNodeConfig() {
  var port = Number(document.getElementById('node-form-port').value);
  if (!(port >= 1 && port <= 65535)) { showNodeFormError('端口必须在 1–65535'); return; }
  // 开关关掉 = 不限制（提交空数组即解除限制）；开着但一项没勾，则拒绝提交——
  // 那会让节点什么都访问不了，几乎肯定是误操作。
  var allowOn = document.getElementById('node-form-allow-on').checked;
  var allow = allowOn ? selectedAllow() : [];
  if (allowOn && !allow.length) { showNodeFormError('已开启访问限制，请至少勾选一项服务'); return; }
  var body, path, method;
  if (editingNode) {
    body = { port: port };
    var protocol = editingNode.protocol || '';
    if (['vless', 'trojan', 'anytls'].indexOf(protocol) >= 0) body.sni = document.getElementById('node-form-sni').value.trim();
    // 刻意不提交 method：加密算法不允许改（见 openNodeEditor 的说明）。
    if (protocol === 'snell') body.psk = document.getElementById('node-form-psk').value.trim();
    path = '/api/nodes/' + editingNode.id; method = 'PUT';
  } else {
    body = {
      type: document.getElementById('node-form-type').value,
      name: document.getElementById('node-form-name').value.trim(),
      port: port,
      sni: document.getElementById('node-form-sni').value.trim(),
      method: document.getElementById('node-form-method').value,
      version: Number(document.getElementById('node-form-version').value)
    };
    path = '/api/nodes'; method = 'POST';
  }
  body.allow = allow;
  var btn = document.getElementById('node-form-save');
  btn.disabled = true; btn.textContent = '处理中…';
  nodeRequest(path, method, body).then(function (d) {
    closeDrawer('node-editor-drawer');
    toast(d.warning || (editingNode ? '节点已更新' : '节点已添加'));
    editingNode = null;
    return refreshAfterNodeMutation();
  }).catch(function (e) {
    if (e.message !== '未登录') showNodeFormError(e.message);
  }).finally(function () {
    btn.disabled = false; btn.textContent = editingNode ? '保存修改' : '添加节点';
  });
}

function showNodeFormError(message) {
  var el = document.getElementById('node-form-error');
  el.textContent = message; el.classList.remove('hidden');
}

/* ---------- 节点访问域名（来自 sing-box 连接表采样） ---------- */
// 数据说明：只统计存活超过采样间隔（约 2 秒）的连接，因此列表里的流量
// 是估算值。接口额外给出「未归类」的量，界面把它单独列一行，让偏差可见。
var domainsCache = {};
var domainsRequest = 0;

// 域名是「按天」查的：日历选到哪天就查哪天，缓存键也要带上日期，
// 否则切日期时会把上一天的列表又画一遍。
function loadNodeDomains(force) {
  if (state.nodeId == null) return;
  var id = String(state.nodeId);
  var day = calState.day || isoToday();
  var key = id + '@' + day;
  var cached = domainsCache[key];
  if (!force && cached && Date.now() - cached.fetchedAt < 30000) { drawNodeDomains(); return; }
  var request = ++domainsRequest;
  if (!cached) {
    var host0 = document.getElementById('node-domains-table');
    if (host0) host0.innerHTML = '<div class="empty">正在加载…</div>';
  }
  api('/api/node-domains', { id: id, day: day, limit: 100 }).then(function (d) {
    domainsCache[key] = { data: d, fetchedAt: Date.now() };
    if (activeView === 'node' && String(state.nodeId) === id && request === domainsRequest) drawNodeDomains();
  }).catch(function (e) { if (e.message !== '未登录') toast(e.message); });
}

function drawNodeDomains() {
  if (state.nodeId == null) return;
  var id = String(state.nodeId);
  var day = calState.day || isoToday();
  var host = document.getElementById('node-domains-table');
  var meta = document.getElementById('node-domains-meta');
  var title = document.getElementById('node-domains-title');
  if (!host) return;
  if (title) title.textContent = (day === isoToday() ? '今日访问域名' : day + ' 访问域名');
  var entry = domainsCache[id + '@' + day];
  if (!entry) return;
  var d = entry.data || {}, rows = d.domains || [], total = d.total || 0;
  if (meta) {
    meta.textContent = total > 0
      ? '合计 ' + fmtBytes(total) + (d.unclassified > 0 ? ' · 未归类 ' + fmtBytes(d.unclassified) : '')
      : '暂无流量';
  }
  if (!rows.length) {
    host.innerHTML = '<div class="empty">' + (total > 0
      ? '该日有流量，但尚未采到域名明细——只统计存活超过约 2 秒的连接。'
      : '该日没有流量记录。') + '</div>';
    return;
  }
  var body = rows.map(function (r, i) {
    return '<tr>' +
      '<td class="dom-rank">' + (i + 1) + '</td>' +
      '<td class="dom-name" title="' + esc(r.domain) + '">' + esc(r.domain) + '</td>' +
      '<td class="dom-num">' + fmtBytes(r.total) + '</td>' +
      '<td class="dom-num">' + (r.hits || 0) + '</td>' +
      '</tr>';
  }).join('');
  var foot = '';
  if (d.unclassified > 0) {
    foot = '<tr class="dom-rest"><td></td><td>未归类（短连接）</td>' +
      '<td class="dom-num">' + fmtBytes(d.unclassified) + '</td><td></td></tr>';
  }
  host.innerHTML = '<table class="dom-table"><thead><tr>' +
    '<th>#</th><th>域名</th><th>流量</th><th>次数</th>' +
    '</tr></thead><tbody>' + body + foot + '</tbody></table>';
}

/* ---------- 访问限制（只允许访问特定服务） ---------- */
// 规则集清单来自服务端内置的一览（上游 blackmatrix7/ios_rule_script 的文本规则，
// 由服务端下载并转换成 sing-box 规则集）。面板只负责勾选，不提供自由填写域名——
// 避免用户写出无效规则，也避免把任意内容拼进 sing-box 配置。
//
// 清单有四百多项，平铺出来没法用，所以按上游首字母分组、默认只展开已选项所在的组，
// 顶部再给一个搜索框。
var ruleSetCache = null;
var allowGroups = [];
var allowSelected = {};
var allowQuery = '';
var allowExpanded = false;

function loadRuleSets() {
  if (ruleSetCache) return Promise.resolve(ruleSetCache);
  return fetch('/api/rulesets').then(function (r) {
    if (r.status === 401) { location.replace('/login'); throw new Error('未登录'); }
    return r.json().then(function (d) { if (!r.ok) throw new Error(d.error || ('请求失败 ' + r.status)); return d; });
  }).then(function (d) {
    ruleSetCache = d.groups || [];
    return ruleSetCache;
  });
}

function renderAllowPicker(selected) {
  allowSelected = {};
  (selected || []).forEach(function (n) { allowSelected[n] = true; });
  allowQuery = '';
  allowExpanded = false;
  var search = document.getElementById('node-form-allow-search');
  if (search) search.value = '';
  var toggle = document.getElementById('node-form-allow-toggle');
  if (toggle) toggle.textContent = '展开全部';
  var host = document.getElementById('node-form-allow-list');
  if (!host) return;
  host.innerHTML = '<div class="empty">正在加载可选服务…</div>';
  loadRuleSets().then(function (groups) {
    allowGroups = groups;
    drawAllowPicker();
  }).catch(function (e) {
    host.innerHTML = '<div class="empty">无法加载可选服务列表：' + esc(e.message) + '</div>';
  });
}

// drawAllowPicker 只负责按当前搜索词与折叠状态重绘。
// 勾选状态存在 allowSelected 里，重绘不会丢用户已经点过的选择。
function drawAllowPicker() {
  var host = document.getElementById('node-form-allow-list');
  if (!host) return;
  var q = allowQuery.trim().toLowerCase();
  var parts = [];
  allowGroups.forEach(function (g) {
    var items = (g.items || []).filter(function (it) {
      if (!q) return true;
      return (it.name + ' ' + (it.label || '')).toLowerCase().indexOf(q) >= 0;
    });
    if (!items.length) return;
    var picked = 0;
    var body = items.map(function (it) {
      var on = allowSelected[it.name] ? true : false;
      if (on) picked++;
      return '<label class="ruleset-item"><input type="checkbox" value="' + esc(it.name) + '"' +
        (on ? ' checked' : '') + '><span>' + esc(it.label || it.name) + '</span></label>';
    }).join('');
    // 搜索时一律展开（用户在找东西）；平时只展开有已选项的组，避免四百多项铺满屏幕。
    var open = q !== '' || allowExpanded || picked > 0;
    parts.push('<section class="ruleset-group' + (open ? ' open' : '') + '">' +
      '<button type="button" class="ruleset-group-head">' +
      '<i class="rs-caret" aria-hidden="true"></i>' +
      '<span>' + esc(g.name) + '</span>' +
      '<b>' + (picked ? picked + '/' : '') + items.length + '</b></button>' +
      '<div class="ruleset-items">' + body + '</div></section>');
  });
  host.innerHTML = parts.length
    ? parts.join('')
    : '<div class="empty">没有匹配「' + esc(allowQuery) + '」的服务。</div>';
  updateAllowCount();
}

// refreshAllowGroupCounts 在勾选变化时更新各组的「已选/总数」，不重建 DOM——
// 重建会让刚点过的复选框失去焦点，连续勾选多个时会很难受。
function refreshAllowGroupCounts() {
  document.querySelectorAll('#node-form-allow-list .ruleset-group').forEach(function (sec) {
    var boxes = sec.querySelectorAll('input[type=checkbox]');
    var picked = 0;
    boxes.forEach(function (b) { if (b.checked) picked++; });
    var badge = sec.querySelector('.ruleset-group-head b');
    if (badge) badge.textContent = (picked ? picked + '/' : '') + boxes.length;
  });
}

function bindAllowPicker() {
  var host = document.getElementById('node-form-allow-list');
  var search = document.getElementById('node-form-allow-search');
  var toggle = document.getElementById('node-form-allow-toggle');
  if (host && !host.dataset.bound) {
    host.dataset.bound = '1';
    host.addEventListener('change', function (e) {
      var el = e.target;
      if (!el || el.type !== 'checkbox') return;
      if (el.checked) allowSelected[el.value] = true; else delete allowSelected[el.value];
      refreshAllowGroupCounts();
      updateAllowCount();
    });
    host.addEventListener('click', function (e) {
      var head = e.target && e.target.closest ? e.target.closest('.ruleset-group-head') : null;
      if (!head) return;
      e.preventDefault();
      head.parentNode.classList.toggle('open');
    });
  }
  if (search && !search.dataset.bound) {
    search.dataset.bound = '1';
    // 回车在表单里会触发提交，这里必须拦掉。
    search.addEventListener('keydown', function (e) { if (e.key === 'Enter') e.preventDefault(); });
    search.addEventListener('input', function () {
      allowQuery = search.value || '';
      drawAllowPicker();
    });
  }
  if (toggle && !toggle.dataset.bound) {
    toggle.dataset.bound = '1';
    toggle.addEventListener('click', function (e) {
      e.preventDefault();
      allowExpanded = !allowExpanded;
      toggle.textContent = allowExpanded ? '收起全部' : '展开全部';
      drawAllowPicker();
    });
  }
}

function selectedAllow() {
  return Object.keys(allowSelected);
}

function updateAllowCount() {
  var el = document.getElementById('node-form-allow-count');
  if (!el) return;
  var n = selectedAllow().length;
  el.classList.toggle('warn', n === 0);
  el.textContent = n
    ? '已选 ' + n + ' 项服务'
    : '尚未勾选任何服务。此时该节点将无法访问任何网站，请至少选择一项。';
}

function deleteNodeConfig(id) {
  var n = findManageNode(id);
  if (!n) return;
  var confirmation = window.prompt('即将删除节点“' + n.name + '”。\n\n该节点的流量历史会完整保留（每日明细与累计都留着），之后仍可在「日报 → 节点」下拉的「历史节点（已删除）」里查看。\n\n请输入“我已确定”以继续删除：');
  if (confirmation === null) return;
  if (confirmation.trim() !== '我已确定') { toast('验证文字不匹配，已取消删除'); return; }
  nodeRequest('/api/nodes/' + id, 'DELETE').then(function (d) {
    toast(d.warning || (d.history_cleared ? '节点已删除；每日与采样历史已按要求清理，累计流量保留' : '节点已删除；流量历史与累计均已保留'));
    return refreshAfterNodeMutation();
  }).catch(function (e) { if (e.message !== '未登录') toast(e.message); });
}

var nodeLinkTexts = [];

// copyText 在 http 明文环境（navigator.clipboard 不可用）下退回到
// execCommand；两条路都失败时让用户手动长按复制。
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(text);
  }
  return new Promise(function (resolve, reject) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    var ok = false;
    try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
    document.body.removeChild(ta);
    if (ok) resolve(); else reject(new Error('复制失败，请长按文本手动复制'));
  });
}

function showNodeLinks(id) {
  var n = findManageNode(id);
  document.getElementById('node-links-title').textContent = n ? n.name : '节点分享';
  var host = document.getElementById('node-links-list');
  host.innerHTML = '<div class="empty">正在加载分享链接…</div>';
  openDrawer('node-links-drawer');
  nodeRequest('/api/nodes/' + id + '/links', 'GET').then(function (d) {
    // 面板走 http 明文时 navigator.clipboard 不可用（安全上下文限制），
    // 统一走 execCommand 回退；iOS Safari 也支持。
    var items = [{ label: '分享链接', text: d.ipv4 }];
    if (d.ipv6) items.push({ label: 'IPv6', text: d.ipv6 });
    (d.surge || []).forEach(function (line, i) {
      items.push({ label: 'Surge' + (i ? ' IPv6' : ''), text: line });
    });
    nodeLinkTexts = items.map(function (it) { return it.text; });
    var html = items.map(function (it, i) {
      return '<div class="link-field"><small>' + esc(it.label) + '</small>' +
        '<div class="link-row"><pre>' + esc(it.text) + '</pre>' +
        '<button type="button" class="mini-btn" data-copy-link="' + i + '">复制</button></div></div>';
    }).join('');
    host.innerHTML = html;
  }).catch(function (e) {
    if (e.message !== '未登录') host.innerHTML = '<div class="err">' + esc(e.message) + '</div>';
  });
}

function bindNodeManageUI() {
  document.getElementById('node-create-open').addEventListener('click', function () { openNodeEditor(null); });
  document.getElementById('node-form-type').addEventListener('change', updateNodeFormFields);
  document.getElementById('node-form-save').addEventListener('click', saveNodeConfig);
  document.getElementById('node-form-allow-on').addEventListener('change', function (e) {
    document.getElementById('node-form-allow-body').classList.toggle('hidden', !e.target.checked);
    updateAllowCount();
  });
  document.getElementById('node-form-allow-list').addEventListener('change', updateAllowCount);
  document.getElementById('node-form-cancel').addEventListener('click', function () { closeDrawer('node-editor-drawer'); });
  document.getElementById('node-editor-close').addEventListener('click', function () { closeDrawer('node-editor-drawer'); });
  document.getElementById('node-links-close').addEventListener('click', function () { closeDrawer('node-links-drawer'); });
  document.getElementById('node-links-done').addEventListener('click', function () { closeDrawer('node-links-drawer'); });
  document.getElementById('node-links-list').addEventListener('click', function (e) {
    var btn = e.target.closest('[data-copy-link]');
    if (!btn) return;
    var text = nodeLinkTexts[Number(btn.getAttribute('data-copy-link'))];
    if (text == null) return;
    copyText(text).then(function () { toast('已复制到剪贴板'); })
      .catch(function (err) { toast(err.message || '复制失败'); });
  });
  document.getElementById('node-manage-list').addEventListener('click', function (e) {
    var el = e.target.closest('[data-node-links]'); if (el) { showNodeLinks(el.getAttribute('data-node-links')); return; }
    el = e.target.closest('[data-node-edit]'); if (el) { openNodeEditor(el.getAttribute('data-node-edit')); return; }
    el = e.target.closest('[data-node-delete]'); if (el) deleteNodeConfig(el.getAttribute('data-node-delete'));
  });
  document.getElementById('node-manage-list').addEventListener('change', function (e) {
    var box = e.target.closest('[data-node-pick]');
    if (!box) return;
    var id = box.getAttribute('data-node-pick');
    if (box.checked) manageSelected[id] = true; else delete manageSelected[id];
    resetBatchDeleteConfirm();
    renderManageNodes();
  });
  document.getElementById('node-batch-manage').addEventListener('click', function () {
    setManageSelectMode(!manageSelectMode);
  });
  document.getElementById('node-batch-delete').addEventListener('click', function () {
    var btn = this;
    var ids = Object.keys(manageSelected);
    if (!ids.length) return;

    if (!pendingBatchDelete) {
      // 第一步：进入待确认。按钮变红并写明数量，5 秒不操作自动退回。
      pendingBatchDelete = { ids: ids.slice(), timer: setTimeout(resetBatchDeleteConfirm, 5000) };
      btn.classList.add('confirming');
      btn.textContent = '确认删除 ' + ids.length + ' 个节点？';
      return;
    }

    // 第二步：真正删除。执行前先取消计时器，避免中途被重置。
    var toDelete = pendingBatchDelete.ids;
    clearTimeout(pendingBatchDelete.timer);
    pendingBatchDelete = null;
    btn.classList.remove('confirming');
    btn.disabled = true;
    nodeRequest('/api/nodes/delete-batch', 'POST', { ids: toDelete }).then(function (d) {
      toast('已删除 ' + toDelete.length + ' 个节点，历史流量已保留');
      setManageSelectMode(false);
      loadManageNodes();
      if (d.warning) toast(d.warning);
    }).catch(function (e) {
      if (e.message !== '未登录') toast(e.message);
      resetBatchDeleteConfirm();
    }).finally(function () { btn.disabled = false; });
  });
  document.getElementById('node-batch-open').addEventListener('click', function () {
    document.getElementById('node-batch-prefix').value = '';
    document.getElementById('node-batch-count').value = 3;
    document.getElementById('node-batch-port').value = '';
    openDrawer('node-batch-drawer');
  });
  document.getElementById('node-batch-close').addEventListener('click', function () { closeDrawer('node-batch-drawer'); });
  document.getElementById('node-batch-cancel').addEventListener('click', function () { closeDrawer('node-batch-drawer'); });
  document.getElementById('node-batch-save').addEventListener('click', saveNodeBatch);
}

function saveNodeBatch() {
  var type = document.getElementById('node-batch-type').value;
  var prefix = document.getElementById('node-batch-prefix').value.trim();
  var count = Number(document.getElementById('node-batch-count').value);
  var portRaw = document.getElementById('node-batch-port').value.trim();
  var body = { type: type, name_prefix: prefix, count: count };
  if (portRaw !== '') {
    if (!/^\d+$/.test(portRaw)) { toast('起始端口必须是数字'); return; }
    body.port_start = Number(portRaw);
  }
  var btn = document.getElementById('node-batch-save');
  btn.disabled = true;
  nodeRequest('/api/nodes/batch', 'POST', body).then(function (d) {
    closeDrawer('node-batch-drawer');
    toast('已创建 ' + d.count + ' 个节点');
    loadManageNodes();
    if (d.warning) toast(d.warning);
  }).catch(function (e) {
    if (e.message !== '未登录') toast(e.message);
  }).finally(function () { btn.disabled = false; });
}

/* ---------- 事件 ---------- */
document.getElementById('node-select').addEventListener('change', function (e) {
  state.nodeId = e.target.value || null;
  loadNodeDaily();
});

/* ---------- 底部导航 ---------- */
(function initNav() {
  var valid = { home: 1, daily: 1, node: 1, manage: 1 }, positions = { home: 0, daily: 0, node: 0, manage: 0 };
  var current = (location.hash || '#home').slice(1); if (!valid[current]) current = 'home';
  function show(name, push) {
    if (!valid[name]) name = 'home';
    positions[current] = window.scrollY || 0; current = name; activeView = name;
    document.querySelectorAll('.view').forEach(function (v) { v.classList.toggle('on', v.id === 'view-' + name); });
    document.querySelectorAll('.tab[data-view]').forEach(function (b) { b.classList.toggle('on', b.dataset.view === name); });
    if (push && location.hash !== '#' + name) history.pushState(null, '', '#' + name);
    requestAnimationFrame(function () { window.scrollTo(0, positions[name] || 0); });
    if (name === 'home') {
      if (state.summary) renderNodeCards(state.summary);
      if (state.live) renderLive(state.live);
      loadSummary();
      loadLive();
    }
    if (name === 'daily') loadDaily();
    if (name === 'node') loadNodeDaily();
    if (name === 'manage') loadManageNodes();
  }
  document.querySelectorAll('.tab[data-view]').forEach(function (b) {
    b.addEventListener('click', function () { show(b.dataset.view, true); });
  });
  window.addEventListener('popstate', function () { show((location.hash || '#home').slice(1), false); });
  show(current, false);
})();

document.getElementById('logout-button').addEventListener('click', function () {
  fetch('/api/logout', { method: 'POST', cache: 'no-store', credentials: 'same-origin' })
    .then(function (r) {
      if (r.status === 401) { location.replace('/login'); return; }
      if (!r.ok) throw new Error('登出失败 ' + r.status);
      location.replace('/login');
    })
    .catch(function (e) { toast(e.message); });
});

/* ---------- 启动与轮询 ---------- */
function loadSummary() { return api('/api/summary').then(renderSummary).catch(function (e) { if (e.message !== '未登录') toast(e.message); }); }
function loadLive() { return api('/api/live').then(renderLive).catch(function () {}); }

loadSummary().then(function () { if (activeView === 'home') loadLive(); });
startEvents();
setInterval(function () { if (!document.hidden && activeView === 'home') loadLive(); }, 2000);
setInterval(function () { if (!document.hidden && activeView === 'home') loadSummary(); }, 8000);
setInterval(function () {
  if (document.hidden) return;
  if (activeView === 'daily') loadDaily(true);
  else if (activeView === 'node') loadNodeDaily(true);
}, 60000);
document.addEventListener('visibilitychange', function () {
  if (!document.hidden && activeView === 'home') {
    loadSummary();
    loadLive();
  }
});

/* ==================== 节点策略管理（暂停 / IP Limit / Rate Limit） ==================== */
var policyState = { nodeId: null, summaryNode: null };

/* ---------- SSE 实时在线 IP ---------- */
var ipState = {};           // nodeId -> NodeIPSnapshot
var ipsDrawerNodeId = null; // 当前打开的在线 IP 抽屉节点

function nodeIPText(nodeId) {
  var s = ipState[String(nodeId)];
  if (!s) return null;
  return s.limited ? (s.granted_count + ' / ' + s.max_ips) : String(s.granted_count);
}

// 局部 patch 单节点在线 IP 数量（不整页/整卡刷新）。
function renderNodeIPCount(nodeId) {
  var txt = nodeIPText(nodeId);
  if (txt == null) return;
  var el = document.querySelector('[data-node-ips="' + String(nodeId) + '"]');
  if (el && el.textContent !== txt) el.textContent = txt;
}

function escIPEntry(e) {
  var v6 = e.ip.indexOf(':') >= 0;
  var proto = '在线 · ' + (e.tcp || 0) + ' TCP · ' + (e.udp || 0) + ' UDP';
  return '<div class="ip-item">' +
    '<div class="ip-line"><span class="ip-addr">' + esc(e.ip) + '</span>' +
    '<span class="ip-tag">' + (v6 ? 'IPv6' : 'IPv4') + '</span></div>' +
    '<span class="ip-meta">' + proto + '</span></div>';
}

function escRejectedEntry(e) {
  return '<div class="ip-item rejected">' +
    '<div class="ip-line"><span class="ip-addr">' + esc(e.ip) + '</span>' +
    '<span class="ip-tag danger">已拒绝</span></div>' +
    '<span class="ip-meta">原因：在线 IP 已达上限</span></div>';
}

function renderIPList() {
  if (!ipsDrawerNodeId) return;
  var list = document.getElementById('ips-list');
  var s = ipState[String(ipsDrawerNodeId)];
  if (!list) return;
  if (!s || ((s.ips || []).length === 0 && (s.rejected || []).length === 0)) {
    list.innerHTML = '<div class="empty">暂无在线 IP</div>';
    return;
  }
  var html = (s.ips || []).map(escIPEntry).join('') + (s.rejected || []).map(escRejectedEntry).join('');
  list.innerHTML = html;
}

function onSnapshot(msg) {
  ipState = {};
  (msg.nodes || []).forEach(function (ns) { ipState[String(ns.node_id)] = ns; });
  Object.keys(ipState).forEach(renderNodeIPCount);
  renderIPList();
}

function onNodeEvent(ns) {
  ipState[String(ns.node_id)] = ns;
  renderNodeIPCount(String(ns.node_id));
  if (ipsDrawerNodeId === String(ns.node_id)) renderIPList();
}

function startEvents() {
  // EventSource 原生自动重连；服务重启后重连即收到完整 snapshot。
  var es = new EventSource('/api/events');
  es.addEventListener('snapshot', function (e) { try { onSnapshot(JSON.parse(e.data)); } catch (err) {} });
  es.addEventListener('node', function (e) { try { onNodeEvent(JSON.parse(e.data)); } catch (err) {} });
  es.onerror = function () { /* 断线由 EventSource 自动重连，无需手工处理 */ };
}

function showPolicy(nodeId) {
  var n = (state.summary && state.summary.nodes || []).filter(function (x) { return String(x.id) === String(nodeId); })[0];
  if (!n) return;
  policyState.nodeId = String(nodeId);
  policyState.summaryNode = n;
  document.getElementById('drawer-node-name').textContent = n.name;
  document.getElementById('pol-node-enabled').checked = !n.paused;
  document.getElementById('pol-ip-active').textContent = (n.active_ip_count || 0);
  document.getElementById('pol-ip-enable').checked = !!n.ip_limit_enabled;
  document.getElementById('pol-ip-box').classList.toggle('hidden', !n.ip_limit_enabled);
  document.getElementById('pol-ip-max').value = n.ip_limit_max > 0 ? n.ip_limit_max : '';
  document.getElementById('pol-rate-enable').checked = !!n.rate_limit_enabled;
  document.getElementById('pol-rate-box').classList.toggle('hidden', !n.rate_limit_enabled);
  document.getElementById('pol-rate-mbps').value = n.rate_limit_mbps > 0 ? n.rate_limit_mbps : '';
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

function savePolicy() {
  var ipOn = document.getElementById('pol-ip-enable').checked;
  var rateOn = document.getElementById('pol-rate-enable').checked;
  var ipMax = document.getElementById('pol-ip-max').value;
  var rateMbps = document.getElementById('pol-rate-mbps').value;

  var body = {
    paused: !document.getElementById('pol-node-enabled').checked,
    ip_limit_enabled: ipOn,
    ip_limit_max: ipOn ? Number(ipMax) : 0,
    rate_limit_enabled: rateOn,
    rate_limit_mbps: rateOn ? Number(rateMbps) : 0
  };
  if (ipOn && !(body.ip_limit_max >= 1)) { showPolError('最大 IP 数必须 ≥ 1'); return; }
  if (rateOn && !(body.rate_limit_mbps >= 1)) { showPolError('限速值必须 ≥ 1 Mbps'); return; }

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

function showActiveIPs(nodeId) {
  var id = nodeId != null ? String(nodeId) : policyState.nodeId;
  ipsDrawerNodeId = String(id);
  var name = '';
  var found = (state.summary && state.summary.nodes || []).filter(function (x) { return String(x.id) === id; })[0];
  if (found) name = found.name;
  else if (policyState.summaryNode && String(policyState.summaryNode.id) === id) name = policyState.summaryNode.name;
  document.getElementById('ips-node-name').textContent = name;
  openDrawer('ips-drawer');
  // 先用已推送的 SSE 状态渲染；没有则 fallback 拉取一次。
  renderIPList();
  fetch('/api/nodes/' + id + '/ip-state')
    .then(function (r) {
      if (r.status === 401) { location.replace('/login'); throw new Error('未登录'); }
      return r.json().then(function (d) { if (!r.ok) throw new Error(d.error || '请求失败'); return d; });
    })
    .then(function (ns) {
      ipState[String(id)] = ns;
      renderIPList();
    })
    .catch(function (e) { if (e.message !== '未登录') toast(e.message); });
}

/* 事件委托：节点卡片上的在线 IP 条 + 管理按钮（动态渲染） */
document.getElementById('node-cards').addEventListener('click', function (e) {
  var ips = e.target.closest('[data-view-ips]');
  if (ips) { showActiveIPs(ips.getAttribute('data-view-ips')); return; }
  var mg = e.target.closest('[data-manage]');
  if (mg) { showPolicy(mg.getAttribute('data-manage')); return; }
});
document.getElementById('drawer-close').addEventListener('click', function () { closeDrawer('policy-drawer'); });
document.getElementById('drawer-mask').addEventListener('click', function () { closeDrawer('policy-drawer'); closeDrawer('ips-drawer'); closeDrawer('node-editor-drawer'); closeDrawer('node-links-drawer'); closeDrawer('node-batch-drawer'); });
document.getElementById('pol-cancel').addEventListener('click', function () { closeDrawer('policy-drawer'); });
document.getElementById('pol-save').addEventListener('click', savePolicy);
document.getElementById('ips-close').addEventListener('click', function () { closeDrawer('ips-drawer'); });
document.getElementById('pol-ip-enable').addEventListener('change', function (e) {
  document.getElementById('pol-ip-box').classList.toggle('hidden', !e.target.checked);
});
document.getElementById('pol-rate-enable').addEventListener('change', function (e) {
  document.getElementById('pol-rate-box').classList.toggle('hidden', !e.target.checked);
});
bindNodeManageUI();
bindAllowPicker();
bindCalendar();
