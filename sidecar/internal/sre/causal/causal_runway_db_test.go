package causal

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Runway families from real probes: a REPEATABLE READ session pins a
// table past its (reloption) freeze maximum, and a bigint sequence owned
// by an integer column consumes toward the column's limit.

func runProbes(ctx context.Context, r *probes.Runner, calls ...probes.ID) []Observation {
	var out []Observation
	for i, id := range calls {
		args := probes.Args{}
		if id == probes.AutovacuumCancellations || id == probes.RunwayTrendsProbe {
			args.Window = time.Hour
		}
		out = append(out, Observation{EvidenceID: fmt.Sprintf("P%d", i+1),
			Result: r.Run(ctx, id, args)})
	}
	return out
}

func burn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	sql := fmt.Sprintf(`DO $$ BEGIN
		PERFORM set_config('synchronous_commit', 'off', false);
		FOR i IN 1..%d LOOP PERFORM pg_current_xact_id(); COMMIT; END LOOP; END $$`, n)
	if _, err := conn.Conn().PgConn().Exec(ctx, sql).ReadAll(); err != nil {
		t.Fatalf("burn xids: %v", err)
	}
}

func TestCausalDB_SessionPinsAnOverdueTable(t *testing.T) {
	pool, ctx, dsn := livePool(t)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	table := uniqueTable(t, ctx, pool, "CREATE TABLE %s (id int) WITH "+
		"(autovacuum_freeze_max_age = 100000)")
	holder := dial(t, ctx, dsn)
	t.Cleanup(func() { _ = holder.Close(context.Background()) })
	var pid int
	if _, err := holder.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("pid: %v", err)
	}
	burn(t, ctx, pool, 120000)
	r := probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))
	obs := runProbes(ctx, r, probes.XIDRunwayProbe, probes.WraparoundTablesProbe,
		probes.XminHorizon, probes.AutovacuumCancellations, probes.RunwayTrendsProbe,
		probes.XIDRunwayProbe)
	d := DiagnoseWraparound(obs, "table "+relName(table))
	// A replication slot of another test package may hold an even older
	// horizon on the shared cluster; then it is the root and the session
	// is still supported.
	h, st := byNode(d, XminHeldBySession)
	if h.Subject != fmt.Sprintf("pid %d", pid) || h.Confidence < 0.6 {
		t.Fatalf("session = %+v (%s), want pid %d supported", h, st, pid)
	}
	if st != StatusRoot && (d.Root == nil || d.Root.Node != XminHeldByReplication) {
		t.Fatalf("root = %+v, want the session (or an older slot)", d.Root)
	}
}

func relName(sanitized string) string {
	return "public." + strings.Trim(sanitized, `"`)
}

func TestCausalDB_SequenceOwnedByANarrowerColumn(t *testing.T) {
	pool, ctx, _ := livePool(t)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	seq := fmt.Sprintf("sre_seq_%d", time.Now().UnixNano())
	table := uniqueTable(t, ctx, pool, "CREATE TABLE %s (id int)")
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SEQUENCE %[1]s;
		ALTER TABLE %[2]s ALTER COLUMN id SET DEFAULT nextval('%[1]s');
		ALTER SEQUENCE %[1]s OWNED BY %[2]s.id;
		SELECT setval('%[1]s', 2147483647 - 100000)`, seq, table)); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	r := probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))
	obs := runProbes(ctx, r, probes.SequenceRunwayProbe, probes.RunwayTrendsProbe)
	if _, err := pool.Exec(ctx, "INSERT INTO "+table+
		" SELECT FROM generate_series(1, 200)"); err != nil {
		t.Fatalf("consume: %v", err)
	}
	later := runProbes(ctx, r, probes.SequenceRunwayProbe)
	later[0].EvidenceID = "P3"
	d := DiagnoseSequence(append(obs, later...), "sequence public."+seq)
	root := requireStatus(t, d, ColumnNarrowerThanSequence, StatusRoot)
	if root.Subject != "sequence public."+seq {
		t.Fatalf("root = %+v", root)
	}
}
