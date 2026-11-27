package budget

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// market-08: the monitor turns each snapshot into ledger rows and
// findings. Fakes stand in for the ledger and the role catalog; the
// store_db_test covers their SQL on a live server.

type fakeSource struct{ snap *collector.Snapshot }

func (f fakeSource) LatestSnapshot() *collector.Snapshot { return f.snap }

type fakeLedger struct {
	recorded []Tally
	roles    []collector.WorkloadRoleStats
	history  map[string][]ShadowDay
	// definitions, when set, is the one definition per budget whose rows
	// history holds; another definition reads no rows.
	definitions map[string]string
	evidence    map[string]RoleEvidence
	err         error
}

func (l *fakeLedger) Record(_ context.Context, _ time.Time, ts []Tally) error {
	l.recorded = append(l.recorded, ts...)
	return l.err
}

func (l *fakeLedger) RecordRoles(_ context.Context, _ time.Time,
	rs []collector.WorkloadRoleStats) error {
	l.roles = append(l.roles, rs...)
	return l.err
}

func (l *fakeLedger) History(_ context.Context, budget, definition string,
	_ time.Time) ([]ShadowDay, error) {
	if want, ok := l.definitions[budget]; ok && want != definition {
		return nil, l.err
	}
	return l.history[budget], l.err
}

func (l *fakeLedger) RoleEvidence(context.Context, time.Time) (map[string]RoleEvidence,
	error) {
	return l.evidence, l.err
}

type fakeCatalog struct {
	self  string
	facts map[string]RoleFact
	err   error
}

func (c fakeCatalog) SelfRole(context.Context) (string, error) { return c.self, c.err }

// CurrentDatabase is the monitored database's own name (datname).
func (c fakeCatalog) CurrentDatabase(context.Context) (string, error) { return "shop", c.err }

func (c fakeCatalog) RoleFacts(_ context.Context, roles []string) (map[string]RoleFact,
	error) {
	out := map[string]RoleFact{}
	for _, r := range roles {
		if f, ok := c.facts[r]; ok {
			out[r] = f
		}
	}
	return out, c.err
}

func snapWith(long ...collector.WorkloadLongSession) *collector.Snapshot {
	return &collector.Snapshot{CollectedAt: t0, WorkloadSessions: &collector.WorkloadSessions{
		Groups: []collector.WorkloadGroup{}, Long: long,
		Roles: []collector.WorkloadRoleStats{{Role: "app", Calls: 500, MaxExecMs: 2500,
			P99EstimateMs: 2000, ClusterSessions: 8}}}}
}

func newTestMonitor(cfg *config.Config, snap *collector.Snapshot, l *fakeLedger,
	c fakeCatalog) *Monitor {
	m := NewMonitor(cfg, fakeSource{snap}, l, c, "shop", func(string, string, ...any) {})
	m.now = func() time.Time { return t0 }
	return m
}

func warnCfg() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Budgets = []config.BudgetConfig{{Name: "app-6s", Match: config.BudgetMatch{
		Role: "app"}, StatementTimeout: "6s"}}
	return cfg
}

func findingsOf(fs []analyzer.Finding, category string) []analyzer.Finding {
	var out []analyzer.Finding
	for _, f := range fs {
		if f.Category == category {
			out = append(out, f)
		}
	}
	return out
}

