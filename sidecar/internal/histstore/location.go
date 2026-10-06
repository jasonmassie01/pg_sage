package histstore

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrMigrationNeeded: a database's history would be split between the two
// placements; the runtime refuses to start until it is migrated.
var ErrMigrationNeeded = errors.New("histstore: history must be migrated first")

// Placement is what a runtime knows about where history may be.
type Placement struct {
	Name      string // the database's name, for messages
	Monitored DB     // the monitored database
	Store     Store  // where the runtime will keep history
	// Meta is the meta database (nil without one) and DatabaseID the
	// database's meta-db record id (0 without one).
	Meta       DB
	DatabaseID int
}

// LocationError says which history is where it should not be and the
// command that moves it.
type LocationError struct {
	Database string
	Table    string
	Want     Mode
	Detail   string
	Command  string
}

func (e *LocationError) Error() string {
	other := ModeMonitored
	if e.Want == ModeMonitored {
		other = ModeMeta
	}
	return fmt.Sprintf("history of database %q is not where history.store says (%s): "+
		"sage.%s %s. Stop pg_sage, run `%s` (it copies the history; add --cleanup to "+
		"remove the source once copied), then start again; or set history.store: %s",
		e.Database, e.Want, e.Table, e.Detail, e.Command, other)
}

// Unwrap makes errors.Is(err, ErrMigrationNeeded) true.
func (e *LocationError) Unwrap() error { return ErrMigrationNeeded }

// CheckLocation refuses a placement that would split a database's
// history: meta mode with history left in the monitored database that was
// written after the last migration (or never migrated), or monitored mode
// with this database's history still in the meta database.
func CheckLocation(ctx context.Context, p Placement) error {
	if isNil(p.Monitored) {
		return ErrNoDatabase
	}
	if p.Store.Scoped() {
		src, err := OpenMonitored(ctx, p.Monitored)
		if err != nil {
			return err
		}
		return checkSide(ctx, p, src, p.Store, ModeMeta)
	}
	if isNil(p.Meta) || p.DatabaseID <= 0 {
		return nil
	}
	ok, err := hasIdentity(ctx, p.Meta)
	if err != nil || !ok {
		return err // a meta database that was never a history store holds none
	}
	src, err := NewMeta(p.Meta, p.DatabaseID)
	if err != nil {
		return err
	}
	return checkSide(ctx, p, src, NewMonitored(p.Monitored), ModeMonitored)
}

// checkSide refuses history in src that the migration into dst has not
// copied.
func checkSide(ctx context.Context, p Placement, src, dst Store, want Mode) error {
	id := p.DatabaseID
	if src.Scoped() {
		id = src.DatabaseID()
	} else if dst.Scoped() {
		id = dst.DatabaseID()
	}
	for _, table := range historyTables {
		m, err := readMark(ctx, dst.DB(), id, want, table)
		if err != nil {
			return err
		}
		after, err := rowsAfter(ctx, src, table, m)
		if err != nil {
			return err
		}
		if !after {
			continue
		}
		detail := "has rows that were never migrated"
		if m.found {
			detail = "has rows written after the last migration"
		}
		return &LocationError{Database: p.Name, Table: table, Want: want, Detail: detail,
			Command: fmt.Sprintf("pg_sage history migrate --to %s --database-id %d",
				want, id)}
	}
	return nil
}

// OpenMonitored is the monitored store of db, scoped to rows without a
// database id when db's history tables have the column (db is also a meta
// database's history store).
func OpenMonitored(ctx context.Context, db DB) (Store, error) {
	if isNil(db) {
		return Store{}, ErrNoDatabase
	}
	has, err := hasIdentity(ctx, db)
	if err != nil {
		return Store{}, err
	}
	return Store{db: db, legacyNull: has}, nil
}

// hasIdentity reports whether db's sage.snapshots has database_id.
func hasIdentity(ctx context.Context, db DB) (bool, error) {
	var has bool
	err := db.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute
		WHERE attrelid = pg_catalog.to_regclass('sage.snapshots')
		  AND attname = 'database_id' AND NOT attisdropped)`).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("inspect sage.snapshots for database_id: %w", err)
	}
	return has, nil
}

// rowsAfter reports whether src holds a row of table the mark does not
// cover: a snapshot with a higher id, a sample captured later (both found
// by index). Without a mark, any row.
func rowsAfter(ctx context.Context, src Store, table string, m mark) (bool, error) {
	var sql string
	var args []any
	switch table {
	case "snapshots":
		sql = `/* pg_sage */ SELECT EXISTS (SELECT 1 FROM sage.snapshots s
			WHERE {db:s} AND s.id > $1)`
		args = []any{m.lastID}
	default:
		sql = `/* pg_sage */ SELECT EXISTS (SELECT 1 FROM sage.query_store q
			WHERE {db:q} AND q.captured_at > $1)`
		args = []any{m.lastAtOrMin()}
	}
	var after bool
	if err := src.QueryRow(ctx, sql, args...).Scan(&after); err != nil {
		return false, fmt.Errorf("look for unmigrated rows of sage.%s: %w", table, err)
	}
	return after, nil
}

// lastAtOrMin is the mark's newest copied sample time, or the earliest
// time when nothing was copied.
func (m mark) lastAtOrMin() time.Time {
	if m.lastAt == nil {
		return time.Time{}
	}
	return *m.lastAt
}
