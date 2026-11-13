package sre

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// market-02 (CHECK-04): an open critical workload_anomaly finding starts
// exactly one investigation across 3 cycles (one idempotency key per
// episode). Warnings and resolved anomalies start none.

func insertAnomalyFinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	severity, status, ident, detail string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, status)
		VALUES ('workload_anomaly', $1, 'workload', $2, 'Workload anomaly', $3::jsonb, $4)
		RETURNING id`, severity, ident, detail, status).Scan(&id); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.findings WHERE id = $1", id)
	})
	return id
}

func TestTriggers_CriticalAnomalyStartsOneInvestigationPerEpisode(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	insertAnomalyFinding(t, ctx, pool, "critical", "open", "query_mean_ms:queryid:101",
		anomalyDetail)
	insertAnomalyFinding(t, ctx, pool, "warning", "open", "wal_bytes_per_s:cluster",
		`{"metric": "wal_bytes_per_s", "key": "cluster",
		  "episode_start": "2026-11-10T09:00:00Z"}`)
	insertAnomalyFinding(t, ctx, pool, "critical", "resolved", "total_backends:database",
		`{"metric": "total_backends", "key": "database",
		  "episode_start": "2026-11-09T09:00:00Z"}`)
	src := NewPGTriggerSource(pool, "orders")
	c, _ := testCoordinator(t, ctx, st, newScriptedRunner(), src)
	var first Investigation
	created := 0
	for cycle := 0; cycle < 3; cycle++ {
		got, err := src.Triggers(ctx)
		if err != nil {
			t.Fatalf("cycle %d triggers: %v", cycle, err)
		}
		var anomalies []Trigger
		for _, tr := range got {
			if tr.Kind == TriggerAnomaly {
				anomalies = append(anomalies, tr)
			}
		}
		if len(anomalies) != 1 {
			t.Fatalf("cycle %d anomaly triggers = %+v, want only the open critical", cycle,
				anomalies)
		}
		inv, isNew, err := c.Start(ctx, anomalies[0])
		if err != nil {
			t.Fatalf("cycle %d start: %v", cycle, err)
		}
		if isNew {
			created++
		}
		if cycle == 0 {
			first = inv
		} else if inv.ID != first.ID {
			t.Fatalf("cycle %d started %s, want the first investigation %s", cycle,
				inv.ID, first.ID)
		}
	}
	if created != 1 || first.TriggerKind != TriggerAnomaly ||
		first.Subject != "query_mean_ms queryid:101" {
		t.Fatalf("created %d, first %+v, want exactly one workload_anomaly investigation",
			created, first)
	}
	requireAnomalyTriage(t, ctx, c, first.ID)
}

// requireAnomalyTriage runs the investigation and wants the inconclusive
// workload_anomaly triage the scripted (empty) probes give.
func requireAnomalyTriage(t *testing.T, ctx context.Context, c *Coordinator, id UUID) {
	t.Helper()
	if err := c.Investigate(ctx, id); err != nil {
		t.Fatalf("investigate: %v", err)
	}
	scope, _ := c.Scope()
	done, err := c.store.Get(ctx, scope, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if done.State != StateInconclusive || done.Summary.Family != "workload_anomaly" {
		t.Fatalf("investigation = %s family %q, want an inconclusive workload_anomaly triage",
			done.State, done.Summary.Family)
	}
}
