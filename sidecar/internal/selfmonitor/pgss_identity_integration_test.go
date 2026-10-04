package selfmonitor_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfcost"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
	"github.com/pg-sage/sidecar/internal/workload"
)

// identityProbe is one pg_sage statement form; table names its own fresh
// relation, so its pg_stat_statements entry is new (a new relation OID is
// a new queryid) and no earlier text of the same queryid can stand in.
type identityProbe struct {
	form  string
	table string
}

// The property pg_sage's self-identification rests on, checked against the
// server's own pg_stat_statements (run it on PostgreSQL 14 through 18):
// whatever form a statement takes in pg_sage's source, sent through a pool
// configured like pg_sage's, the text pg_stat_statements stores carries the
// pg_sage tag, and every reader that tells pg_sage's statements apart
// (selfcost, the workload filters, IsTagged) recognizes it.
func TestSelfIdentification_PgStatStatementsKeepsTheTag(t *testing.T) {
	env := newIdentityEnv(t)
	probes := env.probes("plain_ext", "leading_ext", "label_simple", "paren_ext",
		"multi_first", "multi_second", "multi_third", "utility")
	pgssepoch.Attempt(t, env.ctx, env.pool, 3, func() []string {
		env.runProbes(t, probes)
		return env.identityProblems(t, probes)
	})
	env.checkMultiStatementData(t, probes)
}

// The reason the tag does not lead the statement: on PostgreSQL 18
// pg_stat_statements stores the text from the first token and a leading
// comment is lost; before 18 it is kept. Sent on a plain connection (no
// pg_sage pool to move it), this documents what the test above guards.
func TestSelfIdentification_LeadingCommentOnlyKeptBefore18(t *testing.T) {
	env := newIdentityEnv(t)
	probe := env.probes("raw_leading")[0]
	raw, err := pgx.Connect(env.ctx, env.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = raw.Close(context.Background()) }()
	wantTagged := env.versionNum < 180000
	pgssepoch.Attempt(t, env.ctx, env.pool, 3, func() []string {
		sql := "/* pg_sage */ SELECT id FROM " + probe.table + " WHERE id = $1"
		if _, err := raw.Exec(env.ctx, sql, 1); err != nil {
			t.Fatalf("raw leading-tag statement: %v", err)
		}
		texts := env.entries(t, probe.table)
		if len(texts) == 0 {
			return []string{"raw_leading: not tracked by pg_stat_statements"}
		}
		var problems []string
		for _, e := range texts {
			if selfmonitor.IsTagged(e.text) != wantTagged {
				problems = append(problems, fmt.Sprintf("server %d stored %q: tagged = %v, "+
					"want %v", env.versionNum, e.text, !wantTagged, wantTagged))
			}
		}
		return problems
	})
}

type identityEnv struct {
	ctx        context.Context
	dsn        string
	pool       *pgxpool.Pool // configured like pg_sage's own
	admin      *pgx.Conn     // fixture setup and pg_stat_statements reads
	suffix     string
	versionNum int
}

func newIdentityEnv(t *testing.T) *identityEnv {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	env := &identityEnv{ctx: ctx, dsn: dsn, admin: admin,
		suffix: fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000_000)}
	if err := admin.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&env.versionNum); err != nil {
		t.Fatalf("server version: %v", err)
	}
	if _, err := admin.Exec(ctx, "SELECT count(*) FROM pg_stat_statements"); err != nil {
		t.Skipf("pg_stat_statements not readable (not installed or not preloaded): %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	selfmonitor.ConfigurePool(cfg)
	if env.pool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(env.pool.Close)
	return env
}

// probes creates one fresh table per form.
func (e *identityEnv) probes(forms ...string) []identityProbe {
	out := make([]identityProbe, 0, len(forms))
	for _, form := range forms {
		out = append(out, identityProbe{form: form,
			table: "public.selfid_" + form + "_" + e.suffix})
	}
	return out
}

func (e *identityEnv) create(t *testing.T, probes []identityProbe) {
	t.Helper()
	for _, p := range probes {
		if _, err := e.admin.Exec(e.ctx, "CREATE TABLE IF NOT EXISTS "+p.table+
			" (id int, v text)"); err != nil {
			t.Fatalf("create %s: %v", p.table, err)
		}
		table := p.table
		t.Cleanup(func() {
			_, _ = e.admin.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
		})
	}
}

func byForm(probes []identityProbe) map[string]string {
	m := map[string]string{}
	for _, p := range probes {
		m[p.form] = p.table
	}
	return m
}

// runProbes sends every form through the pg_sage pool, as source code
// writes them: untagged, with a leading tag, with a labelled leading tag,
// parenthesized, as a multi-statement simple query (the schema bootstrap's
// shape, with literals that hold ';' and tag text) and as utility DDL.
func (e *identityEnv) runProbes(t *testing.T, probes []identityProbe) {
	t.Helper()
	e.create(t, probes)
	tb := byForm(probes)
	run := func(what, sql string, args ...any) {
		if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	run("plain_ext", "SELECT id FROM "+tb["plain_ext"]+" WHERE id = $1", 1)
	run("leading_ext", "/* pg_sage */ SELECT id FROM "+tb["leading_ext"]+" WHERE id = $1", 1)
	run("label_simple", "/* pg_sage probe:selfid v1 */ SELECT count(*) FROM "+
		tb["label_simple"], pgx.QueryExecModeSimpleProtocol)
	run("paren_ext", "(SELECT id FROM "+tb["paren_ext"]+" WHERE id = $1)", 1)
	run("multi", "TRUNCATE "+tb["multi_first"]+", "+tb["multi_second"]+", "+
		tb["multi_third"]+"; INSERT INTO "+tb["multi_first"]+" VALUES (1, 'a; SELECT 2');\n"+
		"/* pg_sage */ INSERT INTO "+tb["multi_second"]+
		" VALUES (2, $q$b; /* pg_sage */ c$q$);\n"+
		"INSERT INTO "+tb["multi_third"]+` VALUES (3, 'it''s; "x"')`)
	run("utility", "CREATE INDEX IF NOT EXISTS selfid_utility_"+e.suffix+"_idx ON "+
		tb["utility"]+" (id)")
}

type pgssEntry struct {
	queryID int64
	text    string
	advice  bool // workload.AdviceSQL over the stored text
}

// entries returns this database's pg_stat_statements entries naming table.
func (e *identityEnv) entries(t *testing.T, table string) []pgssEntry {
	t.Helper()
	rows, err := e.admin.Query(e.ctx, `SELECT queryid, query, `+workload.AdviceSQL("query")+`
		FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND queryid IS NOT NULL AND strpos(query, $1) > 0`,
		strings.TrimPrefix(table, "public."))
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pgssEntry, error) {
		var x pgssEntry
		return x, r.Scan(&x.queryID, &x.text, &x.advice)
	})
	if err != nil {
		t.Fatalf("scan pg_stat_statements: %v", err)
	}
	return out
}

