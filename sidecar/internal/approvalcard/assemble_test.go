package approvalcard

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/store"
)

// One card per queued action: what, on which objects, the evidence with
// numbers, the model's rationale, the predicted effect, the exact SQL and
// rollback, the blast radius and guardrails, why a human must decide,
// and when the request expires.

var now = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func queued(id int, actionType, sql, rollback, risk string) store.QueuedAction {
	return store.QueuedAction{ID: id, FindingID: 500 + id, ProposedSQL: sql,
		RollbackSQL: rollback, ActionRisk: risk, Status: "pending",
		ActionType: actionType, PolicyDecision: "queue_approval",
		ProposedAt: now.Add(-time.Hour), ExpiresAt: now.Add(23 * time.Hour)}
}

func contractOf(t *testing.T, a store.QueuedAction) *executor.ActionContract {
	t.Helper()
	c, ok := executor.ContractForQueuedAction(a)
	if !ok {
		return nil
	}
	return &c
}

func reasonCodes(c Card) []string {
	out := make([]string, 0, len(c.Why))
	for _, r := range c.Why {
		out = append(out, r.Code)
	}
	return out
}

func hasCode(c Card, code string) bool {
	for _, r := range c.Why {
		if r.Code == code {
			return true
		}
	}
	return false
}

func evidenceLabels(c Card) string {
	var b strings.Builder
	for _, e := range c.Evidence {
		b.WriteString(e.Kind + ":" + e.Label + "=" + e.Value + "|" + e.Ref + "\n")
	}
	return b.String()
}

func TestAssembleCreateIndexFromTheOptimizer(t *testing.T) {
	a := queued(1, "create_index_concurrently",
		"CREATE INDEX CONCURRENTLY idx_orders_customer ON public.orders (customer_id)",
		"DROP INDEX CONCURRENTLY public.idx_orders_customer", "moderate")
	a.Guardrails = []string{"maintenance_window"}
	in := Inputs{Database: "orders", Action: a, Now: now, TrustLevel: "advisory",
		Contract: contractOf(t, a),
		Finding: &FindingRow{ID: a.FindingID, Category: "index_optimization",
			Severity: "warning", ObjectType: "index",
			Object: "public.orders|btree(customer_id)",
			Title:  "Index recommendation for public.orders",
			Detail: map[string]any{"table": "public.orders",
				"llm_rationale": "Seq scans on orders filter by customer_id",
				"confidence_score": 0.82, "estimated_improvement_pct": 38.5,
				"estimated_size_bytes": float64(12 << 20), "what_if_verdict": "unverified",
				"what_if_reason": "hypopg unavailable", "hypopg_validated": false,
				"affected_queries": []any{"SELECT * FROM orders WHERE customer_id = $1"},
				"queryids": []any{float64(4242)}, "seq_scan": float64(18233)}},
		Decision: &DecisionRow{ID: 77, Verdict: "queue_approval", RiskTier: "moderate",
			Reason: "approval_required", EvidenceID: "ev-1",
			Guardrails: []string{"approval_required"}}}
	c := Assemble(in)
	if c.QueueID != 1 || c.Database != "orders" ||
		c.Title != "Index recommendation for public.orders" ||
		c.ActionType != "create_index_concurrently" || c.SQL != a.ProposedSQL ||
		c.Rollback.SQL != a.RollbackSQL || c.Rollback.Class != "reversible" {
		t.Fatalf("card head = %+v", c)
	}
	if c.Rationale == nil || c.Rationale.Source != "llm" ||
		!strings.Contains(c.Rationale.Text, "customer_id") ||
		c.Rationale.Confidence == nil || *c.Rationale.Confidence != 0.82 {
		t.Fatalf("rationale = %+v", c.Rationale)
	}
	p := c.Predicted
	if p.ImprovementPct == nil || *p.ImprovementPct != 38.5 ||
		p.EstimatedSizeBytes == nil || *p.EstimatedSizeBytes != 12<<20 ||
		len(p.AffectedQueries) != 1 || len(p.QueryIDs) != 1 || p.QueryIDs[0] != 4242 ||
		p.WhatIfVerdict != "unverified" || p.Method != "llm_estimate" || p.Forecast != nil {
		t.Fatalf("predicted = %+v", p)
	}
	for _, code := range []string{"what_if_unverified", "gate:approval_required",
		"trust_level"} {
		if !hasCode(c, code) {
			t.Fatalf("why %v lacks %s", reasonCodes(c), code)
		}
	}
	ev := evidenceLabels(c)
	for _, want := range []string{"finding:", "finding:" + itoa(a.FindingID),
		"seq scan=18233", "query:", "queryid:4242", "decision:77"} {
		if !strings.Contains(ev, want) {
			t.Fatalf("evidence lacks %q:\n%s", want, ev)
		}
	}
	if c.Risk.Tier != "moderate" || !strings.Contains(c.Risk.Lock, "SHARE UPDATE EXCLUSIVE") ||
		!containsAll(c.Risk.Guardrails, "maintenance_window", "approval_required") ||
		len(c.Risk.PostChecks) == 0 || !containsAll(c.Targets, "public.orders") {
		t.Fatalf("risk = %+v targets = %v", c.Risk, c.Targets)
	}
	if !c.ExpiresAt.Equal(a.ExpiresAt) || c.CardHash != ContentHash(a) || c.CardHash == "" {
		t.Fatalf("expiry/hash = %v %q", c.ExpiresAt, c.CardHash)
	}
}

