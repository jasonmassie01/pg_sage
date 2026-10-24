package reasoner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/impact"
	"github.com/pg-sage/sidecar/internal/config"
)

// Fakes for the reasoner's unit tests: a scripted impact engine (the
// closed tools' backend) and an in-memory daily budget. The live tests
// (live_*_test.go) use the real engine on schema rv and the real budget.

func inspectRef(origin, sql string) string {
	sum := sha256.Sum256([]byte(origin + "\n" + sql))
	return hex.EncodeToString(sum[:6])
}

func inspectID(origin, sql string) string { return "inspect:" + inspectRef(origin, sql) }

func mustEvidence(kind, ref, label string, data any) changereview.Evidence {
	e, err := changereview.NewEvidence(kind, ref, label, data)
	if err != nil {
		panic(err)
	}
	return e
}

type fakeTools struct {
	mu        sync.Mutex
	parseFail map[string]string // sql -> parser message
	planFail  map[string]string // sql -> plan error
	matched   map[string]int64  // sql -> matching queryid
	hits      map[string][]changereview.Finding
	hitEv     map[string][]changereview.Evidence
	whatIfPct *float64
	whatIfErr error
	touchText string // queries_touching's statement text ("": the default)
	err       error  // every tool fails with it

	inspected []string
	origins   []string
	touching  []string
	contexts  []string
	whatIfs   [][]string
	ddls      []string
}

func newFakeTools() *fakeTools {
	return &fakeTools{parseFail: map[string]string{}, planFail: map[string]string{},
		matched: map[string]int64{}, hits: map[string][]changereview.Finding{},
		hitEv: map[string][]changereview.Evidence{}}
}

func (f *fakeTools) InspectStatement(_ context.Context, sql, origin string) (
	changereview.Inspection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspected, f.origins = append(f.inspected, sql), append(f.origins, origin)
	if f.err != nil {
		return changereview.Inspection{}, f.err
	}
	if strings.TrimSpace(sql) == "" || strings.Count(sql, ";") > 1 {
		return changereview.Inspection{}, fmt.Errorf("%w: inspect one statement",
			changereview.ErrInvalid)
	}
	ref := inspectRef(origin, sql)
	kind := strings.ToLower(strings.Fields(sql)[0])
	ev := mustEvidence("inspect", ref, kind+" statement", map[string]any{"sql": sql,
		"kind": kind, "index": 1})
	st := changereview.Statement{Index: 1, Origin: origin, SQL: sql, Kind: kind,
		Relations: []string{"public.orders"}, EvidenceID: ev.ID, Fingerprint: "fp" + ref}
	if msg, bad := f.parseFail[sql]; bad {
		st.Kind = "unparsed"
		return changereview.Inspection{Statement: st, ParseError: msg,
			Evidence: []changereview.Evidence{ev}}, nil
	}
	if id, ok := f.matched[sql]; ok {
		st.MatchedQueryID = &id
	}
	ins := changereview.Inspection{Statement: st, Parsed: true, Planned: true,
		Evidence: append([]changereview.Evidence{ev}, f.hitEv[sql]...),
		Findings: f.hits[sql]}
	if msg, bad := f.planFail[sql]; bad {
		ins.Planned, ins.PlanError = false, msg
	} else if kind == "select" {
		ins.Statement.PlanSummary = "Index Scan using orders_status_idx on orders"
	}
	return ins, nil
}

func (f *fakeTools) QueriesTouching(_ context.Context, relation, column string) (
	impact.Touching, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touching = append(f.touching, relation+"."+column)
	if f.err != nil {
		return impact.Touching{}, f.err
	}
	if relation == "missing" {
		return impact.Touching{}, fmt.Errorf("%w: relation public.missing",
			changereview.ErrNotFound)
	}
	text := "SELECT email FROM orders WHERE id = $1"
	if f.touchText != "" {
		text = f.touchText
	}
	q := mustEvidence("queryid", "42", "queryid 42: 412000 calls", map[string]any{
		"queryid": 42, "calls": 412000, "calls_7d": 2800000, "peak_calls_per_s": 31.5,
		"text": text,
		"tags": map[string]string{"application": "billing-api"}})
	return impact.Touching{Relation: "public.orders", Known: true,
		Evidence: []changereview.Evidence{q}}, nil
}

func (f *fakeTools) TableContext(_ context.Context, relation string) (
	[]changereview.Evidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contexts = append(f.contexts, relation)
	if f.err != nil {
		return nil, f.err
	}
	name := relation
	if !strings.Contains(name, ".") {
		name = "public." + name
	}
	return []changereview.Evidence{mustEvidence("table", name, name+": 200000 rows",
		map[string]any{"relation": name, "reltuples": 200000, "total_bytes": 52428800})}, nil
}

func (f *fakeTools) WhatIfIndex(_ context.Context, ddl string, statements []string) (
	changereview.Evidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ddls, f.whatIfs = append(f.ddls, ddl), append(f.whatIfs, statements)
	switch {
	case f.err != nil:
		return changereview.Evidence{}, f.err
	case f.whatIfErr != nil:
		return changereview.Evidence{}, f.whatIfErr
	case !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(ddl)), "CREATE INDEX"):
		return changereview.Evidence{}, fmt.Errorf("%w: ddl must be one CREATE INDEX",
			changereview.ErrInvalid)
	}
	sum := sha256.Sum256([]byte(ddl + "\x00" + strings.Join(statements, "\x00")))
	data := map[string]any{"ddl": ddl, "method": "hypopg", "measured": len(statements)}
	label := "Index benefit not measured"
	if f.whatIfPct != nil {
		data["improvement_pct"] = *f.whatIfPct
		label = fmt.Sprintf("HypoPG: %.1f%% lower planned cost for %d statements",
			*f.whatIfPct, len(statements))
	}
	return mustEvidence("whatif", "tool."+hex.EncodeToString(sum[:6]), label, data), nil
}

