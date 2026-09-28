package sre

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The RCA trigger adapter reads committed triggers: open incidents whose
// signals map to an investigation family, and open plan_regression
// findings (AI-SRE-SPEC §5: plan regressions are an investigation
// trigger). Case ids match the Cases projection so the panel can link
// them.

func insertIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string,
	resolved bool, signals ...string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, causal_chain, signal_ids, source, confidence,
		 database_name, resolved_at)
		VALUES ('warning', 'fixture', '[]'::jsonb, $1, 'deterministic', 1.0, $2,
		        CASE WHEN $3 THEN now() END)
		RETURNING id::text`, signals, db, resolved).Scan(&id); err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.incidents WHERE id = $1::uuid", id)
	})
	return id
}

func insertPlanFinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	objectID, detail string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail)
		VALUES ('plan_regression', 'warning', 'query', $1, 'Plan regression', $2::jsonb)
		RETURNING id`, objectID, detail).Scan(&id); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.findings WHERE id = $1", id)
	})
	return id
}

func byKey(ts []Trigger) map[string]Trigger {
	out := map[string]Trigger{}
	for _, tr := range ts {
		out[tr.IdempotencyKey] = tr
	}
	return out
}

func TestTriggers_MapsIncidentsAndPlanFindings(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	lock := insertIncident(t, ctx, pool, "orders", false, "lock_contention")
	conn := insertIncident(t, ctx, pool, "orders", false, "cache_hit_low", "connections_high")
	wal := insertIncident(t, ctx, pool, "orders", false, "wal_growth_spike")
	insertIncident(t, ctx, pool, "orders", true, "lock_contention")
	unmapped := insertIncident(t, ctx, pool, "orders", false, "cache_hit_low")
	other := insertIncident(t, ctx, pool, "billing", false, "lock_contention")
	finding := insertPlanFinding(t, ctx, pool, "queryid:4711", `{"queryid": 4711}`)
	insertPlanFinding(t, ctx, pool, "queryid:bad", `{"query": "no id"}`)

	got, err := NewPGTriggerSource(pool, "orders").Triggers(ctx)
	if err != nil {
		t.Fatalf("triggers: %v", err)
	}
	m := byKey(got)
	if len(m) != 4 {
		t.Fatalf("triggers = %+v, want lock, connections, wal and one plan", got)
	}
	want := map[string]TriggerKind{"incident:" + lock: TriggerLock,
		"incident:" + conn: TriggerConnections, "incident:" + wal: TriggerWAL}
	for key, kind := range want {
		if m[key].Kind != kind || m[key].IncidentID != strings.TrimPrefix(key, "incident:") {
			t.Errorf("%s = %+v, want %s", key, m[key], kind)
		}
	}
	for _, id := range []string{unmapped, other} {
		if _, ok := m["incident:"+id]; ok {
			t.Errorf("incident %s should not trigger", id)
		}
	}
	lockCase := cases.ProjectIncident(cases.SourceIncident{ID: lock, DatabaseName: "orders",
		SignalIDs: []string{"lock_contention"}, Source: "deterministic"}).ID
	if m["incident:"+lock].CaseID != lockCase {
		t.Errorf("lock case id %q, want the projected %q", m["incident:"+lock].CaseID, lockCase)
	}
	plan := m["finding:"+itoa(finding)]
	planCase := cases.IdentityKeyForFinding(cases.SourceFinding{DatabaseName: "orders",
		Category: "plan_regression", ObjectType: "query", ObjectIdentifier: "queryid:4711",
		Detail: map[string]any{"queryid": 4711.0}})
	if plan.Kind != TriggerPlan || plan.Subject != "queryid 4711" || plan.CaseID != planCase {
		t.Fatalf("plan trigger = %+v, want queryid 4711 and case %q", plan, planCase)
	}
}

func TestTriggers_EmptyAndMissingSchema(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	got, err := NewPGTriggerSource(pool, "nothing_here").Triggers(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("no incidents = %+v (%v), want none", got, err)
	}
	fresh, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "sre_triggers_fresh"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(fresh.Close)
	if _, err := NewPGTriggerSource(fresh, "x").Triggers(ctx); err == nil {
		t.Fatal("a database without the sage schema produced no error")
	}
	if _, err := NewPGTriggerSource(nil, "x").Triggers(ctx); err == nil {
		t.Fatal("a nil pool produced no error")
	}
}
