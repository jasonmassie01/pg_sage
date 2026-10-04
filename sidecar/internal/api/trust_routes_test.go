package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Roadmap 1.2: GET /api/v1/trust serves the Trust page: one ledger view
// per database (every family x class of both kinds, level, evidence
// counts, last change and the path to the next level), for every signed-in
// role.

func trustRows(t *testing.T, db map[string]any) []map[string]any {
	t.Helper()
	raw, _ := db["rows"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func findTrustRow(rows []map[string]any, family, class string) map[string]any {
	for _, r := range rows {
		if r["family"] == family && r["class"] == class {
			return r
		}
	}
	return nil
}

func TestTrustAPI_ViewerReadsEveryDatabase(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	f.register("billing")
	bound := policy.RuntimeState{ExecutorEnabled: true, ExecutionMode: policy.ExecutionAuto,
		TrustLevel: policy.TrustAutonomous, Tier3Safe: true, Tier3Moderate: true,
		RampStart: f.clock.Now().Add(-90 * 24 * time.Hour)}
	if _, err := f.ledger.SeedGrandfathered(context.Background(), "orders", bound); err != nil {
		t.Fatal(err)
	}
	code, body := autonomyCall(t, f.router(testViewerUser()), "GET", "/api/v1/trust", "")
	dbs, _ := body["databases"].([]any)
	if code != 200 || len(dbs) != 2 || body["meaning"] == nil {
		t.Fatalf("trust = %d %v", code, body)
	}
	if m, _ := body["meaning"].(string); !strings.Contains(m,
		"the ledger grants, the operator caps") || !strings.Contains(m, "tier3") {
		t.Fatalf("meaning = %q", m)
	}
	orders := dbs[1].(map[string]any)
	if orders["database"] != "orders" {
		orders = dbs[0].(map[string]any)
	}
	rows := trustRows(t, orders)
	vac := findTrustRow(rows, "hygiene", "vacuum")
	if vac == nil || vac["level"] != "L3" || vac["provenance"] != "grandfathered" ||
		vac["kind"] != "self_initiated" || vac["outcome_class"] != "vacuum" ||
		vac["effective"] == nil {
		t.Fatalf("hygiene/vacuum = %v", vac)
	}
	ev, _ := vac["evidence"].(map[string]any)
	for _, k := range []string{"improved", "neutral", "regressed", "rolled_back", "rejected"} {
		if _, ok := ev[k]; !ok {
			t.Fatalf("evidence lacks %q: %v", k, ev)
		}
	}
	freeze := findTrustRow(rows, "wraparound_runway", "freeze")
	if freeze == nil || freeze["kind"] != "incident" || freeze["next"] == nil {
		t.Fatalf("wraparound_runway/freeze = %v", freeze)
	}
	if got := len(rows); got < len(earned.Families()) {
		t.Fatalf("only %d rows", got)
	}
}

func TestTrustAPI_OneDatabase(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	f.register("billing")
	h := f.router(testViewerUser())
	code, body := autonomyCall(t, h, "GET", "/api/v1/trust?database=billing", "")
	dbs, _ := body["databases"].([]any)
	if code != 200 || len(dbs) != 1 || dbs[0].(map[string]any)["database"] != "billing" {
		t.Fatalf("one database = %d %v", code, body)
	}
	code, body = autonomyCall(t, h, "GET", "/api/v1/trust?database=nope", "")
	if code != 404 || body["code"] != "not_found" {
		t.Fatalf("unknown database = %d %v", code, body)
	}
	code, body = autonomyCall(t, h, "GET", "/api/v1/trust?database="+longName(), "")
	if code != 400 || body["code"] != "invalid_request" {
		t.Fatalf("oversized database name = %d %v", code, body)
	}
}

func longName() string {
	b := make([]byte, 300)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestTrustAPI_RequiresSignIn(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	code, _ := autonomyCall(t, f.router(nil), "GET", "/api/v1/trust", "")
	if code != 401 {
		t.Fatalf("anonymous trust = %d, want 401", code)
	}
	for _, u := range []string{"viewer", "operator", "admin"} {
		user := testViewerUser()
		switch u {
		case "operator":
			user = testOperatorUser()
		case "admin":
			user = testAdminUser()
		}
		if code, body := autonomyCall(t, f.router(user), "GET", "/api/v1/trust", ""); code !=
			200 {
			t.Fatalf("%s trust = %d %v", u, code, body)
		}
	}
}

// Without the ledger (no autonomy deps) the route is not served.
func TestTrustAPI_AbsentWithoutLedgers(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	f.reg = earned.NewRegistry(true)
	code, body := autonomyCall(t, f.router(testViewerUser()), "GET", "/api/v1/trust", "")
	dbs, _ := body["databases"].([]any)
	if code != 200 || len(dbs) != 0 {
		t.Fatalf("empty registry = %d %v", code, body)
	}
}
