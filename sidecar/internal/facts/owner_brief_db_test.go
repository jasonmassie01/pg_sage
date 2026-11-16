package facts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// evid-03a on a real server: one owner brief per database (the unique
// subject key), re-declared in place, never expired for absence, never
// binding, and measured by BriefOutcome.

func declareBrief(t *testing.T, ctx context.Context, s *Store,
	value map[string]string) Fact {
	t.Helper()
	f, err := s.Declare(ctx, briefProposal(value), "alice@example.com", "from the owner")
	if err != nil {
		t.Fatalf("declare brief %v: %v", value, err)
	}
	return f
}

func briefRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.facts
		WHERE fact_type = 'owner_brief'`).Scan(&n); err != nil {
		t.Fatalf("count briefs: %v", err)
	}
	return n
}

func TestOwnerBriefDB_DeclareListPrompt(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f := declareBrief(t, ctx, s, map[string]string{"owner": "Payments",
		"peak_windows": "weekdays 09:00-17:00 America/Chicago", "growth": "20 GB/month"})
	if f.Status != StatusConfirmed || f.Kind != KindDatabase || f.Subject != "*" ||
		f.DecidedBy != "alice@example.com" || f.Proposals != 1 {
		t.Fatalf("declared brief = %+v", f)
	}
	listed, err := s.List(ctx, Filter{Type: TypeOwnerBrief})
	if err != nil || len(listed) != 1 || listed[0].ID != f.ID ||
		listed[0].Value["growth"] != "20 GB/month" {
		t.Fatalf("list owner briefs = %+v, %v", listed, err)
	}
	declareSlot(t, ctx, s, uniqueName("cdc_"))
	lines := s.PromptLines(ctx)
	if len(lines) != 2 || !strings.Contains(lines[0], "owner brief: owner Payments") {
		t.Fatalf("prompt lines = %q, want the brief first", lines)
	}
}

func TestOwnerBriefDB_RedeclareReplacesInPlace(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	first := declareBrief(t, ctx, s, map[string]string{"owner": "Payments",
		"note": "old"})
	execAll(t, ctx, pool, `UPDATE sage.facts SET decided_at = now() - interval '3 days'
		WHERE fact_type = 'owner_brief'`)
	backdated := mustGet(t, ctx, s, first.ID).DecidedAt
	second, err := s.Declare(ctx, briefProposal(map[string]string{"owner": "Ledger"}),
		"bob@example.com", "")
	if err != nil {
		t.Fatalf("re-declare: %v", err)
	}
	if second.ID != first.ID || second.Proposals != 2 || second.DecidedBy !=
		"bob@example.com" || second.Value["owner"] != "Ledger" || len(second.Value) != 1 {
		t.Fatalf("re-declared brief = %+v, want the same row replaced", second)
	}
	if !second.DecidedAt.After(backdated.Add(2 * day)) {
		t.Fatalf("decided_at %v did not move past the backdated %v", second.DecidedAt,
			backdated)
	}
	if n := briefRows(t, ctx, pool); n != 1 {
		t.Fatalf("%d brief rows, want 1", n)
	}
}

// Error propagation: a declaration whose transaction fails leaves the
// earlier brief's value and decision as they were.
func TestOwnerBriefDB_FailedDeclarationKeepsEarlierBrief(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	first := declareBrief(t, ctx, s, map[string]string{"owner": "Payments"})
	execAll(t, ctx, pool, `CREATE OR REPLACE FUNCTION sage.evid03a_refuse() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
		  IF NEW.value->>'owner' = 'boom' THEN RAISE EXCEPTION 'evid03a refused'; END IF;
		  RETURN NEW; END $$`,
		`CREATE TRIGGER evid03a_refuse BEFORE UPDATE ON sage.facts
		 FOR EACH ROW EXECUTE FUNCTION sage.evid03a_refuse()`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DROP TRIGGER IF EXISTS evid03a_refuse ON sage.facts`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS sage.evid03a_refuse()`)
	})
	_, err := s.Declare(ctx, briefProposal(map[string]string{"owner": "boom"}),
		"mallory@example.com", "")
	if err == nil || !strings.Contains(err.Error(), "declare fact") ||
		!strings.Contains(err.Error(), "evid03a refused") {
		t.Fatalf("failed declaration: err = %v", err)
	}
	got := mustGet(t, ctx, s, first.ID)
	if got.Value["owner"] != "Payments" || got.DecidedBy != "alice@example.com" ||
		got.Proposals != 1 || !got.DecidedAt.Equal(*first.DecidedAt) {
		t.Fatalf("brief after a failed declaration = %+v", got)
	}
}

// Concurrency: two declarations at once leave one row holding one of the
// two values whole, with proposals = 2.
func TestOwnerBriefDB_ConcurrentDeclarations(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	values := []map[string]string{
		{"owner": "Payments", "note": "a"},
		{"owner": "Ledger", "contact": "#ledger", "growth_pct_month": "3"},
	}
	var wg sync.WaitGroup
	errs := make([]error, len(values))
	for i, v := range values {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Declare(ctx, briefProposal(v), "op@example.com", "")
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("concurrent declarations: %v", err)
	}
	all, err := s.List(ctx, Filter{Type: TypeOwnerBrief})
	if err != nil || len(all) != 1 {
		t.Fatalf("briefs = %+v, %v; want exactly one", all, err)
	}
	got := all[0]
	whole := false
	for _, v := range values {
		whole = whole || (len(got.Value) == len(v) && got.Value["owner"] == v["owner"] &&
			got.Value["note"] == v["note"] && got.Value["contact"] == v["contact"])
	}
	if !whole || got.Proposals != 2 {
		t.Fatalf("brief = %v (proposals %d), want one value whole and 2", got.Value,
			got.Proposals)
	}
}

// countingCatalog fails every exists-query, as an unreachable catalog
// would, and counts them.
type countingCatalog struct{ calls *atomic.Int32 }

func (c countingCatalog) QueryRow(context.Context, string, ...any) pgx.Row {
	c.calls.Add(1)
	return errRow{err: errors.New("dial tcp: connection refused")}
}

func TestOwnerBriefDB_ReverifyNeverExpiresForAbsence(t *testing.T) {
	pool, ctx := livePool(t)
	calls := &atomic.Int32{}
	s := NewStore(pool).WithMissingGrace(0).WithConfirmedAbsence(0)
	s.catalog = countingCatalog{calls: calls}
	f := declareBrief(t, ctx, s, map[string]string{"owner": "Payments"})
	for pass := 0; pass < 2; pass++ {
		expired, err := s.Reverify(ctx, time.Now().Add(time.Hour))
		if err != nil || len(expired) != 0 {
			t.Fatalf("pass %d: expired %v, err %v", pass, expired, err)
		}
	}
	got := mustGet(t, ctx, s, f.ID)
	if got.Status != StatusConfirmed || got.MissingSince != nil ||
		got.LastVerifiedAt == nil {
		t.Fatalf("brief after re-verification = %+v", got)
	}
	if calls.Load() != 0 {
		t.Fatalf("%d catalog lookups for a database fact, want none", calls.Load())
	}
}

// briefTransitions drives one brief's states and checks Bind at each.
type briefTransitions struct {
	t    *testing.T
	ctx  context.Context
	s    *Store
	refs []ObjectRef
	req  Request
}

func (b briefTransitions) noBind(stage string, f Fact) {
	b.t.Helper()
	if got := Bind([]Fact{f}, b.refs, b.req); len(got) != 0 {
		b.t.Fatalf("%s: the brief bound: %+v", stage, got)
	}
}

func (b briefTransitions) propose(owner string) Fact {
	b.t.Helper()
	p := briefProposal(map[string]string{"owner": owner})
	p.Evidence = []Citation{{Kind: "mcp", Ref: "agent", Detail: "from the runbook"}}
	f, _, err := b.s.Propose(b.ctx, p)
	if err != nil || f.Status != StatusProposed {
		b.t.Fatalf("propose %s: %+v %v", owner, f, err)
	}
	b.noBind("proposed", f)
	return f
}

func (b briefTransitions) decide(id int64, confirm bool, want Status) Fact {
	b.t.Helper()
	f, err := b.s.Decide(b.ctx, id, Decision{Confirm: confirm, Actor: "alice@example.com"})
	if err != nil || f.Status != want {
		b.t.Fatalf("decide %d (confirm %v): %+v %v", id, confirm, f, err)
	}
	b.noBind(string(want), f)
	return f
}

// State transitions: proposed -> confirmed -> re-declared -> expired, then
// a new proposal -> rejected; Bind returns nothing in every state.
func TestOwnerBriefDB_StateTransitions(t *testing.T) {
	pool, ctx := livePool(t)
	b := briefTransitions{t: t, ctx: ctx, s: NewStore(pool),
		refs: []ObjectRef{{Kind: KindTable, Schema: "public", Name: "t"}},
		req: Request{ActionType: "alter_table", SQL: "ALTER TABLE public.t SET (a=1)",
			Targets: []string{"public.t"}, Now: time.Now()}}
	f := b.decide(b.propose("Payments").ID, true, StatusConfirmed)
	r := declareBrief(t, ctx, b.s, map[string]string{"owner": "Ledger"})
	if r.ID != f.ID || r.Value["owner"] != "Ledger" || r.DecidedAt.Before(*f.DecidedAt) {
		t.Fatalf("re-declared = %+v", r)
	}
	b.noBind("re-declared", r)
	e, err := b.s.Expire(ctx, r.ID, "alice@example.com", "owner changed teams")
	if err != nil || e.Status != StatusExpired || e.ExpiredBy != "alice@example.com" {
		t.Fatalf("expire: %+v %v", e, err)
	}
	b.noBind("expired", e)
	b.decide(b.propose("Platform").ID, false, StatusRejected)
	if n := briefRows(t, ctx, pool); n != 1 {
		t.Fatalf("%d brief rows, want 1", n)
	}
}

func insertStep(t *testing.T, ctx context.Context, pool *pgxpool.Pool, status string,
	ago time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_queue (proposed_sql, action_risk,
		status, proposed_at, decided_at) VALUES ('SELECT 1', 'safe', $1,
		now() - make_interval(secs => $2) - interval '1 minute',
		now() - make_interval(secs => $2))`, status, ago.Seconds()); err != nil {
		t.Fatalf("insert %s step %s ago: %v", status, ago, err)
	}
}