func TestAssembleVerifiedIndexHasNoWhatIfReason(t *testing.T) {
	a := queued(2, "create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i ON public.t (a)", "DROP INDEX CONCURRENTLY public.i",
		"moderate")
	c := Assemble(Inputs{Database: "db", Action: a, Now: now, Contract: contractOf(t, a),
		Finding: &FindingRow{ID: a.FindingID, Title: "Index", Detail: map[string]any{
			"what_if_verdict": "verified", "hypopg_validated": true,
			"estimated_improvement_pct": 51.0}}})
	if hasCode(c, "what_if_unverified") || c.Predicted.Method != "hypopg" {
		t.Fatalf("why %v, method %q", reasonCodes(c), c.Predicted.Method)
	}
	if len(c.Why) == 0 || c.Why[len(c.Why)-1].Code == "" {
		t.Fatalf("a queued card must always say why: %+v", c.Why)
	}
}

func TestAssembleDropUnusedIndex(t *testing.T) {
	a := queued(3, "drop_unused_index", "DROP INDEX CONCURRENTLY public.idx_old",
		"CREATE INDEX CONCURRENTLY idx_old ON public.t (b)", "moderate")
	c := Assemble(Inputs{Database: "db", Action: a, Now: now, Contract: contractOf(t, a),
		Finding: &FindingRow{ID: a.FindingID, Title: "Unused index public.idx_old",
			Object: "public.idx_old", Detail: map[string]any{
				"idx_scan": float64(0), "index_size_bytes": float64(4 << 30),
				"unused_since": "2026-07-01T00:00:00Z", "replica_index_usage": "unknown",
				"approval_required": "standby index usage is unknown"}}})
	if !hasCode(c, "approval_required") {
		t.Fatalf("why = %v", reasonCodes(c))
	}
	if !strings.Contains(textOf(c.Why), "standby index usage is unknown") {
		t.Fatalf("why text = %+v", c.Why)
	}
	ev := evidenceLabels(c)
	for _, want := range []string{"idx scan=0", "unused since=2026-07-01T00:00:00Z",
		"index size bytes=4294967296"} {
		if !strings.Contains(ev, want) {
			t.Fatalf("evidence lacks %q:\n%s", want, ev)
		}
	}
	if c.Rationale == nil || c.Rationale.Source != "rule" {
		// No model text: the rule's recommendation (empty here) is absent too.
		if c.Rationale != nil {
			t.Fatalf("rationale = %+v", c.Rationale)
		}
	}
	if !containsAll(c.Targets, "public.idx_old") {
		t.Fatalf("targets = %v", c.Targets)
	}
}

