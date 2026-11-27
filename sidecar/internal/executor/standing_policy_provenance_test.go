package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// policy-03: the standing gate decided under one store.Current read and
// recorded under a second one, so a ratification between the two recorded
// the wrong version. The decision now carries the version and document
// hash the gate evaluated; the recorder reads nothing.

// ratifyingReader serves v1 on its first read and v2 (a ratification that
// landed right after the gate read the policy) on every later one.
type ratifyingReader struct {
	mu    sync.Mutex
	calls int
	err   error
	v1    []byte
	v2    []byte
}

func (r *ratifyingReader) Current(context.Context, policy.Scope) (policy.Policy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return policy.Policy{}, r.err
	}
	if r.calls == 1 {
		return policy.Policy{Version: 1, Profile: "unattended", Document: r.v1}, nil
	}
	return policy.Policy{Version: 2, Profile: "unattended", Document: r.v2}, nil
}

func (r *ratifyingReader) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newRatifyingReader() *ratifyingReader {
	return &ratifyingReader{v1: []byte(lifeosLegacyPolicyJSON),
		v2: []byte(lifeosLegacyPolicyJSON + " ")}
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type recordedProvenance struct {
	rows    int
	verdict string
	reason  string
	version int
	hash    *string
}

// provenanceOn reads the recorded decision d (rows is 0 when the gate
// recorded none).
func provenanceOn(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, d policy.Decision,
) recordedProvenance {
	t.Helper()
	var got recordedProvenance
	err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(verdict), ''),
		COALESCE(max(reason), ''), COALESCE(max(policy_version), -1),
		max(policy_doc_sha256) FROM sage.decision WHERE id = $1`,
		d.DecisionID).Scan(&got.rows, &got.verdict, &got.reason, &got.version, &got.hash)
	if err != nil {
		t.Fatalf("read decision %d: %v", d.DecisionID, err)
	}
	return got
}

func storeGateExecutor(t *testing.T, pool *pgxpool.Pool, reader policyReader) *Executor {
	t.Helper()
	exec := budgetExecutor(t, pool, lifeosLegacyPolicy())
	exec.WithPolicyGate(exec.newStandingPolicyGate(reader, policy.Scope{}))
	return exec
}

func TestRatificationBetweenEvaluationAndRecordKeepsTheEvaluatedVersion(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	reader := newRatifyingReader()
	exec := storeGateExecutor(t, pool, reader)
	d := exec.StandingPolicyGate().Authorize(ctx,
		findingRequest(createIndexFinding("public.p03_ratify"), false))
	if d.DecisionID <= 0 {
		t.Fatalf("decision %s was not recorded (%s)", d.LedgerLabel(), d.Detail)
	}
	got := provenanceOn(t, ctx, pool, d)
	want := sha256Hex(reader.v1)
	if got.rows != 1 || got.version != 1 || got.hash == nil || *got.hash != want {
		t.Fatalf("recorded %+v (hash %v), want one row at v1 hash %s", got,
			derefHash(got.hash), want)
	}
	if calls := reader.count(); calls != 1 {
		t.Fatalf("store.Current called %d times for one decision, want 1", calls)
	}
}

func TestPolicyReadFailureStillRecordsTheBlock(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	reader := newRatifyingReader()
	reader.err = errors.New("dial tcp: connection refused")
	exec := storeGateExecutor(t, pool, reader)
	d := exec.StandingPolicyGate().Authorize(ctx,
		findingRequest(createIndexFinding("public.p03_unreadable"), false))
	if d.Verdict != policy.VerdictBlocked || d.Reason != policy.ReasonPolicyUnavailable {
		t.Fatalf("decision = %s, want blocked/policy_unavailable", d.LedgerLabel())
	}
	got := provenanceOn(t, ctx, pool, d)
	if got.rows != 1 || got.reason != string(policy.ReasonPolicyUnavailable) ||
		got.version != 0 || got.hash != nil {
		t.Fatalf("recorded %+v (hash %v), want one policy_unavailable row at v0, no hash",
			got, derefHash(got.hash))
	}
}

func TestHardStopRecordsVersionZeroWithoutReadingThePolicy(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	reader := newRatifyingReader()
	exec := storeGateExecutor(t, pool, reader)
	exec.WithEmergencyStopCheck(func(context.Context) bool { return true })
	d := exec.StandingPolicyGate().Authorize(ctx,
		findingRequest(createIndexFinding("public.p03_stop"), false))
	if d.Reason != policy.ReasonEmergencyStop {
		t.Fatalf("decision = %s, want emergency_stop", d.LedgerLabel())
	}
	got := provenanceOn(t, ctx, pool, d)
	if got.rows != 1 || got.version != 0 || got.hash != nil {
		t.Fatalf("recorded %+v (hash %v), want one row at v0, no hash", got,
			derefHash(got.hash))
	}
	if calls := reader.count(); calls != 0 {
		t.Fatalf("a hard stop read the policy %d times, want 0", calls)
	}
}

// The in-memory document is version 1 and carries the hash of its
// canonical encoding, so an execute it allows names a policy (CHECK-01).
func TestInMemoryPolicyExecuteCarriesVersionAndHash(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	doc := lifeosLegacyPolicy()
	exec := budgetExecutor(t, pool, doc)
	d := exec.StandingPolicyGate().Authorize(ctx,
		findingRequest(createIndexFinding("public.p03_memory"), false))
	if d.Verdict != policy.VerdictExecute {
		t.Fatalf("decision = %s (%s), want execute", d.LedgerLabel(), d.Detail)
	}
	raw, err := policy.MarshalDocument(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := provenanceOn(t, ctx, pool, d)
	if got.version != 1 || got.hash == nil || *got.hash != sha256Hex(raw) {
		t.Fatalf("recorded %+v (hash %v), want v1 hash %s", got, derefHash(got.hash),
			sha256Hex(raw))
	}
	var unnamed int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM sage.decision WHERE verdict =
		'execute' AND policy_doc_sha256 IS NULL AND policy_version > 0`).Scan(&unnamed)
	if err != nil || unnamed != 0 {
		t.Fatalf("execute rows naming a version but no hash = %d (%v), want 0", unnamed, err)
	}
}

