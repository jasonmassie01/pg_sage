package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// collection-05: the extra categories (io, prepared_xacts, partitions,
// config_data) are unknown, not empty, when their read fails, and the
// configuration snapshot fails as a whole instead of reading a failed
// sub-read as a fact. These tests need no database.
//
// No concurrent access tests: the collector reads its steps sequentially
// in one goroutine, and the fakes here are used by one test each.

// fakeRead is the canned result of one configuration sub-read.
type fakeRead struct {
	rows     [][]any
	queryErr error
	rowsErr  error
}

// fakeConfigQuerier answers the configuration snapshot's sub-reads by the
// SQL they send. standby simulates a hot standby: pg_current_wal_lsn()
// fails there unless the statement asks pg_is_in_recovery() first.
type fakeConfigQuerier struct {
	reads      map[string]fakeRead
	standby    bool
	standbyLSN any
	sqls       map[string]string
}

func goodConfigReads() map[string]fakeRead {
	return map[string]fakeRead{
		"pg_settings": {rows: [][]any{{"work_mem", "4096", "kB", "default", false, "user"}}},
		"reloptions": {rows: [][]any{{"app", "orders",
			"{autovacuum_vacuum_scale_factor=0.01}"}}},
		"connection_states": {rows: [][]any{{"active", 3, 1.5}}},
		"wal_position":      {rows: [][]any{{"0/16B3748"}}},
		"extensions":        {rows: [][]any{{"hypopg"}}},
		"database_owner":    {rows: [][]any{{true}}},
	}
}

func newFakeConfigQuerier() *fakeConfigQuerier {
	return &fakeConfigQuerier{reads: goodConfigReads(), sqls: map[string]string{}}
}

// subReadOf names the configuration sub-read sql belongs to.
func subReadOf(sql string) string {
	switch {
	case strings.Contains(sql, "FROM pg_settings"):
		return "pg_settings"
	case strings.Contains(sql, "reloptions"):
		return "reloptions"
	case strings.Contains(sql, "GROUP BY state"):
		return "connection_states"
	case strings.Contains(sql, "_wal_"):
		return "wal_position"
	case strings.Contains(sql, "pg_available_extensions"):
		return "extensions"
	case strings.Contains(sql, "datdba"):
		return "database_owner"
	}
	return ""
}

func (q *fakeConfigQuerier) read(sql string) (fakeRead, error) {
	name := subReadOf(sql)
	q.sqls[name] = sql
	r, ok := q.reads[name]
	if !ok {
		return fakeRead{}, fmt.Errorf("fake: unexpected SQL %q", sql)
	}
	if name == "wal_position" && q.standby {
		if !strings.Contains(sql, "pg_is_in_recovery()") {
			return fakeRead{}, &pgconn.PgError{Code: "55000",
				Message: "recovery is in progress"}
		}
		if !strings.Contains(sql, "pg_last_wal_receive_lsn()") {
			return fakeRead{}, errors.New("fake: standby WAL read without a receive LSN")
		}
		return fakeRead{rows: [][]any{{q.standbyLSN}}}, nil
	}
	return r, nil
}

func (q *fakeConfigQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	r, err := q.read(sql)
	if err != nil {
		return nil, err
	}
	if r.queryErr != nil {
		return nil, r.queryErr
	}
	return &fakeRows{rows: r.rows, err: r.rowsErr}, nil
}

func (q *fakeConfigQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	r, err := q.read(sql)
	if err == nil {
		err = r.queryErr
	}
	if err != nil {
		return fakeRow{err: err}
	}
	if len(r.rows) == 0 {
		return fakeRow{err: pgx.ErrNoRows}
	}
	return fakeRow{vals: r.rows[0]}
}

// fakeRows is a pgx.Rows over canned values.
type fakeRows struct {
	rows [][]any
	i    int
	err  error
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return r.rows[r.i-1], nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }

func (r *fakeRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *fakeRows) Scan(dest ...any) error { return assignAll(r.rows[r.i-1], dest) }

