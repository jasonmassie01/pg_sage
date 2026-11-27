package approvalcard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/store"
)

// tuning-05b: a query-hint card carries the plan comparison that gated the
// hint (detail.hint_evidence), lifted with the stored key names. It is
// read-only: the card hash and chat text do not change.

// No concurrent access tests: hintEvidenceOf is a pure function of one
// card's inputs and adds no shared state (the WARN goes to slog).

const (
	plainPlanJSON = `[{"Plan":{"Node Type":"Seq Scan","Plan Rows":1200,` +
		`"Relation Name":"orders","Total Cost":412}}]`
	hintedPlanJSON = `[{"Plan":{"Index Name":"idx_orders_customer",` +
		`"Node Type":"Index Scan","Plan Rows":1200,"Relation Name":"orders",` +
		`"Total Cost":254.6}}]`
)

func changedEvidenceJSON(plain, hinted string) string {
	return `{"verdict":"changed","reason":"the hinted plan uses idx_orders_customer",` +
		`"pg_major":17,"generic_plan":false,"tolerance_pct":5,"basis":"plan",` +
		`"plain_cost":412.0,"hinted_cost":254.6,"plain_plan_hash":"a1b2",` +
		`"hinted_plan_hash":"c3d4","cost_delta_pct":-38.2,` +
		`"plain_plan":` + plain + `,"hinted_plan":` + hinted + `}`
}

// detailOf decodes a finding detail the way the loader does.
func detailOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("detail %s: %v", s, err)
	}
	return m
}

func hintAction(id int, actionType string) store.QueuedAction {
	return queued(id, actionType, "INSERT INTO hint_plan.hints (query_id, "+
		"application_name, hints) VALUES (4242, '', 'IndexScan(orders idx_orders_customer)')",
		"DELETE FROM hint_plan.hints WHERE query_id = 4242", "moderate")
}

func hintInputs(a store.QueuedAction, detail map[string]any) Inputs {
	return Inputs{Database: "orders", Action: a, Now: now, TrustLevel: "advisory",
		Finding: &FindingRow{ID: a.FindingID, Category: "query_tuning",
			Severity: "warning", ObjectType: "query", Object: "queryid:4242",
			Title: "Hint for query 4242", Detail: detail}}
}

// captureWarnings sends slog's default logger to a buffer for one test.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func hintWarnings(buf *bytes.Buffer) []string {
	var out []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "hint_evidence") {
			out = append(out, line)
		}
	}
	return out
}

func assertJSONEqual(t *testing.T, what string, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: card JSON %q: %v", what, got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: want JSON: %v", what, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s = %s, want %s", what, got, want)
	}
}

func TestHintEvidence_ChangedComparisonIsLiftedWithBothPlans(t *testing.T) {
	for _, typ := range []string{"apply_query_hint", "retire_query_hint"} {
		t.Run(typ, func(t *testing.T) {
			ev := changedEvidenceJSON(plainPlanJSON, hintedPlanJSON)
			c := Assemble(hintInputs(hintAction(41, typ),
				detailOf(t, `{"queryid":4242,"hint_evidence":`+ev+`}`)))
			h := c.HintEvidence
			if h == nil {
				t.Fatal("hint_evidence is absent from a hint card")
			}
			if h.Verdict != "changed" || h.Reason != "the hinted plan uses idx_orders_customer" ||
				h.Basis != "plan" || h.PGMajor != 17 || h.GenericPlan ||
				h.PlainPlanHash != "a1b2" || h.HintedPlanHash != "c3d4" {
				t.Fatalf("hint evidence head = %+v", h)
			}
			if h.PlainCost == nil || *h.PlainCost != 412 || h.HintedCost == nil ||
				*h.HintedCost != 254.6 || h.CostDeltaPct == nil || *h.CostDeltaPct != -38.2 {
				t.Fatalf("costs = %v %v %v", h.PlainCost, h.HintedCost, h.CostDeltaPct)
			}
			assertJSONEqual(t, "plain_plan", h.PlainPlan, plainPlanJSON)
			assertJSONEqual(t, "hinted_plan", h.HintedPlan, hintedPlanJSON)
			if h.PlainPlanOmitted != "" || h.HintedPlanOmitted != "" {
				t.Fatalf("omitted = %q %q", h.PlainPlanOmitted, h.HintedPlanOmitted)
			}
		})
	}
}

