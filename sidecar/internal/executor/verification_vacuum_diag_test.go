package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// vacuumVerbose runs VACUUM (VERBOSE) on table in a session of its own and
// returns the server's report: how many dead row versions it removed and,
// when it could not remove them, the oldest xmin that held them.
func vacuumVerbose(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	table string) string {
	t.Helper()
	cfg := pool.Config().ConnConfig.Copy()
	var report strings.Builder
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		fmt.Fprintf(&report, "%s: %s", n.Severity, n.Message)
		if n.Detail != "" {
			fmt.Fprintf(&report, " (%s)", n.Detail)
		}
		report.WriteString("\n")
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect for VACUUM: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "VACUUM (VERBOSE) public."+table); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	return report.String()
}

// vacuumStats is the table's statistics row as the verifier reads it.
func vacuumStats(ctx context.Context, pool *pgxpool.Pool, table string) (
	dead, vacuums int64, err error) {
	err = pool.QueryRow(ctx, `SELECT COALESCE(n_dead_tup, 0), COALESCE(vacuum_count, 0)
		FROM pg_stat_user_tables WHERE relid = to_regclass($1)`, "public."+table).
		Scan(&dead, &vacuums)
	return dead, vacuums, err
}

// waitVacuumReported waits until the statistics show the VACUUM: dead
// tuples under limit. PostgreSQL 14's collector receives reports over UDP
// and drops them when its socket buffer is full (a loaded CI server); a
// VACUUM whose report never arrives leaves vacuum_count unchanged, and is
// run once more so its report can arrive. On failure it names what held
// the dead rows back.
func waitVacuumReported(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	table string, limit, vacuumsBefore int64, report string) {
	t.Helper()
	rerun := false
	for i := 0; i < 120; i++ {
		dead, vacuums, err := vacuumStats(ctx, pool, table)
		if err != nil {
			t.Fatalf("read dead tuples: %v", err)
		}
		if dead < limit {
			return
		}
		if i == 40 && vacuums == vacuumsBefore && !rerun {
			t.Logf("VACUUM's statistics report did not arrive in 10 s; running it again")
			report += vacuumVerbose(t, ctx, pool, table)
			rerun = true
		}
		time.Sleep(250 * time.Millisecond)
	}
	dead, vacuums, _ := vacuumStats(ctx, pool, table)
	t.Fatalf("dead tuples on %s still %d 30 s after VACUUM (vacuum_count %d, %d before)\n"+
		"VACUUM VERBOSE:\n%s\nhorizon holders:\n%s", table, dead, vacuums, vacuumsBefore,
		report, horizonHolders(ctx, pool))
}

// horizonHolders lists every session, replication slot and prepared
// transaction on the server holding an xmin or xid, oldest first, with its
// database and query, so a failure names what kept rows from VACUUM.
func horizonHolders(ctx context.Context, pool *pgxpool.Pool) string {
	rows, err := pool.Query(ctx, `
		SELECT 'session pid=' || pid || ' db=' || COALESCE(datname, '-') ||
		       ' type=' || COALESCE(backend_type, '-') || ' state=' || COALESCE(state, '-') ||
		       ' app=' || COALESCE(application_name, '') ||
		       ' xmin=' || COALESCE(backend_xmin::text, '-') ||
		       ' xid=' || COALESCE(backend_xid::text, '-') ||
		       ' xact_start=' || COALESCE(xact_start::text, '-') ||
		       ' query=' || left(COALESCE(query, ''), 200),
		       GREATEST(age(backend_xmin), age(backend_xid))::bigint
		FROM pg_stat_activity
		WHERE (backend_xmin IS NOT NULL OR backend_xid IS NOT NULL)
		  AND pid <> pg_backend_pid()
		UNION ALL
		SELECT 'slot ' || slot_name || ' type=' || slot_type || ' db=' ||
		       COALESCE(database, '-') || ' active=' || active ||
		       ' xmin=' || COALESCE(xmin::text, '-') ||
		       ' catalog_xmin=' || COALESCE(catalog_xmin::text, '-'),
		       GREATEST(age(xmin), age(catalog_xmin))::bigint
		FROM pg_replication_slots
		UNION ALL
		SELECT 'prepared ' || gid || ' db=' || database || ' xid=' || transaction ||
		       ' prepared=' || prepared, age(transaction)::bigint
		FROM pg_prepared_xacts
		ORDER BY 2 DESC NULLS LAST`)
	if err != nil {
		return "(could not read: " + err.Error() + ")"
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		var age *int64
		if err := rows.Scan(&line, &age); err != nil {
			return b.String() + "(read error: " + err.Error() + ")"
		}
		fmt.Fprintf(&b, "  age=%v %s\n", derefAge(age), line)
	}
	if b.Len() == 0 {
		return "  (none)\n"
	}
	return b.String()
}

func derefAge(a *int64) string {
	if a == nil {
		return "-"
	}
	return fmt.Sprint(*a)
}
