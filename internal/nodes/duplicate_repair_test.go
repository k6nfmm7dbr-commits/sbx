package nodes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const duplicateNodeFixture = `[
  {"id":1,"name":"first","type":"shadowsocks","port":443,"method":"2022-blake3-aes-128-gcm","password":"pw-one"},
  {"id":2,"name":"second","type":"shadowsocks","port":443,"method":"2022-blake3-aes-128-gcm","password":"pw-two"}
]`

func TestRepairLoadAllowsOnlyPortDuplicates(t *testing.T) {
	p := strictPath(t)
	writeFixture(t, p, duplicateNodeFixture)
	if _, err := LoadToolNodesStrict(p); err == nil {
		t.Fatal("strict loader must reject duplicate ports")
	}
	list, err := LoadToolNodesRepair(p)
	if err != nil || len(list) != 2 {
		t.Fatalf("repair loader should expose both otherwise-valid records: len=%d err=%v", len(list), err)
	}
	panelList, err := LoadPanelNodesRepair(p)
	if err != nil || len(panelList) != 2 {
		t.Fatalf("panel recovery loader failed: len=%d err=%v", len(panelList), err)
	}
	if _, err := LoadPanelNodesStrict(p); err == nil {
		t.Fatal("strict panel loader must remain fail-closed")
	}
	conflicts := PortConflicts(list)
	if len(conflicts[1]) != 1 || conflicts[1][0] != 2 || len(conflicts[2]) != 1 || conflicts[2][0] != 1 {
		t.Fatalf("unexpected conflict map: %#v", conflicts)
	}

	writeFixture(t, p, `[{"id":1,"name":"bad","type":"vless","port":443},{"id":1,"name":"duplicate-id","type":"vless","port":444}]`)
	if _, err := LoadToolNodesRepair(p); err == nil || !strings.Contains(err.Error(), "节点 id 重复") {
		t.Fatalf("repair loader must still reject duplicate IDs, err=%v", err)
	}
}

func TestCLIAddRejectsDuplicatePort(t *testing.T) {
	cli, store, dir := newTestCLI(t)
	t.Setenv("SBX_LOCK", filepath.Join(dir, "test.lock"))
	first := []string{"add", "shadowsocks", "--port", "443", "--method", SS2022Method128, "--password", "pw-one"}
	if rc := cli.run(first); rc != exitOK {
		t.Fatalf("first add failed: %s", cli.err())
	}
	if rc := cli.run([]string{"commit"}); rc != exitOK {
		t.Fatalf("first commit failed: %s", cli.err())
	}
	if rc := cli.run([]string{"add", "shadowsocks", "--port", "443", "--method", SS2022Method128, "--password", "pw-two"}); rc == exitOK {
		t.Fatal("adding a duplicate port must fail")
	}
	if !strings.Contains(cli.err(), "节点端口重复: 443") {
		t.Fatalf("duplicate diagnosis missing: %q", cli.err())
	}
	if _, err := os.Stat(store.NodesPath() + ".candidate"); !os.IsNotExist(err) {
		t.Fatalf("invalid duplicate candidate must not be written: %v", err)
	}
}

func TestCLIEditAndRemoveRepairDuplicatePorts(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "edit", args: []string{"edit", "2", "--port", "30012"}},
		{name: "remove", args: []string{"remove", "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, store, dir := newTestCLI(t)
			t.Setenv("SBX_LOCK", filepath.Join(dir, "test.lock"))
			if err := SaveNodesFile(store.NodesPath(), []Node{
				{"id": int64(1), "name": "first", "type": "shadowsocks", "port": int64(443), "method": SS2022Method128, "password": "pw-one"},
				{"id": int64(2), "name": "second", "type": "shadowsocks", "port": int64(443), "method": SS2022Method128, "password": "pw-two"},
			}); err != nil {
				t.Fatal(err)
			}
			if rc := cli.run(tc.args); rc != exitOK {
				t.Fatalf("repair %s failed: %s", tc.name, cli.err())
			}
			cand, err := LoadToolNodesStrict(store.NodesPath() + ".candidate")
			if err != nil {
				t.Fatalf("final candidate must be strictly valid: %v", err)
			}
			wantLen := 1
			if tc.name == "edit" {
				wantLen = 2
			}
			if len(cand) != wantLen {
				t.Fatalf("expected %d nodes after %s, got %d", wantLen, tc.name, len(cand))
			}
			if tc.name == "edit" && Str(cand[1], "port") != "30012" {
				t.Fatalf("edited port not persisted: %#v", cand[1])
			}
		})
	}
}
