package impact

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/rvfixture"
)

// Live fixtures: every test gets its own copy of schema rv
// (testsupport/rvfixture) with the workload Q1..Q3 run, its query_store
// samples recorded and ANALYZE done. Nothing here reaches an LLM.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/changereview/impact"))
}

type env struct {
	t   *testing.T
	ctx context.Context
	f   *rvfixture.Fixture
	eng *Engine
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := rvfixture.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	e := &env{t: t, ctx: ctx, f: f}
	e.eng = e.engine(f.Pool, nil)
	return e
}

// testTimeouts bound the engine's catalog reads in tests. The shipped
// 500 ms bound (catalogread.Default) is too tight for PostgreSQL 14's
// statistics collector on a loaded CI host: a pg_stat_all_indexes read
// timed out there and turned a CR-07 into CR-17. What a read that hits
// its bound does is TestBlockedCatalogReadIsTimedOutEvidence's subject.
var testTimeouts = catalogread.Timeouts{Statement: 10 * time.Second, Lock: 5 * time.Second}

// engine is an engine over pool with the default configuration; mod
// adjusts its dependencies first.
func (e *env) engine(pool *pgxpool.Pool, mod func(*Deps)) *Engine {
	e.t.Helper()
	d := Deps{Pool: pool, Config: config.DefaultChangeReviewConfig(),
		Timeouts: testTimeouts, Logf: e.t.Logf}
	if mod != nil {
		mod(&d)
	}
	eng, err := New(d)
	if err != nil {
		e.t.Fatalf("New: %v", err)
	}
	return eng
}

func input(sql string) changereview.Input {
	return changereview.Input{ReviewID: "00000000-0000-0000-0000-000000000001",
		Database: "rv", Actor: "test", Config: config.DefaultChangeReviewConfig(),
		SQLFiles:          []changereview.SQLFile{{Content: sql}},
		TransactionalFrom: changereview.TransactionalFromTool}
}

func (e *env) analyze(sql string) changereview.Analysis {
	e.t.Helper()
	return e.analyzeWith(e.eng, input(sql))
}

func (e *env) analyzeWith(eng *Engine, in changereview.Input) changereview.Analysis {
	e.t.Helper()
	a, err := eng.Analyze(e.ctx, in)
	if err != nil {
		e.t.Fatalf("Analyze: %v", err)
	}
	checkAnalysis(e.t, a)
	return a
}

// checkAnalysis asserts what the runner requires of every analysis and
// that every cited evidence id is held and verifies.
func checkAnalysis(t *testing.T, a changereview.Analysis) {
	t.Helper()
	held := map[string]bool{}
	for _, ev := range a.Evidence {
		if held[ev.ID] {
			t.Errorf("evidence %s is listed twice", ev.ID)
		}
		held[ev.ID] = true
		if !changereview.ValidEvidenceID(ev.ID) || !ev.Verify() {
			t.Errorf("evidence %s is invalid or its digest does not verify", ev.ID)
		}
	}
	for _, f := range a.Findings {
		if f.Origin != changereview.OriginRule || ruleNames[f.RuleID] != f.Kind {
			t.Errorf("finding %+v is not a named rule finding", f)
		}
		if len(f.EvidenceIDs) == 0 {
			t.Errorf("finding %s %q cites no evidence", f.RuleID, f.Title)
		}
		for _, id := range f.EvidenceIDs {
			if !held[id] {
				t.Errorf("finding %s cites %s, which the analysis does not hold",
					f.RuleID, id)
			}
		}
	}
	if len(a.Expectations.WatchQueryIDs) > changereview.MaxWatchQueryIDs {
		t.Errorf("watch set of %d queryids", len(a.Expectations.WatchQueryIDs))
	}
}

func byRule(a changereview.Analysis, rule string) []changereview.Finding {
	var out []changereview.Finding
	for _, f := range a.Findings {
		if f.RuleID == rule {
			out = append(out, f)
		}
	}
	return out
}

// one is the single finding of rule; it fails the test otherwise.
func one(t *testing.T, a changereview.Analysis, rule string) changereview.Finding {
	t.Helper()
	fs := byRule(a, rule)
	if len(fs) != 1 {
		t.Fatalf("want exactly one %s finding, got %d: %s", rule, len(fs), describe(a))
	}
	return fs[0]
}

func none(t *testing.T, a changereview.Analysis, rules ...string) {
	t.Helper()
	for _, r := range rules {
		if fs := byRule(a, r); len(fs) > 0 {
			t.Errorf("want no %s finding, got %+v", r, fs)
		}
	}
}

func evidence(t *testing.T, a changereview.Analysis, id string) changereview.Evidence {
	t.Helper()
	for _, ev := range a.Evidence {
		if ev.ID == id {
			return ev
		}
	}
	t.Fatalf("evidence %s not found: %s", id, describe(a))
	return changereview.Evidence{}
}

// citedOfKind is the evidence of kind f cites, in citation order.
func citedOfKind(t *testing.T, a changereview.Analysis, f changereview.Finding,
	kind string) []changereview.Evidence {
	t.Helper()
	var out []changereview.Evidence
	for _, id := range f.EvidenceIDs {
		if strings.HasPrefix(id, kind+":") {
			out = append(out, evidence(t, a, id))
		}
	}
	return out
}

func decode[T any](t *testing.T, ev changereview.Evidence) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(ev.Data, &v); err != nil {
		t.Fatalf("decode evidence %s: %v (%s)", ev.ID, err, ev.Data)
	}
	return v
}

func verdict(a changereview.Analysis) string {
	return changereview.VerdictOf(a.Findings, a.ParseMode)
}

func cites(f changereview.Finding, id string) bool { return slices.Contains(f.EvidenceIDs, id) }

func describe(a changereview.Analysis) string {
	var b strings.Builder
	for _, f := range a.Findings {
		b.WriteString("\n  " + f.RuleID + " " + f.Severity + " " + f.Title + " " +
			strings.Join(f.EvidenceIDs, ","))
	}
	for _, nd := range a.NotDetermined {
		b.WriteString("\n  not determined: " + nd.Subject + ": " + nd.Reason)
	}
	return b.String()
}

func near(got, want, share float64) bool {
	if want == 0 {
		return got == 0
	}
	d := (got - want) / want
	return d <= share && d >= -share
}

// queryIDsOf is the queryids of the queryid evidence a finding cites.
func queryIDsOf(t *testing.T, a changereview.Analysis, f changereview.Finding) []int64 {
	t.Helper()
	var ids []int64
	for _, ev := range citedOfKind(t, a, f, "queryid") {
		ids = append(ids, decode[queryData](t, ev).QueryID)
	}
	slices.Sort(ids)
	return ids
}
