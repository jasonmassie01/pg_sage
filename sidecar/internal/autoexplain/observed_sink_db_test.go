package autoexplain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// rca-04 integration: a csvlog record holding an auto_explain JSON plan,
// read by the log watcher, becomes one auto_explain_log row for its own
// database; the same plan logged for another database is not stored, and
// a second delivery of the same record is not stored twice.

func csvPlanLine(db string, queryID int64) string {
	return `2026-11-11 10:00:00.000 UTC,"app","` + db + `",42,"10.0.0.1:5000",` +
		`6551a000.2a,1,"SELECT",2026-11-11 09:59:00 UTC,3/7,0,LOG,00000,` +
		`"duration: 3.250 ms  plan:` + "\n" + `{` + "\n" +
		`  ""Query Text"": ""SELECT * FROM t WHERE id = 1"",` + "\n" +
		fmt.Sprintf(`  ""Query Identifier"": %d,`, queryID) + "\n" +
		`  ""Plan"": {""Node Type"": ""Index Scan"", ""Total Cost"": 8.3, ` +
		`""Actual Rows"": 1, ""Actual Loops"": 1}` + "\n" +
		`}",,,,,,,,,"psql","client backend",,0` + "\n"
}

// watchCSVLog writes body to a csvlog file, drains a log watcher over it
// and returns the entries a plans subscriber of db received.
func watchCSVLog(t *testing.T, db, body string) []logwatch.LogEntry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "postgresql.csv")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fw := logwatch.NewFileWatcher(config.LogWatchConfig{LogDirectory: dir, Format: "csvlog",
		PollIntervalMs: 3600000, MaxLineLenBytes: 65536}, nil)
	if err := fw.Start(t.Context()); err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	t.Cleanup(fw.Stop)
	sub := fw.SubscribeEntries("plans:test", db)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fw.Drain()
	return sub.Drain()
}

func TestObservedPlanSink_CSVLogPlanBecomesOneAutoExplainLogRow(t *testing.T) {
	pool := requireAuditFixturePool(t)
	bootstrapAuditFixture(t, pool)
	ctx := context.Background()
	var db string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	const qid = int64(991200601)
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.explain_cache WHERE queryid = $1", qid)
	}
	clean()
	t.Cleanup(clean)
	line := csvPlanLine(db, qid)
	entries := watchCSVLog(t, db, line+csvPlanLine("other_db", qid)+line)
	if len(entries) != 2 {
		t.Fatalf("subscription for %s got %d entries, want 2", db, len(entries))
	}
	sink := ObservedPlanSink{Database: db, MaxPerCycle: 100,
		Store: func(ctx context.Context, p ObservedPlan) error {
			return StoreObservedPlan(ctx, pool, p)
		}}
	res, err := sink.Ingest(ctx, append(entries, logwatch.LogEntry{Database: "other_db",
		Timestamp: entries[0].Timestamp, Message: entries[0].Message}))
	if err != nil || res.Stored != 2 || res.Rejected[RejectOtherDatabase] != 1 {
		t.Fatalf("Ingest = %+v, %v", res, err)
	}
	var n int
	var source, plan string
	var execMS float64
	if err := pool.QueryRow(ctx, `SELECT count(*), min(source), min(plan_json::text),
		min(execution_time) FROM sage.explain_cache WHERE queryid = $1`, qid).
		Scan(&n, &source, &plan, &execMS); err != nil {
		t.Fatal(err)
	}
	if n != 1 || source != "auto_explain_log" || execMS != 3.25 ||
		!strings.Contains(plan, `"Actual Rows": 1`) {
		t.Fatalf("explain_cache rows = %d, source %q, execution %v, plan %s; want one "+
			"executed auto_explain_log row (the duplicate delivery is not stored)",
			n, source, execMS, plan)
	}
}