func TestAssembleRestartGUC(t *testing.T) {
	a := queued(4, "alter_system_guc", "ALTER SYSTEM SET shared_buffers = '4GB'",
		"ALTER SYSTEM RESET shared_buffers", "high")
	c := Assemble(Inputs{Database: "db", Action: a, Now: now, Contract: contractOf(t, a),
		Finding: &FindingRow{ID: a.FindingID, Title: "shared_buffers is small",
			Recommendation: "Raise shared_buffers to 25% of RAM",
			Detail: map[string]any{"rationale": "cache hit ratio is 91%",
				"current_value": "128MB"}}})
	if !hasCode(c, "guc_restart") {
		t.Fatalf("why = %v", reasonCodes(c))
	}
	if c.Rationale == nil || c.Rationale.Source != "llm" ||
		c.Rationale.Text != "cache hit ratio is 91%" {
		t.Fatalf("rationale = %+v", c.Rationale)
	}
	if !strings.Contains(c.Risk.Lock, "restart") {
		t.Fatalf("lock = %q", c.Risk.Lock)
	}
}

func TestAssembleNonAllowlistedGUC(t *testing.T) {
	a := queued(5, "alter_system_guc", "ALTER SYSTEM SET fsync = off", "", "high")
	c := Assemble(Inputs{Database: "db", Action: a, Now: now, Contract: contractOf(t, a)})
	if !hasCode(c, "guc_not_allowlisted") {
		t.Fatalf("why = %v", reasonCodes(c))
	}
	if c.Rollback.SQL != "" || c.Rollback.Note == "" {
		t.Fatalf("rollback = %+v", c.Rollback)
	}
}

func TestAssembleAnalyzeVacuumReindexAutovacuum(t *testing.T) {
	cases := []struct {
		typ, sql, lock string
	}{
		{"analyze_table", "ANALYZE public.orders", "SHARE UPDATE EXCLUSIVE"},
		{"vacuum_table", "VACUUM (ANALYZE) public.orders", "SHARE UPDATE EXCLUSIVE"},
		{"reindex_concurrently", "REINDEX INDEX CONCURRENTLY public.i", "SHARE UPDATE EXCLUSIVE"},
		{"set_table_autovacuum",
			"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.02)",
			"SHARE UPDATE EXCLUSIVE"},
		{"alter_table", "ALTER TABLE public.orders ALTER COLUMN a TYPE bigint",
			"ACCESS EXCLUSIVE"},
	}
	for i, tc := range cases {
		a := queued(10+i, tc.typ, tc.sql, "", "safe")
		c := Assemble(Inputs{Database: "db", Action: a, Now: now, Contract: contractOf(t, a),
			TrustLevel: "observation"})
		if c.ActionType != tc.typ || !strings.Contains(c.Risk.Lock, tc.lock) ||
			c.SQL != tc.sql || !hasCode(c, "trust_level") {
			t.Errorf("%s: card = %+v", tc.typ, c)
		}
		if c.Title == "" || c.Rollback.Class == "" && contractOf(t, a) != nil {
			t.Errorf("%s: title %q rollback %+v", tc.typ, c.Title, c.Rollback)
		}
	}
}

func TestAssembleCancelBackend(t *testing.T) {
	a := queued(20, "cancel_backend", "SELECT pg_cancel_backend(5151)", "", "moderate")
	c := Assemble(Inputs{Database: "db", Action: a, Now: now, Contract: contractOf(t, a),
		Finding: &FindingRow{ID: a.FindingID, Title: "Cancel blocking backend pid 5151",
			Detail: map[string]any{"blocked_sessions": float64(2),
				"narrative": "pid 5151 holds a lock 2 sessions wait on"}}})
	if !strings.Contains(c.Risk.Lock, "cancels one query") || c.Rationale == nil ||
		c.Rationale.Source != "llm" {
		t.Fatalf("card = %+v rationale %+v", c.Risk, c.Rationale)
	}
	if !strings.Contains(evidenceLabels(c), "blocked sessions=2") {
		t.Fatalf("evidence:\n%s", evidenceLabels(c))
	}
}

