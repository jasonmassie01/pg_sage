package pgaudit

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/pgaudit"))
}

func freshDB(t *testing.T, label string) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, dsn
}

func count(t *testing.T, pool *pgxpool.Pool, where string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.guard_pgaudit_events WHERE "+where).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// Agent statements are kept; pg_sage's own statements are kept when they
// are an action (linked to it), otherwise dropped; everyone else's are
// not pg_sage's to keep.
func TestIngestKeepsOnlyCorrelatedRecords(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_ingest")
	ctx := context.Background()
	sql := "CREATE INDEX CONCURRENTLY orders_status ON orders (status)"
	var actionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		executed_at) VALUES ('create_index', $1, $2) RETURNING id`, sql,
		time.Date(2026, 10, 10, 12, 0, 1, 0, time.UTC)).Scan(&actionID); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	own := sessionUser(t, pool)
	entries := []logwatch.LogEntry{
		entry("pg_sage agent:"+idA+":9", agentguard.BrokerRoleName(idA), ddl),
		entry("pg_sage", own, `AUDIT: SESSION,1,1,DDL,CREATE INDEX,INDEX,`+
			`public.orders_status,"`+sql+`",<not logged>`),
		entry("pg_sage", own, `AUDIT: SESSION,2,1,READ,SELECT,,,`+
			`SELECT * FROM pg_stat_statements,<not logged>`),
		entry("psql", "alice", ddl),
		entry("pg_sage agent:ci-bot", "x", "duration: 3 ms"),
	}
	c := NewCorrelator(pool, "orders", nil)
	stats, err := c.Ingest(ctx, entries)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stats.Stored != 2 || stats.Dropped != 2 || stats.NotAudit != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if count(t, pool, "principal_id = '"+idA+"' AND action_id = 9 AND "+
		"correlated_by = 'application_name'") != 1 {
		t.Fatalf("agent record not stored with its principal and action")
	}
	if count(t, pool, "action_id = "+itoa(actionID)+" AND correlated_by = 'statement' "+
		"AND db_user = '"+own+"' AND database_name = 'orders'") != 1 {
		t.Fatalf("pg_sage action record not linked to action %d", actionID)
	}
	rep, err := auditchain.Verify(ctx, pool, auditchain.PGAuditEvents, auditchain.Window{})
	if err != nil || !rep.OK() || rep.Links != 2 {
		t.Fatalf("stored records are not chained: %+v %v", rep, err)
	}
}

// Nothing to ingest writes nothing; a storage failure is an error.
func TestIngestEmptyAndErrors(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_empty")
	c := NewCorrelator(pool, "orders", nil)
	stats, err := c.Ingest(context.Background(), nil)
	if err != nil || stats != (Stats{}) {
		t.Fatalf("empty ingest = %+v, %v", stats, err)
	}
	pool.Close()
	if _, err := c.Ingest(context.Background(), []logwatch.LogEntry{
		entry("pg_sage agent:ci-bot", "x", ddl)}); err == nil {
		t.Fatalf("closed pool: no error")
	}
}

// Installed reports pgaudit preloaded on the server.
func TestInstalledOnPlainServer(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_installed")
	got, err := Installed(context.Background(), pool)
	if err != nil {
		t.Fatalf("installed: %v", err)
	}
	var preload string
	_ = pool.QueryRow(context.Background(),
		"SELECT current_setting('shared_preload_libraries')").Scan(&preload)
	if got != strings.Contains(preload, "pgaudit") {
		t.Fatalf("Installed = %v with shared_preload_libraries %q", got, preload)
	}
}

// End to end with real pgaudit: a statement an agent role runs under its
// application_name is logged by pgaudit, read back from the server's
// jsonlog, parsed, attributed and stored; the same application_name set on
// a session of another role is stored unverified. Needs pgaudit preloaded
// and the jsonlog collector (the e2 test containers; CI's images lack
// pgaudit).
func TestRealPGAuditCorrelation(t *testing.T) {
	pool, dsn := freshDB(t, "pgaudit_real")
	ctx := context.Background()
	if ok, _ := Installed(ctx, pool); !ok {
		t.Skip("pgaudit is not in shared_preload_libraries on this server")
	}
	var logfile *string
	if err := pool.QueryRow(ctx, "SELECT pg_current_logfile('jsonlog')").
		Scan(&logfile); err != nil || logfile == nil {
		t.Skip("the server does not write jsonlog through the logging collector")
	}
	marker := "pgaudit_e2e_" + itoa(time.Now().UnixNano())
	id, err := agentguard.NewID()
	if err != nil {
		t.Fatalf("principal id: %v", err)
	}
	agent, role := agentLogin(t, pool, dsn, agentguard.LoginRoleName(id),
		"pg_sage agent:"+id+":77")
	if _, err := agent.Exec(ctx, "CREATE TABLE "+marker+"_agent (a int)"); err != nil {
		t.Fatalf("agent DDL: %v", err)
	}
	spoofDDL(t, pool, marker+"_spoof")
	entries := readLog(t, pool, *logfile, marker, 2)
	stats, err := NewCorrelator(pool, "orders", nil).Ingest(ctx, entries)
	if err != nil || stats.Stored != 2 {
		t.Fatalf("ingest of %d log lines = %+v, %v", len(entries), stats, err)
	}
	if count(t, pool, "principal_id = '"+id+"' AND action_id = 77 AND command = "+
		"'CREATE TABLE' AND correlated_by = 'application_name' AND db_user = '"+role+
		"' AND statement LIKE '%"+marker+"_agent%'") != 1 {
		t.Fatalf("real pgaudit record of the agent role not stored as attributed")
	}
	if count(t, pool, "principal_id IS NULL AND action_id IS NULL AND correlated_by = "+
		"'application_name_unverified' AND application = 'pg_sage agent:ci-bot:78' "+
		"AND statement LIKE '%"+marker+"_spoof%'") != 1 {
		t.Fatalf("spoofed real pgaudit record not stored unverified")
	}
	if n := nonAgentPrincipals(t, pool); n != 0 {
		t.Fatalf("CHECK-20: %d records name a principal for a non-agent role", n)
	}
}

// spoofDDL creates table on one of pool's (non-agent) sessions under an
// agent's application_name, with pgaudit logging DDL.
func spoofDDL(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, s := range []string{"SET pgaudit.log = 'ddl'",
		"SET application_name = 'pg_sage agent:ci-bot:78'",
		"CREATE TABLE " + table + " (a int)", "RESET pgaudit.log",
		"RESET application_name"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// readLog reads the server's current jsonlog and returns the parsed lines
// that mention marker, once there are at least want of them.
func readLog(t *testing.T, pool *pgxpool.Pool, file, marker string,
	want int) []logwatch.LogEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var body string
		if err := pool.QueryRow(context.Background(), "SELECT pg_read_file($1)", file).
			Scan(&body); err != nil {
			t.Fatalf("read log: %v", err)
		}
		var out []logwatch.LogEntry
		sc := bufio.NewScanner(strings.NewReader(body))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if !strings.Contains(sc.Text(), marker) {
				continue
			}
			if e, err := logwatch.ParseJSONLogLine(sc.Bytes()); err == nil {
				out = append(out, e)
			}
		}
		if len(out) >= want {
			return out
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("fewer than %d log lines mention %s", want, marker)
	return nil
}
