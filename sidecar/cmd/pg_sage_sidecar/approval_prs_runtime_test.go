package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// cli-02b: the runner is built only when configured, and refuses to run
// without its token. No concurrent access test: built once at start.
func TestNewApprovalPRs_OffMissingTokenAndBuilt(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://sage@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	c := config.DefaultConfig()
	if prs, err := newApprovalPRs(c, pool, nil); prs != nil || err != nil {
		t.Fatalf("off by default: %v, %v", prs, err)
	}
	if prs, err := newApprovalPRs(nil, pool, nil); prs != nil || err != nil {
		t.Fatalf("nil config: %v, %v", prs, err)
	}
	c.Policy.Directory = t.TempDir()
	c.Policy.ApprovalPRs.Repo = "acme/config"
	t.Setenv(config.ApprovalPRsTokenEnv, "")
	if _, err := newApprovalPRs(c, pool, nil); err == nil ||
		!strings.Contains(err.Error(), config.ApprovalPRsTokenEnv) {
		t.Fatalf("missing token: err = %v, want it named", err)
	}
	t.Setenv(config.ApprovalPRsTokenEnv, "ghp_testtoken")
	prs, err := newApprovalPRs(c, pool, nil)
	if err != nil || prs == nil {
		t.Fatalf("configured: %v, %v", prs, err)
	}
	if prs.Config.Repo != "acme/config" || prs.Config.Path != "approvals/" ||
		prs.Config.Expiry.Hours() != 72 || prs.Users == nil || prs.Queue == nil {
		t.Fatalf("runner config = %+v", prs.Config)
	}
	c.Policy.ApprovalPRs.Provider = "gitea"
	if _, err := newApprovalPRs(c, pool, nil); err == nil ||
		!strings.Contains(err.Error(), "policy.approval_prs.provider") {
		t.Fatalf("unknown provider: err = %v", err)
	}
}
