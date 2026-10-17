package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
	"github.com/k6nfmm7dbr-commits/sbx/internal/ruleset"
)

// CLI 是 nodes_tool.py 的 Go 版命令入口。stdout/stderr 可注入便于测试。
type CLI struct {
	Store  *Store
	Stdout io.Writer
	Stderr io.Writer
}

const (
	exitOK    = 0
	exitErr   = 1
	exitUsage = 2
)

// Run 执行 node 子命令，返回进程退出码。
func (c *CLI) Run(args []string) int { return c.runCommand(args, true) }

// RunUnlocked 执行子命令而不获取 CLI flock。调用方必须已经持有 SBX_LOCK；
// HTTP 节点编辑事务会用它把 prepare/commit/restart/rollback 包在同一锁内。
func (c *CLI) RunUnlocked(args []string) int { return c.runCommand(args, false) }

func (c *CLI) runCommand(args []string, lockMutations bool) int {
	mutate := func(fn func() int) int {
		if lockMutations {
			return c.mutationLocked(fn)
		}
		return fn()
	}
	if len(args) == 0 {
		return c.usageError("需要子命令")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "add":
		return mutate(func() int { return c.cmdAdd(rest) })
	case "add-batch":
		return mutate(func() int { return c.cmdAddBatch(rest) })
	case "remove-batch":
		return mutate(func() int { return c.cmdRemoveBatch(rest) })
	case "remove":
		return mutate(func() int { return c.cmdRemove(rest) })
	case "edit":
		return mutate(func() int { return c.cmdEdit(rest) })
	case "sync":
		return mutate(c.cmdSync)
	case "rulesets":
		return mutate(func() int { return c.cmdRuleSets(rest) })
	case "commit":
		return mutate(c.cmdCommit)
	case "rollback":
		return mutate(c.cmdRollback)
	case "ss2022-key":
		return c.cmdSS2022Key(rest)
	case "list":
		return c.cmdList(rest)
	case "count":
		return c.cmdCount()
	case "last":
		return c.cmdLast()
	case "info":
		return c.cmdInfo(rest)
	case "links":
		return c.cmdLinks(rest)
	case "port-used":
		return c.cmdPortUsed(rest)
	case "set-host":
		return c.cmdSetHost(rest)
	case "get-host":
		fmt.Fprintln(c.Stdout, c.Store.ShareHost())
		return exitOK
	case "set-host6":
		return c.cmdSetHost6(rest)
	case "get-host6":
		fmt.Fprintln(c.Stdout, c.Store.ShareHost6())
		return exitOK
	default:
		return c.usageError("未知子命令: " + cmd)
	}
}

func (c *CLI) usageError(msg string) int {
	fmt.Fprintln(c.Stderr, msg)
	fmt.Fprintln(c.Stderr, "用法: sbx-core node <add|add-batch|edit|remove|remove-batch|list|count|last|info|links|sync|commit|rollback|rulesets|ss2022-key|port-used|set-host|get-host|set-host6|get-host6>")
	return exitUsage
}

// mutationLocked 在跨进程排他锁内执行节点变更。锁文件与 sbx.sh 的 sbx_lock 共用
// （SBX_LOCK，默认 /run/lock/sbx.lock）：经菜单操作时 shell 已持锁并导出
// SBX_LOCK_HELD=1，这里跳过自锁避免自死锁；绕过菜单直接跑 sbx-core node 时，
// 这里补上锁，缩小 lost-update / candidate 串写的窗口。
//
// 锁不可用（目录不可写等）时 fail-open 放行——与修复前行为一致，绝不因锁故障
// 阻断本就单机串行的手工操作。
func (c *CLI) mutationLocked(fn func() int) int {
	if os.Getenv("SBX_LOCK_HELD") != "" {
		return fn()
	}
	path := os.Getenv("SBX_LOCK")
	if path == "" {
		path = "/run/lock/sbx.lock"
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fn() // 锁不可用不阻断（fail-open，与历史行为一致）
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fn()
	}
	return fn()
}

func (c *CLI) fail(msg string) int {
	fmt.Fprintln(c.Stderr, msg)
	return exitErr
}

// ---- 参数解析 ------------------------------------------------------------

type parsed struct {
	positional []string
	flags      map[string]string // 不含 "--" 前缀
}

