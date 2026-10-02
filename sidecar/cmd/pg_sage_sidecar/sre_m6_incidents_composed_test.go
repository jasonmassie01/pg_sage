package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// growingTemp is the real probe runner with temp-file growth added to
// this database's cumulative temp bytes (2 GiB per sample), so the
// detector sees an explosion on real PostgreSQL rows.
type growingTemp struct {
	real sre.ProbeRunner
	mu   sync.Mutex
	n    float64
}

func (g *growingTemp) Run(ctx context.Context, id probes.ID, args probes.Args) probes.Result {
	res := g.real.Run(ctx, id, args)
	if id != probes.TempFileActivity || res.Status != probes.StatusOK || len(res.Rows) != 1 {
		return res
	}
	g.mu.Lock()
	g.n++
	grown := g.n * (2 << 30)
	g.mu.Unlock()
	row := probes.Row{}
	for k, v := range res.Rows[0] {
		row[k] = v
	}
	base, _ := row["temp_bytes"].(float64)
	if v, ok := row["temp_bytes"].(int64); ok {
		base = float64(v)
	}
	row["temp_bytes"] = base + grown
	res.Rows = []probes.Row{row}
	return res
}

// Sage SRE M6 follow-up exit path: a temp-file explosion seen by the
// reactive detector opens a sage.incidents row through the RCA engine,
// notifies through the incident notification path, links the
// investigation to the incident's case (the Cases panel's identity) and
// is reviewable as shadow evidence of its family (M7).
func TestComposedSRE_DetectorEpisodeOpensAnIncidentCaseAndInvestigation(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	pool := openComposedPool(t, ctx, dsn)
	db := fmt.Sprintf("m6inc_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE database_name = $1", db)
	})
	c := composedConfig("standalone")
	eng := rca.NewEngine(&c.RCA, func(string, string, ...any) {})
	eng.WithDatabaseName(db)
	events := &eventLog{}
	eng.WithDispatcher(events)
	sink := newEpisodeIncidents(eng, pool, func(string, string, ...any) {})
	settings := c.SRE
	settings.AutomaticStart = true
	settings.TriggerIntervalSeconds, settings.SampleIntervalSeconds = 1, 1
	runner := &growingTemp{real: probes.NewRunner(pool, probes.Catalog(),
		probes.NewLimiter(1))}
	svc, err := newSREInvestigator(sreInvestigatorDeps{control: pool, monitored: pool,
		runner: runner, name: db, runtimeKey: "composed:" + db, settings: settings,
		logFn: func(string, string, ...any) {}, episodes: sink})
	if err != nil {
		t.Fatalf("investigator: %v", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sink.Run(runCtx) }()
	go func() { defer wg.Done(); svc.Coordinator().Run(runCtx) }()
	t.Cleanup(func() { stop(); wg.Wait() })

	inv := waitKind(t, ctx, svc, sre.TriggerTempFiles)
	if inv.IncidentID == "" {
		t.Fatalf("investigation = %+v, want it linked to an incident", inv)
	}
	row := waitIncidentRow(t, ctx, pool, inv.IncidentID)
	if row.severity != "warning" || len(row.signals) != 1 ||
		row.signals[0] != "sre_temp_file_explosion" {
		t.Fatalf("incident row = %+v", row)
	}
	projected := cases.ProjectIncident(cases.SourceIncident{ID: inv.IncidentID,
		DatabaseName: db, SignalIDs: row.signals, Source: row.source})
	if inv.CaseID != projected.ID {
		t.Fatalf("investigation case %q, Cases panel case %q", inv.CaseID, projected.ID)
	}
	page, err := svc.List(ctx, sre.ListFilter{CaseID: projected.ID})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != inv.ID {
		t.Fatalf("investigations of the case = %+v (%v), want exactly %s", page.Items, err,
			inv.ID)
	}
	for deadline := time.Now().Add(10 * time.Second); len(events.ofType(
		"incident_detected")) == 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if got := events.ofType("incident_detected"); len(got) != 1 ||
		got[0].Data["incident_id"] != inv.IncidentID {
		t.Fatalf("incident_detected = %+v, want one for %s", got, inv.IncidentID)
	}
	reviewDetectorInvestigation(t, ctx, pool, db, inv)
}

// reviewDetectorInvestigation records the operator's verdict the way the
// autonomy API does and checks it counts for the family.
func reviewDetectorInvestigation(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	db string, inv sre.Investigation) {
	t.Helper()
	family := earned.Family(inv.TriggerKind)
	if inv.Summary.Family != "" {
		family = earned.Family(inv.Summary.Family)
	}
	if family != earned.FamilyTempFiles {
		t.Fatalf("investigation family = %s, want temp_file_explosion", family)
	}
	deployment, err := earned.EnsureDeployment(ctx, pool)
	if err != nil {
		t.Fatalf("deployment: %v", err)
	}
	store, err := earned.NewPostgresStore(pool, deployment)
	if err != nil {
		t.Fatalf("ledger store: %v", err)
	}
	svc, err := earned.NewService(store, earned.DefaultConfig())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	since := time.Now().Add(-time.Hour)
	before, err := store.ShadowStats(ctx, family, since)
	if err != nil {
		t.Fatalf("shadow before: %v", err)
	}
	if err := svc.RecordReview(ctx, earned.Review{Database: db,
		InvestigationID: string(inv.ID), Family: family, Verdict: earned.VerdictAccepted,
		Reviewer: "user:1:ops@example.com"}); err != nil {
		t.Fatalf("review: %v", err)
	}
	after, err := store.ShadowStats(ctx, family, since)
	if err != nil || after.Reviewed != before.Reviewed+1 ||
		after.Accepted != before.Accepted+1 {
		t.Fatalf("shadow record %+v -> %+v (%v), want one more accepted review", before,
			after, err)
	}
}
