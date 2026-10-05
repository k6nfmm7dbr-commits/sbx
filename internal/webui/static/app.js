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
  var previous = state.nodeId == null ? '' : String(state.nodeId);
  var exists = nodes.some(function (n) { return String(n.id) === previous; });
  var want = exists ? previous : (nodes.length ? String(nodes[0].id) : '');
  var sig = nodes.map(function (n) { return n.id + ':' + n.name; }).join('|');
  if (sel._sig !== sig) {
    sel._sig = sig;
    sel.innerHTML = nodes.map(function (n) { return '<option value="' + esc(n.id) + '">' + esc(n.name) + '</option>'; }).join('');
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
function drawNodeDaily() {
  if (state.nodeId == null) return;
  var entry = cache.nodeDaily[String(state.nodeId)];
  if (entry) renderTable('node-daily-table', entry.days);
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
  if (!force && cached) {
    drawNodeDaily();
    if (Date.now() - cached.fetchedAt < 60000) return Promise.resolve();
  }
  var request = ++nodeDailyRequest, epoch = nodeDailyEpoch;
  if (!cached) {
    var host = document.getElementById('node-daily-table');
    if (host) host.innerHTML = '<div class="empty">正在加载节点趋势…</div>';
  }
  return api('/api/daily', { days: 180, scope: 'node:' + id })
    .then(function (d) {
      if (epoch !== nodeDailyEpoch) return;
      cache.nodeDaily[id] = { days: d.days || [], fetchedAt: Date.now() };
      if (activeView === 'node' && String(state.nodeId) === id && request === nodeDailyRequest) drawNodeDaily();
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
  host.innerHTML = manageNodes.map(function (n) {
    var protocol = n.protocol || n.type;
    var meta = [n.type || protocol, '端口 ' + n.port];
    if (n.paused) meta.push('已暂停');
    return '<article class="manage-node' + (n.paused ? ' paused' : '') + '">' +
      '<div class="manage-node-main"><strong>' + esc(n.name) + '</strong>' +
      '<span>' + esc(meta.join(' · ')) + '</span></div>' +
      '<div class="manage-node-actions">' +
      '<button type="button" class="mini-btn" data-node-links="' + esc(n.id) + '">分享</button>' +
      '<button type="button" class="mini-btn" data-node-edit="' + esc(n.id) + '">编辑</button>' +
      '<button type="button" class="mini-btn danger" data-node-delete="' + esc(n.id) + '">删除</button>' +
      '</div></article>';
  }).join('');
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
  secretNote.textContent = editingNode && protocol === 'shadowsocks'
    ? '切换加密算法会重新生成密钥；新的分享链接会同步更新。'
    : '节点密钥和密码由服务器安全生成，面板不会显示或传输这些密钥。';
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
  document.getElementById('node-form-method').value = editingNode ? (editingNode.method || '2022-blake3-aes-128-gcm') : '2022-blake3-aes-128-gcm';
  document.getElementById('node-form-version').value = editingNode ? String(editingNode.version || 5) : '5';
  document.getElementById('node-form-version').disabled = !!editingNode;
  document.getElementById('node-form-psk').value = '';
  document.getElementById('node-form-error').classList.add('hidden');
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

function saveNodeConfig() {
  var port = Number(document.getElementById('node-form-port').value);
  if (!(port >= 1 && port <= 65535)) { showNodeFormError('端口必须在 1–65535'); return; }
  var body, path, method;
  if (editingNode) {
    body = { port: port };
    var protocol = editingNode.protocol || '';
    if (['vless', 'trojan', 'anytls'].indexOf(protocol) >= 0) body.sni = document.getElementById('node-form-sni').value.trim();
    if (protocol === 'shadowsocks') body.method = document.getElementById('node-form-method').value;
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

function deleteNodeConfig(id) {
  var n = findManageNode(id);
  if (!n) return;
  var confirmation = window.prompt('即将删除节点“' + n.name + '”，并清除其累计、每日和采样流量历史，此操作不可恢复。\n请输入“我已确定”以继续删除：');
  if (confirmation === null) return;
  if (confirmation.trim() !== '我已确定') { toast('验证文字不匹配，已取消删除'); return; }
  nodeRequest('/api/nodes/' + id, 'DELETE').then(function (d) {
    toast(d.warning || (d.history_cleared ? '节点及全部历史流量已删除' : '节点已删除'));
    return refreshAfterNodeMutation();
  }).catch(function (e) { if (e.message !== '未登录') toast(e.message); });
}

function showNodeLinks(id) {
  var n = findManageNode(id);
  document.getElementById('node-links-title').textContent = n ? n.name : '节点分享';
  var host = document.getElementById('node-links-list');
  host.innerHTML = '<div class="empty">正在加载分享链接…</div>';
  openDrawer('node-links-drawer');
  nodeRequest('/api/nodes/' + id + '/links', 'GET').then(function (d) {
    var html = '<div class="link-field"><small>分享链接</small><pre>' + esc(d.ipv4) + '</pre></div>';
    if (d.ipv6) html += '<div class="link-field"><small>IPv6</small><pre>' + esc(d.ipv6) + '</pre></div>';
    (d.surge || []).forEach(function (line, i) { html += '<div class="link-field"><small>Surge' + (i ? ' IPv6' : '') + '</small><pre>' + esc(line) + '</pre></div>'; });
    host.innerHTML = html;
  }).catch(function (e) {
    if (e.message !== '未登录') host.innerHTML = '<div class="err">' + esc(e.message) + '</div>';
  });
}

function bindNodeManageUI() {
  document.getElementById('node-create-open').addEventListener('click', function () { openNodeEditor(null); });
  document.getElementById('node-form-type').addEventListener('change', updateNodeFormFields);
  document.getElementById('node-form-save').addEventListener('click', saveNodeConfig);
  document.getElementById('node-form-cancel').addEventListener('click', function () { closeDrawer('node-editor-drawer'); });
  document.getElementById('node-editor-close').addEventListener('click', function () { closeDrawer('node-editor-drawer'); });
  document.getElementById('node-links-close').addEventListener('click', function () { closeDrawer('node-links-drawer'); });
  document.getElementById('node-links-done').addEventListener('click', function () { closeDrawer('node-links-drawer'); });
  document.getElementById('node-manage-list').addEventListener('click', function (e) {
    var el = e.target.closest('[data-node-links]'); if (el) { showNodeLinks(el.getAttribute('data-node-links')); return; }
    el = e.target.closest('[data-node-edit]'); if (el) { openNodeEditor(el.getAttribute('data-node-edit')); return; }
    el = e.target.closest('[data-node-delete]'); if (el) deleteNodeConfig(el.getAttribute('data-node-delete'));
  });
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
    document.querySelectorAll('.tab').forEach(function (b) { b.classList.toggle('on', b.dataset.view === name); });
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
  document.querySelectorAll('.tab').forEach(function (b) {
    b.addEventListener('click', function () { show(b.dataset.view, true); });
  });
  window.addEventListener('popstate', function () { show((location.hash || '#home').slice(1), false); });
  show(current, false);
})();

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
document.getElementById('drawer-mask').addEventListener('click', function () { closeDrawer('policy-drawer'); closeDrawer('ips-drawer'); closeDrawer('node-editor-drawer'); closeDrawer('node-links-drawer'); });
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
