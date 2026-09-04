// Package firewall 实现基于 nftables 的流量计数后端：named counter 读取
// （exec `nft -j list counters`）与计数规则文件生成。
//
// 架构（v3.0.9 起）：**nftables-only**。不存在后端选择、不存在 iptables 回退。
// nft 不可用时明确失败（fail-closed），绝不静默降级。
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
	// NFTTable 是 nftables 计数表名。
	NFTTable = "sbx_traffic"

	// CTActivateCounter 是 conntrack 激活链使用的计数器名。
	// 刻意不匹配 ParseCounterName 的 `sbx_(n<id>|sys)_(i|o)` 形态，
	// 因此不会被采集器当成流量计数入账（它只是让 ct hook 有个合法动作）。
	CTActivateCounter = "sbx_ct_activate"

	execTimeout = 15 * time.Second
)

// RunCmd 执行外部命令，返回 (rc, stdout, stderr)。找不到命令时 rc=127，
// 超时 rc=124。参数直接列表传入，用户数据永远不进入 shell。
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

// ErrLookup 表示计数器/规则不存在（可自愈）。
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
//
// 生产实现只有一个：*Nft（见 nft.go）。保留 interface 是为了让 Collector 的
// 单元测试能注入 fake backend 做故障注入（读失败 / 部分快照 / Repair 行为），
// 而不是为了支持多种后端——绝不要重新引入“运行时选择后端”。
type Backend interface {
	Name() string
	Read(ctx context.Context) (Snapshot, error)
	Repair(ctx context.Context) error
}

// BackendName 是对外报告的后端名称，恒为 "nft"。
// 保留该常量是为了 /api/summary 的 `backend` 字段保持 schema 兼容。
const BackendName = "nft"

// NftAvailable 报告 nftables 是否真正可用：nft 命令在 PATH 且能列出表
// （能列表说明有权限且内核支持）。nftables-only 架构下这是硬前置条件，
// 不可用时调用方必须明确失败，绝不降级到其它后端。
func NftAvailable(ctx context.Context) bool {
	if !whichFn("nft") {
		return false
	}
	rc, _, _ := runCmdFn(ctx, "nft", "list", "tables")
	return rc == 0
}

// IsMissingMsg 判断 nft 错误信息是否为「目标不存在」类（可自愈 / 对删除即成功）。
// 供 nft.go 分类 ErrLookup，以及 service.Clear 判断「删除时表本就不存在」。
func IsMissingMsg(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "no such file or directory") ||
		strings.Contains(m, "does not exist") ||
		strings.Contains(m, "no such table") ||
		strings.Contains(m, "no such chain")
}
