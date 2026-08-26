package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
	"github.com/k6nfmm7dbr-commits/sbx/internal/policy"
)

// 策略 API 路由（仅 Web 面板管理，不进 sbx CLI）。
// 路径形态：
//
//	GET  /api/nodes/<id>/policy
//	PUT  /api/nodes/<id>/policy
//	POST /api/nodes/<id>/quota/reset
//	GET  /api/nodes/<id>/active-ips
func (s *Server) handlePolicyAPI(w http.ResponseWriter, r *http.Request, idStr string, sub string) {
	if s.policy == nil {
		s.sendJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "policy 未初始化"})
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid node id"})
		return
	}
	nodeID := strconv.FormatInt(id, 10)

	switch {
	case sub == "policy" && r.Method == http.MethodGet:
		s.getPolicy(w, r, nodeID)
	case sub == "policy" && r.Method == http.MethodPut:
		s.putPolicy(w, r, nodeID)
	case sub == "quota/reset" && r.Method == http.MethodPost:
		s.resetQuota(w, r, nodeID)
	case sub == "active-ips" && r.Method == http.MethodGet:
		s.activeIPs(w, r, nodeID)
	default:
		s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// policyNodeExists 校验节点 id 是否真实存在。
func (s *Server) policyNodeExists(nodeID string) bool {
	list := nodes.LoadPanelNodes(s.cfg.NodesFile)
	for _, n := range list {
		if nodes.IDString(n) == nodeID {
			return true
		}
	}
	return false
}

func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request, nodeID string) {
	if !s.policyNodeExists(nodeID) {
		s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	states, _ := s.policy.Snapshot()
	st, ok := states[nodeID]
	if !ok {
		// 尚未 reconcile 或无策略记录：返回默认「全不限」。
		st = policyStateDefault()
	}
	s.sendJSON(w, r, http.StatusOK, st)
}

// putPolicyRequest 是 PUT /api/nodes/:id/policy 的请求体。
type putPolicyRequest struct {
	QuotaEnabled    bool  `json:"quota_enabled"`
	QuotaLimitBytes int64 `json:"quota_limit_bytes"`
	IPLimitEnabled  bool  `json:"ip_limit_enabled"`
	IPLimitMax      int   `json:"ip_limit_max"`
}

func (s *Server) putPolicy(w http.ResponseWriter, r *http.Request, nodeID string) {
	if !s.policyNodeExists(nodeID) {
		s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	var req putPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	// 参数校验：enabled 时 limit 必须 > 0；max_ips 必须 >= 1。
	if req.QuotaEnabled && req.QuotaLimitBytes <= 0 {
		s.sendJSON(w, r, http.StatusBadRequest,
			map[string]string{"error": "quota_limit_bytes 必须 > 0"})
		return
	}
	if req.IPLimitEnabled && req.IPLimitMax < 1 {
		s.sendJSON(w, r, http.StatusBadRequest,
			map[string]string{"error": "ip_limit_max 必须 >= 1"})
		return
	}

	cur, err := s.policy.GetConfig(r.Context(), nodeID)
	if err != nil {
		s.sendJSON(w, r, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// 保留 reset 基线：Quota 的「已用」语义依赖它，PUT 不能顺带重置。
	cur.QuotaEnabled = req.QuotaEnabled
	cur.QuotaLimitBytes = req.QuotaLimitBytes
	cur.IPLimitEnabled = req.IPLimitEnabled
	cur.IPLimitMax = req.IPLimitMax

	if err := s.policy.UpsertConfig(r.Context(), cur); err != nil {
		s.sendJSON(w, r, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// 立即 reconcile：让 nft enforcement 生效并刷新缓存。
	if err := s.policy.Reconcile(r.Context()); err != nil {
		s.sendJSON(w, r, http.StatusInternalServerError,
			map[string]string{"error": "策略已保存但应用失败: " + err.Error()})
		return
	}
	states, _ := s.policy.Snapshot()
	s.sendJSON(w, r, http.StatusOK, states[nodeID])
}

func (s *Server) resetQuota(w http.ResponseWriter, r *http.Request, nodeID string) {
	if !s.policyNodeExists(nodeID) {
		s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if _, err := s.policy.ResetQuota(r.Context(), nodeID); err != nil {
		s.sendJSON(w, r, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	states, _ := s.policy.Snapshot()
	s.sendJSON(w, r, http.StatusOK, states[nodeID])
}

func (s *Server) activeIPs(w http.ResponseWriter, r *http.Request, nodeID string) {
	if !s.policyNodeExists(nodeID) {
		s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	ips := s.policy.ActiveIPs(nodeID)
	if ips == nil {
		ips = []string{}
	}
	s.sendJSON(w, r, http.StatusOK, map[string]any{"ips": ips})
}

func policyStateDefault() policy.State {
	return policy.State{
		QuotaEnabled: false,
		QuotaLimit:   0,
		QuotaUsed:    0,
		QuotaState:   "unlimited",
		IPLimitOn:    false,
		IPLimitMax:   0,
		ActiveIPs:    0,
		IPLimitState: "unlimited",
	}
}
