package nodes

import (
	"encoding/base64"
	"strings"
)

// ShareHost 读取分享用 IPv4/域名（state.json）。
func (s *Store) ShareHost() string {
	return Str(LoadState(s.StatePath()), "host")
}

// ShareHost6 读取分享用 IPv6。
func (s *Store) ShareHost6() string {
	return Str(LoadState(s.StatePath()), "host6")
}

// SetHost 写入 IPv4/域名。
func (s *Store) SetHost(host string) error {
	st := LoadState(s.StatePath())
	st["host"] = host
	return SaveState(s.StatePath(), st)
}

// SetHost6 写入/清除（空值=清除）IPv6 分享地址。
func (s *Store) SetHost6(host string) error {
	st := LoadState(s.StatePath())
	host = strings.TrimSpace(host)
	if host != "" {
		st["host6"] = host
	} else {
		delete(st, "host6")
	}
	return SaveState(s.StatePath(), st)
}

// URIHost IPv6 地址加方括号。
func URIHost(h string) string {
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		return "[" + h + "]"
	}
	return h
}

// LinkFor 生成单个节点分享链接。host 为空时回退 state.json，再回退占位符。
// labelSuffix 追加在名称之后（IPv6 版本用 "-IPv6"）。类型未知返回空串。
func (s *Store) LinkFor(n Node, host, labelSuffix string) string {
	if host == "" {
		host = s.ShareHost()
	}
	if host == "" {
		host = "SERVER_IP"
	}
	h := URIHost(host)
	t := Str(n, "type")
	baseName := Str(n, "name")
	if baseName == "" {
		baseName = t
	}
	name := PyQuote(baseName+labelSuffix, "/")
	port := Str(n, "port")

	switch t {
	case "vless":
		q := []QueryPair{
			{"encryption", "none"},
			{"security", "reality"},
			{"type", "tcp"},
			{"sni", Str(n, "sni")},
			{"fp", "chrome"},
			{"pbk", Str(n, "public_key")},
			{"sid", Str(n, "short_id")},
		}
		if Truthy(n, "flow") {
			q = append(q, QueryPair{"flow", Str(n, "flow")})
		}
		return "vless://" + Str(n, "uuid") + "@" + h + ":" + port + "?" + EncodeQuery(q) + "#" + name

	case "shadowsocks":
		userinfo := base64.RawURLEncoding.EncodeToString(
			[]byte(Str(n, "method") + ":" + Str(n, "password")))
		return "ss://" + userinfo + "@" + h + ":" + port + "#" + name

	case "trojan":
		q := []QueryPair{
			{"security", "tls"},
			{"sni", Str(n, "sni")},
			{"allowInsecure", "1"},
			{"type", "tcp"},
		}
		return "trojan://" + PyQuote(Str(n, "password"), "/") + "@" + h + ":" + port +
			"?" + EncodeQuery(q) + "#" + name

	case "anytls":
		q := []QueryPair{
			{"sni", Str(n, "sni")},
			{"insecure", "1"},
		}
		return "anytls://" + PyQuote(Str(n, "password"), "/") + "@" + h + ":" + port +
			"?" + EncodeQuery(q) + "#" + name
	}
	return ""
}

// Subscription 生成 Base64 订阅；host6 非空时每个节点附一条 IPv6 版链接。
func (s *Store) Subscription(list []Node, host, host6 string) string {
	var links []string
	for _, n := range list {
		links = append(links, s.LinkFor(n, host, ""))
		if host6 != "" {
			links = append(links, s.LinkFor(n, host6, "-IPv6"))
		}
	}
	var body []string
	for _, l := range links {
		if l != "" {
			body = append(body, l)
		}
	}
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(body, "\n")))
}
