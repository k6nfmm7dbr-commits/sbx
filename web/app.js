/* sbx-panel 前端 — 原生 JS + 手绘 SVG，无外部依赖
 * 反应速度优化要点：
 *  1) 拆分接口：高频轮询轻量 /api/live（速率+连接数），重的 /api/summary 只在切范围/低频时拉
 *  2) 实时数字用 requestAnimationFrame 做数值缓动，视觉上连续不跳变
 *  3) hero 迷你曲线本地维护滑动窗口，每帧平滑推进，不等下一次请求
 *  4) 只更新变化的 DOM 文本节点，避免整表 innerHTML 重建导致的闪烁
 */
'use strict';

var TOKEN = new URLSearchParams(location.search).get('token') || '';
var state = { days: 60, nodeId: null, summary: null, live: null };

var inflight = {};
function api(path, params) {
  var key = path + JSON.stringify(params || {});
  if (inflight[key]) return inflight[key];
  var u = new URL(path, location.origin);
  if (params) Object.keys(params).forEach(function (k) { u.searchParams.set(k, params[k]); });
  if (TOKEN) u.searchParams.set('token', TOKEN);
  var req = fetch(u, { cache: 'no-store' }).then(function (r) {
    if (r.status === 401) throw new Error('令牌无效，请重新登录');
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

/* ---------- 渲染：概览（低频 summary）---------- */
function renderSummary(s) {
  state.summary = s;
  setText('meta-backend', '');
  setText('meta-clock', '');
  easeTo('kpi-today-total', s.today.rx + s.today.tx, fmtBytes);
  easeTo('kpi-today-up', s.today.rx, fmtBytes);
  easeTo('kpi-today-down', s.today.tx, fmtBytes);
  easeTo('kpi-all-total', s.total.rx + s.total.tx, fmtBytes);
  easeTo('kpi-all-up', s.total.rx, fmtBytes);
  easeTo('kpi-all-down', s.total.tx, fmtBytes);
  setText('kpi-nodes', s.nodes.length);
  setText('foot-note', '');
  if (s.error) toast(s.error);
  renderNodesStatic(s);
  renderNodeSelect(s);
}

/* ---------- 渲染：实时（高频 live）---------- */
function renderLive(v) {
  state.live = v;
  var healthy = v.healthy, live = v.rate_known !== false;
  var dot = document.getElementById('health-dot');
  if (dot) dot.className = 'dot ' + (healthy ? 'ok' : 'bad');
  setText('status-txt', healthy ? '实时监控中' : (v.error ? '采集异常' : '等待采集'));
  document.getElementById('pulse').className = 'hero-pulse' + (live ? '' : ' stale');

  var rt = v.rate_total || { rx: 0, tx: 0 };
  easeTo('hero-rate', live ? rt.rx + rt.tx : 0, function (n) { return live ? fmtRate(n) : '—'; });
  easeTo('hero-up', live ? rt.rx : 0, function (n) { return live ? fmtRate(n) : '—'; });
  easeTo('hero-down', live ? rt.tx : 0, function (n) { return live ? fmtRate(n) : '—'; });
  setText('kpi-conns', typeof v.conns_total === 'number' ? v.conns_total : '—');
  setText('kpi-conns-udp', typeof v.conns_udp_total === 'number' ? v.conns_udp_total : '—');

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
    else if (kind === 'rate-up') el.textContent = live ? fmtRate(n.rate.rx) : '—';
    else if (kind === 'rate-down') el.textContent = live ? fmtRate(n.rate.tx) : '—';
  });
}

/* ---------- 节点表：静态部分（流量随 summary 重建）---------- */
function renderNodesStatic(s) {
  var tbody = document.getElementById('node-tbody'), cards = document.getElementById('node-cards');
  var nodes = s.nodes.slice();
  if (!nodes.length) {
    tbody.innerHTML = '<tr><td colspan="12" class="empty">还没有节点，先用 sbx 菜单添加一个</td></tr>';
    cards.innerHTML = '<div class="empty">还没有节点，先用 sbx 菜单添加一个</div>';
    return;
  }
  function portText(n) { return n.ports ? (n.port ? n.port + ',' + n.ports : n.ports) : (n.port || '—'); }

  tbody.innerHTML = nodes.map(function (n) {
    return '<tr>'
      + '<td><div class="node-name">' + esc(n.name) + '</div></td>'
      + '<td><span class="chip">' + esc(n.type || '—') + '</span></td>'
      + '<td class="num">' + esc(portText(n)) + '</td>'
      + '<td class="num"><b class="conns" data-node-live="' + n.id + '" data-kind="conns">—</b></td>'
      + '<td class="num"><b class="conns-udp" data-node-live="' + n.id + '" data-kind="conns_udp">—</b></td>'
      + '<td class="num live" data-node-live="' + n.id + '" data-kind="rate">—</td>'
      + '</tr>';
  }).join('');

  cards.innerHTML = nodes.map(function (n) {
    return '<div class="ncard node-panel">'
      + '<div class="ncard-top"><span class="ncard-name">' + esc(n.name) + '</span>'
      + '<span class="chip">' + esc(n.type || '—') + '</span></div>'
      + '<div class="node-status-grid">'
      + '<div><span>端口</span><b>' + esc(portText(n)) + '</b></div>'
      + '<div><span>TCP</span><b class="conns" data-node-live="' + n.id + '" data-kind="conns">—</b></div>'
      + '<div><span>UDP</span><b class="conns-udp" data-node-live="' + n.id + '" data-kind="conns_udp">—</b></div>'
      + '</div>'
      + '<div class="node-rate-grid">'
      + '<div><span class="up">↑ 上传速率</span><b class="up" data-node-live="' + n.id + '" data-kind="rate-up">—</b></div>'
      + '<div><span class="down">↓ 下载速率</span><b class="down" data-node-live="' + n.id + '" data-kind="rate-down">—</b></div>'
      + '</div>'
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
var cache = { daily: null, nodeDaily: null };

function loadSummary() { return api('/api/summary').then(renderSummary).catch(function (e) { toast(e.message); }); }
function loadLive() { return api('/api/live').then(renderLive).catch(function () {}); }

function fmtTableBytes(n) { return fmtBytes(Number(n)||0); }
function renderExcelTable(hostId, rows) {
  var host=document.getElementById(hostId); if(!host)return;
  if(!rows||!rows.length){host.innerHTML='<div class="empty">暂无数据</div>';return;}
  var html='<div class="excel-scroll"><table class="excel-table"><colgroup><col class="col-date"><col class="col-up"><col class="col-down"><col class="col-total"></colgroup><thead><tr><th>日期</th><th class="up">上传</th><th class="down">下载</th><th>合计</th></tr></thead><tbody>';
  rows.slice().reverse().forEach(function(r){html+='<tr><td>'+esc(r.day)+'</td><td class="up">'+fmtTableBytes(r.rx)+'</td><td class="down">'+fmtTableBytes(r.tx)+'</td><td><b>'+fmtTableBytes(r.rx+r.tx)+'</b></td></tr>';});
  html+='</tbody></table></div>';
  host.innerHTML=html;
}
function drawDaily() { if(cache.daily) renderExcelTable('daily-table',cache.daily); }
function drawNodeDaily() { if(cache.nodeDaily) renderExcelTable('node-daily-table',cache.nodeDaily); }
function loadDaily() { return api('/api/daily', { days: 60 }).then(function (d) { cache.daily = d.days; drawDaily(); }).catch(function (e) { toast(e.message); }); }
function loadNodeDaily() {
  if (state.nodeId == null) return Promise.resolve();
  return api('/api/daily', { days: 60, scope: 'node:' + state.nodeId })
    .then(function (d) { cache.nodeDaily = d.days; drawNodeDaily(); }).catch(function (e) { toast(e.message); });
}

/* ---------- 节点管理页 ---------- */
var manageState={key:'',nodes:[],unlocked:false};
try{manageState.key=sessionStorage.getItem('sbx-manage-key')||'';}catch(e){}
function manageFetch(path,body){
  var opt={cache:'no-store',headers:{'X-SBX-Manage-Key':manageState.key}};
  if(body){opt.method='POST';opt.headers['Content-Type']='application/json';body.manage_key=manageState.key;opt.body=JSON.stringify(body);}
  return fetch(path,opt).then(function(r){return r.json().then(function(d){if(!r.ok)throw new Error(d.error||('HTTP '+r.status));return d;});});
}
function manageMessage(text,ok){var e=document.getElementById('manage-result');if(e){e.textContent=text||'';e.style.fontWeight='700';e.style.color='#000';}}
function manageFillNode(){
  var sel=document.getElementById('manage-node'),id=Number(sel.value),n=manageState.nodes.find(function(x){return Number(x.id)===id;});if(!n)return;
  document.getElementById('manage-port').value=n.port||'';
  document.getElementById('manage-sni').value=n.sni||'';
  document.getElementById('manage-sni-box').hidden=!n.can_sni;
}
function manageRender(){
  var sel=document.getElementById('manage-node');
  sel.innerHTML=manageState.nodes.map(function(n){return '<option value="'+n.id+'">'+esc(n.name)+' · '+esc(n.type)+'</option>';}).join('');
  document.getElementById('manage-content').hidden=false;document.getElementById('manage-lock').hidden=true;
  if(manageState.nodes.length)manageFillNode();else manageMessage('暂无节点');
}
function manageUnlock(){
  var key=document.getElementById('manage-key').value.trim()||manageState.key;if(!key){document.getElementById('manage-msg').textContent='请输入管理密钥';return;}
  manageState.key=key;
  manageFetch('/api/manage/nodes').then(function(d){manageState.nodes=d.nodes||[];manageState.unlocked=true;try{sessionStorage.setItem('sbx-manage-key',key);}catch(e){}manageRender();})
  .catch(function(e){manageState.unlocked=false;document.getElementById('manage-msg').textContent=e.message;});
}
function manageSave(){
  var id=Number(document.getElementById('manage-node').value),port=Number(document.getElementById('manage-port').value);
  var n=manageState.nodes.find(function(x){return Number(x.id)===id;});if(!n)return;
  var body={id:id,port:port};if(n.can_sni)body.sni=document.getElementById('manage-sni').value.trim();
  manageMessage('正在校验并应用…');
  manageFetch('/api/manage/edit',body).then(function(d){manageState.nodes=d.nodes||[];manageRender();manageMessage('修改成功，服务与计数规则已重载');loadSummary();loadLive();})
  .catch(function(e){manageMessage('修改失败：'+e.message);});
}
function manageDelete(){
  var id=Number(document.getElementById('manage-node').value),n=manageState.nodes.find(function(x){return Number(x.id)===id;});if(!n)return;
  if(!confirm('确定删除节点「'+n.name+'」？此操作会重载 sing-box。'))return;
  var clear=document.getElementById('manage-clear-history').checked;
  manageMessage('正在删除并重载…');
  manageFetch('/api/manage/remove',{id:id,clear_history:clear}).then(function(d){manageState.nodes=d.nodes||[];manageRender();manageMessage('节点已删除'+(clear?'，历史流量已清除':'，历史流量已保留'));loadSummary();loadLive();})
  .catch(function(e){manageMessage('删除失败：'+e.message);});
}
document.getElementById('manage-unlock').addEventListener('click',manageUnlock);
document.getElementById('manage-key').addEventListener('keydown',function(e){if(e.key==='Enter')manageUnlock();});
document.getElementById('manage-node').addEventListener('change',manageFillNode);
document.getElementById('manage-save').addEventListener('click',manageSave);
document.getElementById('manage-delete').addEventListener('click',manageDelete);

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
// 节点固定按 nodes.json 添加顺序显示
document.getElementById('node-select').addEventListener('change', function (e) { state.nodeId = e.target.value; loadNodeDaily(); });
// CSV 导出入口已按用户要求从页面移除

var reflowTimer;
window.addEventListener('resize', function () {
  clearTimeout(reflowTimer);
  reflowTimer = setTimeout(function () { drawDaily(); drawNodeDaily(); }, 160);
});

/* ---------- 三页底部导航 ---------- */
(function initNavigation(){
  var valid={home:1,daily:1,node:1,manage:1}, positions={home:0,daily:0,node:0};
  var current=(location.hash||'#home').slice(1); if(!valid[current])current='home';
  function show(name,push){
    if(!valid[name])name='home'; positions[current]=window.scrollY||0; current=name;
    document.querySelectorAll('.app-view').forEach(function(v){v.classList.toggle('on',v.id==='view-'+name);});
    document.querySelectorAll('.nav-btn').forEach(function(b){b.classList.toggle('on',b.dataset.view===name);});
    if(push && location.hash!=='#'+name)history.pushState(null,'','#'+name);
    requestAnimationFrame(function(){window.scrollTo(0,positions[name]||0);});
    if(name==='daily'&&!cache.daily)loadDaily();
    if(name==='node'&&!cache.nodeDaily)loadNodeDaily();
    if(name==='manage'&&!manageState.unlocked&&manageState.key){document.getElementById('manage-key').value=manageState.key;manageUnlock();}
  }
  document.querySelectorAll('.nav-btn').forEach(function(b){b.addEventListener('click',function(){show(b.dataset.view,true);});});
  window.addEventListener('popstate',function(){show((location.hash||'#home').slice(1),false);});
  show(current,false);
})();

/* ---------- 启动与轮询 ---------- */
setInterval(tickEase, 40);               // 数字缓动循环（setInterval 比 rAF 在后台更可靠）

loadSummary().then(function () { loadLive(); loadDaily(); });

// 实时数据：2 秒一次（轻量 /api/live）
setInterval(function () { if (!document.hidden) loadLive(); }, 2000);
// 概览+节点表：8 秒一次（较重的 /api/summary）
setInterval(function () { if (!document.hidden) loadSummary(); }, 8000);
// 每日流量图 + 单节点明细：60 秒
setInterval(function () {
  if (document.hidden) return;
  loadDaily(); loadNodeDaily();
}, 60000);
// 页面重新可见时立即刷新一次
document.addEventListener('visibilitychange', function () {
  if (!document.hidden) { loadLive(); loadSummary(); }
});
