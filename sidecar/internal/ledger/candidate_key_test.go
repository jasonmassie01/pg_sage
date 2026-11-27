package ledger

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// policy-02: a withheld verdict is one row per episode. The candidate key
// names the candidate across verdicts (park, queue, execute) so an execute
// ends the candidate's open withheld rows, and the next park starts a new
// episode on a fresh row.

func TestCandidateKeyIgnoresVerdictIntentAndPolicyVersion(t *testing.T) {
	base := parkedInput("orders", 7)
	key := CandidateKey(base)
	if !strings.HasPrefix(key, "ck_") || len(key) != len("ck_")+32 {
		t.Fatalf("candidate key %q, want ck_ and 32 hex digits", key)
	}
	same := []func(*DecisionInput){
		func(in *DecisionInput) { in.Verdict = VerdictExecute },
		func(in *DecisionInput) { in.Verdict = VerdictQueueApproval },
		func(in *DecisionInput) { in.PolicyVersion = 9 },
		func(in *DecisionInput) { in.Intent = "operator_approved" },
		func(in *DecisionInput) { in.Reason = "blast radius exceeded" },
		func(in *DecisionInput) { in.RiskTier = "high" },
		func(in *DecisionInput) { in.Evidence = map[string]any{"x": 1} },
		func(in *DecisionInput) { in.Fingerprint = "fp_other" },
	}
	for i, change := range same {
		input := base
		change(&input)
		if got := CandidateKey(input); got != key {
			t.Fatalf("change %d moved the candidate key: %q != %q", i, got, key)
		}
	}
	other := 8
	differ := map[string]func(*DecisionInput){
		"sql":       func(in *DecisionInput) { in.ProposedSQL += " WHERE x" },
		"target":    func(in *DecisionInput) { in.TargetObjects = []string{"public.other"} },
		"principal": func(in *DecisionInput) { in.PrincipalID = "agent-1" },
		"database":  func(in *DecisionInput) { in.DatabaseID = &other },
		"feature":   func(in *DecisionInput) { in.Feature = "vacuum" },
	}
	for name, change := range differ {
		input := base
		change(&input)
		if CandidateKey(input) == key {
			t.Fatalf("a different %s kept the candidate key %q", name, key)
		}
	}
}

func TestCandidateKeyNilDatabaseAndEmptyTargetsAreStable(t *testing.T) {
	input := validPostgresDecision("")
	input.DatabaseID = nil
	input.TargetObjects = nil
	key := CandidateKey(input)
	if key == "" || CandidateKey(input) != key {
		t.Fatalf("nil database, nil targets: key %q is empty or unstable", key)
	}
	input.TargetObjects = []string{}
	if CandidateKey(input) != key {
		t.Fatal("empty targets and nil targets gave different keys")
	}
	zero := 0
	input.DatabaseID = &zero
	if CandidateKey(input) == key {
		t.Fatal("database 0 and no database gave the same key")
	}
	reordered := parkedInput("a", 7)
	reordered.TargetObjects = []string{"public.b", "public.a"}
	sorted := reordered
	sorted.TargetObjects = []string{"public.a", "public.b"}
	if CandidateKey(reordered) != CandidateKey(sorted) {
		t.Fatal("target order changed the candidate key")
	}
}

// episodeRow is the part of a sage.decision row the episode tests assert.
type episodeRow struct {
	ID          int64
	Verdict     string
	Fingerprint *string
	Key         *string
	Repeats     int
	Resolved    bool
}

