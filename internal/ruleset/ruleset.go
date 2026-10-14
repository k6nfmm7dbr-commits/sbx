// Package ruleset 管理 sing-box 规则集（.srs）的本地缓存。
//
// 背景：节点可以配置「只允许访问特定服务」的白名单。白名单由若干规则集组成
// （如 Google / TikTok），规则集文件来自 Repcz/Tool 的公开分发。
//
// 为什么缓存到本机而不是让 sing-box 直接引用远程 URL：
//  1. sing-box 启动时才去拉远程规则集，一旦网络不可达或 GitHub 被墙，**整个
//     sing-box 起不来**——所有节点一起挂，不只是配了白名单的那个。
//  2. 本地文件能在提交前用 `sing-box check` 真正解析验证，问题拦在写入之前。
//  3. 允许运维手动放入 .srs 离线导入，完全不依赖外网。
package ruleset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
)

const (
	// DirName 是规则集缓存目录名，位于 sing-box 配置所在目录之下。
	DirName = "rulesets"
	// fileExt 是 sing-box 二进制规则集扩展名。
	fileExt = ".srs"
	// srsMagic 是二进制规则集的文件头（其后紧跟一个版本字节）。
	srsMagic = "SRS"
	// minSize 只需容纳文件头。
	//
	// 刻意不设更大的体积阈值：真实的极小规则集（单条 ip_cidr）编译后只有 29 字节，
	// 用体积当"是否就绪"的判据会把合法文件误判为缺失，导致反复下载、或者 CLI
	// 直接拒绝保存节点。内容的语义正确性交给随后的 `sing-box check`。
	minSize = len(srsMagic) + 1
	// maxSize 是单文件上限，防止异常响应写爆磁盘。
	maxSize = 16 << 20
	// downloadTimeout 是单次下载的整体超时。
	downloadTimeout = 30 * time.Second
)

// Item 是一个可被节点选中的规则集。
type Item struct {
	// Name 是规则集标识，同时也是 .srs 的文件名（来自上游目录）。
	Name string `json:"name"`
	// Label 是面板上显示的名称。
	Label string `json:"label"`
}

// Group 是一组按用途归类的规则集，仅用于面板分组展示。
type Group struct {
	Name  string `json:"name"`
	Items []Item `json:"items"`
}

// catalog 是内置的可选清单。只收录对「受限节点」有实际意义的条目，
// 不把上游全部 73 个规则集铺开——面板上要能一眼选完。
//
// 名字必须与上游 sing-box/Rules 目录下的 <Name>.srs 完全一致（大小写敏感）。
var catalog = []Group{
	{Name: "常用服务", Items: []Item{
		{"Google", "Google（含搜索、Gmail、YouTube）"},
		{"YouTube", "YouTube"},
		{"Microsoft", "微软（含 Office、OneDrive）"},
		{"Apple", "Apple（含 App Store、iCloud）"},
		{"Github", "GitHub"},
	}},
	{Name: "AI 服务", Items: []Item{
		{"OpenAI", "OpenAI / ChatGPT"},
		{"Claude", "Anthropic Claude"},
		{"xAI", "xAI / Grok"},
		{"AI", "其它常见 AI 站点"},
	}},
	{Name: "社交媒体", Items: []Item{
		{"TikTok", "TikTok"},
		{"Twitter", "X / Twitter"},
		{"Facebook", "Facebook"},
		{"Instagram", "Instagram"},
		{"Telegram", "Telegram"},
		{"Reddit", "Reddit"},
	}},
	{Name: "影音流媒体", Items: []Item{
		{"Netflix", "Netflix"},
		{"Disney", "Disney+"},
		{"PrimeVideo", "Amazon Prime Video"},
		{"HBO", "HBO / Max"},
		{"Spotify", "Spotify"},
		{"Emby", "Emby"},
		{"Bahamut", "巴哈姆特动画疯"},
	}},
	{Name: "游戏", Items: []Item{
		{"Steam", "Steam"},
		{"Epic", "Epic Games"},
		{"Blizzard", "暴雪"},
		{"Nintendo", "任天堂"},
		{"Xbox", "Xbox"},
		{"EA", "EA"},
	}},
	{Name: "办公与开发", Items: []Item{
		{"OneDrive", "OneDrive"},
		{"AppStore", "App Store"},
		{"Cloudflare", "Cloudflare"},
		{"Oracle", "Oracle Cloud"},
	}},
	{Name: "国内站点", Items: []Item{
		{"Bilibili", "哔哩哔哩"},
		{"WeChat", "微信"},
		{"ChinaDomain", "中国大陆域名"},
		{"ChinaIP", "中国大陆 IP 段"},
	}},
	{Name: "支付与加密", Items: []Item{
		{"PayPal", "PayPal"},
		{"Crypto", "加密货币相关"},
		{"TronLink", "TronLink 钱包"},
	}},
}

// Catalog 返回内置清单的副本，避免调用方改动全局状态。
func Catalog() []Group {
	out := make([]Group, 0, len(catalog))
	for _, g := range catalog {
		items := make([]Item, len(g.Items))
		copy(items, g.Items)
		out = append(out, Group{Name: g.Name, Items: items})
	}
	return out
}

