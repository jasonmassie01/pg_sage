package catalogread

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// footprint-05: Reader.Done reports what each bounded statement cost
// (begin and execution time, and its error) exactly once, so the
// collector can meter the statements it really runs instead of a proxy.

type doneRecorder struct {
	mu    sync.Mutex
	stats []ReadStat
	ctxs  []context.Context
}

func (d *doneRecorder) done(ctx context.Context, s ReadStat) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stats = append(d.stats, s)
	d.ctxs = append(d.ctxs, ctx)
}

func (d *doneRecorder) all() []ReadStat {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]ReadStat(nil), d.stats...)
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestReader_DoneReportsEachQueryOnceOnClose(t *testing.T) {
	rec := &doneRecorder{}
	r := New(testPool(t), Default())
	r.Done = rec.done
	const sql = "SELECT g, pg_sleep(0.01) FROM generate_series(1, 3) g"
	rows, err := r.Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("Done ran before the rows were read: %+v", got)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	rows.Close() // a second Close must not report again
	got := rec.all()
	if n != 3 || len(got) != 1 {
		t.Fatalf("read %d rows, Done ran %d times (%+v), want 3 rows and once", n,
			len(got), got)
	}
	if got[0].SQL != sql || got[0].Err != nil {
		t.Fatalf("stat = %+v, want the statement's SQL and no error", got[0])
	}
	// Three 10 ms sleeps: the execution time covers the rows' production.
	if got[0].Exec < 30*time.Millisecond || got[0].Exec > 5*time.Second {
		t.Fatalf("Exec = %s, want at least the 30 ms the statement slept", got[0].Exec)
	}
	if got[0].Begin <= 0 || got[0].Begin > 5*time.Second {
		t.Fatalf("Begin = %s, want the measured begin of the transaction", got[0].Begin)
	}
}

func TestReader_DoneReportsQueryRowOnScan(t *testing.T) {
	rec := &doneRecorder{}
	r := New(testPool(t), Default())
	r.Done = rec.done
	var v int
	if err := r.QueryRow(context.Background(), "SELECT $1::int", 41).Scan(&v); err != nil {
		t.Fatalf("row: %v", err)
	}
	got := rec.all()
	if v != 41 || len(got) != 1 || got[0].SQL != "SELECT $1::int" || got[0].Err != nil {
		t.Fatalf("value %d, stats %+v: want 41 and one clean stat", v, got)
	}
	// A row that is not there is reported with its error too.
	err := r.QueryRow(context.Background(), "SELECT 1 WHERE false").Scan(&v)
	got = rec.all()
	if !errors.Is(err, pgx.ErrNoRows) || len(got) != 2 || !errors.Is(got[1].Err, pgx.ErrNoRows) {
		t.Fatalf("no-row read = %v, stats %+v: want ErrNoRows reported", err, got)
	}
}

func TestReader_DoneReportsTheStatementTimeout(t *testing.T) {
	rec := &doneRecorder{}
	r := New(testPool(t), Timeouts{Statement: 100 * time.Millisecond})
	r.Done = rec.done
	err := r.QueryRow(context.Background(), "SELECT pg_sleep(5)").Scan()
	got := rec.all()
	if pgCode(err) != "57014" || len(got) != 1 || pgCode(got[0].Err) != "57014" {
		t.Fatalf("read = %v, stats %+v: want one 57014 reported", err, got)
	}
	var pe *PhaseError
	if !errors.As(got[0].Err, &pe) || pe.Phase != PhaseExecution {
		t.Fatalf("reported error %v does not name the execution phase", got[0].Err)
	}
	if got[0].Exec < 90*time.Millisecond || got[0].Exec > 3*time.Second {
		t.Fatalf("Exec = %s, want about the 100 ms deadline", got[0].Exec)
	}
}

func TestReader_DoneReportsAFailingQuery(t *testing.T) {
	rec := &doneRecorder{}
	r := New(testPool(t), Default())
	r.Done = rec.done
	rows, err := r.Query(context.Background(), "SELECT * FROM no_such_table_fp05")
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	got := rec.all()
	if pgCode(err) != "42P01" || len(got) != 1 || pgCode(got[0].Err) != "42P01" {
		t.Fatalf("read = %v, stats %+v: want one 42P01 reported", err, got)
	}
}

func TestReader_DoneReportsTheBeforeStatementFailure(t *testing.T) {
	rec := &doneRecorder{}
	r := New(testPool(t), Timeouts{Statement: 100 * time.Millisecond})
	r.Done = rec.done
	ctx := WithBeforeStatement(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT pg_sleep(5)")
		return err
	})
	if _, err := r.Query(ctx, "SELECT 1"); pgCode(err) != "57014" {
		t.Fatalf("hooked read = %v, want a statement timeout", err)
	}
	got := rec.all()
	if len(got) != 1 || got[0].SQL != "SELECT 1" || pgCode(got[0].Err) != "57014" {
		t.Fatalf("stats %+v, want the hooked statement's timeout reported once", got)
	}
	if got[0].Exec < 90*time.Millisecond {
		t.Fatalf("Exec = %s, want the time the hook spent inside the deadline", got[0].Exec)
	}
}

// Begin failure needs no database: a reader without one fails to begin.
func TestReader_DoneReportsBeginFailure(t *testing.T) {
	rec := &doneRecorder{}
	r := Reader{Done: rec.done}
	if _, err := r.Query(context.Background(), "SELECT 1"); !errors.Is(err, errNoDatabase) {
		t.Fatalf("query without a database = %v", err)
	}
	if err := r.QueryRow(context.Background(), "SELECT 2").Scan(); !errors.Is(err,
		errNoDatabase) {
		t.Fatalf("row without a database = %v", err)
	}
	got := rec.all()
	if len(got) != 2 || got[0].SQL != "SELECT 1" || got[1].SQL != "SELECT 2" {
		t.Fatalf("stats %+v, want both begin failures reported", got)
	}
	for _, s := range got {
		if !errors.Is(s.Err, errNoDatabase) || s.Exec != 0 {
			t.Fatalf("begin failure stat %+v, want the error and no execution time", s)
		}
	}
}

func TestReader_DoneCarriesTheStatementContext(t *testing.T) {
	type key struct{}
	rec := &doneRecorder{}
	r := Reader{Done: rec.done}
	ctx := context.WithValue(context.Background(), key{}, "indexes")
	_, _ = r.Query(ctx, "SELECT 1")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.ctxs) != 1 || rec.ctxs[0].Value(key{}) != "indexes" {
		t.Fatalf("Done did not get the statement's context: %v", rec.ctxs)
	}
}

// Nil: a reader without Done (every caller before footprint-05) still reads.
func TestReader_NilDoneIsAllowed(t *testing.T) {
	r := Reader{}
	if _, err := r.Query(context.Background(), "SELECT 1"); !errors.Is(err, errNoDatabase) {
		t.Fatalf("nil Done changed the error: %v", err)
	}
}

func TestReader_DoneUnderConcurrentReads(t *testing.T) {
	rec := &doneRecorder{}
	r := New(testPool(t), Default())
	r.Done = rec.done
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = r.QueryRow(context.Background(), "SELECT $1::int", i).Scan(new(int))
		}(i)
	}
	wg.Wait()
	got := rec.all()
	if len(got) != 12 {
		t.Fatalf("Done ran %d times for 12 concurrent reads", len(got))
	}
	for _, s := range got {
		if s.Err != nil || s.SQL != "SELECT $1::int" {
			t.Fatalf("concurrent stat %+v", s)
		}
	}
}