// Happy path, warn mode: a 7 s query records one would-cancel and raises
// one info violation finding with no SQL.
func TestMonitor_WarnModeRecordsAndRaisesInfo(t *testing.T) {
	l := &fakeLedger{history: map[string][]ShadowDay{"app-6s": {{Day: t0.Truncate(24 *
		time.Hour), FirstSeen: t0, WouldCancel: 1, WorstMs: 7000}}}}
	m := newTestMonitor(warnCfg(), snapWith(longActive(3, "app", "web", 7, nil)), l,
		fakeCatalog{self: "sage"})
	fs, err := m.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(l.recorded) != 1 || l.recorded[0].WouldCancel != 1 {
		t.Fatalf("recorded %+v, want one would-cancel", l.recorded)
	}
	v := findingsOf(fs, CategoryViolation)
	if len(v) != 1 || v[0].Severity != "info" || v[0].RecommendedSQL != "" ||
		v[0].ObjectIdentifier != "app-6s" || v[0].Detail["would_cancel_today"] != int64(1) {
		t.Fatalf("violation findings = %+v", v)
	}
	if got := findingsOf(fs, CategoryEnforce); len(got) != 0 {
		t.Fatalf("warn mode raised enforce actions: %+v", got)
	}
	cats := strings.Join(m.LastEvaluatedCategories(), ",")
	if !strings.Contains(cats, CategoryViolation) || !strings.Contains(cats, CategoryEnforce) {
		t.Fatalf("evaluated categories = %s", cats)
	}
}

// CHECK-15: seven days of warn-only shadow data with violations enforce
// nothing: no finding carries SQL.
func TestMonitor_WarnOnlyReplayEnforcesNothing(t *testing.T) {
	var days []ShadowDay
	for d := 7; d >= 0; d-- {
		day := t0.Add(-time.Duration(d) * 24 * time.Hour).Truncate(24 * time.Hour) // a date
		days = append(days, ShadowDay{Day: day, FirstSeen: day, WouldCancel: 4, WouldReject: 2})
	}
	cfg := warnCfg()
	cfg.Budgets[0].ConnectionLimit = 5
	l := &fakeLedger{history: map[string][]ShadowDay{"app-6s": days}}
	m := newTestMonitor(cfg, snapWith(longActive(3, "app", "web", 70, nil)), l,
		fakeCatalog{self: "sage", facts: map[string]RoleFact{"app": {}}})
	fs, err := m.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.RecommendedSQL != "" {
			t.Fatalf("warn-only budget produced an action: %+v", f)
		}
	}
	if len(findingsOf(fs, CategoryViolation)) != 1 {
		t.Fatalf("findings = %+v, want the violation", fs)
	}
}

func enforceCfg() *config.Config {
	cfg := warnCfg()
	cfg.Budgets[0].Mode = "enforce"
	return cfg
}

func sevenDays() map[string][]ShadowDay {
	return map[string][]ShadowDay{"app-6s": ledgerDays(8, -1)}
}

// State transition: enforce mode with 7 days of clean shadow data raises
// the role setting action; the rollback restores the prior value.
func TestMonitor_EnforceRaisesRoleTimeoutAction(t *testing.T) {
	l := &fakeLedger{history: sevenDays()}
	c := fakeCatalog{self: "sage", facts: map[string]RoleFact{"app": {
		Settings: map[string]string{"statement_timeout": "30s"}}}}
	m := newTestMonitor(enforceCfg(), snapWith(), l, c)
	fs, err := m.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e := findingsOf(fs, CategoryEnforce)
	if len(e) != 1 {
		t.Fatalf("enforce findings = %+v, want one", e)
	}
	want := `ALTER ROLE "app" IN DATABASE "shop" SET statement_timeout = '6000ms'`
	if e[0].RecommendedSQL != want || e[0].ActionRisk != "moderate" {
		t.Fatalf("action = %q (%s), want %q", e[0].RecommendedSQL, e[0].ActionRisk, want)
	}
	if e[0].RollbackSQL != `ALTER ROLE "app" IN DATABASE "shop" SET statement_timeout = '30s'` {
		t.Fatalf("rollback = %q, want the prior 30s restored", e[0].RollbackSQL)
	}
}