// Fleet: the policy lives in the control database. The decision in the
// monitored database records the control policy's version and hash, with
// no reference into a sage.policy table it does not share.
func TestFleetDecisionRecordsTheControlPolicyHash(t *testing.T) {
	control, ctx := isolatedSageDB(t)
	monitored := anotherSageDB(t, ctx)
	databaseID := 880000 + int(time.Now().UnixNano()%100000)
	exec := budgetExecutor(t, monitored, lifeosLegacyPolicy())
	if err := exec.EnableStandingPolicyWithStore(ctx, control, "unattended",
		&databaseID); err != nil {
		t.Fatalf("EnableStandingPolicyWithStore: %v", err)
	}
	d := exec.StandingPolicyGate().Authorize(ctx,
		findingRequest(createIndexFinding("public.p03_fleet"), false))
	if d.DecisionID <= 0 {
		t.Fatalf("decision %s was not recorded: %s", d.LedgerLabel(), d.Detail)
	}
	// The database follows the default policy set (fleetpol-01a): the
	// control policy in force is resolved, not a row keyed by its id.
	id64 := int64(databaseID)
	current, err := policy.NewStore(control).Current(ctx, policy.Scope{DatabaseID: &id64})
	if err != nil {
		t.Fatalf("read control policy: %v", err)
	}
	doc, err := policy.DocumentFromPolicy(current)
	if err != nil {
		t.Fatalf("control policy document: %v", err)
	}
	version, hash := int(doc.Version), doc.SHA256
	got := provenanceOn(t, ctx, monitored, d)
	if got.rows != 1 || got.version != version || got.hash == nil || *got.hash != hash {
		t.Fatalf("recorded %+v (hash %v), want v%d hash %s from the control policy",
			got, derefHash(got.hash), version, hash)
	}
	var local int
	if err := monitored.QueryRow(ctx, `SELECT count(*) FROM sage.policy
		WHERE database_id = $1`, databaseID).Scan(&local); err != nil || local != 0 {
		t.Fatalf("monitored database holds %d policy rows (%v), want 0", local, err)
	}
}

func derefHash(hash *string) string {
	if hash == nil {
		return "<nil>"
	}
	return *hash
}

// A lease conflict's verdict row is derived from the execute decision it
// replaces: it carries that decision's policy and safety provenance.
func TestLeaseVerdictCopiesTheSourceProvenance(t *testing.T) {
	pool, ctx := requireDB(t)
	hash := sha256Hex([]byte("p03 lease"))
	var source int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.decision (feature, intent,
		target_objects, policy_version, policy_doc_sha256, verdict, risk_tier, reason,
		guardrails, off_window_ok, evidence_id) VALUES ('freeze', 'freeze',
		'["public.p03_lease"]', 6, $1, 'execute', 'moderate', 'deadline_override',
		'["concurrently","lock_timeout"]', true, $2) RETURNING id`, hash,
		fmt.Sprintf("p03-lease-%d", time.Now().UnixNano())).Scan(&source)
	if err != nil {
		t.Fatalf("seed source decision: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.decision
			WHERE id = $1 OR (evidence->>'parked_decision_id')::bigint = $1`, source)
	})
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	exec.recordLeaseVerdict(ctx, source, leaseVerdictParked, policy.ErrLeaseConflict)
	var version int
	var gotHash, guardrails, verdict, reason string
	var offWindow bool
	err = pool.QueryRow(ctx, `SELECT policy_version, policy_doc_sha256, guardrails::text,
		off_window_ok, verdict, reason FROM sage.decision
		WHERE (evidence->>'parked_decision_id')::bigint = $1`, source).Scan(&version,
		&gotHash, &guardrails, &offWindow, &verdict, &reason)
	if err != nil {
		t.Fatalf("read lease verdict row: %v", err)
	}
	if version != 6 || gotHash != hash || guardrails != `["concurrently", "lock_timeout"]` ||
		!offWindow || verdict != "parked" || reason != string(policy.ReasonDDLConflict) {
		t.Fatalf("lease verdict = v%d %s %s off_window=%v %s/%s, want the source's "+
			"provenance parked/ddl_conflict", version, gotHash, guardrails, offWindow,
			verdict, reason)
	}
}
