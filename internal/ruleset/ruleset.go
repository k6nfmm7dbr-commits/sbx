// Package ruleset 管理 sing-box 规则集（节点白名单用）的本地缓存。
//
// 背景：节点可以配置「只允许访问特定服务」的白名单，白名单由若干规则集组成
// （如 Google / TikTok / AI）。规则集内容来自 blackmatrix7/ios_rule_script 的公开分发。
//
// 上游给的是 **Surge 格式的文本规则**，不是 sing-box 格式，所以本地要多转一道
// （转换逻辑见 surge.go）。
//
// 为什么缓存到本机而不是让 sing-box 直接引用远程 URL：
//  1. sing-box 启动时才去拉远程规则集，一旦网络不可达或 GitHub 被墙，**整个
//     sing-box 起不来**——所有节点一起挂，不只是配了白名单的那个。
//  2. 本地文件能在提交前用 `sing-box check` 真正解析验证，问题拦在写入之前。
//  3. 允许运维手动放入 .list 文本离线导入，完全不依赖外网。
package ruleset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	// fileExt 是写入本地的 sing-box source 规则集扩展名。
	fileExt = ".json"
	// listExt 是上游的文本规则格式，同时也是手动离线导入使用的格式。
	listExt = ".list"
	// maxSize 是单文件上限，防止异常响应写爆磁盘。
	maxSize = 16 << 20
	// downloadTimeout 是单次下载的整体超时。
	downloadTimeout = 30 * time.Second
)

// Item 是一个可被节点选中的规则集。
type Item struct {
	// Name 是规则集标识，同时也是本地文件名（<Name>.json）。
	Name string `json:"name"`
	// Label 是面板上显示的名称。
	Label string `json:"label"`
	// Sources 是上游对应的文件名，留空表示与 Name 同名。
	//
	// 多个来源会被逐个下载并合并成一个规则集——AI 项就是这么来的：
	// 上游把 OpenAI / Claude / Gemini 拆成 8 个文件，对使用者来说
	// 「我只要放行 AI 站点」，拆开选既费事又容易漏。
	Sources []string `json:"-"`
}

// Group 是一组按用途归类的规则集，仅用于面板分组展示。
type Group struct {
	Name  string `json:"name"`
	Items []Item `json:"items"`
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

// known 是名字到清单项的索引，供校验与展示复用。
var known = func() map[string]Item {
	m := map[string]Item{}
	for _, g := range catalog {
		for _, it := range g.Items {
			m[it.Name] = it
		}
	}
	return m
}()

// itemOf 按名字取清单项。
func itemOf(name string) (Item, bool) {
	it, ok := known[name]
	return it, ok
}

// Known 报告 name 是否是清单内的合法规则集名。
//
// 这是白名单的第一道闸：只有清单内的名字才会被拼进文件路径与 sing-box 配置，
// 因此不存在路径穿越，也不会把任意 URL 写进配置。
func Known(name string) bool {
	_, ok := known[name]
	return ok
}

// Label 返回规则集的显示名；未知名字返回空串。
func Label(name string) string { return known[name].Label }

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

// FilePath 返回某个规则集写入后的本地路径（sing-box source 格式）。
func FilePath(dir, name string) string {
	return filepath.Join(dir, name+fileExt)
}

// ListPath 返回手动离线导入时使用的文本规则路径。
//
// 运维想用内网镜像或自己维护的名单时，把 Surge 格式的文本放到这里即可，
// 不需要会写 sing-box 的规则集格式。
func ListPath(dir, name string) string {
	return filepath.Join(dir, name+listExt)
}

// Cached 报告规则集是否已在本地就绪。
//
// 判据是「文件存在且看起来是 JSON 对象」——不做完整解析，语义正确性交给
// 随后的 `sing-box check`（它才是权威）。这一步只是为了不把空文件或
// 明显损坏的文件当成就绪。
func Cached(dir, name string) bool {
	f, err := os.Open(FilePath(dir, name))
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 1)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return head[0] == '{'
}

// CachedSet 一次扫目录，返回所有已就绪的规则集名。
//
// 面板要展示四百多个规则集的就绪状态，逐个开文件会让 /api/rulesets 变成
// 几百次 IO，所以这里只扫一次目录。
func CachedSet(dir string) map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ok := strings.CutSuffix(e.Name(), fileExt)
		if !ok {
			continue
		}
		// 空文件不算就绪（原子写入失败、磁盘写满都可能留下 0 字节文件）。
		if info, err := e.Info(); err == nil && info.Size() > 1 {
			out[name] = true
		}
	}
	return out
}

// upstreamSources 返回某个上游规则文件的下载地址，按优先级排列。
//
// jsDelivr 放第一位：它同时服务国内与海外，对国内 VPS 的可达性明显好于
// raw.githubusercontent.com。可用 SBX_RULESET_BASE 覆盖为自建镜像，
// 镜像目录结构需与上游一致（<base>/<Name>/<Name>.list）。
func upstreamSources(file string) []string {
	if base := strings.TrimRight(strings.TrimSpace(os.Getenv("SBX_RULESET_BASE")), "/"); base != "" {
		return []string{base + "/" + file + "/" + file + listExt}
	}
	const dir = "rule/Surge/"
	return []string{
		"https://cdn.jsdelivr.net/gh/blackmatrix7/ios_rule_script@master/" + dir + file + "/" + file + listExt,
		"https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/" + dir + file + "/" + file + listExt,
		"https://github.com/blackmatrix7/ios_rule_script/raw/master/" + dir + file + "/" + file + listExt,
	}
}