func parseArgs(args []string, known map[string]bool) (*parsed, error) {
	p := &parsed{flags: map[string]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			name := strings.TrimPrefix(a, "--")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				key, val := name[:eq], name[eq+1:]
				if !known[key] {
					return nil, fmt.Errorf("未知参数: --%s", key)
				}
				p.flags[key] = val
				continue
			}
			if !known[name] {
				return nil, fmt.Errorf("未知参数: --%s", name)
			}
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				p.flags[name] = args[i+1]
				i++
			} else {
				p.flags[name] = ""
			}
			continue
		}
		p.positional = append(p.positional, a)
	}
	return p, nil
}

func (p *parsed) has(key string) bool {
	_, ok := p.flags[key]
	return ok
}

func (c *CLI) outJSON(v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return c.fail(err.Error())
	}
	fmt.Fprintln(c.Stdout, string(data))
	return exitOK
}

// ---- add -----------------------------------------------------------------

var addKnown = map[string]bool{
	"port": true, "name": true, "uuid": true, "password": true, "method": true,
	"sni": true, "flow": true, "private-key": true, "public-key": true, "short-id": true,
	"version": true, "psk": true, "obfs-mode": true, "mode": true,
	// allow：逗号分隔的规则集名（"只允许访问这些服务"）。空值表示不限制。
	"allow": true,
}

func (c *CLI) cmdAdd(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		return c.usageError("缺少节点类型")
	}
	typ := args[0]
	if !ValidType(typ) {
		return c.usageError("不支持的节点类型: " + typ)
	}
	p, err := parseArgs(args[1:], addKnown)
	if err != nil {
		return c.usageError(err.Error())
	}
	portStr, ok := p.flags["port"]
	if !ok {
		return c.usageError("缺少 --port")
	}

	// 修改类操作必须严格读取：损坏文件拒绝继续，绝不静默覆盖
	list, err := LoadToolNodesStrict(c.Store.NodesPath())
	if err != nil {
		return c.fail(err.Error())
	}
	nid, err := NextID(c.Store, list) // 先持久化游标（对齐旧实现顺序）
	if err != nil {
		return c.fail(err.Error())
	}
	node := Node{"id": json.Number(strconv.FormatInt(nid, 10)), "type": typ}

	portNum, perr := strconv.ParseInt(strings.TrimSpace(portStr), 10, 64)
	if perr != nil {
		return c.fail("invalid literal for int() with base 10: '" + portStr + "'")
	}
	if portNum < 1 || portNum > 65535 {
		return c.fail("端口需在 1-65535")
	}
	node["port"] = json.Number(strconv.FormatInt(portNum, 10))

	if name := p.flags["name"]; name != "" {
		node["name"] = name
	} else {
		node["name"] = fmt.Sprintf("%s-%d", typ, nid)
	}
	for _, key := range []string{"uuid", "password", "method", "sni", "flow",
		"private_key", "public_key", "short_id", "psk", "obfs_mode", "mode"} {
		flag := strings.ReplaceAll(key, "_", "-")
		if v := p.flags[flag]; v != "" {
			node[key] = v
		}
	}
	if typ == "snell" {
		// Snell 版本字段（json.Number）；非法版本拒绝
		verStr := p.flags["version"]
		if verStr == "" {
			return c.fail("Snell 需要 --version=5|6")
		}
		ver, verr := strconv.ParseInt(verStr, 10, 64)
		if verr != nil || (ver != 5 && ver != 6) {
			return c.fail("Snell version 必须是 5 或 6")
		}
		node["version"] = json.Number(strconv.FormatInt(ver, 10))
		if Str(node, "psk") == "" {
			return c.fail("Snell 需要 --psk")
		}
	}
	if typ == "trojan" || typ == "anytls" {
		node["cert"] = c.Store.CertDir() + "/cert.pem"
		node["key"] = c.Store.CertDir() + "/key.pem"
	}
	if raw, has := p.flags["allow"]; has {
		names, aerr := parseAllowFlag(raw)
		if aerr != nil {
			return c.usageError(aerr.Error())
		}
		if rc := c.requireRuleSetsCached(names); rc != exitOK {
			return rc
		}
		if len(names) > 0 {
			node[AllowKey] = allowToAny(names)
		}
	}

	list = append(list, node)
	cand, nodesCand, rc := c.writeCandidates(list)
	if rc != exitOK {
		return rc
	}
	return c.outJSON(map[string]any{
		"id":              node["id"],
		"candidate":       cand,
		"nodes_candidate": nodesCand,
	})
}

