//go:build cgo

package reasoner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/diffscan"
	"github.com/pg-sage/sidecar/internal/changereview/impact"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/rvfixture"
)

// The reasoner on the real impact engine and store, over its own copy of
// schema rv (testsupport/rvfixture: Q1 reads email 2000 times, Q2 writes
// status, Q3 counts by created_at). The provider is the fake; every tool
// result comes from the live database.

type liveEnv struct {
	t   *testing.T
	ctx context.Context
	f   *rvfixture.Fixture
	eng *impact.Engine
}

func newLiveEnv(t *testing.T) *liveEnv {
	t.Helper()
	f := rvfixture.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	eng, err := impact.New(impact.Deps{Pool: f.Pool, Config: config.DefaultChangeReviewConfig(),
		Timeouts: catalogread.Timeouts{Statement: 10 * time.Second, Lock: 5 * time.Second},
		Logf:     t.Logf})
	if err != nil {
		t.Fatalf("impact.New: %v", err)
	}
	return &liveEnv{t: t, ctx: ctx, f: f, eng: eng}
}

// service is Change Review on the fixture database with the impact engine
// and the reasoner over model, wired as production wires it.
func (e *liveEnv) service(model *fakeLLM) *changereview.Service {
	e.t.Helper()
	cfg := config.DefaultChangeReviewConfig()
	r, err := New(Deps{Model: model.client(), Tools: e.eng, Bounds: testBounds(),
		Budget: changereview.NewBudget(e.f.Pool, int64(cfg.DailyTokensPerDatabase),
			int64(cfg.DailyTokensPerActor), nil), Logf: e.t.Logf})
	if err != nil {
		e.t.Fatalf("New: %v", err)
	}
	s, err := changereview.New(changereview.Deps{Database: e.f.Name, Pool: e.f.Pool,
		Config: cfg, Analyzer: e.eng, Reasoner: r, Redact: diffscan.Redact,
		HunkFilter: diffscan.HunkFilter, Logf: e.t.Logf, ModelTimeout: 60 * time.Second})
	if err != nil {
		e.t.Fatalf("changereview.New: %v", err)
	}
	if _, err := s.Start(e.ctx); err != nil {
		e.t.Fatalf("Start: %v", err)
	}
	return s
}

func (e *liveEnv) submit(s *changereview.Service, req changereview.Request) changereview.Review {
	e.t.Helper()
	req.WaitSeconds = 50
	got, _, err := s.Submit(e.ctx, caller, req)
	if err != nil {
		e.t.Fatalf("Submit: %v", err)
	}
	if got.State != changereview.StateComplete {
		got, err = s.Wait(e.ctx, got.ID, 60*time.Second)
		if err != nil || got.State != changereview.StateComplete {
			e.t.Fatalf("review %s is %s (%s, %v)", got.ID, got.State, got.FailureReason, err)
		}
	}
	return got
}

const rvRenameSQL = "ALTER TABLE rv.rv_orders RENAME COLUMN email TO email_address"

var rvRailsDiff = strings.Replace(railsRenameDiff, ":orders, :email", `"rv.rv_orders", :email`,
	1)

func TestLive_RailsRenameIsTranslatedInspectedAndCited(t *testing.T) {
	e := newLiveEnv(t)
	q1 := e.f.Q1
	fake := newFakeLLM(t,
		calls(toolCall{ToolInspect, jsonArgs(map[string]any{"sql": rvRenameSQL,
			"origin": "translated"})}),
		calls(toolCall{ToolQueriesTouching, `{"relation":"rv.rv_orders","column":"email"}`}),
		submit(func(body string) map[string]any {
			touching := aliasOfKind(t, body, "tool:queries_touching.")
			return map[string]any{
				"statements": []any{map[string]any{"evidence_id": aliasFor(t, body,
					inspectID(changereview.OriginModelTranslated, rvRenameSQL)),
					"origin": "translated", "file": railsMigration, "line": 3,
					"orm": "activerecord", "confidence": 0.95}},
				"findings": []any{
					map[string]any{"severity": "critical", "kind": "breaks_readers",
						"title": "billing-api still reads email",
						"text": fmt.Sprintf("queryid %d ran %d calls reading email.", q1.ID,
							q1.Calls), "evidence_ids": []string{touching}},
					map[string]any{"severity": "warning", "kind": "breaks_readers",
						"title": "Uncited", "text": "Trust me.", "evidence_ids": []string{}},
				}}
		}))
	got := e.submit(e.service(fake), railsRequest(rvRailsDiff))
	if got.ModelStatus != changereview.ModelOK || got.Verdict != changereview.VerdictReview {
		t.Fatalf("review %s/%s findings %+v dropped %+v", got.ModelStatus, got.Verdict,
			got.Findings, got.Dropped)
	}
	cited := findingTitled(got.Findings, "billing-api still reads email")
	if cited == nil || cited.Origin != changereview.OriginModel ||
		cited.Severity != changereview.SeverityWarning {
		t.Fatalf("cited finding = %+v", cited)
	}
	if len(got.Statements) != 1 || got.Statements[0].Origin != changereview.OriginModelTranslated ||
		got.Statements[0].Kind != "rename_column" || got.Statements[0].Line != 3 {
		t.Fatalf("statements = %+v", got.Statements)
	}
	var cr03 *changereview.Finding
	for i, x := range got.Findings {
		if strings.Contains(x.Title, "CR-03") {
			cr03 = &got.Findings[i]
		}
	}
	queryEv := fmt.Sprintf("queryid:%d", q1.ID)
	if cr03 == nil || cr03.Severity != changereview.SeverityWarning ||
		!containsID(cr03.EvidenceIDs, queryEv) {
		t.Fatalf("the translated rename's CR-03 = %+v, want a warning citing %s", cr03, queryEv)
	}
	if !hasRule(got.Findings, changereview.RuleModelTranslated) ||
		!strings.Contains(got.Markdown, "1 model claims or findings were dropped") {
		t.Fatalf("findings %+v markdown:\n%s", got.Findings, got.Markdown)
	}
	for _, ev := range got.Evidence {
		if !ev.Verify() {
			t.Fatalf("stored evidence %s does not verify", ev.ID)
		}
	}
}

