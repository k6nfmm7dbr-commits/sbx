package nodes

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
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
func (c *CLI) Run(args []string) int {
	if len(args) == 0 {
		return c.usageError("需要子命令")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "add":
		return c.cmdAdd(rest)
	case "remove":
		return c.cmdRemove(rest)
	case "edit":
		return c.cmdEdit(rest)
	case "sync":
		return c.cmdSync()
	case "commit":
		return c.cmdCommit()
	case "rollback":
		return c.cmdRollback()
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
	fmt.Fprintln(c.Stderr, "用法: sbx-core node <add|edit|remove|list|count|last|info|links|sync|commit|rollback|port-used|set-host|get-host|set-host6|get-host6>")
	return exitUsage
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
	node["port"] = json.Number(strconv.FormatInt(portNum, 10))

	if name := p.flags["name"]; name != "" {
		node["name"] = name
	} else {
		node["name"] = fmt.Sprintf("%s-%d", typ, nid)
	}
	for _, key := range []string{"uuid", "password", "method", "sni", "flow",
		"private_key", "public_key", "short_id"} {
		flag := strings.ReplaceAll(key, "_", "-")
		if v := p.flags[flag]; v != "" {
			node[key] = v
		}
	}
	if typ == "trojan" || typ == "anytls" {
		node["cert"] = c.Store.CertDir() + "/cert.pem"
		node["key"] = c.Store.CertDir() + "/key.pem"
	}

	list = append(list, node)
	cfg, err := RebuildConfig(c.Store, list)
	if err != nil {
		return c.fail(err.Error())
	}
	cand, err := WriteCandidate(c.Store, cfg)
	if err != nil {
		return c.fail(err.Error())
	}
	nodesCand, err := WriteNodesCandidate(c.Store, list)
	if err != nil {
		return c.fail(err.Error())
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

// strictLoadForMutation 供 edit/remove/sync 使用。
func (c *CLI) strictLoadForMutation() ([]Node, int) {
	list, err := LoadToolNodesStrict(c.Store.NodesPath())
	if err != nil {
		return nil, c.fail(err.Error())
	}
	return list, exitOK
}

func (c *CLI) writeCandidates(list []Node) (string, string, int) {
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

func (c *CLI) cmdRemove(args []string) int {
	p, err := parseArgs(args, map[string]bool{})
	if err != nil {
		return c.usageError(err.Error())
	}
	if len(p.positional) != 1 {
		return c.usageError("用法: remove <id>")
	}
	list, rc := c.strictLoadForMutation()
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

var editKnown = map[string]bool{"port": true, "sni": true}

func (c *CLI) cmdEdit(args []string) int {
	p, err := parseArgs(args, editKnown)
	if err != nil {
		return c.usageError(err.Error())
	}
	if len(p.positional) != 1 {
		return c.usageError("用法: edit <id> [--port N] [--sni DOMAIN]")
	}
	id := p.positional[0]
	list, rc := c.strictLoadForMutation()
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
	if len(changed) == 0 {
		return c.fail("未指定要修改的内容（--port / --sni）")
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

// ---- commit / rollback ---------------------------------------------------

func (c *CLI) cmdCommit() int {
	cand := c.Store.NodesPath() + ".candidate"
	if _, err := os.Stat(cand); err == nil {
		if err := os.Rename(cand, c.Store.NodesPath()); err != nil {
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
			IDString(n), TruncateRunes(Str(n, "name"), 16), Str(n, "type"), Str(n, "port"))
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
			fmt.Fprintf(c.Stdout, "%s\t%s\t%s\n", Str(n, "type"), Str(n, "sni"), Str(n, "port"))
			return exitOK
		}
	}
	return c.fail(fmt.Sprintf("未找到节点 id=%s", args[0]))
}

// ---- links ---------------------------------------------------------------

var linksKnown = map[string]bool{"host": true, "host6": true, "sub": true}

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
	host6Given := false
	host6Val := ""
	if v, ok := p.flags["host6"]; ok {
		host6Given = true
		host6Val = v
	} else {
		host6Val = c.Store.ShareHost6()
	}

	if p.has("sub") {
		fmt.Fprintln(c.Stdout, c.Store.Subscription(list, host, host6Val))
		return exitOK
	}
	for _, n := range list {
		fmt.Fprintf(c.Stdout, "### %s (%s, 端口 %s)\n",
			Str(n, "name"), Str(n, "type"), Str(n, "port"))
		fmt.Fprintln(c.Stdout, c.Store.LinkFor(n, host, ""))
		if host6Val != "" {
			fmt.Fprintln(c.Stdout, "# IPv6:")
			fmt.Fprintln(c.Stdout, c.Store.LinkFor(n, host6Val, "-IPv6"))
		}
		fmt.Fprintln(c.Stdout, "")
	}
	_ = host6Given
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
