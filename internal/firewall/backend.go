// Package firewall 实现流量计数后端：nftables named counter（exec nft -j）
// 与 iptables 自定义链回退，以及计数规则文件的生成。
package firewall

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

const (
	// NFTTable 是 nftables 计数表名，与旧实现一致。
	NFTTable = "sbx_traffic"
	// IptChainIn / IptChainOut 是 iptables 自定义链名。
	IptChainIn  = "SBX_IN"
	IptChainOut = "SBX_OUT"

	execTimeout = 15 * time.Second
)

// RunCmd 执行外部命令，返回 (rc, stdout, stderr)。找不到命令时 rc=127，
// 超时 rc=124 —— 对齐旧 run_cmd 的约定。参数直接列表传入，
// 用户数据永远不进入 shell。
func RunCmd(ctx context.Context, args ...string) (int, string, string) {
	if len(args) == 0 {
		return 127, "", "empty command"
	}
	cctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, args[0], args[1:]...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	rc := 0
	switch {
	case err == nil:
	case cctx.Err() == context.DeadlineExceeded:
		rc = 124
	case isNotFound(err):
		rc = 127
	default:
		rc = 1
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
			if rc < 0 {
				rc = 1
			}
		}
	}
	return rc, out.String(), errBuf.String()
}

func isNotFound(err error) bool {
	if _, ok := err.(*exec.Error); ok {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "executable file not found")
}

// Which 报告可执行文件是否存在于 PATH。
func Which(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// ErrLookup 表示计数器/规则不存在（可自愈），等价旧 LookupError。
type ErrLookup struct{ Msg string }

func (e *ErrLookup) Error() string { return e.Msg }

// IsLookup 判断错误是否为“不存在”类。
func IsLookup(err error) bool {
	var le *ErrLookup
	return errors.As(err, &le)
}

// 测试钩子：允许单测注入假命令执行与 PATH 检测。
var (
	runCmdFn = RunCmd
	whichFn  = Which
)

// Snapshot 是一次计数器读取结果：name -> [bytes, packets]。
type Snapshot map[string][2]int64

// Backend 是计数器读取后端接口。
type Backend interface {
	Name() string
	Read(ctx context.Context) (Snapshot, error)
	Repair(ctx context.Context) error
}

// DetectBackend 对齐 detect_backend（纯探测，不读持久化状态）：
// 配置强制 nft/ipt；auto 时优先探测 nft（`nft list tables` 成功），
// 再看 iptables；两者都缺失仍返回 nft（由后续 Read 报错自愈）。
// 注意：会改变系统状态的路径请用 EffectiveBackend（单一事实源），
// 不要用这个纯探测结果——否则 auto 下可能与 Apply 实际后端分叉。
func DetectBackend(forced string) string {
	return probeBackendForced(forced)
}

// probeBackendForced：forced=nft/iptables 直接返回；auto 走 probeBackend。
func probeBackendForced(forced string) string {
	switch normalizeBackend(forced) {
	case "nft":
		return "nft"
	case "iptables":
		return "iptables"
	}
	return probeBackend()
}

// New 按 EffectiveBackend 构造后端（Collector 通过这里读取与 Apply 一致的后端）。
func New(forced string, nftConf, iptScript string) Backend {
	if EffectiveBackend(forced) == "nft" {
		return NewNft(nftConf)
	}
	return NewIptables(iptScript)
}