var improvementRe = regexp.MustCompile(`\\"improvement_pct\\":([0-9.]+)`)

func TestLive_WhatIfOnTheExtractedQueryGroundsTheFinding(t *testing.T) {
	e := newLiveEnv(t)
	// 2000 distinct statuses, so status = $1 is selective and the composite
	// index beats rv_orders_created_idx for the extracted query.
	e.f.Exec(`UPDATE rv.rv_orders SET status = 's' || (id % 2000)`)
	e.f.Exec(`ANALYZE rv.rv_orders`)
	const query = "SELECT id, status, created_at FROM rv.rv_orders WHERE status = $1 " +
		"ORDER BY created_at LIMIT 50"
	const ddl = "CREATE INDEX CONCURRENTLY ON rv.rv_orders (status, created_at)"
	var pct string
	fake := newFakeLLM(t,
		calls(toolCall{ToolInspect, jsonArgs(map[string]any{"sql": query,
			"origin": "extracted"})}),
		callTools(func(body string) []toolCall {
			return []toolCall{{ToolWhatIf, jsonArgs(map[string]any{"ddl": ddl,
				"statement_evidence_ids": []string{aliasFor(t, body,
					inspectID(changereview.OriginModelExtracted, query))}})}}
		}),
		submit(func(body string) map[string]any {
			m := improvementRe.FindAllStringSubmatch(body, -1)
			if len(m) == 0 {
				t.Errorf("the what-if result shows no improvement_pct")
				return map[string]any{"findings": []any{}}
			}
			pct = m[len(m)-1][1]
			return map[string]any{
				"statements": []any{map[string]any{"evidence_id": aliasFor(t, body,
					inspectID(changereview.OriginModelExtracted, query)), "origin": "extracted",
					"file": controllerPath, "line": 12, "orm": "activerecord",
					"confidence": 0.8}},
				"findings": []any{map[string]any{"severity": "info", "kind": "index_benefit",
					"title": "The new index serves the new orders query",
					"text":         "HypoPG plans the query at " + pct + "% lower cost.",
					"evidence_ids": []string{aliasOfKind(t, body, "whatif:tool.")}}},
			}
		}))
	got := e.submit(e.service(fake), changereview.Request{MigrationTool: "flyway",
		SQL: "CREATE INDEX CONCURRENTLY rv_orders_status_created_idx " +
			"ON rv.rv_orders (status, created_at)",
		Diff: strings.ReplaceAll(controllerDiff, "Order.where", "RvOrder.where")})
	f := findingTitled(got.Findings, "The new index serves the new orders query")
	if f == nil || len(f.EvidenceIDs) != 1 {
		t.Fatalf("finding = %+v, dropped %+v", f, got.Dropped)
	}
	var data struct {
		ImprovementPct *json.Number `json:"improvement_pct"`
		Method         string       `json:"method"`
	}
	for _, ev := range got.Evidence {
		if ev.ID == f.EvidenceIDs[0] {
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if data.ImprovementPct == nil || data.Method != "hypopg" {
		t.Fatalf("what-if evidence %+v", data)
	}
	v, err := data.ImprovementPct.Float64()
	if err != nil || v <= 0 || data.ImprovementPct.String() != pct ||
		!strings.Contains(f.Text, data.ImprovementPct.String()+"%") {
		t.Fatalf("improvement %s (%v), finding text %q, model saw %s", data.ImprovementPct,
			err, f.Text, pct)
	}
	var stmt *changereview.Statement
	for i, s := range got.Statements {
		if s.Origin == changereview.OriginModelExtracted {
			stmt = &got.Statements[i]
		}
	}
	if stmt == nil || stmt.MatchedQueryID != nil || stmt.Fingerprint == "" ||
		stmt.EvidenceID != inspectID(changereview.OriginModelExtracted, query) ||
		!strings.Contains(got.Markdown, "new shape") {
		t.Fatalf("extracted statement %+v, markdown:\n%s", stmt, got.Markdown)
	}
}

func TestLive_UnparsableExtractionIsNotDetermined(t *testing.T) {
	e := newLiveEnv(t)
	const bad = "SELEC id FROM rv.rv_orders"
	fake := newFakeLLM(t,
		calls(toolCall{ToolInspect, jsonArgs(map[string]any{"sql": bad, "origin": "extracted"})}),
		submit(func(body string) map[string]any {
			return map[string]any{"findings": []any{}, "statements": []any{map[string]any{
				"evidence_id": aliasFor(t, body, inspectID(changereview.OriginModelExtracted,
					bad)), "origin": "extracted", "file": controllerPath, "line": 12}}}
		}))
	got := e.submit(e.service(fake), changereview.Request{Diff: controllerDiff,
		MigrationTool: "rails"})
	if len(got.Statements) != 0 {
		t.Fatalf("an unparsable statement was accepted: %+v", got.Statements)
	}
	found := false
	for _, n := range got.NotDetermined {
		if strings.Contains(n.Reason, "parse") {
			found = true
		}
	}
	if !found {
		t.Fatalf("not_determined %+v lacks the parse failure", got.NotDetermined)
	}
}

func findingTitled(fs []changereview.Finding, title string) *changereview.Finding {
	for i, f := range fs {
		if f.Title == title {
			return &fs[i]
		}
	}
	return nil
}

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