func TestAssembleLegacyRowWithoutTypeOrFinding(t *testing.T) {
	a := queued(30, "", "CREATE INDEX CONCURRENTLY i ON public.t (a)", "", "moderate")
	a.PolicyDecision = ""
	c := Assemble(Inputs{Database: "db", Action: a, Now: now, Contract: contractOf(t, a)})
	if c.ActionType != "create_index_concurrently" || c.Finding != nil ||
		!strings.Contains(c.Title, "queue item 30") {
		t.Fatalf("legacy card = %+v", c)
	}
	if !hasCode(c, "finding_missing") || !hasCode(c, "queued_for_approval") {
		t.Fatalf("why = %v", reasonCodes(c))
	}
	if c.Evidence == nil || c.Targets == nil || c.Risk.Guardrails == nil {
		t.Fatal("JSON slices must be empty, not null")
	}
}

func TestAssembleUnknownTypeAndZeroInputs(t *testing.T) {
	c := Assemble(Inputs{})
	if c.QueueID != 0 || c.CardHash != ContentHash(store.QueuedAction{}) || len(c.Why) == 0 {
		t.Fatalf("zero card = %+v", c)
	}
	a := queued(31, "mystery_action", "SELECT 1", "", "")
	c = Assemble(Inputs{Database: "db", Action: a, Now: now})
	if c.Risk.Tier != "unknown" || c.Rollback.Class != "" || c.Risk.Lock != "" {
		t.Fatalf("unknown type card = %+v", c.Risk)
	}
}

func TestAssemblePriorRejectionAndRevision(t *testing.T) {
	a := queued(40, "create_index_concurrently", "CREATE INDEX CONCURRENTLY i ON public.t (a)",
		"", "moderate")
	rec := int64(9)
	rev := 3
	a.RecommendationID, a.RecommendationRevision, a.ContentHash = &rec, &rev, "rev-hash"
	c := Assemble(Inputs{Database: "db", Action: a, Now: now, Contract: contractOf(t, a),
		Revision: &RevisionRow{ID: 9, Revision: 3, ContentHash: "rev-hash",
			Title: "Revised index", Recommendation: "Index customer_id",
			Evidence: map[string]any{"calls": float64(120000)}},
		Rejection: &RejectionRow{QueueID: 12, DecidedAt: now.Add(-48 * time.Hour),
			Reason: "app owns this index"}})
	if c.Title != "Revised index" || c.Recommendation == nil ||
		c.Recommendation.ID != 9 || c.Recommendation.Revision != 3 {
		t.Fatalf("card = %+v rec %+v", c.Title, c.Recommendation)
	}
	if !hasCode(c, "operator_rejected_before") ||
		!strings.Contains(textOf(c.Why), "app owns this index") {
		t.Fatalf("why = %+v", c.Why)
	}
	if !strings.Contains(evidenceLabels(c), "calls=120000") ||
		!strings.Contains(evidenceLabels(c), "recommendation:9@3") {
		t.Fatalf("evidence:\n%s", evidenceLabels(c))
	}
}

func TestAssembleSnoozed(t *testing.T) {
	a := queued(50, "analyze_table", "ANALYZE public.t", "", "safe")
	until := now.Add(3 * time.Hour)
	c := Assemble(Inputs{Database: "db", Action: a, Now: now,
		Snooze: &SnoozeRow{Until: until, By: 4, Reason: "busy hours"}})
	if c.SnoozedUntil == nil || !c.SnoozedUntil.Equal(until) || c.SnoozeReason != "busy hours" {
		t.Fatalf("snooze = %v %q", c.SnoozedUntil, c.SnoozeReason)
	}
	past := Assemble(Inputs{Database: "db", Action: a, Now: now,
		Snooze: &SnoozeRow{Until: now.Add(-time.Minute), By: 4, Reason: "old"}})
	if past.SnoozedUntil != nil {
		t.Fatal("an ended snooze is still shown")
	}
}

