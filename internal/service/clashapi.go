package service

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/firewall"
	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// singBoxBinary 定位 sing-box 可执行文件（与 api 层保持一致的查找顺序）。
func singBoxBinary() string {
	if v := os.Getenv("SBX_SB_BIN"); v != "" {
		return v
	}
	if p, err := exec.LookPath("sing-box"); err == nil {
		return p
	}
	return "/usr/local/bin/sing-box"
}

// hasClashAPI 判断配置里是否已经有可用的管理接口。
func hasClashAPI(data []byte) bool {
	var parsed struct {
		Experimental struct {
			ClashAPI struct {
				ExternalController string `json:"external_controller"`
			} `json:"clash_api"`
		} `json:"experimental"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return false
	}
	return parsed.Experimental.ClashAPI.ExternalController != ""
}

// ensureClashAPIOnStartup 确保 sing-box 暴露出可读连接表的管理接口。
//
// 为什么必须在这里做：配置文件平时只在「节点增删改」时重建。若用户升级 SBX 后
// 从没动过节点，配置里就永远不会出现管理接口，「节点访问域名」也就永远没数据。
//
// 只在**确实缺失**时执行一次：生成候选 → sing-box check → 备份 → 提交 → 重启。
// 任何失败都降级处理（记日志、继续启动）——这只是附加功能的数据源，不该因为它
// 让整个面板起不来。重启会短暂中断现有连接，但用户此刻刚重启过面板，影响一致。
func ensureClashAPIOnStartup(parent context.Context) {
	store := nodes.NewStore()
	data, err := os.ReadFile(store.SBConf)
	if err != nil {
		return // 配置不存在：交给其它流程处理
	}
	if hasClashAPI(data) {
		return
	}

	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	cli := &nodes.CLI{Store: store, Stdout: io.Discard, Stderr: io.Discard}
	if rc := cli.Run([]string{"sync"}); rc != 0 {
		slog.Warn("启用 sing-box 管理接口失败：无法生成候选配置（域名统计不可用）")
		return
	}
	cand := store.SBConf + ".candidate"
	if rc, _, stderr := firewall.RunCmd(ctx, singBoxBinary(), "check", "-c", cand); rc != 0 {
		_ = os.Remove(cand)
		slog.Warn("启用 sing-box 管理接口失败：候选配置校验未通过（域名统计不可用）",
			"err", trimErr(stderr))
		return
	}

	backup := store.SBConf + ".clashapi-bak"
	if err := copyFile(store.SBConf, backup); err != nil {
		_ = os.Remove(cand)
		slog.Warn("启用 sing-box 管理接口失败：无法备份原配置（域名统计不可用）")
		return
	}
	restore := func() {
		_ = os.Rename(backup, store.SBConf)
		_ = restartSingBoxForNodeMutation(context.Background())
	}
	if rc := cli.Run([]string{"commit"}); rc != 0 {
		_ = os.Remove(cand)
		restore()
		slog.Warn("启用 sing-box 管理接口失败：提交候选中止（已回滚）")
		return
	}
	if err := restartSingBoxForNodeMutation(ctx); err != nil {
		restore()
		slog.Warn("启用 sing-box 管理接口失败：sing-box 重启未成功（已回滚）", "err", err)
		return
	}
	_ = os.Remove(backup)
	slog.Info("已启用 sing-box 管理接口（用于「节点访问域名」统计）；sing-box 已重启，连接会短暂中断")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func trimErr(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max]
	}
	return s
}
