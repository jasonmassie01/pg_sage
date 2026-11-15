package verify

import (
	"testing"
)

// lab-01c: the outcome ledger records which verifier acted (verifier) and
// the other one's verdict and evidence. The trust ledger keeps reading
// verdict only, so nothing is counted twice.

func TestV2_OutcomeStoreRecordsTheOtherVerifier(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	id := insertOutcomeAction(t, pool, "CREATE INDEX CONCURRENTLY os_v2 ON t (a)")
	store := NewOutcomeStore(pool)
	o := Outcome{ActionLogID: id, Class: ClassIndexCreate, Verdict: OutcomeImproved,
		Reason: "v1 improved", Verifier: VerifierV1, OtherVerdict: OutcomeNeutral,
		OtherEvidence: map[string]any{"v2": map[string]any{"method": MethodDiD}},
		Evidence:      map[string]any{}}
	if err := store.RecordVerdict(ctx, o); err != nil {
		t.Fatalf("RecordVerdict: %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Verdict != OutcomeImproved || got.Verifier != VerifierV1 ||
		got.OtherVerdict != OutcomeNeutral {
		t.Fatalf("stored %+v, want improved by v1 with neutral from v2", got)
	}
	v2, _ := got.OtherEvidence["v2"].(map[string]any)
	if v2["method"] != MethodDiD {
		t.Fatalf("other evidence %v, want the v2 method", got.OtherEvidence)
	}
	var method string
	if err := pool.QueryRow(ctx, `SELECT other_evidence->'v2'->>'method' FROM
		sage.action_outcome WHERE action_log_id = $1`, id).Scan(&method); err != nil ||
		method != MethodDiD {
		t.Fatalf("other_evidence->'v2'->>'method' = %q (%v), want did", method, err)
	}
}

// A verdict recorded without a verifier (every caller before lab-01c) is
// v1's, with no other verdict.
func TestV2_OutcomeStoreDefaultsToV1(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	id := insertOutcomeAction(t, pool, "CREATE INDEX CONCURRENTLY os_v1 ON t (a)")
	store := NewOutcomeStore(pool)
	if err := store.RecordVerdict(ctx, Outcome{ActionLogID: id, Class: ClassIndexCreate,
		Verdict: OutcomeNeutral}); err != nil {
		t.Fatalf("RecordVerdict: %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Verifier != VerifierV1 || got.OtherVerdict != "" || got.OtherEvidence != nil {
		t.Fatalf("stored %+v, want v1 with no other verdict", got)
	}
}

// Invalid input: a verifier other than v1 or v2 is refused before the
// write, naming it.
func TestV2_OutcomeStoreRefusesAnUnknownVerifier(t *testing.T) {
	pool := verifyIntegrationPool(t)
	id := insertOutcomeAction(t, pool, "CREATE INDEX CONCURRENTLY os_v3 ON t (a)")
	err := NewOutcomeStore(pool).RecordVerdict(t.Context(), Outcome{ActionLogID: id,
		Class: ClassIndexCreate, Verdict: OutcomeNeutral, Verifier: "v3"})
	if err == nil {
		t.Fatal("verifier v3 was accepted")
	}
}
