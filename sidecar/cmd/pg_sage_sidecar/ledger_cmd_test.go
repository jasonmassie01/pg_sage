package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/outcomes"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// pg_sage ledger (ux-02a): the outcome ledger of one database, read-only.
// Exit 0 for a read, 2 for a usage or connection error. No concurrent
// access test: the command is one read per process.

func runLedgerCLI(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runLedgerCommand(context.Background(), args,
		func(k string) string { return env[k] }, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestLedgerCommandUsageErrors(t *testing.T) {
	dsn := map[string]string{"SAGE_HISTORY_MONITORED_DSN": "postgres://m@127.0.0.1:1/app"}
	for _, c := range []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"bad since", dsn, []string{"--since", "abc"}, "9600h"},
		{"negative since", dsn, []string{"--since", "-1h"}, "9600h"},
		{"over retention", dsn, []string{"--since", "9601h"}, "9600h"},
		{"bad verdict", dsn, []string{"--verdict", "bogus"}, "no_change"},
		{"bad limit", dsn, []string{"--limit", "0"}, "1 to 10000"},
		{"unknown flag", dsn, []string{"--everything"}, "usage"},
		{"stray argument", dsn, []string{"now"}, "usage"},
		{"no dsn", map[string]string{}, nil, "SAGE_HISTORY_MONITORED_DSN"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := runLedgerCLI(t, c.env, c.args...)
			if code != 2 || out != "" || !strings.Contains(errOut, c.want) {
				t.Fatalf("exit %d stdout %q stderr %q, want 2, nothing and %q", code, out,
					errOut, c.want)
			}
		})
	}
}

func TestLedgerCommandConnectionErrorExitsTwo(t *testing.T) {
	env := map[string]string{"SAGE_META_DB": "postgres://u:secretpw@127.0.0.1:1/app?" +
		"connect_timeout=2"}
	code, out, errOut := runLedgerCLI(t, env, "--since", "24h", "--database", "orders")
	if code != 2 || out != "" {
		t.Fatalf("exit %d stdout %q, want 2 and nothing on stdout", code, out)
	}
	if !strings.Contains(errOut, "read outcomes: orders: ") {
		t.Fatalf("stderr %q, want read outcomes: orders: ...", errOut)
	}
	if strings.Contains(errOut, "secretpw") {
		t.Fatalf("stderr leaks the password: %q", errOut)
	}
}

func TestLedgerLineIsOneLinePerRow(t *testing.T) {
	m, e := &outcomes.Effect{Metric: "blocks_per_call", ChangePct: -42},
		&outcomes.Effect{Metric: "blocks_per_call", ChangePct: -40}
	at := time.Date(2026, 11, 16, 3, 4, 5, 0, time.UTC)
	line := ledgerLine(outcomes.Row{ActionID: 12, Database: "orders", What: "Add an index",
		Why: "seq scans\non orders", Verdict: "improved", Measured: m, Expected: e,
		RollbackState: "available", EvidenceID: "ev-1", ExecutedAt: at})
	for _, want := range []string{"2026-11-16T03:04:05Z", "orders", "#12", "Add an index",
		"improved", "measured -42.0% blocks_per_call", "expected -40.0%",
		"rollback available", "evidence ev-1"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("line %q spans lines", line)
	}
	bare := ledgerLine(outcomes.Row{ActionID: 3, Database: "o", What: "Rebuild an index",
		Verdict: "unverifiable", MeasuredReason: "suspend epoch break",
		RollbackState: "not_reversible", ExecutedAt: at})
	if !strings.Contains(bare, "not measured: suspend epoch break") ||
		strings.Contains(bare, "expected") {
		t.Fatalf("unmeasured line %q", bare)
	}
}

func ledgerDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "ledger_cli")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return dsn, pool
}

func TestLedgerCommandPrintsTheRowsAsTextAndJSON(t *testing.T) {
	dsn, pool := ledgerDB(t)
	var act int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.action_log
		(executed_at, action_type, sql_executed, rollback_sql, outcome) VALUES
		(now() - interval '1 hour', 'create_index', 'CREATE INDEX i ON t (a)',
		'DROP INDEX i', 'success') RETURNING id`).Scan(&act); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.action_outcome
		(action_log_id, action_class, predicted, prediction_method, verdict, observed)
		VALUES ($1, 'index_create', '{"method":"none"}', 'none', 'improved',
		'{"metric":"blocks_per_call","before":100,"after":58,"change_pct":-42}')`,
		act); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"SAGE_HISTORY_MONITORED_DSN": dsn}
	code, out, errOut := runLedgerCLI(t, env, "--since", "24h", "--database", "orders")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "Add an index") ||
		!strings.Contains(lines[0], "measured -42.0%") {
		t.Fatalf("text output %q, want one line for the one change", out)
	}
	code, out, errOut = runLedgerCLI(t, env, "--since", "24h", "--database", "orders",
		"--json")
	if code != 0 || errOut != "" {
		t.Fatalf("json exit %d stderr %q", code, errOut)
	}
	var got outcomes.Result
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json output %q: %v", out, err)
	}
	want, err := outcomes.Read(context.Background(), pool, "orders", outcomes.Query{
		Since: 24 * time.Hour, Now: time.Now(), Limit: outcomes.DefaultLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 1 || got.Rows[0].ActionID != act || got.Rows[0].Measured == nil ||
		len(want.Rows) != 1 || got.Rows[0].ActionID != want.Rows[0].ActionID ||
		got.Rows[0].Verdict != want.Rows[0].Verdict || got.Summary.Changes != 1 {
		t.Fatalf("json rows %+v, the read gives %+v", got.Rows, want.Rows)
	}
	code, out, _ = runLedgerCLI(t, env, "--since", "30m", "--database", "orders")
	if code != 0 || !strings.Contains(out, "no changes") {
		t.Fatalf("an empty window: exit %d %q", code, out)
	}
}
