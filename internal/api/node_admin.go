package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

const maxNodeMutationBody = 64 << 10

type nodeCreateRequest struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Port    int    `json:"port"`
	SNI     string `json:"sni"`
	Method  string `json:"method"`
	Version int    `json:"version"`
}

type nodeEditRequest struct {
	Port   *int   `json:"port"`
	SNI    string `json:"sni"`
	Method string `json:"method"`
	PSK    string `json:"psk"`
}

type mutationResponse struct {
	Node    nodes.PublicNodeDTO `json:"node"`
	Warning string              `json:"warning,omitempty"`
}

type nodeLinksResponse struct {
	ID    int64    `json:"id"`
	Name  string   `json:"name"`
	IPv4  string   `json:"ipv4"`
	IPv6  string   `json:"ipv6,omitempty"`
	Surge []string `json:"surge,omitempty"`
}

func decodeNodeBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxNodeMutationBody+1))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing data or oversized body")
	}
	return nil
}

func (s *Server) nodeStore() *nodes.Store {
	appDir := config.AppDir()
	sbConf := os.Getenv("SBX_SB_CONF")
	if sbConf == "" {
		sbConf = "/etc/sing-box/config.json"
	}
	return &nodes.Store{AppDir: appDir, SBConf: sbConf, NodesFile: s.cfg.NodesFile}
}

func (s *Server) nodeCLI(store *nodes.Store) (*nodes.CLI, *bytes.Buffer, *bytes.Buffer) {
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &nodes.CLI{Store: store, Stdout: out, Stderr: stderr}, out, stderr
}

func runNodeCLI(cli *nodes.CLI, args ...string) (string, error) {
	rc := cli.RunUnlocked(args)
	if rc == 0 {
		if b, ok := cli.Stdout.(*bytes.Buffer); ok {
			return strings.TrimSpace(b.String()), nil
		}
		return "", nil
	}
	if b, ok := cli.Stderr.(*bytes.Buffer); ok && b.Len() > 0 {
		return "", errors.New(strings.TrimSpace(b.String()))
	}
	return "", fmt.Errorf("node operation failed (exit %d)", rc)
}

