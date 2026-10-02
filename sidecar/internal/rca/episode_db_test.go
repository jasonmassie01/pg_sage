package rca

import (
	"encoding/json"
	"testing"
	"time"
)

// A detector episode's incident goes through the engine's own
// persistence and notification path: one sage.incidents row with the
// detector's signal, severity and measured evidence, one
// incident_detected notification however many polls refresh it, and the
// operator's resolution in the database is never overwritten.
func TestObserveEpisode_PersistsAndNotifiesOnce(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	rec := &recordingDispatcher{}
	eng.WithDispatcher(rec)
	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	ep := checkpointEpisode(0)
	ep.ObservedAt = time.Now().Add(-time.Minute)
	inc, ok := eng.ObserveEpisode(ctx, ep)
	if !ok {
		t.Fatal("episode not recorded")
	}
	for i := 0; i < 3; i++ {
		if err := eng.PersistIncidents(ctx, pool); err != nil {
			t.Fatalf("persist: %v", err)
		}
		next := ep
		next.IncidentID, next.ObservedAt = inc.ID, time.Now()
		if _, ok := eng.ObserveEpisode(ctx, next); !ok {
			t.Fatalf("refresh %d failed", i)
		}
	}
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("persist: %v", err)
	}
	var severity, source, rootCause string
	var signals []string
	var chain []byte
	var occurrences int
	if err := pool.QueryRow(ctx, `SELECT severity, source, root_cause, signal_ids,
		causal_chain, occurrence_count FROM sage.incidents
		WHERE id = $1 AND database_name = $2 AND resolved_at IS NULL`, inc.ID, db).
		Scan(&severity, &source, &rootCause, &signals, &chain, &occurrences); err != nil {
		t.Fatalf("read incident row: %v", err)
	}
	var links []ChainLink
	if err := json.Unmarshal(chain, &links); err != nil {
		t.Fatalf("decode chain: %v", err)
	}
	if severity != "warning" || source != "deterministic" || occurrences != 1 ||
		len(signals) != 1 || signals[0] != "sre_checkpoint_storm" ||
		rootCause != ep.RootCause || len(links) != 1 || links[0].Evidence != ep.Evidence {
		t.Fatalf("row = %s %s %q %v %+v x%d", severity, source, rootCause, signals, links,
			occurrences)
	}
	detected := rec.byType("incident_detected")
	if len(detected) != 1 || detected[0].Data["incident_id"] != inc.ID ||
		detected[0].Data["database"] != db {
		t.Fatalf("incident_detected events = %+v, want exactly one for %s", detected, inc.ID)
	}

	if err := ResolveIncident(ctx, pool, inc.ID, "user:1:ops@example.com",
		"handled"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	cont := ep
	cont.IncidentID, cont.ObservedAt = inc.ID, time.Now()
	if got, ok := eng.ObserveEpisode(ctx, cont); ok {
		t.Fatalf("the operator's resolution was reopened by the episode: %+v", got)
	}
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("persist: %v", err)
	}
	var resolvedBy string
	if err := pool.QueryRow(ctx, `SELECT resolved_by FROM sage.incidents WHERE id = $1`,
		inc.ID).Scan(&resolvedBy); err != nil || resolvedBy != "user:1:ops@example.com" {
		t.Fatalf("resolved_by = %q (%v), want the operator", resolvedBy, err)
	}
	if n := len(incidentRows(t, ctx, pool, db)); n != 1 {
		t.Fatalf("%d incident rows, want 1", n)
	}
}

// After a restart the engine hydrates the open detector incident and a
// new episode attaches to it instead of opening a duplicate.
func TestObserveEpisode_AttachesToAHydratedIncident(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	ep := checkpointEpisode(0)
	ep.ObservedAt = time.Now()
	first, _ := eng.ObserveEpisode(ctx, ep)
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("persist: %v", err)
	}
	restarted := testEngine()
	restarted.WithDatabaseName(db)
	if err := restarted.Hydrate(ctx, pool); err != nil {
		t.Fatalf("hydrate after restart: %v", err)
	}
	ep.ObservedAt = time.Now()
	again, ok := restarted.ObserveEpisode(ctx, ep)
	if !ok || again.ID != first.ID || again.OccurrenceCount != 2 {
		t.Fatalf("after restart = %+v, %v; want %s attached", again, ok, first.ID)
	}
	if err := restarted.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("persist: %v", err)
	}
	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 1 || rows[0].occurrences != 2 {
		t.Fatalf("rows = %+v, want one incident with 2 occurrences", rows)
	}
}
