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
  return '<div class="node-stat"><span>流量配额</span><b>' + fmtBytes(n.quota_used_bytes) + ' / ' + fmtBytes(n.quota_limit_bytes) + '</b></div>';
}
function nodeStatus(n) {
  if (n.quota_state === 'exceeded') return '<span class="status-pill danger">流量已用尽</span>';
  if (n.ip_limit_state === 'exceeded') return '<span class="status-pill warn">IP 已达上限</span>';
  return '<span class="status-pill ok">正常</span>';
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
        '<div class="node-stat"><span>TCP 连接</span><b data-node-live="' + esc(n.id) + '" data-kind="conns">—</b></div>' +
        '<div class="node-stat"><span>UDP 会话</span><b data-node-live="' + esc(n.id) + '" data-kind="conns_udp">—</b></div>' +
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
startEvents();
setInterval(function () { if (!document.hidden) loadLive(); }, 2000);
setInterval(function () { if (!document.hidden) loadSummary(); }, 8000);
setInterval(function () { if (!document.hidden) { loadDaily(); loadNodeDaily(); } }, 60000);
document.addEventListener('visibilitychange', function () {
  if (!document.hidden) { loadLive(); loadSummary(); }
});

/* ==================== 节点策略管理（Quota / IP Limit） ==================== */
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
  document.getElementById('pol-quota-used').textContent = fmtBytes(n.quota_used_bytes || 0);
  document.getElementById('pol-ip-active').textContent = (n.active_ip_count || 0);
  document.getElementById('pol-quota-enable').checked = !!n.quota_enabled;
  document.getElementById('pol-ip-enable').checked = !!n.ip_limit_enabled;
  document.getElementById('pol-quota-box').classList.toggle('hidden', !n.quota_enabled);
  document.getElementById('pol-ip-box').classList.toggle('hidden', !n.ip_limit_enabled);
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

  var body = {
    quota_enabled: quotaOn,
    quota_limit_bytes: quotaOn ? unitToBytes(quotaVal, quotaUnit) : 0,
    ip_limit_enabled: ipOn,
    ip_limit_max: ipOn ? Number(ipMax) : 0
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
document.getElementById('drawer-mask').addEventListener('click', function () { closeDrawer('policy-drawer'); closeDrawer('ips-drawer'); });
document.getElementById('pol-cancel').addEventListener('click', function () { closeDrawer('policy-drawer'); });
document.getElementById('pol-save').addEventListener('click', savePolicy);
document.getElementById('pol-quota-reset').addEventListener('click', resetQuota);
document.getElementById('ips-close').addEventListener('click', function () { closeDrawer('ips-drawer'); });
document.getElementById('pol-quota-enable').addEventListener('change', function (e) {
  document.getElementById('pol-quota-box').classList.toggle('hidden', !e.target.checked);
});
document.getElementById('pol-ip-enable').addEventListener('change', function (e) {
  document.getElementById('pol-ip-box').classList.toggle('hidden', !e.target.checked);
});
