package sre

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention (AI-SRE-SPEC §10): redacted evidence 30 days, timelines 90
// days; pinned and live investigations are kept; every delete leaves a
// tombstone, so a conclusion whose evidence is gone reads as unavailable,
// never as silently correct.

var testPolicy = RetentionPolicy{EvidenceAge: 30 * 24 * time.Hour,
	TimelineAge: 90 * 24 * time.Hour, BatchSize: 50}

// agedConcluded returns a concluded investigation last changed daysAgo.
func agedConcluded(t *testing.T, ctx context.Context, st *PostgresStore,
	pool *pgxpool.Pool, scope Scope, subject string, daysAgo float64) UUID {
	t.Helper()
	lease, ev := evaluating(t, ctx, st, scope, subject)
	if _, err := st.Conclude(ctx, lease, concluded(ev)); err != nil {
		t.Fatalf("conclude: %v", err)
	}
	backdate(t, ctx, pool, lease.InvestigationID, daysAgo)
	return lease.InvestigationID
}

func backdate(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id UUID,
	daysAgo float64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_investigations
		SET updated_at = clock_timestamp() - make_interval(secs => $2),
		    created_at = clock_timestamp() - make_interval(secs => $2 + 60),
		    expires_at = clock_timestamp() + interval '1 hour'
		WHERE id = $1`, string(id), daysAgo*86400); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string,
	id UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sage."+table+
		" WHERE investigation_id = $1", string(id)).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func tombstoneKinds(t *testing.T, ctx context.Context, st *PostgresStore, scope Scope,
	id UUID) map[string]Tombstone {
	t.Helper()
	ts, err := st.Tombstones(ctx, scope, id)
	if err != nil {
		t.Fatalf("tombstones: %v", err)
	}
	out := map[string]Tombstone{}
	for _, tb := range ts {
		out[tb.Kind] = tb
	}
	return out
}

func TestRetention_EvidenceAgesOutWithTombstoneKeepingConclusion(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	old := agedConcluded(t, ctx, st, pool, scope, "pid 30", 31)
	recent := agedConcluded(t, ctx, st, pool, scope, "pid 31", 29)
	pinned := agedConcluded(t, ctx, st, pool, scope, "pid 32", 31)
	if _, err := st.SetPinned(ctx, scope, pinned, true, "user:1"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	backdate(t, ctx, pool, pinned, 31)
	live, _ := evaluating(t, ctx, st, scope, "pid 33")
	backdate(t, ctx, pool, live.InvestigationID, 31)

	res, err := st.Purge(ctx, scope, testPolicy)
	if err != nil || res.EvidencePurged != 1 || res.InvestigationsPurged != 0 {
		t.Fatalf("purge = %+v (%v), want one evidence purge", res, err)
	}
	if n := countRows(t, ctx, pool, "sre_evidence", old); n != 0 {
		t.Fatalf("aged evidence rows left: %d", n)
	}
	for name, id := range map[string]UUID{"29 days": recent, "pinned": pinned,
		"live": live.InvestigationID} {
		if n := countRows(t, ctx, pool, "sre_evidence", id); n != 2 {
			t.Errorf("%s investigation lost evidence: %d rows", name, n)
		}
	}
	tb := tombstoneKinds(t, ctx, st, scope, old)["evidence"]
	if tb.RowCount != 2 || tb.Reason != "retention" {
		t.Fatalf("evidence tombstone = %+v", tb)
	}
	inv, _ := st.Get(ctx, scope, old)
	hs, _ := st.Hypotheses(ctx, scope, old)
	if inv.EvidencePurgedAt.IsZero() || inv.State != StateConcluded || len(hs) != 2 {
		t.Fatalf("after evidence purge: %+v, %d hypotheses", inv, len(hs))
	}
	if err := st.VerifyEvents(ctx, scope, old); err != nil {
		t.Fatalf("chain after purge event: %v", err)
	}
	again, err := st.Purge(ctx, scope, testPolicy)
	if err != nil || again.EvidencePurged != 0 {
		t.Fatalf("second purge = %+v (%v), want idempotent", again, err)
	}
}

func TestRetention_TimelineAgesOutLeavingInvestigationTombstone(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	gone := agedConcluded(t, ctx, st, pool, scope, "pid 40", 91)
	kept := agedConcluded(t, ctx, st, pool, scope, "pid 41", 89)
	pinned := agedConcluded(t, ctx, st, pool, scope, "pid 42", 91)
	if _, err := st.SetPinned(ctx, scope, pinned, true, "user:1"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	backdate(t, ctx, pool, pinned, 91)

	res, err := st.Purge(ctx, scope, testPolicy)
	if err != nil || res.InvestigationsPurged != 1 {
		t.Fatalf("purge = %+v (%v), want one investigation purge", res, err)
	}
	if _, err := st.Get(ctx, scope, gone); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purged investigation still readable: %v", err)
	}
	for _, table := range []string{"sre_evidence", "sre_steps", "sre_hypotheses",
		"sre_events"} {
		if n := countRows(t, ctx, pool, table, gone); n != 0 {
			t.Errorf("%s rows left for the purged investigation: %d", table, n)
		}
	}
	tb := tombstoneKinds(t, ctx, st, scope, gone)["investigation"]
	if tb.Reason != "retention" || len(tb.Detail) == 0 {
		t.Fatalf("investigation tombstone = %+v", tb)
	}
	for name, id := range map[string]UUID{"89 days": kept, "pinned": pinned} {
		if _, err := st.Get(ctx, scope, id); err != nil {
			t.Errorf("%s investigation purged: %v", name, err)
		}
	}
}

func TestRetention_RejectsInvalidPolicies(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	day := 24 * time.Hour
	for name, p := range map[string]RetentionPolicy{
		"zero":                   {},
		"negative evidence":      {EvidenceAge: -day, TimelineAge: day, BatchSize: 1},
		"evidence over timeline": {EvidenceAge: 2 * day, TimelineAge: day, BatchSize: 1},
		"zero batch":             {EvidenceAge: day, TimelineAge: day},
		"sub-day":                {EvidenceAge: time.Hour, TimelineAge: day, BatchSize: 1},
	} {
		if _, err := st.Purge(ctx, scope, p); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: purge = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := st.Purge(ctx, Scope{}, testPolicy); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("zero scope purge = %v", err)
	}
}

// Two retention passes racing over the same scope delete each row once
// and leave one tombstone per kind.
func TestRetention_ConcurrentPassesAreSafe(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	ids := []UUID{agedConcluded(t, ctx, st, pool, scope, "pid 50", 95),
		agedConcluded(t, ctx, st, pool, scope, "pid 51", 45)}
	var wg sync.WaitGroup
	results := make([]PurgeResult, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = st.Purge(ctx, scope, testPolicy)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent purge: %v", err)
		}
	}
	total := results[0].InvestigationsPurged + results[1].InvestigationsPurged
	evidence := results[0].EvidencePurged + results[1].EvidencePurged
	if total != 1 || evidence != 1 {
		t.Fatalf("results %+v: want one investigation and one evidence purge", results)
	}
	if kinds := tombstoneKinds(t, ctx, st, scope, ids[1]); len(kinds) != 1 {
		t.Fatalf("tombstones for the 45-day investigation: %+v", kinds)
	}
}
