package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Roadmap 1.2 wiring: installing a database's ledger grandfathers the
// autonomy its time ramp already granted (once), hands the ledger the
// ramp as its promotion floor, and from then on the ledger decides every
// self-initiated class. Startup explains the new meaning of the trust
// settings; demotions reach a human through the notification rules.

func analyzeCustodianProposal() executor.CustodianProposal {
	return executor.CustodianProposal{Feature: "analyze",
		SQL: `ANALYZE "public"."orders"`, TargetObjects: []string{"public.orders"},
		ObservedAt: time.Now()}
}

func uniqueDatabase(prefix string) string {
	return prefix + "-" + time.Now().UTC().Format("150405.000000000")
}

func TestInstallGrandfathersTheRampAutonomy(t *testing.T) {
	pool := autonomyPool(t)
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	ledgers := newAutonomyLedgers(true)
	ex := autonomousExecutor(pool) // ramp started 60 days ago, tier3_safe on
	db := uniqueDatabase("gf")
	binding := autonomyBinding{database: db, control: pool, monitored: pool,
		settings: config.DefaultConfig().SRE.Autonomy}
	if err := ledgers.install(context.Background(), ex, binding); err != nil {
		t.Fatal(err)
	}
	ex.EnableStandingPolicyDocument(doc, nil)
	got := ex.EvaluateCustodianProposal(context.Background(), analyzeCustodianProposal())
	if got.Decision != executor.PolicyDecisionExecute ||
		got.BlockedReason != string(policy.ReasonAutonomyL3) {
		t.Fatalf("grandfathered analyze = %+v", got)
	}
	entry, ok := ledgers.registry.Lookup(db)
	if !ok {
		t.Fatal("not registered")
	}
	st, err := entry.Service.Granted(context.Background(), earned.FamilyHygiene,
		earned.ClassAnalyze)
	if err != nil || st.Level != earned.L3 || st.Provenance != earned.ProvenanceGrandfathered {
		t.Fatalf("hygiene/analyze = %+v (%v)", st, err)
	}
	// MODERATE classes were not autonomous (tier3_moderate off): L1.
	st, _ = entry.Service.Granted(context.Background(), earned.FamilyTuning,
		earned.ClassIndexCreate)
	if st.Level != earned.L1 || st.Stored {
		t.Fatalf("tuning/index_create = %+v, want the L1 default", st)
	}
	rec, err := entry.Service.Grandfathered(context.Background())
	if err != nil || rec == nil || rec.Database != db {
		t.Fatalf("grandfather record = %+v (%v)", rec, err)
	}
}

// A database whose ramp has not elapsed is not grandfathered, and the
// ramp elapsing later grants nothing: only earned evidence promotes.
func TestInstallWithAFreshRampLeavesTheLedgerAtL1(t *testing.T) {
	pool := autonomyPool(t)
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	cfg := config.DefaultConfig()
	cfg.Trust.Level, cfg.Trust.Tier3Safe = "autonomous", true
	ex := executor.New(pool, cfg, time.Now(), logStructuredWrapper)
	ex.SetExecutionMode("auto")
	ex.WithEmergencyStopCheck(func(context.Context) bool { return false })
	ledgers := newAutonomyLedgers(true)
	if err := ledgers.install(context.Background(), ex, autonomyBinding{
		database: uniqueDatabase("fresh"), control: pool, monitored: pool,
		settings: config.DefaultConfig().SRE.Autonomy}); err != nil {
		t.Fatal(err)
	}
	ex.EnableStandingPolicyDocument(doc, nil)
	got := ex.EvaluateCustodianProposal(context.Background(), analyzeCustodianProposal())
	if got.Decision != executor.PolicyDecisionObserveOnly ||
		got.BlockedReason != string(policy.ReasonAutonomyLevel) {
		t.Fatalf("fresh-ramp analyze = %+v, want an L1 manual script", got)
	}
}

func TestInstallHandsTheLedgerTheRampFloor(t *testing.T) {
	pool := autonomyPool(t)
	cfg := config.DefaultConfig()
	cfg.Trust.RampSafeHours, cfg.Trust.RampModerateHours = 2, 5
	start := time.Now().Add(-3 * time.Hour).UTC()
	ex := executor.New(pool, cfg, start, logStructuredWrapper)
	ledgers := newAutonomyLedgers(true)
	db := uniqueDatabase("floor")
	binding := autonomyBinding{database: db, control: pool, monitored: pool,
		settings: config.DefaultConfig().SRE.Autonomy}
	if err := ledgers.install(context.Background(), ex, binding); err != nil {
		t.Fatal(err)
	}
	entry, _ := ledgers.registry.Lookup(db)
	v, err := entry.Service.TrustView(context.Background())
	if err != nil || v.Floor == nil || !v.Floor.Known || !v.Floor.Start.Equal(start) ||
		v.Floor.RequiredL2 != 2*time.Hour || v.Floor.RequiredL3 != 5*time.Hour {
		t.Fatalf("floor = %+v (%v)", v.Floor, err)
	}
}