func cleanSteps(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	clean := func() { _, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_queue") }
	clean()
	t.Cleanup(clean)
}

const day = 24 * time.Hour

func TestBriefOutcome_AbsentSplitsAtSince(t *testing.T) {
	pool, ctx := livePool(t)
	cleanSteps(t, ctx, pool)
	insertStep(t, ctx, pool, "executed", 2*day)  // after
	insertStep(t, ctx, pool, "approved", 3*day)  // after
	insertStep(t, ctx, pool, "rejected", 4*day)  // after
	insertStep(t, ctx, pool, "rejected", 12*day) // before
	insertStep(t, ctx, pool, "pending", 13*day)  // neither
	insertStep(t, ctx, pool, "executed", 30*day) // outside both
	m, err := BriefOutcome(ctx, pool, time.Now().Add(-10*day))
	if err != nil {
		t.Fatalf("BriefOutcome: %v", err)
	}
	if m.Brief != BriefAbsent || m.FactID != 0 {
		t.Fatalf("brief = %q (fact %d), want absent", m.Brief, m.FactID)
	}
	if m.After.Accepted != 2 || m.After.Rejected != 1 || m.Before.Accepted != 0 ||
		m.Before.Rejected != 1 {
		t.Fatalf("counts before %+v after %+v", m.Before, m.After)
	}
	if !m.Before.To.Equal(m.After.From) || m.After.To.Sub(m.After.From) <
		m.Before.To.Sub(m.Before.From)-time.Second {
		t.Fatalf("periods are not adjacent and equal: before %+v after %+v", m.Before,
			m.After)
	}
}

func TestBriefOutcome_PresentSplitsAtDecision(t *testing.T) {
	pool, ctx := livePool(t)
	cleanSteps(t, ctx, pool)
	f := declareBrief(t, ctx, NewStore(pool), map[string]string{"owner": "Payments"})
	execAll(t, ctx, pool, `UPDATE sage.facts SET decided_at = now() - interval '5 days'
		WHERE fact_type = 'owner_brief'`)
	insertStep(t, ctx, pool, "executed", 1*day) // after
	insertStep(t, ctx, pool, "approved", 6*day) // before
	insertStep(t, ctx, pool, "rejected", 7*day) // before
	insertStep(t, ctx, pool, "executed", 11*day)
	m, err := BriefOutcome(ctx, pool, time.Now().Add(-30*day))
	if err != nil {
		t.Fatalf("BriefOutcome: %v", err)
	}
	if m.Brief != BriefPresent || m.FactID != f.ID {
		t.Fatalf("brief = %q (fact %d), want present (%d)", m.Brief, m.FactID, f.ID)
	}
	if m.After.Accepted != 1 || m.After.Rejected != 0 || m.Before.Accepted != 1 ||
		m.Before.Rejected != 1 {
		t.Fatalf("counts before %+v after %+v", m.Before, m.After)
	}
	if d := time.Since(m.After.From); d < 5*day-time.Minute || d > 5*day+time.Minute {
		t.Fatalf("split %v is not the brief's decision (5 days ago)", m.After.From)
	}
}

func TestBriefOutcome_ExpiredBriefIsAbsent(t *testing.T) {
	pool, ctx := livePool(t)
	cleanSteps(t, ctx, pool)
	s := NewStore(pool)
	f := declareBrief(t, ctx, s, map[string]string{"owner": "Payments"})
	if _, err := s.Expire(ctx, f.ID, "alice@example.com", "stale"); err != nil {
		t.Fatalf("expire: %v", err)
	}
	m, err := BriefOutcome(ctx, pool, time.Now().Add(-day))
	if err != nil || m.Brief != BriefAbsent || m.After.Accepted != 0 ||
		m.Before.Rejected != 0 {
		t.Fatalf("outcome after expiry = %+v, %v", m, err)
	}
}

func TestBriefOutcome_ErrorPropagation(t *testing.T) {
	pool, _ := livePool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := BriefOutcome(ctx, pool, time.Now().Add(-day))
	if err == nil || !strings.Contains(err.Error(), "owner brief outcome") ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
	if _, err := BriefOutcome(context.Background(), pool, time.Time{}); !errors.Is(err,
		ErrInvalidValue) {
		t.Fatalf("zero since: %v, want ErrInvalidValue", err)
	}
}
