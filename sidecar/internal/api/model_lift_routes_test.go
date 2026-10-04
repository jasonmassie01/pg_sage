package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Roadmap 2.4: GET /api/v1/model-lift serves "model lift over
// deterministic, per family" (held-out override precision, inconclusive
// lift, Safe Pass against the causal graph) and each family's model root
// authority, for every signed-in role. GET .../investigations/{id}/
// replay-case exports a contested investigation as a redacted replay
// case, for operators.

// liftReportJSON is a revision-2 report with one held-out live record.
func liftReportJSON(at time.Time, family string, k, n int) string {
	return fmt.Sprintf(`{"schema": "pg_sage.pgincidentbench.v1", "schema_revision": 2,
		"generated_at": %q, "llm": {"mode": "live", "model": "gpt-4o-mini"},
		"gated_arms": ["causal-graph", "causal-graph+llm"],
		"cells": [{"arm": "causal-graph", "family": "all", "runs": 0,
			"safe_pass": {"k": 0, "n": 0}, "top1": {"k": 0, "n": 0},
			"mechanism_precision": null, "forbidden_actions": 0}],
		"model_lift": [{"arm": "causal-graph+llm", "baseline": "causal-graph",
			"family": %q, "split": "held_out", "llm_mode": "live", "runs": 40,
			"safe_pass": {"k": 38, "n": 40}, "baseline_safe_pass": {"k": 30, "n": 40},
			"top1": {"k": 20, "n": 22}, "baseline_top1": {"k": 18, "n": 22},
			"override_precision": {"k": %d, "n": %d},
			"override_safe_pass": {"k": 36, "n": 40},
			"inconclusive_runs": 6, "inconclusive_resolved_right": 3,
			"inconclusive_resolved_wrong": 1, "forbidden_actions": 0}]}`,
		at.UTC().Format(time.RFC3339), family, k, n)
}

func familyLift(t *testing.T, body map[string]any, family string) map[string]any {
	t.Helper()
	fams, _ := body["families"].([]any)
	for _, f := range fams {
		m := f.(map[string]any)
		if m["family"] == family {
			return m
		}
	}
	t.Fatalf("no family %s in %v", family, body)
	return nil
}

func TestModelLiftAPI_ViewerReadsLiftAndAuthority(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	raw := liftReportJSON(f.clock.Now().Add(-time.Hour), "lock_blocking", 16, 16)
	if _, err := f.ledger.IngestEvalRun(context.Background(), []byte(raw),
		earned.SourceBench, "admin", ""); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	code, body := autonomyCall(t, f.router(testViewerUser()), "GET", "/api/v1/model-lift", "")
	if code != 200 || body["database"] != "orders" || body["threshold"] != 0.8 ||
		body["min_overrides"] != float64(10) || body["meaning"] == nil {
		t.Fatalf("model lift = %d %v", code, body)
	}
	lock := familyLift(t, body, "lock_blocking")
	lift, _ := lock["lift"].(map[string]any)
	if lock["granted"] != true || lock["status"] != "adopt" || lift == nil ||
		lift["baseline"] != "causal-graph" {
		t.Fatalf("lock_blocking = %v", lock)
	}
	ov, _ := lift["override_precision"].(map[string]any)
	if ov["k"] != float64(16) || ov["n"] != float64(16) {
		t.Fatalf("override precision = %v", ov)
	}
	wal := familyLift(t, body, "wal_retention")
	if wal["granted"] != false || wal["status"] != "advisory" || wal["lift"] != nil {
		t.Fatalf("wal_retention = %v", wal)
	}
}

func TestModelLiftAPI_DatabaseParameter(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	f.register("billing")
	h := f.router(testViewerUser())
	if code, body := autonomyCall(t, h, "GET", "/api/v1/model-lift?database=billing",
		""); code != 200 || body["database"] != "billing" {
		t.Fatalf("billing = %d %v", code, body)
	}
	if code, _ := autonomyCall(t, h, "GET", "/api/v1/model-lift?database=nope",
		""); code != 404 {
		t.Fatalf("unknown database = %d", code)
	}
	long := strings.Repeat("d", 201)
	if code, _ := autonomyCall(t, h, "GET", "/api/v1/model-lift?database="+long,
		""); code != 400 {
		t.Fatalf("over-long database = %d", code)
	}
}

func TestModelLiftAPI_NeedsASignedInUser(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	if code, _ := autonomyCall(t, f.router(nil), "GET", "/api/v1/model-lift",
		""); code != 401 {
		t.Fatalf("anonymous = %d, want 401", code)
	}
}

func replayCasePath(id sre.UUID, query string) string {
	return "/api/v1/databases/orders/investigations/" + string(id) + "/replay-case" + query
}

func TestReplayCaseAPI_ContestedInvestigationExports(t *testing.T) {
	mgr, orders, _ := sreFixture(t)
	h := sreRouter(t, mgr, testOperatorUser())
	code, body, _ := sreCall(t, h, "GET", replayCasePath(orders.ID, ""))
	if code != 409 || !strings.Contains(body, "not_contested") {
		t.Fatalf("uncontested = %d %s", code, body)
	}
	svc := mgr.GetInstance("orders").Investigations
	if _, err := svc.RecordOutcome(context.Background(), orders.ID, sre.OutcomeRequest{
		Verdict: "refuted", ActualNode: "ddl_lock_queue", Actor: "operator"}); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	code, body, ctype := sreCall(t, h, "GET", replayCasePath(orders.ID, ""))
	var exp struct {
		Case struct {
			Schema string `json:"schema"`
			Gold   struct {
				Root string `json:"root"`
			} `json:"gold"`
		} `json:"case"`
		GraphRootPreserved bool `json:"graph_root_preserved"`
	}
	if code != 200 || !strings.Contains(ctype, "application/json") ||
		json.Unmarshal([]byte(body), &exp) != nil ||
		exp.Case.Schema != "pg_sage.sre.replay_case.v1" ||
		exp.Case.Gold.Root != "ddl_lock_queue" {
		t.Fatalf("export = %d %s", code, body)
	}
	if strings.Contains(body, "public.orders") || strings.Contains(body, string(orders.ID)) {
		t.Fatalf("the default export must hash identifiers: %s", body)
	}
	code, body, _ = sreCall(t, h, "GET", replayCasePath(orders.ID, "?keep_identifiers=true"))
	if code != 200 || !strings.Contains(body, "public.orders") {
		t.Fatalf("opted-in export = %d %s", code, body)
	}
}

func TestReplayCaseAPI_RolesAndErrors(t *testing.T) {
	mgr, orders, _ := sreFixture(t)
	if code, _, _ := sreCall(t, sreRouter(t, mgr, testViewerUser()), "GET",
		replayCasePath(orders.ID, "")); code != 403 {
		t.Fatalf("viewer = %d, want 403", code)
	}
	h := sreRouter(t, mgr, testOperatorUser())
	if code, body, _ := sreCall(t, h, "GET", replayCasePath(orders.ID,
		"?keep_identifiers=maybe")); code != 400 {
		t.Fatalf("bad flag = %d %s", code, body)
	}
	if code, _, _ := sreCall(t, h, "GET", replayCasePath(sre.NewUUID(), "")); code != 404 {
		t.Fatalf("unknown id = %d", code)
	}
	if code, _, _ := sreCall(t, h, "GET",
		"/api/v1/databases/nope/investigations/"+string(orders.ID)+"/replay-case"); code != 404 {
		t.Fatalf("unknown database = %d", code)
	}
}
