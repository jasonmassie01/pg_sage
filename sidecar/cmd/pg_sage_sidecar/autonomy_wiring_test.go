package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Sage SRE M7 wiring: every database's executor consults the earned-
// autonomy ledger of its control database (one ledger per control pool,
// shared in fleet mode with a meta database); enforcement can only be
// turned off explicitly; custodian proposals carry the time their
// evidence was sampled; bench reports dropped in a path are ingested
// once; game days are off unless a disposable target is configured.

func autonomyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err := schema.Bootstrap(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAutonomyServiceConfigMapsSettings(t *testing.T) {
	s := config.DefaultConfig().SRE.Autonomy
	s.MaxEvidenceAgeSeconds, s.ConcurrencyWindowMinutes = 90, 5
	c := autonomyServiceConfig(s)
	if c.MaxEvidenceAge != 90*time.Second || c.ConcurrencyWindow != 5*time.Minute ||
		c.SafetyWindow != 30*24*time.Hour || c.FailoverCooldown != 30*time.Minute ||
		c.ProposalTTL != 168*time.Hour || c.Thresholds != earned.DefaultThresholds() {
		t.Fatalf("service config = %+v", c)
	}
}

// P0-5: one ledger per control pool AND database. Databases of one
// control database share its deployment (and its bench evidence) but
// never a ledger.
func TestLedgerForBuildsOneLedgerPerControlPoolAndDatabase(t *testing.T) {
	pool := autonomyPool(t)
	other := autonomyPool(t)
	ledgers := newAutonomyLedgers(true)
	settings := config.DefaultConfig().SRE.Autonomy
	a, err := ledgers.ledgerFor(context.Background(), pool, "orders", settings)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ledgers.ledgerFor(context.Background(), pool, "orders", settings)
	if err != nil || a != b {
		t.Fatalf("same control pool and database built two ledgers (%v)", err)
	}
	billing, err := ledgers.ledgerFor(context.Background(), pool, "billing", settings)
	if err != nil || billing == a || billing.Database() != "billing" ||
		billing.Store().DeploymentID() != a.Store().DeploymentID() {
		t.Fatalf("billing ledger = %v (%v): its own ledger in the same deployment",
			billing, err)
	}
	c, err := ledgers.ledgerFor(context.Background(), other, "orders", settings)
	if err != nil || c == a {
		t.Fatalf("a different control pool shared the ledger (%v)", err)
	}
	if _, err := ledgers.ledgerFor(context.Background(), nil, "orders", settings); err == nil {
		t.Fatal("a nil control pool built a ledger")
	}
	if _, err := ledgers.ledgerFor(context.Background(), pool, " ", settings); err == nil {
		t.Fatal("a ledger without a database was built")
	}
}

// Installing a database adopts the deployment's legacy (pre-scope)
// levels for that database once, before carry-over is seeded.
func TestInstallAdoptsLegacyLevelsForTheDatabase(t *testing.T) {
	pool := autonomyPool(t)
	ctx := context.Background()
	deployment, err := earned.EnsureDeployment(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	db := "legacy_" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_family_autonomy
		(deployment_id, database_name, family, action_class, level, changed_by,
		 change_reason) VALUES ($1, '', 'lock_blocking', 'backend_terminate', 0,
		 'user:1:a@e', 'legacy restriction')
		ON CONFLICT DO NOTHING`, deployment); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.sre_family_autonomy
			WHERE deployment_id = $1 AND database_name = ''`, deployment)
	})
	ledgers := newAutonomyLedgers(true)
	if err := ledgers.install(ctx, autonomousExecutor(pool), autonomyBinding{database: db,
		control: pool, monitored: pool,
		settings: config.DefaultConfig().SRE.Autonomy}); err != nil {
		t.Fatal(err)
	}
	entry, ok := ledgers.registry.Lookup(db)
	if !ok {
		t.Fatal("not registered")
	}
	st, err := entry.Service.Granted(ctx, earned.FamilyLockBlocking,
		earned.ClassBackendTerminate)
	if err != nil || st.Level != earned.L0 || !st.Stored {
		t.Fatalf("adopted legacy restriction = %+v (%v), want a stored L0", st, err)
	}
}

// freezeCustodianProposal names its own table: these proposals are
// evaluated and never run, so each execute verdict holds its table (one
// change per object) and must not hold the next test's.
func freezeCustodianProposal() executor.CustodianProposal {
	table := fixtureTable("orders")
	return executor.CustodianProposal{Feature: "freeze",
		SQL: `VACUUM (FREEZE) public.` + table, TargetObjects: []string{"public." + table},
		ObservedAt: time.Now()}
}

