package nodes

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// BuildSubscriptionURIs 生成通用订阅内容（每行一个分享链接）。
//
// 只输出 IPv4/域名版：订阅是给手机客户端用的，同时塞 IPv4+IPv6 会让
// 节点数翻倍、在只支持一种栈的网络上多出一半"连不上的节点"。
// 需要 IPv6 的用户可以单独复制面板里的单节点链接。
func BuildSubscriptionURIs(store *Store, list []Node) []string {
	host := store.ShareHost()
	out := make([]string, 0, len(list))
	for _, n := range list {
		if link := store.LinkFor(n, host, ""); link != "" {
			out = append(out, link)
		}
	}
	return out
}

// EncodeSubscription 把链接列表编码成通用订阅体（base64，无换行）。
// Shadowrocket / v2rayN / Stash 等客户端都认这个格式。
func EncodeSubscription(links []string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(links, "\n")))
}

// BuildSingboxProfile 生成 sing-box 客户端完整配置（可被
// sing-box://import-remote-profile 直接导入）。
//
// 刻意保持最小化：不写 dns 段、不引用远程 rule_set。
//   - dns：各版本 sing-box 的 dns schema 变动频繁（1.12 起 address 形式
//     标记废弃），写错会让客户端整份配置加载失败；省略时客户端用系统解析器，
//     虽不抗污染但一定能起来。要更强的解析策略应由用户自己在客户端里配。
//   - rule_set：依赖远程下载，弱网/被墙时首次导入即失败。
//
// 返回的 skipped 是"合法但无法承载"的节点说明，供面板提示用户。
func BuildSingboxProfile(store *Store, list []Node, host string) (map[string]any, []string, error) {
	if host == "" {
		return nil, nil, fmt.Errorf("尚未设置分享地址（state.json 的 host）")
	}

	outbounds := make([]any, 0, len(list)+2)
	tags := make([]string, 0, len(list))
	skipped := make([]string, 0)
	used := make(map[string]bool, len(list))

	for _, n := range list {
		name := DisplayName(n)
		if name == "" {
			name = Str(n, "type")
		}
		tag := uniqueTag(name, used)
		ob, reason, err := OutboundFor(n, host, tag)
		if err != nil {
			skipped = append(skipped, name+"：数据异常（"+err.Error()+"）")
			continue
		}
		if reason != "" {
			skipped = append(skipped, name+"："+reason)
			continue
		}
		outbounds = append(outbounds, ob)
		tags = append(tags, tag)
	}

	if len(tags) == 0 {
		return nil, skipped, fmt.Errorf("没有可导出的节点")
	}

	outbounds = append(outbounds,
		map[string]any{
			"type":      "selector",
			"tag":       "proxy",
			"outbounds": tags,
			"default":   tags[0],
		},
		map[string]any{"type": "direct", "tag": "direct"},
	)

	profile := map[string]any{
		"log": map[string]any{"level": "warn", "timestamp": true},
		// 空数组：由客户端 App 自己注入 tun/mixed inbound。
		// 写成非空反而会和 App 生成的入站冲突。
		"inbounds":  []any{},
		"outbounds": outbounds,
		"route": map[string]any{
			"rules": []any{
				// 1.11+ 的规则动作写法；旧版客户端会因未知字段拒绝加载，
				// 面板订阅页已标注最低版本要求。
				map[string]any{"action": "sniff"},
				map[string]any{"protocol": "dns", "action": "hijack-dns"},
				map[string]any{"ip_is_private": true, "outbound": "direct"},
			},
			"final":                 "proxy",
			"auto_detect_interface": true,
		},
	}
	return profile, skipped, nil
}

// uniqueTag 保证 outbound tag 唯一。重名节点加 " (2)" 后缀——
// tag 重复会让 sing-box 直接拒绝启动，而节点重名在面板里是完全允许的。
func uniqueTag(base string, used map[string]bool) string {
	tag := base
	for i := 2; used[tag]; i++ {
		tag = fmt.Sprintf("%s (%d)", base, i)
	}
	used[tag] = true
	return tag
}