// identityProblems lists every form pg_stat_statements does not hold as
// pg_sage's own, by each reader that tells pg_sage's statements apart.
func (e *identityEnv) identityProblems(t *testing.T, probes []identityProbe) []string {
	t.Helper()
	reading, err := selfcost.Read(e.ctx, e.admin)
	if err != nil {
		t.Fatalf("selfcost.Read: %v", err)
	}
	selfIDs := map[int64]bool{}
	for k := range reading.Statements {
		selfIDs[k.QueryID] = true
	}
	var problems []string
	for _, p := range probes {
		found := e.entries(t, p.table)
		if p.form == "multi_first" || p.form == "multi_second" || p.form == "multi_third" {
			found = onlyInserts(found)
		}
		if len(found) == 0 {
			problems = append(problems, p.form+": not tracked by pg_stat_statements")
		}
		for _, x := range found {
			problems = append(problems, entryProblems(p.form, x, selfIDs)...)
		}
	}
	return problems
}

// entryProblems checks one stored entry against every self-identifying
// reader.
func entryProblems(form string, x pgssEntry, selfIDs map[int64]bool) []string {
	var problems []string
	if !selfmonitor.IsTagged(x.text) {
		problems = append(problems, fmt.Sprintf("%s: stored untagged: %q", form, x.text))
	}
	if !selfIDs[x.queryID] {
		problems = append(problems, fmt.Sprintf("%s: selfcost does not count %q", form, x.text))
	}
	if workload.Classify(x.text) != workload.Self {
		problems = append(problems, fmt.Sprintf("%s: workload.Classify(%q) = %q, want %q",
			form, x.text, workload.Classify(x.text), workload.Self))
	}
	if x.advice {
		problems = append(problems, fmt.Sprintf("%s: workload.AdviceSQL keeps %q", form, x.text))
	}
	return problems
}

// onlyInserts keeps the INSERT entries of a multi-statement probe table
// (its TRUNCATE is one statement for all three tables).
func onlyInserts(entries []pgssEntry) []pgssEntry {
	var out []pgssEntry
	for _, x := range entries {
		if strings.HasPrefix(strings.ToUpper(x.text), "INSERT") {
			out = append(out, x)
		}
	}
	return out
}

// checkMultiStatementData proves the tagger split the multi-statement
// query only between statements: literals holding ';' and tag text
// arrive byte for byte.
func (e *identityEnv) checkMultiStatementData(t *testing.T, probes []identityProbe) {
	t.Helper()
	tb := byForm(probes)
	want := map[string]string{
		tb["multi_first"]:  "a; SELECT 2",
		tb["multi_second"]: "b; /* pg_sage */ c",
		tb["multi_third"]:  `it's; "x"`,
	}
	for table, value := range want {
		var got string
		if err := e.admin.QueryRow(e.ctx, "SELECT v FROM "+table).Scan(&got); err != nil {
			t.Fatalf("read back %s: %v", table, err)
		}
		if got != value {
			t.Errorf("%s holds %q, want %q (a tag was placed inside a literal)", table, got,
				value)
		}
	}
}
