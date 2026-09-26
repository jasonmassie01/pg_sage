package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// SURF-09 / G6-B09: query ids are 64-bit and must survive projection
// exactly, including values beyond 2^53 and the int64 limits.
func TestSourceQueryHintFromMap_PreservesInt64QueryID(t *testing.T) {
	cases := []int64{
		1 << 53, (1 << 53) + 1, (1 << 53) - 1, 9007199254740993,
		math.MaxInt64, math.MinInt64, -((1 << 53) + 1), -1,
	}
	for _, want := range cases {
		got := sourceQueryHintFromMap(map[string]any{"queryid": want})
		if got.QueryID != want {
			t.Errorf("int64 %d projected as %d", want, got.QueryID)
		}
		asText := sourceQueryHintFromMap(map[string]any{
			"queryid": itoa64(want),
		})
		if asText.QueryID != want {
			t.Errorf("string %d projected as %d", want, asText.QueryID)
		}
	}
}

// The /query-hints JSON boundary serialises queryid as a decimal
// string so browsers do not round it.
func TestQueryHintsHandler_SerializesQueryIDAsString(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	const big = int64(9007199254740993)
	_, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
		(queryid, hint_text, symptom, status)
		VALUES ($1, 'Use idx', 'seq_scan', 'active')`, big)
	if err != nil {
		t.Fatalf("insert hint: %v", err)
	}
	h := queryHintsHandler(phase2MgrWithPool(pool))
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/query-hints?database=testdb", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Hints []map[string]json.RawMessage `json:"hints"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Hints) != 1 {
		t.Fatalf("hints = %d, want 1", len(resp.Hints))
	}
	if got := string(resp.Hints[0]["queryid"]); got != `"9007199254740993"` {
		t.Fatalf("queryid JSON = %s, want \"9007199254740993\"", got)
	}
}

// SURF-12 / G6-B10: execution success is "applied"; only a completed
// successful durable verification is "verified".
func TestActionLogVerificationStatus_UsesDurableVerification(t *testing.T) {
	done := time.Now()
	tests := []struct {
		name string
		row  map[string]any
		want string
	}{
		{"success without verification", map[string]any{
			"outcome": "success"}, "applied"},
		{"success with legacy measured_at only", map[string]any{
			"outcome": "success", "measured_at": &done}, "applied"},
		{"verification pending", map[string]any{
			"outcome": "success", "verification_verdict": "pending"},
			"pending"},
		{"verification extended", map[string]any{
			"outcome": "success", "verification_verdict": "extended"},
			"pending"},
		{"verified", map[string]any{
			"outcome": "success", "verification_verdict": "success",
			"verification_completed_at": &done}, "verified"},
		{"success verdict not completed", map[string]any{
			"outcome": "success", "verification_verdict": "success"},
			"pending"},
		{"unverifiable", map[string]any{
			"outcome": "success", "verification_verdict": "unverifiable",
			"verification_completed_at": &done}, "inconclusive"},
		{"verification revert", map[string]any{
			"outcome": "success", "verification_verdict": "revert",
			"verification_completed_at": &done}, "reverted"},
		{"verification failed", map[string]any{
			"outcome": "success", "verification_verdict": "failed",
			"verification_completed_at": &done}, "failed"},
		{"rolled back", map[string]any{"outcome": "rolled_back"},
			"reverted"},
		{"monitoring", map[string]any{"outcome": "pending"}, "pending"},
		{"execution failed", map[string]any{"outcome": "failed"},
			"failed"},
		{"empty", map[string]any{}, "not_started"},
	}
	for _, tc := range tests {
		if got := actionLogVerificationStatus(tc.row); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The executed ledger carries the durable verification status.
func TestActionsLedger_VerificationStatusFromDurableRecord(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanLedgerTables(t, pool, ctx)
	applied := insertLedgerLogRow(t, pool, ctx, "success")
	verified := insertLedgerLogRow(t, pool, ctx, "success")
	var decisionID int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.decision
		(feature, intent, verdict, risk_tier, reason, evidence_id)
		VALUES ('index','create_index','execute','safe','t','ev-ver-1')
		RETURNING id`).Scan(&decisionID)
	if err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.verification
		(decision_id, action_log_id, criterion, baseline, minimum_samples,
		 next_evaluation_at, hard_deadline_at, verdict, completed_at)
		VALUES ($1, $2, '{}', '{}', 1, now(), now(), 'success', now())`,
		decisionID, verified)
	if err != nil {
		t.Fatalf("insert verification: %v", err)
	}
	statuses := map[string]string{}
	for _, raw := range getLedger(t, pool)["actions"].([]any) {
		row := raw.(map[string]any)
		statuses[row["id"].(string)], _ = row["verification_status"].(string)
	}
	if got := statuses[itoa64(applied)]; got != "applied" {
		t.Errorf("unverified success = %q, want applied", got)
	}
	if got := statuses[itoa64(verified)]; got != "verified" {
		t.Errorf("verified success = %q, want verified", got)
	}
}