// State: once the setting is in place nothing is raised (the action
// finding resolves); a short window raises nothing either.
func TestMonitor_EnforceSettledOrTooEarly(t *testing.T) {
	c := fakeCatalog{self: "sage", facts: map[string]RoleFact{"app": {
		Settings: map[string]string{"statement_timeout": "6000ms"}}}}
	m := newTestMonitor(enforceCfg(), snapWith(), &fakeLedger{history: sevenDays()}, c)
	fs, _ := m.Detect(context.Background())
	if got := findingsOf(fs, CategoryEnforce); len(got) != 0 {
		t.Fatalf("settled budget raised %+v", got)
	}
	early := &fakeLedger{history: map[string][]ShadowDay{"app-6s": {{
		FirstSeen: t0.Add(-6*24*time.Hour - 23*time.Hour)}}}}
	m = newTestMonitor(enforceCfg(), snapWith(), early, fakeCatalog{self: "sage",
		facts: map[string]RoleFact{"app": {}}})
	fs, _ = m.Detect(context.Background())
	if got := findingsOf(fs, CategoryEnforce); len(got) != 0 {
		t.Fatalf("6d23h window raised %+v", got)
	}
}

// Invalid input: an enforce budget on a superuser or on pg_sage's role
// is refused; a connection limit below current sessions plus a margin
// is refused; a sound one is a HIGH-risk action with its rollback.
func TestMonitor_EnforceRefusals(t *testing.T) {
	for name, c := range map[string]fakeCatalog{
		"superuser": {self: "sage", facts: map[string]RoleFact{"app": {Superuser: true}}},
		"self":      {self: "app", facts: map[string]RoleFact{"app": {}}},
	} {
		m := newTestMonitor(enforceCfg(), snapWith(), &fakeLedger{history: sevenDays()}, c)
		fs, _ := m.Detect(context.Background())
		if got := findingsOf(fs, CategoryEnforce); len(got) != 0 {
			t.Errorf("%s: raised %+v", name, got)
		}
	}
	cfg := enforceCfg()
	cfg.Budgets[0].StatementTimeout = ""
	cfg.Budgets[0].ConnectionLimit = 9 // 8 sessions now: below 8 + margin 2
	c := fakeCatalog{self: "sage", facts: map[string]RoleFact{"app": {ConnLimit: -1}}}
	m := newTestMonitor(cfg, snapWith(), &fakeLedger{history: sevenDays()}, c)
	fs, _ := m.Detect(context.Background())
	if got := findingsOf(fs, CategoryEnforce); len(got) != 0 {
		t.Fatalf("limit 9 for 8 sessions raised %+v", got)
	}
	cfg.Budgets[0].ConnectionLimit = 10
	m = newTestMonitor(cfg, snapWith(), &fakeLedger{history: sevenDays()}, c)
	fs, _ = m.Detect(context.Background())
	got := findingsOf(fs, CategoryEnforce)
	if len(got) != 1 || got[0].RecommendedSQL != `ALTER ROLE "app" CONNECTION LIMIT 10` ||
		got[0].RollbackSQL != `ALTER ROLE "app" CONNECTION LIMIT -1` ||
		got[0].ActionRisk != "high_risk" {
		t.Fatalf("connection limit action = %+v", got)
	}
}

// Proposals: a role with p99 2 s and no budget gets a 6 s proposal; a
// budgeted, refused or thinly measured role gets none; the switch off
// gives none.
func TestMonitor_Proposals(t *testing.T) {
	l := &fakeLedger{evidence: map[string]RoleEvidence{
		"app":      {P99Ms: 2000, MaxMs: 2500, Calls: 500},
		"reports":  {P99Ms: 2000, MaxMs: 2500, Calls: 500},
		"postgres": {P99Ms: 10, MaxMs: 10, Calls: 900},
		"rare":     {P99Ms: 10, MaxMs: 10, Calls: 3},
	}}
	cfg := warnCfg()
	c := fakeCatalog{self: "sage", facts: map[string]RoleFact{"reports": {}, "rare": {}}}
	fs, _ := newTestMonitor(cfg, snapWith(), l, c).Detect(context.Background())
	p := findingsOf(fs, CategoryProposal)
	if len(p) != 1 || p[0].ObjectIdentifier != "reports" ||
		p[0].Detail["proposed_statement_timeout_ms"] != int64(6000) ||
		!strings.Contains(p[0].Recommendation, "statement_timeout: 6000ms") {
		t.Fatalf("proposals = %+v, want one 6 s proposal for reports", p)
	}
	cfg.Advisor.BudgetProposals = false
	fs, _ = newTestMonitor(cfg, snapWith(), l, c).Detect(context.Background())
	if got := findingsOf(fs, CategoryProposal); len(got) != 0 {
		t.Fatalf("proposals off raised %+v", got)
	}
}