func (f *fakeTools) inspections() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inspected...)
}

// memBudget is the daily budget in memory: a database cap shared by every
// actor.
type memBudget struct {
	mu       sync.Mutex
	limit    int64
	used     int64
	reserves int
	settled  []int64
	err      error // a store failure on Reserve
}

func (b *memBudget) Reserve(_ context.Context, _ string, tokens int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.reserves++
	if b.used+tokens > b.limit {
		return &changereview.BudgetError{Scope: changereview.BudgetScopeDatabase,
			Used: b.used, Limit: b.limit, Need: tokens}
	}
	b.used += tokens
	return nil
}

func (b *memBudget) Settle(_ context.Context, _ string, reserved, used int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used += used - reserved
	b.settled = append(b.settled, used)
	return nil
}

func (b *memBudget) total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

var errStore = errors.New("budget store unreachable")

// Fixture inputs.

const railsMigration = "db/migrate/20261023120000_rename_email.rb"

// railsRenameDiff is an ORM-only migration renaming orders.email; the
// rename is line 3.
const railsRenameDiff = "diff --git a/" + railsMigration + " b/" + railsMigration + "\n" +
	"new file mode 100644\n--- /dev/null\n+++ b/" + railsMigration + "\n" +
	"@@ -0,0 +1,5 @@\n" +
	"+class RenameEmail < ActiveRecord::Migration[7.1]\n" +
	"+  def change\n" +
	"+    rename_column :orders, :email, :email_address\n" +
	"+  end\n" +
	"+end\n"

const controllerPath = "app/controllers/orders_controller.rb"

// controllerDiff adds Order.where(status:).order(:created_at).limit(50) at
// line 12 of the orders controller.
const controllerDiff = "diff --git a/" + controllerPath + " b/" + controllerPath + "\n" +
	"--- a/" + controllerPath + "\n+++ b/" + controllerPath + "\n" +
	"@@ -10,3 +10,5 @@ class OrdersController\n" +
	"   def index\n" +
	"     status = params[:status]\n" +
	"+    @orders = Order.where(status:).order(:created_at).limit(50)\n" +
	"+    render json: @orders\n" +
	"   end\n"

const renameSQL = "ALTER TABLE orders RENAME COLUMN email TO email_address"

const extractedSQL = "SELECT * FROM orders WHERE status = $1 ORDER BY created_at LIMIT 50"

func testInput(diff, tool string) changereview.Input {
	return changereview.Input{ReviewID: "00000000-0000-0000-0000-0000000000a1",
		Database: "orders", Actor: "user:7", Diff: diff, MigrationTool: tool,
		PR: changereview.PRInfo{URL: "https://github.com/acme/shop/pull/12", Number: 12,
			Title: "Rename email", Repo: "acme/shop"},
		Config: config.DefaultChangeReviewConfig()}
}

// safeAnalysis is a deterministic analysis with one statement and no
// finding (verdict safe).
func safeAnalysis() changereview.Analysis {
	st := mustEvidence("stmt", "1", "statement 1: add_column", map[string]any{"index": 1,
		"kind": "add_column", "sql": "ALTER TABLE orders ADD COLUMN note text"})
	return changereview.Analysis{ParseMode: changereview.ParseAST,
		Statements: []changereview.Statement{{Index: 1, Origin: changereview.OriginSQLInput,
			SQL: "ALTER TABLE orders ADD COLUMN note text", Kind: "add_column",
			EvidenceID: st.ID}},
		Evidence: []changereview.Evidence{st},
		Expectations: changereview.Expectations{Relations: []string{"public.orders"},
			PostConditions: []changereview.PostCondition{}, WatchQueryIDs: []int64{}}}
}

// riskyAnalysis adds a CR-03 critical finding citing the statement and a
// workload statement.
func riskyAnalysis() changereview.Analysis {
	a := safeAnalysis()
	q := mustEvidence("queryid", "42", "queryid 42: 412000 calls", map[string]any{
		"queryid": 42, "calls": 412000})
	a.Evidence = append(a.Evidence, q)
	a.Findings = []changereview.Finding{{RuleID: "CR-03", Kind: "column_still_referenced",
		Severity: changereview.SeverityCritical, Origin: changereview.OriginRule,
		Title: "1 statement still reads orders.email", Text: "queryid 42 (412000 calls)",
		EvidenceIDs: []string{"stmt:1", "queryid:42"}}}
	return a
}

// noSQLAnalysis is the analysis of a review without SQL.
func noSQLAnalysis() changereview.Analysis {
	return changereview.Analysis{ParseMode: changereview.ParseAST,
		Expectations: changereview.Expectations{Relations: []string{},
			PostConditions: []changereview.PostCondition{}, WatchQueryIDs: []int64{}}}
}

func testBounds() Bounds {
	b := DefaultBounds()
	b.Wall, b.StepTimeout = 20*time.Second, 5*time.Second
	return b
}

func newTestReasoner(t *testing.T, d Deps) *Reasoner {
	t.Helper()
	if d.Tools == nil {
		d.Tools = newFakeTools()
	}
	if d.Model != nil && d.Budget == nil {
		d.Budget = &memBudget{limit: 10_000_000}
	}
	if d.Bounds == (Bounds{}) {
		d.Bounds = testBounds()
	}
	d.Logf = t.Logf
	r, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func reason(t *testing.T, r *Reasoner, in changereview.Input,
	a changereview.Analysis) changereview.Reasoning {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := r.Reason(ctx, in, a)
	if err != nil {
		t.Fatalf("Reason: %v", err)
	}
	return res
}