// Ensure 确保 names 对应的规则集都已就绪，缺失的会尝试下载并转换。
//
// 已存在的文件不会被覆盖：运维手动放置的版本（可能是内网镜像或自己维护的
// 名单）应当被尊重，删除文件即可触发重新生成。
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
		item, _ := itemOf(name)
		if err := build(ctx, client, dir, item); err != nil {
			return err
		}
	}
	return nil
}

// CompileManual 把手动放入的 <Name>.list 转换成规则集文件。
//
// 报告是否找到了手动文件；找不到不是错误——那只是说明该走联网路径。
//
// 单独留这个入口是因为 CLI 刻意不联网，而离线导入本身是纯本地操作：
// 运维把 .list 放进规则集目录之后，用 CLI 保存节点不该还需要先开一次面板
// 或者先连上外网。
func CompileManual(ctx context.Context, dir, name string) (bool, error) {
	path := ListPath(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	if err := compile(ctx, dir, name, data, path); err != nil {
		return true, err
	}
	return true, nil
}

// build 生成单个规则集：优先用手动放入的文本，否则下载上游并合并。
func build(ctx context.Context, client *http.Client, dir string, item Item) error {
	if done, err := CompileManual(ctx, dir, item.Name); done {
		return err
	}
	files := item.Sources
	if len(files) == 0 {
		files = []string{item.Name}
	}
	merged := newDomainSet()
	for _, file := range files {
		data, from, err := fetchFirst(ctx, client, file)
		if err != nil {
			return err
		}
		set, err := parseSurgeList(data)
		if err != nil {
			return fmt.Errorf("解析 %s 失败: %w", from, err)
		}
		merged.absorb(set)
	}
	return writeCompiled(ctx, dir, item.Name, merged, item.Name)
}

// compile 解析文本并落盘，用于手动离线导入的路径。
func compile(ctx context.Context, dir, name string, data []byte, origin string) error {
	set, err := parseSurgeList(data)
	if err != nil {
		return fmt.Errorf("解析 %s 失败: %w", origin, err)
	}
	return writeCompiled(ctx, dir, name, set, origin)
}

// writeCompiled 把解析结果写成 sing-box source 规则集。
//
// 解析不出任何规则时**报错而不是照写**：一个空规则集在 sing-box 里是合法的，
// 但会让配了它的节点除了 DNS 什么都放行不了——这种「勾了却什么都没生效」的
// 静默状态比直接报错难查得多。
func writeCompiled(ctx context.Context, dir, name string, set *domainSet, origin string) error {
	if set.ruleCount() == 0 {
		return fmt.Errorf("规则集 %s 没有解析出任何可用规则（来源 %s）："+
			"可能上游改了格式，或下载到了错误页", name, origin)
	}
	out, err := marshalSourceFile(set.build())
	if err != nil {
		return fmt.Errorf("序列化规则集 %s 失败: %w", name, err)
	}
	dst := FilePath(dir, name)
	// 0644：sing-box 可能以非 root 身份运行，需要可读。
	if err := fsx.WriteFileAtomic(dst, out, 0o644); err != nil {
		return fmt.Errorf("写入规则集 %s 失败: %w", name, err)
	}
	if len(set.skipped) > 0 {
		// 上游用了一些 sing-box 表达不了的条件（IP-ASN / USER-AGENT / …），
		// 记下来便于排查「为什么某个站点没被放行」。
		slog.DebugContext(ctx, "规则集转换完成，部分条件无法表达已跳过",
			"rule_set", name, "rules", set.ruleCount(), "skipped", fmt.Sprint(set.skipped))
	}
	return nil
}

// fetchFirst 依次尝试某个上游文件的各个源，返回最先成功的一份。
func fetchFirst(ctx context.Context, client *http.Client, file string) ([]byte, string, error) {
	var lastErr error
	for _, url := range upstreamSources(file) {
		data, err := fetch(ctx, client, url)
		if err != nil {
			lastErr = err
			continue
		}
		if err := checkPayload(data); err != nil {
			lastErr = fmt.Errorf("%s: %w", url, err)
			continue
		}
		return data, url, nil
	}
	return nil, "", fmt.Errorf("规则集 %s 下载失败（网络不可达或上游变更）：%v；"+
		"可手动下载 %s%s 放入规则集目录后重试", file, lastErr, file, listExt)
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
// 只拦「明显不是规则文本」的响应：空文件、被截断到只剩几个字节的下载、
// 以及 GitHub 偶尔返回的 HTML 错误页。规则语义交给解析器与随后的
// `sing-box check`，它们才是权威。
func checkPayload(data []byte) error {
	if len(data) == 0 {
		return errors.New("内容为空")
	}
	if data[0] == '<' {
		return errors.New("内容是 HTML，可能下载到了错误页")
	}
	return nil
}
