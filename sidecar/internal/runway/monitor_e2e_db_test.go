package runway

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// End to end on real PostgreSQL: a bigint sequence owned by an integer
// column is consuming toward the column's limit; the runway monitor's
// tick opens the forecast finding and a pre-incident investigation in
// the real coordinator, which concludes on the binding limit.
func TestMonitor_OpensARealPreIncidentInvestigation(t *testing.T) {
	pool, ctx := livePool(t)
	name := fmt.Sprintf("e2e_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SEQUENCE %[1]s_seq;
		CREATE TABLE %[1]s (id int DEFAULT nextval('%[1]s_seq'));
		ALTER SEQUENCE %[1]s_seq OWNED BY %[1]s.id;
		SELECT setval('%[1]s_seq', 2147383647)`, name)); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+name) })
	seq := "public." + name + "_seq"
	// An hour of samples consuming one value per second, ending just
	// below the current value, so the monitor's own sample continues them.
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value, counter, limit_value)
		SELECT 'sequence', $1, 'seed', now() - make_interval(mins => 55 - i * 5),
		       2147383647 - (11 - i) * 300, 2147383647 - (11 - i) * 300, 2147483647
		FROM generate_series(0, 11) i`, seq); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runner := probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))
	coord, st := realCoordinator(t, ctx, pool, runner, name)
	m := newTestMonitor(t, pool, runner, coord)
	if _, err := m.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	scope, _ := coord.Scope()
	page, err := st.List(ctx, scope, sre.ListFilter{})
	var inv sre.Investigation
	for _, item := range page.Items {
		if item.Subject == "sequence "+seq {
			inv = item
		}
	}
	if err != nil || inv.TriggerKind != sre.TriggerSequence {
		t.Fatalf("investigations = %+v (%v), want the one the tick opened", page.Items, err)
	}
	if err := coord.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("investigate: %v", err)
	}
	got, err := st.Get(ctx, scope, inv.ID)
	if err != nil || got.State != sre.StateConcluded ||
		got.Summary.Root != "column_narrower_than_sequence" {
		t.Fatalf("investigation = %s root %q (%s) err %v", got.State, got.Summary.Root,
			got.Summary.Reason, err)
	}
}

// realCoordinator is the investigator on the test database; between its
// two samples the table consumes 50 sequence values.
func realCoordinator(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	runner sre.ProbeRunner, table string) (*sre.Coordinator, *sre.PostgresStore) {
	t.Helper()
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	cfg := sre.DefaultCoordinatorConfig("runway-e2e:" + table)
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st, Runner: runner,
		Config: cfg, Wait: func(ctx context.Context, _ time.Duration) error {
			_, err := pool.Exec(ctx, "INSERT INTO "+table+
				" SELECT FROM generate_series(1, 50)")
			return err
		}})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	if _, err := coord.Bind(ctx); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return coord, st
}
