package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/policy"
)

func TestResetPreservesPausedAndOtherNodePolicies(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SBX_DIR", dir)
	t.Setenv("SBX_CONF", filepath.Join(dir, "panel.json"))
	if err := os.WriteFile(filepath.Join(dir, "nodes.json"),
		[]byte(`[{"id":1,"name":"n1","type":"vless","port":443}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadStrict()
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	svc := policy.New(db.DB, dir, policy.DefaultPolicyConf(dir))
	want := policy.Config{NodeID: "1", Paused: true, IPLimitEnabled: true, IPLimitMax: 3, RateLimitEnabled: true, RateLimitMbps: 50}
	if err := svc.UpsertConfig(ctx, want); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES('node:1',10,20,0,0)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := Reset("node:1"); err != nil {
		t.Fatal(err)
	}

	db2, err := database.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	got, err := policy.New(db2.DB, dir, policy.DefaultPolicyConf(dir)).GetConfig(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("statistics reset must preserve paused/IP/rate policy: got %+v want %+v", got, want)
	}
	var count int
	if err := db2.QueryRow("SELECT COUNT(*) FROM totals WHERE scope='node:1'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("statistics reset must still clear totals: count=%d", count)
	}
}