type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return assignAll(r.vals, dest)
}

// assignAll scans vals into dest the way pgx would for these simple
// types: a value of another type is a scan error, NULL zeroes the target.
func assignAll(vals []any, dest []any) error {
	if len(vals) != len(dest) {
		return fmt.Errorf("fake scan: %d values into %d targets", len(vals), len(dest))
	}
	for i, d := range dest {
		dst := reflect.ValueOf(d).Elem()
		if vals[i] == nil {
			dst.SetZero()
			continue
		}
		v := reflect.ValueOf(vals[i])
		switch {
		case v.Type().AssignableTo(dst.Type()):
			dst.Set(v)
		case dst.Kind() == reflect.Pointer && v.Type().AssignableTo(dst.Type().Elem()):
			p := reflect.New(dst.Type().Elem())
			p.Elem().Set(v)
			dst.Set(p)
		default:
			return fmt.Errorf("fake scan: cannot scan %T into %s (column %d)",
				vals[i], dst.Type(), i)
		}
	}
	return nil
}

// Happy path: every sub-read lands in the snapshot.
func TestConfigSnapshot_AllSubReadsFillTheSnapshot(t *testing.T) {
	cs, err := collectConfigSnapshot(context.Background(), newFakeConfigQuerier())
	if err != nil {
		t.Fatalf("collectConfigSnapshot: %v", err)
	}
	if cs == nil || len(cs.PGSettings) != 1 || cs.PGSettings[0].Name != "work_mem" ||
		cs.PGSettings[0].Context != "user" {
		t.Fatalf("pg_settings = %+v", cs)
	}
	if len(cs.TableReloptions) != 1 || cs.TableReloptions[0].RelName != "orders" {
		t.Fatalf("reloptions = %+v", cs.TableReloptions)
	}
	if len(cs.ConnectionStates) != 1 || cs.ConnectionStates[0].Count != 3 ||
		cs.ConnectionStates[0].AvgDurationSeconds != 1.5 {
		t.Fatalf("connection states = %+v", cs.ConnectionStates)
	}
	if cs.WALPosition != "0/16B3748" || !cs.OwnsDatabase ||
		strings.Join(cs.ExtensionsAvailable, ",") != "hypopg" {
		t.Fatalf("wal %q owns database %v extensions %v", cs.WALPosition, cs.OwnsDatabase,
			cs.ExtensionsAvailable)
	}
}

