package main

import (
	"context"
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

func TestLedgerForSharesOneLedgerPerControlPool(t *testing.T) {
	pool := autonomyPool(t)
	other := autonomyPool(t)
	ledgers := newAutonomyLedgers(true)
	settings := config.DefaultConfig().SRE.Autonomy
	a, err := ledgers.ledgerFor(context.Background(), pool, settings)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ledgers.ledgerFor(context.Background(), pool, settings)
	if err != nil || a != b {
		t.Fatalf("same control pool built two ledgers (%v)", err)
	}
	c, err := ledgers.ledgerFor(context.Background(), other, settings)
	if err != nil || c == a {
		t.Fatalf("a different control pool shared the ledger (%v)", err)
	}
	if _, err := ledgers.ledgerFor(context.Background(), nil, settings); err == nil {
		t.Fatal("a nil control pool built a ledger")
	}
}

func freezeCustodianProposal() executor.CustodianProposal {
	return executor.CustodianProposal{Feature: "freeze",
		SQL: `VACUUM (FREEZE) "public"."orders"`, TargetObjects: []string{"public.orders"},
		ObservedAt: time.Now()}
}

func autonomousExecutor(pool *pgxpool.Pool) *executor.Executor {
	cfg := config.DefaultConfig()
	cfg.Trust.Level, cfg.Trust.Tier3Safe = "autonomous", true
	ex := executor.New(pool, cfg, time.Now().Add(-60*24*time.Hour), logStructuredWrapper)
	ex.SetExecutionMode("auto")
	ex.WithEmergencyStopCheck(func(context.Context) bool { return false })
	return ex
}

func TestInstallAutonomyRestrictsCustodiansByDefault(t *testing.T) {
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
	if got.Decision != executor.PolicyDecisionObserveOnly ||
		got.BlockedReason != string(policy.ReasonAutonomyLevel) {
		t.Fatalf("freeze at the default L1 = %+v", got)
	}
	if _, ok := ledgers.registry.Lookup("orders"); !ok {
		t.Fatal("the database was not registered for the API")
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
		freezeCustodianProposal()); got.Decision != executor.PolicyDecisionExecute {
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
	ledger, err := ledgers.ledgerFor(context.Background(), pool,
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
	n, err := ingestBenchPath(context.Background(), ledger, dir)
	if err != nil || n != 1 {
		t.Fatalf("first ingest = %d (%v)", n, err)
	}
	if n, err = ingestBenchPath(context.Background(), ledger, dir); err != nil || n != 0 {
		t.Fatalf("second ingest = %d (%v), want nothing new", n, err)
	}
	if n, err = ingestBenchPath(context.Background(), ledger,
		filepath.Join(dir, "pgincidentbench.json")); err != nil || n != 0 {
		t.Fatalf("file path ingest = %d (%v)", n, err)
	}
	if _, err := ingestBenchPath(context.Background(), ledger,
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