func withNodeFileLock(ctx context.Context) (*os.File, error) {
	path := os.Getenv("SBX_LOCK")
	if path == "" {
		path = "/run/lock/sbx.lock"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("cannot prepare node lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open node lock: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, fmt.Errorf("cannot acquire node lock: %w", err)
		}
		t := time.NewTimer(40 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

type fileBackup struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func snapshotFile(path string) (fileBackup, error) {
	b := fileBackup{path: path}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return b, nil
		}
		return b, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return b, err
	}
	b.data, b.mode, b.exists = data, info.Mode().Perm(), true
	return b, nil
}

func (b fileBackup) restore() error {
	if b.exists {
		return fsx.WriteFileAtomic(b.path, b.data, b.mode)
	}
	if err := os.Remove(b.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func sanitizeRouteFinal(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	v, err := nodes.DecodeJSON(data)
	if err != nil {
		return err
	}
	cfg, ok := v.(map[string]any)
	if !ok {
		return errors.New("sing-box config must be an object")
	}
	rawOut, _ := cfg["outbounds"].([]any)
	tags := make(map[string]bool, len(rawOut))
	direct := ""
	for _, raw := range rawOut {
		ob, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if tag, ok := ob["tag"].(string); ok && tag != "" {
			tags[tag] = true
			if direct == "" && ob["type"] == "direct" {
				direct = tag
			}
		}
	}
	if route, ok := cfg["route"].(map[string]any); ok {
		final, _ := route["final"].(string)
		if final != "" && !tags[final] {
			if direct != "" {
				route["final"] = direct
			} else {
				delete(route, "final")
			}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, buf.Bytes(), 0o600)
}

func singBoxBinary() string {
	if p := os.Getenv("SBX_SB_BIN"); p != "" {
		return p
	}
	if p, err := exec.LookPath("sing-box"); err == nil {
		return p
	}
	return "/usr/local/bin/sing-box"
}

func runNodeCommand(ctx context.Context, args ...string) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return exec.CommandContext(cmdCtx, args[0], args[1:]...).CombinedOutput()
}

func validateSingBoxCandidate(ctx context.Context, path string) error {
	if _, err := runNodeCommand(ctx, singBoxBinary(), "check", "-c", path); err != nil {
		return errors.New("sing-box 配置校验失败；原节点和配置未改动")
	}
	return nil
}

func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	x := hex.EncodeToString(b[:])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:], nil
}

func (s *Server) newNodeCLI() (*nodes.CLI, *bytes.Buffer, *bytes.Buffer) {
	return s.nodeCLI(s.nodeStore())
}

func (s *Server) withNodeMutation(ctx context.Context, cli *nodes.CLI, args ...string) (string, string, error) {
	if s.nodeRestart == nil || s.nodeFwApply == nil {
		return "", "", errors.New("节点管理服务尚未就绪")
	}
	return s.mutateNodeFiles(ctx, cli, func() ([]string, error) { return args, nil })
}

func (s *Server) withNodeMutationBuild(ctx context.Context, cli *nodes.CLI, build func() ([]string, error)) (string, string, error) {
	if s.nodeRestart == nil || s.nodeFwApply == nil {
		return "", "", errors.New("节点管理服务尚未就绪")
	}
	return s.mutateNodeFiles(ctx, cli, build)
}

func (s *Server) mutationID(result, fallback string) string {
	var data map[string]any
	dec := json.NewDecoder(strings.NewReader(result))
	dec.UseNumber()
	if dec.Decode(&data) == nil {
		if v, ok := data["id"]; ok {
			return fmt.Sprint(v)
		}
	}
	return fallback
}

func (s *Server) publicNodeByID(id string) (nodes.PublicNodeDTO, error) {
	list, err := nodes.LoadPanelNodesStrict(s.cfg.NodesFile)
	if err != nil {
		return nodes.PublicNodeDTO{}, err
	}
	for _, n := range list {
		if nodes.IDString(n) == id {
			return nodes.PublicNodes([]nodes.Node{n})[0], nil
		}
	}
	return nodes.PublicNodeDTO{}, os.ErrNotExist
}

func uuidForNode() (string, error) { return randomUUID() }

func keyPairFromSingBox(ctx context.Context) (string, string, error) {
	out, err := runNodeCommand(ctx, singBoxBinary(), "generate", "reality-keypair")
	if err != nil {
		return "", "", errors.New("生成 Reality 密钥失败")
	}
	var privateKey, publicKey string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.Contains(fields[0], "PrivateKey") {
			privateKey = fields[len(fields)-1]
		}
		if strings.Contains(fields[0], "PublicKey") {
			publicKey = fields[len(fields)-1]
		}
	}
	if privateKey == "" || publicKey == "" {
		return "", "", errors.New("sing-box Reality 密钥输出格式不识别")
	}
	return privateKey, publicKey, nil
}

func ensureNodeCerts(ctx context.Context, store *nodes.Store, sni string) error {
	certPath, keyPath := filepath.Join(store.CertDir(), "cert.pem"), filepath.Join(store.CertDir(), "key.pem")
	if ci, ce := os.Stat(certPath); ce == nil && ci.Size() > 0 {
		if ki, ke := os.Stat(keyPath); ke == nil && ki.Size() > 0 {
			return nil
		}
	}
	if err := os.MkdirAll(store.CertDir(), 0o700); err != nil {
		return err
	}
	out, err := runNodeCommand(ctx, singBoxBinary(), "generate", "tls-keypair", sni, "-m", "1200")
	if err != nil {
		return errors.New("生成节点 TLS 证书失败")
	}
	cert, key := pemBlock(out, "CERTIFICATE"), pemBlock(out, "PRIVATE KEY")
	if len(cert) == 0 || len(key) == 0 {
		return errors.New("sing-box TLS 证书输出格式不识别")
	}
	if err := fsx.WriteFileAtomic(certPath, cert, 0o644); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(keyPath, key, 0o600)
}