// Error propagation: a failed reloptions read is not "no per-table
// autovacuum overrides": the whole snapshot is unknown, naming reloptions.
func TestConfigSnapshot_FailedReloptionsFailsSnapshot(t *testing.T) {
	q := newFakeConfigQuerier()
	q.reads["reloptions"] = fakeRead{queryErr: &pgconn.PgError{Code: "57014",
		Message: "canceling statement due to statement timeout"}}
	cs, err := collectConfigSnapshot(context.Background(), q)
	if cs != nil {
		t.Fatalf("snapshot = %+v after a failed reloptions read, want nil", cs)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "reloptions: ") ||
		!strings.Contains(err.Error(), "57014") {
		t.Fatalf("err = %v, want the reloptions timeout named", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v does not wrap the server error (retries need it)", err)
	}
}

// Error propagation: every sub-read's failure fails the snapshot and is
// named, for a query error and for an error found while reading the rows.
func TestConfigSnapshot_EverySubReadFailureFailsSnapshot(t *testing.T) {
	for name := range goodConfigReads() {
		for _, kind := range []string{"query", "rows"} {
			q := newFakeConfigQuerier()
			r := q.reads[name]
			if kind == "query" {
				r.queryErr = errors.New("boom")
			} else {
				if name == "wal_position" || name == "database_owner" {
					continue // single-row reads have no row iteration
				}
				r.rowsErr = errors.New("connection reset mid-read")
			}
			q.reads[name] = r
			cs, err := collectConfigSnapshot(context.Background(), q)
			if cs != nil || err == nil || !strings.HasPrefix(err.Error(), name+": ") {
				t.Errorf("%s %s error: snapshot %v err %v, want nil and %q named",
					name, kind, cs != nil, err, name)
			}
		}
	}
}

// Invalid input: a row that does not scan fails the sub-read instead of
// being skipped (a skipped reloptions row reads as "no override").
func TestConfigSnapshot_ScanErrorFailsSnapshot(t *testing.T) {
	bad := map[string][]any{
		"pg_settings":       {"work_mem", "4096", "kB", "default", "yes", "user"},
		"reloptions":        {"app", "orders", 42},
		"connection_states": {"active", "three", 1.5},
		"wal_position":      {12},
		"extensions":        {true},
		"database_owner":    {"yes"},
	}
	for name, row := range bad {
		q := newFakeConfigQuerier()
		q.reads[name] = fakeRead{rows: [][]any{row}}
		cs, err := collectConfigSnapshot(context.Background(), q)
		if cs != nil || err == nil || !strings.HasPrefix(err.Error(), name+": ") ||
			!strings.Contains(err.Error(), "cannot scan") {
			t.Errorf("%s bad row: snapshot %v err %v, want a named scan error",
				name, cs != nil, err)
		}
	}
}

// A hot standby (pg_is_in_recovery stubbed): pg_current_wal_lsn() fails
// there, so the WAL position is the received LSN and the snapshot stays
// available.
func TestConfigSnapshot_StandbyReadsReceivedWALPosition(t *testing.T) {
	q := newFakeConfigQuerier()
	q.standby, q.standbyLSN = true, "0/3000060"
	cs, err := collectConfigSnapshot(context.Background(), q)
	if err != nil {
		t.Fatalf("collectConfigSnapshot on a standby: %v (sql %q)", err, q.sqls["wal_position"])
	}
	if cs.WALPosition != "0/3000060" {
		t.Fatalf("wal position = %q, want the standby's received LSN", cs.WALPosition)
	}
	if !strings.Contains(q.sqls["wal_position"], "pg_current_wal_lsn()") {
		t.Fatalf("primary branch lost: %q", q.sqls["wal_position"])
	}
}

// Nil/empty: a standby with no WAL received yet has no position: the
// position is empty, which is not a failure of the snapshot.
func TestConfigSnapshot_NullWALPositionIsEmpty(t *testing.T) {
	q := newFakeConfigQuerier()
	q.standby, q.standbyLSN = true, nil
	cs, err := collectConfigSnapshot(context.Background(), q)
	if err != nil || cs == nil || cs.WALPosition != "" {
		t.Fatalf("snapshot %+v err %v, want an empty WAL position", cs, err)
	}
	if cs.PGSettings == nil || !cs.OwnsDatabase {
		t.Fatalf("the rest of the snapshot was lost: %+v", cs)
	}
}

// Nil/empty: sub-reads that return no rows are a real "none".
func TestConfigSnapshot_EmptySubReadsAreAvailable(t *testing.T) {
	q := newFakeConfigQuerier()
	for _, name := range []string{"reloptions", "connection_states", "extensions"} {
		q.reads[name] = fakeRead{}
	}
	cs, err := collectConfigSnapshot(context.Background(), q)
	if err != nil || cs == nil {
		t.Fatalf("snapshot %v err %v, want available with no rows", cs, err)
	}
	if len(cs.TableReloptions)+len(cs.ConnectionStates)+len(cs.ExtensionsAvailable) != 0 ||
		len(cs.PGSettings) != 1 {
		t.Fatalf("snapshot = %+v", cs)
	}
}

func rowCategories(t *testing.T, snap *Snapshot) map[string]string {
	t.Helper()
	rows, err := snapshotRows(snap)
	if err != nil {
		t.Fatalf("snapshotRows: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Category] = string(r.Data)
	}
	return got
}

// CHECK-01 (rows): an unavailable extra category leaves no persisted row;
// the other categories are still written.
func TestSnapshotRows_UnavailableExtrasLeaveNoRow(t *testing.T) {
	for _, cat := range []string{"io", "prepared_xacts", "partitions", "config_data"} {
		snap := &Snapshot{CollectedAt: time.Now(),
			PreparedXacts: []PreparedTransaction{},
			Unavailable:   map[string]string{cat: "statement timeout"}}
		got := rowCategories(t, snap)
		if _, ok := got[cat]; ok {
			t.Errorf("%s unavailable: a row was persisted (%s)", cat, got[cat])
		}
		if _, ok := got["system"]; !ok || len(got) != 17 {
			t.Errorf("%s unavailable: rows %v, want the 17 other categories", cat, keys(got))
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Nil/empty: no prepared transactions persists [], which differs from the
// missing row an unknown leaves.
func TestSnapshotRows_EmptyPreparedXactsPersistsEmptyList(t *testing.T) {
	none := rowCategories(t, &Snapshot{PreparedXacts: []PreparedTransaction{}})
	if data, ok := none["prepared_xacts"]; !ok || data != "[]" {
		t.Fatalf("no prepared xacts: row %q present %v, want []", data, ok)
	}
	unknown := rowCategories(t, &Snapshot{PreparedXacts: []PreparedTransaction{},
		Unavailable: map[string]string{"prepared_xacts": "permission denied"}})
	if _, ok := unknown["prepared_xacts"]; ok {
		t.Fatal("unknown prepared xacts persisted a row")
	}
}

// Happy path: a prepared transaction is persisted with its gid, the key
// its history pairs on.
func TestSnapshotRows_PreparedXactsRowHoldsGID(t *testing.T) {
	at := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	got := rowCategories(t, &Snapshot{PreparedXacts: []PreparedTransaction{{GID: "g1",
		Prepared: at, Owner: "app", Database: "shop", XIDAge: 1234}}})
	var stored []map[string]any
	if err := json.Unmarshal([]byte(got["prepared_xacts"]), &stored); err != nil {
		t.Fatalf("prepared_xacts row %q: %v", got["prepared_xacts"], err)
	}
	if len(stored) != 1 || stored[0]["gid"] != "g1" || stored[0]["database"] != "shop" {
		t.Fatalf("stored = %v, want g1 of shop", stored)
	}
}

func stepNames(steps []catalogStep) string {
	var names []string
	for _, s := range steps {
		names = append(names, s.name)
	}
	return strings.Join(names, ",")
}

// Boundary / state: io is read from PG16 (pg_stat_io) on; below it the
// category is not collected, which is not the same as failed. The
// configuration snapshot is read only for an advisor consumer.
func TestExtraSteps_GatedByVersionAndConsumer(t *testing.T) {
	cfg := testConfig()
	cases := []struct {
		version  int
		noConfig bool
		want     string
	}{
		{159999, false, "prepared_xacts,archiver,partitions,index_bloat,workload_sessions," +
			"wait_events,subscriptions,config_data"},
		{160000, false, "io,prepared_xacts,archiver,partitions,index_bloat,workload_sessions," +
			"wait_events,subscriptions,config_data"},
		{170000, true, "io,prepared_xacts,archiver,partitions,index_bloat,workload_sessions," +
			"wait_events,subscriptions"},
		{0, true, "prepared_xacts,archiver,partitions,index_bloat,workload_sessions," +
			"wait_events,subscriptions"},
	}
	for _, tc := range cases {
		c := New(nil, cfg, tc.version, noopLog)
		if tc.noConfig {
			c.WithoutConfigSnapshots()
		}
		if got := stepNames(c.extraSteps()); got != tc.want {
			t.Errorf("version %d, no config %v: steps %s, want %s", tc.version, tc.noConfig,
				got, tc.want)
		}
		for _, st := range c.extraSteps() {
			if st.reset == nil {
				t.Errorf("extra step %s has no reset: a failed read would keep its value",
					st.name)
			}
		}
	}
}
