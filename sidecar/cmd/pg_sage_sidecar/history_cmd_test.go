package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func runHistoryCLI(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runHistoryCommand(context.Background(), args,
		func(k string) string { return env[k] }, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestHistoryCommandUsageErrors(t *testing.T) {
	dsns := map[string]string{"SAGE_HISTORY_MONITORED_DSN": "postgres://m@h/m",
		"SAGE_META_DB": "postgres://m@h/meta"}
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"no subcommand", dsns, nil, "usage"},
		{"unknown subcommand", dsns, []string{"move"}, "usage"},
		{"no database", dsns, []string{"migrate"}, "--database-id or --database"},
		{"both identities", dsns, []string{"migrate", "--database-id", "3",
			"--database", "app"}, "not both"},
		{"zero id", dsns, []string{"migrate", "--database-id", "0"}, "--database-id"},
		{"bad direction", dsns, []string{"migrate", "--database-id", "3", "--to", "s3"},
			"--to"},
		{"negative batch", dsns, []string{"migrate", "--database-id", "3", "--batch", "-1"},
			"--batch"},
		{"no monitored dsn", map[string]string{"SAGE_META_DB": "postgres://m@h/meta"},
			[]string{"migrate", "--database-id", "3"}, "SAGE_HISTORY_MONITORED_DSN"},
		{"no meta dsn", map[string]string{"SAGE_HISTORY_MONITORED_DSN": "postgres://m@h/m"},
			[]string{"migrate", "--database-id", "3"}, "SAGE_META_DB"},
		{"stray argument", dsns, []string{"migrate", "--database-id", "3", "extra"}, "usage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := runHistoryCLI(t, c.env, c.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2 (usage); stdout %q stderr %q", code, out, errOut)
			}
			if !strings.Contains(errOut, c.want) {
				t.Fatalf("stderr must mention %q:\n%s", c.want, errOut)
			}
		})
	}
}

// historyDBs returns a bootstrapped monitored database and a meta database
// with the history store, each with its DSN.
func historyDBs(t *testing.T) (monDSN, metaDSN string, mon, meta *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	monDSN = testdb.CreateDatabase(t, "histcmd_mon")
	metaDSN = testdb.CreateDatabase(t, "histcmd_meta")
	for _, d := range []struct {
		dsn string
		p   **pgxpool.Pool
	}{{monDSN, &mon}, {metaDSN, &meta}} {
		pool, err := pgxpool.New(ctx, d.dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatal(err)
		}
		*d.p = pool
	}
	if err := schema.BootstrapHistoryStore(ctx, meta); err != nil {
		t.Fatal(err)
	}
	return monDSN, metaDSN, mon, meta
}

func seedHistoryRows(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(context.Background(), `INSERT INTO sage.snapshots
			(collected_at, category, data) VALUES (now() - $1 * interval '1 minute', 'system',
			'{"i":1}')`, i); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `INSERT INTO sage.query_store
			(captured_at, queryid, calls, total_exec_time, mean_exec_time)
			VALUES (now() - $1 * interval '1 minute', 5, $1, $1, 1)`, i); err != nil {
			t.Fatal(err)
		}
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestHistoryCommandMigratesStatusAndCleansUp(t *testing.T) {
	monDSN, metaDSN, mon, meta := historyDBs(t)
	seedHistoryRows(t, mon, 4)
	if _, err := meta.Exec(context.Background(), `INSERT INTO sage.databases (name, host,
		database_name, username, password_enc) VALUES ('app', 'h', 'd', 'u', '\x00')`); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"SAGE_HISTORY_MONITORED_DSN": monDSN, "SAGE_META_DB": metaDSN}
	code, out, errOut := runHistoryCLI(t, env, "migrate", "--database", "app", "--batch", "3")
	if code != 0 {
		t.Fatalf("migrate exit %d: %s", code, errOut)
	}
	for _, want := range []string{"snapshots", "query_store", "copied 4", "complete"} {
		if !strings.Contains(out, want) {
			t.Fatalf("migrate output must mention %q:\n%s", want, out)
		}
	}
	if n := count(t, meta, "SELECT count(*) FROM sage.snapshots WHERE database_id IS NOT NULL"); n != 4 {
		t.Fatalf("store holds %d snapshots, want 4", n)
	}
	if n := count(t, mon, "SELECT count(*) FROM sage.snapshots"); n != 4 {
		t.Fatalf("migrate without --cleanup must leave the source (%d rows)", n)
	}
	code, out, errOut = runHistoryCLI(t, env, "status", "--database", "app")
	if code != 0 || !strings.Contains(out, "complete") {
		t.Fatalf("status exit %d: %s %s", code, out, errOut)
	}
	code, out, errOut = runHistoryCLI(t, env, "migrate", "--database", "app", "--cleanup")
	if code != 0 {
		t.Fatalf("cleanup exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "removed") {
		t.Fatalf("cleanup output must say what it removed:\n%s", out)
	}
	if n := count(t, mon, "SELECT count(*) FROM sage.snapshots"); n != 0 {
		t.Fatalf("cleanup left %d source rows", n)
	}
}

func TestHistoryCommandUnknownDatabaseName(t *testing.T) {
	monDSN, metaDSN, _, _ := historyDBs(t)
	env := map[string]string{"SAGE_HISTORY_MONITORED_DSN": monDSN, "SAGE_META_DB": metaDSN}
	code, _, errOut := runHistoryCLI(t, env, "migrate", "--database", "nope")
	if code != 1 || !strings.Contains(errOut, `"nope"`) {
		t.Fatalf("unknown database: exit %d, stderr %q", code, errOut)
	}
}

