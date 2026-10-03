package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// A family row lists every member in target_objects: its dry run counts
// for each member, and the newest row's hash is the identity's last hash.
// The rows carry the family's real identity (InvariantIdentity), the only
// key the custodian ever looks a hash up by.
func TestHistoryCountsFamilyRowsPerMemberAndKeepsTheNewestHash(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	tag := fmt.Sprintf("hist_%d", time.Now().UnixNano())
	members := []string{tag + "_a.events", tag + "_b.events"}
	family := &schemaguard.Family{Key: tag, Members: []string{tag + "_a", tag + "_b"}}
	second := schemaguard.Invariant{Kind: schemaguard.InvariantUnboundedAppend,
		Schema: tag + "_b", Table: "events", Family: family}
	identity := schemaguard.InvariantIdentity(second)
	service := ledger.NewService(ledger.NewPostgresRepository(pool))
	for _, hash := range []string{"old", "new"} {
		if _, err := service.RecordDecision(ctx, ledger.DecisionInput{
			Feature: "schema_guard", Intent: string(schemaguard.InvariantUnboundedAppend),
			Evidence: map[string]any{"disposition": "dry_run",
				"invariant_key": identity, "decision_hash": hash},
			Verdict: ledger.VerdictObserveOnly, Reason: "dry run", RiskTier: "moderate",
			PolicyVersion: 1, TargetObjects: members,
		}); err != nil {
			t.Fatalf("record family row: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.decision
			WHERE evidence->>'invariant_key' = $1`, identity)
	})
	index, err := (postgresSchemaHistorySource{pool}).History(ctx,
		[]schemaguard.Invariant{second})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if got := index.For(second).SuccessfulRetentionDryRuns; got != 2 {
		t.Fatalf("dry runs for the second member = %d, want 2", got)
	}
	if index.LastHash[identity] != "new" {
		t.Fatalf("last hash = %q, want the newest row's", index.LastHash[identity])
	}
	other := schemaguard.Invariant{Kind: schemaguard.InvariantMissingFKIndex,
		Schema: tag + "_b", Table: "events"}
	if got := index.For(other); got != (schemaguard.History{}) {
		t.Fatalf("another kind on the same table has history %+v", got)
	}
	empty, err := (postgresSchemaHistorySource{pool}).History(ctx, nil)
	if err != nil || len(empty.ByTarget) != 0 || len(empty.LastHash) != 0 {
		t.Fatalf("empty History = %+v, %v", empty, err)
	}
}

// recordGuardRow writes one schema guard row and removes it after the test.
func recordGuardRow(
	t *testing.T, pool *pgxpool.Pool, intent string, targets []string,
	evidence map[string]any,
) {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.decision (feature,
		intent, target_objects, verdict, risk_tier, reason, evidence, evidence_id)
		VALUES ('schema_guard', $1, to_jsonb($2::text[]), 'observe_only', 'moderate',
		'history test', $3, 'histtest-' || gen_random_uuid()) RETURNING id`,
		intent, targets, evidence).Scan(&id); err != nil {
		t.Fatalf("record schema guard row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.decision WHERE id=$1", id)
	})
}

// The last hash is the identity's newest row, whichever members it lists.
// A family that changed from A to B and back to A must compare against
// B's hash: comparing against the newest row naming A (the old read) left
// the change back to A unrecorded.
func TestHistoryLastHashIsTheIdentitysNewestRowWhateverItsTargets(t *testing.T) {
	pool := requireAutonomyDB(t)
	tag := fmt.Sprintf("hnew_%d", time.Now().UnixNano())
	family := &schemaguard.Family{Key: tag, Members: []string{tag + "_a", tag + "_b"}}
	onA := schemaguard.Invariant{Kind: schemaguard.InvariantMissingFKIndex,
		Schema: tag + "_a", Table: "orders", Subject: "orders_customer_fk", Family: family}
	identity := schemaguard.InvariantIdentity(onA)
	intent := string(onA.Kind)
	recordGuardRow(t, pool, intent, []string{tag + "_a.orders"},
		map[string]any{"disposition": "recommend", "invariant_key": identity,
			"decision_hash": "members-a"})
	recordGuardRow(t, pool, intent, []string{tag + "_b.orders"},
		map[string]any{"disposition": "recommend", "invariant_key": identity,
			"decision_hash": "members-b"})
	index, err := (postgresSchemaHistorySource{pool}).History(context.Background(),
		[]schemaguard.Invariant{onA})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if got := index.LastHash[identity]; got != "members-b" {
		t.Fatalf("last hash = %q, want the identity's newest row (members-b)", got)
	}
}

// Only the scan's identities are looked up: a row of another identity on
// the same table is not part of the scan's history.
func TestHistoryReturnsOnlyTheScannedIdentities(t *testing.T) {
	pool := requireAutonomyDB(t)
	tag := fmt.Sprintf("hscope_%d", time.Now().UnixNano())
	scanned := schemaguard.Invariant{Kind: schemaguard.InvariantTypeTightening,
		Schema: tag, Table: "events", Subject: "user_id"}
	other := scanned
	other.Subject = "order_id"
	for _, invariant := range []schemaguard.Invariant{scanned, other} {
		recordGuardRow(t, pool, string(invariant.Kind), []string{invariant.Target()},
			map[string]any{"disposition": "recommend",
				"invariant_key": schemaguard.InvariantIdentity(invariant),
				"decision_hash": invariant.Subject})
	}
	index, err := (postgresSchemaHistorySource{pool}).History(context.Background(),
		[]schemaguard.Invariant{scanned})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	want := map[string]string{schemaguard.InvariantIdentity(scanned): "user_id"}
	if len(index.LastHash) != 1 || index.LastHash[schemaguard.InvariantIdentity(scanned)] !=
		want[schemaguard.InvariantIdentity(scanned)] {
		t.Fatalf("last hashes = %v, want only the scanned identity's %v", index.LastHash, want)
	}
}

// External reversions count per intent and target: a 'false' flag, another
// intent or another table does not count.
func TestHistoryCountsExternalReversionsPerIntentAndTarget(t *testing.T) {
	pool := requireAutonomyDB(t)
	tag := fmt.Sprintf("hrev_%d", time.Now().UnixNano())
	invariant := schemaguard.Invariant{Kind: schemaguard.InvariantMissingFKIndex,
		Schema: tag, Table: "orders", Subject: "orders_fk"}
	fk, target := string(invariant.Kind), invariant.Target()
	reverted := map[string]any{"disposition": "apply", "external_reversion": "true"}
	for range 3 {
		recordGuardRow(t, pool, fk, []string{target, tag + ".other"}, reverted)
	}
	recordGuardRow(t, pool, fk, []string{target},
		map[string]any{"disposition": "apply", "external_reversion": "false"})
	recordGuardRow(t, pool, string(schemaguard.InvariantRedundantIndex),
		[]string{target}, reverted)
	recordGuardRow(t, pool, fk, []string{tag + ".other"}, reverted)
	index, err := (postgresSchemaHistorySource{pool}).History(context.Background(),
		[]schemaguard.Invariant{invariant})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	want := schemaguard.History{ExternalReversions: 3}
	if got := index.For(invariant); got != want {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
}

// A failed read is reported as the history read, with its cause.
func TestHistoryReportsAFailedRead(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	invariant := schemaguard.Invariant{Kind: schemaguard.InvariantMissingFKIndex,
		Schema: "public", Table: "orders"}
	index, err := (postgresSchemaHistorySource{pool}).History(ctx,
		[]schemaguard.Invariant{invariant})
	if err == nil || !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "read schema remediation history") {
		t.Fatalf("History on a cancelled context = %v, want a wrapped context.Canceled", err)
	}
	if len(index.ByTarget) != 0 || len(index.LastHash) != 0 {
		t.Fatalf("failed read returned history %+v", index)
	}
}
