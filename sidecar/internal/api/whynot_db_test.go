package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// ux-08a on a real server: the chain's policy, budget, privilege and stop
// links come from a real standing gate explaining real findings.

const whyNotIndexSQL = "DROP INDEX CONCURRENTLY public.whynot_idx"

// whyNotSetup is "prod" on the test database, observed a minute ago, with
// an executor in the given mode and the standing policy doc (nil: the
// unattended profile from the database).
type whyNotSetup struct {
	pool *pgxpool.Pool
	ctx  context.Context
	mgr  *fleet.DatabaseManager
	exec *executor.Executor
	mux  *http.ServeMux
}

func newWhyNotSetup(t *testing.T, mode string, status fleet.InstanceStatus,
	doc *policy.Document) whyNotSetup {
	t.Helper()
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	cfg := caseAutonomousConfig()
	cfg.Collector.IntervalSeconds = 60
	cfg.LLM.Enabled = true
	exec := executor.New(pool, cfg, time.Now().Add(-40*24*time.Hour),
		func(string, string, ...any) {})
	exec.SetExecutionMode(mode)
	if doc != nil {
		exec.EnableStandingPolicyDocument(*doc, nil)
	} else if err := exec.EnableStandingPolicy(ctx, "unattended", nil); err != nil {
		t.Fatalf("enable standing policy: %v", err)
	}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "prod",
		Config: config.DatabaseConfig{Name: "prod", ExecutionMode: mode},
		Pool:   pool, Executor: exec, Status: &status})
	return whyNotSetup{pool: pool, ctx: ctx, mgr: mgr, exec: exec, mux: decisionsMux(mgr)}
}

func observedNow() fleet.InstanceStatus {
	return fleet.InstanceStatus{Connected: true, CollectorLastRun: time.Now()}
}

func (s whyNotSetup) seedIndexFinding(t *testing.T) int64 {
	t.Helper()
	for _, sql := range []string{
		"DROP TABLE IF EXISTS public.whynot_t",
		"CREATE TABLE public.whynot_t (a int)",
		"CREATE INDEX whynot_idx ON public.whynot_t (a)",
	} {
		if _, err := s.pool.Exec(s.ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS public.whynot_t")
	})
	seedFinding(t, s.ctx, "unused_index", "index", "public.whynot_idx", whyNotIndexSQL)
	return findingIDFor(t, s.ctx, s.pool, "public.whynot_idx")
}

func rowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// Happy path: an unused-index finding under an approval-mode executor:
// observed, finding and candidate pass; the policy link fails with
// "asks first" and links the card; nothing is recorded.
func TestWhyNotApprovalRequiredLinksTheCard(t *testing.T) {
	s := newWhyNotSetup(t, "approval", observedNow(), nil)
	id := s.seedIndexFinding(t)
	before := rowCount(t, s.ctx, s.pool, "sage.decision")
	code, body, raw := getWhyNot(t, s.mux, "database=prod&finding_id="+strconv.FormatInt(id, 10))
	if code != http.StatusOK || len(body.Links) != 10 {
		t.Fatalf("whynot: %d %s", code, raw)
	}
	for _, name := range []string{"observed", "finding", "candidate"} {
		if link := body.link(t, name); link.Status != "pass" {
			t.Fatalf("%s = %+v, want pass (%s)", name, link, raw)
		}
	}
	policyLink := body.link(t, "policy")
	card := "#/cases?finding=" + strconv.FormatInt(id, 10) + "&database=prod"
	if body.FirstFail != "policy" || policyLink.Status != "fail" ||
		!strings.Contains(policyLink.Sentence, "Asks first: approval required") ||
		policyLink.Fix["kind"] != "ui_link" || policyLink.Fix["value"] != card {
		t.Fatalf("policy = %+v, first %s (%s)", policyLink, body.FirstFail, raw)
	}
	if body.link(t, "privilege").Status != "pass" || body.link(t, "emergency_stop").Status !=
		"pass" || body.link(t, "trust").Status != "pass" {
		t.Fatalf("gate facts = %s", raw)
	}
	if strings.Contains(raw, "DROP INDEX") {
		t.Fatalf("whynot served the SQL text: %s", raw)
	}
	if after := rowCount(t, s.ctx, s.pool, "sage.decision"); after != before {
		t.Fatalf("sage.decision %d -> %d: a why-not read recorded a decision", before, after)
	}
}

