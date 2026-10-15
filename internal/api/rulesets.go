package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/ruleset"
)

// rulesetItemDTO 是面板可选的规则集条目。
type rulesetItemDTO struct {
	Name string `json:"name"`
	// Label 是中文显示名；缺失时前端退回 Name。
	Label string `json:"label"`
	// Cached 表示规则集是否已在服务器本地就绪。
	// 面板据此提示「首次启用需要联网下载」，但不禁用选项——下载发生在保存时。
	Cached bool `json:"cached"`
}

type rulesetGroupDTO struct {
	Name  string           `json:"name"`
	Items []rulesetItemDTO `json:"items"`
}

// ruleSetDir 返回规则集缓存目录。
func (s *Server) ruleSetDir() string {
	cli, _, _ := s.newNodeCLI()
	return ruleset.Dir(cli.Store.SBConf)
}

// handleRuleSets 返回可选规则集清单，供面板渲染多选。
func (s *Server) handleRuleSets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	dir := s.ruleSetDir()
	// 清单有四百多项，逐个开文件判断就绪状态会让这个接口变成几百次 IO，
	// 所以先扫一遍目录拿到集合。
	cached := ruleset.CachedSet(dir)
	groups := ruleset.Catalog()
	out := make([]rulesetGroupDTO, 0, len(groups))
	for _, g := range groups {
		items := make([]rulesetItemDTO, 0, len(g.Items))
		for _, it := range g.Items {
			items = append(items, rulesetItemDTO{
				Name:   it.Name,
				Label:  it.Label,
				Cached: cached[it.Name],
			})
		}
		out = append(out, rulesetGroupDTO{Name: g.Name, Items: items})
	}
	s.sendJSON(w, r, http.StatusOK, map[string]any{"groups": out})
}

// ensureRuleSets 确保本次变更需要的规则集已缓存到本地。
//
// 时机很关键：必须在生成候选配置之前。配置里引用的是本地规则集路径，
// 文件缺失会让 sing-box 直接起不来——那不只是这个节点不可用，而是
// 整机上所有节点一起挂。所以取不到就整次变更失败，绝不带着缺口往下走。
func (s *Server) ensureRuleSets(ctx context.Context, args []string) error {
	var names []string
	for _, a := range args {
		v, ok := strings.CutPrefix(a, "--allow=")
		if !ok {
			continue
		}
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	return ruleset.Ensure(ctx, s.ruleSetDir(), names)
}
