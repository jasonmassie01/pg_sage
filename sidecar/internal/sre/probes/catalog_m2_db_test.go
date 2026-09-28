package probes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// M2 probes against real PostgreSQL: the archiver, pg_sage's own recent
// actions, and the server start time on connection_saturation.

func TestCatalog_ArchiverReportsModeAndCounters(t *testing.T) {
	pool, ctx := livePool(t)
	res := NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, Archiver, Args{})
	arch, err := ArchiverStats(res)
	if err != nil {
		t.Fatalf("archiver = %+v (%v)", res, err)
	}
	var mode string
	_ = pool.QueryRow(ctx, "SHOW archive_mode").Scan(&mode)
	if arch.Mode != mode || arch.Archived < 0 || arch.Failed < 0 {
		t.Fatalf("archiver = %+v, want mode %q and known counters", arch, mode)
	}
}

func TestCatalog_SageActionsReadsRecentOwnActionsWithoutSQL(t *testing.T) {
	pool, ctx := livePool(t)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	var recent, old int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES ('create_index',
		'CREATE INDEX secret_literal_idx ON t (x) WHERE y = ''hunter2''', 'success')
		RETURNING id`).Scan(&recent); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, executed_at)
		VALUES ('vacuum', 'VACUUM t', 'success', now() - interval '3 hours')
		RETURNING id`).Scan(&old); err != nil {
		t.Fatalf("insert old action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.action_log WHERE id = ANY($1)", []int64{recent, old})
	})
	res := NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, SageActions,
		Args{Window: time.Hour})
	rows, err := SageActionRows(res)
	if err != nil {
		t.Fatalf("sage_actions = %+v (%v)", res, err)
	}
	var found bool
	for _, r := range rows {
		if r.ID == old {
			t.Fatalf("an action older than the window was returned: %+v", r)
		}
		if r.ID == recent {
			found = r.ActionType == "create_index" && r.Outcome == "success" && r.AgeS >= 0
		}
	}
	if !found {
		t.Fatalf("recent action %d missing from %+v", recent, rows)
	}
	for _, col := range res.Columns {
		if strings.Contains(col, "sql") {
			t.Fatalf("sage_actions returns SQL text (%s)", col)
		}
	}
}

// Without the sage schema, pg_sage's own action history is unsupported,
// never "no actions".
func TestCatalog_SageActionsUnsupportedWithoutSchema(t *testing.T) {
	_, ctx := livePool(t)
	fresh, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "sre_actions_fresh"))
	if err != nil {
		t.Fatalf("connect fresh: %v", err)
	}
	t.Cleanup(fresh.Close)
	res := NewRunner(fresh, Catalog(), NewLimiter(1)).Run(ctx, SageActions, Args{})
	if res.Status != StatusUnsupported {
		t.Fatalf("sage_actions without the schema = %+v, want unsupported", res)
	}
}

func TestCatalog_ConnectionSaturationCarriesServerStart(t *testing.T) {
	pool, ctx := livePool(t)
	gs, err := ConnectionGroups(NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx,
		ConnectionSaturation, Args{}))
	if err != nil || len(gs) == 0 {
		t.Fatalf("groups = %+v (%v)", gs, err)
	}
	var started time.Time
	_ = pool.QueryRow(ctx, "SELECT pg_postmaster_start_time()").Scan(&started)
	if !gs[0].ServerStartedAt.Equal(started) {
		t.Fatalf("server_started_at %v, want %v", gs[0].ServerStartedAt, started)
	}
}