// Invalid input on a real database: a finding of another database is 404
// (each database has its own findings), an unknown id 404, and a database
// with no findings answers for the whole database.
func TestWhyNotFindingOfAnotherDatabase(t *testing.T) {
	s := newWhyNotSetup(t, "approval", observedNow(), nil)
	id := strconv.FormatInt(s.seedIndexFinding(t), 10)
	otherPool, err := pgxpool.New(s.ctx, testdb.CreateDatabase(t, "whynot_other"))
	if err != nil {
		t.Fatalf("other pool: %v", err)
	}
	t.Cleanup(otherPool.Close)
	// Every fleet database carries its own sage schema and findings.
	if err := schema.Bootstrap(s.ctx, otherPool); err != nil {
		t.Fatalf("bootstrap other: %v", err)
	}
	otherStatus := observedNow()
	s.mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "other",
		Config: config.DatabaseConfig{Name: "other"}, Pool: otherPool, Status: &otherStatus})
	if code, _, raw := getWhyNot(t, s.mux, "database=other&finding_id="+id); code !=
		http.StatusNotFound {
		t.Fatalf("another database's finding: %d %s, want 404", code, raw)
	}
	if code, _, raw := getWhyNot(t, s.mux, "database=prod&finding_id=99999999"); code !=
		http.StatusNotFound {
		t.Fatalf("unknown finding: %d %s, want 404", code, raw)
	}
	code, body, raw := getWhyNot(t, s.mux, "database=other")
	if code != http.StatusOK || body.link(t, "finding").Status != "fail" ||
		body.link(t, "candidate").Status != "not_applicable" {
		t.Fatalf("database-wide on an empty database: %d %s", code, raw)
	}
	code, body, raw = getWhyNot(t, s.mux, "database=prod&object=public.whynot_idx")
	if code != http.StatusOK || body.link(t, "finding").Status != "pass" ||
		body.Object != "public.whynot_idx" {
		t.Fatalf("by object: %d %s", code, raw)
	}
}

// Nil/empty: no snapshot yet fails the first link with its due time; no
// findings of an LLM category with LLM off says so with the setting.
func TestWhyNotNotObservedAndLLMOff(t *testing.T) {
	s := newWhyNotSetup(t, "approval", fleet.InstanceStatus{Connected: true}, nil)
	s.mgr.Config().LLM.Enabled = false
	code, body, raw := getWhyNot(t, s.mux, "database=prod&category=missing_index")
	if code != http.StatusOK {
		t.Fatalf("whynot: %d %s", code, raw)
	}
	observed := body.link(t, "observed")
	if observed.Status != "fail" ||
		!strings.Contains(observed.Sentence, "Not observed yet; first snapshot due at ") ||
		observed.Fix["kind"] != "wait_until" || body.FirstFail != "observed" {
		t.Fatalf("observed = %+v (%s)", observed, raw)
	}
	finding := body.link(t, "finding")
	if finding.Status != "fail" || !strings.Contains(finding.Sentence, "LLM is off") ||
		finding.Fix["kind"] != "setting" || finding.Fix["value"] != "llm.enabled=true" ||
		body.Category != "missing_index" {
		t.Fatalf("finding = %+v (%s)", finding, raw)
	}
}

// Error propagation: when the explain path errors, the policy link is
// unknown with the error class only, later links still run, and the
// answer is 200.
func TestWhyNotExplainErrorIsAPartialAnswer(t *testing.T) {
	s := newWhyNotSetup(t, "approval", observedNow(), nil)
	id := strconv.FormatInt(s.seedIndexFinding(t), 10)
	previous := explainWhyNotCandidates
	explainWhyNotCandidates = func(context.Context, []cases.Case,
		*fleet.DatabaseInstance) ([]explainedCandidate, error) {
		return nil, errors.New("dial tcp 10.1.2.3:5432: connection refused")
	}
	t.Cleanup(func() { explainWhyNotCandidates = previous })
	code, body, raw := getWhyNot(t, s.mux, "database=prod&finding_id="+id)
	if code != http.StatusOK {
		t.Fatalf("whynot: %d %s, want 200 with partial results", code, raw)
	}
	policyLink := body.link(t, "policy")
	if policyLink.Status != "unknown" || policyLink.Error != "error" ||
		strings.Contains(raw, "10.1.2.3") {
		t.Fatalf("policy = %+v (%s)", policyLink, raw)
	}
	if body.link(t, "privilege").Status != "pass" ||
		body.link(t, "emergency_stop").Status != "pass" {
		t.Fatalf("later links did not run: %s", raw)
	}
}

