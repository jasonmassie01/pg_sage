package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

type prQueueFixture struct {
	q    *ApprovalPRQueue
	as   *store.ActionStore
	pool *pgxpool.Pool
	ctx  context.Context
}

func newPRQueueFixture(t *testing.T) prQueueFixture {
	t.Helper()
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
	return prQueueFixture{q: &ApprovalPRQueue{Mgr: mgr}, as: store.NewActionStore(pool),
		pool: pool, ctx: ctx}
}

func (f prQueueFixture) propose(t *testing.T, sql string) int {
	t.Helper()
	id, err := f.as.Propose(f.ctx, nil, insertActionHandlerFinding(t, f.pool, f.ctx,
		fmt.Sprintf("public.t_%d", time.Now().UnixNano())), sql, "", "safe")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestApprovalPRQueue_ReadsTheInboxCards(t *testing.T) {
	f := newPRQueueFixture(t)
	first := f.propose(t, "ANALYZE public.orders")
	f.propose(t, "ANALYZE public.items")
	items, err := f.q.Pending(f.ctx)
	if err != nil || len(items) != 2 {
		t.Fatalf("pending = %d items, %v; want 2", len(items), err)
	}
	a, _ := f.as.GetByID(f.ctx, first)
	hash, _ := approvalcard.CurrentHash(f.ctx, f.pool, *a)
	got := items[0]
	if got.Card.QueueID != first || got.Card.CardHash != hash || got.Card.Database != "orders" ||
		got.Card.SQL != "ANALYZE public.orders" || got.Tier == "" {
		t.Fatalf("first item = %+v (tier %s), want the card with hash %s", got.Card, got.Tier,
			hash)
	}
	if _, err := f.q.Item(f.ctx, "orders", 999999); !errors.Is(err, gitops.ErrItemNotFound) {
		t.Fatalf("missing item: err = %v, want ErrItemNotFound", err)
	}
	if _, err := f.q.Item(f.ctx, "ledger", first); err == nil ||
		errors.Is(err, gitops.ErrItemNotFound) {
		t.Fatalf("unmonitored database: err = %v, want a transient error", err)
	}
}

func TestApprovalPRQueue_ApprovesOnceAndRefusesOtherSQL(t *testing.T) {
	f := newPRQueueFixture(t)
	first, second := f.propose(t, "ANALYZE public.orders"), f.propose(t, "ANALYZE public.items")
	if _, err := f.q.Approve(f.ctx, "orders", first, 0, "ANALYZE public.orders"); err == nil {
		t.Fatal("an approval without a user was accepted")
	}
	if _, err := f.q.Approve(f.ctx, "orders", second, 7, "ANALYZE public.other"); err == nil ||
		errors.Is(err, gitops.ErrAlreadyDecided) {
		t.Fatalf("approve with other SQL: err = %v, want a refusal", err)
	}
	if b, _ := f.as.GetByID(f.ctx, second); b.Status != "pending" {
		t.Fatalf("a refused approval moved the item to %s", b.Status)
	}
	out, err := f.q.Approve(f.ctx, "orders", first, 7, "ANALYZE public.orders")
	if err != nil || !strings.Contains(out, "orders") && !strings.Contains(out, "did not run") {
		t.Fatalf("approve = %q, %v; want the outcome on orders", out, err)
	}
	if a, _ := f.as.GetByID(f.ctx, first); a.Status == "pending" || a.DecidedBy == nil ||
		*a.DecidedBy != 7 {
		t.Fatalf("approved item = %s by %v, want decided by 7", a.Status, a.DecidedBy)
	}
	if _, err := f.q.Approve(f.ctx, "orders", first, 8, "ANALYZE public.orders"); !errors.Is(
		err, gitops.ErrAlreadyDecided) {
		t.Fatalf("second approval: err = %v, want ErrAlreadyDecided", err)
	}
}

func TestApprovalPRQueue_RejectsOnce(t *testing.T) {
	f := newPRQueueFixture(t)
	id := f.propose(t, "ANALYZE public.items")
	if err := f.q.Reject(f.ctx, "orders", id, 0, "closed in acme/cfg#2"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	b, _ := f.as.GetByID(f.ctx, id)
	if b.Status != "rejected" || b.Reason != "closed in acme/cfg#2" {
		t.Fatalf("rejected item = %s (%q)", b.Status, b.Reason)
	}
	if err := f.q.Reject(f.ctx, "orders", id, 0, "again"); !errors.Is(err,
		gitops.ErrAlreadyDecided) {
		t.Fatalf("second reject: err = %v, want ErrAlreadyDecided", err)
	}
	if _, err := f.q.Approve(f.ctx, "orders", id, 7, "ANALYZE public.items"); !errors.Is(err,
		gitops.ErrAlreadyDecided) {
		t.Fatalf("approve a rejected item: err = %v, want ErrAlreadyDecided", err)
	}
}

// The inbox's PR line: the PR row when there is one, app_only above the
// cap, nothing for a change PR mode will still open.
func TestApprovalPRNote_ShowsThePRRowOrAppOnly(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	clean := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM sage.approval_prs
			WHERE database = 'notes_db'`); err != nil {
			t.Fatal(err)
		}
	}
	clean()
	t.Cleanup(clean)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.approval_prs (database, queue_id, repo,
		path, card_hash, expires_at, state, pr_number, pr_url) VALUES ('notes_db', 5, 'o/r',
		'approvals/notes_db/5.yaml', 'h', now() + interval '1 day', 'open', 3,
		'https://example.invalid/o/r/pull/3')`); err != nil {
		t.Fatal(err)
	}
	SetApprovalPRs(&gitops.ApprovalPRs{Config: gitops.ApprovalPRConfig{Repo: "o/r",
		ChatCap: "safe"}}, pool)
	t.Cleanup(func() { SetApprovalPRs(nil, nil) })
	safe := &store.QueuedAction{ID: 5, Status: "pending", ActionRisk: "safe",
		ProposedSQL: "ANALYZE public.t", ActionType: "analyze_table"}
	n := approvalPRNote(ctx, "notes_db", approvalcard.Card{QueueID: 5}, safe)
	if n == nil || n.State != "open" || n.PRNumber != 3 || n.Repo != "o/r" ||
		n.PRURL != "https://example.invalid/o/r/pull/3" {
		t.Fatalf("note = %+v, want the open PR o/r#3", n)
	}
	moderate := &store.QueuedAction{ID: 6, Status: "pending", ActionRisk: "moderate",
		ProposedSQL: "ANALYZE public.t", ActionType: "analyze_table"}
	n = approvalPRNote(ctx, "notes_db", approvalcard.Card{QueueID: 6}, moderate)
	if n == nil || n.State != "app_only" || !strings.Contains(n.Detail, "chat approval cap") {
		t.Fatalf("note above the cap = %+v, want app_only naming the cap", n)
	}
	safe.ID = 7
	if n := approvalPRNote(ctx, "notes_db", approvalcard.Card{QueueID: 7}, safe); n != nil {
		t.Fatalf("a change PR mode will open has note %+v", n)
	}
	cards := []approvalcard.Card{{QueueID: 5, Database: "notes_db", Status: "pending",
		SQL: "ANALYZE public.t", Risk: approvalcard.Risk{Tier: "safe"}}}
	withApprovalPRNotes(ctx, cards)
	if cards[0].ApprovalPR == nil || cards[0].ApprovalPR.PRNumber != 3 {
		t.Fatalf("list note = %+v, want PR 3", cards[0].ApprovalPR)
	}
}
