package api

import (
	"net/http"
)

// 错误响应口径（v3.0.10 安全收敛）
//
// 背景：旧实现把 err.Error() 直接拼进 JSON 返回给客户端，泄漏了文件绝对路径、
// SQL 语句片段、nft 命令行报错等内部实现细节——这些信息对攻击者有价值
// （探测部署结构、数据库 schema、是否 root、nft 版本差异），对普通用户则毫无意义。
//
// 新口径：
//   - 客户端只拿到**稳定**的 error_code + 通用文案 + request_id；
//   - 完整错误写服务端日志，用 request_id 与客户端提示一一对应；
//   - 4xx（客户端自身参数错误）仍返回可读的具体原因，因为那是用户需要修正的输入，
//     且不涉及内部实现。
//
// error_code 是给前端/脚本判断用的稳定标识，**不要**在版本间随意改名。
const (
	// 500 类
	codeSummaryFailed = "summary_failed" // /api/summary 构建失败
	codeLiveFailed    = "live_failed"    // /api/live 构建失败
	codeDailyFailed   = "daily_failed"   // /api/daily 查询失败
	codeExportFailed  = "export_failed"  // /api/export 查询失败
	codePolicyLoad    = "policy_load_failed"
	codePolicySave    = "policy_save_failed"
	codePolicyApply   = "policy_apply_failed"
	codeQuotaReset    = "quota_reset_failed"

	// 503 类
	codeNodesFileUnavailable = "nodes_file_unavailable"
)

// errInternalText 是 500 响应对外暴露的通用文案。
const errInternalText = "internal error"

// failInternal 处理 500：细节只进日志，客户端拿通用文案 + error_code + request_id。
func (s *Server) failInternal(w http.ResponseWriter, r *http.Request, code string, err error) {
	s.failInternalMsg(w, r, code, "", errInternalText, err)
}

// failInternalNode 同 failInternal，但额外带上 node_id 字段（策略类端点）。
func (s *Server) failInternalNode(w http.ResponseWriter, r *http.Request, code, nodeID string, err error) {
	s.failInternalMsg(w, r, code, nodeID, errInternalText, err)
}

// failInternalMsg 是 500 的通用实现：msg 是**面向用户**的说明（由调用方保证不含
// 内部细节，例如"策略已保存但应用失败"），原始 err 只写日志。
func (s *Server) failInternalMsg(w http.ResponseWriter, r *http.Request, code, nodeID, msg string, err error) {
	rid := requestID(r)
	logRequestError("API 内部错误", rid, code, nodeID, r, err)
	s.sendJSON(w, r, http.StatusInternalServerError, map[string]any{
		"error":      msg,
		"error_code": code,
		"request_id": rid,
	})
}

// failUnavailable 处理 503：msg 是**面向用户**的说明（不得含内部细节，由调用方保证），
// 原始错误只进日志。
func (s *Server) failUnavailable(w http.ResponseWriter, r *http.Request, code, nodeID, msg string, err error) {
	rid := requestID(r)
	logRequestError("API 依赖不可用", rid, code, nodeID, r, err)
	s.sendJSON(w, r, http.StatusServiceUnavailable, map[string]any{
		"error":      msg,
		"error_code": code,
		"request_id": rid,
	})
}
