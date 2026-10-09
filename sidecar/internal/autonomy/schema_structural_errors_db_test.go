package autonomy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// faultTracer cancels the statement whose SQL contains the armed
// fragment, so each step of a structural pass can be failed on its own.
type faultTracer struct {
	mu       sync.Mutex
	fragment string
}

func (f *faultTracer) arm(fragment string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fragment = fragment
}

func (f *faultTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn,
	data pgx.TraceQueryStartData) context.Context {
	f.mu.Lock()
	fragment := f.fragment
	f.mu.Unlock()
	if fragment != "" && strings.Contains(data.SQL, fragment) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return cancelled
	}
	return ctx
}

func (f *faultTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func faultyDetector(t *testing.T, dsn string, now func() time.Time) (
	postgresSchemaDetector, *faultTracer) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	fault := &faultTracer{}
	cfg.ConnConfig.Tracer = fault
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return newPostgresSchemaDetector(pool, now), fault
}

// Every step of a pass names itself when it fails, keeps the cause, and
// leaves the cache as it was: the next cycle runs the pass again and
// answers like a full scan.
func TestStructuralIncremental_ErrorsNameTheirStepAndKeepTheCache(t *testing.T) {
	dsn := cadenceDatabase(t)
	execAndClose(t, dsn, incrementalFixture)
	clock := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	detector, fault := faultyDetector(t, dsn, func() time.Time { return clock })
	ctx := context.Background()
	fail := func(fragment, want string) {
		t.Helper()
		fault.arm(fragment)
		defer fault.arm("")
		items, err := detector.detectStructuralPathologies(ctx)
		if err == nil || !strings.Contains(err.Error(), want) ||
			!errors.Is(err, context.Canceled) || items != nil {
			t.Fatalf("%s failed: items %v, err %v; want %q wrapping context.Canceled",
				fragment, items, err, want)
		}
	}
	for _, step := range []struct{ fragment, want string }{
		{"pg_stat_sys_tables", "read catalog change counters"},
		{"repeatable read", "begin the structural catalog snapshot"},
		{"structural:tables", "list tables for the structural scan"},
		{"structural:text_types", "read text types for the structural scan"},
		{"structural:columns", "aggregate table columns for the structural scan"},
	} {
		fail(step.fragment, step.want)
		if detector.structural.scanned || len(detector.structural.tables) != 0 {
			t.Fatalf("a failed %s left a cache behind", step.fragment)
		}
	}
	assertReference(t, dsn, "after the failures", structuralAnswer(t, detector))
	passed := detector.structural.at
	clock = clock.Add(structuralMaxAge)
	fail("structural:versions", "read column versions for the structural scan")
	if !detector.structural.at.Equal(passed) || len(detector.structural.tables) != 3 {
		t.Fatalf("a failed verified pass changed the cache: at %s (want %s), %d tables",
			detector.structural.at, passed, len(detector.structural.tables))
	}
	assertReference(t, dsn, "after the failed verified pass", structuralAnswer(t, detector))
	if !detector.structural.at.Equal(clock) {
		t.Fatalf("the retried pass was not recorded: at %s, want %s",
			detector.structural.at, clock)
	}
}