// The card JSON keeps the stored key names: nothing is renamed between the
// finding and the card.
func TestHintEvidence_CardJSONKeepsTheStoredKeyNames(t *testing.T) {
	c := Assemble(hintInputs(hintAction(42, "apply_query_hint"),
		detailOf(t, `{"hint_evidence":`+changedEvidenceJSON(plainPlanJSON,
			hintedPlanJSON)+`}`)))
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		HintEvidence map[string]json.RawMessage `json:"hint_evidence"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"verdict", "reason", "basis", "plain_cost", "hinted_cost",
		"cost_delta_pct", "plain_plan_hash", "hinted_plan_hash", "plain_plan",
		"hinted_plan", "generic_plan", "pg_major"} {
		if _, ok := out.HintEvidence[key]; !ok {
			t.Fatalf("card hint_evidence lacks %q: %s", key, raw)
		}
	}
}

func TestHintEvidence_MalformedGivesNoSectionAndOneWarn(t *testing.T) {
	good := changedEvidenceJSON(plainPlanJSON, hintedPlanJSON)
	cases := map[string]string{
		"plain_cost string": strings.Replace(good, `"plain_cost":412.0`,
			`"plain_cost":"412.0"`, 1),
		"plain_plan number": changedEvidenceJSON("17", hintedPlanJSON),
		"hinted_plan string": changedEvidenceJSON(plainPlanJSON,
			`"[{\"Plan\":{}}]"`),
		"hint_evidence array": `[` + good + `]`,
		"verdict missing":     `{"reason":"x","plain_cost":1}`,
		"pg_major fraction":   strings.Replace(good, `"pg_major":17`, `"pg_major":17.5`, 1),
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			buf := captureWarnings(t)
			a := hintAction(43, "apply_query_hint")
			bad := Assemble(hintInputs(a, detailOf(t, `{"queryid":4242,"hint_evidence":`+
				ev+`}`)))
			if bad.HintEvidence != nil {
				t.Fatalf("malformed evidence gave a section: %+v", bad.HintEvidence)
			}
			warns := hintWarnings(buf)
			if len(warns) != 1 || !strings.Contains(warns[0], "level=WARN") ||
				!strings.Contains(warns[0], fmt.Sprintf("finding_id=%d", a.FindingID)) {
				t.Fatalf("want one WARN naming finding %d, got %q", a.FindingID, warns)
			}
			plain := Assemble(hintInputs(a, detailOf(t, `{"queryid":4242}`)))
			b1, _ := json.Marshal(bad)
			b2, _ := json.Marshal(plain)
			if !bytes.Equal(b1, b2) {
				t.Fatalf("malformed evidence changed the card:\n%s\nwant\n%s", b1, b2)
			}
		})
	}
}

// A scalar hint_evidence gives no section and one WARN. The card is not
// compared with a card without the key: evidenceOf already cites a scalar
// detail value as metric evidence, before and after tuning-05b.
func TestHintEvidence_ScalarGivesNoSectionAndOneWarn(t *testing.T) {
	buf := captureWarnings(t)
	a := hintAction(57, "apply_query_hint")
	c := Assemble(hintInputs(a, detailOf(t, `{"hint_evidence":"changed"}`)))
	if c.HintEvidence != nil {
		t.Fatalf("scalar evidence gave a section: %+v", c.HintEvidence)
	}
	if w := hintWarnings(buf); len(w) != 1 ||
		!strings.Contains(w[0], fmt.Sprintf("finding_id=%d", a.FindingID)) {
		t.Fatalf("want one WARN naming finding %d, got %q", a.FindingID, w)
	}
	if !strings.Contains(evidenceLabels(c), "hint evidence=changed") {
		t.Fatalf("the existing metric citation changed:\n%s", evidenceLabels(c))
	}
}

func TestHintEvidence_AbsentFromTheJSONWhenNotAHintCard(t *testing.T) {
	ev := `{"hint_evidence":` + changedEvidenceJSON(plainPlanJSON, hintedPlanJSON) + `}`
	cases := map[string]Inputs{
		"non-hint action": hintInputs(queued(44, "create_index_concurrently",
			"CREATE INDEX CONCURRENTLY idx_a ON public.orders (a)",
			"DROP INDEX CONCURRENTLY public.idx_a", "moderate"), detailOf(t, ev)),
		"hint without evidence": hintInputs(hintAction(45, "apply_query_hint"),
			detailOf(t, `{"queryid":4242}`)),
		"hint with nil detail": hintInputs(hintAction(46, "apply_query_hint"), nil),
		"hint evidence null": hintInputs(hintAction(47, "apply_query_hint"),
			detailOf(t, `{"hint_evidence":null}`)),
	}
	deleted := hintInputs(hintAction(48, "apply_query_hint"), nil)
	deleted.Finding = nil // the loader read no finding row
	cases["deleted finding"] = deleted
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			buf := captureWarnings(t)
			c := Assemble(in)
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if c.HintEvidence != nil || bytes.Contains(raw, []byte(`"hint_evidence"`)) {
				t.Fatalf("hint_evidence present: %s", raw)
			}
			if w := hintWarnings(buf); len(w) != 0 {
				t.Fatalf("absent evidence logged %q", w)
			}
		})
	}
}

// The writer returns early when no plan was read: verdict and reason only.
func TestHintEvidence_UnverifiableWithoutPlansKeepsVerdictAndReason(t *testing.T) {
	c := Assemble(hintInputs(hintAction(49, "apply_query_hint"), detailOf(t,
		`{"hint_evidence":{"verdict":"unverifiable","reason":"pg_hint_plan not loaded",`+
			`"pg_major":14,"generic_plan":true,"tolerance_pct":5}}`)))
	h := c.HintEvidence
	if h == nil || h.Verdict != "unverifiable" || h.Reason != "pg_hint_plan not loaded" ||
		h.PGMajor != 14 || !h.GenericPlan {
		t.Fatalf("hint evidence = %+v", h)
	}
	if h.PlainCost != nil || h.HintedCost != nil || h.CostDeltaPct != nil ||
		h.PlainPlan != nil || h.HintedPlan != nil || h.PlainPlanHash != "" {
		t.Fatalf("unverifiable evidence grew costs or plans: %+v", h)
	}
	raw, _ := json.Marshal(h)
	for _, key := range []string{"plain_plan", "hinted_plan", "plain_cost", "cost_delta_pct"} {
		if bytes.Contains(raw, []byte(`"`+key+`"`)) {
			t.Fatalf("unverifiable JSON carries %s: %s", key, raw)
		}
	}
}

// paddedPlan is a compact EXPLAIN JSON array (keys sorted) of exactly n bytes.
func paddedPlan(t *testing.T, n int) string {
	t.Helper()
	head, tail := `[{"Plan":{"Filter":"`, `","Node Type":"Seq Scan"}}]`
	pad := n - len(head) - len(tail)
	if pad < 0 {
		t.Fatalf("plan of %d bytes is too small", n)
	}
	return head + strings.Repeat("x", pad) + tail
}

func TestHintEvidence_PlanOverTheCapIsDroppedAndNamed(t *testing.T) {
	big := paddedPlan(t, 70<<10)
	c := Assemble(hintInputs(hintAction(50, "apply_query_hint"),
		detailOf(t, `{"hint_evidence":`+changedEvidenceJSON(big, hintedPlanJSON)+`}`)))
	h := c.HintEvidence
	if h == nil {
		t.Fatal("an oversized plan dropped the whole section")
	}
	if h.PlainPlan != nil {
		t.Fatalf("a 70 KB plan was kept (%d bytes)", len(h.PlainPlan))
	}
	want := fmt.Sprintf("plan of %d bytes exceeds %d", 70<<10, 64<<10)
	if h.PlainPlanOmitted != want {
		t.Fatalf("plain_plan_omitted = %q, want %q", h.PlainPlanOmitted, want)
	}
	assertJSONEqual(t, "hinted_plan", h.HintedPlan, hintedPlanJSON)
	if h.HintedPlanOmitted != "" {
		t.Fatalf("hinted_plan_omitted = %q", h.HintedPlanOmitted)
	}
}

func TestHintEvidence_PlanOfExactlyTheCapIsKept(t *testing.T) {
	edge := paddedPlan(t, 64<<10)
	if len(edge) != 64<<10 {
		t.Fatalf("premise: plan is %d bytes", len(edge))
	}
	c := Assemble(hintInputs(hintAction(51, "apply_query_hint"),
		detailOf(t, `{"hint_evidence":`+changedEvidenceJSON(plainPlanJSON, edge)+`}`)))
	h := c.HintEvidence
	if h == nil || h.HintedPlanOmitted != "" {
		t.Fatalf("a 64 KB plan was omitted: %+v", h)
	}
	assertJSONEqual(t, "hinted_plan", h.HintedPlan, edge)
	over := paddedPlan(t, 64<<10+1)
	c = Assemble(hintInputs(hintAction(52, "apply_query_hint"),
		detailOf(t, `{"hint_evidence":`+changedEvidenceJSON(plainPlanJSON, over)+`}`)))
	if c.HintEvidence == nil || c.HintEvidence.HintedPlan != nil ||
		c.HintEvidence.HintedPlanOmitted == "" {
		t.Fatalf("a plan one byte over the cap was kept: %+v", c.HintEvidence)
	}
}

// A writer-omitted plan keeps the writer's note.
func TestHintEvidence_WriterOmittedPlanKeepsItsNote(t *testing.T) {
	c := Assemble(hintInputs(hintAction(53, "apply_query_hint"), detailOf(t,
		`{"hint_evidence":{"verdict":"changed","reason":"","plain_cost":10,`+
			`"hinted_cost":5,"plain_plan_hash":"a","hinted_plan_hash":"b",`+
			`"cost_delta_pct":-50,"plain_plan_omitted":"plan of 90000 bytes exceeds 65536",`+
			`"hinted_plan":`+hintedPlanJSON+`,"pg_major":17,"generic_plan":false}}`)))
	h := c.HintEvidence
	if h == nil || h.PlainPlan != nil ||
		h.PlainPlanOmitted != "plan of 90000 bytes exceeds 65536" || h.HintedPlan == nil {
		t.Fatalf("hint evidence = %+v", h)
	}
}

func TestHintEvidence_ZeroDeltaIsKeptAndAbsentDeltaStaysAbsent(t *testing.T) {
	zero := strings.Replace(changedEvidenceJSON(plainPlanJSON, hintedPlanJSON),
		`"cost_delta_pct":-38.2`, `"cost_delta_pct":0`, 1)
	c := Assemble(hintInputs(hintAction(54, "apply_query_hint"),
		detailOf(t, `{"hint_evidence":`+zero+`}`)))
	if c.HintEvidence == nil || c.HintEvidence.CostDeltaPct == nil ||
		*c.HintEvidence.CostDeltaPct != 0 {
		t.Fatalf("a zero delta was lost: %+v", c.HintEvidence)
	}
	raw, _ := json.Marshal(c.HintEvidence)
	if !bytes.Contains(raw, []byte(`"cost_delta_pct":0`)) {
		t.Fatalf("zero delta absent from JSON: %s", raw)
	}
	// plain_cost 0: the writer stores no delta.
	absent := strings.Replace(strings.Replace(changedEvidenceJSON(plainPlanJSON,
		hintedPlanJSON), `"cost_delta_pct":-38.2,`, ``, 1), `"plain_cost":412.0`,
		`"plain_cost":0`, 1)
	c = Assemble(hintInputs(hintAction(55, "apply_query_hint"),
		detailOf(t, `{"hint_evidence":`+absent+`}`)))
	if c.HintEvidence == nil || c.HintEvidence.CostDeltaPct != nil ||
		c.HintEvidence.PlainCost == nil || *c.HintEvidence.PlainCost != 0 {
		t.Fatalf("absent delta = %+v", c.HintEvidence)
	}
	raw, _ = json.Marshal(c.HintEvidence)
	if bytes.Contains(raw, []byte(`"cost_delta_pct"`)) {
		t.Fatalf("absent delta appears in JSON: %s", raw)
	}
}

// goldenHintCardHash is ContentHash of hintAction(41, "apply_query_hint")
// before tuning-05b, computed outside Go from the "approval-card-v1" parts:
// pending approvals keep their hash across the upgrade.
const goldenHintCardHash = "46212f7de027fe5ed06c1b0dd013e166181328a45164e18e3976420651606c53"

func TestContentHash_UnchangedByHintEvidence(t *testing.T) {
	a := hintAction(41, "apply_query_hint")
	without := Assemble(hintInputs(a, detailOf(t, `{"queryid":4242}`)))
	with := Assemble(hintInputs(a, detailOf(t, `{"queryid":4242,"hint_evidence":`+
		changedEvidenceJSON(plainPlanJSON, hintedPlanJSON)+`}`)))
	if with.HintEvidence == nil {
		t.Fatal("premise: the card has no hint_evidence")
	}
	if with.CardHash != without.CardHash || with.CardHash != ContentHash(a) ||
		with.CardHash != goldenHintCardHash {
		t.Fatalf("card hash with %s, without %s, golden %s", with.CardHash,
			without.CardHash, goldenHintCardHash)
	}
}

// The chat and Slack text do not change: the plans are a web-card section.
func TestHintEvidence_ChatTextIsUnchanged(t *testing.T) {
	a := hintAction(56, "apply_query_hint")
	without := Assemble(hintInputs(a, detailOf(t, `{"queryid":4242}`)))
	with := Assemble(hintInputs(a, detailOf(t, `{"queryid":4242,"hint_evidence":`+
		changedEvidenceJSON(plainPlanJSON, hintedPlanJSON)+`}`)))
	if with.HintEvidence == nil {
		t.Fatal("premise: the card has no hint_evidence")
	}
	if got, want := Text(with, now), Text(without, now); got != want {
		t.Fatalf("chat text changed:\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(Text(with, now), "Seq Scan") {
		t.Fatal("chat text carries a plan")
	}
}