func TestAutonomyNotifierTellsAHumanAboutADemotion(t *testing.T) {
	d := &capturedDispatch{}
	n := autonomyNotifier{dispatcher: d}
	err := n.NotifyDemotion(context.Background(), earned.Demotion{Database: "orders",
		Family: earned.FamilyHygiene, Class: earned.ClassIndexDrop, From: earned.L3,
		To: earned.L2, Cause: earned.CauseRegressed, ActionLogID: 41,
		Detail: "action 41 regressed"})
	if err != nil || len(d.events) != 1 {
		t.Fatalf("notification = %+v (%v)", d.events, err)
	}
	e := d.events[0]
	if e.Type != "action_failed" || e.Severity != "warning" ||
		!strings.Contains(e.Subject, "index_drop") || !strings.Contains(e.Subject, "L3") ||
		!strings.Contains(e.Subject, "L2") || !strings.Contains(e.Body, "regressed") ||
		e.Data["database"] != "orders" || e.Data["cause"] != "regressed" ||
		e.DedupKey == "" {
		t.Fatalf("demotion event = %+v", e)
	}
	if err := (autonomyNotifier{}).NotifyDemotion(context.Background(),
		earned.Demotion{}); err != nil {
		t.Fatalf("no dispatcher must log, not fail: %v", err)
	}
}

type lineCapture struct{ lines []string }

func (c *lineCapture) log(component, format string, args ...any) {
	c.lines = append(c.lines, component+": "+fmt.Sprintf(format, args...))
}

func TestTrustStartupExplainsTheNewMeaning(t *testing.T) {
	var info, warn lineCapture
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "autonomous"
	cfg.Trust.RampSafeHours, cfg.Trust.RampModerateHours = 6, 12
	logTrustMeaning(cfg, info.log, warn.log)
	all := strings.Join(info.lines, "\n")
	for _, want := range []string{"trust.level=autonomous", "ceiling",
		"trust.ramp_safe_hours=6", "trust.ramp_moderate_hours=12", "floor",
		"grandfathered", "/api/v1/trust"} {
		if !strings.Contains(all, want) {
			t.Errorf("startup message lacks %q:\n%s", want, all)
		}
	}
	if len(warn.lines) != 0 {
		t.Fatalf("enforced ledger warned: %v", warn.lines)
	}
	info, warn = lineCapture{}, lineCapture{}
	cfg.SRE.Autonomy.Enforce = false
	logTrustMeaning(cfg, info.log, warn.log)
	if len(warn.lines) == 0 || !strings.Contains(strings.Join(warn.lines, "\n"),
		"sre.autonomy.enforce=false") {
		t.Fatalf("enforce=false is not a warning: info %v warn %v", info.lines, warn.lines)
	}
}

func TestGrandfatherReportIsLoggedOnce(t *testing.T) {
	var info lineCapture
	logGrandfathered(earned.GrandfatherReport{Database: "orders", Migrated: true,
		Seeded: []earned.State{{Family: earned.FamilyHygiene, Class: earned.ClassVacuum,
			Level: earned.L3}}}, info.log)
	if len(info.lines) != 1 || !strings.Contains(info.lines[0], "hygiene/vacuum=L3") ||
		!strings.Contains(info.lines[0], "orders") {
		t.Fatalf("grandfather log = %v", info.lines)
	}
	info = lineCapture{}
	logGrandfathered(earned.GrandfatherReport{Database: "orders"}, info.log)
	if len(info.lines) != 0 {
		t.Fatalf("an already migrated database logged again: %v", info.lines)
	}
}

func TestAutonomyServiceConfigMapsTheClassBar(t *testing.T) {
	s := config.DefaultConfig().SRE.Autonomy
	c := autonomyServiceConfig(s)
	if c.Thresholds.ClassMinSuccessesL2 != 3 || c.Thresholds.ClassMinSuccessesL3 != 10 ||
		c.Thresholds.ClassMinSuccessRate != 0.8 {
		t.Fatalf("default class bar = %+v", c.Thresholds)
	}
	s.ClassPromotion = config.SREClassPromotionConfig{MinSuccessesL2: 1,
		MinSuccessesL3: 2, MinSuccessRatePct: 60}
	c = autonomyServiceConfig(s)
	if c.Thresholds.ClassMinSuccessesL2 != 1 || c.Thresholds.ClassMinSuccessesL3 != 2 ||
		c.Thresholds.ClassMinSuccessRate != 0.6 {
		t.Fatalf("lowered class bar = %+v", c.Thresholds)
	}
}

// MCP keeps its contract and gains the unified grid (additive).
func TestAutonomyMCPGetCarriesTheTrustGrid(t *testing.T) {
	pool := autonomyPool(t)
	ledgers := newAutonomyLedgers(true)
	db := uniqueDatabase("mcp")
	svc, err := ledgers.ledgerFor(context.Background(), pool, db,
		config.DefaultConfig().SRE.Autonomy)
	if err != nil {
		t.Fatal(err)
	}
	ledgers.registry.Register(db, earned.RegistryEntry{Service: svc,
		Limiter: svc.Limiter(earned.Binding{Database: db})})
	got, err := autonomyMCPBackend{registry: ledgers.registry}.GetAutonomy(
		context.Background(), mcp.AutonomyRequest{Database: db})
	if err != nil {
		t.Fatal(err)
	}
	body := got.(map[string]any)
	trust, ok := body["trust"].(earned.TrustView)
	if !ok || body["view"] == nil || trust.Database != db || len(trust.Rows) == 0 {
		t.Fatalf("get_autonomy = %+v", body)
	}
	for _, r := range trust.Rows {
		if r.Effective == nil {
			t.Fatalf("row %s/%s not annotated", r.Family, r.Class)
		}
	}
}