func pemBlock(data []byte, label string) []byte {
	begin := []byte("-----BEGIN " + label + "-----")
	end := []byte("-----END " + label + "-----")
	i := bytes.Index(data, begin)
	if i < 0 {
		return nil
	}
	j := bytes.Index(data[i:], end)
	if j < 0 {
		return nil
	}
	j += i + len(end)
	return append(bytes.TrimSpace(data[i:j]), '\n')
}

func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	var req nodeCreateRequest
	if err := decodeNodeBody(r, &req); err != nil {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if !nodes.ValidType(req.Type) || req.Port < 1 || req.Port > 65535 {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "节点类型或端口无效"})
		return
	}
	cli, _, _ := s.newNodeCLI()
	result, warning, err := s.withNodeMutationBuild(r.Context(), cli, func() ([]string, error) {
		return s.newNodeArgs(r.Context(), cli.Store, req)
	})
	if err != nil {
		s.sendJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	id := s.mutationID(result, "")
	dto, err := s.publicNodeByID(id)
	if err != nil {
		s.failInternal(w, r, codeNodesFileUnavailable, err)
		return
	}
	s.invalidateCache()
	s.sendJSON(w, r, http.StatusOK, mutationResponse{Node: dto, Warning: warning})
}

func (s *Server) newNodeArgs(ctx context.Context, store *nodes.Store, req nodeCreateRequest) ([]string, error) {
	args := []string{"add", req.Type, "--port=" + strconv.Itoa(req.Port)}
	if strings.TrimSpace(req.Name) != "" {
		args = append(args, "--name="+strings.TrimSpace(req.Name))
	}
	sni := strings.TrimSpace(req.SNI)
	if sni == "" {
		if req.Type == "vless" {
			sni = "www.microsoft.com"
		}
		if req.Type == "trojan" || req.Type == "anytls" {
			sni = "www.bing.com"
		}
	}
	switch req.Type {
	case "vless":
		id, err := uuidForNode()
		if err != nil {
			return nil, errors.New("生成节点 UUID 失败")
		}
		priv, pub, err := keyPairFromSingBox(ctx)
		if err != nil {
			return nil, err
		}
		sid, err := nodes.GenerateHex(8)
		if err != nil {
			return nil, errors.New("生成 Reality short-id 失败")
		}
		args = append(args, "--uuid="+id, "--sni="+sni, "--flow=xtls-rprx-vision", "--private-key="+priv, "--public-key="+pub, "--short-id="+sid)
	case "shadowsocks":
		method := req.Method
		if method == "" {
			method = nodes.SS2022Method128
		}
		pw, err := nodes.GenerateSS2022Password(method)
		if err != nil {
			return nil, err
		}
		args = append(args, "--method="+method, "--password="+pw)
	case "trojan", "anytls":
		if err := ensureNodeCerts(ctx, store, sni); err != nil {
			return nil, err
		}
		pw, err := nodes.GenerateHex(12)
		if err != nil {
			return nil, errors.New("生成节点密码失败")
		}
		args = append(args, "--sni="+sni, "--password="+pw)
	case "snell":
		version := req.Version
		if version == 0 {
			version = 5
		}
		if version != 5 && version != 6 {
			return nil, errors.New("Snell 版本必须为 5 或 6")
		}
		psk, err := nodes.GenerateHex(32)
		if err != nil {
			return nil, errors.New("生成 Snell PSK 失败")
		}
		args = append(args, "--version="+strconv.Itoa(version), "--psk="+psk)
	}
	return args, nil
}

func parsePositiveNodeID(id string) (string, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return "", errors.New("invalid node id")
	}
	return strconv.FormatInt(n, 10), nil
}

