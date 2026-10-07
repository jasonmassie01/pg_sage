// Package histstore says where one monitored database's telemetry history
// lives (history.store, v2.3.0) and runs every history statement there.
//
// The history is the collector's snapshots (sage.snapshots) and the
// per-query samples (sage.query_store). In monitored mode (the default)
// they stay in the monitored database, as they always did. In meta mode
// they live in the meta database, shared by every monitored database, and
// each row carries its database's meta-db record id in database_id.
//
// Every history statement is written once, with markers, and runs through
// a Store that binds them:
//
//	{db:ALIAS}  true                     | ALIAS.database_id = $n
//	{dbcol}     (nothing)                | , database_id
//	{dbval}     (nothing)                | , $n
//
// with $n one argument past the statement's own. A marker is not SQL, so a
// statement sent without a Store fails to parse instead of reading an
// empty or a mixed history. Readers find their Store from the monitored
// pool they already hold (Resolve), so none can be wired to the wrong one.
package histstore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Mode is where history lives.
type Mode string

// The two placements.
const (
	ModeMonitored Mode = "monitored"
	ModeMeta      Mode = "meta"
)

// ParseMode parses a history.store value; empty means monitored.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "", string(ModeMonitored):
		return ModeMonitored, nil
	case string(ModeMeta):
		return ModeMeta, nil
	}
	return "", fmt.Errorf("history store %q: want %q or %q", s, ModeMonitored, ModeMeta)
}

// Errors a caller can tell apart.
var (
	// ErrNoDatabase: the store has no handle that can run the statement.
	ErrNoDatabase = errors.New("histstore: no database to run history statements on")
	// ErrNoDatabaseID: a meta store needs the database's meta-db record id.
	ErrNoDatabaseID = errors.New("histstore: history in the meta database needs a " +
		"positive database id (the meta-db record id)")
)

// DB is a pool, connection or transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Store is where one monitored database's history lives. The zero Store
// has no database.
type Store struct {
	db         any
	databaseID int
	// legacyNull scopes a monitored store to rows without a database id:
	// the meta database monitored as itself, whose tables also hold the
	// store's rows.
	legacyNull bool
}

// NewMonitored keeps history in db, the monitored database itself. db is
// usually a pool; anything with the methods a statement needs works.
func NewMonitored(db any) Store {
	return Store{db: db}
}

// NewMeta keeps history in the meta database db, as databaseID's rows.
func NewMeta(db DB, databaseID int) (Store, error) {
	if isNil(db) {
		return Store{}, ErrNoDatabase
	}
	if databaseID <= 0 {
		return Store{}, fmt.Errorf("%w, got %d", ErrNoDatabaseID, databaseID)
	}
	return Store{db: db, databaseID: databaseID}, nil
}

// Mode is the store's placement.
func (s Store) Mode() Mode {
	if s.databaseID > 0 {
		return ModeMeta
	}
	return ModeMonitored
}

// Scoped reports whether the store is the meta database (rows scoped by
// database id).
func (s Store) Scoped() bool { return s.databaseID > 0 }

// DatabaseID is the meta-db record id; 0 for a monitored store.
func (s Store) DatabaseID() int { return s.databaseID }

// DB is the handle statements run on.
func (s Store) DB() any { return s.db }

// WithDB is the same placement on another handle of the same database,
// such as a transaction begun on it.
func (s Store) WithDB(db any) Store {
	s.db = db
	return s
}

var dbMarker = regexp.MustCompile(`\{db:([A-Za-z_][A-Za-z0-9_]*)?\}`)

// Bind rewrites the statement's markers for this store and returns the
// arguments to run it with: the database id is appended once, after the
// statement's own arguments, when a marker uses it.
func (s Store) Bind(sql string, args ...any) (string, []any) {
	if !strings.Contains(sql, "{db") {
		return sql, args
	}
	ph := "$" + strconv.Itoa(len(args)+1)
	used := false
	out := dbMarker.ReplaceAllStringFunc(sql, func(m string) string {
		col := "database_id"
		if alias := m[len("{db:") : len(m)-1]; alias != "" {
			col = alias + "." + col
		}
		switch {
		case s.databaseID > 0:
			used = true
			return col + " = " + ph
		case s.legacyNull:
			return col + " IS NULL"
		}
		return "true"
	})
	col, val := "", ""
	if s.databaseID > 0 {
		col, val = ", database_id", ", "+ph
		used = used || strings.Contains(out, "{dbval}")
	}
	out = strings.ReplaceAll(strings.ReplaceAll(out, "{dbcol}", col), "{dbval}", val)
	if !used {
		return out, args
	}
	bound := make([]any, len(args), len(args)+1)
	copy(bound, args)
	return out, append(bound, s.databaseID)
}

func (s Store) noDB() error {
	if isNil(s.db) {
		return ErrNoDatabase
	}
	return fmt.Errorf("%w (%T cannot run this statement)", ErrNoDatabase, s.db)
}

// Exec binds and runs a statement.
func (s Store) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e, ok := s.db.(execer)
	if !ok || isNil(s.db) {
		return pgconn.CommandTag{}, s.noDB()
	}
	sql, args = s.Bind(sql, args...)
	return e.Exec(ctx, sql, args...)
}

// Query binds and runs a query.
func (s Store) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q, ok := s.db.(querier)
	if !ok || isNil(s.db) {
		return nil, s.noDB()
	}
	sql, args = s.Bind(sql, args...)
	return q.Query(ctx, sql, args...)
}

// QueryRow binds and runs a single-row query.
func (s Store) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	q, ok := s.db.(rowQuerier)
	if !ok || isNil(s.db) {
		return errRow{s.noDB()}
	}
	sql, args = s.Bind(sql, args...)
	return q.QueryRow(ctx, sql, args...)
}

// Begin starts a transaction on the store's database; bind statements
// on it with WithDB.
func (s Store) Begin(ctx context.Context) (pgx.Tx, error) {
	b, ok := s.db.(beginner)
	if !ok || isNil(s.db) {
		return nil, s.noDB()
	}
	return b.Begin(ctx)
}

// errRow is a row whose Scan reports err.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// isNil reports a nil handle, including a typed nil pointer in an interface.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func,
		reflect.Chan:
		return rv.IsNil()
	}
	return false
}
