package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 路径校验（审计项「路径遍历」）。
//
// 威胁模型已在 Validate 注释中说明：这些路径没有 HTTP 入口，可控入口只有本机
// root 的 config-set 与 panel.json。测试覆盖清单要求的三类攻击形态。
func TestValidateConfigPath(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"正常绝对路径", "/etc/sbx/nodes.json", false},
		{"正常绝对路径-深层", "/opt/sbx/data/traffic.db", false},
		{"自定义部署路径", "/srv/app/x.db", false},
		{"前缀相似不应误伤-devices", "/devices/x.db", false},
		{"前缀相似不应误伤-sysroot", "/sysroot/x.db", false},
		{"穿越-上一级", "/etc/sbx/../shadow", true},
		{"穿越-多级", "/etc/sbx/../../etc/shadow", true},
		{"穿越-尾部", "/etc/sbx/nodes.json/..", true},
		{"穿越-仅..", "/..", true},
		{"相对路径", "etc/sbx/nodes.json", true},
		{"相对路径-点点", "../nodes.json", true},
		{"冗余斜杠", "/etc/sbx//nodes.json", true},
		{"尾部斜杠", "/etc/sbx/", true},
		{"空串", "", true},
		{"伪文件系统-proc", "/proc/self/environ", true},
		{"伪文件系统-sys", "/sys/kernel/x", true},
		{"伪文件系统-dev", "/dev/sda", true},
		{"伪文件系统本身", "/proc", false}, // 仅 /proc/ 下拒绝；"/proc" 自身是绝对路径
	}
	for _, c := range cases {
		err := validateConfigPath("nodes_file", c.path)
		if c.wantErr && err == nil {
			t.Errorf("%s: %q 应被拒绝", c.name, c.path)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: %q 应通过, got %v", c.name, c.path, err)
		}
	}
}

// 端到端：损坏/恶意的 panel.json 必须让 LoadStrict fail-closed，而不是带着
// 危险路径继续启动（serve / config-set / apply 都走 LoadStrict）。
func TestLoadStrictRejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "panel.json")

	bad := []string{
		`{"db":"/etc/sbx/traffic.db","nodes_file":"../../etc/shadow","nft_conf":"/etc/sbx/nft.conf"}`,
		`{"db":"traffic.db","nodes_file":"/etc/sbx/nodes.json","nft_conf":"/etc/sbx/nft.conf"}`,
		`{"db":"/proc/self/mem","nodes_file":"/etc/sbx/nodes.json","nft_conf":"/etc/sbx/nft.conf"}`,
	}
	for _, content := range bad {
		if err := os.WriteFile(conf, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SBX_CONF", conf)
		if _, err := LoadStrict(); err == nil {
			t.Errorf("危险路径应被 LoadStrict 拒绝: %s", content)
		}
	}

	// 正常配置必须照常通过（不能误伤合法部署）
	good := `{"db":"` + dir + `/traffic.db","nodes_file":"` + dir + `/nodes.json","nft_conf":"` + dir + `/nft.conf"}`
	if err := os.WriteFile(conf, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SBX_CONF", conf)
	c, err := LoadStrict()
	if err != nil {
		t.Fatalf("合法配置不应被拒绝: %v", err)
	}
	if !strings.HasSuffix(c.NodesFile, "nodes.json") {
		t.Errorf("NodesFile 解析异常: %q", c.NodesFile)
	}
}
