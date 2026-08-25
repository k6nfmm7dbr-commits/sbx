package firewall

import (
	"os"
	"path/filepath"
	"testing"
)

// 用 SBX_RUN_DIR 隔离状态文件，避免污染真实 /run/sbx。
func stateEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SBX_RUN_DIR", dir)
	return dir
}

func TestEffectiveBackendPersistRoundtrip(t *testing.T) {
	stateEnv(t)
	if err := WriteEffectiveBackend("nft"); err != nil {
		t.Fatalf("写入 nft: %v", err)
	}
	if b, ok := ReadEffectiveBackend(); !ok || b != "nft" {
		t.Fatalf("读回 nft 失败: %q %v", b, ok)
	}
	if err := WriteEffectiveBackend("iptables"); err != nil {
		t.Fatalf("写入 iptables: %v", err)
	}
	if b, ok := ReadEffectiveBackend(); !ok || b != "iptables" {
		t.Fatalf("读回 iptables 失败: %q %v", b, ok)
	}
}

func TestEffectiveBackendCorruption(t *testing.T) {
	stateEnv(t)
	p := effectiveBackendPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadEffectiveBackend(); ok {
		t.Fatal("损坏的状态文件必须视为不可用")
	}
}

func TestEffectiveBackendForcedAndAuto(t *testing.T) {
	stateEnv(t)
	// forced 不读状态
	if got := EffectiveBackend("nft"); got != "nft" {
		t.Fatalf("forced nft: %q", got)
	}
	if got := EffectiveBackend("iptables"); got != "iptables" {
		t.Fatalf("forced iptables: %q", got)
	}
	// auto 无状态时走探测（这里不依赖真实 nft/iptables 是否安装，只断言返回合法值）
	if got := EffectiveBackend("auto"); got != "nft" && got != "iptables" {
		t.Fatalf("auto 探测返回非法后端: %q", got)
	}
	// auto 有状态时优先读状态
	_ = WriteEffectiveBackend("iptables")
	if got := EffectiveBackend("auto"); got != "iptables" {
		t.Fatalf("auto 应读持久化状态: %q", got)
	}
}

func TestIsAutoBackend(t *testing.T) {
	for forced, want := range map[string]bool{
		"auto": true, "": true, "nft": false, "nftables": false, "ipt": false, "iptables": false,
	} {
		if got := IsAutoBackend(forced); got != want {
			t.Errorf("IsAutoBackend(%q)=%v want %v", forced, got, want)
		}
	}
}

func TestWriteEffectiveBackendRejectsInvalid(t *testing.T) {
	stateEnv(t)
	if err := WriteEffectiveBackend("bogus"); err == nil {
		t.Fatal("非法后端必须报错")
	}
}

func TestIsMissingMsg(t *testing.T) {
	for msg, want := range map[string]bool{
		"No such file or directory": true,
		"does not exist":            true,
		"no such table":             true,
		"no such chain":             true,
		"permission denied":         false,
		"operation not permitted":   false,
		"syntax error":              false,
	} {
		if got := IsMissingMsg(msg); got != want {
			t.Errorf("IsMissingMsg(%q)=%v want %v", msg, got, want)
		}
	}
}