func TestHistoryCommandRefusesAMetaDatabaseWithoutTheStore(t *testing.T) {
	ctx := context.Background()
	monDSN := testdb.CreateDatabase(t, "histcmd_mon2")
	metaDSN := testdb.CreateDatabase(t, "histcmd_plain")
	for _, dsn := range []string{monDSN, metaDSN} {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatal(err)
		}
		pool.Close()
	}
	env := map[string]string{"SAGE_HISTORY_MONITORED_DSN": monDSN, "SAGE_META_DB": metaDSN}
	code, _, errOut := runHistoryCLI(t, env, "migrate", "--database-id", "4")
	// The command bootstraps the store itself (idempotent), so it works on a
	// meta database that never ran in meta mode.
	if code != 0 {
		t.Fatalf("migrate into a fresh meta database: exit %d: %s", code, errOut)
	}
}

func TestResolveRuntimeHistoryRefusals(t *testing.T) {
	_, _, mon, meta := historyDBs(t)
	ctx := context.Background()
	preserveHistoryGlobals(t)
	cfg.MetaDB, cfg.History.Store = "postgres://meta", "meta"

	historyMetaPool = func() *pgxpool.Pool { return nil }
	_, err := resolveRuntimeHistory(ctx, databaseRuntimeSpec{Name: "app", DatabaseID: 3,
		Pool: mon})
	if err == nil || !strings.Contains(err.Error(), "meta database") {
		t.Fatalf("meta mode without a meta pool: %v", err)
	}
	historyMetaPool = func() *pgxpool.Pool { return meta }
	_, err = resolveRuntimeHistory(ctx, databaseRuntimeSpec{Name: "agent", Pool: mon})
	if err == nil || !strings.Contains(err.Error(), "record") {
		t.Fatalf("meta mode without a database record id: %v", err)
	}
	seedHistoryRows(t, mon, 1)
	_, err = resolveRuntimeHistory(ctx, databaseRuntimeSpec{Name: "app", DatabaseID: 3,
		Pool: mon})
	if !errorsIsMigration(err) {
		t.Fatalf("unmigrated history must refuse the runtime: %v", err)
	}
	if _, err := mon.Exec(ctx, "TRUNCATE sage.snapshots, sage.query_store"); err != nil {
		t.Fatal(err)
	}
	st, err := resolveRuntimeHistory(ctx, databaseRuntimeSpec{Name: "app", DatabaseID: 3,
		Pool: mon})
	if err != nil || !st.Scoped() || st.DatabaseID() != 3 {
		t.Fatalf("clean database in meta mode: %+v %v", st, err)
	}
	if n := count(t, meta, `SELECT count(*) FROM sage.history_store_databases
		WHERE database_id = 3 AND database_name = 'app'`); n != 1 {
		t.Fatal("the runtime must register its database in the store")
	}
	cfg.History.Store = "monitored"
	if _, err := meta.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category, data,
		database_id) VALUES (now(), 'system', '{}', 3)`); err != nil {
		t.Fatal(err)
	}
	_, err = resolveRuntimeHistory(ctx, databaseRuntimeSpec{Name: "app", DatabaseID: 3,
		Pool: mon})
	if !errorsIsMigration(err) || !strings.Contains(err.Error(), "--to monitored") {
		t.Fatalf("history left in the store must refuse monitored mode: %v", err)
	}
	st, err = resolveRuntimeHistory(ctx, databaseRuntimeSpec{Name: "other", DatabaseID: 9,
		Pool: mon})
	if err != nil || st.Scoped() {
		t.Fatalf("monitored mode, nothing in the store for it: %+v %v", st, err)
	}
}

func errorsIsMigration(err error) bool {
	return errors.Is(err, histstore.ErrMigrationNeeded) &&
		strings.Contains(err.Error(), "history migrate")
}

func preserveHistoryGlobals(t *testing.T) {
	t.Helper()
	oldCfg, oldMetaPool := cfg, historyMetaPool
	if oldCfg == nil {
		cfg = config.DefaultConfig()
	} else {
		cfg = config.Clone(oldCfg)
	}
	t.Cleanup(func() { cfg, historyMetaPool = oldCfg, oldMetaPool })
}

func TestInstallHistoryRegistersUntilTheRuntimeStops(t *testing.T) {
	_, _, mon, meta := historyDBs(t)
	preserveHistoryGlobals(t)
	cfg.MetaDB, cfg.History.Store = "postgres://meta", "meta"
	historyMetaPool = func() *pgxpool.Pool { return meta }
	ctx, cancel := context.WithCancel(context.Background())
	rt := &databaseRuntime{spec: databaseRuntimeSpec{Name: "app", DatabaseID: 5, Pool: mon},
		ctx: ctx, cancel: cancel, workers: &sync.WaitGroup{}}
	if err := rt.installHistory(context.Background()); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := histstore.Resolve(mon); !got.Scoped() || got.DatabaseID() != 5 {
		t.Fatalf("the monitored pool must resolve to the store while the runtime runs: %+v",
			got)
	}
	cancel()
	rt.workers.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for histstore.Resolve(mon).Scoped() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if histstore.Resolve(mon).Scoped() {
		t.Fatal("a stopped runtime must unregister its history store")
	}
}
