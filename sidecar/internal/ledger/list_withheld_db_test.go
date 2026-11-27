package ledger

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// policy-07: ListOpenWithheld answers "why hasn't this run?" from the
// ledger. Each test writes under its own database_id and filters on it,
// so rows other tests write to the shared server never reach it.

func withheldFilterFor(databaseID int) WithheldFilter {
	id := int64(databaseID)
	return WithheldFilter{DatabaseID: &id, Limit: DefaultWithheldLimit}
}

func cleanupDatabaseDecisions(t *testing.T, pool *pgxpool.Pool, databaseID int) {
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.decision WHERE database_id = $1", databaseID)
	})
}

func budgetPark(tag string, databaseID int) DecisionInput {
	park := parkedInput(tag, databaseID)
	park.Reason = "rate_limit_exceeded"
	park.Evidence = map[string]any{"budget_detail": "next frees at 18:00",
		"budget_kind": "performance"}
	park.PolicySHA256 = strings.Repeat("ab", 32)
	return park
}

// Happy path: three parks of one candidate are one entry counting three.
func TestListOpenWithheldFoldsThreeParksIntoOneEntry(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	databaseID := ledgerDatabaseID()
	cleanupDatabaseDecisions(t, pool, databaseID)
	service := NewService(NewPostgresRepository(pool))
	park := budgetPark("fold", databaseID)
	for range 3 {
		if _, err := service.RecordDecision(ctx, park); err != nil {
			t.Fatalf("park: %v", err)
		}
	}
	entries, truncated, err := ListOpenWithheld(ctx, pool, withheldFilterFor(databaseID))
	if err != nil || truncated {
		t.Fatalf("list: truncated=%v err=%v", truncated, err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want one", entries)
	}
	got := entries[0]
	if got.RepeatCount != 3 || got.OpenRows != 1 || got.Verdict != VerdictPark ||
		got.Reason != "rate_limit_exceeded" {
		t.Fatalf("entry = %+v, want 3 repeats of one parked rate_limit_exceeded row", got)
	}
	if got.BudgetDetail != "next frees at 18:00" || got.CandidateKey != CandidateKey(park) {
		t.Fatalf("budget detail %q, key %q; want the park's", got.BudgetDetail, got.CandidateKey)
	}
	if got.PolicyVersion == nil || *got.PolicyVersion != 1 ||
		got.PolicySHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("policy = %v/%q, want version 1 and the hash", got.PolicyVersion,
			got.PolicySHA256)
	}
	if got.FirstSeen.IsZero() || got.LastSeenAt.Before(got.FirstSeen) {
		t.Fatalf("first %s, last %s: want first <= last", got.FirstSeen, got.LastSeenAt)
	}
	if len(got.TargetObjects) != 1 || got.TargetObjects[0] != "public.fold" ||
		got.DecisionID <= 0 || got.RiskTier != "safe" || got.Feature != "index" {
		t.Fatalf("entry identity = %+v", got)
	}
}

// One candidate parked and then blocked has two open rows (the verdict is
// in the fingerprint) but is one entry: the latest verdict, every repeat.
func TestListOpenWithheldGroupsACandidateAcrossVerdicts(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	databaseID := ledgerDatabaseID()
	cleanupDatabaseDecisions(t, pool, databaseID)
	service := NewService(NewPostgresRepository(pool))
	park := budgetPark("group", databaseID)
	for range 2 {
		if _, err := service.RecordDecision(ctx, park); err != nil {
			t.Fatalf("park: %v", err)
		}
	}
	blocked := park
	blocked.Verdict, blocked.Reason = VerdictBlocked, "outside_maintenance_window"
	blocked.Evidence = map[string]any{}
	blocked.Fingerprint = DecisionFingerprint(blocked)
	if _, err := service.RecordDecision(ctx, blocked); err != nil {
		t.Fatalf("block: %v", err)
	}
	entries, _, err := ListOpenWithheld(ctx, pool, withheldFilterFor(databaseID))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v (%v), want one", entries, err)
	}
	got := entries[0]
	if got.Verdict != VerdictBlocked || got.Reason != "outside_maintenance_window" ||
		got.RepeatCount != 3 || got.OpenRows != 2 || got.BudgetDetail != "" {
		t.Fatalf("entry = %+v, want the latest (blocked) verdict over 3 repeats in 2 rows",
			got)
	}
}

