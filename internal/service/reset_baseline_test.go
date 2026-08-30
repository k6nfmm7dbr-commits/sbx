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

// Reset 必须在同一事务里把配额基线清零：used = lifetime - baseline 且 clamp 到 0，
// totals 被删后 lifetime 归零，若基线仍停在旧高水位，配额要重新跑满该水位才生效。
func TestResetClearsQuotaBaseline(t *testing.T) {
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
	if err := svc.UpsertConfig(ctx, policy.Config{
		NodeID: "1", QuotaEnabled: true, QuotaLimitBytes: 1 << 30,
		QuotaResetBaseline: 100 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		"INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES('node:1',?,0,0,0)",
		int64(100)<<30); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if err := Reset("node:1"); err != nil {
		t.Fatal(err)
	}

	db2, err := database.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	svc2 := policy.New(db2.DB, dir, policy.DefaultPolicyConf(dir))
	got, err := svc2.GetConfig(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if got.QuotaResetBaseline != 0 {
		t.Errorf("reset 未清零配额基线, baseline=%d", got.QuotaResetBaseline)
	}
	// 配额本身的开关与额度必须保留（reset 只清统计，不改策略配置）
	if !got.QuotaEnabled || got.QuotaLimitBytes != 1<<30 {
		t.Errorf("reset 不应改动配额开关/额度: %+v", got)
	}
}

// 无 scope 的全量 reset 也要清所有基线。
func TestResetAllClearsAllBaselines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SBX_DIR", dir)
	t.Setenv("SBX_CONF", filepath.Join(dir, "panel.json"))
	if err := os.WriteFile(filepath.Join(dir, "nodes.json"),
		[]byte(`[{"id":1,"name":"n1","type":"vless","port":443},{"id":2,"name":"n2","type":"trojan","port":8443}]`), 0o600); err != nil {
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
	for _, id := range []string{"1", "2"} {
		if err := svc.UpsertConfig(ctx, policy.Config{
			NodeID: id, QuotaEnabled: true, QuotaLimitBytes: 1 << 30,
			QuotaResetBaseline: 50 << 30,
		}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	if err := Reset(""); err != nil {
		t.Fatal(err)
	}

	db2, err := database.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	svc2 := policy.New(db2.DB, dir, policy.DefaultPolicyConf(dir))
	for _, id := range []string{"1", "2"} {
		got, err := svc2.GetConfig(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.QuotaResetBaseline != 0 {
			t.Errorf("节点 %s 基线未清零: %d", id, got.QuotaResetBaseline)
		}
	}
}