// Nil/empty: no snapshot or an unknown category evaluates nothing (open
// findings stay open); no budgets still records role evidence.
func TestMonitor_NoReading(t *testing.T) {
	m := newTestMonitor(warnCfg(), nil, &fakeLedger{}, fakeCatalog{})
	fs, err := m.Detect(context.Background())
	if err != nil || len(fs) != 0 || len(m.LastEvaluatedCategories()) != 0 {
		t.Fatalf("nil snapshot: %v %+v %v", err, fs, m.LastEvaluatedCategories())
	}
	snap := snapWith()
	snap.WorkloadSessions = nil
	snap.Unavailable = map[string]string{"workload_sessions": "timeout"}
	m = newTestMonitor(warnCfg(), snap, &fakeLedger{}, fakeCatalog{})
	if fs, _ := m.Detect(context.Background()); len(fs) != 0 ||
		len(m.LastEvaluatedCategories()) != 0 {
		t.Fatalf("unknown category: %+v %v", fs, m.LastEvaluatedCategories())
	}
	cfg := config.DefaultConfig()
	l := &fakeLedger{}
	if _, err := newTestMonitor(cfg, snapWith(), l, fakeCatalog{}).Detect(
		context.Background()); err != nil || len(l.recorded) != 0 || len(l.roles) != 1 {
		t.Fatalf("no budgets: err %v recorded %+v roles %+v", err, l.recorded, l.roles)
	}
}

// Error propagation: a ledger failure is returned with context and no
// finding is raised from a half-written cycle.
func TestMonitor_LedgerErrorPropagates(t *testing.T) {
	l := &fakeLedger{err: errors.New("relation \"sage.budget_shadow\" does not exist")}
	m := newTestMonitor(warnCfg(), snapWith(longActive(3, "app", "", 9, nil)), l,
		fakeCatalog{self: "sage"})
	fs, err := m.Detect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "budget_shadow") ||
		!strings.Contains(err.Error(), "record") {
		t.Fatalf("err = %v, want the ledger error with context", err)
	}
	if len(fs) != 0 {
		t.Fatalf("findings after a failed cycle: %+v", fs)
	}
}

// State: a budget whose limits changed (60 s in warn mode, now 6 s in
// enforce mode) starts a new ledger: the looser budget's 7 days never
// promote the tighter one.
func TestMonitor_ChangedBudgetStartsANewLedger(t *testing.T) {
	loose := roleBudget("app-6s", "app", 60000).Definition()
	c := fakeCatalog{self: "sage", facts: map[string]RoleFact{"app": {}}}
	l := &fakeLedger{history: sevenDays(), definitions: map[string]string{"app-6s": loose}}
	fs, err := newTestMonitor(enforceCfg(), snapWith(), l, c).Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := findingsOf(fs, CategoryEnforce); len(got) != 0 {
		t.Fatalf("the 60 s budget's ledger promoted the 6 s budget: %+v", got)
	}
	tight := roleBudget("app-6s", "app", 6000).Definition()
	if len(l.recorded) != 1 || l.recorded[0].Definition != tight {
		t.Fatalf("recorded %+v, want the 6 s definition", l.recorded)
	}
	l = &fakeLedger{history: sevenDays(), definitions: map[string]string{"app-6s": tight}}
	fs, _ = newTestMonitor(enforceCfg(), snapWith(), l, c).Detect(context.Background())
	if got := findingsOf(fs, CategoryEnforce); len(got) != 1 {
		t.Fatalf("its own ledger: enforce findings = %+v, want one", got)
	}
}