// ---- remove / edit / sync ------------------------------------------------

func findByID(list []Node, id string) int {
	for i, n := range list {
		if IDString(n) == id || Str(n, "id") == id {
			return i
		}
	}
	return -1
}

// strictLoadForMutation 供 sync 使用；编辑/删除使用 repairLoadForMutation。
func (c *CLI) strictLoadForMutation() ([]Node, int) {
	list, err := LoadToolNodesStrict(c.Store.NodesPath())
	if err != nil {
		return nil, c.fail(err.Error())
	}
	return list, exitOK
}

// repairLoadForMutation 仅供 edit/remove 使用：允许读取历史重复端口状态，以便通过
// 编辑端口或删除冲突项修复；writeCandidates 仍执行完整校验，未修复则拒绝写入。
func (c *CLI) repairLoadForMutation() ([]Node, int) {
	list, err := LoadToolNodesRepair(c.Store.NodesPath())
	if err != nil {
		return nil, c.fail(err.Error())
	}
	return list, exitOK
}

func (c *CLI) writeCandidates(list []Node) (string, string, int) {
	// 所有 mutation 共享的兜底校验：最终节点列表必须语义合法，
	// 防止 add/edit/remove/sync 写入损坏节点。
	if err := validateNodes(list); err != nil {
		return "", "", c.fail(err.Error())
	}
	cfg, err := RebuildConfig(c.Store, list)
	if err != nil {
		return "", "", c.fail(err.Error())
	}
	cand, err := WriteCandidate(c.Store, cfg)
	if err != nil {
		return "", "", c.fail(err.Error())
	}
	nodesCand, err := WriteNodesCandidate(c.Store, list)
	if err != nil {
		return "", "", c.fail(err.Error())
	}
	return cand, nodesCand, exitOK
}

// batchNodeSpec 是 add-batch 的单个节点规格。
// 凭据由面板生成后传入，CLI 只做校验、id 分配与候选生成——
// 与单节点 add 的职责划分一致（密钥生成在 API 层）。
type BatchNodeSpec struct {
	Type       string `json:"type"`
	Name       string `json:"name"`
	Port       int64  `json:"port"`
	Method     string `json:"method,omitempty"`
	Password   string `json:"password,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	SNI        string `json:"sni,omitempty"`
	Flow       string `json:"flow,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	PublicKey  string `json:"public_key,omitempty"`
	ShortID    string `json:"short_id,omitempty"`
	PSK        string `json:"psk,omitempty"`
	Version    int64  `json:"version,omitempty"`
}

