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
	"log/slog"
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
	"github.com/k6nfmm7dbr-commits/sbx/internal/ruleset"
)

const maxNodeMutationBody = 64 << 10

type nodeCreateRequest struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Port    int    `json:"port"`
	SNI     string `json:"sni"`
	Method  string `json:"method"`
	Version int    `json:"version"`
	// Allow 是「只允许访问的规则集」名单；空表示不限制。
	Allow []string `json:"allow"`
}

type nodeEditRequest struct {
	Port   *int   `json:"port"`
	SNI    string `json:"sni"`
	Method string `json:"method"`
	PSK    string `json:"psk"`
	// Allow 用指针区分「没传」与「传了空数组」：前者不改动，后者表示解除限制。
	Allow *[]string `json:"allow"`
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

// requireNodeForRepairMutation is intentionally narrower than requireNode: edit/delete
// may read a structurally valid nodes.json with duplicate ports so users can repair it.
// The CLI still validates the final candidate strictly before any commit or enforcement.
func (s *Server) requireNodeForRepairMutation(w http.ResponseWriter, r *http.Request, nodeID string) bool {
	list, err := nodes.LoadPanelNodesRepair(s.cfg.NodesFile)
	if err != nil {
		s.failUnavailable(w, r, codeNodesFileUnavailable, "",
			"节点配置文件不可用，无法安全修复", err)
		return false
	}
	for _, n := range list {
		if nodes.IDString(n) == nodeID {
			return true
		}
	}
	s.sendJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
	return false
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
	auditResult, auditNodeID := "failure", ""
	defer func() { auditEvent(r, "node_create", auditResult, "node_id", auditNodeID) }()
	var req nodeCreateRequest
	if err := decodeNodeBody(r, &req); err != nil {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if !nodes.ValidType(req.Type) || req.Port < 1 || req.Port > 65535 {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "节点类型或端口无效"})
		return
	}
	allow, aerr := ruleset.Validate(req.Allow)
	if aerr != nil {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": aerr.Error()})
		return
	}
	cli, _, _ := s.newNodeCLI()
	result, warning, err := s.withNodeMutationBuild(r.Context(), cli, func() ([]string, error) {
		return s.newNodeArgs(r.Context(), cli.Store, req, allow)
	})
	if err != nil {
		s.sendJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	id := s.mutationID(result, "")
	auditNodeID, auditResult = id, "success"
	dto, err := s.publicNodeByID(id)
	if err != nil {
		s.failInternal(w, r, codeNodesFileUnavailable, err)
		return
	}
	s.invalidateCache()
	s.sendJSON(w, r, http.StatusOK, mutationResponse{Node: dto, Warning: warning})
}

// nodeBatchCreateRequest 是批量创建的请求体。
// 端口可以指定起始值，也可以留空由服务端自动挑选连续空闲段。
type nodeBatchCreateRequest struct {
	Type       string `json:"type"`
	NamePrefix string `json:"name_prefix"`
	Count      int    `json:"count"`
	PortStart  int    `json:"port_start,omitempty"`
}

// listeningTCPPorts 返回本机当前处于 LISTEN 状态的 TCP 端口（v4+v6）。
// 批量建节点选端口时用它避开系统里已有的服务，防止把端口分给一个
// 已经被别人监听的号——那会让 sing-box check 通过但重启起不来。
func listeningTCPPorts() (map[int]bool, error) {
	out := map[int]bool{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // tcp6 不存在（未启用 IPv6）是正常情况
		}
		for i, line := range strings.Split(string(data), "\n") {
			if i == 0 || line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[3] != "0A" { // 0A = LISTEN
				continue
			}
			local := strings.Split(fields[1], ":")
			if len(local) != 2 {
				continue
			}
			p, err := strconv.ParseInt(local[1], 16, 32)
			if err == nil && p > 0 && p < 65536 {
				out[int(p)] = true
			}
		}
	}
	return out, nil
}

