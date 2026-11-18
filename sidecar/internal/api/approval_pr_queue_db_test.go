package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/gitops"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// cli-02b on a real server: the queue approval PRs use reads the inbox's
// cards and decides through the app's approval path.
func TestApprovalPRQueue_ReadsCardsAndDecidesThroughTheAppPath(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	cfg := &config.Config{}
	cfg.Trust.Level = "autonomous"
	cfg.Trust.MaintenanceWindow = "always"
	exec := executor.New(pool, cfg, time.Now().Add(-40*24*time.Hour),
		func(string, string, ...any) {})
	exec.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: pool, Executor: exec})
	q := &ApprovalPRQueue{Mgr: mgr}
	as := store.NewActionStore(pool)
	propose := func(sql string) int {
		id, err := as.Propose(ctx, nil, insertActionHandlerFinding(t, pool, ctx,
			fmt.Sprintf("public.t_%d", time.Now().UnixNano())), sql, "", "safe")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first, second := propose("ANALYZE public.orders"), propose("ANALYZE public.items")
	items, err := q.Pending(ctx)
	if err != nil || len(items) != 2 {
		t.Fatalf("pending = %d items, %v; want 2", len(items), err)
	}
	a, _ := as.GetByID(ctx, first)
	hash, _ := approvalcard.CurrentHash(ctx, pool, *a)
	got := items[0]
	if got.Card.QueueID != first || got.Card.CardHash != hash || got.Card.Database != "orders" ||
		got.Card.SQL != "ANALYZE public.orders" || got.Tier == "" {
		t.Fatalf("first item = %+v (tier %s), want the card with hash %s", got.Card, got.Tier,
			hash)
	}
	if _, err := q.Item(ctx, "orders", 999999); !errors.Is(err, gitops.ErrItemNotFound) {
		t.Fatalf("missing item: err = %v, want ErrItemNotFound", err)
	}
	if _, err := q.Item(ctx, "ledger", first); err == nil ||
		errors.Is(err, gitops.ErrItemNotFound) {
		t.Fatalf("unmonitored database: err = %v, want a transient error", err)
	}
	if _, err := q.Approve(ctx, "orders", first, 0, "ANALYZE public.orders"); err == nil {
		t.Fatal("an approval without a user was accepted")
	}
	if _, err := q.Approve(ctx, "orders", second, 7, "ANALYZE public.other"); err == nil ||
		errors.Is(err, gitops.ErrAlreadyDecided) {
		t.Fatalf("approve with other SQL: err = %v, want a refusal", err)
	}
	if b, _ := as.GetByID(ctx, second); b.Status != "pending" {
		t.Fatalf("a refused approval moved the item to %s", b.Status)
	}
	out, err := q.Approve(ctx, "orders", first, 7, "ANALYZE public.orders")
	if err != nil || !strings.Contains(out, "orders") && !strings.Contains(out, "did not run") {
		t.Fatalf("approve = %q, %v; want the outcome on orders", out, err)
	}
	if a, _ := as.GetByID(ctx, first); a.Status == "pending" || a.DecidedBy == nil ||
		*a.DecidedBy != 7 {
		t.Fatalf("approved item = %s by %v, want decided by 7", a.Status, a.DecidedBy)
	}
	if _, err := q.Approve(ctx, "orders", first, 8, "ANALYZE public.orders"); !errors.Is(err,
		gitops.ErrAlreadyDecided) {
		t.Fatalf("second approval: err = %v, want ErrAlreadyDecided", err)
	}
	if err := q.Reject(ctx, "orders", second, 0, "closed in acme/cfg#2"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	b, _ := as.GetByID(ctx, second)
	if b.Status != "rejected" || b.Reason != "closed in acme/cfg#2" {
		t.Fatalf("rejected item = %s (%q)", b.Status, b.Reason)
	}
	if err := q.Reject(ctx, "orders", second, 0, "again"); !errors.Is(err,
		gitops.ErrAlreadyDecided) {
		t.Fatalf("second reject: err = %v, want ErrAlreadyDecided", err)
	}
	if _, err := q.Approve(ctx, "orders", second, 7, "ANALYZE public.items"); !errors.Is(err,
		gitops.ErrAlreadyDecided) {
		t.Fatalf("approve a rejected item: err = %v, want ErrAlreadyDecided", err)
	}
}