// Boundary: a stopped fleet is marked first even with the approval
// failure before it, and the approval failure is still listed.
func TestWhyNotStoppedFleetIsMarkedFirst(t *testing.T) {
	s := newWhyNotSetup(t, "approval", observedNow(), nil)
	id := strconv.FormatInt(s.seedIndexFinding(t), 10)
	if _, err := s.mgr.EmergencyStopStrict("", "alice"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	t.Cleanup(func() { _, _ = s.mgr.ResumeStrict("", "alice") })
	code, body, raw := getWhyNot(t, s.mux, "database=prod&finding_id="+id)
	stop := body.link(t, "emergency_stop")
	if code != http.StatusOK || body.FirstFail != "emergency_stop" || !stop.FirstFail ||
		stop.Status != "fail" || len(body.Failing) < 2 || body.Failing[0] != "emergency_stop" {
		t.Fatalf("stopped fleet: %d %s", code, raw)
	}
	if stop.Fix["kind"] != "ui_link" {
		t.Fatalf("stop fix = %+v", stop.Fix)
	}
}

// A replica: the gate refuses the change and the privilege link says so,
// with the reason there is no fix.
func TestWhyNotReplica(t *testing.T) {
	status := observedNow()
	status.Capabilities.IsReplica = true
	s := newWhyNotSetup(t, "auto", status, nil)
	id := strconv.FormatInt(s.seedIndexFinding(t), 10)
	code, body, raw := getWhyNot(t, s.mux, "database=prod&finding_id="+id)
	privilege := body.link(t, "privilege")
	if code != http.StatusOK || privilege.Status != "fail" ||
		privilege.Reason != "replica_mutation" || privilege.NoFix == "" {
		t.Fatalf("replica: %d %+v (%s)", code, privilege, raw)
	}
}

// A spent budget: a performance budget of no tables parks an autovacuum
// setting, and the budget link fails with the gate's reason and a fix.
func TestWhyNotSpentBudget(t *testing.T) {
	doc := policy.UnattendedProfile()
	doc.ApprovalRequiredClasses = nil
	doc.BlastRadius.MaxTablesPerWindow = 0
	s := newWhyNotSetup(t, "auto", observedNow(), &doc)
	s.seedIndexFinding(t)
	seedFinding(t, s.ctx, "vacuum_tuning", "table", "public.whynot_t",
		"ALTER TABLE public.whynot_t SET (autovacuum_vacuum_scale_factor = 0.05)")
	id := strconv.FormatInt(findingIDFor(t, s.ctx, s.pool, "public.whynot_t"), 10)
	code, body, raw := getWhyNot(t, s.mux, "database=prod&finding_id="+id)
	budget := body.link(t, "budget")
	if code != http.StatusOK || budget.Status != "fail" || budget.Fix == nil ||
		budget.Reason != "blast_radius_exceeded" || body.FirstFail != "budget" {
		t.Fatalf("spent budget: %d %+v (%s)", code, budget, raw)
	}
}

// Concurrent access (CHECK-03): 100 chain reads, 20 at a time, write no
// decision and no action row, so they take no budget slot.
func TestWhyNotReadsTakeNoBudgetSlot(t *testing.T) {
	s := newWhyNotSetup(t, "auto", observedNow(), nil)
	id := strconv.FormatInt(s.seedIndexFinding(t), 10)
	decisions := rowCount(t, s.ctx, s.pool, "sage.decision")
	actions := rowCount(t, s.ctx, s.pool, "sage.action_log")
	var wg sync.WaitGroup
	codes := make(chan int, 100)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				code, _, _ := getWhyNot(t, s.mux, "database=prod&finding_id="+id)
				codes <- code
			}
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("parallel read status %d", code)
		}
	}
	if got := rowCount(t, s.ctx, s.pool, "sage.decision"); got != decisions {
		t.Fatalf("sage.decision %d -> %d", decisions, got)
	}
	if got := rowCount(t, s.ctx, s.pool, "sage.action_log"); got != actions {
		t.Fatalf("sage.action_log %d -> %d", actions, got)
	}
}