func (s *Server) createNodeBatch(w http.ResponseWriter, r *http.Request) {
	auditResult := "failure"
	defer func() { auditEvent(r, "node_batch_create", auditResult) }()
	var req nodeBatchCreateRequest
	if err := decodeNodeBody(r, &req); err != nil {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	req.NamePrefix = strings.TrimSpace(req.NamePrefix)
	if !nodes.ValidType(req.Type) {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "节点类型不支持"})
		return
	}
	if req.Count < 1 || req.Count > 50 {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "数量需在 1-50"})
		return
	}
	if req.NamePrefix == "" {
		req.NamePrefix = req.Type
	}
	if len(req.NamePrefix) > 40 {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "名称前缀过长（≤40 字符）"})
		return
	}

	list, err := nodes.LoadPanelNodesStrict(s.cfg.NodesFile)
	if err != nil {
		s.failUnavailable(w, r, codeNodesFileUnavailable, "", "节点配置文件不可用", err)
		return
	}
	usedPorts := map[int]bool{}
	for _, n := range list {
		for _, pr := range nodes.ParsePorts(n) {
			for p := pr[0]; p <= pr[1]; p++ {
				usedPorts[int(p)] = true
			}
		}
	}
	listeners, err := listeningTCPPorts()
	if err != nil {
		s.failInternal(w, r, codeNodesFileUnavailable, err)
		return
	}
	for p := range listeners {
		usedPorts[p] = true
	}

	// 分配 count 个互不冲突的端口。
	ports := make([]int, 0, req.Count)
	if req.PortStart > 0 {
		if req.PortStart < 1024 || req.PortStart+req.Count-1 > 65535 {
			s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "起始端口无效或区间越界（1024-65535）"})
			return
		}
		for i := 0; i < req.Count; i++ {
			p := req.PortStart + i
			if usedPorts[p] {
				s.sendJSON(w, r, http.StatusUnprocessableEntity,
					map[string]string{"error": fmt.Sprintf("端口 %d 已被占用；请换起始端口或留空自动分配", p)})
				return
			}
			ports = append(ports, p)
		}
	} else {
		for p := 30000; p <= 65535 && len(ports) < req.Count; p++ {
			if !usedPorts[p] {
				ports = append(ports, p)
			}
		}
		if len(ports) < req.Count {
			s.sendJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": "30000-65535 内找不到足够的空闲端口"})
			return
		}
	}

	store := s.nodeStore()
	sni := ""
	if req.Type == "trojan" || req.Type == "anytls" {
		sni = "www.bing.com"
		if err := ensureNodeCerts(r.Context(), store, sni); err != nil {
			s.sendJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
	}

	type spec = nodes.BatchNodeSpec
	specs := make([]spec, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		sp := spec{Type: req.Type, Port: int64(ports[i]), Name: fmt.Sprintf("%s-%d", req.NamePrefix, i+1)}
		switch req.Type {
		case "vless":
			uid, err := uuidForNode()
			if err != nil {
				s.failInternal(w, r, codeNodesFileUnavailable, err)
				return
			}
			priv, pub, err := keyPairFromSingBox(r.Context())
			if err != nil {
				s.sendJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": "生成 Reality 密钥失败"})
				return
			}
			sid, err := nodes.GenerateHex(8)
			if err != nil {
				s.failInternal(w, r, codeNodesFileUnavailable, err)
				return
			}
			sp.UUID, sp.SNI, sp.Flow, sp.PrivateKey, sp.PublicKey, sp.ShortID = uid, "www.microsoft.com", "xtls-rprx-vision", priv, pub, sid
		case "shadowsocks":
			pw, err := nodes.GenerateSS2022Password(nodes.SS2022Method128)
			if err != nil {
				s.sendJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": "生成密码失败"})
				return
			}
			sp.Method, sp.Password = nodes.SS2022Method128, pw
		case "trojan", "anytls":
			pw, err := nodes.GenerateHex(12)
			if err != nil {
				s.failInternal(w, r, codeNodesFileUnavailable, err)
				return
			}
			sp.SNI, sp.Password = sni, pw
		case "snell":
			psk, err := nodes.GenerateHex(32)
			if err != nil {
				s.failInternal(w, r, codeNodesFileUnavailable, err)
				return
			}
			sp.PSK, sp.Version = psk, 5
		}
		specs = append(specs, sp)
	}
	specJSON, err := json.Marshal(specs)
	if err != nil {
		s.failInternal(w, r, codeNodesFileUnavailable, err)
		return
	}

	cli, _, _ := s.newNodeCLI()
	result, warning, err := s.withNodeMutationBuild(r.Context(), cli, func() ([]string, error) {
		return []string{"add-batch", "--spec=" + string(specJSON)}, nil
	})
	if err != nil {
		s.sendJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	var out struct {
		Created []struct {
			ID   json.Number `json:"id"`
			Name string      `json:"name"`
			Port json.Number `json:"port"`
		} `json:"created"`
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		s.failInternal(w, r, codeNodesFileUnavailable, err)
		return
	}
	ids := make([]string, 0, len(out.Created))
	for _, c := range out.Created {
		ids = append(ids, c.ID.String())
	}
	auditResult = fmt.Sprintf("success:%d", len(ids))
	dt, err := s.publicNodesByIDs(ids)
	if err != nil {
		s.failInternal(w, r, codeNodesFileUnavailable, err)
		return
	}
	s.invalidateCache()
	s.sendJSON(w, r, http.StatusOK, map[string]any{"created": dt, "count": len(ids), "warning": warning})
}

func (s *Server) publicNodesByIDs(ids []string) ([]nodes.PublicNodeDTO, error) {
	list, err := nodes.LoadPanelNodesStrict(s.cfg.NodesFile)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make([]nodes.PublicNodeDTO, 0, len(ids))
	for _, n := range list {
		if want[nodes.IDString(n)] {
			out = append(out, nodes.PublicNodes([]nodes.Node{n})[0])
		}
	}
	return out, nil
}

// nodeBatchDeleteRequest：ids 必须全部存在，否则整体拒绝（与 CLI 语义一致）。
type nodeBatchDeleteRequest struct {
	IDs          []string `json:"ids"`
	ClearHistory bool     `json:"clear_history,omitempty"`
}

func (s *Server) deleteNodeBatch(w http.ResponseWriter, r *http.Request) {
	auditResult := "failure"
	defer func() { auditEvent(r, "node_batch_delete", auditResult) }()
	var req nodeBatchDeleteRequest
	if err := decodeNodeBody(r, &req); err != nil {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if len(req.IDs) < 1 || len(req.IDs) > 50 {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "ids 数量需在 1-50"})
		return
	}
	clean := make([]string, 0, len(req.IDs))
	seen := map[string]bool{}
	for _, id := range req.IDs {
		id, err := parsePositiveNodeID(strings.TrimSpace(id))
		if err != nil {
			s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if seen[id] {
			s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "重复的节点 id: " + id})
			return
		}
		seen[id] = true
		clean = append(clean, id)
	}
	names := make(map[string]string, len(clean))
	for _, id := range clean {
		names[id] = s.nodeDisplayName(id)
	}
	if !s.requireNodesForBatchMutation(w, r, clean) {
		return
	}
	cli, _, _ := s.newNodeCLI()
	joined := strings.Join(clean, ",")
	_, warning, err := s.withNodeMutation(r.Context(), cli, "remove-batch", joined)
	if err != nil {
		s.sendJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	auditResult = fmt.Sprintf("success:%d", len(clean))
	for _, id := range clean {
		s.rememberNodeName(id, names[id])
	}
	historyCleared := false
	if req.ClearHistory {
		for _, id := range clean {
			if err := s.clearNodeDailyAndSampleHistory(r.Context(), id); err != nil {
				if warning != "" {
					warning += "; "
				}
				warning += "节点 #" + id + " 的历史清理失败；累计流量已保留"
				continue
			}
			historyCleared = true
		}
	}
	s.invalidateCache()
	s.sendJSON(w, r, http.StatusOK, map[string]any{
		"deleted": clean, "history_cleared": historyCleared, "cumulative_preserved": true, "warning": warning,
	})
}