// fixtureTable is a table name no other test uses.
func fixtureTable(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

func autonomousExecutor(pool *pgxpool.Pool) *executor.Executor {
	cfg := config.DefaultConfig()
	cfg.Trust.Level, cfg.Trust.Tier3Safe = "autonomous", true
	ex := executor.New(pool, cfg, time.Now().Add(-60*24*time.Hour), logStructuredWrapper)
	ex.SetExecutionMode("auto")
	ex.WithEmergencyStopCheck(func(context.Context) bool { return false })
	return ex
}

// Coordinator decision 2026-10-02: installing the ledger carries over
// the autonomy today's configuration grants (here: an autonomous safe
// tier, so the custodian freeze), and the ledger then governs it: the
// freeze runs as an L3 ledger decision, not as a bare trust-ramp one.
func TestInstallAutonomyCarriesOverAndGovernsCustodians(t *testing.T) {
	pool := autonomyPool(t)
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}

	ledgers := newAutonomyLedgers(true)
	ex := autonomousExecutor(pool)
	if err := ledgers.install(context.Background(), ex, autonomyBinding{database: "orders",
		control: pool, monitored: pool, settings: config.DefaultConfig().SRE.Autonomy}); err != nil {
		t.Fatal(err)
	}
	ex.EnableStandingPolicyDocument(doc, nil)
	got := ex.EvaluateCustodianProposal(context.Background(), freezeCustodianProposal())
	if got.Decision != executor.PolicyDecisionExecute ||
		got.BlockedReason != string(policy.ReasonAutonomyL3) {
		t.Fatalf("carried-over freeze = %+v", got)
	}
	entry, ok := ledgers.registry.Lookup("orders")
	if !ok {
		t.Fatal("the database was not registered for the API")
	}
	st, err := entry.Service.Granted(context.Background(), earned.FamilyWraparound,
		earned.ClassFreeze)
	if err != nil || st.Level != earned.L3 || st.Provenance != earned.ProvenanceCarriedOver {
		t.Fatalf("freeze ledger state = %+v (%v)", st, err)
	}

	off := newAutonomyLedgers(false)
	legacy := autonomousExecutor(pool)
	settings := config.DefaultConfig().SRE.Autonomy
	settings.Enforce = false
	if err := off.install(context.Background(), legacy, autonomyBinding{database: "orders",
		control: pool, monitored: pool, settings: settings}); err != nil {
		t.Fatal(err)
	}
	legacy.EnableStandingPolicyDocument(doc, nil)
	if got := legacy.EvaluateCustodianProposal(context.Background(),
		freezeCustodianProposal()); got.Decision != executor.PolicyDecisionExecute ||
		got.BlockedReason == string(policy.ReasonAutonomyL3) {
		t.Fatalf("with enforcement off the trust ramp decides: %+v", got)
	}
}

// When the ledger cannot be built, family actions fail closed rather
// than fall back to the trust ramp.
func TestInstallAutonomyFailsClosedWithoutALedger(t *testing.T) {
	pool := autonomyPool(t)
	ledgers := newAutonomyLedgers(true)
	ex := autonomousExecutor(pool)
	err := ledgers.install(context.Background(), ex, autonomyBinding{database: "orders",
		control: nil, monitored: pool, settings: config.DefaultConfig().SRE.Autonomy})
	if err == nil {
		t.Fatal("install without a control pool reported success")
	}
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	ex.EnableStandingPolicyDocument(doc, nil)
	got := ex.EvaluateCustodianProposal(context.Background(), freezeCustodianProposal())
	if got.Decision != executor.PolicyDecisionBlocked ||
		got.BlockedReason != string(policy.ReasonAutonomyUnavailable) {
		t.Fatalf("freeze without a ledger = %+v", got)
	}
}

func TestRouteStampsTheEvidenceTime(t *testing.T) {
	gate := &runtimePolicyRecorder{}
	exec := executor.New(nil, config.DefaultConfig(), time.Now(), nil)
	exec.WithPolicyGate(gate)
	router := executorProposalRouter{executor: exec}
	before := time.Now()
	_ = router.Route(context.Background(), autonomy.Proposal{Database: "orders",
		Feature: "freeze", SQL: `VACUUM (FREEZE) "public"."orders"`,
		TargetObjects: []string{"public.orders"}})
	at := gate.request.EvidenceObservedAt
	if at.Before(before) || at.After(time.Now()) ||
		gate.request.IncidentFamily != "wraparound_runway" {
		t.Fatalf("routed request: observed %v family %q", at, gate.request.IncidentFamily)
	}
}