func (s *Server) editNode(w http.ResponseWriter, r *http.Request, id string) {
	id, err := parsePositiveNodeID(id)
	if err != nil {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !s.requireNode(w, r, id) {
		return
	}
	var req nodeEditRequest
	if err := decodeNodeBody(r, &req); err != nil {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	args := []string{"edit", id}
	if req.Port != nil {
		if *req.Port < 1 || *req.Port > 65535 {
			s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "端口必须在 1–65535"})
			return
		}
		args = append(args, "--port="+strconv.Itoa(*req.Port))
	}
	if req.SNI != "" {
		args = append(args, "--sni="+strings.TrimSpace(req.SNI))
	}
	if req.Method != "" {
		args = append(args, "--method="+req.Method)
	}
	if req.PSK != "" {
		args = append(args, "--psk="+req.PSK)
	}
	if len(args) == 2 {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "请选择至少一个可修改字段"})
		return
	}
	cli, _, _ := s.newNodeCLI()
	_, warning, err := s.withNodeMutation(r.Context(), cli, args...)
	if err != nil {
		s.sendJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	dto, err := s.publicNodeByID(id)
	if err != nil {
		s.failInternal(w, r, codeNodesFileUnavailable, err)
		return
	}
	s.invalidateCache()
	s.sendJSON(w, r, http.StatusOK, mutationResponse{Node: dto, Warning: warning})
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request, id string) {
	id, err := parsePositiveNodeID(id)
	if err != nil {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	clearHistory := true
	if raw := qsGet(r, "clear_history"); raw != "" {
		if raw != "1" && raw != "0" {
			s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "clear_history must be 0 or 1"})
			return
		}
		clearHistory = raw == "1"
	}
	if !s.requireNode(w, r, id) {
		return
	}
	cli, _, _ := s.newNodeCLI()
	_, warning, err := s.withNodeMutation(r.Context(), cli, "remove", id)
	if err != nil {
		s.sendJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	historyCleared := false
	if clearHistory {
		if err := s.clearNodeDailyAndSampleHistory(r.Context(), id); err != nil {
			if warning != "" {
				warning += "; "
			}
			warning += "节点已删除，但每日/采样流量历史清理失败；累计流量已保留"
		} else {
			historyCleared = true
		}
	}
	s.invalidateCache()
	s.sendJSON(w, r, http.StatusOK, map[string]any{"deleted": id, "history_cleared": historyCleared, "cumulative_preserved": true, "warning": warning})
}