// known 是名字到标签的索引，供校验与展示复用。
var known = func() map[string]string {
	m := map[string]string{}
	for _, g := range catalog {
		for _, it := range g.Items {
			m[it.Name] = it.Label
		}
	}
	return m
}()

// Known 报告 name 是否是清单内的合法规则集名。
//
// 这是白名单的第一道闸：只有清单内的名字才会被拼进文件路径与 sing-box 配置，
// 因此不存在路径穿越，也不会把任意 URL 写进配置。
func Known(name string) bool {
	_, ok := known[name]
	return ok
}

// Label 返回规则集的中文显示名；未知名字返回空串。
func Label(name string) string { return known[name] }

// Validate 校验一组规则集名，并按清单顺序去重。
func Validate(names []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if !Known(name) {
			return nil, fmt.Errorf("未知的规则集: %q", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// Dir 返回规则集缓存目录（sing-box 配置同级的 rulesets/）。
func Dir(sbConfPath string) string {
	return filepath.Join(filepath.Dir(sbConfPath), DirName)
}

// FilePath 返回某个规则集的本地路径。
func FilePath(dir, name string) string {
	return filepath.Join(dir, name+fileExt)
}

// Cached 报告规则集是否已在本地且格式正确。
//
// 判据是文件头而不是体积——见 minSize 的说明。这让「手工放入一个损坏文件」
// 不会被当成就绪，同时不会误伤体积很小的合法规则集。
func Cached(dir, name string) bool {
	f, err := os.Open(FilePath(dir, name))
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(srsMagic))
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return string(head) == srsMagic
}

// sources 返回可用的下载地址，按优先级排列。
//
// jsDelivr 放第一位：它同时服务国内与海外，对国内 VPS 的可达性明显好于
// raw.githubusercontent.com。可用 SBX_RULESET_BASE 覆盖为自建镜像
// （值形如 https://example.com/sing-box/Rules）。
func sources(name string) []string {
	if base := strings.TrimRight(strings.TrimSpace(os.Getenv("SBX_RULESET_BASE")), "/"); base != "" {
		return []string{base + "/" + name + fileExt}
	}
	return []string{
		"https://cdn.jsdelivr.net/gh/Repcz/Tool@X/sing-box/Rules/" + name + fileExt,
		"https://raw.githubusercontent.com/Repcz/Tool/X/sing-box/Rules/" + name + fileExt,
		"https://github.com/Repcz/Tool/raw/X/sing-box/Rules/" + name + fileExt,
	}
}

// Ensure 确保 names 对应的规则集都已就绪，缺失的会尝试下载。
//
// 已存在的文件不会被覆盖：运维手动放置的版本（可能是内网镜像或自己维护的
// 名单）应当被尊重，删除文件即可触发重新下载。
//
// 任何一个规则集取不到都会返回错误。调用方必须把错误当作「整次变更失败」处理，
// 绝不能带着缺失的规则集去写配置——那会让 sing-box 起不来。
func Ensure(ctx context.Context, dir string, names []string) error {
	list, err := Validate(names)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("无法创建规则集目录 %s: %w", dir, err)
	}
	client := &http.Client{Timeout: downloadTimeout}
	for _, name := range list {
		if Cached(dir, name) {
			continue
		}
		if err := download(ctx, client, dir, name); err != nil {
			return err
		}
	}
	return nil
}

// download 依次尝试各个源，成功后原子写入。
func download(ctx context.Context, client *http.Client, dir, name string) error {
	var lastErr error
	for _, url := range sources(name) {
		data, err := fetch(ctx, client, url)
		if err != nil {
			lastErr = err
			continue
		}
		if err := checkPayload(data); err != nil {
			lastErr = fmt.Errorf("%s: %w", url, err)
			continue
		}
		dst := FilePath(dir, name)
		// 0644：sing-box 可能以非 root 身份运行，需要可读。
		if err := fsx.WriteFileAtomic(dst, data, 0o644); err != nil {
			return fmt.Errorf("写入规则集 %s 失败: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("规则集 %s 下载失败（网络不可达或上游变更）：%v；"+
		"可手动下载 %s.srs 放入 %s 后重试", name, lastErr, name, dir)
}

// fetch 读取整个响应体，同时限制体积。
func fetch(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "sbx-ruleset/1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSize {
		return nil, errors.New("响应体过大")
	}
	return data, nil
}

// checkPayload 做最小但必要的格式校验。
//
// 不解析规则内容——语义正确性交给随后的 `sing-box check`，它才是权威。
// 这里只拦「明显不是规则集」的响应：错误页（HTML 以 "<" 开头）、空文件、
// 以及被截断到只剩几个字节的下载。
func checkPayload(data []byte) error {
	if len(data) < minSize {
		return fmt.Errorf("内容过小（%d 字节），可能不是有效的规则集", len(data))
	}
	if string(data[:len(srsMagic)]) != srsMagic {
		return errors.New("文件头不是 SRS，可能下载到了错误页")
	}
	return nil
}
