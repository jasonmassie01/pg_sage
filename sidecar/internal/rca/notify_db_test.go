package rca

import (
	"context"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/notify"
)

// Regression tests for substrate-B3: incidents never notified anyone.

type recordingDispatcher struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recordingDispatcher) Dispatch(
	_ context.Context, e notify.Event,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recordingDispatcher) byType(typ string) []notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []notify.Event
	for _, e := range r.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestLifecycle_NotifiesDetectedEscalatedResolvedOnce(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	rec := &recordingDispatcher{}
	eng.WithDispatcher(rec)

	for i := 0; i < 6; i++ { // escalation_cycles = 5
		cycle(t, ctx, eng, pool, true)
	}
	for i := 0; i < 4; i++ {
		cycle(t, ctx, eng, pool, false)
	}

	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	id := rows[0].id
	for _, typ := range []string{
		"incident_detected", "incident_escalated", "incident_resolved",
	} {
		evs := rec.byType(typ)
		if len(evs) != 1 {
			t.Errorf("%s events = %d, want exactly 1", typ, len(evs))
			continue
		}
		if evs[0].Data["incident_id"] != id {
			t.Errorf("%s incident_id = %v, want %s",
				typ, evs[0].Data["incident_id"], id)
		}
		if evs[0].Data["database"] != db {
			t.Errorf("%s database = %v, want %s",
				typ, evs[0].Data["database"], db)
		}
	}
	if evs := rec.byType("incident_escalated"); len(evs) == 1 &&
		evs[0].Severity != "critical" {
		t.Errorf("escalated severity = %q, want critical", evs[0].Severity)
	}
	if evs := rec.byType("incident_resolved"); len(evs) == 1 &&
		evs[0].Data["resolved_by"] != ResolvedByAuto {
		t.Errorf("resolved_by = %v, want %s",
			evs[0].Data["resolved_by"], ResolvedByAuto)
	}
}

func TestLifecycle_ManualResolveEmitsResolvedWithActor(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	rec := &recordingDispatcher{}
	eng.WithDispatcher(rec)

	cycle(t, ctx, eng, pool, true)
	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if err := ResolveIncident(
		ctx, pool, rows[0].id, "user:ops@example.com", "restarted",
	); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}
	cycle(t, ctx, eng, pool, false)

	evs := rec.byType("incident_resolved")
	if len(evs) != 1 {
		t.Fatalf("incident_resolved events = %d, want 1", len(evs))
	}
	if evs[0].Data["resolved_by"] != "user:ops@example.com" ||
		evs[0].Data["reason"] != "restarted" {
		t.Errorf("resolved event data = %v", evs[0].Data)
	}
}
