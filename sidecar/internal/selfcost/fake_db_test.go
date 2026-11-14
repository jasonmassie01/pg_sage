package selfcost

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeEntry is one pg_stat_statements entry of the fake server.
type fakeEntry struct {
	key  StatementKey
	c    StatementCounters // Text is the entry's full query text
	sage bool              // the text carries the /* pg_sage */ tag
}

// fakeDB answers the statements selfcost sends, by shape: the extension
// version, the counters (pg_stat_statements(false)), the tagged texts
// (pg_stat_statements(true)) and the sage tables (pg_class). It counts
// each shape and keeps every statement it was sent.
type fakeDB struct {
	mu          sync.Mutex
	version     string // "" means the extension is not installed
	versionErr  error
	entries     []fakeEntry
	reset       *time.Time // pg_stat_statements_info.stats_reset
	countersErr error
	textsErr    error
	textsOnly   map[int64]bool // entries the counters read does not see yet
	noText      map[int64]bool // entries whose text reads as NULL
	tables      []fakeTable
	schemaErr   error
	noSchemaRow bool
	calls       map[string]int
	sent        []string
}

type fakeTable struct {
	name                 string
	bytes, read, written int64
}

func newFakeDB(version string) *fakeDB {
	return &fakeDB{version: version, calls: map[string]int{}}
}

// add appends an entry with pg_sage's tag or an application's text.
func (f *fakeDB) add(id int64, sage bool, calls int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	text := fmt.Sprintf("SELECT app_%d", id)
	if sage {
		text = fmt.Sprintf("/* pg_sage */ SELECT sage_%d", id)
	}
	f.entries = append(f.entries, fakeEntry{key: StatementKey{UserID: 10, QueryID: id,
		TopLevel: true}, c: StatementCounters{TimeMs: float64(calls) * 2, Calls: calls,
		Blocks: calls * 3, Text: text}, sage: sage})
}

// set changes an entry's calls (and its time and blocks with them).
func (f *fakeDB) set(id int64, calls int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.entries {
		if f.entries[i].key.QueryID == id {
			f.entries[i].c.Calls, f.entries[i].c.TimeMs = calls, float64(calls)*2
			f.entries[i].c.Blocks = calls * 3
		}
	}
}

func (f *fakeDB) remove(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.entries[:0]
	for _, e := range f.entries {
		if e.key.QueryID != id {
			kept = append(kept, e)
		}
	}
	f.entries = kept
}

func (f *fakeDB) count(shape string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[shape]
}

func (f *fakeDB) record(sql string) string {
	f.sent = append(f.sent, sql)
	switch {
	case strings.Contains(sql, "pg_extension"):
		f.calls["version"]++
		return "version"
	case strings.Contains(sql, `pg_stat_statements"(false)`):
		f.calls["counters"]++
		return "counters"
	case strings.Contains(sql, `pg_stat_statements"(true)`):
		f.calls["texts"]++
		return "texts"
	case strings.Contains(sql, "pg_class"):
		f.calls["schema"]++
		return "schema"
	}
	f.calls["other"]++
	return "other"
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.record(sql) != "version" {
		return fakeRow{err: fmt.Errorf("fake: unexpected QueryRow %q", sql)}
	}
	if f.versionErr != nil {
		return fakeRow{err: f.versionErr}
	}
	if f.version == "" {
		return fakeRow{err: pgx.ErrNoRows}
	}
	return fakeRow{vals: []any{f.version, "public"}} // version, schema
}

func (f *fakeDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.record(sql) {
	case "counters":
		return f.counterRows(sql)
	case "texts":
		return f.textRows(sql)
	case "schema":
		return f.schemaRows()
	}
	return nil, fmt.Errorf("fake: unexpected Query %q", sql)
}

func (f *fakeDB) counterRows(sql string) (pgx.Rows, error) {
	if f.countersErr != nil {
		return nil, f.countersErr
	}
	top := strings.Contains(sql, "toplevel")
	var reset any
	if f.reset != nil && strings.Contains(sql, "pg_stat_statements_info") {
		reset = *f.reset
	}
	out := &fakeRows{}
	for _, e := range f.entries {
		if f.textsOnly[e.key.QueryID] {
			continue
		}
		out.rows = append(out.rows, []any{e.key.UserID, e.key.QueryID, e.key.TopLevel && top,
			e.c.TimeMs, e.c.Calls, e.c.Blocks, reset})
	}
	return out, nil
}

func (f *fakeDB) textRows(sql string) (pgx.Rows, error) {
	if f.textsErr != nil {
		return nil, f.textsErr
	}
	top := strings.Contains(sql, "toplevel")
	out := &fakeRows{}
	nulls := strings.Contains(sql, "query IS NULL")
	for _, e := range f.entries {
		if f.noText[e.key.QueryID] {
			if nulls { // a strpos filter alone drops the NULL texts
				out.rows = append(out.rows, []any{e.key.UserID, e.key.QueryID,
					e.key.TopLevel && top, e.c.TimeMs, e.c.Calls, e.c.Blocks, nil})
			}
			continue
		}
		if e.sage {
			out.rows = append(out.rows, []any{e.key.UserID, e.key.QueryID,
				e.key.TopLevel && top, e.c.TimeMs, e.c.Calls, e.c.Blocks, e.c.Text})
		}
	}
	return out, nil
}

func (f *fakeDB) schemaRows() (pgx.Rows, error) {
	if f.schemaErr != nil {
		return nil, f.schemaErr
	}
	out := &fakeRows{}
	if f.noSchemaRow {
		return out, nil
	}
	if len(f.tables) == 0 {
		out.rows = append(out.rows, []any{"app", nil, nil, nil, nil})
	}
	for _, t := range f.tables {
		out.rows = append(out.rows, []any{"app", t.name, t.bytes, t.read, t.written})
	}
	return out, nil
}

type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return assign(r.vals, dest)
}

type fakeRows struct {
	rows [][]any
	i    int
	err  error
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return r.rows[r.i-1], nil }

func (r *fakeRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *fakeRows) Scan(dest ...any) error { return assign(r.rows[r.i-1], dest) }

// assign stores vals in dest like pgx: a NULL leaves a pointer target nil.
func assign(vals []any, dest []any) error {
	if len(vals) != len(dest) {
		return fmt.Errorf("fake: %d columns, %d destinations", len(vals), len(dest))
	}
	for i, v := range vals {
		target := reflect.ValueOf(dest[i]).Elem()
		if v == nil {
			target.Set(reflect.Zero(target.Type()))
			continue
		}
		val := reflect.ValueOf(v)
		if target.Kind() == reflect.Pointer && val.Kind() != reflect.Pointer {
			p := reflect.New(target.Type().Elem())
			p.Elem().Set(val.Convert(target.Type().Elem()))
			val = p
		}
		if !val.Type().AssignableTo(target.Type()) {
			return fmt.Errorf("fake: column %d is %T, destination %s", i, v, target.Type())
		}
		target.Set(val)
	}
	return nil
}

// noticeLog records log lines.
type noticeLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *noticeLog) log(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(msg, args...))
}

func (l *noticeLog) matching(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}
