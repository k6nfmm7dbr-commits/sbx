# SBX v2.8.0 迁移前完整审计（Go 化依据）

> 本文是 Go 重构的**行为基线**。所有结论来自对现有代码的逐行阅读，不含猜测。
> 迁移原则：正确性 > 兼容性 > 稳定性 > 可维护性 > 资源占用 > 性能。
>
> **历史存档说明**：本文档记录 v2.8.0 的旧 Python 实现（`src/panel.py`、
> `src/nodes_tool.py`）行为，作为 Go 重构的行为依据。这些 Python 文件已随
> 全 Go 化彻底移除，本文仅保留作迁移依据与数据口径溯源，不再描述现行实现。

## 1. panel.py 功能全景（src/panel.py, 1290 行）

| 模块 | 行为 |
|---|---|
| 配置 | `SBX_CONF`(默认 `$SBX_DIR/panel.json`) 覆盖内置默认值；键: db/nodes_file/nft_conf/ipt_script/web_root/backend/listen/port/token/interval(2)/tz(Asia/Shanghai) |
| SQLite | 库文件 `traffic.db`；WAL（失败静默退回）；busy_timeout=30000; timeout=30; isolation_level=None（自动提交+手工 BEGIN IMMEDIATE） |
| 表结构 | meta(k,v)、counter_state(name,last_bytes,last_pkts,updated_at)、daily(day,scope,rx,tx,rx_pkts,tx_pkts PK(day,scope))、totals(scope PK,...)、samples(ts,scope,rx,tx,duration_ms,valid PK(ts,scope)) + idx_samples_ts |
| 迁移 | samples 缺 duration_ms/valid 列则 ALTER ADD；`UPDATE samples SET valid=0 WHERE duration_ms<=0`；进程内只跑一次 |
| 后端检测 | backend=auto → which(nft) 且 `nft list tables` rc==0 → nft；否则 which(iptables) → iptables；都无 → 默认 nft |
| nft 读取 | `nft -j list counters table inet sbx_traffic`；rc!=0 时 err 含 "No such file or directory"/"does not exist" 或 rc==1 → LookupError（可自愈），否则 RuntimeError；JSON `{nftables:[{counter:{name,bytes,packets}}]}`；空表也 LookupError |
| iptables 读取 | 对 iptables(v4)/ip6tables(v6) 各读 SBX_IN/SBX_OUT：`<bin> -w -nvxL <chain>`；行正则 `^\s*(\d+)\s+(\d+)\s+.*?/\*\s*(sbx:[A-Za-z0-9_:.-]+)\s*\*/`（pkts,g1 bytes,g2 tag,g3）；tag 按 `@v4/@v6` 聚合；epoch 标记存 (0,0)；任一 family 成功即 ok，全失败 LookupError |
| 自愈 | LookupError 时 30 秒节流 repair()：nft→`nft -f nft.conf`；iptables→`sh iptables.sh apply` |
| 计数器名 | `^sbx[_:](n(\d+)|sys)[_:]([io])$` → scope=`node:<id>`/`system`，方向 i=rx(收=上传) o=tx(发=下载)；世代标记 `^sbx[_:]epoch[_:](\d+)$` |
| tick 差分 | ts_ms=time_ns/1e6；样本秒 ts=max(wall, last_sample_ts+1) 防回拨覆盖；fresh_ruleset=(epoch 存在且与 meta.epoch 字符串不同)→state 清空；expected_ms=interval*1000，valid 窗口 [max(200,e//4), e*3+500]ms |
| delta 规则 | 首见: db=bytes 全量入账、速率无效；cur<last(归零): db=cur 补记、reset_hits++、不写速率；正常: db=cur-last、dp=max(0,pkts-last)、窗口内 valid |
| slot 聚合 | 每 scope 汇总 rx/tx/pkts、duration_ms 取 max、valid 取 AND；最终需 seen_rx&&seen_tx&&duration>0 才写样本（零字节也写，让速率回落） |
| 提交事务 | BEGIN IMMEDIATE：daily/totals upsert 累加（仅非零增量）→ samples upsert(MAX(duration_ms)) → **DELETE counter_state 全表重写**(updated_at=ms) → meta epoch upsert(str) → DELETE samples ts<now-120 → COMMIT；异常 ROLLBACK |
| 兼容 | updated_at 旧版秒级(<1e12) 读取时 ×1000 |
| run 循环 | 绝对周期调度（deadline 单调累加，跨周期不对齐补跑）；LookupError→repair 节流；其它异常 log；成功清 last_error |
| 连接数缓存 | 每轮 tick 后 count_conns(load_nodes()) 存 last_conns，API 直接读；失败置 {} |
| q_daily | scope 有值: WHERE scope=? ORDER BY day DESC LIMIT ? 再反转（旧→新）；无值: SUM over scope LIKE 'node:%' GROUP BY day 同序 |
| q_rate | window=max(8,iv*4)；latest=MAX(ts) of valid&dur>0 样本；now-latest>max(15,iv*4) → {}（过期视为无速率）；按 scope SUM(rx,tx,duration_ms)，rate=bytes/max(0.001,dur/1000) |
| summary | now/day/tz(conf 或本地时区名)/interval/backend/healthy/error/last_sample/nodes[](id,name(type兜底),type,port,today4,total4,rate{rx,tx},conns_tcp,conns_udp)/today/total(**仅节点聚合，不含 system**)/system_today/system_total/rate_total(仅 node: 前缀)/rate_known(bool(rates))/conns_total(tcp 求和真值)/conns_udp_total |
| live | 轻量：now/healthy/error/last_sample/interval/rate_known/rate_total/conns_total/conns_udp_total/nodes[{id,rate,conns_tcp,conns_udp}] |
| HTTP | ThreadingHTTPServer；Server 头 sbx-panel/1.0；全部响应带 Cache-Control: no-store + X-Content-Type-Options: nosniff；JSON ensure_ascii=False 紧凑分隔符 |
| 路由 | GET /healthz(免鉴权 {"ok":true})；/ 与 /index.html(未鉴权→login.html 内容 200；已鉴权→index.html)；/app.js /style.css(免鉴权)；POST /login(form token，比对成功→302 / + Set-Cookie sbx_token=urlquote; Path=/; Max-Age=604800; HttpOnly; SameSite=Strict，失败→302 /login?error=1，其它路径 404)；/api/* 未鉴权→401 {"error":"unauthorized"} |
| 鉴权顺序 | Bearer 头 > ?token= > Cookie sbx_token(urldecode)；hmac.compare_digest；token 为空 = 完全开放 |
| API | /api/summary、/api/live、/api/daily?days(默认30,钳[1,365],非法int→500)&scope(空→None)、/api/nodes({"nodes":原文数组})、/api/export(CSV day,scope,rx_bytes,tx_bytes,rx_pkts,tx_pkts ORDER BY day,scope；text/csv + attachment; filename=sbx-traffic.csv)、未知 api→404 {"error":"not found"}；异常→500 {"error":str} |
| CLI | serve/once/show/daily[N]/reset[scope]/rules/apply/clear/selftest/config-get/config-set(rules 写 nft.conf+iptables.sh chmod755；apply=先强制采样(容错)再重建规则再应用，nft 失败退 iptables；clear=采样容错+nft delete table+ipt clear；selftest 读两次间隔3s 打印增量) |
| human | "%.2f B/KiB/MiB/GiB/TiB/PiB" 1024 进制 |
| 时区 | tz 名 → zoneinfo；失败解析 UTC±HH:MM 正则 → 固定偏移回退表(asia/shanghai=8 等) → 兜底 UTC+8；local/system → 本地时间 |

## 2. nodes_tool.py 功能全景（489 行）

- 常量：APP_DIR(SBX_DIR)、SB_CONF(SBX_SB_CONF)、NODES_JSON、STATE_JSON、CERT_DIR(certs)、TAG_PREFIX=`sbx-n`、TYPES=(vless,shadowsocks,trojan,anytls)
- next_id：state.json next_node_id 与已有最大 id 取 max+1，**单调永不回收**，立即持久化 state.json
- build_inbound：base {type,tag=sbx-n<id>,listen:"::",listen_port}；
  - vless: users[{name:"u",uuid,flow?}] + tls{enabled,server_name:sni,reality{enabled,handshake{server:sni,server_port:443},private_key,short_id:[sid]}}
  - shadowsocks: method+password
  - trojan/anytls: users[{name:"u",password}] + tls{enabled,server_name,certificate_path,key_path}
- rebuild_config：读 SB_CONF，保留 tag 非 sbx-n* 的 inbound，追加生成节点；setdefault outbounds=[direct]、route.final=direct
- 候选流：add/edit/remove/sync 都写 `SB_CONF.candidate` + `nodes.json.candidate` 并打印 JSON（{"id"/"changed"/"candidate"/"nodes_candidate"}）；commit 提升 nodes candidate；rollback 删除两候选。sing-box check 由外层 shell 做
- edit：--port 校验 1-65535+查重(排除自身)；--sni 仅 vless/trojan/anytls；changed 文案 "端口→N"/"SNI→X"；无变更报错
- 分享链接：
  - vless: `vless://uuid@host:port?`urlencode(encryption=none,security=reality,type=tcp,sni,fp=chrome,pbk,sid[,flow])`#`quote(name)
  - ss: userinfo=urlsafe_b64(method:password) 去 `=`，无 query
  - trojan: password 经 quote(safe='/') + urlencode(security=tls,sni,allowInsecure=1,type=tcp)
  - anytls: quote(password) + urlencode(sni,insecure=1)
  - host 含 ":" 加方括号；name 空→type；links 输出 `### name (type, 端口 P)` + 链接 (+`# IPv6:` 段，标签后缀 `-IPv6`)
  - subscription: 链接按行 join 后标准 base64
- 其它命令：list(--json indent=2)、count、last(空输出当无节点)、info(id→"type\tsni\tport")、port-used(exit0=占用打印 used by node X)、set-host/set-host6(空清除)/get-host/get-host6

## 3. sbx.sh 与两者的关系

- 所有 JSON 操作走 `py_json()`=python3 nodes_tool.py；面板操作走 `python3 panel.py <cmd>`
- add 流程：菜单收集参数 → py_json add（生成候选）→ `sing-box check -c candidate` → 备份 conf+nodes.json → mv candidate → py_json commit → restart sing-box → fw_apply(panel.py apply) → restart sbx-panel
- 卸载/清理：fw_clear 先停面板再 clear + 双重 `nft delete table inet sbx_traffic`
- 服务单元：systemd 三个单元 sing-box/sbx-firewall(oneshot, ExecStart=panel.py apply, ExecStop=clear)/sbx-panel；OpenRC 对应 init.d 三件套
- 升级：下载新 sbx.sh → bash -n + grep write_payload 校验 → 版本+SHA256 比较 → 替换本体 → --apply-update：write_payload 覆盖资源 → py_compile+web 非空校验失败回滚 → install_self/setup_services/py_json sync+check+commit → 重启全家
- 架构映射 amd64/arm64/armv7/armv6/386/s390x/riscv64；Alpine 用 musl 后缀包
- 依赖安装含 python3（JSON 处理全靠它）——迁移后移除

## 4. Web 前端依赖（web/*, 不改动）

- app.js 只调：GET /api/live(2s 轮询)、/api/summary(8s)、/api/daily?days=60[&scope=node:N](60s)；401→跳 /login
- 使用字段：today.rx/tx、total、nodes[].id/name/type/port/rate/conns_tcp/conns_udp、healthy/error/rate_known/rate_total/conns_total/conns_udp_total、daily[].day/rx/tx —— 全部在上述 API 内

## 5. 数据口径（不得改变）

- rx=服务器收到(IP层)=用户上传；tx=服务器发出=用户下载；字节含包头，比应用层高约 2%~5%
- daily/totals 记确定增量；samples 仅可信时长样本且保留 ~120s；跨天 UTC+8
- epoch/重启/归零衔接语义见 §1 tick 差分——这是统计正确性的核心，逐条复刻

## 6. 已知问题/风险（迁移中处理或记录）

1. Python config-set 会把全部默认键写回 panel.json（保持兼容复刻）
2. parse_qs 丢弃空值参数（`?token=` 视为未提供）——Go 需显式模拟
3. iptables 后端插 INPUT/OUTPUT 首位会计入随后被丢弃的包（nft prio300 不计）——口径差异既有，保持
4. uninstall 的 fw_clear 偶发残留 nft 表（shell 双删兜底已存在）
5. armv6：Go 本身支持 linux/arm，但纯 Go SQLite(modernc libc) 需要 GOARM≥7 → 见 FUTURE_IMPROVEMENTS
