package selfconfig

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Evidence is read from the real catalog: relation and sequence counts,
// how long the catalog and sequence scans take, the server's connection
// limit and the database's temp-file counters.

func seedEvidenceSchema(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	for _, stmt := range []string{
		"DROP SCHEMA IF EXISTS sc_evidence CASCADE",
		"CREATE SCHEMA sc_evidence",
		"CREATE SEQUENCE sc_evidence.s1", "CREATE SEQUENCE sc_evidence.s2",
		"CREATE SEQUENCE sc_evidence.s3",
		"CREATE TABLE sc_evidence.t (id int)",
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS sc_evidence CASCADE")
	})
}

func TestGatherReadsTheCatalog(t *testing.T) {
	pool, ctx := testPool(t)
	seedEvidenceSchema(t, pool, ctx)
	var relations, sequences, maxConns float64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_class),
		(SELECT count(*) FROM pg_sequences),
		current_setting('max_connections')::float8`).
		Scan(&relations, &sequences, &maxConns); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	ev, err := Gather(ctx, pool)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if !ev.Relations.Known || ev.Relations.Value != relations {
		t.Errorf("relations %+v, want %v", ev.Relations, relations)
	}
	if !ev.Sequences.Known || ev.Sequences.Value != sequences || sequences < 3 {
		t.Errorf("sequences %+v, want %v", ev.Sequences, sequences)
	}
	if !ev.MaxConnections.Known || ev.MaxConnections.Value != maxConns {
		t.Errorf("max_connections %+v, want %v", ev.MaxConnections, maxConns)
	}
	if !ev.CatalogScanMs.Known || ev.CatalogScanMs.Value < 0 ||
		ev.CatalogScanMs.Value > 5000 {
		t.Errorf("catalog scan %+v", ev.CatalogScanMs)
	}
	if !ev.SequenceScanMs.Known || ev.SequenceScanMs.Value < 0 {
		t.Errorf("sequence scan %+v", ev.SequenceScanMs)
	}
	if !ev.TempBytes.Known || ev.TempBytes.Value < 0 || !ev.StatsAgeSeconds.Known ||
		ev.StatsAgeSeconds.Value <= 0 || !ev.TempBytesPerSecond.Known {
		t.Errorf("temp counters %+v %+v %+v", ev.TempBytes, ev.StatsAgeSeconds,
			ev.TempBytesPerSecond)
	}
	// The collector cycle estimate is the catalog scan plus the statements
	// read (zero when pg_stat_statements is absent).
	if !ev.CollectorCycleMs.Known ||
		ev.CollectorCycleMs.Value != ev.CatalogScanMs.Value+ev.StatementsScanMs.Value {
		t.Errorf("collector cycle %+v, catalog %+v, statements %+v", ev.CollectorCycleMs,
			ev.CatalogScanMs, ev.StatementsScanMs)
	}
	if ev.At.Before(before) {
		t.Errorf("evidence time %v before the call %v", ev.At, before)
	}
}

func TestGatherErrorsAreDistinguishable(t *testing.T) {
	pool, _ := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ev, err := Gather(ctx, pool)
	if err == nil {
		t.Fatal("Gather on a cancelled context returned no error")
	}
	if !strings.Contains(err.Error(), "selfconfig evidence") {
		t.Fatalf("error %q does not say which layer failed", err)
	}
	if ev.Relations.Known || ev.CatalogScanMs.Known || ev.MaxConnections.Known ||
		ev.Sequences.Known || ev.TempBytes.Known {
		t.Fatalf("measures claimed known after a failed read: %+v", ev)
	}
	if _, err := Gather(context.Background(), nil); err == nil ||
		!strings.Contains(err.Error(), "selfconfig evidence") {
		t.Fatalf("nil pool: %v", err)
	}
}

func TestTempRateSincePreviousSample(t *testing.T) {
	t1 := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	prev := Evidence{At: t1, TempBytes: Known(1000), TempBytesPerSecond: Known(1)}
	cur := Evidence{At: t1.Add(100 * time.Second), TempBytes: Known(51000),
		TempBytesPerSecond: Known(3)}
	if got := cur.WithTempRateSince(prev).TempBytesPerSecond; !got.Known || got.Value != 500 {
		t.Fatalf("delta rate %+v, want 500 B/s", got)
	}
	// A stats reset (counter went down) or no elapsed time keeps the
	// since-reset rate.
	reset := cur
	reset.TempBytes = Known(10)
	if got := reset.WithTempRateSince(prev).TempBytesPerSecond; got.Value != 3 {
		t.Fatalf("after a reset %+v", got)
	}
	same := cur
	same.At = t1
	if got := same.WithTempRateSince(prev).TempBytesPerSecond; got.Value != 3 {
		t.Fatalf("no elapsed time %+v", got)
	}
	if got := cur.WithTempRateSince(Evidence{}).TempBytesPerSecond; got.Value != 3 {
		t.Fatalf("no previous sample %+v", got)
	}
}

func TestKnownAndCitations(t *testing.T) {
	if m := Known(4); !m.Known || m.Value != 4 {
		t.Fatalf("Known(4) = %+v", m)
	}
	ev := Evidence{Relations: Known(10), MaxConnections: Known(100)}
	cites := ev.Citations()
	names := map[string]bool{}
	for _, c := range cites {
		names[c.Name] = true
	}
	if !names["relations"] || !names["max_connections"] || names["catalog_scan_ms"] {
		t.Fatalf("citations cite only known measures: %+v", cites)
	}
}