func candidateRows(t *testing.T, pool *pgxpool.Pool, key string) []episodeRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id, verdict, fingerprint,
		candidate_key, repeat_count, resolved_at IS NOT NULL FROM sage.decision
		WHERE candidate_key = $1 ORDER BY id`, key)
	if err != nil {
		t.Fatalf("read candidate rows: %v", err)
	}
	defer rows.Close()
	var out []episodeRow
	for rows.Next() {
		var r episodeRow
		if err := rows.Scan(&r.ID, &r.Verdict, &r.Fingerprint, &r.Key, &r.Repeats,
			&r.Resolved); err != nil {
			t.Fatalf("scan candidate row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate candidate rows: %v", err)
	}
	return out
}

func cleanupCandidate(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.decision WHERE candidate_key=$1", key)
	})
}

func asExecute(input DecisionInput) DecisionInput {
	input.Verdict, input.Reason, input.Fingerprint = VerdictExecute, "authorized", ""
	input.EvidenceID = ""
	return input
}

func TestExecuteResolvesTheCandidatesOpenEpisode(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	service := NewService(NewPostgresRepository(pool))
	park := parkedInput(fmt.Sprintf("ep_%d", time.Now().UnixNano()), ledgerDatabaseID())
	key := CandidateKey(park)
	cleanupCandidate(t, pool, key)
	for range 3 {
		if _, err := service.RecordDecision(ctx, park); err != nil {
			t.Fatalf("park: %v", err)
		}
	}
	queued := park
	queued.Verdict, queued.Reason = VerdictQueueApproval, "approval_required"
	queued.Fingerprint = DecisionFingerprint(queued)
	if _, err := service.RecordDecision(ctx, queued); err != nil {
		t.Fatalf("queue: %v", err)
	}
	executed, err := service.RecordDecision(ctx, asExecute(park))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := service.RecordDecision(ctx, park); err != nil {
		t.Fatalf("park after execute: %v", err)
	}
	rows := candidateRows(t, pool, key)
	if len(rows) != 4 {
		t.Fatalf("rows = %+v, want park, queue, execute, park", rows)
	}
	first, queue, exec, next := rows[0], rows[1], rows[2], rows[3]
	if first.Verdict != "parked" || first.Repeats != 3 || !first.Resolved {
		t.Fatalf("first episode = %+v, want parked x3, resolved", first)
	}
	if queue.Verdict != "queue_approval" || !queue.Resolved {
		t.Fatalf("queued row = %+v, want resolved by the execute", queue)
	}
	if exec.ID != executed.ID || exec.Fingerprint != nil || exec.Resolved {
		t.Fatalf("execute row = %+v, want id %d, no fingerprint, open", exec, executed.ID)
	}
	if next.Verdict != "parked" || next.Repeats != 1 || next.Resolved ||
		next.Fingerprint == nil || *next.Fingerprint != park.Fingerprint {
		t.Fatalf("next episode = %+v, want a fresh open park", next)
	}
}

func TestExecuteWithNoOpenRowsResolvesNothing(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	service := NewService(NewPostgresRepository(pool))
	tag := time.Now().UnixNano()
	bystander := parkedInput(fmt.Sprintf("by_%d", tag), ledgerDatabaseID())
	cleanupCandidate(t, pool, CandidateKey(bystander))
	if _, err := service.RecordDecision(ctx, bystander); err != nil {
		t.Fatalf("bystander park: %v", err)
	}
	lone := asExecute(parkedInput(fmt.Sprintf("lone_%d", tag), ledgerDatabaseID()))
	key := CandidateKey(lone)
	cleanupCandidate(t, pool, key)
	executed, err := service.RecordDecision(ctx, lone)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	rows := candidateRows(t, pool, key)
	if len(rows) != 1 || rows[0].ID != executed.ID || rows[0].Resolved ||
		rows[0].Key == nil || *rows[0].Key != key {
		t.Fatalf("rows = %+v, want only the open execute row %d", rows, executed.ID)
	}
	other := candidateRows(t, pool, CandidateKey(bystander))
	if len(other) != 1 || other[0].Resolved {
		t.Fatalf("another candidate's park = %+v, want it left open", other)
	}
}

// Integration: a row written before the upgrade has no candidate key; its
// next repeat stamps one, and a later execute resolves it.
func TestPreUpgradeRowGainsTheKeyOnRepeatAndIsResolved(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	service := NewService(NewPostgresRepository(pool))
	park := parkedInput(fmt.Sprintf("pre_%d", time.Now().UnixNano()), ledgerDatabaseID())
	key := CandidateKey(park)
	cleanupFingerprint(t, pool, park.Fingerprint)
	cleanupCandidate(t, pool, key)
	first, err := service.RecordDecision(ctx, park)
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.decision SET candidate_key = NULL
		WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("simulate a pre-upgrade row: %v", err)
	}
	again, err := service.RecordDecision(ctx, park)
	if err != nil || again.ID != first.ID {
		t.Fatalf("repeat = %d (%v), want the folded row %d", again.ID, err, first.ID)
	}
	rows := candidateRows(t, pool, key)
	if len(rows) != 1 || rows[0].ID != first.ID || rows[0].Repeats != 2 {
		t.Fatalf("rows = %+v, want row %d keyed with 2 repeats", rows, first.ID)
	}
	if _, err := service.RecordDecision(ctx, asExecute(park)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rows := candidateRows(t, pool, key); len(rows) != 2 || !rows[0].Resolved {
		t.Fatalf("rows = %+v, want the pre-upgrade row resolved", rows)
	}
}