// State transition: an execute resolves the candidate, and it leaves the
// list; a different candidate stays.
func TestListOpenWithheldDropsACandidateOnceItExecutes(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	databaseID := ledgerDatabaseID()
	cleanupDatabaseDecisions(t, pool, databaseID)
	service := NewService(NewPostgresRepository(pool))
	park, other := budgetPark("runs", databaseID), budgetPark("waits", databaseID)
	for _, input := range []DecisionInput{park, park, other} {
		if _, err := service.RecordDecision(ctx, input); err != nil {
			t.Fatalf("park: %v", err)
		}
	}
	before, _, err := ListOpenWithheld(ctx, pool, withheldFilterFor(databaseID))
	if err != nil || len(before) != 2 {
		t.Fatalf("before = %+v (%v), want two entries", before, err)
	}
	if _, err := service.RecordDecision(ctx, asExecute(park)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	after, _, err := ListOpenWithheld(ctx, pool, withheldFilterFor(databaseID))
	if err != nil || len(after) != 1 || after[0].CandidateKey != CandidateKey(other) {
		t.Fatalf("after = %+v (%v), want only the waiting candidate", after, err)
	}
}

// Nil/empty: no withheld decisions is an empty, non-nil list.
func TestListOpenWithheldEmptyIsNotNil(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	entries, truncated, err := ListOpenWithheld(ctx, pool, withheldFilterFor(ledgerDatabaseID()))
	if err != nil || truncated || entries == nil || len(entries) != 0 {
		t.Fatalf("list = %#v, %v, %v; want an empty non-nil list", entries, truncated, err)
	}
}

// Filters: verdict, reason and since select on the entry's latest state;
// limit cuts and reports the cut.
func TestListOpenWithheldFilters(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	databaseID := ledgerDatabaseID()
	cleanupDatabaseDecisions(t, pool, databaseID)
	service := NewService(NewPostgresRepository(pool))
	park := budgetPark("filter_park", databaseID)
	queued := parkedInput("filter_queue", databaseID)
	queued.Verdict, queued.Reason = VerdictQueueApproval, "approval_required"
	queued.Fingerprint = DecisionFingerprint(queued)
	for _, input := range []DecisionInput{park, queued} {
		if _, err := service.RecordDecision(ctx, input); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	filter := withheldFilterFor(databaseID)
	filter.Verdict = VerdictPark
	assertOnlyCandidate(t, ctx, pool, filter, CandidateKey(park))
	filter = withheldFilterFor(databaseID)
	filter.Reason = "approval_required"
	assertOnlyCandidate(t, ctx, pool, filter, CandidateKey(queued))
	filter = withheldFilterFor(databaseID)
	future := time.Now().Add(time.Hour)
	filter.Since = &future
	if entries, _, err := ListOpenWithheld(ctx, pool, filter); err != nil || len(entries) != 0 {
		t.Fatalf("since an hour ahead = %+v (%v), want none", entries, err)
	}
	filter = withheldFilterFor(databaseID)
	filter.Limit = 1
	entries, truncated, err := ListOpenWithheld(ctx, pool, filter)
	if err != nil || len(entries) != 1 || !truncated {
		t.Fatalf("limit 1 = %d entries, truncated %v (%v); want 1 and truncated",
			len(entries), truncated, err)
	}
	filter.Limit = 2
	if _, truncated, _ = ListOpenWithheld(ctx, pool, filter); truncated {
		t.Fatal("limit 2 of 2 entries reported a cut")
	}
}

func assertOnlyCandidate(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, filter WithheldFilter, key string,
) {
	t.Helper()
	entries, _, err := ListOpenWithheld(ctx, pool, filter)
	if err != nil || len(entries) != 1 || entries[0].CandidateKey != key {
		t.Fatalf("filter %+v = %+v (%v), want only %s", filter, entries, err, key)
	}
}

// Error propagation: a cancelled context is an error naming the read.
func TestListOpenWithheldPropagatesQueryErrors(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err := ListOpenWithheld(cancelled, pool, withheldFilterFor(1))
	if err == nil || !strings.Contains(err.Error(), "open withheld decisions") {
		t.Fatalf("err = %v, want a read error naming open withheld decisions", err)
	}
}

// Integration: on 10,000 decision rows the list reads the partial index
// idx_decision_open_withheld, with and without a database filter.
func TestListOpenWithheldUsesThePartialIndex(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	databaseID := ledgerDatabaseID()
	cleanupDatabaseDecisions(t, pool, databaseID)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.decision
		(database_id, feature, intent, verdict, risk_tier, reason, evidence_id,
		 resolved_at, fingerprint)
		SELECT $1::bigint, 'index', 'bulk', CASE WHEN g % 2 = 0 THEN 'execute' ELSE 'parked' END,
		       'safe', 'authorized', 'bulk-' || $1::text || '-' || g,
		       CASE WHEN g % 2 = 0 OR g > 20 THEN now() END,
		       CASE WHEN g % 2 = 1 THEN 'fp_bulk_' || $1::text || '_' || g END
		  FROM generate_series(1, 10000) AS g`, databaseID); err != nil {
		t.Fatalf("seed 10k decisions: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE sage.decision"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	for _, filter := range []WithheldFilter{withheldFilterFor(databaseID),
		{Limit: DefaultWithheldLimit}} {
		sql, args := withheldQuery(filter)
		plan := explainPlan(t, ctx, pool, sql, args)
		if !strings.Contains(plan, "idx_decision_open_withheld") {
			t.Errorf("database filter %v: plan does not use idx_decision_open_withheld:\n%s",
				filter.DatabaseID != nil, plan)
		}
	}
	entries, _, err := ListOpenWithheld(ctx, pool, withheldFilterFor(databaseID))
	if err != nil || len(entries) != 10 {
		t.Fatalf("open withheld = %d (%v), want the 10 open parks", len(entries), err)
	}
}

func explainPlan(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args []any,
) string {
	t.Helper()
	rows, err := pool.Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// Concurrent access: lists run while the gate keeps recording repeats; no
// read fails and the final count is every repeat.
func TestListOpenWithheldConcurrentWithRecording(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	databaseID := ledgerDatabaseID()
	cleanupDatabaseDecisions(t, pool, databaseID)
	service := NewService(NewPostgresRepository(pool))
	park := budgetPark("race", databaseID)
	const writers, repeats = 4, 5
	var wg sync.WaitGroup
	errs := make(chan error, writers*repeats*2)
	for w := 0; w < writers; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range repeats {
				if _, err := service.RecordDecision(ctx, park); err != nil {
					errs <- fmt.Errorf("record: %w", err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range repeats {
				_, _, err := ListOpenWithheld(ctx, pool, withheldFilterFor(databaseID))
				if err != nil {
					errs <- fmt.Errorf("list: %w", err)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	entries, _, err := ListOpenWithheld(ctx, pool, withheldFilterFor(databaseID))
	if err != nil || len(entries) != 1 || entries[0].RepeatCount != writers*repeats {
		t.Fatalf("after the race = %+v (%v), want one entry of %d repeats", entries, err,
			writers*repeats)
	}
}

// State transition: a withheld row recorded without a fingerprint (a
// lease conflict's park, a custodian escalation, a schema-guard verdict)
// is never resolved by the execute that follows, which resolves only
// fingerprinted rows. Its candidate's later execute still takes it off
// the list: the change has run (policy-07 review).
func TestListOpenWithheldDropsAnUnfingerprintedRowOnceItsCandidateExecutes(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	databaseID := ledgerDatabaseID()
	cleanupDatabaseDecisions(t, pool, databaseID)
	repo := NewPostgresRepository(pool)
	blocked, other := budgetPark("one_off", databaseID), budgetPark("one_off_waits", databaseID)
	blocked.Verdict, blocked.Reason, blocked.Fingerprint = VerdictBlocked, "ddl_conflict", ""
	other.Fingerprint = ""
	for _, input := range []DecisionInput{blocked, other} {
		input.EvidenceID = NewEvidenceID()
		if _, err := repo.InsertDecision(ctx, input); err != nil {
			t.Fatalf("insert unfingerprinted withheld row: %v", err)
		}
	}
	before, _, err := ListOpenWithheld(ctx, pool, withheldFilterFor(databaseID))
	if err != nil || len(before) != 2 {
		t.Fatalf("before = %+v (%v), want both one-off rows", before, err)
	}
	if _, err := NewService(repo).RecordDecision(ctx, asExecute(blocked)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	after, _, err := ListOpenWithheld(ctx, pool, withheldFilterFor(databaseID))
	if err != nil || len(after) != 1 || after[0].CandidateKey != CandidateKey(other) {
		t.Fatalf("after = %+v (%v), want only the candidate that has not run", after, err)
	}
	// A withheld row recorded after the execute is open again.
	blocked.EvidenceID = NewEvidenceID()
	if _, err := repo.InsertDecision(ctx, blocked); err != nil {
		t.Fatalf("insert the next withheld row: %v", err)
	}
	again, _, err := ListOpenWithheld(ctx, pool, withheldFilterFor(databaseID))
	if err != nil || len(again) != 2 || again[0].CandidateKey != CandidateKey(blocked) ||
		again[0].Reason != "ddl_conflict" || again[0].OpenRows != 1 {
		t.Fatalf("again = %+v (%v), want the new withheld row listed alone", again, err)
	}
}