func TestIngestBenchPathIngestsEachReportOnce(t *testing.T) {
	pool := autonomyPool(t)
	ledgers := newAutonomyLedgers(true)
	ledger, err := ledgers.ledgerFor(context.Background(), pool, "orders",
		config.DefaultConfig().SRE.Autonomy)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	report := `{"schema":"pg_sage.pgincidentbench.v1","generated_at":"` +
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339) + `","gated_arms":` +
		`["causal-graph"],"cells":[{"arm":"causal-graph","family":"wal_retention",` +
		`"runs":1,"safe_pass":{"k":1,"n":1},"top1":{"k":1,"n":1},` +
		`"mechanism_precision":1,"forbidden_actions":0}]}`
	if err := os.WriteFile(filepath.Join(dir, "pgincidentbench.json"), []byte(report),
		0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := operatorIngest(context.Background(), ledger, dir)
	if err != nil || n != 1 {
		t.Fatalf("first ingest = %d (%v)", n, err)
	}
	if n, err = operatorIngest(context.Background(), ledger, dir); err != nil || n != 0 {
		t.Fatalf("second ingest = %d (%v), want nothing new", n, err)
	}
	if n, err = operatorIngest(context.Background(), ledger,
		filepath.Join(dir, "pgincidentbench.json")); err != nil || n != 0 {
		t.Fatalf("file path ingest = %d (%v)", n, err)
	}
	if _, err := operatorIngest(context.Background(), ledger,
		filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing path was not reported")
	}
}

func TestGameDayProviderSelection(t *testing.T) {
	settings := config.DefaultConfig().SRE.Autonomy.GameDays
	cloneNone := config.CloneProviderConfig{Provider: "none"}
	monitored := []string{"postgres://app@db.prod:5432/orders"}
	if p, name, err := gameDayProvider(settings, cloneNone, monitored); err != nil ||
		p != nil || name != "" {
		t.Fatalf("disabled game days built a provider: %v %q %v", p, name, err)
	}
	settings.Enabled = true
	if p, _, err := gameDayProvider(settings, cloneNone, monitored); err != nil || p != nil {
		t.Fatalf("enabled without a target built a provider: %v %v", p, err)
	}
	settings.LocalDSN = "postgres://u@127.0.0.1:5999/scratch"
	if p, name, err := gameDayProvider(settings, cloneNone, monitored); err != nil ||
		p == nil || name != "local" {
		t.Fatalf("local fallback = %v %q %v", p, name, err)
	}
	settings.LocalDSN = monitored[0]
	if _, _, err := gameDayProvider(settings, cloneNone, monitored); err == nil {
		t.Fatal("a monitored database was accepted as the game-day target")
	}
	settings.LocalDSN = ""
	dle := config.CloneProviderConfig{Provider: "dle", DLEEndpoint: "https://dle.internal",
		DLEToken: "t"}
	if p, name, err := gameDayProvider(settings, dle, monitored); err != nil || p == nil ||
		name != "dle" {
		t.Fatalf("DLE provider = %v %q %v", p, name, err)
	}
}

type capturedDispatch struct {
	mu     sync.Mutex
	events []notify.Event
}

func (d *capturedDispatch) Dispatch(_ context.Context, e notify.Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, e)
	return nil
}

func TestAutonomyNotifierTellsAHumanAboutL3(t *testing.T) {
	d := &capturedDispatch{}
	n := autonomyNotifier{dispatcher: d}
	err := n.NotifyAutonomous(context.Background(), earned.AutoExecution{
		Database: "orders", ActionLogID: 9, Family: earned.FamilyWraparound,
		Class: earned.ClassFreeze, SQL: `VACUUM (FREEZE) "public"."orders"`})
	if err != nil || len(d.events) != 1 || d.events[0].Type != "action_executed" ||
		!strings.Contains(d.events[0].Subject, "L3") ||
		d.events[0].Data["database"] != "orders" {
		t.Fatalf("notification = %+v (%v)", d.events, err)
	}
	if err := (autonomyNotifier{}).NotifyAutonomous(context.Background(),
		earned.AutoExecution{}); err != nil {
		t.Fatalf("no dispatcher must log, not fail: %v", err)
	}
}
