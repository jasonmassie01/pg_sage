package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
)

// Roadmap 2.4 owner addition A wiring: a family earning or losing
// model-root authority reaches a human through the database's
// notification rules (and the log) with the report that decided it, and
// the hourly bench loop finds the changes (a newer report, or the
// deciding report aging out).

func TestAutonomyNotifierTellsAHumanAboutRootAuthority(t *testing.T) {
	d := &capturedDispatch{}
	n := autonomyNotifier{dispatcher: d}
	at := time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)
	grant := earned.RootAuthorityChange{Database: "orders", Family: earned.FamilyLockBlocking,
		Granted: true, ReportID: "run-7", Reason: "override precision 16/16", At: at}
	if err := n.NotifyRootAuthority(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	revoke := grant
	revoke.Granted, revoke.ReportID, revoke.Reason = false, "run-8", "lower bound 0.11 < 0.80"
	if err := n.NotifyRootAuthority(context.Background(), revoke); err != nil {
		t.Fatal(err)
	}
	if len(d.events) != 2 {
		t.Fatalf("events = %+v", d.events)
	}
	g, r := d.events[0], d.events[1]
	if g.Type != "action_executed" || g.Severity != "warning" ||
		!strings.Contains(g.Subject, "lock_blocking") || !strings.Contains(g.Subject, "orders") ||
		!strings.Contains(g.Body, "run-7") || !strings.Contains(g.Body, "16/16") ||
		g.Data["report_id"] != "run-7" || g.Data["granted"] != true ||
		g.Data["family"] != "lock_blocking" || !strings.Contains(g.DedupKey, "run-7") {
		t.Fatalf("grant event = %+v", g)
	}
	if r.Type != "action_failed" || r.Severity != "warning" ||
		!strings.Contains(r.Subject, "advisory") || !strings.Contains(r.Body, "run-8") ||
		r.Data["granted"] != false || r.DedupKey == g.DedupKey {
		t.Fatalf("revoke event = %+v", r)
	}
	if err := (autonomyNotifier{}).NotifyRootAuthority(context.Background(),
		grant); err != nil {
		t.Fatalf("no dispatcher must log, not fail: %v", err)
	}
}

func TestBenchIngesterRecordsAndTellsARootAuthorityChange(t *testing.T) {
	pool := autonomyPool(t)
	reg := ledgerWithLift(t, pool, 16, 16)
	entry, _ := reg.Lookup("orders")
	d := &capturedDispatch{}
	entry.Service.WithRootAuthorityNotifier(autonomyNotifier{dispatcher: d})
	tick := benchIngester(entry.Service, config.SREAutonomyConfig{})
	tick(context.Background())
	tick(context.Background())
	if len(d.events) != 1 || d.events[0].Type != "action_executed" ||
		d.events[0].Data["family"] != "lock_blocking" || d.events[0].Data["report_id"] == "" {
		t.Fatalf("told = %+v, want one grant", d.events)
	}
	hist, err := entry.Service.History(context.Background(), earned.EventFilter{
		Family: earned.FamilyLockBlocking, Class: earned.ModelRootClass})
	if err != nil || len(hist) != 1 || hist[0].Type != earned.EventRootAuthorityGranted {
		t.Fatalf("history = %+v (%v)", hist, err)
	}
}

// The ledger installed for a database tells its operator: a grant found
// when an investigation asks is told, not only one the hourly loop finds.
func TestInstallGivesTheLedgerTheNotifyPath(t *testing.T) {
	pool := autonomyPool(t)
	ledgers := newAutonomyLedgers(true)
	db := uniqueDatabase("root")
	d := &capturedDispatch{}
	binding := autonomyBinding{database: db, control: pool, monitored: pool,
		settings: config.DefaultConfig().SRE.Autonomy,
		notifier: autonomyNotifier{dispatcher: d}}
	if err := ledgers.install(context.Background(), autonomousExecutor(pool),
		binding); err != nil {
		t.Fatal(err)
	}
	entry, ok := ledgers.registry.Lookup(db)
	if !ok {
		t.Fatal("not registered")
	}
	if _, err := entry.Service.IngestEvalRun(context.Background(), liftJSON(
		time.Now().Add(-time.Hour), "lock_blocking", 16, 16), earned.SourceBench, "admin",
		""); err != nil {
		t.Fatal(err)
	}
	a := registryRootAuthority{registry: ledgers.registry, database: db}
	got, err := a.ModelRootAuthority(context.Background(), "lock_blocking")
	if err != nil || !got.Granted || len(d.events) != 1 ||
		d.events[0].Data["database"] != db {
		t.Fatalf("authority = %+v (%v), told %+v", got, err, d.events)
	}
}