// cmdAddBatch 一次变更里添加多个节点：只生成一份候选、跑一次 sing-box check、
// 重启一次 sing-box。N 次逐个 add 会让员工断连 N 次。
func (c *CLI) cmdAddBatch(args []string) int {
	specJSON := ""
	for _, a := range args {
		if strings.HasPrefix(a, "--spec=") {
			specJSON = strings.TrimPrefix(a, "--spec=")
		}
	}
	if specJSON == "" {
		return c.usageError("用法: add-batch --spec='<json 数组>'")
	}
	dec := json.NewDecoder(strings.NewReader(specJSON))
	dec.UseNumber()
	var specs []BatchNodeSpec
	if err := dec.Decode(&specs); err != nil {
		return c.fail("--spec 不是合法 JSON 数组: " + err.Error())
	}
	if len(specs) < 1 || len(specs) > 50 {
		return c.fail("批量数量需在 1-50")
	}
	list, rc := c.repairLoadForMutation()
	if rc != exitOK {
		return rc
	}
	// 已占用端口集合：既有节点的全部监听端口（含多端口节点）。
	used := map[int64]bool{}
	for _, n := range list {
		for _, pr := range ParsePorts(n) {
			for p := pr[0]; p <= pr[1]; p++ {
				used[p] = true
			}
		}
	}
	created := make([]map[string]any, 0, len(specs))
	for i, sp := range specs {
		if !ValidType(sp.Type) {
			return c.fail(fmt.Sprintf("第 %d 个节点类型不支持: %s", i+1, sp.Type))
		}
		if sp.Port < 1 || sp.Port > 65535 {
			return c.fail(fmt.Sprintf("第 %d 个节点端口无效: %d", i+1, sp.Port))
		}
		if used[sp.Port] {
			return c.fail(fmt.Sprintf("端口 %d 已被占用（第 %d 个节点）", sp.Port, i+1))
		}
		nid, err := NextID(c.Store, list)
		if err != nil {
			return c.fail("分配节点 id 失败: " + err.Error())
		}
		node := Node{
			"id":   json.Number(strconv.FormatInt(nid, 10)),
			"type": sp.Type, "port": json.Number(strconv.FormatInt(sp.Port, 10)),
		}
		if name := strings.TrimSpace(sp.Name); name != "" {
			node["name"] = name
		} else {
			node["name"] = fmt.Sprintf("%s-%d", sp.Type, nid)
		}
		switch sp.Type {
		case "vless":
			if sp.UUID == "" || sp.PrivateKey == "" || sp.PublicKey == "" || sp.ShortID == "" {
				return c.fail(fmt.Sprintf("第 %d 个节点缺少 Reality 凭据", i+1))
			}
			node["uuid"] = sp.UUID
			node["sni"] = sp.SNI
			node["flow"] = sp.Flow
			node["private_key"] = sp.PrivateKey
			node["public_key"] = sp.PublicKey
			node["short_id"] = sp.ShortID
		case "shadowsocks":
			if sp.Password == "" {
				return c.fail(fmt.Sprintf("第 %d 个节点缺少密码", i+1))
			}
			method := sp.Method
			if method == "" {
				method = SS2022Method128
			}
			node["method"] = method
			node["password"] = sp.Password
		case "trojan", "anytls":
			if sp.Password == "" {
				return c.fail(fmt.Sprintf("第 %d 个节点缺少密码", i+1))
			}
			node["sni"] = sp.SNI
			node["password"] = sp.Password
			node["cert"] = c.Store.CertDir() + "/cert.pem"
			node["key"] = c.Store.CertDir() + "/key.pem"
		case "snell":
			if sp.PSK == "" || (sp.Version != 5 && sp.Version != 6) {
				return c.fail(fmt.Sprintf("第 %d 个节点 Snell 参数无效", i+1))
			}
			node["psk"] = sp.PSK
			node["version"] = json.Number(strconv.FormatInt(sp.Version, 10))
		}
		list = append(list, node)
		used[sp.Port] = true
		created = append(created, map[string]any{
			"id": json.Number(strconv.FormatInt(nid, 10)), "name": node["name"],
			"port": json.Number(strconv.FormatInt(sp.Port, 10)),
		})
	}
	cand, nodesCand, rc := c.writeCandidates(list)
	if rc != exitOK {
		return rc
	}
	return c.outJSON(map[string]any{
		"candidate": cand, "nodes_candidate": nodesCand, "created": created,
	})
}

// cmdRemoveBatch 一次变更里删除多个节点。任何一个 id 不存在都整体拒绝：
// 批量操作要么全成要么不动，不做部分成功——那会让用户对着结果猜哪些删了。
func (c *CLI) cmdRemoveBatch(args []string) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return c.usageError("用法: remove-batch <id,id,...>")
	}
	ids := strings.Split(args[0], ",")
	list, rc := c.repairLoadForMutation()
	if rc != exitOK {
		return rc
	}
	kill := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return c.fail("存在空 id")
		}
		if !strings.HasPrefix(id, "node:") && !isValidNodeID(id) {
			return c.fail("无效节点 id: " + id)
		}
		plain := strings.TrimPrefix(id, "node:")
		if findByID(list, plain) < 0 {
			return c.fail(fmt.Sprintf("未找到节点 id=%s，整体未改动", plain))
		}
		if kill[plain] {
			return c.fail("重复的节点 id: " + plain)
		}
		kill[plain] = true
	}
	keep := make([]Node, 0, len(list))
	for _, n := range list {
		if !kill[IDString(n)] {
			keep = append(keep, n)
		}
	}
	if len(keep) == len(list) {
		return c.fail("没有任何节点被删除")
	}
	cand, nodesCand, rc := c.writeCandidates(keep)
	if rc != exitOK {
		return rc
	}
	return c.outJSON(map[string]any{"candidate": cand, "nodes_candidate": nodesCand, "removed": len(kill)})
}