func TestAssembleEvidenceIsBoundedAndSkipsInternals(t *testing.T) {
	detail := map[string]any{"ddl": "CREATE INDEX x", "index_fingerprint": "f",
		"llm_rationale": "r", "partition_plan": []any{"a"},
		"nested": map[string]any{"a": 1}, "long": strings.Repeat("x", 500)}
	for i := range 30 {
		detail["metric_"+itoa(100+i)] = float64(i)
	}
	a := queued(60, "analyze_table", "ANALYZE public.t", "", "safe")
	c := Assemble(Inputs{Database: "db", Action: a, Now: now,
		Finding: &FindingRow{ID: 1, Title: "t", Detail: detail}})
	ev := evidenceLabels(c)
	for _, banned := range []string{"ddl=", "index fingerprint", "llm rationale",
		"partition plan", "nested", "long="} {
		if strings.Contains(ev, banned) {
			t.Fatalf("evidence leaks %q:\n%s", banned, ev)
		}
	}
	metrics := strings.Count(ev, "metric:")
	if metrics != maxMetricEvidence {
		t.Fatalf("metric evidence = %d, want %d", metrics, maxMetricEvidence)
	}
}

func TestContentHashIsStableAndSensitive(t *testing.T) {
	a := queued(70, "create_index_concurrently", "CREATE INDEX CONCURRENTLY i ON t (a)",
		"DROP INDEX CONCURRENTLY i", "moderate")
	h := ContentHash(a)
	if len(h) != 64 || h != ContentHash(a) {
		t.Fatalf("hash %q not stable", h)
	}
	b := a
	b.Status, b.ExpiresAt, b.Reason = "approved", now, "x"
	if ContentHash(b) != h {
		t.Fatal("lifecycle fields must not change the content hash")
	}
	mutations := map[string]func(*store.QueuedAction){
		"sql":         func(q *store.QueuedAction) { q.ProposedSQL += " " },
		"rollback":    func(q *store.QueuedAction) { q.RollbackSQL = "" },
		"type":        func(q *store.QueuedAction) { q.ActionType = "other" },
		"finding":     func(q *store.QueuedAction) { q.FindingID++ },
		"queue":       func(q *store.QueuedAction) { q.ID++ },
		"content":     func(q *store.QueuedAction) { q.ContentHash = "x" },
		"risk":        func(q *store.QueuedAction) { q.ActionRisk = "high" },
		"sql vs roll": func(q *store.QueuedAction) { q.ProposedSQL, q.RollbackSQL = q.RollbackSQL, q.ProposedSQL },
	}
	for name, m := range mutations {
		c := a
		m(&c)
		if ContentHash(c) == h {
			t.Errorf("%s change kept the hash", name)
		}
	}
}

func TestTextCarriesTheWhyAndFitsChat(t *testing.T) {
	a := queued(80, "create_index_concurrently",
		"CREATE INDEX CONCURRENTLY idx ON public.orders (customer_id)",
		"DROP INDEX CONCURRENTLY public.idx", "moderate")
	c := Assemble(Inputs{Database: "orders", Action: a, Now: now, Contract: contractOf(t, a),
		Finding: &FindingRow{ID: a.FindingID, Title: "Index recommendation",
			Detail: map[string]any{"llm_rationale": "seq scans by customer_id",
				"estimated_improvement_pct": 38.5, "what_if_verdict": "unverified",
				"affected_queries": []any{strings.Repeat("SELECT 1 ", 1000)}}}})
	txt := Text(c, now)
	for _, want := range []string{"Why it needs you", "what-if", "Evidence",
		"Model rationale", "seq scans by customer_id", "Predicted effect", "38.5%",
		"CREATE INDEX CONCURRENTLY idx", "Rollback", "DROP INDEX CONCURRENTLY public.idx",
		"Risk: moderate", "Expires in 23h", "queue item 80", "orders"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("text lacks %q:\n%s", want, txt)
		}
	}
	if n := len([]rune(txt)); n > MaxTextRunes {
		t.Fatalf("text is %d runes, limit %d", n, MaxTextRunes)
	}
	if s := Summary(c); !strings.Contains(s, "38.5%") {
		t.Fatalf("summary = %q", s)
	}
	if s := Summary(Card{}); s != "" {
		t.Fatalf("empty summary = %q", s)
	}
}

func textOf(rs []Reason) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString(r.Text + "\n")
	}
	return b.String()
}

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}
