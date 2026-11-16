package facts

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CoverageLimit is how many objects a coverage answer lists; more are
// reported as truncated.
const CoverageLimit = 50

// The bounds of a coverage read on the monitored database.
const (
	coverageStatementTimeout = "2s"
	coverageLockTimeout      = "1s"
)

// coverageSQL lists what a subject names, by kind, with the verifier's
// LIKE patterns (existsSQL): $1 the schema, $2 the name, $3 the limit.
var coverageSQL = map[Kind]string{
	KindSchema: `/* pg_sage facts */ SELECT nspname::text FROM pg_catalog.pg_namespace
		WHERE nspname LIKE $1 AND $2 = '' ORDER BY 1 LIMIT $3`,
	KindTable: `/* pg_sage facts */ SELECT n.nspname || '.' || c.relname
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname LIKE $1 AND c.relname LIKE $2 AND c.relkind IN ('r','p','m','f')
		ORDER BY n.nspname, c.relname LIMIT $3`,
	KindIndex: `/* pg_sage facts */ SELECT n.nspname || '.' || c.relname
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname LIKE $1 AND c.relname LIKE $2 AND c.relkind IN ('i','I')
		ORDER BY n.nspname, c.relname LIMIT $3`,
	KindSlot: `/* pg_sage facts */ SELECT slot_name::text FROM pg_catalog.pg_replication_slots
		WHERE slot_name LIKE $2 AND $1 = '' ORDER BY 1 LIMIT $3`,
}

// Coverage is what a subject matches today: at most CoverageLimit object
// names in name order, and whether there were more.
type Coverage struct {
	Objects   []string `json:"objects"`
	Truncated bool     `json:"truncated"`
}

// txBeginner opens a transaction (a pool).
type txBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// CoverageOf lists the objects of kind that subject matches in the
// database behind db, by the same patterns re-verification checks. A
// subject a fact could not have (invalid or protected) is refused with
// the fact errors before anything is read; the read is a bounded,
// read-only catalog query.
func CoverageOf(ctx context.Context, db txBeginner, kind Kind, subject string) (Coverage,
	error) {
	p, err := ParsePattern(kind, subject)
	if err != nil {
		return Coverage{}, err
	}
	if p.Kind == KindDatabase {
		return Coverage{}, fmt.Errorf("%w: a database fact covers the database itself; "+
			"there are no objects to list", ErrInvalidKind)
	}
	out := Coverage{Objects: []string{}}
	err = boundedRead(ctx, db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, coverageSQL[p.Kind], likePattern(p.Schema),
			likePattern(p.Name), CoverageLimit+1)
		if err != nil {
			return err
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		out.Objects = append(out.Objects, names...)
		return nil
	})
	if err != nil {
		return Coverage{}, fmt.Errorf("coverage of %s %s: %w", p.Kind, p.String(), err)
	}
	if len(out.Objects) > CoverageLimit {
		out.Objects, out.Truncated = out.Objects[:CoverageLimit], true
	}
	return out, nil
}

// boundedRead runs fn in a read-only transaction whose statement and lock
// timeouts bound every query (SET LOCAL: they end with it).
func boundedRead(ctx context.Context, db txBeginner, fn func(pgx.Tx) error) error {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer rollbackQuietly(ctx, tx)
	if _, err := tx.Exec(ctx, `/* pg_sage facts */ SELECT set_config('statement_timeout', $1, true),
		set_config('lock_timeout', $2, true)`, coverageStatementTimeout,
		coverageLockTimeout); err != nil {
		return fmt.Errorf("bound the read: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