func isValidNodeID(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func (c *CLI) cmdRemove(args []string) int {
	p, err := parseArgs(args, map[string]bool{})
	if err != nil {
		return c.usageError(err.Error())
	}
	if len(p.positional) != 1 {
		return c.usageError("用法: remove <id>")
	}
	list, rc := c.repairLoadForMutation()
	if rc != exitOK {
		return rc
	}
	idx := findByID(list, p.positional[0])
	if idx < 0 {
		return c.fail(fmt.Sprintf("未找到节点 id=%s", p.positional[0]))
	}
	keep := append(append([]Node{}, list[:idx]...), list[idx+1:]...)
	cand, nodesCand, rc := c.writeCandidates(keep)
	if rc != exitOK {
		return rc
	}
	return c.outJSON(map[string]any{"candidate": cand, "nodes_candidate": nodesCand})
}

// SniTypes 是允许修改 SNI 的类型（reality + 自签证书类）。
var sniTypes = map[string]bool{"vless": true, "trojan": true, "anytls": true}

var editKnown = map[string]bool{"port": true, "sni": true, "method": true, "psk": true, "allow": true}

// parseAllowFlag 解析 --allow=Google,TikTok 形式的白名单参数。
//
// 名字必须是内置清单里的合法项（ruleset.Validate 负责），因此拼进配置里的
// 只有受控字符串，不存在把任意内容注入 sing-box 配置的可能。
// 空值表示解除限制。
func parseAllowFlag(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	return ruleset.Validate(strings.Split(raw, ","))
}

// allowToAny 把规则集名转成可直接写入 nodes.json 的 JSON 数组。
func allowToAny(names []string) []any {
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

// requireRuleSetsCached 确认白名单用到的规则集已在本地就绪。
//
// CLI 刻意不做网络下载（那是面板的职责，也便于在无外网环境下用 CLI 管理节点），
// 但必须在这里拦住：候选配置引用的是本地规则集路径，文件缺失同样会让 sing-box
// 起不来。提前给出可操作的提示，比让用户看到「sing-box 配置校验失败」有用得多。
//
// 手动放入的 <Name>.list 会在这里现场转换——那是本地操作，不需要网络，
// 所以「离线导入 + CLI 保存节点」这条路是通的。
func (c *CLI) requireRuleSetsCached(names []string) int {
	if len(names) == 0 {
		return exitOK
	}
	dir := ruleset.Dir(c.Store.SBConf)
	var missing []string
	for _, n := range names {
		if ruleset.Cached(dir, n) {
			continue
		}
		if _, err := ruleset.CompileManual(context.Background(), dir, n); err != nil {
			return c.fail(fmt.Sprintf("规则集 %s 转换失败：%v", n, err))
		}
		if !ruleset.Cached(dir, n) {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return exitOK
	}
	return c.fail(fmt.Sprintf(
		"规则集 %s 尚未缓存；请在面板中配置（会自动下载并转换），或手动把 %s.list 放入 %s",
		strings.Join(missing, ", "), missing[0], dir))
}

func (c *CLI) cmdEdit(args []string) int {
	p, err := parseArgs(args, editKnown)
	if err != nil {
		return c.usageError(err.Error())
	}
	if len(p.positional) != 1 {
		return c.usageError("用法: edit <id> [--port N] [--sni DOMAIN] [--allow A,B]")
	}
	id := p.positional[0]
	list, rc := c.repairLoadForMutation()
	if rc != exitOK {
		return rc
	}
	idx := findByID(list, id)
	if idx < 0 {
		return c.fail(fmt.Sprintf("未找到节点 id=%s", id))
	}
	target := list[idx]

	var changed []string
	if portStr, has := p.flags["port"]; has && portStr != "" {
		newp, perr := strconv.ParseInt(strings.TrimSpace(portStr), 10, 64)
		if perr != nil {
			newp = 0
		}
		if newp < 1 || newp > 65535 {
			return c.fail("端口需在 1-65535")
		}
		for _, n := range list {
			if IDString(n) == IDString(target) {
				continue
			}
			if existing, eerr := toInt(n["port"]); eerr == nil && existing == newp {
				return c.fail(fmt.Sprintf("端口 %d 已被节点 %s 使用", newp, IDString(n)))
			}
		}
		target["port"] = json.Number(strconv.FormatInt(newp, 10))
		changed = append(changed, fmt.Sprintf("端口→%d", newp))
	}
	if sni, has := p.flags["sni"]; has && sni != "" {
		t := Str(target, "type")
		if !sniTypes[t] {
			return c.fail(fmt.Sprintf("%s 类型节点没有 SNI 可改", t))
		}
		target["sni"] = sni
		changed = append(changed, "SNI→"+sni)
	}
	if method, has := p.flags["method"]; has && method != "" {
		t := Str(target, "type")
		if t != "shadowsocks" {
			return c.fail(fmt.Sprintf("%s 类型节点没有加密算法可改", t))
		}
		if !IsSS2022Method(method) {
			return c.fail("未知的 Shadowsocks 2022 method: " + method)
		}
		old := Str(target, "method")
		if method != old {
			// 算法切换：旧 key 长度不再符合新 method，必须重新生成
			newPw, gerr := GenerateSS2022Password(method)
			if gerr != nil {
				return c.fail(gerr.Error())
			}
			target["method"] = method
			target["password"] = newPw
			changed = append(changed, "加密算法→"+method)
		}
		// method 相同：保留原 password，无需重生成
	}
	if psk, has := p.flags["psk"]; has && psk != "" {
		t := Str(target, "type")
		if t != "snell" {
			return c.fail(fmt.Sprintf("%s 类型节点没有 PSK 可改", t))
		}
		// v6 PSK 需满足 12~255 bytes（sing-box 以 []byte(psk) 计）
		if len([]byte(psk)) < 12 || len([]byte(psk)) > 255 {
			return c.fail("Snell PSK 必须为 12~255 字节")
		}
		target["psk"] = psk
		changed = append(changed, "PSK 已更新")
	}
	if raw, has := p.flags["allow"]; has {
		names, aerr := parseAllowFlag(raw)
		if aerr != nil {
			return c.usageError(aerr.Error())
		}
		_, existed := target[AllowKey]
		switch {
		case len(names) == 0 && existed:
			delete(target, AllowKey)
			changed = append(changed, "已解除访问限制")
		case len(names) == 0:
			// 本来就没有限制，空值等于没改——不记入 changed，让它走"未指定修改"分支
		default:
			if rc := c.requireRuleSetsCached(names); rc != exitOK {
				return rc
			}
			target[AllowKey] = allowToAny(names)
			changed = append(changed, "访问限制→"+strings.Join(names, ","))
		}
	}
	if len(changed) == 0 {
		return c.fail("未指定要修改的内容（--port / --sni / --method / --psk / --allow）")
	}

	cand, nodesCand, rc := c.writeCandidates(list)
	if rc != exitOK {
		return rc
	}
	return c.outJSON(map[string]any{
		"id": target["id"], "changed": changed,
		"candidate": cand, "nodes_candidate": nodesCand,
	})
}

func (c *CLI) cmdSync() int {
	list, rc := c.strictLoadForMutation()
	if rc != exitOK {
		return rc
	}
	cand, nodesCand, rc := c.writeCandidates(list)
	if rc != exitOK {
		return rc
	}
	return c.outJSON(map[string]any{"candidate": cand, "nodes_candidate": nodesCand})
}

// rulesetEnsureTimeout 是准备规则集的总超时。
//
// 首次启用白名单要把上游文本拉下来再转换，项数多、网络慢都可能花上几分钟；
// 而升级流程里这一步失败会让整次升级中止，所以余量给得比单次下载大得多。
const rulesetEnsureTimeout = 5 * time.Minute

// cmdRuleSets 确保 nodes.json 里白名单用到的规则集都已就绪（下载并转换）。
//
// 存在的理由是升级流程：配置里引用的规则集文件必须在生成配置**之前**就位，
// 否则 `sing-box check` 会失败——那不只是这次升级失败，而是所有节点一起挂。
//
// 之所以不塞进 sync 里自动做：CLI 刻意不碰网络，这样在无外网环境下仍然能用
// CLI 管理节点（sync / commit 都不需要联网）。
func (c *CLI) cmdRuleSets(args []string) int {
	if len(args) > 0 {
		return c.usageError("用法: rulesets")
	}
	list, rc := c.strictLoadForMutation()
	if rc != exitOK {
		return rc
	}
	var names []string
	for _, n := range list {
		names = append(names, AllowOf(n)...)
	}
	if len(names) == 0 {
		return exitOK
	}
	ctx, cancel := context.WithTimeout(context.Background(), rulesetEnsureTimeout)
	defer cancel()
	if err := ruleset.Ensure(ctx, ruleset.Dir(c.Store.SBConf), names); err != nil {
		return c.fail(fmt.Sprintf("规则集准备失败：%v", err))
	}
	fmt.Fprintf(c.Stdout, "规则集已就绪：%d 项\n", len(names))
	return exitOK
}

// ---- commit / rollback ---------------------------------------------------

func (c *CLI) cmdCommit() int {
	// 先提交 sing-box 候选配置（若有），再提交 nodes.json。
	// 两者都走 RenameAtomic（rename + fsync 父目录），保证掉电场景下目录项同样持久。
	// v3.0.7 之前 config 由 shell `mv -f` 提交（无目录 fsync），与 nodes.json 的
	// 耐久性不一致；收拢到 Go 后统一语义，失败时由 shell 的 restore_old 回滚。
	confCand := c.Store.SBConf + ".candidate"
	if _, err := os.Stat(confCand); err == nil {
		if err := fsx.RenameAtomic(confCand, c.Store.SBConf); err != nil {
			return c.fail("config 提交失败: " + err.Error())
		}
	}
	cand := c.Store.NodesPath() + ".candidate"
	if _, err := os.Stat(cand); err == nil {
		// durable rename：rename 成功后 fsync 父目录，掉电场景下目录项不丢
		if err := fsx.RenameAtomic(cand, c.Store.NodesPath()); err != nil {
			return c.fail(err.Error())
		}
	}
	fmt.Fprintln(c.Stdout, "ok")
	return exitOK
}

func (c *CLI) cmdRollback() int {
	for _, path := range []string{c.Store.NodesPath() + ".candidate", c.Store.SBConf + ".candidate"} {
		if _, err := os.Stat(path); err == nil {
			_ = os.Remove(path)
		}
	}
	fmt.Fprintln(c.Stdout, "ok")
	return exitOK
}

// cmdSS2022Key 生成 Shadowsocks 2022 Base64 密码（crypto/rand）。
// 用法: ss2022-key --method=2022-blake3-aes-128-gcm|2022-blake3-aes-256-gcm
var ss2022KeyKnown = map[string]bool{"method": true}

func (c *CLI) cmdSS2022Key(args []string) int {
	p, err := parseArgs(args, ss2022KeyKnown)
	if err != nil {
		return c.usageError(err.Error())
	}
	method := p.flags["method"]
	if !IsSS2022Method(method) {
		return c.fail("未知的 Shadowsocks 2022 method: " + method)
	}
	pw, err := GenerateSS2022Password(method)
	if err != nil {
		return c.fail(err.Error())
	}
	fmt.Fprintln(c.Stdout, pw)
	return exitOK
}

// ---- 查询类 --------------------------------------------------------------

func (c *CLI) cmdList(args []string) int {
	p, err := parseArgs(args, map[string]bool{"json": true})
	if err != nil {
		return c.usageError(err.Error())
	}
	list := LoadToolNodes(c.Store.NodesPath())
	if p.has("json") {
		arr := make([]any, len(list))
		for i, n := range list {
			arr[i] = map[string]any(n)
		}
		data, merr := marshalIndentCompact(arr)
		if merr != nil {
			return c.fail(merr.Error())
		}
		fmt.Fprintln(c.Stdout, string(data))
		return exitOK
	}
	if len(list) == 0 {
		fmt.Fprintln(c.Stdout, "(暂无节点)")
		return exitOK
	}
	fmt.Fprintf(c.Stdout, "%-4s %-16s %-14s %-8s\n", "ID", "名称", "类型", "端口")
	for _, n := range list {
		fmt.Fprintf(c.Stdout, "%-4s %-16s %-14s %-8s\n",
			IDString(n), TruncateRunes(Str(n, "name"), 16), DisplayType(n), Str(n, "port"))
	}
	return exitOK
}

func (c *CLI) cmdCount() int {
	fmt.Fprintln(c.Stdout, len(LoadToolNodes(c.Store.NodesPath())))
	return exitOK
}

func (c *CLI) cmdLast() int {
	list := LoadToolNodes(c.Store.NodesPath())
	if len(list) == 0 {
		fmt.Fprintln(c.Stdout, "")
		return exitOK
	}
	fmt.Fprintln(c.Stdout, IDString(list[len(list)-1]))
	return exitOK
}

func (c *CLI) cmdInfo(args []string) int {
	if len(args) != 1 {
		return c.usageError("用法: info <id>")
	}
	for _, n := range LoadToolNodes(c.Store.NodesPath()) {
		if IDString(n) == args[0] || Str(n, "id") == args[0] {
			fmt.Fprintf(c.Stdout, "%s\t%s\t%s", Str(n, "type"), Str(n, "sni"), Str(n, "port"))
			// 第 4 字段：shadowsocks=method；snell=version；其余为空
			if m := Str(n, "method"); m != "" {
				fmt.Fprintf(c.Stdout, "\t%s", m)
			} else if Str(n, "type") == "snell" {
				fmt.Fprintf(c.Stdout, "\t%s", Str(n, "version"))
			}
			fmt.Fprintln(c.Stdout)
			return exitOK
		}
	}
	return c.fail(fmt.Sprintf("未找到节点 id=%s", args[0]))
}

// ---- links ---------------------------------------------------------------

var linksKnown = map[string]bool{"host": true, "host6": true}

func (c *CLI) cmdLinks(args []string) int {
	p, err := parseArgs(args, linksKnown)
	if err != nil {
		return c.usageError(err.Error())
	}
	list := LoadToolNodes(c.Store.NodesPath())
	if len(p.positional) > 0 {
		id := p.positional[0]
		var filtered []Node
		for _, n := range list {
			if IDString(n) == id || Str(n, "id") == id {
				filtered = append(filtered, n)
			}
		}
		if len(filtered) == 0 {
			return c.fail(fmt.Sprintf("未找到节点 id=%s", id))
		}
		list = filtered
	}
	host := p.flags["host"]
	host6Val := ""
	if v, ok := p.flags["host6"]; ok {
		host6Val = v
	} else {
		host6Val = c.Store.ShareHost6()
	}

	for _, n := range list {
		fmt.Fprintf(c.Stdout, "### %s (%s, 端口 %s)\n",
			Str(n, "name"), DisplayType(n), Str(n, "port"))
		if Str(n, "type") == "snell" {
			fmt.Fprintln(c.Stdout, c.Store.LinkFor(n, host, ""))
			if host6Val != "" {
				fmt.Fprintln(c.Stdout, "# IPv6:")
				fmt.Fprintln(c.Stdout, c.Store.LinkFor(n, host6Val, "-IPv6"))
			}
			fmt.Fprintf(c.Stdout, "# Surge 配置格式（iOS/macOS Surge）:\n")
			fmt.Fprintln(c.Stdout, c.Store.SnellSurgeFor(n, host, ""))
			if host6Val != "" {
				fmt.Fprintln(c.Stdout, "# IPv6:")
				fmt.Fprintln(c.Stdout, c.Store.SnellSurgeFor(n, host6Val, "-IPv6"))
			}
		} else {
			fmt.Fprintln(c.Stdout, c.Store.LinkFor(n, host, ""))
			if host6Val != "" {
				fmt.Fprintln(c.Stdout, "# IPv6:")
				fmt.Fprintln(c.Stdout, c.Store.LinkFor(n, host6Val, "-IPv6"))
			}
		}
		fmt.Fprintln(c.Stdout, "")
	}
	return exitOK
}

// ---- port-used / 分享地址 --------------------------------------------------

func (c *CLI) cmdPortUsed(args []string) int {
	if len(args) != 1 {
		return c.usageError("用法: port-used <port>")
	}
	want, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
	if err != nil {
		return c.fail("invalid literal for int(): '" + args[0] + "'")
	}
	for _, n := range LoadToolNodes(c.Store.NodesPath()) {
		if p, eerr := toInt(n["port"]); eerr == nil && p == want {
			fmt.Fprintf(c.Stdout, "used by node %s\n", IDString(n))
			return exitOK
		}
	}
	return exitErr
}

func (c *CLI) cmdSetHost(args []string) int {
	if len(args) != 1 {
		return c.usageError("用法: set-host <host>")
	}
	if err := c.Store.SetHost(args[0]); err != nil {
		return c.fail(err.Error())
	}
	fmt.Fprintln(c.Stdout, args[0])
	return exitOK
}

func (c *CLI) cmdSetHost6(args []string) int {
	v := ""
	if len(args) > 0 {
		v = strings.TrimSpace(args[0])
	}
	if err := c.Store.SetHost6(v); err != nil {
		return c.fail(err.Error())
	}
	fmt.Fprintln(c.Stdout, v)
	return exitOK
}
