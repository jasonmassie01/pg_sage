package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// facts-07: an expiry through the API names its actor in expired_by and in
// the fact history; the reason is the operator's note.

func TestFactRoutesExpireRecordsTheActor(t *testing.T) {
	f := newFactAPI(t)
	cases := []struct{ note, reason string }{
		{"fixtures moved", "fixtures moved"},
		{"", "expired by operator@test.com"},
	}
	for i, c := range cases {
		p := f.propose(t, fmt.Sprintf("test_expire_%d_*", i))
		code, body := f.do(t, testOperatorUser(), "POST",
			fmt.Sprintf("/api/v1/facts/%d/expire?database=testdb", p.ID),
			map[string]string{"note": c.note})
		fact, _ := body["fact"].(map[string]any)
		if code != http.StatusOK || fact["status"] != "expired" ||
			fact["expired_by"] != "operator@test.com" || fact["expired_reason"] != c.reason {
			t.Fatalf("expire with note %q: %d %v", c.note, code, body)
		}
		var actor, note string
		err := f.pool.QueryRow(context.Background(), `SELECT actor, note
			FROM sage.fact_events WHERE fact_id = $1 AND verb = 'expired'`, p.ID).
			Scan(&actor, &note)
		if err != nil || actor != "operator@test.com" || note != c.reason {
			t.Fatalf("expiry event: actor %q note %q %v", actor, note, err)
		}
	}
}

// CHECK-02: GET /api/v1/audit/verify lists the fact history chain as ok.
func TestAuditVerifyListsTheFactEventsChain(t *testing.T) {
	pool := auditPool(t, "api_audit_fact_events")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO sage.facts (fact_type, subject_kind, subject,
		source, proposed_by) VALUES ('test_fixture', 'schema', 'fx_*', 'detector', 'd')`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE sage.facts SET status = 'rejected',
		decided_by = 'alice', decided_at = now()`)
	require.NoError(t, err)
	w := auditGet(auditMux(pool, AuditDeps{}), "/api/v1/audit/verify?database=orders", true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		OK      bool `json:"ok"`
		Reports []struct {
			Chain    string            `json:"chain"`
			Links    int               `json:"links"`
			Problems []json.RawMessage `json:"problems"`
		} `json:"reports"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	found := false
	for _, r := range body.Reports {
		if r.Chain == "fact_events" {
			found = true
			if r.Links != 2 || len(r.Problems) != 0 {
				t.Fatalf("fact_events report = %+v, want 2 links and ok", r)
			}
		}
	}
	if !body.OK || !found {
		t.Fatalf("verify body = %+v, want ok with a fact_events chain", body)
	}
}