// requireNodesForBatchMutation 是 requireNodeForRepairMutation 的批量版：
// 全部存在才放行，任何一个不存在都整体拒绝。
func (s *Server) requireNodesForBatchMutation(w http.ResponseWriter, r *http.Request, ids []string) bool {
	existing := map[string]bool{}
	for _, n := range nodes.LoadPanelNodes(s.cfg.NodesFile) {
		existing[nodes.IDString(n)] = true
	}
	var missing []string
	for _, id := range ids {
		if !existing[id] {
			missing = append(missing, "#"+id)
		}
	}
	if len(missing) > 0 {
		s.sendJSON(w, r, http.StatusUnprocessableEntity,
			map[string]string{"error": "以下节点不存在，整体未改动: " + strings.Join(missing, ", ")})
		return false
	}
	return true
}

func (s *Server) newNodeArgs(ctx context.Context, store *nodes.Store, req nodeCreateRequest, allow []string) ([]string, error) {
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
	// 受限节点：只允许访问勾选的服务，其余一律拒绝。
	if len(allow) > 0 {
		args = append(args, "--allow="+strings.Join(allow, ","))
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
	auditResult := "failure"
	defer func() { auditEvent(r, "node_edit", auditResult, "node_id", id) }()
	if !s.requireNodeForRepairMutation(w, r, id) {
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
	if req.PSK != "" {
		args = append(args, "--psk="+req.PSK)
	}
	// 加密算法刻意**不支持修改**：换 method 必须同时换 password，而旧凭据早已
	// 分发到客户端，改了以后用户看到的是「节点突然连不上」，且无从判断原因。
	// 要换算法就删掉重建。前端已经不提交这个字段，这里再兜一层，防止直接调 API。
	if req.Allow != nil {
		allow, aerr := ruleset.Validate(*req.Allow)
		if aerr != nil {
			s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": aerr.Error()})
			return
		}
		// 空列表会拼成 --allow=，CLI 据此解除限制。
		args = append(args, "--allow="+strings.Join(allow, ","))
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
	auditResult = "success"
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
	auditResult := "failure"
	defer func() { auditEvent(r, "node_delete", auditResult, "node_id", id) }()
	// 默认保留历史：删除节点是"不再使用"，不是"抹掉账"。
	//
	// 每日明细与采样一律保留，累计流量本来就一直保留；只有显式传
	// clear_history=1 才清理每日与采样——那是给"节点建错了、要清干净"准备的。
	// 之所以把"保留"设为默认：删掉的历史无法恢复，而误留的数据随时能再清，
	// 两种代价完全不对等。
	clearHistory := false
	if raw := qsGet(r, "clear_history"); raw != "" {
		if raw != "1" && raw != "0" {
			s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "clear_history must be 0 or 1"})
			return
		}
		clearHistory = raw == "1"
	}
	// 删除前记下展示名：节点一旦从 nodes.json 移除，名字就再也找不回来，
	// 而保留下来的历史流量需要一个认得出的标题（否则只剩"节点 #1"）。
	name := s.nodeDisplayName(id)
	if !s.requireNodeForRepairMutation(w, r, id) {
		return
	}
	cli, _, _ := s.newNodeCLI()
	_, warning, err := s.withNodeMutation(r.Context(), cli, "remove", id)
	if err != nil {
		s.sendJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	auditResult = "success"
	s.rememberNodeName(id, name)

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

// nodeNameMetaKey 是"已删除节点的展示名"在 meta 表中的键前缀（后接节点 id）。
//
// 存在 meta 里而不是新建表：这只是一个展示用的补充信息，值很小、只增不改，
// meta 本来就是通用 KV，为它单独建表并不划算。
const nodeNameMetaKey = "node_name:"

// nodeDisplayName 返回节点的展示名；找不到或读取失败时返回空串。
//
// 走宽松读取（LoadPanelNodes）：这只是删除时的一次留档，
// 节点文件有问题不该反过来拦住删除动作本身。
func (s *Server) nodeDisplayName(id string) string {
	for _, n := range nodes.LoadPanelNodes(s.cfg.NodesFile) {
		if nodes.IDString(n) == id {
			return nodes.DisplayName(n)
		}
	}
	return ""
}

// rememberNodeName 记下已删除节点的展示名，供「历史节点」列表显示。
// 失败只记日志：留档失败不该让删除动作整体失败。
func (s *Server) rememberNodeName(id, name string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	if _, err := s.db.Exec(
		"INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v",
		nodeNameMetaKey+id, name); err != nil {
		slog.Warn("记录已删除节点名称失败, 历史列表将只显示编号",
			"node_id", id, "err", err)
	}
}

func (s *Server) nodeLinks(w http.ResponseWriter, r *http.Request, id string) {
	id, err := parsePositiveNodeID(id)
	if err != nil {
		s.sendJSON(w, r, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !s.requireNodeForRepairMutation(w, r, id) {
		return
	}
	list, err := nodes.LoadPanelNodesRepair(s.cfg.NodesFile)
	if err != nil {
		s.failUnavailable(w, r, codeNodesFileUnavailable, "", "节点配置文件不可用", err)
		return
	}
	if len(nodes.PortConflicts(list)[toI64(id)]) > 0 {
		s.sendJSON(w, r, http.StatusConflict, map[string]string{"error": "该节点端口与其他节点重复，请先编辑或删除冲突节点"})
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
	if route == "/api/nodes/batch" && r.Method == http.MethodPost {
		s.createNodeBatch(w, r)
		return true
	}
	if route == "/api/nodes/delete-batch" && r.Method == http.MethodPost {
		s.deleteNodeBatch(w, r)
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
	// 规则集必须在生成候选之前就位：候选配置引用的是本地规则集路径，缺文件会让
	// sing-box 起不来——那是全节点一起挂，而不是单个节点不可用。
	if err = s.ensureRuleSets(ctx, args); err != nil {
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