func (s *Server) clearNodeDailyAndSampleHistory(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	scope := "node:" + id
	for _, table := range []string{"daily", "samples"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE scope=?", scope); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) nodeLinks(w http.ResponseWriter, r *http.Request, id string) {
	id, err := parsePositiveNodeID(id)
	if err != nil {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !s.requireNode(w, r, id) {
		return
	}
	list, err := nodes.LoadPanelNodesStrict(s.cfg.NodesFile)
	if err != nil {
		s.failUnavailable(w, r, codeNodesFileUnavailable, "", "节点配置文件不可用", err)
		return
	}
	store := s.nodeStore()
	for _, n := range list {
		if nodes.IDString(n) != id {
			continue
		}
		resp := nodeLinksResponse{ID: int64(toI64(n["id"])), Name: nodes.DisplayName(n), IPv4: store.LinkFor(n, "", "")}
		if host6 := store.ShareHost6(); host6 != "" {
			resp.IPv6 = store.LinkFor(n, host6, "-IPv6")
		}
		if nodes.Str(n, "type") == "snell" {
			resp.Surge = []string{store.SnellSurgeFor(n, "", "")}
			if host6 := store.ShareHost6(); host6 != "" {
				resp.Surge = append(resp.Surge, store.SnellSurgeFor(n, host6, "-IPv6"))
			}
		}
		s.sendJSON(w, r, http.StatusOK, resp)
		return
	}
	s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
}

// tryNodeAdminRoute dispatches panel-only node CRUD/share routes.
func (s *Server) tryNodeAdminRoute(w http.ResponseWriter, r *http.Request, route string) bool {
	if route == "/api/nodes" && r.Method == http.MethodPost {
		s.createNode(w, r)
		return true
	}
	const prefix = "/api/nodes/"
	if !strings.HasPrefix(route, prefix) {
		return false
	}
	rest := strings.TrimPrefix(route, prefix)
	if strings.HasSuffix(rest, "/links") && r.Method == http.MethodGet {
		s.nodeLinks(w, r, strings.TrimSuffix(rest, "/links"))
		return true
	}
	if strings.Contains(rest, "/") {
		return false
	}
	switch r.Method {
	case http.MethodPut:
		s.editNode(w, r, rest)
		return true
	case http.MethodDelete:
		s.deleteNode(w, r, rest)
		return true
	default:
		return false
	}
}

func (s *Server) mutateNodeFiles(ctx context.Context, cli *nodes.CLI, build func() ([]string, error)) (result, warning string, err error) {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	lock, err := withNodeFileLock(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close() }()
	args, err := build()
	if err != nil {
		_ = cli.RunUnlocked([]string{"rollback"})
		return "", "", err
	}
	result, err = runNodeCLI(cli, args...)
	if err != nil {
		_ = cli.RunUnlocked([]string{"rollback"})
		return "", "", fmt.Errorf("节点配置候选生成失败：%w", err)
	}
	confCand := cli.Store.SBConf + ".candidate"
	if _, err = os.Stat(confCand); err != nil {
		_ = cli.RunUnlocked([]string{"rollback"})
		return "", "", errors.New("未生成 sing-box 候选配置")
	}
	if err = sanitizeRouteFinal(confCand); err != nil {
		_ = cli.RunUnlocked([]string{"rollback"})
		return "", "", errors.New("候选 sing-box 配置无效")
	}
	if err = validateSingBoxCandidate(ctx, confCand); err != nil {
		_ = cli.RunUnlocked([]string{"rollback"})
		return "", "", errors.New("sing-box 配置校验失败；原节点和配置未改动")
	}
	confBackup, err := snapshotFile(cli.Store.SBConf)
	if err != nil {
		_ = cli.RunUnlocked([]string{"rollback"})
		return "", "", errors.New("无法读取 sing-box 原配置，未执行修改")
	}
	nodesBackup, err := snapshotFile(cli.Store.NodesPath())
	if err != nil {
		_ = cli.RunUnlocked([]string{"rollback"})
		return "", "", errors.New("无法读取原节点配置，未执行修改")
	}
	written := []string{}
	for _, b := range []fileBackup{confBackup, nodesBackup} {
		if !b.exists {
			continue
		}
		if err := fsx.WriteFileAtomic(b.path+".bak", b.data, b.mode); err != nil {
			for _, p := range written {
				_ = os.Remove(p)
			}
			_ = cli.RunUnlocked([]string{"rollback"})
			return "", "", errors.New("无法安全备份原配置，未执行修改")
		}
		written = append(written, b.path+".bak")
	}
	cleanBackup := func() {
		for _, p := range written {
			_ = os.Remove(p)
		}
	}
	if _, err = runNodeCLI(cli, "commit"); err != nil {
		restoreConfErr := confBackup.restore()
		restoreNodesErr := nodesBackup.restore()
		_ = cli.RunUnlocked([]string{"rollback"})
		if restoreConfErr != nil || restoreNodesErr != nil {
			return "", "", errors.New("提交失败且自动回滚不完整；原配置备份仍保留为 .bak")
		}
		if s.policy != nil {
			_ = s.policy.Reconcile(context.Background())
		}
		cleanBackup()
		return "", "", errors.New("节点配置提交失败，原配置已恢复")
	}
	if err = s.nodeRestart(ctx); err != nil {
		restoreConfErr := confBackup.restore()
		restoreNodesErr := nodesBackup.restore()
		if restoreConfErr != nil || restoreNodesErr != nil {
			return "", "", errors.New("sing-box 重启失败且自动回滚不完整；原配置备份仍保留为 .bak")
		}
		rollbackRestartErr := s.nodeRestart(context.Background())
		if s.policy != nil {
			_ = s.policy.Reconcile(context.Background())
		}
		if rollbackRestartErr != nil {
			return "", "", errors.New("节点文件已回滚，但旧版 sing-box 未能重新启动；请检查服务日志")
		}
		cleanBackup()
		return "", "", errors.New("sing-box 重启失败，节点和配置已回滚")
	}
	cleanBackup()
	if err = s.nodeFwApply(ctx); err != nil {
		warning = "节点已生效，但流量计数规则未能重建；请稍后在系统设置与运维中重试"
	}
	if s.policy != nil {
		if e := s.policy.Reconcile(ctx); e != nil && warning == "" {
			warning = "节点已保存，但策略规则同步失败；请检查面板策略状态"
		}
	}
	return result, warning, nil
}
